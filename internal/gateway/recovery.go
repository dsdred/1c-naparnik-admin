package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"naparnikadmin/internal/naparnik"
	"naparnikadmin/internal/store"
)

// interruptedTurn records which upstream messages were acknowledged before SSE ended.
type interruptedTurn struct {
	Request   string
	Count     int
	Role      string
	Acks      []string
	ToolIDs   []string
	Prefix    []Part
	Remaining int
}

func (e *Engine) rememberInterrupted(ctx context.Context, in Input, sessionID, conversation string,
	parent *string, fingerprint string, toolMap map[string]string, cut interruptedTurn) {
	data, err := json.Marshal(cut)
	if err != nil || cut.Request == "" {
		return
	}
	tools, _ := json.Marshal(toolMap)
	session := store.Session{
		TokenID: in.Token.ID, Protocol: in.Protocol, SessionID: sessionID, Conversation: conversation,
		Fingerprint: fingerprint, ToolsJSON: string(tools), PendingJSON: string(data),
	}
	if parent != nil {
		session.Parent = *parent
	}
	// The client may have disconnected after the upstream acknowledged the message.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := e.store.SaveSession(saveCtx, session); err != nil {
		log := e.Log
		if log == nil {
			log = slog.Default()
		}
		log.Warn("не удалось сохранить подтверждение прерванного запроса", "token_id", in.Token.ID,
			"protocol", in.Protocol, "conversation", conversation, "err", err)
	}
}

func turnDigest(turns []Turn) string {
	normalized := slices.Clone(turns)
	for i := range normalized {
		t := &normalized[i]
		t.Calls = slices.Clone(t.Calls)
		slices.SortFunc(t.Calls, func(a, b naparnik.ToolCall) int { return strings.Compare(a.ID, b.ID) })
		for j := range t.Calls {
			t.Calls[j].Index = 0
			t.Calls[j].Function.Arguments = normalizeArgs(t.Calls[j].Function.Arguments)
		}
		t.Results = slices.Clone(t.Results)
		slices.SortFunc(t.Results, func(a, b naparnik.ToolResult) int {
			return strings.Compare(a.ToolCallID, b.ToolCallID)
		})
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func interruptedStream(err error) bool {
	var fault *naparnik.Fault
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &fault) && fault.Code == "bad_stream")
}

func recoveryPending(message string) *Error {
	return NewError(http.StatusConflict, "recovery_pending", message)
}

// errPendingDelivered — внутренний сигнал: ответ на прерванный запрос уже доставлен
// клиенту, а признак восстановления остался из-за сбоя записи. Такой признак надо
// снять и продолжить обычную обработку, а не отвечать 409.
var errPendingDelivered = errors.New("recovery already delivered")

// clearPending снимает признак восстановления в фоне и с ограниченным ожиданием:
// ответ уже у клиента, ждать освобождения SQLite на его пути нельзя.
func (e *Engine) clearPending(sessionKey string, in Input, expected string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		ok, err := e.store.ClearPending(ctx, in.Token.ID, in.Protocol, sessionKey, expected)
		if err != nil || !ok {
			log := e.Log
			if log == nil {
				log = slog.Default()
			}
			if err != nil {
				log.Warn("не удалось снять признак восстановления", "token_id", in.Token.ID,
					"protocol", in.Protocol, "err", err)
			}
		}
	}()
}

type recoveredTurn struct {
	packets   []naparnik.Packet
	prefix    []Part
	tail      []Turn
	remaining []Turn
}

func (e *Engine) recoverTurn(ctx context.Context, in Input, binding store.Session) (*recoveredTurn, error) {
	var cut interruptedTurn
	if err := json.Unmarshal([]byte(binding.PendingJSON), &cut); err != nil ||
		cut.Count <= 0 || cut.Count > len(in.Turns) || len(cut.Acks) == 0 ||
		cut.Request == "" || cut.Request != turnDigest(in.Turns[:cut.Count]) {
		return nil, recoveryPending("перед продолжением повторите прерванный запрос или сбросьте сессию")
	}
	tail := in.Turns[cut.Count:]
	if len(tail) > 0 && cut.Remaining == 0 && tail[0].Role == RoleAssistant {
		// В истории клиента уже есть ответ на прерванный запрос: значит восстановление
		// отработало, а запись об очистке pending не прошла (база была занята).
		return nil, errPendingDelivered
	}
	if len(tail) > 0 && (len(tail) != 1 || tail[0].Role != RoleUser || cut.Remaining != 0) {
		return nil, recoveryPending("перед продолжением повторите прерванный запрос")
	}
	var remaining []Turn
	if cut.Remaining > 0 {
		groups := groupTurns(tailAfterLastAssistant(in.Turns[:cut.Count]))
		if cut.Remaining > len(groups) {
			return nil, recoveryPending("не удалось определить оставшиеся сообщения прерванного запроса")
		}
		remaining = groups[len(groups)-cut.Remaining:]
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	messages, err := e.client.ConversationMessages(readCtx, in.Key, binding.Conversation)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, recoveryPending("не удалось сверить историю Напарника, повторите запрос позже")
	}
	packets := recoveredPackets(messages, cut)
	if len(packets) == 0 {
		return nil, recoveryPending("Напарник принял сообщение, но однозначного ответа в истории пока нет")
	}
	if len(tail) > 0 {
		for _, p := range packets {
			if len(p.Content.ToolCalls) > 0 {
				return nil, recoveryPending("повторите прерванный запрос, чтобы получить вызовы инструментов")
			}
		}
	}
	return &recoveredTurn{packets: packets, prefix: cut.Prefix, tail: tail, remaining: remaining}, nil
}

func recoveredPackets(messages []naparnik.ConversationMessage, cut interruptedTurn) []naparnik.Packet {
	byID := make(map[string]naparnik.ConversationMessage, len(messages))
	children := make(map[string][]naparnik.ConversationMessage, len(messages))
	for _, msg := range messages {
		if msg.UUID != "" {
			byID[msg.UUID] = msg
			children[msg.ParentUUID] = append(children[msg.ParentUUID], msg)
		}
	}
	current, ok := byID[cut.Acks[0]]
	if !ok || current.Role != cut.Role {
		return nil
	}
	seen := map[string]bool{}
	toolIDs := make(map[string]bool, len(cut.ToolIDs))
	for _, id := range cut.ToolIDs {
		toolIDs[id] = false
	}
	var packets []naparnik.Packet
walk:
	for {
		if seen[current.UUID] {
			return nil
		}
		seen[current.UUID] = true
		switch current.Role {
		case RoleUser:
			if current.UUID != cut.Acks[0] {
				break walk
			}
		case RoleTool:
			var result naparnik.ToolResult
			if len(packets) > 0 || json.Unmarshal(current.Content, &result) != nil || result.Status == "ignored" {
				return nil
			}
			if alreadySeen, expected := toolIDs[result.ToolCallID]; !expected || alreadySeen {
				return nil
			}
			toolIDs[result.ToolCallID] = true
		case RoleAssistant:
			var content naparnik.AssistantContent
			if json.Unmarshal(current.Content, &content) != nil ||
				(content.Content == "" && content.ReasoningContent == "" && len(content.ToolCalls) == 0) {
				return nil
			}
			packets = append(packets, naparnik.Packet{UUID: current.UUID, Role: RoleAssistant, Finished: true, Content: &content})
		default:
			return nil
		}
		next := children[current.UUID]
		if len(next) == 0 {
			break
		}
		if len(next) != 1 {
			return nil
		}
		current = next[0]
	}
	for _, id := range cut.Acks {
		if !seen[id] {
			return nil
		}
	}
	for _, seen := range toolIDs {
		if !seen {
			return nil
		}
	}
	return packets
}
