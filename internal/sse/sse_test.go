package sse

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadReportsBrokenFrames(t *testing.T) {
	for name, tc := range map[string]struct {
		reader io.Reader
		want   error
	}{
		"incomplete frame": {strings.NewReader("data: incomplete\n"), io.ErrUnexpectedEOF},
		"read failure":     {io.MultiReader(strings.NewReader("data: incomplete\n"), failedReader{}), io.ErrClosedPipe},
	} {
		t.Run(name, func(t *testing.T) {
			var events []Event
			for event := range Read(context.Background(), tc.reader) {
				events = append(events, event)
			}
			if len(events) != 1 || !errors.Is(events[0].Err, tc.want) || events[0].Data != "" {
				t.Fatalf("expected %v without partial data, got %+v", tc.want, events)
			}
		})
	}
}

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestReadParsesEvents(t *testing.T) {
	input := ": keep-alive\n\n" +
		"event: message_start\n" +
		"data: {\"a\":1}\n\n" +
		"data: line1\n" +
		"data: line2\n\n" +
		"data: [DONE]\n\n"

	var got []Event
	for ev := range Read(context.Background(), strings.NewReader(input)) {
		got = append(got, ev)
	}
	if len(got) != 3 {
		t.Fatalf("ожидалось 3 события, получено %d: %+v", len(got), got)
	}
	if got[0].Name != "message_start" || got[0].Data != `{"a":1}` {
		t.Fatalf("неверное первое событие: %+v", got[0])
	}
	if got[1].Data != "line1\nline2" {
		t.Fatalf("многострочные данные склеены неверно: %q", got[1].Data)
	}
	if got[2].Data != "[DONE]" {
		t.Fatalf("неверное последнее событие: %+v", got[2])
	}
}

func TestReadHandlesCRLF(t *testing.T) {
	input := "data: a\r\n\r\ndata: b\r\n\r\n"
	var got []string
	for ev := range Read(context.Background(), strings.NewReader(input)) {
		got = append(got, ev.Data)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("CRLF разобран неверно: %+v", got)
	}
}

func TestReadStopsOnContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := Read(ctx, endlessReader{})
	<-ch
	cancel()
	for range ch {
	}
}

// endlessReader бесконечно отдаёт одно и то же событие.
type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) {
	return copy(p, "data: x\n\n"), nil
}
