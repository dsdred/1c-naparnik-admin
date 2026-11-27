// Package monitor опрашивает бюджеты ключей Напарника и считает производные метрики.
package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"naparnikadmin/internal/config"
	"naparnikadmin/internal/naparnik"
	"naparnikadmin/internal/secrets"
	"naparnikadmin/internal/store"
)

// Poller периодически снимает состояние лимитов по всем активным ключам.
type Poller struct {
	store  *store.Store
	client *naparnik.Client
	cipher *secrets.Cipher
	cfg    *config.Runtime
	log    *slog.Logger

	mu     sync.Mutex
	flight map[int64]chan struct{}
	levels map[int64]string
}

// New создаёт поллер.
func New(st *store.Store, client *naparnik.Client, cipher *secrets.Cipher, cfg *config.Runtime, log *slog.Logger) *Poller {
	return &Poller{
		store: st, client: client, cipher: cipher, cfg: cfg, log: log,
		flight: map[int64]chan struct{}{}, levels: map[int64]string{},
	}
}

// Start запускает фоновый опрос до отмены контекста.
func (p *Poller) Start(ctx context.Context) {
	go func() {
		// Первый опрос — сразу, чтобы админка не была пустой.
		p.RefreshAll(ctx)
		last := time.Now()
		for {
			cfg, changed := p.cfg.Watch()
			timer := time.NewTimer(time.Until(last.Add(cfg.BudgetPollInterval.D())))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-changed:
				timer.Stop()
			case <-timer.C:
				// An interval increase may have raced with the timer firing.
				if time.Since(last) < p.cfg.Snapshot().BudgetPollInterval.D() {
					continue
				}
				last = time.Now()
				p.RefreshAll(ctx)
			}
		}
	}()
}

// RefreshAll обновляет бюджеты всех ключей.
func (p *Poller) RefreshAll(ctx context.Context) {
	keys, err := p.store.ListKeys(ctx)
	if err != nil {
		p.log.Warn("не удалось прочитать ключи для опроса бюджета", "err", err)
		return
	}
	for _, k := range keys {
		if k.Disabled {
			continue
		}
		if _, err := p.Refresh(ctx, k.ID); err != nil {
			p.log.Debug("опрос бюджета не удался", "key", k.ID, "err", err)
		}
	}
}

// Refresh снимает бюджет одного ключа (single-flight: параллельные вызовы делят один запрос).
func (p *Poller) Refresh(ctx context.Context, keyID int64) (store.Sample, error) {
	p.mu.Lock()
	if ch, ok := p.flight[keyID]; ok {
		p.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return store.Sample{}, ctx.Err()
		}
		return p.store.LatestSample(ctx, keyID)
	}
	ch := make(chan struct{})
	p.flight[keyID] = ch
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		delete(p.flight, keyID)
		p.mu.Unlock()
		close(ch)
	}()

	enc, err := p.store.KeySecret(ctx, keyID)
	if err != nil {
		return store.Sample{}, err
	}
	key, err := p.cipher.Decrypt(enc)
	if err != nil {
		_ = p.store.UpdateKeyStatus(ctx, keyID, "error", err.Error())
		return store.Sample{}, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	budget, err := p.client.Budget(reqCtx, key)
	if err != nil {
		ge := naparnikError(err)
		_ = p.store.UpdateKeyStatus(ctx, keyID, ge.code, ge.text)
		return store.Sample{}, err
	}

	sample := store.Sample{
		KeyID:    keyID,
		TS:       time.Now(),
		Used5h:   budget.Limits.Sliding5h.UsedPercent,
		UsedWeek: budget.Limits.Weekly.UsedPercent,
		Reset5h:  budget.Limits.Sliding5h.ResetsAt,
		ResetWk:  budget.Limits.Weekly.ResetsAt,
	}
	if err := p.store.AddSample(ctx, sample); err != nil {
		return store.Sample{}, err
	}
	_ = p.store.UpdateKeyStatus(ctx, keyID, "active", "")
	p.checkAlerts(ctx, keyID, sample)
	return sample, nil
}

type upstreamStatus struct {
	code string
	text string
}

func naparnikError(err error) upstreamStatus {
	if f, ok := naparnik.AsFault(err); ok {
		return upstreamStatus{code: f.Code, text: ruUpstreamText(f.Code, f.Message)}
	}
	return upstreamStatus{code: "connection_error", text: err.Error()}
}

// ruUpstreamText переводит типовые отказы API Напарника на русский,
// чтобы в админке не мелькали англоязычные тексты апстрима.
func ruUpstreamText(code, fallback string) string {
	switch code {
	case "token_not_found", "invalid_token", "no_token", "unauthorized":
		return "Ключ Напарника недействителен или отозван — проверьте его на code.1c.ai"
	case "forbidden":
		return "У ключа нет доступа к API Напарника"
	}
	return fallback
}

// checkAlerts сравнивает уровень расхода с порогами и шлёт webhook при смене уровня.
func (p *Poller) checkAlerts(ctx context.Context, keyID int64, s store.Sample) {
	cfg := p.cfg.Snapshot()
	level := ""
	switch {
	case s.UsedWeek >= cfg.CritPercent || s.Used5h >= cfg.CritPercent:
		level = "crit"
	case s.UsedWeek >= cfg.WarnPercent || s.Used5h >= cfg.WarnPercent:
		level = "warn"
	}
	p.mu.Lock()
	prev := p.levels[keyID]
	p.levels[keyID] = level
	p.mu.Unlock()
	if level == "" || level == prev || cfg.AlertWebhook == "" {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"key_id": keyID, "level": level,
		"used_5h": s.Used5h, "used_week": s.UsedWeek,
		"threshold": map[string]float64{"warn": cfg.WarnPercent, "crit": cfg.CritPercent},
		"time":      s.TS.UTC().Format(time.RFC3339),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.AlertWebhook, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		p.log.Warn("webhook алерта не доставлен", "err", err)
		return
	}
	_ = resp.Body.Close()
}
