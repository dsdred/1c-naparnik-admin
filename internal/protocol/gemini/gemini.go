// Package gemini реализует входящий протокол Google Gemini (generateContent).
package gemini

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"naparnikadmin/internal/config"
	"naparnikadmin/internal/gateway"
	"naparnikadmin/internal/httpx"
	"naparnikadmin/internal/naparnik"
	"naparnikadmin/internal/store"
)

// Handler — обработчики /v1beta/models/{model}:generateContent.
type Handler struct {
	Auth   *gateway.Auth
	Engine *gateway.Engine
	Store  *store.Store
	Cfg    config.Source
	Log    *slog.Logger

	// callIDs хранит соответствие «имя функции → id вызовов» в рамках сессии:
	// формат Gemini не передаёт идентификатор вызова инструмента. Список нужен
	// из-за параллельных вызовов одной функции: у них разные id, а имя одно,
	// поэтому ответы сопоставляются по порядку поступления.
	callIDs sync.Map
}

// callQueue — очередь id вызовов одной функции: ответы приходят в том же
// порядке, в каком были объявлены вызовы.
type callQueue struct {
	mu  sync.Mutex
	ids []string
}

type generateRequest struct {
	Contents          []content `json:"contents"`
	SystemInstruction *content  `json:"systemInstruction"`
	Tools             []struct {
		FunctionDeclarations []declaration `json:"functionDeclarations"`
	} `json:"tools"`
}

type content struct {
	Role  string `json:"role"`
	Parts []part `json:"parts"`
}

type declaration struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

type part struct {
	Text         string `json:"text"`
	FunctionCall *struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"functionCall"`
	FunctionResponse *struct {
		Name     string          `json:"name"`
		Response json.RawMessage `json:"response"`
	} `json:"functionResponse"`
	InlineData any `json:"inlineData"`
}

// Handle обслуживает POST /v1beta/models/{model}:generateContent и :streamGenerateContent.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	model, stream, ok := parsePath(r.URL.Path)
	if !ok {
		h.writeError(w, nil, gateway.NewError(http.StatusNotFound, "not_found", "Неизвестный эндпоинт Gemini"))
		return
	}

	var req generateRequest
	if err := httpx.ReadJSON(w, r, &req, 0); err != nil {
		h.writeError(w, nil, gateway.NewError(http.StatusBadRequest, "INVALID_ARGUMENT", "Не удалось разобрать тело запроса: "+err.Error()))
		return
	}
	principal, err := h.Auth.Resolve(r.Context(), gateway.FromGemini(r), "gemini")
	if err != nil {
		h.writeError(w, nil, err)
		return
	}

	turns, tools, err := h.normalize(principal.Token.ID, sessionID(r), &req)
	if err != nil {
		h.writeError(w, principal, err)
		return
	}

	input := gateway.Input{
		Token: principal.Token, Key: principal.Key, Protocol: "gemini",
		SessionID: sessionID(r), Turns: turns, Tools: tools,
	}

	if !stream {
		reply, err := h.Engine.Execute(r.Context(), input)
		if err != nil {
			h.writeError(w, principal, err)
			return
		}
		h.remember(principal.Token.ID, sessionID(r), reply)
		h.logRequest(principal, model, 200, "", reply, time.Since(started), false, gateway.LastUserText(turns))
		httpx.JSON(w, http.StatusOK, h.buildResponse(model, reply))
		return
	}

	sw := httpx.NewWriter(w)
	defer sw.Close()

	streamMode := h.streamMode(principal)
	if streamMode == "deltas" {
		input.OnText = func(text string) {
			sw.Event("", mustJSON(map[string]any{
				"candidates": []any{map[string]any{
					"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": text}}},
					"index":   0,
				}},
			}))
		}
	}
	input.OnServerTool = func(string) { sw.Start() }
	input.OnQueued = func() { sw.Start() }
	input.OnStarted = func() { sw.Start() }

	reply, err := h.Engine.Execute(r.Context(), input)
	if err != nil {
		ge := gateway.FromError(err)
		h.logRequest(principal, model, ge.Status, ge.Code, nil, time.Since(started), true, gateway.LastUserText(turns))
		sw.Event("", mustJSON(geminiError(ge)))
		return
	}
	h.remember(principal.Token.ID, sessionID(r), reply)
	h.logRequest(principal, model, 200, "", reply, time.Since(started), true, gateway.LastUserText(turns))

	if streamMode != "deltas" {
		if text := reply.Text(); text != "" {
			sw.Event("", mustJSON(map[string]any{
				"candidates": []any{map[string]any{
					"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": text}}},
					"index":   0,
				}},
			}))
		}
	}
	if calls := reply.ToolCalls(); len(calls) > 0 {
		parts := make([]any, 0, len(calls))
		for _, c := range calls {
			var args any = map[string]any{}
			if strings.TrimSpace(c.Function.Arguments) != "" {
				_ = json.Unmarshal([]byte(c.Function.Arguments), &args)
			}
			parts = append(parts, map[string]any{
				"functionCall": map[string]any{"name": c.Function.Name, "args": args},
			})
		}
		sw.Event("", mustJSON(map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{"role": "model", "parts": parts},
				"index":   0,
			}},
		}))
	}

	final := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": []any{}},
			"finishReason": finishReason(reply),
			"index":        0,
		}},
	}
	if reply.Usage != nil {
		final["usageMetadata"] = usageMetadata(reply.Usage)
	}
	sw.Event("", mustJSON(final))
}

func (h *Handler) streamMode(p *gateway.Principal) string {
	if p.Token.StreamMode == "deltas" || p.Token.StreamMode == "final" {
		return p.Token.StreamMode
	}
	return h.Cfg.Snapshot().StreamMode
}

// normalize переводит запрос Gemini во внутреннее представление.
func (h *Handler) normalize(tokenID int64, session string, req *generateRequest) ([]gateway.Turn, []gateway.Tool, error) {
	var turns []gateway.Turn
	if req.SystemInstruction != nil {
		text := partsText(req.SystemInstruction.Parts)
		if text != "" {
			turns = append(turns, gateway.Turn{Role: gateway.RoleUser, Text: "[Системные инструкции]\n" + text})
		}
	}

	for _, c := range req.Contents {
		role := c.Role
		if role == "" {
			role = "user"
		}
		switch role {
		case "user":
			var texts []string
			for _, p := range c.Parts {
				switch {
				case p.Text != "":
					texts = append(texts, p.Text)
				case p.FunctionResponse != nil:
					payload := any(string(p.FunctionResponse.Response))
					if len(p.FunctionResponse.Response) > 0 {
						var v any
						if err := json.Unmarshal(p.FunctionResponse.Response, &v); err == nil {
							payload = v
						}
					}
					callID := h.takeCallID(tokenID, session, p.FunctionResponse.Name)
					if callID == "" {
						callID = p.FunctionResponse.Name
					}
					turns = append(turns, gateway.Turn{Role: gateway.RoleTool, Results: []naparnik.ToolResult{{
						ToolCallID: callID, Status: "ok", Content: payload,
					}}})
				case p.InlineData != nil:
					return nil, nil, gateway.NewError(http.StatusBadRequest, "INVALID_ARGUMENT",
						"Шлюз принимает только текст; вложения не поддерживаются.")
				}
			}
			if len(texts) > 0 {
				turns = append(turns, gateway.Turn{Role: gateway.RoleUser, Text: strings.Join(texts, "\n")})
			}
		case "model", "assistant":
			t := gateway.Turn{Role: gateway.RoleAssistant}
			for _, p := range c.Parts {
				if p.Text != "" {
					if t.Text != "" {
						t.Text += "\n"
					}
					t.Text += p.Text
				}
				if p.FunctionCall != nil {
					args := "{}"
					if len(p.FunctionCall.Args) > 0 {
						args = string(p.FunctionCall.Args)
					}
					call := naparnik.NewToolCall("call_"+p.FunctionCall.Name, p.FunctionCall.Name, args)
					t.Calls = append(t.Calls, call)
				}
			}
			turns = append(turns, t)
		}
	}

	var tools []gateway.Tool
	for _, t := range req.Tools {
		for _, d := range t.FunctionDeclarations {
			tools = append(tools, gateway.Tool{Name: d.Name, Description: d.Description, Schema: d.Parameters})
		}
	}
	return turns, tools, nil
}

func (h *Handler) buildResponse(model string, reply *gateway.Reply) map[string]any {
	parts := []any{}
	if text := reply.Text(); text != "" {
		parts = append(parts, map[string]any{"text": text})
	}
	for _, c := range reply.ToolCalls() {
		var args any = map[string]any{}
		if strings.TrimSpace(c.Function.Arguments) != "" {
			_ = json.Unmarshal([]byte(c.Function.Arguments), &args)
		}
		parts = append(parts, map[string]any{
			"functionCall": map[string]any{"name": c.Function.Name, "args": args},
		})
	}
	out := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": parts},
			"finishReason": finishReason(reply),
			"index":        0,
		}},
		"modelVersion": model,
	}
	if reply.Usage != nil {
		out["usageMetadata"] = usageMetadata(reply.Usage)
	}
	return out
}

// remember запоминает id вызовов, чтобы сопоставить их с functionResponse.
// Для параллельных вызовов одной функции сохраняется список: имя одно, а id разные.
func (h *Handler) remember(tokenID int64, session string, reply *gateway.Reply) {
	byName := map[string][]string{}
	order := make([]string, 0, len(reply.ToolCalls()))
	for _, c := range reply.ToolCalls() {
		name := c.Function.Name
		if _, seen := byName[name]; !seen {
			order = append(order, name)
		}
		byName[name] = append(byName[name], c.ID)
	}
	for _, name := range order {
		h.callIDs.Store(callKey(tokenID, session, name), &callQueue{ids: byName[name]})
	}
}

// takeCallID выдаёт очередной id вызова по имени функции. Порядок ответов
// совпадает с порядком объявленных вызовов, поэтому очередь идёт по кругу:
// когда ответов больше, чем id, возвращается последний известный.
func (h *Handler) takeCallID(tokenID int64, session, name string) string {
	v, ok := h.callIDs.Load(callKey(tokenID, session, name))
	if !ok {
		return ""
	}
	q, ok := v.(*callQueue)
	if !ok || len(q.ids) == 0 {
		return ""
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	id := q.ids[0]
	if len(q.ids) > 1 {
		q.ids = q.ids[1:]
	}
	return id
}

func callKey(tokenID int64, session, name string) string {
	return strings.Join([]string{itoa(tokenID), session, name}, "|")
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func partsText(parts []part) string {
	var texts []string
	for _, p := range parts {
		if p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func finishReason(reply *gateway.Reply) string {
	if len(reply.ToolCalls()) > 0 {
		return "STOP"
	}
	return "STOP"
}

func usageMetadata(u *naparnik.Usage) map[string]any {
	out := map[string]any{
		"promptTokenCount":     u.PromptTokens,
		"candidatesTokenCount": u.CompletionTokens,
		"totalTokenCount":      u.TotalTokens,
	}
	if u.CachedTokens > 0 && u.CachedTokens <= u.PromptTokens {
		out["cachedContentTokenCount"] = u.CachedTokens
	}
	return out
}

// parsePath разбирает /v1beta/models/{model}:generateContent[?alt=sse].
func parsePath(path string) (model string, stream bool, ok bool) {
	const prefix = "/v1beta/models/"
	if !strings.HasPrefix(path, prefix) {
		return "", false, false
	}
	rest := strings.TrimPrefix(path, prefix)
	idx := strings.LastIndex(rest, ":")
	if idx <= 0 {
		return "", false, false
	}
	model = rest[:idx]
	switch rest[idx+1:] {
	case "generateContent":
		return model, false, true
	case "streamGenerateContent":
		return model, true, true
	}
	return "", false, false
}

func (h *Handler) writeError(w http.ResponseWriter, p *gateway.Principal, err error) {
	ge := gateway.FromError(err)
	if p != nil {
		h.logRequest(p, "", ge.Status, ge.Code, nil, 0, false, "")
	}
	httpx.JSON(w, ge.Status, geminiError(ge))
}

func geminiError(ge *gateway.Error) map[string]any {
	status := "INTERNAL"
	switch ge.Status {
	case http.StatusBadRequest:
		status = "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		status = "UNAUTHENTICATED"
	case http.StatusForbidden:
		status = "PERMISSION_DENIED"
	case http.StatusNotFound:
		status = "NOT_FOUND"
	case http.StatusTooManyRequests:
		status = "RESOURCE_EXHAUSTED"
	case http.StatusPaymentRequired:
		status = "RESOURCE_EXHAUSTED"
	}
	return map[string]any{"error": map[string]any{"code": ge.Status, "message": ge.Message, "status": status}}
}

func (h *Handler) logRequest(p *gateway.Principal, model string, status int, errType string,
	reply *gateway.Reply, latency time.Duration, stream bool, prompt string) {
	if p == nil {
		return
	}
	entry := store.LogEntry{
		TS: time.Now(), TokenID: p.Token.ID, KeyID: p.Token.KeyID,
		Protocol: "gemini", Model: model, Status: status, ErrorType: errType,
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

func mustJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func sessionID(r *http.Request) string {
	for _, h := range []string{"x-session-id", "x-conversation-id"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	return ""
}
