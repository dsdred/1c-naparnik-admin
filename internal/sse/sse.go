// Package sse — минимальный разбор и запись Server-Sent Events.
package sse

import (
	"bufio"
	"context"
	"io"
	"strings"
)

// Event — одно событие SSE.
type Event struct {
	Name string
	Data string
}

// Read разбирает поток SSE и отдаёт события в канал. Канал закрывается
// при EOF, ошибке чтения или отмене контекста.
func Read(ctx context.Context, r io.Reader) <-chan Event {
	out := make(chan Event)
	go func() {
		defer close(out)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

		var name string
		var data []string
		dispatch := func() bool {
			if len(data) == 0 && name == "" {
				return true
			}
			ev := Event{Name: name, Data: strings.Join(data, "\n")}
			name, data = "", nil
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}

		for scanner.Scan() {
			line := strings.TrimSuffix(scanner.Text(), "\r")
			switch {
			case line == "":
				if !dispatch() {
					return
				}
			case strings.HasPrefix(line, ":"):
				// комментарий/keep-alive — пропускаем
			default:
				field, value, found := strings.Cut(line, ":")
				if !found {
					continue
				}
				value = strings.TrimPrefix(value, " ")
				switch field {
				case "event":
					name = value
				case "data":
					data = append(data, value)
				}
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
		dispatch()
	}()
	return out
}
