package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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
	}, "hash-1", "enc-1")
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
	tokID, _ := st.CreateToken(ctx, Token{KeyID: keyID, Name: "T", Prefix: "nr_", ServerTools: "auto", Enabled: true}, "h", "")

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

func TestLogPromptAndPurge(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	keyID, _ := st.CreateKey(ctx, "К", "…1111", []byte{1})
	tokID, _ := st.CreateToken(ctx, Token{KeyID: keyID, Name: "T", Prefix: "nr_", ServerTools: "auto", Enabled: true}, "h", "")

	now := time.Now()
	long := strings.Repeat("я", promptLimit+50)
	for _, ts := range []time.Time{now.AddDate(0, 0, -10), now} {
		if err := st.LogRequest(ctx, LogEntry{
			TS: ts, TokenID: tokID, KeyID: keyID, Protocol: "openai", Model: "naparnik",
			Status: 200, Prompt: long,
		}); err != nil {
			t.Fatal(err)
		}
	}

	entries, total, err := st.QueryLog(ctx, LogFilter{})
	if err != nil || total != 2 {
		t.Fatalf("журнал: %v total=%d", err, total)
	}
	if got := utf8.RuneCountInString(entries[0].Prompt); got != promptLimit {
		t.Fatalf("вопрос должен обрезаться до %d символов, получено %d", promptLimit, got)
	}

	deleted, err := st.PurgeLog(ctx, now.AddDate(0, 0, -5))
	if err != nil || deleted != 1 {
		t.Fatalf("удаление старых: deleted=%d err=%v", deleted, err)
	}
	_, total, _ = st.QueryLog(ctx, LogFilter{})
	if total != 1 {
		t.Fatalf("должна остаться одна свежая запись, осталось %d", total)
	}

	deleted, err = st.PurgeLog(ctx, time.Time{})
	if err != nil || deleted != 1 {
		t.Fatalf("полная очистка: deleted=%d err=%v", deleted, err)
	}
	_, total, _ = st.QueryLog(ctx, LogFilter{})
	if total != 0 {
		t.Fatalf("журнал должен быть пуст, осталось %d", total)
	}
}

func TestCheckpointRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	keyID, err := st.CreateKey(ctx, "Ключ", "…abcd", []byte{1})
	if err != nil {
		t.Fatalf("ключ: %v", err)
	}
	tokenID, err := st.CreateToken(ctx, Token{KeyID: keyID, Name: "IDE", Prefix: "nr_abc", Enabled: true}, "hash", "enc")
	if err != nil {
		t.Fatalf("токен: %v", err)
	}

	cp := Checkpoint{
		TokenID: tokenID, Protocol: "openai", SessionID: "s1",
		Fingerprint: "fp-1", Conversation: "conv-1", Parent: "msg-1",
	}
	if err := st.SaveCheckpoint(ctx, cp); err != nil {
		t.Fatalf("сохранение точки: %v", err)
	}
	got, ok, err := st.FindCheckpoint(ctx, tokenID, "openai", "s1", "fp-1")
	if err != nil || !ok {
		t.Fatalf("точка не найдена: ok=%v err=%v", ok, err)
	}
	if got.Conversation != "conv-1" || got.Parent != "msg-1" {
		t.Fatalf("неверные поля точки: %+v", got)
	}
	if _, ok, _ := st.FindCheckpoint(ctx, tokenID, "openai", "s1", "нет-такой"); ok {
		t.Fatal("неизвестный отпечаток не должен находиться")
	}
	// Другая сессия того же токена точки не видит.
	if _, ok, _ := st.FindCheckpoint(ctx, tokenID, "openai", "s2", "fp-1"); ok {
		t.Fatal("точка не должна протекать в чужую сессию")
	}

	// Обрезка: держим только checkpointLimit последних точек.
	for i := 0; i < checkpointLimit+10; i++ {
		cp := Checkpoint{
			TokenID: tokenID, Protocol: "openai", SessionID: "s1",
			Fingerprint: fmt.Sprintf("fp-%d", i), Conversation: "conv-1", Parent: "msg-1",
		}
		if err := st.SaveCheckpoint(ctx, cp); err != nil {
			t.Fatalf("точка %d: %v", i, err)
		}
	}
	n, err := st.CountCheckpoints(ctx, tokenID, "openai", "s1")
	if err != nil {
		t.Fatalf("подсчёт точек: %v", err)
	}
	if n != checkpointLimit {
		t.Fatalf("должно остаться %d точек, осталось %d", checkpointLimit, n)
	}
	// Самая свежая точка на месте.
	if _, ok, _ := st.FindCheckpoint(ctx, tokenID, "openai", "s1", fmt.Sprintf("fp-%d", checkpointLimit+9)); !ok {
		t.Fatal("свежая точка потерялась при обрезке")
	}

	if err := st.DeleteCheckpoints(ctx, tokenID, "openai", "s1"); err != nil {
		t.Fatalf("удаление точек: %v", err)
	}
	if n, _ := st.CountCheckpoints(ctx, tokenID, "openai", "s1"); n != 0 {
		t.Fatalf("точки должны быть удалены, осталось %d", n)
	}
}

func TestPurgeSessionsRemovesOnlyStaleCheckpoints(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	keyID, err := st.CreateKey(ctx, "test", "test", []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	tokenID, err := st.CreateToken(ctx, Token{KeyID: keyID, Name: "test"}, "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSession(ctx, Session{TokenID: tokenID, Protocol: "openai", SessionID: "active", Conversation: "dialog"}); err != nil {
		t.Fatal(err)
	}
	for _, sessionID := range []string{"active", "orphan"} {
		if err := st.SaveCheckpoint(ctx, Checkpoint{TokenID: tokenID, Protocol: "openai", SessionID: sessionID, Fingerprint: "reply", Conversation: "dialog"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PurgeSessions(ctx, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	for sessionID, want := range map[string]int{"active": 1, "orphan": 0} {
		if count, err := st.CountCheckpoints(ctx, tokenID, "openai", sessionID); err != nil || count != want {
			t.Fatalf("%s checkpoints=%d, want %d: %v", sessionID, count, want, err)
		}
	}
}
