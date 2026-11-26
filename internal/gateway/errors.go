package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"naparnikadmin/internal/naparnik"
)

// Error — ошибка шлюза в терминах HTTP-кода и машинного кода.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// NewError создаёт ошибку шлюза.
func NewError(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// FromError приводит любую ошибку к Error с осмысленным HTTP-статусом.
func FromError(err error) *Error {
	if err == nil {
		return nil
	}
	var ge *Error
	if errors.As(err, &ge) {
		return ge
	}
	var fe *naparnik.Fault
	if errors.As(err, &fe) {
		status := fe.Status
		switch {
		case status == http.StatusUnprocessableEntity:
			status = http.StatusBadRequest
		case status >= 500 || status == 0:
			status = http.StatusBadGateway
		}
		return &Error{Status: status, Code: fe.Code, Message: humanMessage(fe)}
	}
	if errors.Is(err, context.Canceled) {
		return &Error{Status: 499, Code: "client_closed_request", Message: "запрос отменён клиентом"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Status: http.StatusGatewayTimeout, Code: "timeout", Message: "Напарник не ответил вовремя"}
	}
	return &Error{Status: http.StatusInternalServerError, Code: "internal_error", Message: err.Error()}
}

// humanMessage даёт понятный текст для типовых ошибок Напарника.
func humanMessage(f *naparnik.Fault) string {
	switch f.Code {
	case "budget_exceeded":
		return "Лимит Напарника исчерпан. Подождите сброса 5-часового окна или недельного лимита."
	case "no_active_tariff":
		return "У ключа Напарника нет активного тарифа. Оформите подписку на портале ИТС."
	case "rate_limit_exceeded":
		return "Слишком много запросов к Напарнику. Повторите позже."
	case "invalid_token", "no_token", "token_not_found":
		return "Ключ Напарника недействителен. Проверьте его в админке."
	case "conflict":
		return "Напарник ещё обрабатывает запрос в этом диалоге."
	case "insufficient_context":
		return "Напарнику не хватает контекста для этой операции."
	}
	if strings.TrimSpace(f.Message) == "" {
		return "Ошибка Напарника (" + f.Code + ")"
	}
	return f.Message
}

// Retryable сообщает, стоит ли повторять запрос при такой ошибке.
func Retryable(err error) bool {
	ge := FromError(err)
	return ge.Status == http.StatusBadGateway || ge.Status == http.StatusGatewayTimeout
}

// timeSince — небольшая обёртка, чтобы не тянуть time в engine.go дважды.
func timeSince(t time.Time) time.Duration { return time.Since(t) }
