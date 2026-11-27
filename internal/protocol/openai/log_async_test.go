package openai

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"naparnikadmin/internal/gateway"
	"naparnikadmin/internal/store"
)

// TestLogRequestDoesNotBlockOnLockedDatabase проверяет, что запись журнала
// не держит путь отправки ответа: при занятой SQLite вызов logRequest должен
// возвращаться сразу, а не ждать блокировку. Иначе готовый ответ доходит
// до клиента с задержкой и короткий таймаут обрывает его.
func TestLogRequestDoesNotBlockOnLockedDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	path := filepath.Join(t.TempDir(), "log.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

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

	h := &Handler{Store: st}
	p := &gateway.Principal{Token: store.Token{ID: 1, KeyID: 1}}

	start := time.Now()
	h.logRequest(p, "naparnik", 200, "", nil, time.Second, false, "вопрос")
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("запись журнала задержала ответ на %s", elapsed)
	}
}
