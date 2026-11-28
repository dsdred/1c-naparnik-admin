package gemini

import (
	"encoding/json"
	"testing"

	"naparnikadmin/internal/gateway"
	"naparnikadmin/internal/naparnik"
)

func TestNormalizePreservesParallelCallIDs(t *testing.T) {
	const user = `{"role":"user","parts":[{"text":"question"}]}`
	const callsWithoutIDs = `{"role":"model","parts":[
		{"functionCall":{"name":"lookup","args":{"a":1,"b":2}}},
		{"functionCall":{"name":"lookup","args":{"a":3}}}
	]}`
	const responsesWithoutIDs = `{"role":"function","parts":[
		{"functionResponse":{"name":"lookup","response":{"result":"first"}}},
		{"functionResponse":{"name":"lookup","response":{"result":"second"}}}
	]}`
	h := &Handler{}
	h.remember(7, "sess", []gateway.Turn{{Role: gateway.RoleUser, Text: "question"}}, &gateway.Reply{Parts: []gateway.Part{{
		ToolCalls: []naparnik.ToolCall{
			naparnik.NewToolCall("first-id", "lookup", `{"b":2,"a":1}`),
			naparnik.NewToolCall("second-id", "lookup", `{"a":3}`),
		},
	}}})
	for range 2 {
		turns := normalizeHistory(t, h, user+","+callsWithoutIDs+","+responsesWithoutIDs)
		if turns[1].Calls[0].ID != "first-id" || turns[1].Calls[1].ID != "second-id" ||
			turns[2].Results[0].ToolCallID != "first-id" || turns[3].Results[0].ToolCallID != "second-id" {
			t.Fatalf("ID вызовов и результатов должны совпадать при повторе запроса: %+v", turns)
		}
	}
}

func TestNormalizeUsesExplicitIDsAfterRestartAndWithReorderedResults(t *testing.T) {
	turns := normalizeHistory(t, &Handler{}, `{"role":"user","parts":[{"text":"question"}]},
		{"role":"model","parts":[
			{"functionCall":{"id":"first-id","name":"lookup","args":{}}},
			{"functionCall":{"id":"second-id","name":"lookup","args":{}}}
		]},
		{"role":"user","parts":[
			{"functionResponse":{"id":"second-id","name":"lookup","response":{}}},
			{"functionResponse":{"name":"lookup","response":{}}}
		]}`)
	if turns[1].Calls[0].ID != "first-id" || turns[1].Calls[1].ID != "second-id" ||
		turns[2].Results[0].ToolCallID != "second-id" || turns[3].Results[0].ToolCallID != "first-id" {
		t.Fatalf("явный ID связывает результат с вызовом независимо от порядка: %+v", turns)
	}
}

func normalizeHistory(t *testing.T, h *Handler, contents string) []gateway.Turn {
	t.Helper()
	var request generateRequest
	if err := json.Unmarshal([]byte(`{"contents":[`+contents+`]}`), &request); err != nil {
		t.Fatal(err)
	}
	turns, _, err := h.normalize(7, "sess", &request)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 4 || len(turns[1].Calls) != 2 || len(turns[2].Results) != 1 || len(turns[3].Results) != 1 {
		t.Fatalf("неожиданная история: %+v", turns)
	}
	return turns
}
