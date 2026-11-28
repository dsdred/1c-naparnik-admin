package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"naparnikadmin/internal/config"
	"naparnikadmin/internal/secrets"
	"naparnikadmin/internal/store"
)

func TestPrometheusListenerReportsResponsesRequests(t *testing.T) {
	a, _ := newConfigTestApp(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/conversations":
			fmt.Fprint(w, `{"uuid":"dialog"}`)
		case strings.HasSuffix(r.URL.Path, "/messages"):
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: "+`{"uuid":"reply","role":"assistant","finished":true,"content":{"content":"ready"},"usage":{"prompt_tokens":8,"completion_tokens":2,"cached_tokens":5,"total_tokens":10}}`+"\n\n")
		case strings.HasSuffix(r.URL.Path, "/context_tokens"):
			fmt.Fprint(w, "{}")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	if err := a.Cfg.Update(func(cfg *config.Config) error {
		cfg.UpstreamBase = upstream.URL
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	enc, err := a.Cipher.Encrypt("synthetic-upstream-key")
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := a.Store.CreateKey(t.Context(), "key", "hint", enc)
	if err != nil {
		t.Fatal(err)
	}
	const bearer = "synthetic-metrics-token"
	if _, err := a.Store.CreateToken(t.Context(), store.Token{KeyID: keyID, Name: "test", Enabled: true}, secrets.HashToken(bearer), ""); err != nil {
		t.Fatal(err)
	}
	gateway := a.GatewayHandler()
	send := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		gateway.ServeHTTP(w, r)
		return w
	}
	if w := send("/v1/responses", `{"input":"test","stream":true}`); w.Code != 200 || !strings.Contains(w.Body.String(), "response.completed") {
		t.Fatalf("Responses success: %d %s", w.Code, w.Body)
	}
	if w := send("/v1/responses", `{"stream":true}`); w.Code != 200 || !strings.Contains(w.Body.String(), "invalid_request_error") {
		t.Fatalf("Responses SSE error: %d %s", w.Code, w.Body)
	}
	w := httptest.NewRecorder()
	gateway.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("gateway unexpectedly exposed metrics: %d", w.Code)
	}
	l := config.Listener{Kind: config.KindMetrics, Addr: "127.0.0.1:0"}
	if err := a.ApplyListeners([]config.Listener{l}); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	base := "http://" + a.servers[listenerKey(l)].ln.Addr().String()
	resp, err := client.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("scrape: HTTP %d, read error %v, body %s", resp.StatusCode, err, body)
	}
	for _, want := range []string{
		"naparnik_admin_up 1",
		`naparnik_admin_gateway_requests_total{code="200",protocol="responses"} 1`,
		`naparnik_admin_gateway_requests_total{code="400",protocol="responses"} 1`,
		`naparnik_admin_gateway_request_duration_seconds_count{protocol="responses"} 2`,
		`naparnik_admin_gateway_tokens_total{kind="input",protocol="responses"} 8`,
		`naparnik_admin_gateway_tokens_total{kind="output",protocol="responses"} 2`,
		`naparnik_admin_gateway_tokens_total{kind="cached",protocol="responses"} 5`,
	} {
		if !strings.Contains(string(body), want+"\n") {
			t.Errorf("scrape missing %s", want)
		}
	}
	resp, err = client.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("metrics listener unexpectedly exposed gateway routes: %d", resp.StatusCode)
	}
	l.Disabled = true
	if err := a.ApplyListeners([]config.Listener{l}); err != nil {
		t.Fatal(err)
	}
	if len(a.servers) != 0 {
		t.Fatal("disabled metrics listener was retained")
	}
}
