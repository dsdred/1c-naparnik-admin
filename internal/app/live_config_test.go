package app

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"naparnikadmin/internal/config"
)

func newConfigTestApp(t *testing.T) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = dir
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		a.Shutdown(ctx)
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	return a, path
}

func TestSettingsRejectInvalidAndUnwritableChanges(t *testing.T) {
	a, path := newConfigTestApp(t)
	admin, cookie := settingsTestLogin(t, a)
	put := func(body string) *httptest.ResponseRecorder {
		return settingsRequest(admin, http.MethodPut, "/api/settings", body, cookie)
	}
	r := put(`{"model_id":"accepted","alert_webhook":"https://alerts.example.invalid"}`)
	if r.Code != http.StatusOK {
		t.Fatalf("valid update: %d %s", r.Code, r.Body)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r = put(`{"model_id":"rejected","session_ttl":"invalid"}`)
	if r.Code != http.StatusBadRequest || a.Cfg.Snapshot().ModelName() != "accepted" {
		t.Fatalf("invalid update: %d %s, current model %q", r.Code, r.Body, a.Cfg.Snapshot().ModelName())
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("rejected update changed persisted config: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	r = put(`{"model_id":"unsaved"}`)
	if r.Code != http.StatusInternalServerError || a.Cfg.Snapshot().ModelName() != "accepted" {
		t.Fatalf("persist failure: %d %s, current model %q", r.Code, r.Body, a.Cfg.Snapshot().ModelName())
	}
}

func TestSettingsConcurrentReadsAndWrites(t *testing.T) {
	a, path := newConfigTestApp(t)
	admin, cookie := settingsTestLogin(t, a)
	var wg sync.WaitGroup
	for _, action := range []struct{ method, body string }{
		{http.MethodPut, `{"model_id":"updated","stream_mode":"deltas","alert_webhook":"https://alerts.example.invalid"}`},
		{http.MethodPut, `{"session_ttl":"3h"}`},
		{http.MethodGet, ""},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				r := settingsRequest(admin, action.method, "/api/settings", action.body, cookie)
				if r.Code != http.StatusOK {
					t.Errorf("%s settings: %d %s", action.method, r.Code, r.Body)
				}
			}
		}()
	}
	wg.Wait()
	for _, cfg := range []*config.Config{a.Cfg.Snapshot(), loadTestConfig(t, path)} {
		if cfg.ModelName() != "updated" || cfg.StreamMode != "deltas" || cfg.SessionTTL.D() != 3*time.Hour ||
			cfg.AlertWebhook != "https://alerts.example.invalid" {
			t.Fatalf("lost an update: %+v", cfg)
		}
	}
}

func TestListenerUpdatePreservesWorkingPort(t *testing.T) {
	a, _ := newConfigTestApp(t)
	original := config.Listener{Kind: config.KindGateway, Addr: "127.0.0.1:0"}
	if err := a.ApplyListeners([]config.Listener{original}); err != nil {
		t.Fatal(err)
	}
	addr := a.servers[listenerKey(original)].ln.Addr().String()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	assertHealthy := func() {
		t.Helper()
		resp, err := client.Get("http://" + addr + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("old listener /healthz: %d", resp.StatusCode)
		}
	}
	assertHealthy()

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	if err := a.ApplyListeners([]config.Listener{{Kind: config.KindGateway, Addr: occupied.Addr().String()}}); err == nil {
		t.Fatal("accepted an occupied port")
	}
	assertHealthy()

	saveErr := errors.New("save failed")
	replacement := []config.Listener{{Kind: config.KindAdmin, Addr: addr}}
	if err := a.applyListeners(replacement, func() error { return saveErr }); !errors.Is(err, saveErr) {
		t.Fatalf("want save failure after binding replacement, got %v", err)
	}
	assertHealthy()

	if err := a.ApplyListeners(replacement); err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get("http://" + addr + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replacement admin endpoint: %d", resp.StatusCode)
	}

	certs := httptest.NewTLSServer(http.NotFoundHandler())
	defer certs.Close()
	cert := certs.TLS.Certificates[0]
	privateKey, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	tlsDir := t.TempDir()
	replacement[0].CertFile = filepath.Join(tlsDir, "tls.crt")
	replacement[0].KeyFile = filepath.Join(tlsDir, "tls.key")
	if err := os.WriteFile(replacement[0].CertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement[0].KeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKey}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyListeners(replacement); err != nil {
		t.Fatal(err)
	}
	tlsClient := certs.Client()
	tlsClient.Timeout = time.Second
	tlsResp, err := tlsClient.Get("https://" + addr + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer tlsResp.Body.Close()
	if tlsResp.StatusCode != http.StatusOK {
		t.Fatalf("TLS replacement admin endpoint: %d", tlsResp.StatusCode)
	}
}

func settingsTestLogin(t *testing.T, a *App) (http.Handler, *http.Cookie) {
	t.Helper()
	admin := a.AdminHandler()
	r := settingsRequest(admin, http.MethodPost, "/api/password", `{"password":"fixture-password"}`, nil)
	cookies := r.Result().Cookies()
	if r.Code != http.StatusOK || len(cookies) != 1 {
		t.Fatalf("admin login: %d %s", r.Code, r.Body)
	}
	return admin, cookies[0]
}

func settingsRequest(h http.Handler, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Requested-With", "test")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func loadTestConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
