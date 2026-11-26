package gateway

import (
	"strings"
	"testing"

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

func TestAssistantAsText(t *testing.T) {
	call := naparnik.NewToolCall("call-1", "read_file", `{"path":"a.txt"}`)
	out := assistantAsText(Turn{Role: RoleAssistant, Text: "сейчас прочитаю", Calls: []naparnik.ToolCall{call}})
	if !strings.Contains(out, "read_file") || !strings.Contains(out, "сейчас прочитаю") {
		t.Fatalf("некорректное представление: %q", out)
	}
}
