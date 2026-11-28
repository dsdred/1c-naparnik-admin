package admin

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"naparnikadmin/internal/config"
	"naparnikadmin/internal/httpx"
	"naparnikadmin/internal/transfer"
)

// transferExport отдаёт файл переноса: ключи, токены, слушатели и настройки.
// Содержимое шифруется паролем — внутри рабочие секреты.
func (s *Server) transferExport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Не удалось разобрать запрос"})
		return
	}
	if len(req.Password) < transfer.MinPasswordLen {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("Пароль короче %d символов: файл содержит ключи и токены", transfer.MinPasswordLen),
		})
		return
	}

	bundle, err := transfer.Export(r.Context(), s.Store, s.Cipher, s.Cfg.Snapshot())
	if err != nil {
		s.fail(w, err)
		return
	}
	data, err := transfer.Seal(bundle, req.Password)
	if err != nil {
		s.fail(w, err)
		return
	}
	// Выгрузка ключей и токенов — событие, о котором стоит оставить след в журнале.
	s.Log.Info("выгружен файл переноса настроек",
		"keys", len(bundle.Keys), "tokens", len(bundle.Tokens), "remote", r.RemoteAddr)

	httpx.JSON(w, http.StatusOK, map[string]any{
		"filename": fmt.Sprintf("naparnik-transfer-%s.json", time.Now().Format("20060102-1504")),
		"data":     base64.StdEncoding.EncodeToString(data),
		"keys":     len(bundle.Keys),
		"tokens":   len(bundle.Tokens),
	})
}

// transferImport заменяет ключи, токены, слушатели и настройки содержимым файла.
// Сначала всё проверяется, потом применяется: битый файл не должен оставить
// сервис без ключей.
func (s *Server) transferImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
		Data     string `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Не удалось разобрать запрос"})
		return
	}
	raw, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil || len(raw) == 0 {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Файл не передан или повреждён"})
		return
	}
	bundle, err := transfer.Open(raw, req.Password)
	if err != nil {
		httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// Проверяем слушатели и настройки до замены ключей: битый файл не должен
	// оставить сервис без ключей и токенов.
	if len(bundle.Listeners) > 0 {
		if err := config.ValidateListeners(bundle.Listeners); err != nil {
			httpx.JSON(w, http.StatusBadRequest, map[string]any{"error": "Слушатели в файле: " + err.Error()})
			return
		}
	}
	var res transfer.Result
	// Слушатели сначала резервируют порты. Замена ключей и сохранение файла
	// выполняются в их callback до публикации новой конфигурации.
	if err := s.Cfg.Update(func(next *config.Config) error {
		if err := bundle.Settings.Apply(next); err != nil {
			return &apiError{Status: http.StatusBadRequest, Message: "Настройки в файле: " + err.Error()}
		}
		commit := func() error {
			var err error
			res, err = transfer.ImportWithConfig(r.Context(), s.Store, s.Cipher, bundle, next)
			if err != nil && !errors.Is(err, transfer.ErrSaveConfig) {
				return &apiError{Status: http.StatusBadRequest, Message: err.Error()}
			}
			return err
		}
		if len(bundle.Listeners) > 0 {
			next.Listeners = bundle.Listeners
			if s.ApplyListeners != nil {
				return s.ApplyListeners(next.Listeners, commit)
			}
		}
		return commit()
	}); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) {
			s.fail(w, err)
		} else {
			s.fail(w, &apiError{Status: http.StatusConflict, Message: "Импорт прерван: " + err.Error()})
		}
		return
	}

	s.Log.Info("импортирован файл переноса настроек",
		"keys", res.Keys, "tokens", res.Tokens, "skipped", res.Skipped, "remote", r.RemoteAddr)

	out := map[string]any{
		"keys": res.Keys, "tokens": res.Tokens, "skipped": res.Skipped,
		"listeners": len(bundle.Listeners),
	}
	if !res.From.IsZero() {
		out["created_at"] = res.From.Format(time.RFC3339)
	}
	if res.Skipped > 0 {
		out["warning"] = "Часть токенов пропущена: их значения не были сохранены в исходной базе. Выпустите такие токены заново."
	}
	httpx.JSON(w, http.StatusOK, out)
}
