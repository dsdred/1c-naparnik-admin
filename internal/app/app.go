// Package app собирает сервис: хранилище, клиент, шлюз, админку и слушателей.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"naparnikadmin/internal/admin"
	"naparnikadmin/internal/config"
	"naparnikadmin/internal/gateway"
	"naparnikadmin/internal/httpx"
	"naparnikadmin/internal/monitor"
	"naparnikadmin/internal/naparnik"
	"naparnikadmin/internal/protocol/anthropic"
	"naparnikadmin/internal/protocol/gemini"
	"naparnikadmin/internal/protocol/openai"
	"naparnikadmin/internal/protocol/responses"
	"naparnikadmin/internal/secrets"
	"naparnikadmin/internal/store"
	"naparnikadmin/internal/web"
)

// App — собранный сервис.
type App struct {
	Cfg     *config.Runtime
	Store   *store.Store
	Cipher  *secrets.Cipher
	Engine  *gateway.Engine
	Monitor *monitor.Poller
	Auth    *admin.Auth
	Log     *slog.Logger

	mu      sync.Mutex
	servers map[string]*listener
	retired map[*http.Server]struct{}
	closed  bool
}

// New создаёт приложение и открывает хранилище.
func New(cfg *config.Config, log *slog.Logger) (*App, error) {
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return nil, fmt.Errorf("открытие базы: %w", err)
	}
	masterKey, err := secrets.LoadOrCreateMasterKey(cfg.DataDir)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("мастер-ключ: %w", err)
	}
	cipher, err := secrets.NewCipher(masterKey)
	if err != nil {
		st.Close()
		return nil, err
	}
	runtime := config.NewRuntime(cfg)
	client := naparnik.NewWithTimeout(cfg.UpstreamBase, func() time.Duration {
		return runtime.Snapshot().RequestTimeout.D()
	})
	a := &App{
		Cfg:     runtime,
		Store:   st,
		Cipher:  cipher,
		Engine:  gateway.New(client, st, runtime),
		Monitor: monitor.New(st, client, cipher, runtime, log),
		Auth:    admin.NewAuth(st, masterKey),
		Log:     log,
		servers: map[string]*listener{},
		retired: map[*http.Server]struct{}{},
	}
	return a, nil
}

// Close освобождает ресурсы.
func (a *App) Close() error { return a.Store.Close() }

// BuildVersion — версия сборки; её проставляет main. Отдаётся в /healthz,
// чтобы после замены бинарника можно было проверить, что поднялась нужная сборка.
var BuildVersion = "dev"

// GatewayHandler собирает маршруты шлюза.
func (a *App) GatewayHandler() http.Handler {
	auth := &gateway.Auth{Store: a.Store, Cipher: a.Cipher}
	mux := http.NewServeMux()

	oa := &openai.Handler{Auth: auth, Engine: a.Engine, Store: a.Store, Cfg: a.Cfg, Log: a.Log}
	an := &anthropic.Handler{Auth: auth, Engine: a.Engine, Store: a.Store, Cfg: a.Cfg, Log: a.Log}
	rs := &responses.Handler{Auth: auth, Engine: a.Engine, Store: a.Store, Cfg: a.Cfg, Log: a.Log}
	gm := &gemini.Handler{Auth: auth, Engine: a.Engine, Store: a.Store, Cfg: a.Cfg, Log: a.Log}

	mux.HandleFunc("POST /v1/chat/completions", oa.ChatCompletions)
	mux.HandleFunc("GET /v1/models", oa.Models)
	mux.HandleFunc("POST /v1/messages", an.Messages)
	mux.HandleFunc("POST /v1/responses", rs.Responses)
	mux.HandleFunc("POST /v1beta/", gm.Handle)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]any{"status": "ok", "version": BuildVersion})
	})
	return withLogging(a.Log, mux)
}

// AdminHandler собирает админку: API и встроенный SPA.
func (a *App) AdminHandler() http.Handler {
	srv := &admin.Server{
		Store:          a.Store,
		Cipher:         a.Cipher,
		Monitor:        a.Monitor,
		Cfg:            a.Cfg,
		Log:            a.Log,
		Auth:           a.Auth,
		ApplyListeners: a.applyListeners,
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", srv.Handler())
	mux.Handle("/", web.Handler())
	return mux
}

// Start запускает все слушатели и фоновый опрос бюджета.
func (a *App) Start(ctx context.Context) error {
	a.Monitor.Start(ctx)
	cfg := a.Cfg.Snapshot()
	// Чистим давно неиспользуемые привязки сессий.
	if err := a.Store.PurgeSessions(ctx, time.Now().Add(-2*cfg.SessionTTL.D())); err != nil {
		a.Log.Warn("не удалось почистить старые сессии", "err", err)
	}
	return a.ApplyListeners(cfg.Listeners)
}

// withLogging пишет в лог каждый запрос шлюза.
func withLogging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/healthz" {
			log.Debug("запрос", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start).String())
		}
	})
}
