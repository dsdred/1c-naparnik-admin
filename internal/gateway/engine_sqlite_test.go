package gateway

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"naparnikadmin/internal/config"
	"naparnikadmin/internal/naparnik"
	"naparnikadmin/internal/store"
)

func TestExecuteKeepsGeneratedReplyWhenSQLiteIsLocked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "gateway.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	keyID, err := st.CreateKey(ctx, "test", "test", []byte("test"))
	if err != nil {
		t.Fatal(err)
	}
	tokenID, err := st.CreateToken(ctx, store.Token{KeyID: keyID, Name: "test"}, "test-hash", "")
	if err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/conversations":
			_, _ = io.WriteString(w, `{"uuid":"dialog"}`)
		case "/conversations/dialog/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: "+`{"uuid":"reply","role":"assistant","finished":true,"content":{"content":"ready"}}`+"\n\n")
		case "/conversations/dialog/context_tokens":
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("неожиданный запрос: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

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
	defer func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") }()

	e := New(naparnik.New(upstream.URL, 3*time.Second), st, &config.Config{})
	var logs bytes.Buffer
	e.Log = slog.New(slog.NewTextHandler(&logs, nil))
	in := Input{Key: "test", Token: store.Token{ID: tokenID}, Protocol: "openai", SessionID: "locked", Turns: []Turn{{Role: RoleUser, Text: "ping"}}}
	start := time.Now()
	reply, err := e.Execute(ctx, in)
	t.Logf("ответ при удерживаемой блокировке: %s", time.Since(start).Round(time.Millisecond))
	if err != nil {
		t.Fatalf("сгенерированный ответ потерян при блокировке базы: %v", err)
	}
	if reply == nil || reply.Text() != "ready" {
		t.Fatalf("сгенерированный ответ не дошёл: %+v", reply)
	}
	if elapsed := time.Since(start); elapsed >= 9*time.Second {
		t.Fatalf("две вспомогательные записи не должны ждать блокировку последовательно: %s", elapsed)
	}
	if got := logs.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "не удалось сохранить сессию") || strings.Count(got, "level=WARN") != 1 {
		t.Fatalf("ожидалось одно предупреждение о сохранении сессии: %s", got)
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}

	in.SessionID = "unlocked"
	if _, err := e.Execute(ctx, in); err != nil {
		t.Fatal(err)
	}
	binding, err := st.GetSession(ctx, tokenID, in.Protocol, in.SessionID)
	if err != nil || binding.Parent != "reply" {
		t.Fatalf("привязка не сохранена без блокировки: %+v, %v", binding, err)
	}
	cp, ok, err := st.FindCheckpoint(ctx, tokenID, in.Protocol, in.SessionID, answerDigest("ready", nil))
	if err != nil || !ok || cp.Parent != "reply" {
		t.Fatalf("контрольная точка не сохранена без блокировки: %+v, %v, %v", cp, ok, err)
	}

	// Отдельная ошибка контрольной точки тоже не должна терять готовый ответ
	// и не должна мешать сохранению привязки сессии.
	if _, err := conn.ExecContext(ctx, `CREATE TRIGGER fail_checkpoint BEFORE INSERT ON session_checkpoints
		BEGIN SELECT RAISE(ABORT, 'checkpoint write failed'); END`); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	in.SessionID = "checkpoint-error"
	reply, err = e.Execute(ctx, in)
	if err != nil || reply == nil || reply.Text() != "ready" {
		t.Fatalf("ответ потерян при ошибке контрольной точки: %+v, %v", reply, err)
	}
	binding, err = st.GetSession(ctx, tokenID, in.Protocol, in.SessionID)
	if err != nil || binding.Parent != "reply" {
		t.Fatalf("ошибка контрольной точки помешала сохранить сессию: %+v, %v", binding, err)
	}
	if got := logs.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "не удалось сохранить контрольную точку") {
		t.Fatalf("нет предупреждения об ошибке контрольной точки: %s", got)
	}
}
