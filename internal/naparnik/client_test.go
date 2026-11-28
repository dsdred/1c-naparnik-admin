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

func TestPinnedEndpointPreservesAnOngoingTurn(t *testing.T) {
	previous := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"limits":{"sliding_5h":{"used_percent":10}}}`)
	}))
	defer previous.Close()
	replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"limits":{"sliding_5h":{"used_percent":20}}}`)
	}))
	defer replacement.Close()
	base := previous.URL
	client := NewWithSettings(func() (string, time.Duration) { return base, time.Second })
	ongoing := client.PinEndpoint(context.Background())
	base = replacement.URL
	for _, tc := range []struct {
		ctx  context.Context
		want float64
	}{
		{ongoing, 10},
		{context.Background(), 20},
	} {
		budget, err := client.Budget(tc.ctx, "fixture")
		if err != nil || budget.Limits.Sliding5h.UsedPercent != tc.want {
			t.Fatalf("wrong upstream: %+v, %v", budget, err)
		}
	}
}
