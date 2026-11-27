package store

import (
	"context"
	"testing"
	"time"
)

// TestQueryLogPeriodAndToken проверяет отбор журнала по периоду «от/до»
// и по токену: фильтры применяются вместе и не теряют границы окна.
func TestQueryLogPeriodAndToken(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	keyID, _ := st.CreateKey(ctx, "К", "…1111", []byte{1})
	first, _ := st.CreateToken(ctx, Token{KeyID: keyID, Name: "Первый", Prefix: "nr_", ServerTools: "auto", Enabled: true}, "h1", "")
	second, _ := st.CreateToken(ctx, Token{KeyID: keyID, Name: "Второй", Prefix: "nr_", ServerTools: "auto", Enabled: true}, "h2", "")

	// Времена с точностью до секунды: в базе ts хранится целым числом.
	base := time.Unix(time.Now().Unix(), 0)
	for i, tok := range []int64{first, second, first} {
		if err := st.LogRequest(ctx, LogEntry{
			TS: base.Add(time.Duration(i) * time.Hour), TokenID: tok, KeyID: keyID,
			Protocol: "openai", Model: "naparnik", Status: 200, Prompt: "вопрос",
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Окно от 30 минут до 90 минут накрывает только вторую запись.
	entries, total, err := st.QueryLog(ctx, LogFilter{
		Since: base.Add(30 * time.Minute), Until: base.Add(90 * time.Minute), Limit: 10,
	})
	if err != nil || total != 1 || len(entries) != 1 {
		t.Fatalf("окно периода: %v total=%d len=%d", err, total, len(entries))
	}
	if entries[0].TokenID != second {
		t.Fatalf("в окно попала чужая запись: token=%d", entries[0].TokenID)
	}

	// Тот же период, но с отбором по первому токену — записей быть не должно.
	_, total, err = st.QueryLog(ctx, LogFilter{
		TokenID: first, Since: base.Add(30 * time.Minute), Until: base.Add(90 * time.Minute), Limit: 10,
	})
	if err != nil || total != 0 {
		t.Fatalf("отбор по токену внутри окна: %v total=%d", err, total)
	}

	// Границы включительные: от самой записи до неё же.
	_, total, err = st.QueryLog(ctx, LogFilter{Since: base, Until: base, Limit: 10})
	if err != nil || total != 1 {
		t.Fatalf("границы окна должны включать запись: %v total=%d", err, total)
	}
}
