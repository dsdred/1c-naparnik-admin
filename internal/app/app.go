// Package app собирает сервис: хранилище, клиент, шлюз, админку и слушателей.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
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
	Cfg     *config.Config
	Store   *store.Store
	Cipher  *secrets.Cipher
	Engine  *gateway.Engine
	Monitor *monitor.Poller
	Auth    *admin.Auth
	Log     *slog.Logger

	mu      sync.Mutex
	servers map[string]*http.Server
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
	client := naparnik.New(cfg.UpstreamBase, cfg.RequestTimeout.D())
	a := &App{
		Cfg:     cfg,
		Store:   st,
		Cipher:  cipher,
		Engine:  gateway.New(client, st, cfg),
		Monitor: monitor.New(st, client, cipher, cfg, log),
		Auth:    admin.NewAuth(st, masterKey),
		Log:     log,
		servers: map[string]*http.Server{},
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
		ApplyListeners: a.ApplyListeners,
		ResetSession:   a.Engine.ResetSession,
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", srv.Handler())
	mux.Handle("/", web.Handler())
	return mux
}

// Start запускает все слушатели и фоновый опрос бюджета.
func (a *App) Start(ctx context.Context) error {
	a.Monitor.Start(ctx)
	// Чистим давно неиспользуемые привязки сессий.
	if err := a.Store.PurgeSessions(ctx, time.Now().Add(-2*a.Cfg.SessionTTL.D())); err != nil {
		a.Log.Warn("не удалось почистить старые сессии", "err", err)
	}
	for _, l := range a.Cfg.Listeners {
		if l.Disabled {
			a.Log.Info("слушатель выключен", "addr", l.Addr, "kind", l.Kind)
			continue
		}
		if err := a.startListener(l); err != nil {
			return err
		}
	}
	return nil
}

// listenerKey — идентичность слушателя. Пути к сертификату входят в ключ:
// если их поменяли в админке, слушатель пересоздаётся и подхватывает новый
// сертификат, а не работает со старым до перезапуска процесса.
func listenerKey(l config.Listener) string {
	return l.Kind + "|" + l.Addr + "|" + l.CertFile + "|" + l.KeyFile
}

func (a *App) startListener(l config.Listener) error {
	if l.Disabled {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	key := listenerKey(l)
	if _, ok := a.servers[key]; ok {
		return nil
	}
	var handler http.Handler
	switch l.Kind {
	case config.KindAdmin:
		handler = a.AdminHandler()
		if !config.IsLoopback(l.Addr) {
			a.Log.Warn("админка слушает не loopback-адрес — доступна по сети", "addr", l.Addr)
		}
	default:
		handler = a.GatewayHandler()
	}
	srv := &http.Server{
		Addr:              l.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	// Пару грузим до открытия порта: битый или отсутствующий сертификат должен
	// вернуться вызывающему сразу, а не уронить слушатель в горутине.
	useTLS := l.CertFile != "" && l.KeyFile != ""
	if useTLS {
		pair, err := tls.LoadX509KeyPair(l.CertFile, l.KeyFile)
		if err != nil {
			return fmt.Errorf("слушатель %s: сертификат: %w", l.Addr, err)
		}
		srv.TLSConfig = &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{pair},
		}
	}
	ln, err := net.Listen("tcp", l.Addr)
	if err != nil {
		return fmt.Errorf("слушатель %s: %w", l.Addr, err)
	}
	a.servers[key] = srv
	a.Log.Info("слушатель запущен", "addr", l.Addr, "kind", l.Kind, "tls", useTLS)
	go func() {
		var err error
		if useTLS {
			err = srv.ServeTLS(ln, "", "")
		} else {
			err = srv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.Log.Error("слушатель остановился", "addr", l.Addr, "err", err)
		}
	}()
	return nil
}

// ApplyListeners приводит набор слушателей к желаемому без перезапуска процесса.
func (a *App) ApplyListeners(list []config.Listener) error {
	// Проверяем сертификаты до остановки работающих слушателей: иначе опечатка
	// в пути оставила бы сервис без уже поднятых портов.
	for _, l := range list {
		if l.Disabled || l.CertFile == "" && l.KeyFile == "" {
			continue
		}
		if l.CertFile == "" || l.KeyFile == "" {
			return fmt.Errorf("слушатель %s: нужны оба файла — сертификат и ключ", l.Addr)
		}
		if _, err := tls.LoadX509KeyPair(l.CertFile, l.KeyFile); err != nil {
			return fmt.Errorf("слушатель %s: сертификат: %w", l.Addr, err)
		}
	}
	desired := map[string]config.Listener{}
	for _, l := range list {
		if l.Disabled {
			continue
		}
		desired[listenerKey(l)] = l
	}

	a.mu.Lock()
	var toStop []*http.Server
	for key, srv := range a.servers {
		if _, ok := desired[key]; !ok {
			toStop = append(toStop, srv)
			delete(a.servers, key)
		}
	}
	a.mu.Unlock()

	for _, srv := range toStop {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
		a.Log.Info("слушатель остановлен", "addr", srv.Addr)
	}

	for _, l := range list {
		if err := a.startListener(l); err != nil {
			return err
		}
	}
	return nil
}

// Shutdown корректно останавливает все слушатели.
func (a *App) Shutdown(ctx context.Context) {
	a.mu.Lock()
	servers := make([]*http.Server, 0, len(a.servers))
	for _, srv := range a.servers {
		servers = append(servers, srv)
	}
	a.servers = map[string]*http.Server{}
	a.mu.Unlock()
	for _, srv := range servers {
		_ = srv.Shutdown(ctx)
	}
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
