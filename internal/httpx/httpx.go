// Package httpx — общие HTTP-утилиты: ленивый SSE-writer и JSON-ответы.
package httpx

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"
)

// Writer пишет SSE-поток. Заголовки отправляются при первом Write —
// это позволяет до первого байта вернуть обычную JSON-ошибку с нужным кодом.
type Writer struct {
	w       http.ResponseWriter
	flusher http.Flusher
	mu      sync.Mutex
	started bool
	closed  bool
	stop    chan struct{}
	done    chan struct{}
}

// NewWriter создаёт писатель потока.
func NewWriter(w http.ResponseWriter) *Writer {
	f, _ := w.(http.Flusher)
	return &Writer{w: w, flusher: f, stop: make(chan struct{}), done: make(chan struct{})}
}

// Start принудительно открывает поток (нужно, когда ждём долгий серверный вызов).
func (s *Writer) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.start()
}

func (s *Writer) start() {
	if !s.started && !s.closed {
		h := s.w.Header()
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
		s.w.WriteHeader(http.StatusOK)
		s.started = true
		if s.flusher != nil {
			s.flusher.Flush()
		}
		go s.ping()
	}
}

func (s *Writer) ping() {
	defer close(s.done)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if !s.writePing() {
				return
			}
		case <-s.stop:
			return
		}
	}
}

func (s *Writer) writePing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if _, err := s.w.Write([]byte(": ping\n\n")); err != nil {
		return false
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return true
}

// Started сообщает, были ли уже отправлены заголовки.
func (s *Writer) Started() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// Event пишет событие: имя (опционально) и данные.
func (s *Writer) Event(name, data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.start()
	if name != "" {
		_, _ = s.w.Write([]byte("event: " + name + "\n"))
	}
	_, _ = s.w.Write([]byte("data: " + data + "\n\n"))
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// Raw пишет уже готовый блок (например, ": ping\n\n").
func (s *Writer) Raw(data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.start()
	_, _ = s.w.Write([]byte(data))
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// Close останавливает пинги.
func (s *Writer) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.stop)
	}
	started := s.started
	s.mu.Unlock()
	if started {
		<-s.done
	}
}

// JSON отправляет JSON-ответ.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// ReadJSON читает тело запроса в v, ограничивая размер и проверяя UTF-8.
// Проверка важна на Windows: консольные клиенты часто отправляют cp1251,
// и без неё в Напарника уходит «кракозябра» вместо текста.
func ReadJSON(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	if limit <= 0 {
		limit = 16 << 20
	}
	body := http.MaxBytesReader(w, r.Body, limit)
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if !utf8.Valid(data) {
		return errNotUTF8
	}
	return json.Unmarshal(data, v)
}

// ErrNotUTF8 возвращается, если тело запроса не является корректным UTF-8.
var ErrNotUTF8 = errNotUTF8

type utf8Error struct{}

func (utf8Error) Error() string {
	return "тело запроса не является корректным UTF-8 (вероятно, клиент отправил cp1251)"
}

var errNotUTF8 = utf8Error{}
