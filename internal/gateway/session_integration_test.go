package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"naparnikadmin/internal/config"
	"naparnikadmin/internal/naparnik"
	"naparnikadmin/internal/secrets"
	"naparnikadmin/internal/store"
)

func TestAuthDoesNotWaitForTokenTimestampOnLockedSQLite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cipher, err := secrets.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	key, err := cipher.Encrypt("fixture-key")
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := st.CreateKey(ctx, "fixture", "fixture", key)
	if err != nil {
		t.Fatal(err)
	}
	const token = "fixture-token"
	hash := secrets.HashToken(token)
	if _, err := st.CreateToken(ctx, store.Token{KeyID: keyID, Name: "fixture", Enabled: true}, hash, ""); err != nil {
		t.Fatal(err)
	}
	locker, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	conn, err := locker.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(ctx, "ROLLBACK")
	auth := &Auth{Store: st, Cipher: cipher}
	start := time.Now()
	if _, err := auth.Resolve(ctx, token); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("token timestamp blocked authentication for %v", elapsed)
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Resolve(ctx, token); err != nil {
		t.Fatal(err)
	}
	touched, err := st.ResolveToken(ctx, hash)
	if err != nil || touched.LastUsedAt == nil {
		t.Fatalf("last usage was not recorded after releasing the lock: %v", err)
	}
}

func TestSessionCheckpointsRespectResetAndExpiry(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprintf("reset=%t", reset), func(t *testing.T) {
			ctx := context.Background()
			st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			keyID, err := st.CreateKey(ctx, "test", "test", []byte("fixture"))
			if err != nil {
				t.Fatal(err)
			}
			token := store.Token{KeyID: keyID, Name: "test"}
			token.ID, err = st.CreateToken(ctx, token, "hash", "")
			if err != nil {
				t.Fatal(err)
			}

			var conversations atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/conversations":
					fmt.Fprintf(w, `{"uuid":"dialog-%d"}`, conversations.Add(1))
				case strings.HasSuffix(r.URL.Path, "/messages"):
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: "+`{"uuid":"msg","role":"assistant","finished":true,"content":{"content":"ready"}}`+"\n\n")
				case strings.HasSuffix(r.URL.Path, "/context_tokens"):
					io.WriteString(w, "{}")
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(upstream.Close)
			cfg := config.Default()
			engine := New(naparnik.New(upstream.URL, time.Second), st, cfg)
			input := Input{Token: token, Key: "fixture", Protocol: "openai", SessionID: "session",
				Turns: []Turn{{Role: RoleUser, Text: "question"}}}
			if _, err := engine.Execute(ctx, input); err != nil {
				t.Fatal(err)
			}
			if reset {
				if err := engine.ResetSession(ctx, token.ID, input.Protocol, input.SessionID); err != nil {
					t.Fatal(err)
				}
				if count, err := st.CountCheckpoints(ctx, token.ID, input.Protocol, input.SessionID); err != nil || count != 0 {
					t.Fatalf("reset left %d checkpoints: %v", count, err)
				}
			} else {
				cfg.SessionTTL = config.Duration(time.Nanosecond)
			}
			input.Turns = append(input.Turns, Turn{Role: RoleAssistant, Text: "ready"}, Turn{Role: RoleUser, Text: "next"})
			if _, err := engine.Execute(ctx, input); err != nil {
				t.Fatal(err)
			}
			if got := conversations.Load(); got != 2 {
				t.Fatalf("created %d upstream conversations, want 2", got)
			}
			cp, found, err := st.FindCheckpoint(ctx, token.ID, input.Protocol, input.SessionID, answerDigest("ready", nil))
			if err != nil || !found || cp.Conversation != "dialog-2" {
				t.Fatalf("replacement checkpoint: %+v, found=%v, err=%v", cp, found, err)
			}
		})
	}
}

func TestAuthHonorsTokenProtocols(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cipher, err := secrets.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	key, err := cipher.Encrypt("fixture")
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := st.CreateKey(ctx, "test", "test", key)
	if err != nil {
		t.Fatal(err)
	}
	auth := &Auth{Store: st, Cipher: cipher}
	for _, tc := range []struct {
		name, configured string
		allowed          []string
		want             int
	}{
		{"unrestricted", "", []string{"gemini"}, http.StatusOK},
		{"matching", "openai", []string{"openai"}, http.StatusOK},
		{"model listing", "responses", []string{"openai", "responses"}, http.StatusOK},
		{"nonmatching", "responses", []string{"openai"}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plain := "fixture-" + tc.name
			_, err := st.CreateToken(ctx, store.Token{KeyID: keyID, Name: tc.name, Enabled: true, Provider: tc.configured}, secrets.HashToken(plain), "")
			if err != nil {
				t.Fatal(err)
			}
			_, err = auth.Resolve(ctx, plain, tc.allowed...)
			status := http.StatusOK
			if err != nil {
				status = FromError(err).Status
			}
			if status != tc.want {
				t.Fatalf("got status %d, want %d: %v", status, tc.want, err)
			}
		})
	}
}
