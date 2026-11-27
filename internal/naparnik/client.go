// Package naparnik — клиент публичного API «1С:Напарник разработчика».
package naparnik

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"naparnikadmin/internal/sse"
)

// sseRead — обёртка над разбором SSE, чтобы не тянуть зависимость в вызывающий код.
// Client — клиент Напарника. Один экземпляр на процесс, ключ передаётся в каждый вызов.
type Client struct {
	base           string
	requestTimeout func() time.Duration
	mu             sync.Mutex
	transport      *http.Transport
}

// New создаёт клиент. base — например https://code.1c.ai/public/v1
func New(base string, requestTimeout time.Duration) *Client {
	return NewWithTimeout(base, func() time.Duration { return requestTimeout })
}

// NewWithTimeout selects a response-header timeout for each new request.
func NewWithTimeout(base string, timeout func() time.Duration) *Client {
	return &Client{base: strings.TrimRight(base, "/"), requestTimeout: timeout}
}

func (c *Client) currentTransport() *http.Transport {
	c.mu.Lock()
	defer c.mu.Unlock()
	timeout := c.requestTimeout()
	if c.transport != nil {
		if c.transport.ResponseHeaderTimeout == timeout {
			return c.transport
		}
		c.transport.CloseIdleConnections()
	}
	c.transport = &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: timeout,
		ForceAttemptHTTP2:     true,
	}
	return c.transport
}

// Fault — ошибка Напарника в терминах HTTP-статуса и машинного кода.
type Fault struct {
	Status  int
	Code    string
	Message string
}

func (f *Fault) Error() string { return f.Message }

// AsFault достаёт Fault из цепочки ошибок.
func AsFault(err error) (*Fault, bool) {
	type faulted interface{ Unwrap() error }
	for err != nil {
		if f, ok := err.(*Fault); ok {
			return f, true
		}
		u, ok := err.(faulted)
		if !ok {
			return nil, false
		}
		err = u.Unwrap()
	}
	return nil, false
}

// Budget — состояние лимитов ключа.
type Budget struct {
	Limits struct {
		Sliding5h Window `json:"sliding_5h"`
		Weekly    Window `json:"weekly"`
	} `json:"limits"`
}

// Window — окно лимита. API отдаёт только процент и время сброса.
type Window struct {
	UsedPercent float64    `json:"used_percent"`
	ResetsAt    *time.Time `json:"resets_at"`
}

// ContextInfo — заполненность контекста диалога.
type ContextInfo struct {
	TokenCount       int `json:"token_count"`
	MaxContextLength int `json:"max_context_length"`
}

// Tool — клиентский инструмент в формате Напарника.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// ToolResult — результат выполнения инструмента клиентом.
type ToolResult struct {
	ToolCallID        string `json:"tool_call_id"`
	Status            string `json:"status"`
	Content           any    `json:"content"`
	StructuredContent any    `json:"structured_content,omitempty"`
	Details           any    `json:"details,omitempty"`
}

// MessageRequest — тело POST /conversations/{uuid}/messages.
type MessageRequest struct {
	ParentUUID *string `json:"parent_uuid"`
	Role       string  `json:"role"`
	Content    any     `json:"content"`
	Tools      []Tool  `json:"tools,omitempty"`
	NoGenerate bool    `json:"no_generate,omitempty"`
}

// ToolCall — вызов инструмента из ответа ассистента.
type ToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// NewToolCall собирает вызов инструмента.
func NewToolCall(id, name, arguments string) ToolCall {
	var c ToolCall
	c.ID = id
	c.Type = "function"
	c.Function.Name = name
	c.Function.Arguments = arguments
	return c
}

// AssistantContent — содержимое сообщения ассистента.
type AssistantContent struct {
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content"`
	ToolCalls        []ToolCall `json:"tool_calls"`
}

// Usage — расход токенов.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CachedTokens     int `json:"cached_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens"`
}

// PacketDetails — служебные детали пакета.
type PacketDetails struct {
	FinishReason     *string `json:"finish_reason"`
	MaxContextLength *int    `json:"max_context_length"`
}

// Packet — пакет SSE-потока Напарника.
type Packet struct {
	UUID         string            `json:"uuid"`
	ParentUUID   *string           `json:"parent_uuid"`
	Role         string            `json:"role"`
	Type         string            `json:"type"`
	Content      *AssistantContent `json:"content"`
	ContentDelta *AssistantContent `json:"content_delta"`
	Finished     bool              `json:"finished"`
	Usage        *Usage            `json:"usage"`
	Details      *PacketDetails    `json:"details"`
	Error        json.RawMessage   `json:"error"`
	ErrorType    string            `json:"error_type"`
}

// Stream — открытый поток сообщений.
type Stream struct {
	Packets <-chan Packet
	ch      chan Packet
	done    chan struct{}
	cancel  context.CancelFunc
	err     error
}

// Wait закрывает поток и возвращает ошибку, если она была.
func (s *Stream) Wait() error {
	s.cancel()
	<-s.done
	return s.err
}

// Close прерывает поток.
func (s *Stream) Close() { s.cancel() }

func (c *Client) request(ctx context.Context, key, method, path string, body any, accept string) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "naparnik-admin/0.1")
	client := http.Client{Transport: c.currentTransport()}
	return client.Do(req)
}

// faultFromBody разбирает тело ошибки Напарника.
func faultFromBody(status int, data []byte) *Fault {
	var raw struct {
		Error     json.RawMessage `json:"error"`
		ErrorType string          `json:"error_type"`
		Detail    string          `json:"detail"`
	}
	_ = json.Unmarshal(data, &raw)
	code := raw.ErrorType
	if code == "" {
		code = "upstream_error"
	}
	msg := errText(raw.Error)
	if msg == "" {
		msg = raw.Detail
	}
	if msg == "" {
		msg = strings.TrimSpace(string(data))
	}
	if msg == "" {
		msg = fmt.Sprintf("Напарник вернул HTTP %d", status)
	}
	return &Fault{Status: status, Code: code, Message: msg}
}

// errText превращает поле error (строка, массив или объект) в читаемый текст.
func errText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var list []struct {
		Msg  string `json:"msg"`
		Loc  []any  `json:"loc"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &list); err == nil {
		parts := make([]string, 0, len(list))
		for _, item := range list {
			loc := make([]string, 0, len(item.Loc))
			for _, l := range item.Loc {
				loc = append(loc, fmt.Sprint(l))
			}
			parts = append(parts, strings.Join(loc, ".")+": "+item.Msg)
		}
		return strings.Join(parts, "; ")
	}
	return string(raw)
}

// Budget читает текущее состояние лимитов.
func (c *Client) Budget(ctx context.Context, key string) (*Budget, error) {
	resp, err := c.request(ctx, key, http.MethodGet, "/budget", nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, faultFromBody(resp.StatusCode, data)
	}
	var b Budget
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("разбор ответа /budget: %w", err)
	}
	return &b, nil
}

// CreateConversation создаёт диалог и возвращает его uuid.
func (c *Client) CreateConversation(ctx context.Context, key, uiLang, progLang, scriptLang string) (string, error) {
	body := map[string]any{
		"ui_language":          orDefault(uiLang, "ru"),
		"programming_language": orDefault(progLang, "1c"),
		"script_language":      orDefault(scriptLang, "ru"),
		"title":                nil,
	}
	resp, err := c.request(ctx, key, http.MethodPost, "/conversations", body, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", faultFromBody(resp.StatusCode, data)
	}
	var out struct {
		UUID string `json:"uuid"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	if out.UUID == "" {
		return "", &Fault{Status: http.StatusBadGateway, Code: "bad_response", Message: "Напарник не вернул uuid диалога"}
	}
	return out.UUID, nil
}

// StreamMessages отправляет сообщение и открывает SSE-поток.
func (c *Client) StreamMessages(ctx context.Context, key, conversation string, body MessageRequest) (*Stream, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	resp, err := c.request(streamCtx, key, http.MethodPost, "/conversations/"+conversation+"/messages", body, "text/event-stream")
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer cancel()
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, faultFromBody(resp.StatusCode, data)
	}

	ch := make(chan Packet)
	st := &Stream{Packets: ch, ch: ch, done: make(chan struct{}), cancel: cancel}

	go func() {
		defer close(st.done)
		defer close(st.ch)
		defer resp.Body.Close()
		defer cancel()

		finished := false
		for ev := range sse.Read(streamCtx, resp.Body) {
			if ev.Err != nil {
				if err := streamCtx.Err(); err != nil {
					st.err = err
				} else {
					st.err = &Fault{Status: http.StatusBadGateway, Code: "bad_stream", Message: "ошибка чтения потока Напарника: " + ev.Err.Error()}
				}
				return
			}
			if strings.TrimSpace(ev.Data) == "[DONE]" {
				break
			}
			if ev.Data == "" {
				continue
			}
			var p Packet
			if err := json.Unmarshal([]byte(ev.Data), &p); err != nil {
				st.err = &Fault{Status: http.StatusBadGateway, Code: "bad_stream", Message: "не удалось разобрать пакет потока Напарника"}
				return
			}
			if p.ErrorType != "" || len(p.Error) > 0 {
				st.err = packetFault(p)
				return
			}
			if p.Finished && (body.NoGenerate || p.Role == "assistant" && p.Content != nil) {
				finished = true
			} else if !body.NoGenerate && p.UUID != "" && !p.Finished {
				finished = false
			}
			select {
			case st.ch <- p:
			case <-streamCtx.Done():
				st.err = streamCtx.Err()
				return
			}
		}
		if err := streamCtx.Err(); err != nil {
			st.err = err
		} else if !finished {
			st.err = &Fault{Status: http.StatusBadGateway, Code: "bad_stream", Message: "поток Напарника завершился без финального сообщения"}
		}
	}()
	return st, nil
}

func packetFault(p Packet) *Fault {
	status := http.StatusBadGateway
	switch p.ErrorType {
	case "budget_exceeded", "no_active_tariff":
		status = http.StatusPaymentRequired
	case "invalid_token", "no_token", "token_not_found":
		status = http.StatusUnauthorized
	case "rate_limit_exceeded":
		status = http.StatusTooManyRequests
	case "validation":
		status = http.StatusBadRequest
	}
	msg := errText(p.Error)
	if msg == "" {
		msg = "Напарник вернул ошибку в потоке: " + p.ErrorType
	}
	return &Fault{Status: status, Code: orDefault(p.ErrorType, "upstream_error"), Message: msg}
}

// ContextTokens возвращает заполненность контекста диалога.
func (c *Client) ContextTokens(ctx context.Context, key, conversation, branch string) (*ContextInfo, error) {
	path := "/conversations/" + conversation + "/context_tokens"
	if branch != "" {
		path += "?branch=" + branch
	}
	resp, err := c.request(ctx, key, http.MethodGet, path, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, faultFromBody(resp.StatusCode, data)
	}
	var info ContextInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Compact сжимает контекст диалога.
func (c *Client) Compact(ctx context.Context, key, conversation, instruction string) (string, error) {
	body := map[string]any{"instruction": instruction}
	resp, err := c.request(ctx, key, http.MethodPost, "/conversations/"+conversation+"/compact", body, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", faultFromBody(resp.StatusCode, data)
	}
	var out struct {
		Success bool `json:"success"`
		Result  struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	return out.Result.Status, nil
}

// Title генерирует название диалога.
func (c *Client) Title(ctx context.Context, key, conversation string) (string, error) {
	resp, err := c.request(ctx, key, http.MethodPost, "/conversations/"+conversation+"/title", nil, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", faultFromBody(resp.StatusCode, data)
	}
	var out struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	return out.Title, nil
}

// DeleteConversation удаляет диалог.
func (c *Client) DeleteConversation(ctx context.Context, key, conversation string) error {
	resp, err := c.request(ctx, key, http.MethodDelete, "/conversations/"+conversation, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return faultFromBody(resp.StatusCode, data)
	}
	return nil
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
