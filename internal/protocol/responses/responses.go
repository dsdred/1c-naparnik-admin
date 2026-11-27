// Package responses реализует входящий протокол OpenAI Responses API.
package responses

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

// Handler — обработчик POST /v1/responses.
type Handler struct {
	Auth   *gateway.Auth
	Engine *gateway.Engine
	Store  *store.Store
	Cfg    config.Source
	Log    *slog.Logger
}

type responsesRequest struct {
	Model        string          `json:"model"`
	Input        json.RawMessage `json:"input"`
	Instructions string          `json:"instructions"`
	Tools        []tool          `json:"tools"`
	Stream       bool            `json:"stream"`
}

type tool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

type inputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    string          `json:"output"`
}

// Responses обрабатывает POST /v1/responses.
func (h *Handler) Responses(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	var req responsesRequest
	if err := httpx.ReadJSON(w, r, &req, 0); err != nil {
		h.writeError(w, nil, gateway.NewError(http.StatusBadRequest, "invalid_request_error", "Не удалось разобрать тело запроса: "+err.Error()))
		return
	}
	principal, err := h.Auth.Resolve(r.Context(), gateway.FromBearer(r), "responses")
	if err != nil {
		h.writeError(w, nil, err)
		return
	}
	req.Model = h.modelName(req.Model)

	turns, tools, err := normalize(&req)
	if err != nil {
		h.writeError(w, principal, err)
		return
	}
	if strings.TrimSpace(req.Instructions) != "" {
		turns = append([]gateway.Turn{{Role: gateway.RoleUser, Text: "[Системные инструкции]\n" + req.Instructions}}, turns...)
	}

	input := gateway.Input{
		Token: principal.Token, Key: principal.Key, Protocol: "responses",
		SessionID: sessionID(r), Turns: turns, Tools: tools,
	}

	respID, created := "resp_"+randomID(), time.Now().Unix()
	itemID := "msg_" + randomID()
	if !req.Stream {
		reply, err := h.Engine.Execute(r.Context(), input)
		if err != nil {
			h.writeError(w, principal, err)
			return
		}
		h.logRequest(principal, req.Model, 200, "", reply, time.Since(started), false, gateway.LastUserText(turns))
		httpx.JSON(w, http.StatusOK, buildResponse(respID, req.Model, created, responseOutput(reply, itemID), reply.Usage))
		return
	}

	sw := httpx.NewWriter(w)
	defer sw.Close()

	base := map[string]any{
		"id": respID, "object": "response", "created_at": created,
		"model": req.Model, "status": "in_progress", "output": []any{},
	}
	sw.Event("response.created", mustJSON(map[string]any{"type": "response.created", "response": base}))
	sw.Event("response.in_progress", mustJSON(map[string]any{"type": "response.in_progress", "response": base}))

	added := false
	sendText := func(text string) {
		if !added {
			added = true
			sw.Event("response.output_item.added", mustJSON(map[string]any{
				"type": "response.output_item.added", "output_index": 0,
				"item": map[string]any{"type": "message", "id": itemID, "role": "assistant", "content": []any{}, "status": "in_progress"},
			}))
			sw.Event("response.content_part.added", mustJSON(map[string]any{
				"type": "response.content_part.added", "item_id": itemID, "output_index": 0, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			}))
		}
		sw.Event("response.output_text.delta", mustJSON(map[string]any{
			"type": "response.output_text.delta", "item_id": itemID, "output_index": 0,
			"content_index": 0, "delta": text,
		}))
	}
	if h.streamMode(principal) == "deltas" {
		input.OnText = sendText
	}

	input.OnServerTool = func(string) { sw.Start() }
	input.OnQueued = func() { sw.Start() }
	reply, err := h.Engine.Execute(r.Context(), input)
	if err != nil {
		ge := gateway.FromError(err)
		h.logRequest(principal, req.Model, ge.Status, ge.Code, nil, time.Since(started), true, gateway.LastUserText(turns))
		sw.Event("error", mustJSON(map[string]any{
			"type": "error", "code": ge.Code, "message": ge.Message,
		}))
		return
	}
	h.logRequest(principal, req.Model, 200, "", reply, time.Since(started), true, gateway.LastUserText(turns))

	// Финальный набор элементов вывода.
	output := responseOutput(reply, itemID)
	outputIndex := 0
	if text := reply.Text(); text != "" {
		if !added {
			sendText(text)
		}
		sw.Event("response.output_text.done", mustJSON(map[string]any{
			"type": "response.output_text.done", "item_id": itemID, "output_index": outputIndex,
			"content_index": 0, "text": text,
		}))
		sw.Event("response.content_part.done", mustJSON(map[string]any{
			"type": "response.content_part.done", "item_id": itemID, "output_index": outputIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		}))
		sw.Event("response.output_item.done", mustJSON(map[string]any{
			"type": "response.output_item.done", "output_index": outputIndex, "item": output[outputIndex],
		}))
		outputIndex++
	}
	for ; outputIndex < len(output); outputIndex++ {
		item := output[outputIndex]
		pending := map[string]any{
			"type": "function_call", "id": item["id"], "call_id": item["call_id"],
			"name": item["name"], "arguments": "", "status": "in_progress",
		}
		sw.Event("response.output_item.added", mustJSON(map[string]any{
			"type": "response.output_item.added", "output_index": outputIndex, "item": pending,
		}))
		sw.Event("response.function_call_arguments.delta", mustJSON(map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": item["id"],
			"output_index": outputIndex, "delta": item["arguments"],
		}))
		sw.Event("response.function_call_arguments.done", mustJSON(map[string]any{
			"type": "response.function_call_arguments.done", "item_id": item["id"],
			"output_index": outputIndex, "arguments": item["arguments"],
		}))
		sw.Event("response.output_item.done", mustJSON(map[string]any{
			"type": "response.output_item.done", "output_index": outputIndex, "item": item,
		}))
	}

	final := buildResponse(respID, req.Model, created, output, reply.Usage)
	sw.Event("response.completed", mustJSON(map[string]any{"type": "response.completed", "response": final}))
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
		"error": map[string]any{"message": ge.Message, "type": ge.Code, "code": ge.Code},
	})
}

func (h *Handler) logRequest(p *gateway.Principal, model string, status int, errType string,
	reply *gateway.Reply, latency time.Duration, stream bool, prompt string) {
	if p == nil {
		return
	}
	entry := store.LogEntry{
		TS: time.Now(), TokenID: p.Token.ID, KeyID: p.Token.KeyID,
		Protocol: "responses", Model: model, Status: status, ErrorType: errType,
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

func normalize(req *responsesRequest) ([]gateway.Turn, []gateway.Tool, error) {
	var turns []gateway.Turn
	if len(req.Input) > 0 {
		var s string
		if err := json.Unmarshal(req.Input, &s); err == nil {
			turns = append(turns, gateway.Turn{Role: gateway.RoleUser, Text: s})
		} else {
			var items []inputItem
			if err := json.Unmarshal(req.Input, &items); err != nil {
				return nil, nil, gateway.NewError(http.StatusBadRequest, "invalid_request_error", "Неизвестный формат input")
			}
			for _, it := range items {
				switch it.Type {
				case "message", "":
					text, err := contentText(it.Content)
					if err != nil {
						return nil, nil, err
					}
					switch it.Role {
					case "user", "":
						turns = append(turns, gateway.Turn{Role: gateway.RoleUser, Text: text})
					case "assistant":
						turns = append(turns, gateway.Turn{Role: gateway.RoleAssistant, Text: text})
					case "system", "developer":
						turns = append([]gateway.Turn{{Role: gateway.RoleUser, Text: "[Системные инструкции]\n" + text}}, turns...)
					}
				case "function_call":
					turns = append(turns, gateway.Turn{Role: gateway.RoleAssistant,
						Calls: []naparnik.ToolCall{naparnik.NewToolCall(it.CallID, it.Name, it.Arguments)}})
				case "function_call_output":
					turns = append(turns, gateway.Turn{Role: gateway.RoleTool, Results: []naparnik.ToolResult{{
						ToolCallID: it.CallID, Status: "ok", Content: it.Output,
					}}})
				}
			}
		}
	}

	var tools []gateway.Tool
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		tools = append(tools, gateway.Tool{Name: t.Name, Description: t.Description, Schema: t.Parameters})
	}
	return turns, tools, nil
}

func contentText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", gateway.NewError(http.StatusBadRequest, "invalid_request_error", "Неизвестный формат content")
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text", "":
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		default:
			return "", gateway.NewError(http.StatusBadRequest, "unsupported_content",
				"Шлюз принимает только текстовые сообщения; вложения не поддерживаются.")
		}
	}
	return b.String(), nil
}

func responseOutput(reply *gateway.Reply, itemID string) []map[string]any {
	output := []map[string]any{}
	if text := reply.Text(); text != "" {
		output = append(output, map[string]any{
			"type": "message", "id": itemID, "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		})
	}
	for _, c := range reply.ToolCalls() {
		output = append(output, map[string]any{
			"type": "function_call", "id": "fc_" + randomID(), "call_id": c.ID,
			"name": c.Function.Name, "arguments": c.Function.Arguments, "status": "completed",
		})
	}
	return output
}

func buildResponse(id, model string, created int64, output []map[string]any, usage *naparnik.Usage) map[string]any {
	out := map[string]any{
		"id": id, "object": "response", "created_at": created,
		"model": model, "status": "completed", "output": output,
	}
	if usage != nil {
		u := map[string]any{
			"input_tokens":  usage.PromptTokens,
			"output_tokens": usage.CompletionTokens,
			"total_tokens":  usage.TotalTokens,
		}
		if usage.CachedTokens > 0 && usage.CachedTokens <= usage.PromptTokens {
			u["input_tokens_details"] = map[string]any{"cached_tokens": usage.CachedTokens}
		}
		out["usage"] = u
	}
	return out
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
	for _, h := range []string{"x-session-id", "x-conversation-id"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	return ""
}
