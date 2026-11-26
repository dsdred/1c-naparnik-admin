package gateway

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"naparnikadmin/internal/naparnik"
)

func TestWireNameSimple(t *testing.T) {
	if got := WireName("get_weather"); got != "client_get_weather" {
		t.Fatalf("ожидалось client_get_weather, получено %q", got)
	}
}

func TestWireNameSanitizes(t *testing.T) {
	got := WireName("Поиск по ИТС: найти всё")
	if !strings.HasPrefix(got, "cx_") {
		t.Fatalf("ожидался префикс cx_, получено %q", got)
	}
	if len(got) > 55 {
		t.Fatalf("имя слишком длинное: %d символов", len(got))
	}
	if got != WireName("Поиск по ИТС: найти всё") {
		t.Fatal("имя должно быть детерминированным")
	}
	if got == WireName("Поиск по ИТС: найти всё!") {
		t.Fatal("разные имена не должны совпадать")
	}
}

func TestWireToolMap(t *testing.T) {
	tools, back := wireToolMap([]Tool{
		{Name: "read_file", Description: "читает файл"},
		{Name: "инструмент", Schema: map[string]any{"type": "object"}},
	})
	if len(tools) != 2 || len(back) != 2 {
		t.Fatalf("ожидалось 2 инструмента, получено %d/%d", len(tools), len(back))
	}
	for wire, orig := range back {
		found := false
		for _, tool := range tools {
			if tool.Name == wire {
				found = true
			}
		}
		if !found {
			t.Fatalf("wire-имя %q отсутствует в списке инструментов", wire)
		}
		if orig != "read_file" && orig != "инструмент" {
			t.Fatalf("неожиданное обратное имя %q", orig)
		}
	}
}

func TestGroupTurnsMergesUsers(t *testing.T) {
	turns := []Turn{
		{Role: RoleUser, Text: "первое"},
		{Role: RoleAssistant, Text: "ответ"},
		{Role: RoleUser, Text: "второе"},
	}
	got := groupTurns(turns)
	if len(got) != 1 {
		t.Fatalf("ожидался один турн, получено %d", len(got))
	}
	if !strings.Contains(got[0].Text, "первое") || !strings.Contains(got[0].Text, "второе") {
		t.Fatalf("тексты не склеились: %q", got[0].Text)
	}
}

func TestGroupTurnsKeepsToolBoundary(t *testing.T) {
	turns := []Turn{
		{Role: RoleUser, Text: "вопрос"},
		{Role: RoleTool, Results: []naparnik.ToolResult{{ToolCallID: "a", Status: "ok", Content: "1"}}},
		{Role: RoleTool, Results: []naparnik.ToolResult{{ToolCallID: "b", Status: "ok", Content: "2"}}},
		{Role: RoleUser, Text: "продолжай"},
	}
	got := groupTurns(turns)
	if len(got) != 3 {
		t.Fatalf("ожидалось 3 группы, получено %d", len(got))
	}
	if got[1].Role != RoleTool || len(got[1].Results) != 2 {
		t.Fatalf("tool-результаты не склеились: %+v", got[1])
	}
}

func TestTailAfterLastAssistant(t *testing.T) {
	turns := []Turn{
		{Role: RoleUser, Text: "1"},
		{Role: RoleAssistant, Text: "2"},
		{Role: RoleUser, Text: "3"},
	}
	got := tailAfterLastAssistant(turns)
	if len(got) != 1 || got[0].Text != "3" {
		t.Fatalf("ожидался хвост из одного турна, получено %+v", got)
	}
}

func TestFingerprint(t *testing.T) {
	a := fingerprintOf([]Turn{{Role: RoleUser, Text: "Привет"}})
	b := fingerprintOf([]Turn{{Role: RoleUser, Text: "Привет "}})
	c := fingerprintOf([]Turn{{Role: RoleUser, Text: "Другое"}})
	if a == "" {
		t.Fatal("отпечаток пустой")
	}
	if a != b {
		t.Fatal("пробелы по краям не должны влиять на отпечаток")
	}
	if a == c {
		t.Fatal("разные сообщения должны давать разные отпечатки")
	}
}

func TestResolveSessionKeyIsolatesChats(t *testing.T) {
	chatA := []Turn{{Role: RoleUser, Text: "первый вопрос"}}
	chatB := []Turn{{Role: RoleUser, Text: "другой вопрос"}}

	keyA, stickyA := resolveSessionKey("", chatA)
	keyB, stickyB := resolveSessionKey("", chatB)
	if !stickyA || !stickyB {
		t.Fatal("без заголовка сессия должна считаться sticky")
	}
	if keyA == keyB {
		t.Fatalf("разные чаты получили один ключ: %q", keyA)
	}

	// Продолжение того же чата: первое сообщение не меняется.
	keyA2, _ := resolveSessionKey("", []Turn{
		{Role: RoleUser, Text: "первый вопрос"},
		{Role: RoleAssistant, Text: "ответ"},
		{Role: RoleUser, Text: "уточнение"},
	})
	if keyA2 != keyA {
		t.Fatalf("продолжение чата сменило ключ: %q != %q", keyA2, keyA)
	}

	// Заголовок клиента важнее отпечатка.
	keyH, stickyH := resolveSessionKey("sess-42", chatA)
	if stickyH || keyH != "sess-42" {
		t.Fatalf("ключ из заголовка потерялся: %q sticky=%v", keyH, stickyH)
	}
}

func TestLockSessionSerializesAndCleansUp(t *testing.T) {
	e := &Engine{locks: map[string]*keyLock{}}
	var inside, peak int32
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := e.lockSession("k", nil)
			defer unlock()
			n := atomic.AddInt32(&inside, 1)
			for {
				p := atomic.LoadInt32(&peak)
				if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt32(&inside, -1)
		}()
	}
	wg.Wait()
	if peak != 1 {
		t.Fatalf("запросы одной сессии пошли параллельно, одновременно: %d", peak)
	}
	if len(e.locks) != 0 {
		t.Fatalf("после освобождения остались замки: %d", len(e.locks))
	}
}

func TestLockSessionNotifiesWaiter(t *testing.T) {
	e := &Engine{locks: map[string]*keyLock{}}
	first := e.lockSession("k", nil)

	waited := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		unlock := e.lockSession("k", func() { waited <- struct{}{} })
		unlock()
		close(done)
	}()

	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("ожидающий запрос не получил уведомление об очереди")
	}

	// Освобождение первого пропускает второго.
	first()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("второй запрос не дождался освобождения")
	}
}

func TestFlattenToolTurns(t *testing.T) {
	turns := []Turn{
		{Role: RoleUser, Text: "вопрос"},
		{Role: RoleTool, Results: []naparnik.ToolResult{{ToolCallID: "c1", Status: "ok", Content: "содержимое"}}},
	}
	got := flattenToolTurns(turns)
	if len(got) != 2 {
		t.Fatalf("ожидалось 2 турна, получено %d", len(got))
	}
	if got[1].Role != RoleUser {
		t.Fatalf("tool-результат должен стать текстом пользователя, получено %q", got[1].Role)
	}
	if !strings.Contains(got[1].Text, "c1") || !strings.Contains(got[1].Text, "содержимое") {
		t.Fatalf("потерялись данные результата: %q", got[1].Text)
	}
}

func TestSplitLastUserSeparatesQuestion(t *testing.T) {
	turns := []Turn{
		{Role: RoleUser, Text: "первый вопрос"},
		{Role: RoleAssistant, Text: "ответ"},
		{Role: RoleUser, Text: "последний вопрос"},
	}
	seed, last := splitLastUser(turns)
	if len(seed) != 2 || len(last) != 1 || last[0].Text != "последний вопрос" {
		t.Fatalf("неверное разделение: seed=%+v last=%+v", seed, last)
	}
	// История засевается отдельной группой, последний вопрос — своей:
	// иначе groupTurns склеит всё в один текст и модель ответит на «простыню».
	groups := append(groupTurns(seed), groupTurns(last)...)
	if len(groups) != 2 {
		t.Fatalf("ожидалось 2 группы (история + вопрос), получено %d: %+v", len(groups), groups)
	}
	if groups[1].Text != "последний вопрос" {
		t.Fatalf("последняя группа должна быть вопросом, получено %q", groups[1].Text)
	}

	// Одна реплика — вся история уходит на генерацию, засева нет.
	if seed, last := splitLastUser([]Turn{{Role: RoleUser, Text: "единственный"}}); len(seed) != 0 || len(last) != 1 {
		t.Fatalf("единственная реплика должна целиком идти в генерацию: seed=%d last=%d", len(seed), len(last))
	}
}

func TestAnchorOf(t *testing.T) {
	turns := []Turn{
		{Role: RoleUser, Text: "вопрос"},
		{Role: RoleAssistant, Text: "первый ответ"},
		{Role: RoleUser, Text: "ещё"},
		{Role: RoleAssistant, Text: "  второй ответ  "},
		{Role: RoleUser, Text: "новый"},
	}
	fp, idx := anchorOf(turns)
	if idx != 3 {
		t.Fatalf("ожидалась последняя реплика ассистента (3), получено %d", idx)
	}
	if fp == "" {
		t.Fatal("отпечаток пустой")
	}
	// Пробелы по краям не влияют: клиент вправе их добавить.
	if fp2, _ := anchorOf([]Turn{{Role: RoleAssistant, Text: "второй ответ"}}); fp2 != fp {
		t.Fatalf("пробелы по краям изменили отпечаток: %q != %q", fp2, fp)
	}
	if fp3, _ := anchorOf([]Turn{{Role: RoleAssistant, Text: "другой ответ"}}); fp3 == fp {
		t.Fatal("разный текст должен давать разный отпечаток")
	}
	if fp4, idx4 := anchorOf([]Turn{{Role: RoleUser, Text: "только вопрос"}}); fp4 != "" || idx4 != -1 {
		t.Fatalf("без реплик ассистента точки нет: %q %d", fp4, idx4)
	}
}

func TestAnswerDigestNormalizesArgs(t *testing.T) {
	a := answerDigest("ответ", []naparnik.ToolCall{
		naparnik.NewToolCall("call-1", "read_file", `{"path": "a.txt", "n": 1}`),
	})
	b := answerDigest("ответ", []naparnik.ToolCall{
		naparnik.NewToolCall("call-1", "read_file", `{"n":1,"path":"a.txt"}`),
	})
	if a != b {
		t.Fatal("порядок ключей в аргументах не должен менять отпечаток")
	}
	c := answerDigest("ответ", []naparnik.ToolCall{
		naparnik.NewToolCall("call-1", "read_file", `{"path":"b.txt","n":1}`),
	})
	if a == c {
		t.Fatal("другие аргументы должны давать другой отпечаток")
	}
}

func TestConversationGone(t *testing.T) {
	if !conversationGone(&naparnik.Fault{Status: 404, Code: "resource_not_found"}) {
		t.Fatal("404 resource_not_found должен считаться потерей диалога")
	}
	if !conversationGone(&naparnik.Fault{Status: 404, Code: "upstream_error"}) {
		t.Fatal("любой 404 означает, что привязка недействительна")
	}
	if conversationGone(&naparnik.Fault{Status: 401, Code: "token_not_found"}) {
		t.Fatal("401 не должен приводить к пересозданию диалога")
	}
	if conversationGone(errors.New("boom")) {
		t.Fatal("обычная ошибка не должна считаться потерей диалога")
	}
}

func TestAssistantAsText(t *testing.T) {
	call := naparnik.NewToolCall("call-1", "read_file", `{"path":"a.txt"}`)
	out := assistantAsText(Turn{Role: RoleAssistant, Text: "сейчас прочитаю", Calls: []naparnik.ToolCall{call}})
	if !strings.Contains(out, "read_file") || !strings.Contains(out, "сейчас прочитаю") {
		t.Fatalf("некорректное представление: %q", out)
	}
}
