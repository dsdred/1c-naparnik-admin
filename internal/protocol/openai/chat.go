// Package openai реализует входящий протокол OpenAI Chat Completions.
package openai

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

// Handler — обработчики OpenAI-совместимых эндпоинтов.
type Handler struct {
	Auth   *gateway.Auth
	Engine *gateway.Engine
	Store  *store.Store
	Cfg    *config.Config
	Log    *slog.Logger
}

type chatRequest struct {
	Model         string          `json:"model"`
	Messages      []chatMessage   `json:"messages"`
	Stream        bool            `json:"stream"`
	Tools         []chatTool      `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters"`
	} `json:"function"`
}

// ChatCompletions обрабатывает POST /v1/chat/completions.
func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	var req chatRequest
	if err := httpx.ReadJSON(w, r, &req, 0); err != nil {
		h.writeError(w, nil, gateway.NewError(http.StatusBadRequest, "invalid_json", "Не удалось разобрать тело запроса: "+err.Error()))
		return
	}
	principal, err := h.Auth.Resolve(r.Context(), gateway.FromBearer(r))
	if err != nil {
		h.writeError(w, nil, err)
		return
	}
	req.Model = h.modelName(req.Model)

	system, turns, tools, err := normalizeChat(&req)
	if err != nil {
		h.writeError(w, principal, err)
		return
	}
	if system != "" {
		turns = append([]gateway.Turn{{Role: gateway.RoleUser, Text: "[Системные инструкции]\n" + system}}, turns...)
	}

	streamMode := h.streamMode(principal)
	input := gateway.Input{
		Token:     principal.Token,
		Key:       principal.Key,
		Protocol:  "openai",
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
		h.logRequest(r, principal, req.Model, 200, "", reply, time.Since(started), false, gateway.LastUserText(turns))
		httpx.JSON(w, http.StatusOK, buildCompletion(req.Model, reply))
		return
	}

	sw := httpx.NewWriter(w)
	defer sw.Close()

	includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
	var streamed strings.Builder
	if streamMode == "deltas" {
		input.OnText = func(text string) {
			streamed.WriteString(text)
			sw.Event("", chunkJSON(req.Model, map[string]any{"content": text}, nil, nil))
		}
		input.OnReasoning = func(text string) {
			sw.Event("", chunkJSON(req.Model, map[string]any{"reasoning_content": text}, nil, nil))
		}
	}
	input.OnServerTool = func(string) { sw.Start() }

	reply, err := h.Engine.Execute(r.Context(), input)
	if err != nil {
		ge := gateway.FromError(err)
		h.logRequest(r, principal, req.Model, ge.Status, ge.Code, nil, time.Since(started), true, gateway.LastUserText(turns))
		sw.Event("", errorJSON(ge))
		return
	}
	h.logRequest(r, principal, req.Model, 200, "", reply, time.Since(started), true, gateway.LastUserText(turns))

	// Первый чанк с ролью.
	sw.Event("", chunkJSON(req.Model, map[string]any{"role": "assistant", "content": ""}, nil, nil))

	if streamMode != "deltas" {
		if r := reply.Reasoning(); r != "" {
			sw.Event("", chunkJSON(req.Model, map[string]any{"reasoning_content": r}, nil, nil))
		}
		if text := reply.Text(); text != "" {
			sw.Event("", chunkJSON(req.Model, map[string]any{"content": text}, nil, nil))
		}
	} else {
		// Дельты могут быть неполными: сервер вправе отфильтровать или поправить текст,
		// и финальный content приоритетнее накопленных дельт. Досылаем недостающее,
		// иначе клиент остаётся с обрезанным ответом.
		if tail := streamTail(reply.Text(), streamed.String()); tail != "" {
			sw.Event("", chunkJSON(req.Model, map[string]any{"content": tail}, nil, nil))
		}
	}
	if calls := reply.ToolCalls(); len(calls) > 0 {
		out := make([]map[string]any, 0, len(calls))
		for i, c := range calls {
			out = append(out, map[string]any{
				"index": i,
				"id":    c.ID,
				"type":  "function",
				"function": map[string]any{
					"name":      c.Function.Name,
					"arguments": c.Function.Arguments,
				},
			})
		}
		sw.Event("", chunkJSON(req.Model, map[string]any{"tool_calls": out}, nil, nil))
	}

	finish := "stop"
	if len(reply.ToolCalls()) > 0 {
		finish = "tool_calls"
	}
	sw.Event("", chunkJSON(req.Model, map[string]any{}, &finish, nil))

	if includeUsage && reply.Usage != nil {
		sw.Event("", chunkJSON(req.Model, nil, nil, usageMap(reply.Usage)))
	}
	sw.Event("", "[DONE]")
}

// Models обрабатывает GET /v1/models.
func (h *Handler) Models(w http.ResponseWriter, r *http.Request) {
	if _, err := h.Auth.Resolve(r.Context(), gateway.FromBearer(r)); err != nil {
		h.writeError(w, nil, err)
		return
	}
	model := map[string]any{
		"id":       h.Cfg.ModelName(),
		"object":   "model",
		"created":  0,
		"owned_by": "naparnik-admin",
	}
	if cw := h.Engine.ContextWindow(); cw > 0 {
		model["context_length"] = cw
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{model}})
}

func (h *Handler) streamMode(p *gateway.Principal) string {
	if p.Token.StreamMode == "deltas" || p.Token.StreamMode == "final" {
		return p.Token.StreamMode
	}
	return h.Cfg.StreamMode
}

func (h *Handler) writeError(w http.ResponseWriter, p *gateway.Principal, err error) {
	ge := gateway.FromError(err)
	if p != nil {
		h.logRequest(nil, p, "", ge.Status, ge.Code, nil, 0, false, "")
	}
	httpx.JSON(w, ge.Status, map[string]any{
		"error": map[string]any{"message": ge.Message, "type": ge.Code, "code": ge.Code},
	})
}

func (h *Handler) logRequest(r *http.Request, p *gateway.Principal, model string, status int, errType string,
	reply *gateway.Reply, latency time.Duration, stream bool, prompt string) {
	if p == nil {
		return
	}
	entry := store.LogEntry{
		TS:        time.Now(),
		TokenID:   p.Token.ID,
		KeyID:     p.Token.KeyID,
		Protocol:  "openai",
		Model:     model,
		Status:    status,
		ErrorType: errType,
		LatencyMS: int(latency.Milliseconds()),
		Stream:    stream,
		Prompt:    prompt,
	}
	if reply != nil && reply.Usage != nil {
		entry.PromptTokens = reply.Usage.PromptTokens
		entry.CompTokens = reply.Usage.CompletionTokens
		entry.CachedTokens = reply.Usage.CachedTokens
	}
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	if err := h.Store.LogRequest(ctx, entry); err != nil && h.Log != nil {
		h.Log.Warn("не удалось записать журнал запроса", "err", err)
	}
}

// normalizeChat переводит запрос OpenAI во внутреннее представление.
func normalizeChat(req *chatRequest) (string, []gateway.Turn, []gateway.Tool, error) {
	var system []string
	var turns []gateway.Turn

	for _, m := range req.Messages {
		text, hasMedia, err := contentText(m.Content)
		if err != nil {
			return "", nil, nil, err
		}
		if hasMedia {
			return "", nil, nil, gateway.NewError(http.StatusBadRequest, "unsupported_content",
				"Шлюз принимает только текстовые сообщения; вложения и изображения не поддерживаются.")
		}
		switch m.Role {
		case "system", "developer":
			if text != "" {
				system = append(system, text)
			}
		case "user":
			turns = append(turns, gateway.Turn{Role: gateway.RoleUser, Text: text})
		case "assistant":
			t := gateway.Turn{Role: gateway.RoleAssistant, Text: text}
			for _, c := range m.ToolCalls {
				t.Calls = append(t.Calls, toNaparnikCall(c))
			}
			turns = append(turns, t)
		case "tool", "function":
			content := any(text)
			if text == "" {
				content = nil
			}
			turns = append(turns, gateway.Turn{Role: gateway.RoleTool, Results: []naparnik.ToolResult{{
				ToolCallID: m.ToolCallID,
				Status:     "ok",
				Content:    content,
			}}})
		}
	}

	var tools []gateway.Tool
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		tools = append(tools, gateway.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Schema:      t.Function.Parameters,
		})
	}
	return strings.Join(system, "\n\n"), turns, tools, nil
}

func toNaparnikCall(c chatToolCall) naparnik.ToolCall {
	var call naparnik.ToolCall
	call.ID = c.ID
	call.Type = "function"
	call.Function.Name = c.Function.Name
	call.Function.Arguments = c.Function.Arguments
	return call
}

// contentText извлекает текст из content (строка или массив частей).
func contentText(raw json.RawMessage) (string, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, false, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", false, gateway.NewError(http.StatusBadRequest, "invalid_request_error", "Неизвестный формат content")
	}
	var b strings.Builder
	hasMedia := false
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text", "output_text":
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		case "":
			// часть без типа — считаем текстом
			b.WriteString(p.Text)
		default:
			hasMedia = true
		}
	}
	return b.String(), hasMedia, nil
}

// streamTail возвращает кусок финального текста, которого не было в дельтах.
// Напарник вправе отфильтровать или поправить текст, поэтому финальный content
// приоритетнее накопленных дельт.
func streamTail(full, sent string) string {
	switch {
	case full == "" || full == sent:
		return ""
	case sent == "":
		return full
	case strings.HasPrefix(full, sent):
		return full[len(sent):]
	default:
		return full
	}
}

func buildCompletion(model string, reply *gateway.Reply) map[string]any {
	msg := map[string]any{"role": "assistant", "content": reply.Text()}
	// Рассуждения Напарника отдаём как reasoning_content — так их показывают
	// Cline, Roo Code, Kilo Code, OpenCode и другие OpenAI-совместимые клиенты.
	if r := reply.Reasoning(); r != "" {
		msg["reasoning_content"] = r
	}
	if calls := reply.ToolCalls(); len(calls) > 0 {
		out := make([]map[string]any, 0, len(calls))
		for _, c := range calls {
			out = append(out, map[string]any{
				"id":   c.ID,
				"type": "function",
				"function": map[string]any{
					"name":      c.Function.Name,
					"arguments": c.Function.Arguments,
				},
			})
		}
		msg["tool_calls"] = out
		if reply.Text() == "" {
			msg["content"] = nil
		}
	}
	finish := "stop"
	if len(reply.ToolCalls()) > 0 {
		finish = "tool_calls"
	}
	out := map[string]any{
		"id":      "chatcmpl-" + randomID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
	}
	if reply.Usage != nil {
		out["usage"] = usageMap(reply.Usage)
	}
	if reply.Context != nil && reply.Context.MaxContextLength > 0 {
		out["naparnik_context"] = map[string]any{
			"token_count":        reply.Context.TokenCount,
			"max_context_length": reply.Context.MaxContextLength,
		}
	}
	return out
}

func chunkJSON(model string, delta map[string]any, finish *string, usage map[string]any) string {
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != nil {
		choice["finish_reason"] = *finish
	} else {
		choice["finish_reason"] = nil
	}
	payload := map[string]any{
		"id":      "chatcmpl-" + randomID(),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
	}
	if delta == nil {
		payload["choices"] = []any{}
	} else {
		payload["choices"] = []any{choice}
	}
	if usage != nil {
		payload["usage"] = usage
	} else {
		payload["usage"] = nil
	}
	data, _ := json.Marshal(payload)
	return string(data)
}

func usageMap(u *naparnik.Usage) map[string]any {
	out := map[string]any{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
	}
	if u.CachedTokens > 0 && u.CachedTokens <= u.PromptTokens {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": u.CachedTokens}
	}
	if u.ReasoningTokens > 0 && u.ReasoningTokens <= u.CompletionTokens {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": u.ReasoningTokens}
	}
	return out
}

func errorJSON(ge *gateway.Error) string {
	data, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": ge.Message, "type": ge.Code, "code": ge.Code},
	})
	return string(data)
}

// modelName подставляет настроенный id модели, если клиент его не указал.
func (h *Handler) modelName(model string) string {
	if strings.TrimSpace(model) != "" {
		return model
	}
	return h.Cfg.ModelName()
}

// randomID возвращает короткий случайный идентификатор для полей id ответов.
func randomID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "000000000000"
	}
	return hex.EncodeToString(buf)
}

// sessionID читает идентификатор сессии клиента из типовых заголовков.
func sessionID(r *http.Request) string {
	for _, h := range []string{"x-session-id", "x-conversation-id", "x-cline-task-id", "x-request-id"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	return ""
}
