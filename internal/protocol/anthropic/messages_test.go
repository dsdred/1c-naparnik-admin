package anthropic

import (
	"encoding/json"
	"reflect"
	"testing"

	"naparnikadmin/internal/gateway"
)

func TestNormalizeTrailingSystemMessage(t *testing.T) {
	for _, tc := range []struct {
		name      string
		payload   string
		wantRoles []string
	}{
		{
			name: "after user message",
			payload: `{
				"system": [{"type":"text","text":"Base instructions."}],
				"messages": [
					{"role":"user","content":"First question."},
					{"role":"system","content":[{"type":"text","text":"Client bookkeeping."}]}
				]
			}`,
			wantRoles: []string{gateway.RoleUser},
		},
		{
			name: "after tool result",
			payload: `{
				"system": "Base instructions.",
				"messages": [
					{"role":"user","content":"First question."},
					{"role":"assistant","content":[{"type":"tool_use","id":"toolu_123","name":"lookup","input":{}}]},
					{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":"result"}]},
					{"role":"system","content":[{"type":"text","text":"Client bookkeeping."}]}
				]
			}`,
			wantRoles: []string{gateway.RoleUser, gateway.RoleAssistant, gateway.RoleTool},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req messagesRequest
			if err := json.Unmarshal([]byte(tc.payload), &req); err != nil {
				t.Fatal(err)
			}
			system, turns, _, err := normalize(&req)
			if err != nil {
				t.Fatal(err)
			}
			if system != "Base instructions." {
				t.Fatalf("top-level system instructions changed: %q", system)
			}
			roles := make([]string, 0, len(turns))
			for _, turn := range turns {
				roles = append(roles, turn.Role)
			}
			if !reflect.DeepEqual(roles, tc.wantRoles) {
				t.Fatalf("turn roles = %v, want %v", roles, tc.wantRoles)
			}
			if turns[0].Text != "First question." {
				t.Fatalf("user text changed: %q", turns[0].Text)
			}
			if last := turns[len(turns)-1]; last.Role == gateway.RoleTool {
				if len(last.Results) != 1 || last.Results[0].ToolCallID != "toolu_123" ||
					last.Results[0].Status != "ok" || last.Results[0].Content != "result" {
					t.Fatalf("tool result changed: %+v", last.Results)
				}
			}
		})
	}
}
