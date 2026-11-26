package admin

import "testing"

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
