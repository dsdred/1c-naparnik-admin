package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"naparnikadmin/internal/app"
	"naparnikadmin/internal/config"
	"naparnikadmin/internal/secrets"
	"naparnikadmin/internal/sse"
	"naparnikadmin/internal/store"
)

const token = "review-local-test-token"

type fixture struct {
	app            *app.App
	gateway, admin http.Handler
	tokenID        int64
	conversations  atomic.Int64
}

func newFixture(t *testing.T, mode string, packet string, beforePacket ...func()) *fixture {
	t.Helper()
	f := &fixture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/conversations":
			fmt.Fprintf(w, `{"uuid":"dialog-%d"}`, f.conversations.Add(1))
		case strings.HasSuffix(r.URL.Path, "/messages"):
			for _, hook := range beforePacket {
				hook()
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", packet)
		case strings.HasSuffix(r.URL.Path, "/context_tokens"):
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = dir
	cfg.UpstreamBase = upstream.URL
	cfg.StreamMode = mode
	cfg.RequestTimeout = config.Duration(3 * time.Second)
	f.app, err = app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.app.Close(); err != nil {
			t.Error(err)
		}
	})
	enc, err := f.app.Cipher.Encrypt("upstream-fixture")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	keyID, err := f.app.Store.CreateKey(ctx, "review", "review", enc)
	if err != nil {
		t.Fatal(err)
	}
	f.tokenID, err = f.app.Store.CreateToken(ctx, store.Token{KeyID: keyID, Name: "review", Enabled: true}, secrets.HashToken(token), "")
	if err != nil {
		t.Fatal(err)
	}
	f.gateway, f.admin = f.app.GatewayHandler(), f.app.AdminHandler()
	return f
}

const finished = `{"uuid":"answer","role":"assistant","finished":true,"content":{"content":"ready"}}`
const chat = `{"messages":[{"role":"user","content":"question"}]}`

func request(h http.Handler, method, path, body, expectedToken, session string, c *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Requested-With", "test")
	if expectedToken != "" {
		r.Header.Set("Authorization", "Bearer "+expectedToken)
	}
	if session != "" {
		r.Header.Set("X-Session-Id", session)
	}
	if c != nil {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func login(t *testing.T, f *fixture) *http.Cookie {
	t.Helper()
	w := request(f.admin, "POST", "/api/password", `{"password":"fixture-password"}`, "", "", nil)
	cs := w.Result().Cookies()
	if w.Code != 200 || len(cs) != 1 {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	return cs[0]
}

func TestSmokeAdapters(t *testing.T) {
	for _, mode := range []string{"final", "deltas"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, mode, finished)
			for _, tc := range []struct{ name, path, body, streamPath, streamBody, terminal string }{
				{"chat", "/v1/chat/completions", chat, "/v1/chat/completions", `{"stream":true,"messages":[{"role":"user","content":"question"}]}`, "[DONE]"},
				{"responses", "/v1/responses", `{"input":"question"}`, "/v1/responses", `{"stream":true,"input":"question"}`, "response.completed"},
				{"anthropic", "/v1/messages", `{"messages":[{"role":"user","content":"question"}]}`, "/v1/messages", `{"stream":true,"messages":[{"role":"user","content":"question"}]}`, "message_stop"},
				{"gemini", "/v1beta/models/naparnik:generateContent", `{"contents":[{"role":"user","parts":[{"text":"question"}]}]}`, "/v1beta/models/naparnik:streamGenerateContent", `{"contents":[{"role":"user","parts":[{"text":"question"}]}]}`, "finishReason"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					w := request(f.gateway, "POST", tc.path, tc.body, "", "", nil)
					if w.Code != 401 {
						t.Fatalf("missing token: %d", w.Code)
					}
					w = request(f.gateway, "POST", tc.path, tc.body, token, "json", nil)
					if w.Code != 200 || !json.Valid(w.Body.Bytes()) || !strings.Contains(w.Body.String(), "ready") {
						t.Fatalf("JSON: %d %s", w.Code, w.Body)
					}
					w = request(f.gateway, "POST", tc.streamPath, tc.streamBody, token, "stream", nil)
					if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") || !strings.Contains(w.Body.String(), "ready") || !strings.Contains(w.Body.String(), tc.terminal) {
						t.Fatalf("SSE: %d %s", w.Code, w.Body)
					}
				})
			}
		})
	}
}

func TestSmokeConcurrentChats(t *testing.T) {
	f := newFixture(t, "final", finished)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := request(f.gateway, "POST", "/v1/chat/completions", chat, token, fmt.Sprintf("session-%d", i%8), nil)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "ready") {
				t.Errorf("response: %d %s", w.Code, w.Body)
			}
		}()
	}
	wg.Wait()
	if got := f.conversations.Load(); got != 8 {
		t.Fatalf("created %d conversations, want 8", got)
	}
}

func TestRegressionTruncatedUpstreamIsNotSuccessful(t *testing.T) {
	f := newFixture(t, "final", `{"uuid":"draft","role":"assistant","finished":false,"content_delta":{"content":"unfinished"}}`)
	w := request(f.gateway, "POST", "/v1/chat/completions", chat, token, "truncated", nil)
	if w.Code != 502 {
		t.Fatalf("truncated upstream got %d, want 502: %s", w.Code, w.Body)
	}
}

func TestRegressionChatStreamKeepsCompletionID(t *testing.T) {
	f := newFixture(t, "deltas", finished)
	w := request(f.gateway, "POST", "/v1/chat/completions", `{"stream":true,"messages":[{"role":"user","content":"question"}]}`, token, "chat-ids", nil)
	ids := make(map[string]bool)
	chunks := 0
	for ev := range sse.Read(context.Background(), w.Body) {
		if ev.Data == "[DONE]" {
			continue
		}
		var data struct{ ID string }
		if err := json.Unmarshal([]byte(ev.Data), &data); err != nil {
			t.Fatal(err)
		}
		chunks++
		ids[data.ID] = true
	}
	if chunks < 2 || len(ids) != 1 || ids[""] {
		t.Fatalf("got %d chunks and %d distinct IDs, want multiple chunks with one ID", chunks, len(ids))
	}
}

func TestRegressionResponsesStreamKeepsOutputID(t *testing.T) {
	f := newFixture(t, "deltas", finished)
	w := request(f.gateway, "POST", "/v1/responses", `{"input":"question","stream":true}`, token, "ids", nil)
	var added, delta, done, finishedID string
	addedCount := 0
	for ev := range sse.Read(context.Background(), w.Body) {
		var data struct {
			ItemID   string                                 `json:"item_id"`
			Item     struct{ ID string }                    `json:"item"`
			Response struct{ Output []struct{ ID string } } `json:"response"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &data); err != nil {
			t.Fatal(err)
		}
		switch ev.Name {
		case "response.output_item.added":
			addedCount++
			added = data.Item.ID
		case "response.output_text.delta":
			delta = data.ItemID
		case "response.output_item.done":
			done = data.Item.ID
		case "response.completed":
			if len(data.Response.Output) > 0 {
				finishedID = data.Response.Output[0].ID
			}
		}
	}
	if addedCount != 1 || added == "" || added != delta || added != done || added != finishedID {
		t.Fatalf("added events=%d, IDs added=%q delta=%q done=%q completed=%q", addedCount, added, delta, done, finishedID)
	}
}
