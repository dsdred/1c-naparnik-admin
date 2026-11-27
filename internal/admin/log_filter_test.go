package admin

import (
	"net/url"
	"testing"
	"time"
)

// TestParseLogFilterPeriod проверяет разбор периода «от/до» из параметров
// запроса: форматы поля datetime-local, границы окна и отказ на мусор.
func TestParseLogFilterPeriod(t *testing.T) {
	f, err := parseLogFilter(url.Values{
		"from":     {"2026-11-27T14:30"},
		"to":       {"2026-11-27T16:45"},
		"token_id": {"7"},
	})
	if err != nil {
		t.Fatalf("корректный фильтр отклонён: %v", err)
	}
	wantFrom := time.Date(2026, 11, 27, 14, 30, 0, 0, time.Local)
	// «до» включает указанную минуту целиком.
	wantTo := time.Date(2026, 11, 27, 16, 45, 59, 0, time.Local)
	if !f.Since.Equal(wantFrom) {
		t.Fatalf("от: получено %v, ожидалось %v", f.Since, wantFrom)
	}
	if !f.Until.Equal(wantTo) {
		t.Fatalf("до: получено %v, ожидалось %v", f.Until, wantTo)
	}
	if f.TokenID != 7 {
		t.Fatalf("token_id: получено %d", f.TokenID)
	}

	// Дата без времени как верхняя граница — весь день целиком.
	f, err = parseLogFilter(url.Values{"to": {"2026-11-27"}})
	if err != nil {
		t.Fatalf("дата без времени отклонена: %v", err)
	}
	wantEnd := time.Date(2026, 11, 27, 23, 59, 59, 0, time.Local)
	if !f.Until.Equal(wantEnd) {
		t.Fatalf("конец дня: получено %v, ожидалось %v", f.Until, wantEnd)
	}

	// Пустые значения — без ограничений.
	f, err = parseLogFilter(url.Values{"from": {"  "}, "to": {""}})
	if err != nil || !f.Since.IsZero() || !f.Until.IsZero() {
		t.Fatalf("пустой фильтр: %v since=%v until=%v", err, f.Since, f.Until)
	}

	// «до» раньше «от» — ошибка, а не молча пустой список.
	if _, err := parseLogFilter(url.Values{"from": {"2026-11-27T16:00"}, "to": {"2026-11-27T14:00"}}); err == nil {
		t.Fatal("перевёрнутый период должен отклоняться")
	}

	// Мусор вместо даты.
	if _, err := parseLogFilter(url.Values{"from": {"вчера"}}); err == nil {
		t.Fatal("неразборчивая дата должна отклоняться")
	}
	if _, err := parseLogFilter(url.Values{"to": {"27.11.2026"}}); err == nil {
		t.Fatal("формат dd.mm.yyyy не поддерживается — ожидалась ошибка")
	}
}
