package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"naparnikadmin/internal/config"
	"naparnikadmin/internal/naparnik"
	"naparnikadmin/internal/store"
)

func TestRecoverAcceptedTurnAfterSSEEnds(t *testing.T) {
	for _, test := range []struct {
		name, role string
		appendUser bool
	}{
		{"retry user message", RoleUser, false},
		{"retry tool result", RoleTool, false},
		{"continue with new message", RoleUser, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			role := test.role
			var posts, creates, reads atomic.Int64
			firstCall := naparnik.NewToolCall("tool-a", WireName("lookup"), "{}")
			firstReply := naparnik.AssistantContent{ToolCalls: []naparnik.ToolCall{firstCall}}
			result := naparnik.ToolResult{ToolCallID: firstCall.ID, Status: "ok", Content: "value"}
			accepted := "accepted-user"
			if role == RoleTool {
				accepted = "accepted-tool"
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/conversations":
					creates.Add(1)
					fmt.Fprint(w, `{"uuid":"dialog"}`)
				case "/conversations/dialog/messages":
					count := posts.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					if role == RoleTool && count == 1 {
						testSSE(t, w, naparnik.Packet{UUID: "tool-branch", Role: RoleAssistant, Finished: true, Content: &firstReply})
						return
					}
					if test.appendUser && count == 2 {
						var request naparnik.MessageRequest
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.ParentUUID == nil ||
							*request.ParentUUID != "saved-reply" || request.Content != "follow up" {
							t.Errorf("expected only the new message after the recovered parent: %+v, %v", request, err)
						}
						testSSE(t, w, naparnik.Packet{UUID: "final", Role: RoleAssistant, Finished: true,
							Content: &naparnik.AssistantContent{Content: "follow-up answer"}})
						return
					}
					testSSE(t, w, naparnik.Packet{UUID: accepted, Role: role, Finished: true})
				case "/conversations/dialog":
					var content any = "question"
					if role == RoleTool {
						content = result
					}
					messages := []map[string]any{{"uuid": accepted, "role": role, "content": content}}
					if reads.Add(1) > 1 {
						messages = append(messages, map[string]any{
							"uuid": "saved-reply", "parent_uuid": accepted,
							"role": RoleAssistant, "content": map[string]string{"content": "recovered answer"},
						})
					}
					json.NewEncoder(w).Encode(map[string]any{"messages": messages})
				case "/conversations/dialog/context_tokens":
					fmt.Fprint(w, "{}")
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(upstream.Close)
			engine, in := recoveryFixture(t, upstream.URL)
			if role == RoleTool {
				reply, err := engine.Execute(context.Background(), in)
				if err != nil || len(reply.ToolCalls()) != 1 {
					t.Fatalf("initial tool call: %v, %v", reply, err)
				}
				in.Turns = append(in.Turns, Turn{Role: RoleAssistant, Calls: reply.ToolCalls()},
					Turn{Role: RoleTool, Results: []naparnik.ToolResult{result}})
			}
			if _, err := engine.Execute(context.Background(), in); err == nil {
				t.Fatal("interrupted stream unexpectedly succeeded")
			}
			before := posts.Load()
			// A fresh engine must recover from SQLite after a service restart.
			engine = New(engine.client, engine.store, engine.cfg)
			if _, err := engine.Execute(context.Background(), in); err == nil || FromError(err).Code != "recovery_pending" {
				t.Fatalf("unfinished accepted turn must not be resent: %v", err)
			}
			var delivered string
			in.OnText = func(text string) { delivered += text }
			want := "recovered answer"
			wantPosts := before
			if test.appendUser {
				in.Turns = append(in.Turns, Turn{Role: RoleUser, Text: "follow up"})
				want, wantPosts = "follow-up answer", before+1
			}
			reply, err := engine.Execute(context.Background(), in)
			if err != nil || reply.Text() != want || delivered != reply.Text() {
				t.Fatalf("recovered reply: %v, %v, stream=%q", reply, err, delivered)
			}
			if posts.Load() != wantPosts || creates.Load() != 1 {
				t.Fatalf("accepted turn was resent: posts %d -> %d, creates %d", before, posts.Load(), creates.Load())
			}
		})
	}
}

func TestParallelToolResultsKeepOriginalBranchWhenReordered(t *testing.T) {
	var posts atomic.Int64
	calls := []naparnik.ToolCall{
		naparnik.NewToolCall("tool-b", WireName("lookup"), `{"n":2}`),
		naparnik.NewToolCall("tool-a", WireName("lookup"), `{"n":1}`),
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/conversations":
			fmt.Fprint(w, `{"uuid":"dialog"}`)
		case "/conversations/dialog/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			if posts.Add(1) == 1 {
				testSSE(t, w, naparnik.Packet{UUID: "tool-branch", Role: RoleAssistant, Finished: true,
					Content: &naparnik.AssistantContent{ToolCalls: calls}})
				return
			}
			var request struct {
				Parent string                `json:"parent_uuid"`
				Role   string                `json:"role"`
				Items  []naparnik.ToolResult `json:"content"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("expected structured results, got %v", err)
				http.Error(w, "expected tool results", http.StatusBadRequest)
				return
			}
			if request.Parent != "tool-branch" || request.Role != "tool" || len(request.Items) != 2 ||
				request.Items[0].ToolCallID != "tool-a" || request.Items[0].Content != "result-a" ||
				request.Items[1].ToolCallID != "tool-b" || request.Items[1].Content != "result-b" {
				t.Errorf("wrong branch or mismatched results: %+v", request)
			}
			testSSE(t, w, naparnik.Packet{UUID: "done", Role: RoleAssistant, Finished: true,
				Content: &naparnik.AssistantContent{Content: "both results received"}})
		case "/conversations/dialog/context_tokens":
			fmt.Fprint(w, "{}")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	engine, in := recoveryFixture(t, upstream.URL)
	reply, err := engine.Execute(context.Background(), in)
	if err != nil || len(reply.ToolCalls()) != 2 {
		t.Fatalf("initial tool calls: %v, %v", reply, err)
	}
	got := reply.ToolCalls()
	in.Turns = append(in.Turns, Turn{Role: RoleAssistant, Calls: []naparnik.ToolCall{got[1], got[0]}},
		Turn{Role: RoleTool, Results: []naparnik.ToolResult{{ToolCallID: "tool-a", Status: "ok", Content: "result-a"}}},
		Turn{Role: RoleTool, Results: []naparnik.ToolResult{{ToolCallID: "tool-b", Status: "ok", Content: "result-b"}}})
	reply, err = engine.Execute(context.Background(), in)
	if err != nil || reply.Text() != "both results received" || posts.Load() != 2 {
		t.Fatalf("parallel results: %v, %v, posts=%d", reply, err, posts.Load())
	}
}

func testSSE(t *testing.T, w http.ResponseWriter, p naparnik.Packet) {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"uuid": p.UUID, "role": p.Role, "finished": p.Finished, "content": p.Content,
	})
	if err != nil {
		t.Error(err)
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func recoveryFixture(t *testing.T, base string) (*Engine, Input) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	keyID, err := st.CreateKey(ctx, "test", "test", []byte("fixture"))
	if err != nil {
		t.Fatal(err)
	}
	tokenID, err := st.CreateToken(ctx, store.Token{KeyID: keyID, Name: "test"}, "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	return New(naparnik.New(base, time.Second), st, config.Default()), Input{
		Token: store.Token{ID: tokenID}, Key: "fixture", Protocol: "openai", SessionID: "test",
		Turns: []Turn{{Role: RoleUser, Text: "question"}}, Tools: []Tool{{Name: "lookup"}},
	}
}
