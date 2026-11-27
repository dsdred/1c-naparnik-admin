package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"naparnikadmin/internal/config"
)

type listener struct {
	cfg config.Listener
	srv *http.Server
	ln  net.Listener
}

// listenerKey includes TLS paths so a certificate change replaces the server.
func listenerKey(l config.Listener) string {
	return l.Kind + "|" + l.Addr + "|" + l.CertFile + "|" + l.KeyFile
}

func (a *App) prepareListener(l config.Listener) (*listener, error) {
	var handler http.Handler
	if l.Kind == config.KindAdmin {
		handler = a.AdminHandler()
	} else {
		handler = a.GatewayHandler()
	}
	srv := &http.Server{
		Addr:              l.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if l.CertFile != "" || l.KeyFile != "" {
		if !l.TLS() {
			return nil, fmt.Errorf("слушатель %s: нужны оба файла — сертификат и ключ", l.Addr)
		}
		pair, err := tls.LoadX509KeyPair(l.CertFile, l.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("слушатель %s: сертификат: %w", l.Addr, err)
		}
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	}
	return &listener{cfg: l, srv: srv}, nil
}

func (a *App) serveListener(l *listener) {
	ln, srv := l.ln, l.srv
	useTLS := l.cfg.TLS()
	if l.cfg.Kind == config.KindAdmin && !config.IsLoopback(l.cfg.Addr) {
		a.Log.Warn("админка слушает не loopback-адрес — доступна по сети", "addr", l.cfg.Addr)
	}
	a.Log.Info("слушатель запущен", "addr", l.cfg.Addr, "kind", l.cfg.Kind, "tls", useTLS)
	go func() {
		var err error
		if useTLS {
			err = srv.ServeTLS(ln, "", "")
		} else {
			err = srv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			a.Log.Error("слушатель остановился", "addr", srv.Addr, "err", err)
		}
	}()
}

// ApplyListeners приводит набор слушателей к желаемому без перезапуска процесса.
func (a *App) ApplyListeners(list []config.Listener) error {
	return a.applyListeners(list, nil)
}

func (a *App) applyListeners(list []config.Listener, save func() error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return http.ErrServerClosed
	}
	desired := make(map[string]*listener, len(list))
	var added, removed []*listener
	for _, l := range list {
		if l.Disabled {
			continue
		}
		key := listenerKey(l)
		if _, ok := desired[key]; ok {
			continue
		}
		if active, ok := a.servers[key]; ok {
			desired[key] = active
			continue
		}
		prepared, err := a.prepareListener(l)
		if err != nil {
			return err
		}
		desired[key] = prepared
		added = append(added, prepared)
	}
	for key, active := range a.servers {
		if _, keep := desired[key]; !keep {
			removed = append(removed, active)
		}
	}

	// Reserve new ports first. An existing socket only needs to be paused
	// when a replacement conflicts with it, such as HTTP changing to HTTPS.
	var deferred []*listener
	pausing := map[*listener]struct{}{}
	paused := map[*listener]string{}
	rollback := func(cause error) error {
		for _, l := range added {
			if l.ln != nil {
				_ = l.ln.Close()
			}
		}
		for active, addr := range paused {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				delete(a.servers, listenerKey(active.cfg))
				a.retireListener(active)
				cause = errors.Join(cause, fmt.Errorf("не удалось восстановить слушатель %s: %w", addr, err))
				continue
			}
			active.ln = ln
			a.serveListener(active)
		}
		return cause
	}
	for _, l := range added {
		ln, err := net.Listen("tcp", l.cfg.Addr)
		if err == nil {
			l.ln = ln
			continue
		}
		_, port, _ := net.SplitHostPort(l.cfg.Addr)
		portNumber, _ := net.LookupPort("tcp", port)
		wait := false
		for _, active := range removed {
			if portNumber == active.ln.Addr().(*net.TCPAddr).Port {
				pausing[active] = struct{}{}
				wait = true
			}
		}
		if !wait {
			return rollback(fmt.Errorf("слушатель %s: %w", l.cfg.Addr, err))
		}
		deferred = append(deferred, l)
	}
	for active := range pausing {
		paused[active] = active.ln.Addr().String()
		_ = active.ln.Close()
	}
	for _, l := range deferred {
		ln, err := net.Listen("tcp", l.cfg.Addr)
		if err != nil {
			return rollback(fmt.Errorf("слушатель %s: %w", l.cfg.Addr, err))
		}
		l.ln = ln
	}
	if save != nil {
		if err := save(); err != nil {
			return rollback(err)
		}
	}
	for _, active := range removed {
		_ = active.ln.Close()
		a.retireListener(active)
	}
	a.servers = desired
	for _, l := range added {
		a.serveListener(l)
	}
	return nil
}

// The request changing an admin port may still be using the retiring server.
func (a *App) retireListener(l *listener) {
	srv := l.srv
	a.retired[srv] = struct{}{}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			_ = srv.Close()
		}
		a.mu.Lock()
		delete(a.retired, srv)
		a.mu.Unlock()
	}()
}

// Shutdown корректно останавливает все слушатели.
func (a *App) Shutdown(ctx context.Context) {
	a.mu.Lock()
	a.closed = true
	servers := make([]*http.Server, 0, len(a.servers)+len(a.retired))
	for _, active := range a.servers {
		_ = active.ln.Close()
		servers = append(servers, active.srv)
	}
	for srv := range a.retired {
		servers = append(servers, srv)
	}
	clear(a.servers)
	a.mu.Unlock()
	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil {
			_ = srv.Close()
		}
	}
}
