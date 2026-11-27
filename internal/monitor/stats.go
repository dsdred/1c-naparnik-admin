package monitor

import (
	"context"
	"errors"
	"math"
	"time"

	"naparnikadmin/internal/store"
)

// KeyUsage — полная картина по одному ключу для админки.
type KeyUsage struct {
	Key      store.Key           `json:"key"`
	Current  *Current            `json:"current"`
	Usage    store.Usage         `json:"usage"`
	ByToken  []store.TokenUsage  `json:"by_token"`
	Series   []store.SeriesPoint `json:"series"`
	Estimate *Estimate           `json:"estimate"`
}

// Current — текущее состояние окон лимита.
type Current struct {
	MeasuredAt  time.Time  `json:"measured_at"`
	Used5h      float64    `json:"used_5h"`
	Remaining5h float64    `json:"remaining_5h"`
	Reset5h     *time.Time `json:"resets_5h"`
	UsedWeek    float64    `json:"used_week"`
	RemainWeek  float64    `json:"remaining_week"`
	ResetWeek   *time.Time `json:"resets_week"`
}

// Estimate — производные оценки (всегда приблизительные).
type Estimate struct {
	PercentPerRequest float64 `json:"percent_per_request"`
	RequestsLeft      int     `json:"requests_left"`
	ExhaustAt         string  `json:"exhaust_at"`
	Note              string  `json:"note"`
}

// Usage собирает статистику по ключу за период.
func (p *Poller) Usage(ctx context.Context, keyID int64, since time.Time) (*KeyUsage, error) {
	key, err := p.store.GetKey(ctx, keyID)
	if err != nil {
		return nil, err
	}
	out := &KeyUsage{Key: key}

	if latest, err := p.store.LatestSample(ctx, keyID); err == nil {
		out.Current = &Current{
			MeasuredAt:  latest.TS,
			Used5h:      latest.Used5h,
			Remaining5h: math.Max(0, 100-latest.Used5h),
			Reset5h:     latest.Reset5h,
			UsedWeek:    latest.UsedWeek,
			RemainWeek:  math.Max(0, 100-latest.UsedWeek),
			ResetWeek:   latest.ResetWk,
		}
	}

	if u, err := p.store.UsageForKey(ctx, keyID, since); err == nil {
		out.Usage = u
	}
	if byToken, err := p.store.UsageByToken(ctx, keyID, since); err == nil {
		out.ByToken = byToken
	}
	if series, err := p.store.SeriesByHour(ctx, keyID, since); err == nil {
		out.Series = series
	}
	out.Estimate = estimate(ctx, p, keyID, out)
	return out, nil
}

// estimate считает «процент на запрос» по приращению недельного окна
// и экстраполирует момент исчерпания.
func estimate(ctx context.Context, p *Poller, keyID int64, u *KeyUsage) *Estimate {
	e := &Estimate{
		Note: "Оценка по собственной статистике: API Напарника отдаёт только проценты, абсолютные квоты не публикуются.",
	}
	if u.Current == nil || u.Usage.Requests == 0 {
		return e
	}
	samples, err := p.store.Samples(ctx, keyID, time.Now().Add(-7*24*time.Hour))
	if err != nil || len(samples) < 2 {
		return e
	}
	first, last := samples[0], samples[len(samples)-1]
	delta := last.UsedWeek - first.UsedWeek
	if delta < 0 {
		// окно сбросилось — оценивать нечего
		return e
	}
	if delta > 0 {
		e.PercentPerRequest = delta / float64(u.Usage.Requests)
		if e.PercentPerRequest > 0 {
			e.RequestsLeft = int(u.Current.RemainWeek / e.PercentPerRequest)
		}
	}

	// Линейная экстраполяция по последним сэмплам.
	tail := samples
	if len(tail) > 12 {
		tail = tail[len(tail)-12:]
	}
	if len(tail) >= 2 {
		a, b := tail[0], tail[len(tail)-1]
		hours := b.TS.Sub(a.TS).Hours()
		slope := (b.UsedWeek - a.UsedWeek) / hours
		if hours > 0 && slope > 0 && b.UsedWeek < 100 {
			hoursLeft := (100 - b.UsedWeek) / slope
			if hoursLeft < 24*30 {
				e.ExhaustAt = b.TS.Add(time.Duration(hoursLeft * float64(time.Hour))).UTC().Format(time.RFC3339)
			}
		}
	}
	return e
}

// Overview — сводка по всем ключам.
type Overview struct {
	Keys        []KeyBrief `json:"keys"`
	TotalTokens int        `json:"total_tokens_today"`
	Requests    int        `json:"requests_today"`
	Errors      int        `json:"errors_today"`
}

// KeyBrief — краткая карточка ключа для главного экрана.
type KeyBrief struct {
	Key      store.Key `json:"key"`
	Current  *Current  `json:"current"`
	Requests int       `json:"requests_today"`
	Tokens   int       `json:"tokens_today"`
	Level    string    `json:"level"`
}

// Overview собирает сводку за сутки.
func (p *Poller) Overview(ctx context.Context) (*Overview, error) {
	cfg := p.cfg.Snapshot()
	keys, err := p.store.ListKeys(ctx)
	if err != nil {
		return nil, err
	}
	out := &Overview{}
	since := time.Now().Add(-24 * time.Hour)
	for _, k := range keys {
		brief := KeyBrief{Key: k}
		if latest, err := p.store.LatestSample(ctx, k.ID); err == nil {
			brief.Current = &Current{
				MeasuredAt: latest.TS, Used5h: latest.Used5h, Remaining5h: math.Max(0, 100-latest.Used5h),
				Reset5h: latest.Reset5h, UsedWeek: latest.UsedWeek, RemainWeek: math.Max(0, 100-latest.UsedWeek),
				ResetWeek: latest.ResetWk,
			}
			switch {
			case latest.UsedWeek >= cfg.CritPercent || latest.Used5h >= cfg.CritPercent:
				brief.Level = "crit"
			case latest.UsedWeek >= cfg.WarnPercent || latest.Used5h >= cfg.WarnPercent:
				brief.Level = "warn"
			}
		}
		if u, err := p.store.UsageForKey(ctx, k.ID, since); err == nil {
			brief.Requests = u.Requests
			brief.Tokens = u.Prompt + u.Completion
			out.Requests += u.Requests
			out.Errors += u.Errors
			out.TotalTokens += u.Prompt + u.Completion
		}
		out.Keys = append(out.Keys, brief)
	}
	return out, nil
}

var errNoSample = errors.New("нет замеров бюджета")
