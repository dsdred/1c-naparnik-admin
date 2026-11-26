package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"naparnikadmin/internal/config"
	"naparnikadmin/internal/httpx"
	"naparnikadmin/internal/monitor"
	"naparnikadmin/internal/secrets"
	"naparnikadmin/internal/store"
)

// Server — HTTP-слой админки.
type Server struct {
	Store          *store.Store
	Cipher         *secrets.Cipher
	Monitor        *monitor.Poller
	Cfg            *config.Config
	Log            *slog.Logger
	Auth           *Auth
	ApplyListeners func([]config.Listener) error
}

// Handler собирает маршруты админки.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("POST /api/password", s.setPassword)

	mux.HandleFunc("GET /api/overview", s.overview)

	mux.HandleFunc("GET /api/keys", s.listKeys)
	mux.HandleFunc("POST /api/keys", s.createKey)
	mux.HandleFunc("PATCH /api/keys/{id}", s.updateKey)
	mux.HandleFunc("DELETE /api/keys/{id}", s.deleteKey)
	mux.HandleFunc("POST /api/keys/{id}/refresh", s.refreshKey)
	mux.HandleFunc("POST /api/keys/{id}/reveal", s.revealKey)
	mux.HandleFunc("GET /api/keys/{id}/usage", s.keyUsage)

	mux.HandleFunc("GET /api/tokens", s.listTokens)
	mux.HandleFunc("POST /api/tokens", s.createToken)
	mux.HandleFunc("PATCH /api/tokens/{id}", s.updateToken)
	mux.HandleFunc("POST /api/tokens/{id}/revoke", s.revokeToken)
	mux.HandleFunc("POST /api/tokens/{id}/reveal", s.revealToken)

	mux.HandleFunc("GET /api/sessions", s.listSessions)
	mux.HandleFunc("DELETE /api/sessions", s.deleteSession)

	mux.HandleFunc("GET /api/logs", s.logs)

	mux.HandleFunc("GET /api/listeners", s.getListeners)
	mux.HandleFunc("PUT /api/listeners", s.putListeners)

	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings", s.putSettings)

	mux.HandleFunc("GET /api/connect", s.connect)

	return s.middleware(mux)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		public := path == "/api/state" || path == "/api/login" || path == "/api/password"
		if strings.HasPrefix(path, "/api/") && !public {
			if !s.Auth.Check(r) {
				httpx.JSON(w, http.StatusUnauthorized, map[string]any{"error": "Требуется вход в админку"})
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				if r.Header.Get("X-Requested-With") == "" {
					httpx.JSON(w, http.StatusForbidden, map[string]any{"error": "Запрос без заголовка X-Requested-With отклонён"})
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- авторизация ----------

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]any{
		"initialized":   s.Auth.Initialized(r.Context()),
		"authenticated": s.Auth.Check(r),
	})
}

func (s *Server) setPassword(w http.ResponseWriter, r *http.Request) {
	if s.Auth.Initialized(r.Context()) && !s.Auth.Check(r) {
		httpx.JSON(w, http.StatusUnauthorized, map[string]any{"error": "Требуется вход в админку"})
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Не удалось разобрать запрос"})
		return
	}
	if err := s.Auth.SetPassword(r.Context(), req.Password); err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	value, err := s.Auth.Login(r.Context(), req.Password)
	if err != nil {
		httpx.JSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	SetCookie(w, value)
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Не удалось разобрать запрос"})
		return
	}
	value, err := s.Auth.Login(r.Context(), req.Password)
	if err != nil {
		status := http.StatusUnauthorized
		var ae *apiError
		if errors.As(err, &ae) {
			status = ae.Status
		}
		httpx.JSON(w, status, map[string]any{"error": err.Error()})
		return
	}
	SetCookie(w, value)
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	ClearCookie(w)
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- обзор ----------

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ov, err := s.Monitor.Overview(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, ov)
}

// ---------- ключи ----------

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.Store.ListKeys(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	if keys == nil {
		keys = []store.Key{}
	}
	s.normalizeHints(r.Context(), keys)
	httpx.JSON(w, http.StatusOK, keys)
}

// normalizeHints приводит подсказки ключей к текущему формату.
// Раньше подсказкой были последние 4 символа ключа («…Key1»), что ничего
// не говорит о ключе; теперь это начало и хвост. Расхождение исправляем
// на месте, чтобы старые записи не остались с бесполезной подсказкой.
func (s *Server) normalizeHints(ctx context.Context, keys []store.Key) {
	for i := range keys {
		enc, err := s.Store.KeySecret(ctx, keys[i].ID)
		if err != nil {
			continue
		}
		plain, err := s.Cipher.Decrypt(enc)
		if err != nil {
			continue
		}
		hint := secrets.KeyHint(plain)
		if hint == keys[i].KeyHint {
			continue
		}
		if err := s.Store.UpdateKeyHint(ctx, keys[i].ID, hint); err != nil {
			continue
		}
		keys[i].KeyHint = hint
	}
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Key         string `json:"key"`
		TariffLabel string `json:"tariff_label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Не удалось разобрать запрос"})
		return
	}
	req.Key = strings.TrimSpace(req.Key)
	if req.Key == "" {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Ключ Напарника обязателен"})
		return
	}
	if req.Name == "" {
		req.Name = "Ключ " + time.Now().Format("02.01.2006 15:04")
	}
	req.Name = strings.TrimSpace(req.Name)
	if busy, err := s.Store.KeyNameExists(r.Context(), req.Name, 0); err != nil {
		s.fail(w, err)
		return
	} else if busy {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{
			"error": "Ключ с названием «" + req.Name + "» уже есть — задайте другое название, чтобы их не путать",
		})
		return
	}
	enc, err := s.Cipher.Encrypt(req.Key)
	if err != nil {
		s.fail(w, err)
		return
	}
	hint := secrets.KeyHint(req.Key)
	id, err := s.Store.CreateKey(r.Context(), req.Name, hint, enc)
	if err != nil {
		s.fail(w, err)
		return
	}
	if req.TariffLabel != "" {
		_ = s.Store.UpdateKeyMeta(r.Context(), id, req.Name, req.TariffLabel, false)
	}
	// Сразу проверяем ключ, чтобы в списке был статус и бюджет.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		_, _ = s.Monitor.Refresh(ctx, id)
	}()
	key, err := s.Store.GetKey(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, key)
}

func (s *Server) updateKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Неверный id"})
		return
	}
	key, err := s.Store.GetKey(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	var req struct {
		Name        *string `json:"name"`
		TariffLabel *string `json:"tariff_label"`
		Disabled    *bool   `json:"disabled"`
		Key         *string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Не удалось разобрать запрос"})
		return
	}
	if req.Key != nil {
		plain := strings.TrimSpace(*req.Key)
		if plain != "" {
			enc, err := s.Cipher.Encrypt(plain)
			if err != nil {
				s.fail(w, err)
				return
			}
			if err := s.Store.UpdateKeySecret(r.Context(), id, enc, secrets.KeyHint(plain)); err != nil {
				s.fail(w, err)
				return
			}
		}
	}
	name := key.Name
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
		if name != key.Name {
			if busy, err := s.Store.KeyNameExists(r.Context(), name, id); err != nil {
				s.fail(w, err)
				return
			} else if busy {
				httpx.JSON(w, http.StatusBadRequest, map[string]any{
					"error": "Ключ с названием «" + name + "» уже есть — задайте другое название, чтобы их не путать",
				})
				return
			}
		}
	}
	tariff := key.TariffLabel
	if req.TariffLabel != nil {
		tariff = *req.TariffLabel
	}
	disabled := key.Disabled
	if req.Disabled != nil {
		disabled = *req.Disabled
	}
	if err := s.Store.UpdateKeyMeta(r.Context(), id, name, tariff, disabled); err != nil {
		s.fail(w, err)
		return
	}
	updated, _ := s.Store.GetKey(r.Context(), id)
	httpx.JSON(w, http.StatusOK, updated)
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Неверный id"})
		return
	}
	if err := s.Store.DeleteKey(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) refreshKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Неверный id"})
		return
	}
	sample, err := s.Monitor.Refresh(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, sample)
}

func (s *Server) revealKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Неверный id"})
		return
	}
	enc, err := s.Store.KeySecret(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	plain, err := s.Cipher.Decrypt(enc)
	if err != nil {
		s.fail(w, err)
		return
	}
	// Показ секрета — событие, о котором стоит оставить след в журнале.
	s.Log.Info("ключ Напарника показан в админке", "key_id", id, "remote", r.RemoteAddr)
	httpx.JSON(w, http.StatusOK, map[string]any{"key": plain})
}

func (s *Server) keyUsage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Неверный id"})
		return
	}
	since := time.Now().Add(-24 * time.Hour)
	switch r.URL.Query().Get("range") {
	case "7d":
		since = time.Now().Add(-7 * 24 * time.Hour)
	case "30d":
		since = time.Now().Add(-30 * 24 * time.Hour)
	}
	usage, err := s.Monitor.Usage(r.Context(), id, since)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, usage)
}

// ---------- токены ----------

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	var keyID int64
	if v := r.URL.Query().Get("key_id"); v != "" {
		keyID, _ = strconv.ParseInt(v, 10, 64)
	}
	tokens, err := s.Store.ListTokens(r.Context(), keyID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if tokens == nil {
		tokens = []store.Token{}
	}
	httpx.JSON(w, http.StatusOK, tokens)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		KeyID       int64  `json:"key_id"`
		Name        string `json:"name"`
		Provider    string `json:"provider"`
		StreamMode  string `json:"stream_mode"`
		ServerTools string `json:"server_tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Не удалось разобрать запрос"})
		return
	}
	if req.KeyID == 0 {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Не указан ключ Напарника"})
		return
	}
	if _, err := s.Store.GetKey(r.Context(), req.KeyID); err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Ключ Напарника не найден"})
		return
	}
	if req.Name == "" {
		req.Name = "Токен " + time.Now().Format("02.01.2006 15:04")
	}
	if req.ServerTools == "" {
		req.ServerTools = "auto"
	}
	token, err := secrets.NewToken()
	if err != nil {
		s.fail(w, err)
		return
	}
	enc, err := s.Cipher.Encrypt(token.Plain)
	if err != nil {
		s.fail(w, err)
		return
	}
	id, err := s.Store.CreateToken(r.Context(), store.Token{
		KeyID: req.KeyID, Name: req.Name, Provider: req.Provider, Prefix: token.Prefix,
		StreamMode: req.StreamMode, ServerTools: req.ServerTools, Enabled: true,
	}, token.Hash, string(enc))
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"id": id, "token": token.Plain, "prefix": token.Prefix,
		"name": req.Name, "provider": req.Provider, "key_id": req.KeyID,
	})
}

func (s *Server) updateToken(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Неверный id"})
		return
	}
	tokens, err := s.Store.ListTokens(r.Context(), 0)
	if err != nil {
		s.fail(w, err)
		return
	}
	var cur *store.Token
	for i := range tokens {
		if tokens[i].ID == id {
			cur = &tokens[i]
			break
		}
	}
	if cur == nil {
		httpx.JSON(w, http.StatusNotFound, map[string]any{"error": "Токен не найден"})
		return
	}
	var req struct {
		KeyID       *int64  `json:"key_id"`
		Name        *string `json:"name"`
		Provider    *string `json:"provider"`
		StreamMode  *string `json:"stream_mode"`
		ServerTools *string `json:"server_tools"`
		Enabled     *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Не удалось разобрать запрос"})
		return
	}
	if req.KeyID != nil && *req.KeyID != cur.KeyID {
		if _, err := s.Store.GetKey(r.Context(), *req.KeyID); err != nil {
			httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Ключ Напарника не найден"})
			return
		}
		cur.KeyID = *req.KeyID
	}
	if req.Name != nil {
		cur.Name = *req.Name
	}
	if req.Provider != nil {
		cur.Provider = *req.Provider
	}
	if req.StreamMode != nil {
		cur.StreamMode = *req.StreamMode
	}
	if req.ServerTools != nil {
		cur.ServerTools = *req.ServerTools
	}
	if req.Enabled != nil {
		cur.Enabled = *req.Enabled
	}
	if err := s.Store.UpdateToken(r.Context(), *cur); err != nil {
		s.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, cur)
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Неверный id"})
		return
	}
	if err := s.Store.RevokeToken(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// revealToken возвращает полное значение токена, чтобы его можно было передать сотруднику.
func (s *Server) revealToken(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Неверный id"})
		return
	}
	enc, err := s.Store.TokenSecret(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if enc == "" {
		httpx.JSON(w, http.StatusConflict, map[string]any{
			"error": "Значение этого токена не сохранено: он выпущен до появления этой возможности. Выпустите новый токен.",
		})
		return
	}
	plain, err := s.Cipher.Decrypt([]byte(enc))
	if err != nil {
		s.fail(w, err)
		return
	}
	name := "?"
	if tokens, err := s.Store.ListTokens(r.Context(), 0); err == nil {
		for i := range tokens {
			if tokens[i].ID == id {
				name = tokens[i].Name
				break
			}
		}
	}
	// Показ значения токена — событие для журнала: токен уходит сотруднику.
	s.Log.Info("токен показан в админке", "token_id", id, "name", name, "remote", r.RemoteAddr)
	httpx.JSON(w, http.StatusOK, map[string]any{"token": plain})
}

// ---------- сессии ----------

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.Store.ListSessions(r.Context(), 200)
	if err != nil {
		s.fail(w, err)
		return
	}
	if sessions == nil {
		sessions = []store.Session{}
	}
	httpx.JSON(w, http.StatusOK, sessions)
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tokenID, _ := strconv.ParseInt(q.Get("token_id"), 10, 64)
	protocol := q.Get("protocol")
	sessionID := q.Get("session_id")
	if tokenID == 0 || protocol == "" || sessionID == "" {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Нужны token_id, protocol и session_id"})
		return
	}
	if err := s.Store.DeleteSession(r.Context(), tokenID, protocol, sessionID); err != nil {
		s.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- журнал ----------

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.LogFilter{
		Protocol: q.Get("protocol"),
		Limit:    atoiDefault(q.Get("limit"), 50),
		Offset:   atoiDefault(q.Get("offset"), 0),
	}
	f.KeyID, _ = strconv.ParseInt(q.Get("key_id"), 10, 64)
	f.TokenID, _ = strconv.ParseInt(q.Get("token_id"), 10, 64)
	f.Status, _ = strconv.Atoi(q.Get("status"))
	if v := q.Get("hours"); v != "" {
		hours := atoiDefault(v, 0)
		if hours > 0 {
			f.Since = time.Now().Add(-time.Duration(hours) * time.Hour)
		}
	}
	entries, total, err := s.Store.QueryLog(r.Context(), f)
	if err != nil {
		s.fail(w, err)
		return
	}
	if entries == nil {
		entries = []store.LogEntry{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"entries": entries, "total": total})
}

// ---------- слушатели ----------

func (s *Server) getListeners(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, s.Cfg.Listeners)
}

func (s *Server) putListeners(w http.ResponseWriter, r *http.Request) {
	var list []config.Listener
	if err := json.NewDecoder(r.Body).Decode(&list); err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Не удалось разобрать запрос"})
		return
	}
	for _, l := range list {
		if _, _, err := net.SplitHostPort(l.Addr); err != nil {
			httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Неверный адрес: " + l.Addr})
			return
		}
		if l.Kind != config.KindGateway && l.Kind != config.KindAdmin {
			httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Неизвестный тип слушателя: " + l.Kind})
			return
		}
	}
	if s.ApplyListeners != nil {
		if err := s.ApplyListeners(list); err != nil {
			httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	}
	s.Cfg.Listeners = list
	if err := s.Cfg.Save(); err != nil {
		s.Log.Warn("не удалось сохранить конфигурацию", "err", err)
	}
	httpx.JSON(w, http.StatusOK, list)
}

// ---------- настройки ----------

type settingsView struct {
	StreamMode         string  `json:"stream_mode"`
	AutoCompactAt      float64 `json:"auto_compact_at"`
	SessionTTL         string  `json:"session_ttl"`
	RequestTimeout     string  `json:"request_timeout"`
	BudgetPollInterval string  `json:"budget_poll_interval"`
	WarnPercent        float64 `json:"warn_percent"`
	CritPercent        float64 `json:"crit_percent"`
	AlertWebhook       string  `json:"alert_webhook"`
	UpstreamBase       string  `json:"upstream_base"`
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, settingsView{
		StreamMode:         s.Cfg.StreamMode,
		AutoCompactAt:      s.Cfg.AutoCompactAt,
		SessionTTL:         s.Cfg.SessionTTL.D().String(),
		RequestTimeout:     s.Cfg.RequestTimeout.D().String(),
		BudgetPollInterval: s.Cfg.BudgetPollInterval.D().String(),
		WarnPercent:        s.Cfg.WarnPercent,
		CritPercent:        s.Cfg.CritPercent,
		AlertWebhook:       s.Cfg.AlertWebhook,
		UpstreamBase:       s.Cfg.UpstreamBase,
	})
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsView
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Не удалось разобрать запрос"})
		return
	}
	if req.StreamMode == "deltas" || req.StreamMode == "final" {
		s.Cfg.StreamMode = req.StreamMode
	}
	if req.AutoCompactAt > 0 && req.AutoCompactAt <= 1 {
		s.Cfg.AutoCompactAt = req.AutoCompactAt
	}
	if d, err := time.ParseDuration(req.SessionTTL); err == nil {
		s.Cfg.SessionTTL = config.Duration(d)
	}
	if d, err := time.ParseDuration(req.RequestTimeout); err == nil {
		s.Cfg.RequestTimeout = config.Duration(d)
	}
	if d, err := time.ParseDuration(req.BudgetPollInterval); err == nil && d >= 30*time.Second {
		s.Cfg.BudgetPollInterval = config.Duration(d)
	}
	if req.WarnPercent > 0 && req.WarnPercent < 100 {
		s.Cfg.WarnPercent = req.WarnPercent
	}
	if req.CritPercent > s.Cfg.WarnPercent && req.CritPercent <= 100 {
		s.Cfg.CritPercent = req.CritPercent
	}
	s.Cfg.AlertWebhook = req.AlertWebhook
	if err := s.Cfg.Save(); err != nil {
		s.Log.Warn("не удалось сохранить конфигурацию", "err", err)
	}
	s.getSettings(w, r)
}

// ---------- сниппеты подключения ----------

func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	base := "http://127.0.0.1:47831"
	if addrs := s.Cfg.GatewayAddrs(); len(addrs) > 0 {
		addr := addrs[0]
		host, port, err := net.SplitHostPort(addr)
		if err == nil {
			if host == "0.0.0.0" || host == "::" {
				host = "127.0.0.1"
			}
			base = "http://" + net.JoinHostPort(host, port)
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"base_url":      base,
		"openai_url":    base + "/v1",
		"anthropic_url": base,
		"gemini_url":    base,
		"model":         "naparnik",
		"token_hint":    "nr_...",
	})
}

// ---------- утилиты ----------

func (s *Server) fail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, store.ErrNotFound) {
		status = http.StatusNotFound
	}
	s.Log.Warn("ошибка админ-API", "err", err)
	httpx.JSON(w, status, map[string]any{"error": err.Error()})
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}
