package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	gateway       http.Handler
	conversations atomic.Int64
}

func newFixture(t *testing.T, mode string, packets ...string) *fixture {
	t.Helper()
	f := &fixture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/conversations":
			fmt.Fprintf(w, `{"uuid":"dialog-%d"}`, f.conversations.Add(1))
		case strings.HasSuffix(r.URL.Path, "/messages"):
			w.Header().Set("Content-Type", "text/event-stream")
			for _, packet := range packets {
				fmt.Fprintf(w, "data: %s\n\n", packet)
			}
		case strings.HasSuffix(r.URL.Path, "/context_tokens"):
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.UpstreamBase = upstream.URL
	cfg.StreamMode = mode
	cfg.RequestTimeout = config.Duration(3 * time.Second)
	a, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	enc, err := a.Cipher.Encrypt("upstream-fixture")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	keyID, err := a.Store.CreateKey(ctx, "review", "review", enc)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Store.CreateToken(ctx, store.Token{KeyID: keyID, Name: "review", Enabled: true}, secrets.HashToken(token), "")
	if err != nil {
		t.Fatal(err)
	}
	f.gateway = a.GatewayHandler()
	return f
}

const finished = `{"uuid":"answer","role":"assistant","finished":true,"content":{"content":"ready"}}`
const chat = `{"messages":[{"role":"user","content":"question"}]}`

func request(h http.Handler, method, path, body, expectedToken, session string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if expectedToken != "" {
		r.Header.Set("Authorization", "Bearer "+expectedToken)
	}
	if session != "" {
		r.Header.Set("X-Session-Id", session)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
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
					w := request(f.gateway, "POST", tc.path, tc.body, "", "")
					if w.Code != 401 {
						t.Fatalf("missing token: %d", w.Code)
					}
					w = request(f.gateway, "POST", tc.path, tc.body, token, "json")
					if w.Code != 200 || !json.Valid(w.Body.Bytes()) || !strings.Contains(w.Body.String(), "ready") {
						t.Fatalf("JSON: %d %s", w.Code, w.Body)
					}
					w = request(f.gateway, "POST", tc.streamPath, tc.streamBody, token, "stream")
					if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") || !strings.Contains(w.Body.String(), "ready") || !strings.Contains(w.Body.String(), tc.terminal) {
						t.Fatalf("SSE: %d %s", w.Code, w.Body)
					}
				})
			}
		})
	}
}

func TestResponsesContinuesWithCodexToolOutput(t *testing.T) {
	const path = "/v1/responses"
	const session = "codex-tool"
	const tools = `,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`
	const call = `{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}`
	f := newFixture(t, "final", `{"uuid":"tool-answer","role":"assistant","finished":true,"content":{"tool_calls":[{"id":"call_1","function":{"name":"client_lookup","arguments":"{}"}}]}}`)

	first := request(f.gateway, "POST", path, `{"stream":true,"input":"question"`+tools, token, session)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "response.completed") ||
		!strings.Contains(first.Body.String(), `"call_id":"call_1"`) {
		t.Fatalf("initial tool call: %d %s", first.Code, first.Body)
	}

	const continuation = `{"input":[{"role":"user","content":[{"type":"input_text","text":"question"}]},` +
		call + `,{"type":"function_call_output","call_id":"call_1","output":"found"}]`
	next := request(f.gateway, "POST", path, continuation+tools, token, session)
	if next.Code != http.StatusOK || !json.Valid(next.Body.Bytes()) || f.conversations.Load() != 1 {
		t.Fatalf("tool continuation: HTTP %d, conversations %d, body %s", next.Code, f.conversations.Load(), next.Body)
	}
}

func TestGeminiContinuesAfterParallelToolResults(t *testing.T) {
	const path = "/v1beta/models/naparnik:generateContent"
	const question = `{"role":"user","parts":[{"text":"question"}]}`
	const declarations = `,"tools":[{"functionDeclarations":[{"name":"lookup"}]}]}`
	const upstreamReply = `{"uuid":"tool-answer","role":"assistant","finished":true,"content":{"tool_calls":[` +
		`{"id":"first","function":{"name":"client_lookup","arguments":"{\"n\":1}"}},` +
		`{"id":"second","function":{"name":"client_lookup","arguments":"{\"n\":2}"}}]}}`
	for _, tc := range []struct{ name, firstPath, history string }{
		{"legacy", path, `{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"n":1}}},{"functionCall":{"name":"lookup","args":{"n":2}}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"n":1}}},{"functionResponse":{"name":"lookup","response":{"n":2}}}]}`},
		{"explicit SSE, out of order", "/v1beta/models/naparnik:streamGenerateContent", `{"role":"model","parts":[{"functionCall":{"id":"first","name":"lookup","args":{"n":1}}},{"functionCall":{"id":"second","name":"lookup","args":{"n":2}}}]},
			{"role":"function","parts":[{"functionResponse":{"id":"second","name":"lookup","response":{"n":2}}},{"functionResponse":{"id":"first","name":"lookup","response":{"n":1}}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "final", upstreamReply)
			first := request(f.gateway, "POST", tc.firstPath, `{"contents":[`+question+`]`+declarations, token, "gemini-parallel")
			if first.Code != 200 || !strings.Contains(first.Body.String(), `"id":"first"`) || !strings.Contains(first.Body.String(), `"id":"second"`) {
				t.Fatalf("initial calls: %d %s", first.Code, first.Body)
			}
			next := request(f.gateway, "POST", path, `{"contents":[`+question+","+tc.history+`]`+declarations, token, "gemini-parallel")
			if next.Code != 200 || f.conversations.Load() != 1 {
				t.Fatalf("tool results started another conversation: HTTP %d, conversations %d, body %s", next.Code, f.conversations.Load(), next.Body)
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
			w := request(f.gateway, "POST", "/v1/chat/completions", chat, token, fmt.Sprintf("session-%d", i%8))
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
	f := newFixture(t, "final", `{"uuid":"input","role":"user","finished":true}`, `{"uuid":"draft","role":"assistant","finished":false,"content_delta":{"content":"unfinished"}}`)
	w := request(f.gateway, "POST", "/v1/chat/completions", chat, token, "truncated")
	if w.Code != 502 {
		t.Fatalf("truncated upstream got %d, want 502: %s", w.Code, w.Body)
	}
}

func TestRegressionChatStreamKeepsCompletionID(t *testing.T) {
	f := newFixture(t, "deltas", finished)
	w := request(f.gateway, "POST", "/v1/chat/completions", `{"stream":true,"messages":[{"role":"user","content":"question"}]}`, token, "chat-ids")
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
	f := newFixture(t, "deltas", `{"uuid":"answer","role":"assistant","finished":true,"content":{"content":"ready","tool_calls":[{"id":"call_1","function":{"name":"client_lookup","arguments":"{}"}}]}}`)
	w := request(f.gateway, "POST", "/v1/responses", `{"input":"question","stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`, token, "ids")
	type itemEvents struct {
		added, delta, done, final string
		addedCount                int
	}
	items := make(map[int]itemEvents)
	for ev := range sse.Read(context.Background(), w.Body) {
		var data struct {
			Index    int                                    `json:"output_index"`
			ItemID   string                                 `json:"item_id"`
			Item     struct{ ID string }                    `json:"item"`
			Response struct{ Output []struct{ ID string } } `json:"response"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &data); err != nil {
			t.Fatal(err)
		}
		events := items[data.Index]
		switch ev.Name {
		case "response.output_item.added":
			events.addedCount++
			events.added = data.Item.ID
		case "response.output_text.delta", "response.function_call_arguments.delta":
			events.delta = data.ItemID
		case "response.output_item.done":
			events.done = data.Item.ID
		case "response.completed":
			for index, item := range data.Response.Output {
				events := items[index]
				events.final = item.ID
				items[index] = events
			}
			continue
		}
		items[data.Index] = events
	}
	for index := range 2 {
		e := items[index]
		if e.addedCount != 1 || e.added == "" || e.added != e.delta || e.added != e.done || e.added != e.final {
			t.Fatalf("output item %d has inconsistent events: %+v", index, e)
		}
	}
}
