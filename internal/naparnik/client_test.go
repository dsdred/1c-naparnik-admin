package naparnik

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestUsesUpdatedHeaderTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(180 * time.Millisecond):
			io.WriteString(w, "{}")
		}
	}))
	defer upstream.Close()
	var timeout atomic.Int64
	timeout.Store(int64(40 * time.Millisecond))
	client := NewWithTimeout(upstream.URL, func() time.Duration { return time.Duration(timeout.Load()) })
	_, err := client.Budget(context.Background(), "fixture")
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("expected short header timeout, got %v", err)
	}
	timeout.Store(int64(3 * time.Second))
	if _, err := client.Budget(context.Background(), "fixture"); err != nil {
		t.Fatalf("updated timeout did not take effect: %v", err)
	}
}
