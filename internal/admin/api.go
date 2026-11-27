package admin

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
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
	Cfg            *config.Runtime
	Log            *slog.Logger
	Auth           *Auth
	ApplyListeners func([]config.Listener, func() error) error
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
	mux.HandleFunc("POST /api/tokens/{id}/rotate", s.rotateToken)

	mux.HandleFunc("GET /api/sessions", s.listSessions)
	mux.HandleFunc("DELETE /api/sessions", s.deleteSession)

	mux.HandleFunc("GET /api/logs", s.logs)
	mux.HandleFunc("GET /api/logs/export", s.exportLogs)
	mux.HandleFunc("POST /api/logs/purge", s.purgeLogs)

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
	SetCookie(w, r, value)
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
	SetCookie(w, r, value)
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	ClearCookie(w, r)
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

// rotateToken выпускает новое значение для существующего токена.
// Настройки (ключ, провайдер, поток) сохраняются, старое значение перестаёт работать.
func (s *Server) rotateToken(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Неверный id"})
		return
	}
	name := "?"
	if tokens, err := s.Store.ListTokens(r.Context(), 0); err == nil {
		found := false
		for i := range tokens {
			if tokens[i].ID == id {
				name = tokens[i].Name
				found = true
				break
			}
		}
		if !found {
			httpx.JSON(w, http.StatusNotFound, map[string]any{"error": "Токен не найден"})
			return
		}
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
	if err := s.Store.RotateToken(r.Context(), id, token.Hash, token.Prefix, string(enc)); err != nil {
		s.fail(w, err)
		return
	}
	s.Log.Info("токен перевыпущен", "token_id", id, "name", name, "remote", r.RemoteAddr)
	httpx.JSON(w, http.StatusOK, map[string]any{
		"id": id, "token": token.Plain, "prefix": token.Prefix,
	})
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

// purgeLogs удаляет старые записи журнала запросов. days=0 — удалить всё.
func (s *Server) purgeLogs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Days int `json:"days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Не удалось разобрать запрос"})
		return
	}
	if req.Days < 0 {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Срок хранения не может быть отрицательным"})
		return
	}
	var before time.Time
	if req.Days > 0 {
		before = time.Now().AddDate(0, 0, -req.Days)
	}
	deleted, err := s.Store.PurgeLog(r.Context(), before)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.Log.Info("журнал запросов очищен", "deleted", deleted, "days", req.Days)
	httpx.JSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

// exportLogs отдаёт журнал файлом CSV (разделитель «;», UTF-8 с BOM — открывается в Excel).
func (s *Server) exportLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.LogFilter{Protocol: q.Get("protocol"), All: true}
	f.KeyID, _ = strconv.ParseInt(q.Get("key_id"), 10, 64)
	f.TokenID, _ = strconv.ParseInt(q.Get("token_id"), 10, 64)
	f.Status, _ = strconv.Atoi(q.Get("status"))
	if v := q.Get("hours"); v != "" {
		if hours := atoiDefault(v, 0); hours > 0 {
			f.Since = time.Now().Add(-time.Duration(hours) * time.Hour)
		}
	}
	entries, _, err := s.Store.QueryLog(r.Context(), f)
	if err != nil {
		s.fail(w, err)
		return
	}
	tokens, err := s.Store.ListTokens(r.Context(), 0)
	if err != nil {
		s.fail(w, err)
		return
	}
	tokenName := make(map[int64]string, len(tokens))
	for _, t := range tokens {
		tokenName[t.ID] = t.Name
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="naparnik-journal-%s.csv"`, time.Now().Format("20060102-1504")))
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF}) // BOM: Excel иначе не распознаёт UTF-8

	cw := csv.NewWriter(w)
	cw.Comma = ';'
	_ = cw.Write([]string{"Время", "Токен", "Протокол", "Модель", "Статус", "Ошибка",
		"Токены запроса", "Токены ответа", "Из кэша", "Задержка, мс", "Поток", "Запрос"})
	for _, e := range entries {
		name := tokenName[e.TokenID]
		if name == "" {
			name = "#" + strconv.FormatInt(e.TokenID, 10)
		}
		stream := "нет"
		if e.Stream {
			stream = "да"
		}
		_ = cw.Write([]string{
			e.TS.Format("2006-01-02 15:04:05"), name, e.Protocol, e.Model,
			strconv.Itoa(e.Status), e.ErrorType,
			strconv.Itoa(e.PromptTokens), strconv.Itoa(e.CompTokens), strconv.Itoa(e.CachedTokens),
			strconv.Itoa(e.LatencyMS), stream, e.Prompt,
		})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		s.Log.Warn("не удалось выгрузить журнал", "err", err)
	}
}

// ---------- слушатели ----------

func (s *Server) getListeners(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, s.Cfg.Snapshot().Listeners)
}

// validateListeners проверяет набор слушателей из админки: адреса, типы, дубли
// и то, что после применения останется доступ к самой админке.
func validateListeners(list []config.Listener) error {
	enabled, enabledAdmin := 0, 0
	seen := map[string]bool{}
	for _, l := range list {
		if _, _, err := net.SplitHostPort(l.Addr); err != nil {
			return fmt.Errorf("Неверный адрес: %s", l.Addr)
		}
		if l.Kind != config.KindGateway && l.Kind != config.KindAdmin {
			return fmt.Errorf("Неизвестный тип слушателя: %s", l.Kind)
		}
		if l.Disabled {
			// Выключенная запись порт не занимает, поэтому дубль адреса с ней допустим.
			continue
		}
		if err := validateTLSFiles(l); err != nil {
			return err
		}
		if seen[l.Addr] {
			return fmt.Errorf("Адрес %s уже занят другим включённым слушателем", l.Addr)
		}
		seen[l.Addr] = true
		enabled++
		if l.Kind == config.KindAdmin {
			enabledAdmin++
		}
	}
	if enabled == 0 {
		return errors.New("Нужен хотя бы один включённый слушатель")
	}
	if enabledAdmin == 0 {
		return errors.New("Нельзя выключить последнюю админку — потеряете доступ к настройкам")
	}
	return nil
}

// validateTLSFiles проверяет пару путей к сертификату включённого слушателя.
// Один путь без другого означал бы молчаливый откат к HTTP, поэтому это ошибка,
// а не предупреждение.
func validateTLSFiles(l config.Listener) error {
	if l.CertFile == "" && l.KeyFile == "" {
		return nil
	}
	if l.CertFile == "" || l.KeyFile == "" {
		return fmt.Errorf("Слушатель %s: нужны оба файла — сертификат и ключ", l.Addr)
	}
	for _, p := range []string{l.CertFile, l.KeyFile} {
		st, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("Слушатель %s: файл не найден — %s", l.Addr, p)
		}
		if st.IsDir() {
			return fmt.Errorf("Слушатель %s: ожидается файл, а не каталог — %s", l.Addr, p)
		}
	}
	return nil
}

func (s *Server) putListeners(w http.ResponseWriter, r *http.Request) {
	var list []config.Listener
	if err := json.NewDecoder(r.Body).Decode(&list); err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Не удалось разобрать запрос"})
		return
	}
	if err := validateListeners(list); err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := s.Cfg.Update(func(next *config.Config) error {
		next.Listeners = list
		if s.ApplyListeners == nil {
			return next.Save()
		}
		var saveErr error
		err := s.ApplyListeners(list, func() error {
			saveErr = next.Save()
			return saveErr
		})
		if err != nil && saveErr == nil {
			return &apiError{Status: http.StatusBadRequest, Message: err.Error()}
		}
		return err
	}); err != nil {
		s.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, list)
}

// ---------- настройки ----------

type settingsView struct {
	ModelID            string  `json:"model_id"`
	StreamMode         string  `json:"stream_mode"`
	AutoCompactAt      float64 `json:"auto_compact_at"`
	SessionTTL         string  `json:"session_ttl"`
	RequestTimeout     string  `json:"request_timeout"`
	BudgetPollInterval string  `json:"budget_poll_interval"`
	WarnPercent        float64 `json:"warn_percent"`
	CritPercent        float64 `json:"crit_percent"`
	AlertWebhook       *string `json:"alert_webhook"`
	UpstreamBase       string  `json:"upstream_base"`
}

// normalizeModelID проверяет публичный id модели: без пробелов и служебных символов,
// чтобы его можно было безопасно подставить в сниппеты подключения и JSON.
func normalizeModelID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", errors.New("ID модели не может быть пустым")
	}
	if len([]rune(id)) > 64 {
		return "", errors.New("ID модели длиннее 64 символов")
	}
	for _, r := range id {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.' || r == ':' || r == '/'
		if !ok {
			return "", errors.New("ID модели может содержать только латиницу, цифры и символы - _ . : /")
		}
	}
	return id, nil
}

// parseDurationSetting разбирает длительность из настроек: «30s», «5m», «2h».
// Пустое значение означает «не менять» и возвращает ok=false.
func parseDurationSetting(name, raw string, min time.Duration) (config.Duration, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, false, fmt.Errorf("%s: неверный формат «%s». Укажите число с единицей: 30s, 5m, 2h", name, raw)
	}
	if d < min {
		return 0, false, fmt.Errorf("%s: слишком мало, минимум %s", name, min)
	}
	return config.Duration(d), true, nil
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	cfg := s.Cfg.Snapshot()
	httpx.JSON(w, http.StatusOK, settingsView{
		ModelID:            cfg.ModelName(),
		StreamMode:         cfg.StreamMode,
		AutoCompactAt:      cfg.AutoCompactAt,
		SessionTTL:         cfg.SessionTTL.D().String(),
		RequestTimeout:     cfg.RequestTimeout.D().String(),
		BudgetPollInterval: cfg.BudgetPollInterval.D().String(),
		WarnPercent:        cfg.WarnPercent,
		CritPercent:        cfg.CritPercent,
		AlertWebhook:       &cfg.AlertWebhook,
		UpstreamBase:       cfg.UpstreamBase,
	})
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsView
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Не удалось разобрать запрос"})
		return
	}
	if err := s.Cfg.Update(func(next *config.Config) error {
		if err := applySettings(next, req); err != nil {
			return &apiError{Status: http.StatusBadRequest, Message: err.Error()}
		}
		return next.Save()
	}); err != nil {
		s.fail(w, err)
		return
	}
	s.getSettings(w, r)
}

func applySettings(next *config.Config, req settingsView) error {
	if req.ModelID != "" {
		id, err := normalizeModelID(req.ModelID)
		if err != nil {
			return err
		}
		next.ModelID = id
	}
	if req.StreamMode != "" {
		if req.StreamMode != "deltas" && req.StreamMode != "final" {
			return fmt.Errorf("неизвестный режим потока %q", req.StreamMode)
		}
		next.StreamMode = req.StreamMode
	}
	if req.AutoCompactAt != 0 {
		if req.AutoCompactAt < 0 || req.AutoCompactAt > 1 {
			return fmt.Errorf("порог авто-компактизации должен быть больше 0 и не больше 1")
		}
		next.AutoCompactAt = req.AutoCompactAt
	}
	for _, field := range []struct {
		name, value string
		min         time.Duration
		target      *config.Duration
	}{
		{"TTL сессии", req.SessionTTL, time.Second, &next.SessionTTL},
		{"Таймаут запроса", req.RequestTimeout, time.Second, &next.RequestTimeout},
		{"Период опроса бюджета", req.BudgetPollInterval, 30 * time.Second, &next.BudgetPollInterval},
	} {
		if v, ok, err := parseDurationSetting(field.name, field.value, field.min); err != nil {
			return err
		} else if ok {
			*field.target = v
		}
	}
	if req.WarnPercent != 0 {
		next.WarnPercent = req.WarnPercent
	}
	if req.CritPercent != 0 {
		next.CritPercent = req.CritPercent
	}
	if (req.WarnPercent != 0 || req.CritPercent != 0) &&
		(next.WarnPercent <= 0 || next.CritPercent <= next.WarnPercent || next.CritPercent > 100) {
		return fmt.Errorf("пороги бюджета должны удовлетворять 0 < предупреждение < критический порог ≤ 100")
	}
	if req.AlertWebhook != nil {
		next.AlertWebhook = *req.AlertWebhook
	}
	return nil
}

// ---------- сниппеты подключения ----------

func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	cfg := s.Cfg.Snapshot()
	base := "http://127.0.0.1:47831"
	if l, ok := cfg.GatewayListener(); ok {
		host, port, err := net.SplitHostPort(l.Addr)
		if err == nil {
			if host == "0.0.0.0" || host == "::" {
				host = "127.0.0.1"
			}
			// Схема берётся у слушателя: на HTTPS-порту сниппет с http:// не заработает.
			scheme := "http"
			if l.TLS() {
				scheme = "https"
			}
			base = scheme + "://" + net.JoinHostPort(host, port)
		}
	}
	// Имя машины нужно для слушателей на 0.0.0.0: из локальной сети клиент
	// обращается по нему, а не по 127.0.0.1.
	host, _ := os.Hostname()
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	httpx.JSON(w, http.StatusOK, map[string]any{
		"base_url":      base,
		"openai_url":    base + "/v1",
		"anthropic_url": base,
		"gemini_url":    base,
		"model":         cfg.ModelName(),
		"token_hint":    "nr_...",
		"host":          host,
	})
}

// ---------- утилиты ----------

func (s *Server) fail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		status = apiErr.Status
	} else if errors.Is(err, store.ErrNotFound) {
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
