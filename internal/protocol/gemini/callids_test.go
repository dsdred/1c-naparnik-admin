package gemini

import (
	"testing"

	"naparnikadmin/internal/gateway"
	"naparnikadmin/internal/naparnik"
)

// TestParallelCallsOfSameFunctionGetDistinctIDs проверяет замечание ревью:
// два параллельных вызова одной функции имеют разные id, но одно имя.
// Раньше карта «имя → id» хранила последний id, и оба functionResponse
// уходили с ним же — второй результат привязывался к чужому вызову.
func TestParallelCallsOfSameFunctionGetDistinctIDs(t *testing.T) {
	reply := &gateway.Reply{Parts: []gateway.Part{{ToolCalls: []naparnik.ToolCall{
		naparnik.NewToolCall("call-a", "read_file", `{"path":"a.txt"}`),
		naparnik.NewToolCall("call-b", "read_file", `{"path":"b.txt"}`),
	}}}}

	h := &Handler{}
	h.remember(7, "sess", reply)

	first := h.takeCallID(7, "sess", "read_file")
	second := h.takeCallID(7, "sess", "read_file")
	if first != "call-a" || second != "call-b" {
		t.Fatalf("ответы должны получить разные id в порядке объявления: %q, %q", first, second)
	}
	// Когда ответов больше, чем объявленных вызовов, возвращается последний id,
	// а не пустая строка: клиент не должен терять привязку вовсе.
	if third := h.takeCallID(7, "sess", "read_file"); third != "call-b" {
		t.Fatalf("лишний ответ должен получить последний известный id, получено %q", third)
	}
}

// TestSingleCallStillResolves проверяет, что обычный одиночный вызов
// по-прежнему сопоставляется со своим id.
func TestSingleCallStillResolves(t *testing.T) {
	reply := &gateway.Reply{Parts: []gateway.Part{{ToolCalls: []naparnik.ToolCall{
		naparnik.NewToolCall("call-only", "lookup", `{}`),
	}}}}
	h := &Handler{}
	h.remember(1, "s", reply)
	if got := h.takeCallID(1, "s", "lookup"); got != "call-only" {
		t.Fatalf("ожидался call-only, получено %q", got)
	}
	// Незнакомая функция остаётся без id — вызывающий подставит имя.
	if got := h.takeCallID(1, "s", "unknown"); got != "" {
		t.Fatalf("неизвестная функция должна давать пустой id, получено %q", got)
	}
}
