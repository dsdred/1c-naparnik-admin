// Package gateway — ядро шлюза: нормализация запросов, привязка сессий,
// последовательная работа с диалогами Напарника, маршрутизация инструментов.
package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"naparnikadmin/internal/config"
	"naparnikadmin/internal/naparnik"
	"naparnikadmin/internal/store"
)

// Роли турнов во внутреннем представлении.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Turn — один шаг истории в нормализованном виде.
type Turn struct {
	Role    string
	Text    string
	Calls   []naparnik.ToolCall
	Results []naparnik.ToolResult
}

// Tool — клиентский инструмент.
type Tool struct {
	Name        string
	Description string
	Schema      any
}

// LastUserText возвращает последнюю непустую реплику пользователя —
// её сохраняем в журнале, чтобы видеть, что именно спрашивали.
func LastUserText(turns []Turn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == RoleUser && strings.TrimSpace(turns[i].Text) != "" {
			return turns[i].Text
		}
	}
	return ""
}

// Part — фрагмент ответа ассистента (их может быть несколько в одном ответе).
type Part struct {
	Text      string
	Reasoning string
	ToolCalls []naparnik.ToolCall
}

// Reply — собранный ответ Напарника.
type Reply struct {
	Parts   []Part
	Usage   *naparnik.Usage
	Context *naparnik.ContextInfo
}

// Text склеивает текст всех фрагментов.
func (r *Reply) Text() string {
	parts := make([]string, 0, len(r.Parts))
	for _, p := range r.Parts {
		if p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// ToolCalls собирает вызовы инструментов из всех фрагментов.
func (r *Reply) ToolCalls() []naparnik.ToolCall {
	var out []naparnik.ToolCall
	for _, p := range r.Parts {
		out = append(out, p.ToolCalls...)
	}
	return out
}

// Input — запрос к движку от протокольного адаптера.
type Input struct {
	Token     store.Token
	Key       string
	Protocol  string
	SessionID string
	Turns     []Turn
	Tools     []Tool

	// OnText вызывается на каждую дельту текста (режим deltas).
	OnText func(text string)
	// OnServerTool вызывается, когда Напарник запросил серверный инструмент.
	OnServerTool func(name string)
}

// Engine — исполнитель запросов.
type Engine struct {
	client *naparnik.Client
	store  *store.Store
	cfg    *config.Config

	mu        sync.Mutex
	locks     map[string]*sync.Mutex
	ctxWindow atomic.Int64
}

// ContextWindow возвращает последнее известное значение max_context_length.
func (e *Engine) ContextWindow() int { return int(e.ctxWindow.Load()) }

// New создаёт движок.
func New(client *naparnik.Client, st *store.Store, cfg *config.Config) *Engine {
	return &Engine{client: client, store: st, cfg: cfg, locks: map[string]*sync.Mutex{}}
}

func (e *Engine) lockFor(key string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	m, ok := e.locks[key]
	if !ok {
		m = &sync.Mutex{}
		e.locks[key] = m
	}
	return m
}

// Execute прогоняет запрос через Напарника и возвращает собранный ответ.
func (e *Engine) Execute(ctx context.Context, in Input) (*Reply, error) {
	if strings.TrimSpace(in.Key) == "" {
		return nil, &Error{Status: 401, Code: "no_token", Message: "у токена не настроен ключ Напарника"}
	}
	if len(in.Turns) == 0 {
		return nil, &Error{Status: 400, Code: "invalid_request_error", Message: "пустая история сообщений"}
	}

	sessionKey := strings.TrimSpace(in.SessionID)
	sticky := sessionKey == ""
	if sticky {
		sessionKey = "sticky"
	}

	mu := e.lockFor(fmt.Sprintf("%d:%s:%s", in.Token.ID, in.Protocol, sessionKey))
	mu.Lock()
	defer mu.Unlock()

	binding, err := e.store.GetSession(ctx, in.Token.ID, in.Protocol, sessionKey)
	expired := err == nil && e.cfg.SessionTTL.D() > 0 && timeSince(binding.UpdatedAt) > e.cfg.SessionTTL.D()
	newConversation := errors.Is(err, store.ErrNotFound) || expired || binding.Conversation == ""

	fingerprint := fingerprintOf(in.Turns)
	if !newConversation && sticky && binding.Fingerprint != "" && binding.Fingerprint != fingerprint {
		newConversation = true
	}

	wireTools, wireMap := wireToolMap(in.Tools)
	toolMap := map[string]string{}
	if !newConversation {
		_ = json.Unmarshal([]byte(binding.ToolsJSON), &toolMap)
	}
	for wire, orig := range wireMap {
		toolMap[wire] = orig
	}

	var conversation string
	var parent *string
	if newConversation {
		conversation, err = e.client.CreateConversation(ctx, in.Key, "ru", "1c", "ru")
		if err != nil {
			return nil, err
		}
	} else {
		conversation = binding.Conversation
		if binding.Parent != "" {
			p := binding.Parent
			parent = &p
		}
	}

	turns := in.Turns
	if !newConversation {
		turns = tailAfterLastAssistant(turns)
		if len(turns) == 0 {
			// Клиент повторил уже отправленную историю — не генерируем заново.
			turns = in.Turns[len(in.Turns)-1:]
		}
	}

	groups := groupTurns(turns)

	reply := &Reply{}
	var lastUUID string

	// Для нового диалога «засеваем» историю без генерации: по одному запросу на группу,
	// генерация — только на последней.
	if newConversation && len(groups) > 1 {
		for _, g := range groups[:len(groups)-1] {
			uuid, err := e.send(ctx, in.Key, conversation, parent, g, wireTools, true, nil)
			if err != nil {
				return nil, err
			}
			if uuid != "" {
				p := uuid
				parent = &p
				lastUUID = uuid
			}
		}
		groups = groups[len(groups)-1:]
	}

	queue := groups
	rounds := 0
	for len(queue) > 0 {
		g := queue[0]
		queue = queue[1:]

		var serverResults []naparnik.ToolResult
		uuid, err := e.send(ctx, in.Key, conversation, parent, g, wireTools, false, func(p naparnik.Packet) {
			if p.UUID != "" {
				lastUUID = p.UUID
			}
			if !p.Finished {
				if in.OnText != nil && p.ContentDelta != nil && p.ContentDelta.Content != "" {
					in.OnText(p.ContentDelta.Content)
				}
				return
			}
			if p.Usage != nil {
				reply.Usage = p.Usage
			}
			if p.Details != nil && p.Details.MaxContextLength != nil {
				mcl := *p.Details.MaxContextLength
				tc := 0
				if p.Usage != nil {
					tc = p.Usage.TotalTokens
				}
				reply.Context = &naparnik.ContextInfo{TokenCount: tc, MaxContextLength: mcl}
			}
			if p.Role != "assistant" || p.Content == nil {
				return
			}
			part := Part{
				Text:      p.Content.Content,
				Reasoning: p.Content.ReasoningContent,
			}
			for _, call := range p.Content.ToolCalls {
				if call.ID == "" || call.Function.Name == "" {
					continue
				}
				if orig, ok := toolMap[call.Function.Name]; ok {
					call.Function.Name = orig
					part.ToolCalls = append(part.ToolCalls, call)
					continue
				}
				// Инструмент не наш — значит, серверный (поиск по ИТС и т.п.).
				if in.OnServerTool != nil {
					in.OnServerTool(call.Function.Name)
				}
				serverResults = append(serverResults, naparnik.ToolResult{
					ToolCallID: call.ID,
					Status:     "accepted",
					Content:    nil,
				})
			}
			reply.Parts = append(reply.Parts, part)
		})
		if err != nil {
			return nil, err
		}
		if uuid != "" {
			p := uuid
			parent = &p
		}

		if len(serverResults) > 0 && rounds < 6 {
			results := serverResults
			rounds++
			queue = append([]Turn{{Role: RoleTool, Results: results}}, queue...)
		}
	}

	if parent != nil {
		lastUUID = *parent
	}

	// Уточняем контекст и при необходимости сжимаем.
	if info, err := e.client.ContextTokens(ctx, in.Key, conversation, lastUUID); err == nil && info.MaxContextLength > 0 {
		reply.Context = info
		e.ctxWindow.Store(int64(info.MaxContextLength))
		if e.cfg.AutoCompactAt > 0 && float64(info.TokenCount)/float64(info.MaxContextLength) >= e.cfg.AutoCompactAt {
			if _, err := e.client.Compact(ctx, in.Key, conversation, ""); err == nil {
				if info2, err := e.client.ContextTokens(ctx, in.Key, conversation, ""); err == nil {
					reply.Context = info2
				}
			}
		}
	}

	if len(toolMap) > 0 {
		if data, err := json.Marshal(toolMap); err == nil {
			binding.ToolsJSON = string(data)
		}
	}
	binding.TokenID = in.Token.ID
	binding.Protocol = in.Protocol
	binding.SessionID = sessionKey
	binding.Conversation = conversation
	binding.Parent = lastUUID
	binding.Fingerprint = fingerprint
	if err := e.store.SaveSession(ctx, binding); err != nil {
		return nil, err
	}
	return reply, nil
}

// send выполняет один запрос к диалогу и разбирает поток.
func (e *Engine) send(ctx context.Context, key, conversation string, parent *string, t Turn,
	tools []naparnik.Tool, noGenerate bool, onPacket func(naparnik.Packet)) (string, error) {

	body := naparnik.MessageRequest{
		ParentUUID: parent,
		Role:       RoleUser,
		NoGenerate: noGenerate,
	}
	switch t.Role {
	case RoleTool:
		body.Role = RoleTool
		items := make([]any, 0, len(t.Results))
		for _, r := range t.Results {
			items = append(items, r)
		}
		body.Content = items
	default:
		body.Role = RoleUser
		body.Content = t.Text
		if len(tools) > 0 && t.Role == RoleUser {
			body.Tools = tools
		}
	}

	stream, err := e.client.StreamMessages(ctx, key, conversation, body)
	if err != nil {
		return "", err
	}

	var lastUUID string
	for p := range stream.Packets {
		if p.UUID != "" {
			lastUUID = p.UUID
		}
		if onPacket != nil {
			onPacket(p)
		}
	}
	if err := stream.Wait(); err != nil {
		return lastUUID, err
	}
	return lastUUID, nil
}

// groupTurns склеивает подряд идущие user/assistant в один user-турн,
// а подряд идущие tool-результаты — в один tool-турн.
func groupTurns(turns []Turn) []Turn {
	var out []Turn
	for _, t := range turns {
		if t.Role == RoleTool {
			if len(t.Results) == 0 {
				continue
			}
			if n := len(out); n > 0 && out[n-1].Role == RoleTool {
				out[n-1].Results = append(out[n-1].Results, t.Results...)
				continue
			}
			out = append(out, Turn{Role: RoleTool, Results: append([]naparnik.ToolResult(nil), t.Results...)})
			continue
		}

		chunk := t.Text
		if t.Role == RoleAssistant {
			chunk = assistantAsText(t)
		}
		if chunk == "" {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Role == RoleUser {
			out[n-1].Text = strings.TrimSpace(out[n-1].Text + "\n\n" + chunk)
			continue
		}
		out = append(out, Turn{Role: RoleUser, Text: chunk})
	}
	return out
}

// assistantAsText представляет реплику ассистента в виде, понятном Напарнику.
func assistantAsText(t Turn) string {
	if len(t.Calls) == 0 {
		return t.Text
	}
	payload := map[string]any{"role": "assistant", "content": t.Text}
	calls := make([]map[string]any, 0, len(t.Calls))
	for _, c := range t.Calls {
		calls = append(calls, map[string]any{
			"id":   c.ID,
			"name": c.Function.Name,
			"args": c.Function.Arguments,
		})
	}
	payload["tool_calls"] = calls
	data, err := json.Marshal(payload)
	if err != nil {
		return t.Text
	}
	return string(data)
}

// tailAfterLastAssistant оставляет только новые турны после последней реплики ассистента.
func tailAfterLastAssistant(turns []Turn) []Turn {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == RoleAssistant {
			return turns[i+1:]
		}
	}
	return turns
}

// fingerprintOf считает отпечаток первого пользовательского сообщения —
// по нему sticky-сессия понимает, что клиент начал новый чат.
func fingerprintOf(turns []Turn) string {
	for _, t := range turns {
		if t.Role == RoleUser && strings.TrimSpace(t.Text) != "" {
			sum := sha256.Sum256([]byte(strings.TrimSpace(t.Text)))
			return hex.EncodeToString(sum[:8])
		}
	}
	return ""
}
