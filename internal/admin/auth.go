// Package admin — JSON API админки и авторизация.
package admin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"naparnikadmin/internal/secrets"
	"naparnikadmin/internal/store"
)

const (
	cookieName = "nk_session"
	settingKey = "admin_password_hash"
	sessionTTL = 12 * time.Hour
)

// Auth проверяет пароль администратора и сессионную cookie.
type Auth struct {
	store *store.Store
	key   []byte
}

// NewAuth создаёт проверку доступа на основе мастер-ключа.
func NewAuth(st *store.Store, masterKey []byte) *Auth {
	sum := sha256.Sum256(append(append([]byte{}, masterKey...), []byte("admin-session")...))
	return &Auth{store: st, key: sum[:]}
}

// Initialized сообщает, задан ли пароль.
func (a *Auth) Initialized(ctx context.Context) bool {
	_, err := a.store.GetSetting(ctx, settingKey)
	return err == nil
}

// SetPassword задаёт (или меняет) пароль администратора.
func (a *Auth) SetPassword(ctx context.Context, password string) error {
	if len(password) < 6 {
		return errShortPassword
	}
	hash, err := secrets.HashPassword(password)
	if err != nil {
		return err
	}
	return a.store.SetSetting(ctx, settingKey, hash)
}

// Login сверяет пароль и возвращает значение cookie.
func (a *Auth) Login(ctx context.Context, password string) (string, error) {
	hash, err := a.store.GetSetting(ctx, settingKey)
	if err != nil {
		return "", errNoPassword
	}
	if !secrets.VerifyPassword(password, hash) {
		return "", errBadPassword
	}
	return a.issue(), nil
}

func (a *Auth) issue() string {
	exp := time.Now().Add(sessionTTL).Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte(itoa(exp)))
	return payload + "." + a.sign(payload)
}

func (a *Auth) sign(payload string) string {
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// Check проверяет cookie запроса.
func (a *Auth) Check(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	parts := strings.SplitN(c.Value, ".", 2)
	if len(parts) != 2 {
		return false
	}
	if !hmac.Equal([]byte(a.sign(parts[0])), []byte(parts[1])) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	exp, err := parseInt(string(raw))
	if err != nil {
		return false
	}
	return time.Now().Unix() < exp
}

// isHTTPS сообщает, пришёл ли запрос по защищённому соединению. Учитывается
// и обратный прокси: он присылает X-Forwarded-Proto.
func isHTTPS(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// SetCookie выставляет cookie сессии. Secure включается только на HTTPS:
// на HTTP браузер такую cookie просто не сохранит и вход сломается.
func SetCookie(w http.ResponseWriter, r *http.Request, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// ClearCookie удаляет cookie сессии.
func ClearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", HttpOnly: true,
		Secure: isHTTPS(r), SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
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

func parseInt(s string) (int64, error) {
	var v int64
	if s == "" {
		return 0, errBadPassword
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errBadPassword
		}
		v = v*10 + int64(r-'0')
	}
	return v, nil
}

var (
	errShortPassword = &apiError{Status: 400, Message: "Пароль должен быть не короче 6 символов"}
	errNoPassword    = &apiError{Status: 400, Message: "Пароль администратора ещё не задан"}
	errBadPassword   = &apiError{Status: 401, Message: "Неверный пароль"}
)

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return e.Message }
