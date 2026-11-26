package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"naparnikadmin/internal/gateway"
)

func TestContentTextString(t *testing.T) {
	text, media, err := contentText(json.RawMessage(`"привет"`))
	if err != nil || media || text != "привет" {
		t.Fatalf("text=%q media=%v err=%v", text, media, err)
	}
}

func TestContentTextParts(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"раз"},{"type":"text","text":"два"}]`)
	text, media, err := contentText(raw)
	if err != nil || media {
		t.Fatalf("err=%v media=%v", err, media)
	}
	if text != "раз\nдва" {
		t.Fatalf("неверная склейка: %q", text)
	}
}

func TestContentTextMedia(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"смотри"},{"type":"image_url","image_url":{"url":"http://x"}}]`)
	_, media, err := contentText(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !media {
		t.Fatal("вложение должно быть распознано")
	}
}

func TestNormalizeChat(t *testing.T) {
	req := &chatRequest{
		Messages: []chatMessage{
			{Role: "system", Content: json.RawMessage(`"будь краток"`)},
			{Role: "user", Content: json.RawMessage(`"вопрос"`)},
			{Role: "assistant", Content: json.RawMessage(`"ответ"`), ToolCalls: []chatToolCall{
				{ID: "call-1", Type: "function", Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "read_file", Arguments: `{"path":"a.txt"}`}},
			}},
			{Role: "tool", Content: json.RawMessage(`"содержимое"`), ToolCallID: "call-1"},
		},
		Tools: []chatTool{{
			Type: "function",
			Function: struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Parameters  any    `json:"parameters"`
			}{Name: "read_file", Description: "читает файл", Parameters: map[string]any{"type": "object"}},
		}},
	}
	system, turns, tools, err := normalizeChat(req)
	if err != nil {
		t.Fatal(err)
	}
	if system != "будь краток" {
		t.Fatalf("system: %q", system)
	}
	if len(turns) != 3 {
		t.Fatalf("ожидалось 3 турна, получено %d: %+v", len(turns), turns)
	}
	if turns[1].Role != gateway.RoleAssistant || len(turns[1].Calls) != 1 || turns[1].Calls[0].Function.Name != "read_file" {
		t.Fatalf("assistant-турн разобран неверно: %+v", turns[1])
	}
	if turns[2].Role != gateway.RoleTool || turns[2].Results[0].ToolCallID != "call-1" {
		t.Fatalf("tool-турн разобран неверно: %+v", turns[2])
	}
	if len(tools) != 1 || tools[0].Name != "read_file" {
		t.Fatalf("инструменты: %+v", tools)
	}
}

func TestNormalizeChatRejectsMedia(t *testing.T) {
	req := &chatRequest{Messages: []chatMessage{
		{Role: "user", Content: json.RawMessage(`[{"type":"image_url","image_url":{"url":"http://x"}}]`)},
	}}
	_, _, _, err := normalizeChat(req)
	if err == nil || !strings.Contains(err.Error(), "вложения") {
		t.Fatalf("ожидалась ошибка про вложения, получено %v", err)
	}
}

func TestBuildCompletion(t *testing.T) {
	reply := &gateway.Reply{Parts: []gateway.Part{{Text: "ответ"}}}
	out := buildCompletion("naparnik", reply)
	if out["object"] != "chat.completion" {
		t.Fatalf("object: %v", out["object"])
	}
	choices := out["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "ответ" {
		t.Fatalf("content: %v", msg["content"])
	}
}
