package monitor

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"naparnikadmin/internal/config"
	"naparnikadmin/internal/naparnik"
	"naparnikadmin/internal/secrets"
	"naparnikadmin/internal/store"
)

func TestPollerReschedulesWhenIntervalChanges(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cipher, err := secrets.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	key, err := cipher.Encrypt("fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateKey(context.Background(), "fixture", "fixture", key); err != nil {
		t.Fatal(err)
	}
	polls := make(chan struct{}, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/budget" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		select {
		case polls <- struct{}{}:
		case <-r.Context().Done():
		}
		io.WriteString(w, "{}")
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.BudgetPollInterval = config.Duration(time.Hour)
	current := config.NewRuntime(cfg)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	poller := New(st, naparnik.New(upstream.URL, time.Second), cipher, current, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	poller.Start(ctx)
	awaitPoll := func() {
		t.Helper()
		select {
		case <-polls:
		case <-time.After(3 * time.Second):
			t.Fatal("scheduled budget poll did not run")
		}
	}
	setInterval := func(interval time.Duration) {
		t.Helper()
		if err := current.Update(func(next *config.Config) error {
			next.BudgetPollInterval = config.Duration(interval)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	awaitPoll()
	setInterval(80 * time.Millisecond)
	awaitPoll()
	setInterval(time.Hour)
	select {
	case <-polls:
		t.Fatal("longer interval did not postpone the next poll")
	case <-time.After(200 * time.Millisecond):
	}
}
