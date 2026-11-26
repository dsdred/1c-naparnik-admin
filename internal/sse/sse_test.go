package sse

import (
	"context"
	"strings"
	"testing"
)

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
