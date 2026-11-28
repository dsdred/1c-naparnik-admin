package responses

import (
	"encoding/json"
	"net/http"
	"testing"

	"naparnikadmin/internal/gateway"
	"naparnikadmin/internal/naparnik"
)

func TestNormalizeCodexToolContinuation(t *testing.T) {
	var req responsesRequest
	if err := json.Unmarshal([]byte(`{
		"input": [
			{"role":"developer","content":[{"type":"input_text","text":"Use the fixture."}]},
			{"role":"user","content":[{"type":"input_text","text":"Look up"},{"type":"input_text","text":"record 7"}]},
			{"type":"function_call","call_id":"call_7","name":"lookup","arguments":"{\"id\":7}"},
			{"type":"function_call_output","call_id":"call_7","output":"found"}
		],
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]
	}`), &req); err != nil {
		t.Fatal(err)
	}

	turns, tools, err := normalize(&req)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 4 || len(tools) != 1 || tools[0].Name != "lookup" {
		t.Fatalf("unexpected turns or tools: %#v, %#v", turns, tools)
	}
	if turns[0].Role != gateway.RoleUser || turns[0].Text != "[Системные инструкции]\nUse the fixture." ||
		turns[1].Role != gateway.RoleUser || turns[1].Text != "Look up\nrecord 7" {
		t.Fatalf("unexpected instructions or user input: %#v", turns[:2])
	}
	call, result := turns[2], turns[3]
	if call.Role != gateway.RoleAssistant || len(call.Calls) != 1 ||
		call.Calls[0].ID != "call_7" || call.Calls[0].Function.Name != "lookup" ||
		call.Calls[0].Function.Arguments != `{"id":7}` {
		t.Fatalf("unexpected function call: %#v", call)
	}
	if result.Role != gateway.RoleTool || len(result.Results) != 1 ||
		result.Results[0].ToolCallID != call.Calls[0].ID || result.Results[0].Content != "found" ||
		result.Results[0].Status != "ok" {
		t.Fatalf("unexpected function result: %#v", result)
	}
}

func TestNormalizeRejectsNonTextContent(t *testing.T) {
	req := responsesRequest{Input: json.RawMessage(`[{"role":"user","content":[{"type":"input_image"}]}]`)}
	_, _, err := normalize(&req)
	if err == nil {
		t.Fatal("non-text input was accepted")
	}
	got := gateway.FromError(err)
	if got.Status != http.StatusBadRequest || got.Code != "unsupported_content" {
		t.Fatalf("unexpected error: %#v", got)
	}
}

func TestBuildResponseIncludesCacheUsage(t *testing.T) {
	usage := &naparnik.Usage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15, CachedTokens: 9}
	got := buildResponse("resp_fixture", "naparnik", 123, nil, usage)
	u, ok := got["usage"].(map[string]any)
	if !ok || u["input_tokens"] != 12 || u["output_tokens"] != 3 || u["total_tokens"] != 15 {
		t.Fatalf("unexpected usage: %#v", got["usage"])
	}
	details, ok := u["input_tokens_details"].(map[string]any)
	if !ok || details["cached_tokens"] != 9 {
		t.Fatalf("unexpected cache usage: %#v", u)
	}
}
