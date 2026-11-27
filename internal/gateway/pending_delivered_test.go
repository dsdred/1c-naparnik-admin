package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"naparnikadmin/internal/config"
	"naparnikadmin/internal/naparnik"
	"naparnikadmin/internal/store"
)

// TestContinueAfterDeliveredRecovery проверяет сценарий из issue #1, второй шаг:
// ответ на прерванный запрос уже ушёл клиенту, а признак восстановления остался
// в базе, потому что запись не прошла (база была занята). Следующая реплика
// не должна получать 409 recovery_pending — признак снимается, работа идёт дальше.
func TestContinueAfterDeliveredRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	st, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	keyID, err := st.CreateKey(ctx, "test", "test", []byte("test"))
	if err != nil {
		t.Fatal(err)
	}
	tokenID, err := st.CreateToken(ctx, store.Token{KeyID: keyID, Name: "test"}, "test-hash", "")
	if err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/conversations":
			_, _ = io.WriteString(w, `{"uuid":"dialog"}`)
		case "/conversations/dialog/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: "+`{"uuid":"reply","role":"assistant","finished":true,"content":{"content":"ready"}}`+"\n\n")
		case "/conversations/dialog/context_tokens":
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("неожиданный запрос: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	// Прерванный запрос: клиент прислал одну реплику, Напарник её подтвердил,
	// а признак восстановления записать не удалось.
	cut := interruptedTurn{
		Request: turnDigest([]Turn{{Role: RoleUser, Text: "ping"}}),
		Count:   1,
		Role:    RoleUser,
		Acks:    []string{"accepted"},
	}
	data, err := json.Marshal(cut)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSession(ctx, store.Session{
		TokenID: tokenID, Protocol: "openai", SessionID: "sess",
		Conversation: "dialog", Parent: "accepted", PendingJSON: string(data),
	}); err != nil {
		t.Fatal(err)
	}

	e := &Engine{client: naparnik.New(upstream.URL, 3*time.Second), store: st, cfg: &config.Config{}, locks: map[string]*keyLock{}}
	// Клиент прислал историю, в которой ответ на прерванный запрос уже есть.
	in := Input{
		Key: "test", Token: store.Token{ID: tokenID}, Protocol: "openai", SessionID: "sess",
		Turns: []Turn{
			{Role: RoleUser, Text: "ping"},
			{Role: RoleAssistant, Text: "recovered answer"},
		},
	}
	reply, err := e.Execute(ctx, in)
	if err != nil {
		t.Fatalf("после доставленного ответа продолжение должно работать, получено: %v", err)
	}
	if reply == nil || reply.Text() != "ready" {
		t.Fatalf("ожидался обычный ответ, получено: %+v", reply)
	}

	// Признак снимается в фоне — ждём его очистки.
	deadline := time.Now().Add(3 * time.Second)
	for {
		binding, err := st.GetSession(ctx, tokenID, "openai", "sess")
		if err == nil && binding.PendingJSON == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("признак восстановления не снят: %+v, %v", binding, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
