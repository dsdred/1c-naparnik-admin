// Package anthropic реализует входящий протокол Anthropic Messages.
package anthropic

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"naparnikadmin/internal/config"
	"naparnikadmin/internal/gateway"
	"naparnikadmin/internal/httpx"
	"naparnikadmin/internal/naparnik"
	"naparnikadmin/internal/store"
)

// Handler — обработчик POST /v1/messages.
type Handler struct {
	Auth   *gateway.Auth
	Engine *gateway.Engine
	Store  *store.Store
	Cfg    config.Source
	Log    *slog.Logger
}

type messagesRequest struct {
	Model     string          `json:"model"`
	System    json.RawMessage `json:"system"`
	Messages  []message       `json:"messages"`
	Tools     []tool          `json:"tools"`
	MaxTokens int             `json:"max_tokens"`
	Stream    bool            `json:"stream"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"input_schema"`
}

// Messages обрабатывает POST /v1/messages.
func (h *Handler) Messages(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	var req messagesRequest
	if err := httpx.ReadJSON(w, r, &req, 0); err != nil {
		h.writeError(w, nil, gateway.NewError(http.StatusBadRequest, "invalid_request_error", "Не удалось разобрать тело запроса: "+err.Error()))
		return
	}
	principal, err := h.Auth.Resolve(r.Context(), gateway.FromAnthropic(r), "anthropic")
	if err != nil {
		h.writeError(w, nil, err)
		return
	}
	req.Model = h.modelName(req.Model)

	system, turns, tools, err := normalize(&req)
	if err != nil {
		h.writeError(w, principal, err)
		return
	}
	if system != "" {
		turns = append([]gateway.Turn{{Role: gateway.RoleUser, Text: "[Системные инструкции]\n" + system}}, turns...)
	}

	input := gateway.Input{
		Token:     principal.Token,
		Key:       principal.Key,
		Protocol:  "anthropic",
		SessionID: sessionID(r),
		Turns:     turns,
		Tools:     tools,
	}

	if !req.Stream {
		reply, err := h.Engine.Execute(r.Context(), input)
		if err != nil {
			h.writeError(w, principal, err)
			return
		}
		h.logRequest(principal, req.Model, 200, "", reply, time.Since(started), false, gateway.LastUserText(turns))
		httpx.JSON(w, http.StatusOK, buildMessage(req.Model, reply))
		return
	}

	sw := httpx.NewWriter(w)
	defer sw.Close()

	msgID := "msg_" + randomID()
	sw.Event("message_start", mustJSON(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant",
			"model": req.Model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}))

	blockIndex := 0
	textOpen := false
	streamMode := h.streamMode(principal)
	if streamMode == "deltas" {
		input.OnText = func(text string) {
			if !textOpen {
				textOpen = true
				sw.Event("content_block_start", mustJSON(map[string]any{
					"type": "content_block_start", "index": blockIndex,
					"content_block": map[string]any{"type": "text", "text": ""},
				}))
			}
			sw.Event("content_block_delta", mustJSON(map[string]any{
				"type": "content_block_delta", "index": blockIndex,
				"delta": map[string]any{"type": "text_delta", "text": text},
			}))
		}
	}
	input.OnServerTool = func(string) { sw.Start() }
	input.OnQueued = func() { sw.Start() }

	reply, err := h.Engine.Execute(r.Context(), input)
	if err != nil {
		ge := gateway.FromError(err)
		h.logRequest(principal, req.Model, ge.Status, ge.Code, nil, time.Since(started), true, gateway.LastUserText(turns))
		sw.Event("error", mustJSON(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": anthropicErrorType(ge.Status), "message": ge.Message},
		}))
		return
	}
	h.logRequest(principal, req.Model, 200, "", reply, time.Since(started), true, gateway.LastUserText(turns))

	if streamMode != "deltas" {
		if text := reply.Text(); text != "" {
			sw.Event("content_block_start", mustJSON(map[string]any{
				"type": "content_block_start", "index": blockIndex,
				"content_block": map[string]any{"type": "text", "text": ""},
			}))
			sw.Event("content_block_delta", mustJSON(map[string]any{
				"type": "content_block_delta", "index": blockIndex,
				"delta": map[string]any{"type": "text_delta", "text": text},
			}))
			sw.Event("content_block_stop", mustJSON(map[string]any{"type": "content_block_stop", "index": blockIndex}))
			blockIndex++
		}
	} else if textOpen {
		sw.Event("content_block_stop", mustJSON(map[string]any{"type": "content_block_stop", "index": blockIndex}))
		blockIndex++
	}

	calls := reply.ToolCalls()
	for _, c := range calls {
		input, err := parseArgs(c.Function.Arguments)
		if err != nil {
			sw.Event("error", mustJSON(map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": "Напарник вернул неполные аргументы инструмента."},
			}))
			return
		}
		sw.Event("content_block_start", mustJSON(map[string]any{
			"type": "content_block_start", "index": blockIndex,
			"content_block": map[string]any{"type": "tool_use", "id": c.ID, "name": c.Function.Name, "input": map[string]any{}},
		}))
		sw.Event("content_block_delta", mustJSON(map[string]any{
			"type": "content_block_delta", "index": blockIndex,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": mustJSON(input)},
		}))
		sw.Event("content_block_stop", mustJSON(map[string]any{"type": "content_block_stop", "index": blockIndex}))
		blockIndex++
	}

	stopReason := "end_turn"
	if len(calls) > 0 {
		stopReason = "tool_use"
	}
	delta := map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": anthropicUsage(reply.Usage),
	}
	if reply.Context != nil && reply.Context.MaxContextLength > 0 {
		delta["naparnik_context"] = contextMap(reply.Context)
	}
	sw.Event("message_delta", mustJSON(delta))
	sw.Event("message_stop", mustJSON(map[string]any{"type": "message_stop"}))
}

func (h *Handler) streamMode(p *gateway.Principal) string {
	if p.Token.StreamMode == "deltas" || p.Token.StreamMode == "final" {
		return p.Token.StreamMode
	}
	return h.Cfg.Snapshot().StreamMode
}

func (h *Handler) writeError(w http.ResponseWriter, p *gateway.Principal, err error) {
	ge := gateway.FromError(err)
	if p != nil {
		h.logRequest(p, "", ge.Status, ge.Code, nil, 0, false, "")
	}
	httpx.JSON(w, ge.Status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": anthropicErrorType(ge.Status), "message": ge.Message},
	})
}

func (h *Handler) logRequest(p *gateway.Principal, model string, status int, errType string,
	reply *gateway.Reply, latency time.Duration, stream bool, prompt string) {
	if p == nil {
		return
	}
	entry := store.LogEntry{
		TS: time.Now(), TokenID: p.Token.ID, KeyID: p.Token.KeyID,
		Protocol: "anthropic", Model: model, Status: status, ErrorType: errType,
		LatencyMS: int(latency.Milliseconds()), Stream: stream, Prompt: prompt,
	}
	if reply != nil && reply.Usage != nil {
		entry.PromptTokens = reply.Usage.PromptTokens
		entry.CompTokens = reply.Usage.CompletionTokens
		entry.CachedTokens = reply.Usage.CachedTokens
	}
	// Журнал не должен задерживать ответ: запись уходит после его отправки, в фоне
	// и с ограниченным ожиданием блокировки SQLite. Иначе при занятой базе клиент
	// с коротким таймаутом теряет ответ, который уже сгенерирован.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := h.Store.LogRequest(ctx, entry); err != nil && h.Log != nil {
			h.Log.Warn("не удалось записать журнал запроса", "err", err)
		}
	}()
}

func normalize(req *messagesRequest) (string, []gateway.Turn, []gateway.Tool, error) {
	system := textFrom(req.System)
	var turns []gateway.Turn

	for _, m := range req.Messages {
		blocks, err := parseBlocks(m.Content)
		if err != nil {
			return "", nil, nil, err
		}
		switch m.Role {
		case "user":
			var texts []string
			for _, b := range blocks {
				switch b.Type {
				case "text", "":
					if b.Text != "" {
						texts = append(texts, b.Text)
					}
				case "tool_result":
					content := textFrom(b.Content)
					status := "ok"
					if b.IsError {
						status = "error"
					}
					var payload any
					if content != "" {
						payload = content
					}
					turns = append(turns, gateway.Turn{Role: gateway.RoleTool, Results: []naparnik.ToolResult{{
						ToolCallID: b.ToolUseID, Status: status, Content: payload,
					}}})
				default:
					return "", nil, nil, gateway.NewError(http.StatusBadRequest, "invalid_request_error",
						"Шлюз принимает текстовые сообщения и клиентские инструменты; блок «"+b.Type+"» не поддерживается.")
				}
			}
			if len(texts) > 0 {
				turns = append(turns, gateway.Turn{Role: gateway.RoleUser, Text: strings.Join(texts, "\n")})
			}
		case "assistant":
			t := gateway.Turn{Role: gateway.RoleAssistant}
			for _, b := range blocks {
				switch b.Type {
				case "text", "":
					if t.Text != "" {
						t.Text += "\n"
					}
					t.Text += b.Text
				case "tool_use":
					var call naparnik.ToolCall
					call.ID = b.ID
					call.Type = "function"
					call.Function.Name = b.Name
					if len(b.Input) > 0 {
						call.Function.Arguments = string(b.Input)
					} else {
						call.Function.Arguments = "{}"
					}
					t.Calls = append(t.Calls, call)
				case "thinking", "redacted_thinking":
					// не передаём
				default:
					return "", nil, nil, gateway.NewError(http.StatusBadRequest, "invalid_request_error",
						"Блок ассистента «"+b.Type+"» не поддерживается.")
				}
			}
			turns = append(turns, t)
		}
	}

	var tools []gateway.Tool
	for _, t := range req.Tools {
		tools = append(tools, gateway.Tool{Name: t.Name, Description: t.Description, Schema: t.InputSchema})
	}
	return system, turns, tools, nil
}

func parseBlocks(raw json.RawMessage) ([]block, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []block{{Type: "text", Text: s}}, nil
	}
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, gateway.NewError(http.StatusBadRequest, "invalid_request_error", "Неизвестный формат content")
	}
	return blocks, nil
}

func textFrom(raw json.RawMessage) string {
	blocks, err := parseBlocks(raw)
	if err != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func buildMessage(model string, reply *gateway.Reply) map[string]any {
	content := []any{}
	if text := reply.Text(); text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	stopReason := "end_turn"
	for _, c := range reply.ToolCalls() {
		input, err := parseArgs(c.Function.Arguments)
		if err != nil {
			input = map[string]any{}
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": c.ID, "name": c.Function.Name, "input": input,
		})
		stopReason = "tool_use"
	}
	out := map[string]any{
		"id": "msg_" + randomID(), "type": "message", "role": "assistant",
		"model": model, "content": content,
		"stop_reason": stopReason, "stop_sequence": nil,
		"usage": anthropicUsage(reply.Usage),
	}
	if reply.Context != nil && reply.Context.MaxContextLength > 0 {
		out["naparnik_context"] = contextMap(reply.Context)
	}
	return out
}

func anthropicUsage(u *naparnik.Usage) map[string]any {
	if u == nil {
		return map[string]any{"input_tokens": 0, "output_tokens": 0}
	}
	cached := u.CachedTokens
	if cached > u.PromptTokens {
		cached = 0
	}
	return map[string]any{
		"input_tokens":                u.PromptTokens - cached,
		"output_tokens":               u.CompletionTokens,
		"cache_read_input_tokens":     cached,
		"cache_creation_input_tokens": 0,
	}
}

func contextMap(c *naparnik.ContextInfo) map[string]any {
	return map[string]any{"token_count": c.TokenCount, "max_context_length": c.MaxContextLength}
}

func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	}
	return "api_error"
}

func parseArgs(raw string) (any, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, err
	}
	return v, nil
}

func mustJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func randomID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "000000000000"
	}
	return hex.EncodeToString(buf)
}

// modelName подставляет настроенный id модели, если клиент его не указал.
func (h *Handler) modelName(model string) string {
	if strings.TrimSpace(model) != "" {
		return model
	}
	return h.Cfg.Snapshot().ModelName()
}

func sessionID(r *http.Request) string {
	for _, h := range []string{"x-deepseek-harness-session-id", "x-session-id", "x-conversation-id"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	return ""
}
