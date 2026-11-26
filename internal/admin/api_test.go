package admin

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"naparnikadmin/internal/config"
	"naparnikadmin/internal/store"
)

func TestNormalizeModelID(t *testing.T) {
	ok := map[string]string{
		" naparnik ":       "naparnik",
		"naparnik-pro":     "naparnik-pro",
		"vendor/model:1.2": "vendor/model:1.2",
		"my_model.v2":      "my_model.v2",
		"NaparnikAdmin":    "NaparnikAdmin",
	}
	for in, want := range ok {
		got, err := normalizeModelID(in)
		if err != nil || got != want {
			t.Fatalf("%q: got %q err=%v, want %q", in, got, err, want)
		}
	}
	bad := []string{"", "  ", "напарник", "two words", "quote\"", "a/b?c", "x\ny"}
	for _, in := range bad {
		if _, err := normalizeModelID(in); err == nil {
			t.Fatalf("%q: ожидалась ошибка", in)
		}
	}
	long := make([]rune, 65)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := normalizeModelID(string(long)); err == nil {
		t.Fatal("слишком длинный id должен отклоняться")
	}
}

func TestValidateListeners(t *testing.T) {
	base := []config.Listener{
		{Addr: "127.0.0.1:47830", Kind: config.KindAdmin},
		{Addr: "127.0.0.1:47831", Kind: config.KindGateway},
	}
	if err := validateListeners(base); err != nil {
		t.Fatalf("корректный набор отклонён: %v", err)
	}

	offGateway := []config.Listener{
		{Addr: "127.0.0.1:47830", Kind: config.KindAdmin},
		{Addr: "127.0.0.1:47831", Kind: config.KindGateway, Disabled: true},
	}
	if err := validateListeners(offGateway); err != nil {
		t.Fatalf("выключенный шлюз должен быть допустим: %v", err)
	}

	offAdmin := []config.Listener{
		{Addr: "127.0.0.1:47830", Kind: config.KindAdmin, Disabled: true},
		{Addr: "127.0.0.1:47831", Kind: config.KindGateway},
	}
	if err := validateListeners(offAdmin); err == nil {
		t.Fatal("последнюю админку выключать нельзя")
	}

	if err := validateListeners([]config.Listener{{Addr: "127.0.0.1:47830", Kind: config.KindAdmin, Disabled: true}}); err == nil {
		t.Fatal("без включённых слушателей набор недопустим")
	}

	offDup := []config.Listener{
		{Addr: "127.0.0.1:47830", Kind: config.KindAdmin},
		{Addr: "127.0.0.1:47830", Kind: config.KindGateway, Disabled: true},
	}
	if err := validateListeners(offDup); err != nil {
		t.Fatalf("дубль с выключенным слушателем допустим: %v", err)
	}

	dup := []config.Listener{
		{Addr: "127.0.0.1:47830", Kind: config.KindAdmin},
		{Addr: "127.0.0.1:47830", Kind: config.KindGateway},
	}
	if err := validateListeners(dup); err == nil {
		t.Fatal("дубль адреса должен отклоняться")
	}

	bad := []config.Listener{{Addr: "не адрес", Kind: config.KindAdmin}}
	if err := validateListeners(bad); err == nil {
		t.Fatal("неверный адрес должен отклоняться")
	}
}

func TestExportLogsCSV(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	keyID, _ := st.CreateKey(ctx, "К", "…1111", []byte{1})
	tokID, _ := st.CreateToken(ctx, store.Token{KeyID: keyID, Name: "Cline", Prefix: "nr_", ServerTools: "auto", Enabled: true}, "h", "")
	if err := st.LogRequest(ctx, store.LogEntry{
		TS: time.Now(), TokenID: tokID, KeyID: keyID, Protocol: "openai", Model: "naparnik",
		Status: 200, Prompt: "вопрос; с точкой с запятой\nи переводом строки",
	}); err != nil {
		t.Fatal(err)
	}

	s := &Server{Store: st}
	rec := httptest.NewRecorder()
	s.exportLogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs/export", nil))

	body := rec.Body.Bytes()
	if !bytes.HasPrefix(body, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatal("нет BOM — Excel не поймёт UTF-8")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/csv") {
		t.Fatalf("content-type: %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".csv") {
		t.Fatalf("content-disposition: %q", cd)
	}
	text := string(body[3:])
	if !strings.HasPrefix(text, "Время;Токен;") {
		t.Fatalf("неверная шапка: %q", strings.SplitN(text, "\n", 2)[0])
	}
	if !strings.Contains(text, "Cline") || !strings.Contains(text, `"вопрос; с точкой с запятой`) {
		t.Fatalf("в CSV нет данных запроса: %q", text)
	}
}

func TestParseDurationSetting(t *testing.T) {
	if _, ok, err := parseDurationSetting("X", "   ", time.Second); err != nil || ok {
		t.Fatalf("пустое значение должно означать «не менять»: ok=%v err=%v", ok, err)
	}
	v, ok, err := parseDurationSetting("X", "5m", 30*time.Second)
	if err != nil || !ok || v.D() != 5*time.Minute {
		t.Fatalf("5m: v=%v ok=%v err=%v", v, ok, err)
	}
	if _, _, err := parseDurationSetting("X", "каждые 5 минут", time.Second); err == nil {
		t.Fatal("текст без единицы должен отклоняться")
	}
	if _, _, err := parseDurationSetting("X", "5s", 30*time.Second); err == nil {
		t.Fatal("значение меньше минимума должно отклоняться")
	}
}
