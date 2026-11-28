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
	"log/slog"
	"net/http"
	"slices"
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

// Reasoning собирает рассуждения модели из всех фрагментов ответа.
func (r *Reply) Reasoning() string {
	parts := make([]string, 0, len(r.Parts))
	for _, p := range r.Parts {
		if p.Reasoning != "" {
			parts = append(parts, p.Reasoning)
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

	// OnText вызывается на каждое подтверждённое сообщение ассистента: финальный
	// пакет, а не черновик. Черновики Напарник вправе отфильтровать или переписать.
	OnText func(text string)
	// OnReasoning — то же для рассуждений (reasoning_content).
	OnReasoning func(text string)
	// OnServerTool вызывается, когда Напарник запросил серверный инструмент.
	OnServerTool func(name string)
	// OnQueued вызывается, если запрос встал в очередь за другим запросом той же
	// сессии: обработчик успевает открыть поток и держать соединение пингами.
	OnQueued func()
	// OnStarted вызывается на первом пакете Напарника. К этому моменту запрос уже
	// принят сервером, поэтому поток можно открывать: ошибки ключа и лимита
	// приходят раньше, обычным HTTP-ответом.
	OnStarted func()
}

// Engine — исполнитель запросов.
type Engine struct {
	// Log получает предупреждения о сбоях сохранения служебного состояния.
	// Если не задан, используется стандартный журнал slog.
	Log *slog.Logger

	client *naparnik.Client
	store  *store.Store
	cfg    config.Source

	mu        sync.Mutex
	locks     map[string]*keyLock
	ctxWindow atomic.Int64
}

// keyLock — мьютекс сессии со счётчиком ожидающих. Когда очередь пуста,
// запись удаляется из карты: иначе на каждый новый чат оставался бы замок навсегда.
type keyLock struct {
	slot chan struct{}
	refs int
}

// ContextWindow возвращает последнее известное значение max_context_length.
func (e *Engine) ContextWindow() int { return int(e.ctxWindow.Load()) }

// New создаёт движок.
func New(client *naparnik.Client, st *store.Store, cfg config.Source) *Engine {
	return &Engine{client: client, store: st, cfg: cfg, locks: map[string]*keyLock{}}
}

// lockSession ставит запрос в очередь его сессии и возвращает функцию разблокировки.
// Ожидающие учитываются в refs до того, как встают в очередь, поэтому замок
// не удаляется, пока за ним кто-то стоит.
func (e *Engine) lockSession(ctx context.Context, key string, onWait func()) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	l, ok := e.locks[key]
	if !ok {
		l = &keyLock{slot: make(chan struct{}, 1)}
		e.locks[key] = l
	}
	l.refs++
	e.mu.Unlock()

	releaseRef := func() {
		e.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(e.locks, key)
		}
		e.mu.Unlock()
	}
	select {
	case l.slot <- struct{}{}:
	default:
		if onWait != nil {
			onWait()
		}
		select {
		case l.slot <- struct{}{}:
		case <-ctx.Done():
			releaseRef()
			return nil, ctx.Err()
		}
	}
	unlock := func() {
		<-l.slot
		releaseRef()
	}
	if err := ctx.Err(); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

func sessionLockKey(tokenID int64, protocol, sessionID string) string {
	return fmt.Sprintf("%d:%s:%s", tokenID, protocol, sessionID)
}

// ResetSession ждёт завершения текущей генерации и удаляет её привязку.
func (e *Engine) ResetSession(ctx context.Context, tokenID int64, protocol, sessionID string) error {
	unlock, err := e.lockSession(ctx, sessionLockKey(tokenID, protocol, sessionID), nil)
	if err != nil {
		return err
	}
	defer unlock()
	return e.store.DeleteSession(ctx, tokenID, protocol, sessionID)
}

// Execute прогоняет запрос через Напарника и возвращает собранный ответ.
func (e *Engine) Execute(ctx context.Context, in Input) (*Reply, error) {
	if strings.TrimSpace(in.Key) == "" {
		return nil, &Error{Status: 401, Code: "no_token", Message: "у токена не настроен ключ Напарника"}
	}
	if len(in.Turns) == 0 {
		return nil, &Error{Status: 400, Code: "invalid_request_error", Message: "пустая история сообщений"}
	}

	sessionKey, sticky := resolveSessionKey(in.SessionID, in.Turns)

	unlock, err := e.lockSession(ctx, sessionLockKey(in.Token.ID, in.Protocol, sessionKey), in.OnQueued)
	if err != nil {
		return nil, err
	}
	defer unlock()

	ctx = e.client.PinEndpoint(ctx)
	reply, err := e.executeLocked(ctx, in, sessionKey, sticky, false)
	if err == nil || !conversationGone(err) || (reply != nil && len(reply.Parts) > 0) {
		return reply, err
	}
	// Напарник не нашёл диалог: он удалён на сервере или создан другим ключом.
	// Оставлять такую привязку нельзя — сессия клиента становится вечно «сломанной»,
	// поэтому забываем её и один раз повторяем запрос в новом диалоге с полной историей.
	if deleteErr := e.store.DeleteSession(ctx, in.Token.ID, in.Protocol, sessionKey); deleteErr != nil {
		return reply, err
	}
	return e.executeLocked(ctx, in, sessionKey, sticky, true)
}

// executeLocked выполняет запрос в уже занятой сессии. forceNew создаёт новый
// диалог, даже если привязка есть.
func (e *Engine) executeLocked(ctx context.Context, in Input, sessionKey string, sticky, forceNew bool) (*Reply, error) {
	cfg := e.cfg.Snapshot()
	binding, err := e.store.GetSession(ctx, in.Token.ID, in.Protocol, sessionKey)
	expired := err == nil && cfg.SessionTTL.D() > 0 && timeSince(binding.UpdatedAt) > cfg.SessionTTL.D()
	hasBinding := err == nil && !expired && binding.Conversation != ""
	// Признак восстановления до очистки: если запись в конце не пройдёт,
	// по нему погасим его отдельно.
	oldPending := binding.PendingJSON
	if expired {
		if err := e.store.DeleteSession(ctx, in.Token.ID, in.Protocol, sessionKey); err != nil {
			return nil, err
		}
	}
	var recovered *recoveredTurn
	if hasBinding && !forceNew && binding.PendingJSON != "" {
		recovered, err = e.recoverTurn(ctx, in, binding)
		if errors.Is(err, errPendingDelivered) {
			// Ответ на прерванный запрос уже ушёл клиенту, а признак восстановления
			// остался из-за сбоя записи. Снимаем его и продолжаем обычную обработку,
			// иначе следующая реплика получала бы 409 бесконечно.
			e.clearPending(sessionKey, in, binding.PendingJSON)
			binding.PendingJSON = ""
			err = nil
			recovered = nil
		}
		if err != nil {
			return nil, err
		}
	}

	// Ищем в присланной истории последнюю реплику ассистента и её контрольную точку:
	// продолжаем ту ветку, которую видит клиент, а не «последнюю отправленную».
	// Клиент вправе откатить, отредактировать или перегенерировать реплику.
	anchorFP, anchorIdx := anchorOf(in.Turns)
	var anchor store.Checkpoint
	haveAnchor := false
	if recovered == nil && hasBinding && !forceNew && anchorFP != "" {
		anchor, haveAnchor, _ = e.store.FindCheckpoint(ctx, in.Token.ID, in.Protocol, sessionKey, anchorFP)
		if !haveAnchor {
			turn := in.Turns[anchorIdx]
			if legacy := orderedAnswerDigest(turn.Text, turn.Calls); legacy != anchorFP {
				anchor, haveAnchor, _ = e.store.FindCheckpoint(ctx, in.Token.ID, in.Protocol, sessionKey, legacy)
			}
		}
	}

	newConversation := forceNew
	switch {
	case recovered != nil:
	case newConversation:
	case haveAnchor:
		// Продолжаем ветку из точки.
	case hasBinding && anchorFP == "":
		// Клиент не прислал историю ассистента — продолжаем привязку, как раньше.
	default:
		// История расходится с тем, что мы отправляли (откат, правка, новый чат
		// или старая база без точек): продолжать нельзя, начинаем новый диалог.
		newConversation = true
	}

	fingerprint := fingerprintOf(in.Turns)
	if recovered == nil && !newConversation && !haveAnchor && sticky && binding.Fingerprint != "" && binding.Fingerprint != fingerprint {
		// Sticky-сессия без точек: клиент начал другой чат.
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
	turns := in.Turns
	switch {
	case recovered != nil:
		conversation = binding.Conversation
		turns = recovered.tail
		if len(turns) > 0 {
			p := recovered.packets[len(recovered.packets)-1].UUID
			parent = &p
		}
	case newConversation:
		conversation, err = e.client.CreateConversation(ctx, in.Key, "ru", "1c", "ru")
		if err != nil {
			return nil, err
		}
	case haveAnchor:
		conversation = anchor.Conversation
		if anchor.Parent != "" {
			p := anchor.Parent
			parent = &p
		}
		// Отправляем только хвост после реплики, которую клиент уже видел.
		turns = in.Turns[anchorIdx+1:]
		if len(turns) == 0 {
			turns = in.Turns[len(in.Turns)-1:]
		}
	default:
		conversation = binding.Conversation
		if binding.Parent != "" {
			p := binding.Parent
			parent = &p
		}
		turns = tailAfterLastAssistant(turns)
		if len(turns) == 0 {
			// Клиент повторил уже отправленную историю — не генерируем заново.
			turns = in.Turns[len(in.Turns)-1:]
		}
	}

	if newConversation {
		binding.ToolsJSON = ""
		// В новый диалог нельзя отправить role:tool: там ещё нет реплики ассистента
		// с tool_calls, и Напарник отвечает 422 «tool responses can only be provided
		// for existing tool calls». Поэтому результаты инструментов переносим текстом.
		turns = flattenToolTurns(turns)
	}

	var groups []Turn
	if newConversation {
		// Историю засеваем без генерации, а по последней реплике пользователя
		// генерируем ответ. Без разделения вся история склеивается в один текст,
		// и модель отвечает на «простыню» вместо последнего вопроса.
		seed, last := splitLastUser(turns)
		groups = append(groupTurns(seed), groupTurns(last)...)
	} else {
		groups = groupTurns(turns)
	}
	if recovered != nil && len(recovered.tail) == 0 {
		groups = recovered.remaining
	}

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
	// Разделитель между сообщениями ассистента тот же, что в Reply.Text/Reasoning.
	textStarted, reasonStarted := false, false
	appendPart := func(part Part) {
		reply.Parts = append(reply.Parts, part)
		if part.Reasoning != "" && in.OnReasoning != nil {
			prefix := ""
			if reasonStarted {
				prefix = "\n\n"
			}
			in.OnReasoning(prefix + part.Reasoning)
			reasonStarted = true
		}
		if part.Text != "" && in.OnText != nil {
			prefix := ""
			if textStarted {
				prefix = "\n\n"
			}
			in.OnText(prefix + part.Text)
			textStarted = true
		}
	}
	accepted := false
	start := func() {
		if !accepted {
			accepted = true
			if in.OnStarted != nil {
				in.OnStarted()
			}
		}
	}
	var replay []naparnik.Packet
	if recovered != nil && len(recovered.tail) == 0 {
		replay = recovered.packets
		start()
		for _, part := range recovered.prefix {
			appendPart(part)
		}
	}
	for len(queue) > 0 || len(replay) > 0 {
		var g Turn
		if len(replay) == 0 {
			g = queue[0]
			queue = queue[1:]
		}

		var serverResults []naparnik.ToolResult
		var acks []string
		partStart := len(reply.Parts)
		handle := func(p naparnik.Packet) {
			start()
			if p.Role == g.Role && p.Finished && p.UUID != "" && !slices.Contains(acks, p.UUID) {
				acks = append(acks, p.UUID)
			}
			if p.UUID != "" {
				lastUUID = p.UUID
			}
			if !p.Finished {
				// Черновики (content_delta) клиенту не отдаём: сервер вправе их
				// отфильтровать или переписать, а финальный content приоритетнее.
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
			// Подтверждённое сообщение отдаём сразу: в режиме deltas клиент получает
			// ответ по мере готовности, но никогда — черновик.
			appendPart(part)
		}
		var uuid string
		var sendErr error
		if len(replay) > 0 {
			for _, p := range replay {
				handle(p)
			}
			uuid = replay[len(replay)-1].UUID
			replay = nil
		} else {
			uuid, sendErr = e.send(ctx, in.Key, conversation, parent, g, wireTools, false, handle)
		}
		if sendErr != nil {
			if len(acks) > 0 && interruptedStream(sendErr) {
				cut := interruptedTurn{Request: turnDigest(in.Turns), Count: len(in.Turns), Role: g.Role,
					Acks: acks, Prefix: reply.Parts[:partStart], Remaining: len(queue)}
				for _, result := range g.Results {
					cut.ToolIDs = append(cut.ToolIDs, result.ToolCallID)
				}
				e.rememberInterrupted(ctx, in, sessionKey, conversation, parent, fingerprint, toolMap, cut)
			}
			if len(reply.Parts) > 0 {
				return reply, sendErr
			}
			return nil, sendErr
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
		if cfg.AutoCompactAt > 0 && float64(info.TokenCount)/float64(info.MaxContextLength) >= cfg.AutoCompactAt {
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
	binding.PendingJSON = ""
	log := e.Log
	if log == nil {
		log = slog.Default()
	}
	// Ответ уже готов: сбой служебной записи не должен превращать его в ошибку.
	// Сначала сохраняем привязку. Если база занята, не ждём ту же блокировку
	// ещё раз при сохранении контрольной точки.
	if err := e.store.SaveSession(ctx, binding); err != nil {
		log.Warn("не удалось сохранить сессию; готовый ответ возвращён клиенту",
			"token_id", in.Token.ID, "protocol", in.Protocol, "conversation", conversation, "err", err)
		// Привязка не записалась — значит и признак восстановления не снялся.
		// Гасим его отдельной записью в фоне: ответ уже у клиента, и следующий
		// запрос не должен упираться в recovery_pending после снятия блокировки.
		if recovered != nil && oldPending != "" {
			e.clearPending(sessionKey, in, oldPending)
		}
		return reply, nil
	}

	// Контрольная точка: по отпечатку этого ответа следующий запрос найдёт,
	// в какой ветке он находится, даже если клиент откатит или поправит историю.
	if text, calls := reply.Text(), reply.ToolCalls(); strings.TrimSpace(text) != "" || len(calls) > 0 {
		if err := e.store.SaveCheckpoint(ctx, store.Checkpoint{
			TokenID:      in.Token.ID,
			Protocol:     in.Protocol,
			SessionID:    sessionKey,
			Fingerprint:  answerDigest(text, calls),
			Conversation: conversation,
			Parent:       lastUUID,
		}); err != nil {
			log.Warn("не удалось сохранить контрольную точку; готовый ответ возвращён клиенту",
				"token_id", in.Token.ID, "protocol", in.Protocol, "conversation", conversation, "err", err)
		}
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

// resolveSessionKey выбирает ключ сессии. Если клиент не прислал заголовок сессии
// (Cline, Continue, Roo), ключ строится по отпечатку первого сообщения: у каждого
// чата одного токена свои замок и диалог, иначе параллельные чаты стоят в одной
// очереди и по очереди пересоздают диалог друг друга.
func resolveSessionKey(sessionID string, turns []Turn) (key string, sticky bool) {
	if key = strings.TrimSpace(sessionID); key != "" {
		return key, false
	}
	if fp := fingerprintOf(turns); fp != "" {
		return "sticky:" + fp, true
	}
	return "sticky", true
}

// splitLastUser отделяет последнюю реплику пользователя от истории. История до неё
// засевается без генерации, а по последней реплике генерируется ответ.
func splitLastUser(turns []Turn) (seed, last []Turn) {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == RoleUser && strings.TrimSpace(turns[i].Text) != "" {
			return turns[:i], turns[i:]
		}
	}
	return nil, turns
}

// flattenToolTurns превращает результаты инструментов в текст пользователя.
// В новый диалог их нельзя отправить как role:tool — там ещё нет реплики
// ассистента с tool_calls, и Напарник отклоняет запрос с 422 validation
// («tool responses can only be provided for existing tool calls»).
func flattenToolTurns(turns []Turn) []Turn {
	out := make([]Turn, 0, len(turns))
	for _, t := range turns {
		if t.Role != RoleTool {
			out = append(out, t)
			continue
		}
		parts := make([]string, 0, len(t.Results))
		for _, r := range t.Results {
			content := ""
			switch v := r.Content.(type) {
			case nil:
			case string:
				content = v
			default:
				if data, err := json.Marshal(v); err == nil {
					content = string(data)
				}
			}
			parts = append(parts, "Результат инструмента "+r.ToolCallID+":\n"+content)
		}
		if len(parts) == 0 {
			continue
		}
		out = append(out, Turn{Role: RoleUser, Text: strings.Join(parts, "\n\n")})
	}
	return out
}

// answerDigest считает отпечаток ответа ассистента: текст и вызовы инструментов.
// Один и тот же отпечаток получается и из нашего Reply, и из реплики, которую
// клиент прислал обратно, — поэтому по нему находится контрольная точка.
func answerDigest(text string, calls []naparnik.ToolCall) string {
	ordered := slices.Clone(calls)
	slices.SortFunc(ordered, func(a, b naparnik.ToolCall) int { return strings.Compare(a.ID, b.ID) })
	return orderedAnswerDigest(text, ordered)
}

func orderedAnswerDigest(text string, calls []naparnik.ToolCall) string {
	h := sha256.New()
	_, _ = h.Write([]byte(strings.TrimSpace(text)))
	for _, c := range calls {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(c.ID))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(c.Function.Name))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(normalizeArgs(c.Function.Arguments)))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// normalizeArgs приводит аргументы инструмента к устойчивому виду: клиенты
// возвращают тот же JSON с другим порядком ключей и пробелами.
func normalizeArgs(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	// json.Marshal сортирует ключи map — представление стабильно.
	data, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return string(data)
}

// anchorOf находит в истории клиента последнюю реплику ассистента: по ней
// определяется, какую ветку диалога продолжать.
func anchorOf(turns []Turn) (fingerprint string, index int) {
	for i := len(turns) - 1; i >= 0; i-- {
		t := turns[i]
		if t.Role != RoleAssistant {
			continue
		}
		if strings.TrimSpace(t.Text) == "" && len(t.Calls) == 0 {
			continue
		}
		return answerDigest(t.Text, t.Calls), i
	}
	return "", -1
}

// conversationGone сообщает, что Напарник не нашёл диалог или сообщение:
// сохранённая привязка недействительна, нужен новый диалог.
func conversationGone(err error) bool {
	var fe *naparnik.Fault
	if errors.As(err, &fe) {
		return fe.Status == http.StatusNotFound || fe.Code == "resource_not_found"
	}
	return false
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
