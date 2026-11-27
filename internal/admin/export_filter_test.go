package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"naparnikadmin/internal/store"
)

// TestExportLogsCSVRespectsFilter проверяет, что выгрузка отдаёт ровно тот
// отбор, который виден на экране: фильтр по токену и по периоду должен
// действовать и на CSV, а не только на список.
func TestExportLogsCSVRespectsFilter(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	keyID, _ := st.CreateKey(ctx, "К", "…1111", []byte{1})
	first, _ := st.CreateToken(ctx, store.Token{KeyID: keyID, Name: "Первый", Prefix: "nr_", ServerTools: "auto", Enabled: true}, "h1", "")
	second, _ := st.CreateToken(ctx, store.Token{KeyID: keyID, Name: "Второй", Prefix: "nr_", ServerTools: "auto", Enabled: true}, "h2", "")

	// Времена с точностью до секунды: в базе ts хранится целым числом.
	base := time.Unix(time.Now().Unix(), 0).Add(-2 * time.Hour)
	entries := []struct {
		at    time.Time
		token int64
		prompt string
	}{
		{base, first, "старый вопрос первого"},
		{base.Add(time.Hour), second, "свежий вопрос второго"},
	}
	for _, e := range entries {
		if err := st.LogRequest(ctx, store.LogEntry{
			TS: e.at, TokenID: e.token, KeyID: keyID, Protocol: "openai",
			Model: "naparnik", Status: 200, Prompt: e.prompt,
		}); err != nil {
			t.Fatal(err)
		}
	}

	s := &Server{Store: st}

	// Отбор по первому токену: в CSV только его запись.
	q := "?token_id=" + strconv.FormatInt(first, 10)
	rec := httptest.NewRecorder()
	s.exportLogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs/export"+q, nil))
	text := string(rec.Body.Bytes()[3:])
	if !strings.Contains(text, "старый вопрос первого") {
		t.Fatalf("в выгрузке нет записи первого токена: %q", text)
	}
	if strings.Contains(text, "свежий вопрос второго") {
		t.Fatalf("в выгрузку попала запись другого токена: %q", text)
	}

	// Отбор по периоду: окно накрывает только вторую запись.
	from := base.Add(30 * time.Minute).Format("2006-01-02T15:04")
	to := base.Add(90 * time.Minute).Format("2006-01-02T15:04")
	rec = httptest.NewRecorder()
	s.exportLogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs/export?from="+from+"&to="+to, nil))
	text = string(rec.Body.Bytes()[3:])
	if !strings.Contains(text, "свежий вопрос второго") {
		t.Fatalf("в выгрузке нет записи из окна периода: %q", text)
	}
	if strings.Contains(text, "старый вопрос первого") {
		t.Fatalf("в выгрузку попала запись вне окна периода: %q", text)
	}

	// Некорректный период: 400 и текст ошибки, а не пустой файл.
	rec = httptest.NewRecorder()
	s.exportLogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs/export?from=2026-11-27T16:00&to=2026-11-27T14:00", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("перевёрнутый период должен давать 400, получено %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "раньше") {
		t.Fatalf("ожидался понятный текст ошибки, получено %q", rec.Body.String())
	}
}
