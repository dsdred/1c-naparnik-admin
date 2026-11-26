package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("не удалось открыть базу: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestKeyLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, err := st.CreateKey(ctx, "Основной", "…abcd", []byte{1, 2, 3})
	if err != nil {
		t.Fatalf("создание ключа: %v", err)
	}
	keys, err := st.ListKeys(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("список ключей: %v, %d", err, len(keys))
	}
	if keys[0].Name != "Основной" || keys[0].KeyHint != "…abcd" {
		t.Fatalf("неверные поля ключа: %+v", keys[0])
	}
	enc, err := st.KeySecret(ctx, id)
	if err != nil || len(enc) != 3 {
		t.Fatalf("секрет ключа: %v", err)
	}
	if err := st.UpdateKeyStatus(ctx, id, "active", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateKeyMeta(ctx, id, "Новый", "Проф", true); err != nil {
		t.Fatal(err)
	}
	key, _ := st.GetKey(ctx, id)
	if key.Name != "Новый" || key.TariffLabel != "Проф" || !key.Disabled || key.Status != "active" {
		t.Fatalf("метаданные не обновились: %+v", key)
	}
	if err := st.DeleteKey(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetKey(ctx, id); err == nil {
		t.Fatal("ключ должен быть удалён")
	}
}

func TestTokensAndSessions(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	keyID, _ := st.CreateKey(ctx, "К", "…1111", []byte{1})

	id, err := st.CreateToken(ctx, Token{
		KeyID: keyID, Name: "Cline", Provider: "openai", Prefix: "nr_1234567",
		ServerTools: "auto", Enabled: true,
	}, "hash-1")
	if err != nil {
		t.Fatalf("создание токена: %v", err)
	}

	tok, err := st.ResolveToken(ctx, "hash-1")
	if err != nil || tok.ID != id || tok.KeyID != keyID {
		t.Fatalf("разрешение токена: %v %+v", err, tok)
	}
	if _, err := st.ResolveToken(ctx, "нет-такого"); err == nil {
		t.Fatal("ожидалась ошибка для неизвестного хеша")
	}

	ss := Session{
		TokenID: id, Protocol: "openai", SessionID: "s1",
		Conversation: "conv-1", Parent: "msg-1", ToolsJSON: `{"client_a":"a"}`,
		Fingerprint: "fp",
	}
	if err := st.SaveSession(ctx, ss); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSession(ctx, id, "openai", "s1")
	if err != nil || got.Conversation != "conv-1" || got.Parent != "msg-1" {
		t.Fatalf("сессия: %v %+v", err, got)
	}
	ss.Parent = "msg-2"
	if err := st.SaveSession(ctx, ss); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetSession(ctx, id, "openai", "s1")
	if got.Parent != "msg-2" {
		t.Fatalf("сессия не обновилась: %+v", got)
	}
	list, err := st.ListSessions(ctx, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("список сессий: %v %d", err, len(list))
	}
	if err := st.DeleteSession(ctx, id, "openai", "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSession(ctx, id, "openai", "s1"); err == nil {
		t.Fatal("сессия должна быть удалена")
	}

	if err := st.RevokeToken(ctx, id); err != nil {
		t.Fatal(err)
	}
	tok, _ = st.ResolveToken(ctx, "hash-1")
	if tok.Enabled || tok.RevokedAt == nil {
		t.Fatalf("токен не отозван: %+v", tok)
	}
}

func TestBudgetSamples(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	keyID, _ := st.CreateKey(ctx, "К", "…1111", []byte{1})

	now := time.Now()
	reset := now.Add(3 * time.Hour)
	if err := st.AddSample(ctx, Sample{KeyID: keyID, TS: now.Add(-time.Hour), Used5h: 10, UsedWeek: 5}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSample(ctx, Sample{KeyID: keyID, TS: now, Used5h: 42.5, UsedWeek: 18, Reset5h: &reset}); err != nil {
		t.Fatal(err)
	}

	latest, err := st.LatestSample(ctx, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Used5h != 42.5 || latest.Reset5h == nil {
		t.Fatalf("последний замер неверен: %+v", latest)
	}
	all, err := st.Samples(ctx, keyID, now.Add(-2*time.Hour))
	if err != nil || len(all) != 2 {
		t.Fatalf("замеры: %v %d", err, len(all))
	}
}

func TestUsageAggregates(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	keyID, _ := st.CreateKey(ctx, "К", "…1111", []byte{1})
	tokID, _ := st.CreateToken(ctx, Token{KeyID: keyID, Name: "T", Prefix: "nr_", ServerTools: "auto", Enabled: true}, "h")

	now := time.Now()
	for i, pt := range []int{100, 200} {
		err := st.LogRequest(ctx, LogEntry{
			TS: now.Add(time.Duration(i) * time.Minute), TokenID: tokID, KeyID: keyID,
			Protocol: "openai", Model: "naparnik", Status: 200,
			PromptTokens: pt, CompTokens: 50, CachedTokens: 10, LatencyMS: 1000,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	_ = st.LogRequest(ctx, LogEntry{
		TS: now, TokenID: tokID, KeyID: keyID, Protocol: "openai",
		Status: 429, ErrorType: "rate_limit_exceeded", LatencyMS: 10,
	})

	u, err := st.UsageForKey(ctx, keyID, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("агрегаты: %v", err)
	}
	if u.Requests != 3 || u.Errors != 1 || u.Prompt != 300 || u.Completion != 100 {
		t.Fatalf("неверные агрегаты: %+v", u)
	}
	if u.SuccessPct < 60 || u.SuccessPct > 70 {
		t.Fatalf("неверный процент успеха: %f", u.SuccessPct)
	}

	byToken, err := st.UsageByToken(ctx, keyID, now.Add(-time.Hour))
	if err != nil || len(byToken) != 1 || byToken[0].Requests != 3 {
		t.Fatalf("разбивка по токенам: %v %+v", err, byToken)
	}

	entries, total, err := st.QueryLog(ctx, LogFilter{KeyID: keyID, Limit: 10})
	if err != nil || total != 3 || len(entries) != 3 {
		t.Fatalf("журнал: %v total=%d len=%d", err, total, len(entries))
	}
}

func TestSettings(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, err := st.GetSetting(ctx, "нет"); err != ErrNotFound {
		t.Fatalf("ожидалась ErrNotFound, получено %v", err)
	}
	if err := st.SetSetting(ctx, "k", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "k", "v2"); err != nil {
		t.Fatal(err)
	}
	v, err := st.GetSetting(ctx, "k")
	if err != nil || v != "v2" {
		t.Fatalf("настройка: %v %q", err, v)
	}
}
