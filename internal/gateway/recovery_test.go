package gateway

import (
	"encoding/json"
	"testing"

	"naparnikadmin/internal/naparnik"
)

func TestRecoveredPacketsRejectAmbiguousOrIncompleteHistory(t *testing.T) {
	first := naparnik.ConversationMessage{UUID: "ack-a", Role: RoleTool, Content: json.RawMessage(`{"tool_call_id":"tool-a","status":"ok"}`)}
	second := naparnik.ConversationMessage{UUID: "ack-b", ParentUUID: first.UUID, Role: RoleTool, Content: json.RawMessage(`{"tool_call_id":"tool-b","status":"ok"}`)}
	answer := naparnik.ConversationMessage{UUID: "answer", ParentUUID: second.UUID, Role: RoleAssistant, Content: json.RawMessage(`{"content":"done"}`)}
	other := answer
	other.UUID = "other"
	cut := interruptedTurn{Role: RoleTool, Acks: []string{first.UUID}, ToolIDs: []string{"tool-b", "tool-a"}}
	for _, tc := range []struct {
		name     string
		messages []naparnik.ConversationMessage
		want     int
	}{
		{"missing tool result", []naparnik.ConversationMessage{first, answer}, 0},
		{"competing assistants", []naparnik.ConversationMessage{first, second, answer, other}, 0},
		{"unique branch with both results", []naparnik.ConversationMessage{answer, first, second}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(recoveredPackets(tc.messages, cut)); got != tc.want {
				t.Fatalf("got %d recovered packets, want %d", got, tc.want)
			}
		})
	}
}
