package admin

import (
	"testing"

	"naparnikadmin/internal/config"
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
