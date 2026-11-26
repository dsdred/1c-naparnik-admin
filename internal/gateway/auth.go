package gateway

import (
	"context"
	"net/http"
	"strings"

	"naparnikadmin/internal/secrets"
	"naparnikadmin/internal/store"
)

// Auth проверяет токен клиента и достаёт ключ Напарника.
type Auth struct {
	Store  *store.Store
	Cipher *secrets.Cipher
}

// Principal — аутентифицированный клиент.
type Principal struct {
	Token store.Token
	Key   string // расшифрованный ключ Напарника
}

// FromBearer достаёт токен из заголовка Authorization: Bearer.
func FromBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// FromAnthropic достаёт токен из x-api-key или Authorization.
func FromAnthropic(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
		return v
	}
	return FromBearer(r)
}

// FromGemini достаёт токен из x-goog-api-key, ?key= или Authorization.
func FromGemini(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("x-goog-api-key")); v != "" {
		return v
	}
	if v := r.URL.Query().Get("key"); v != "" {
		return v
	}
	return FromBearer(r)
}

// Resolve проверяет токен и возвращает принципала.
func (a *Auth) Resolve(ctx context.Context, plain string) (*Principal, error) {
	plain = strings.TrimSpace(plain)
	if plain == "" {
		return nil, NewError(http.StatusUnauthorized, "no_token", "Не передан токен доступа. Укажите его как API-ключ.")
	}
	tok, err := a.Store.ResolveToken(ctx, secrets.HashToken(plain))
	if err != nil {
		return nil, NewError(http.StatusUnauthorized, "invalid_token", "Токен не найден. Проверьте значение в админке.")
	}
	if !tok.Enabled || tok.RevokedAt != nil {
		return nil, NewError(http.StatusUnauthorized, "invalid_token", "Токен отозван.")
	}
	enc, err := a.Store.KeySecret(ctx, tok.KeyID)
	if err != nil {
		return nil, NewError(http.StatusUnauthorized, "invalid_token", "К токену не привязан ключ Напарника.")
	}
	key, err := a.Cipher.Decrypt(enc)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "decrypt_error", err.Error())
	}
	keyInfo, err := a.Store.GetKey(ctx, tok.KeyID)
	if err != nil {
		return nil, NewError(http.StatusUnauthorized, "invalid_token", "Ключ Напарника удалён.")
	}
	if keyInfo.Disabled {
		return nil, NewError(http.StatusPaymentRequired, "no_active_tariff", "Ключ Напарника отключён в админке.")
	}
	a.Store.TouchToken(ctx, tok.ID)
	return &Principal{Token: tok, Key: key}, nil
}
