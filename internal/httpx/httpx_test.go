package httpx

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestEventAndKeepAliveWriteWholeFrames(t *testing.T) {
	rec := httptest.NewRecorder()
	writer := NewWriter(rec)
	writer.Start()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 100 {
			writer.Event("message", "ready")
		}
	}()
	go func() {
		defer wg.Done()
		for range 100 {
			writer.writePing()
		}
	}()
	wg.Wait()
	writer.Close()
	frames := strings.Split(strings.TrimSuffix(rec.Body.String(), "\n\n"), "\n\n")
	if len(frames) != 200 {
		t.Fatalf("got %d frames, want 200", len(frames))
	}
	for _, frame := range frames {
		if frame != ": ping" && frame != "event: message\ndata: ready" {
			t.Fatalf("interleaved frame: %q", frame)
		}
	}
}
