// Package secrets отвечает за мастер-ключ шифрования, шифрование секретов,
// выпуск и хеширование токенов, а также за пароль администратора.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Cipher шифрует секреты алгоритмом AES-256-GCM.
type Cipher struct {
	aead cipher.AEAD
}

// LoadOrCreateMasterKey читает мастер-ключ из переменной окружения
// NAPARNIK_ADMIN_MASTER_KEY (base64) либо из файла <dataDir>/master.key,
// создавая его при первом запуске.
func LoadOrCreateMasterKey(dataDir string) ([]byte, error) {
	if v := strings.TrimSpace(os.Getenv("NAPARNIK_ADMIN_MASTER_KEY")); v != "" {
		key, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("NAPARNIK_ADMIN_MASTER_KEY: %w", err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("NAPARNIK_ADMIN_MASTER_KEY: нужно 32 байта, получено %d", len(key))
		}
		return key, nil
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "master.key")
	if data, err := os.ReadFile(path); err == nil {
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if err != nil {
			return nil, fmt.Errorf("master.key: %w", err)
		}
		if len(key) != 32 {
			return nil, errors.New("master.key: ожидалось 32 байта")
		}
		return key, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	enc := base64.StdEncoding.EncodeToString(key)
	if err := os.WriteFile(path, []byte(enc+"\n"), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// NewCipher создаёт шифратор по мастер-ключу.
func NewCipher(key []byte) (*Cipher, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt возвращает nonce||ciphertext.
func (c *Cipher) Encrypt(plain string) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, []byte(plain), nil), nil
}

// Decrypt расшифровывает значение, полученное из Encrypt.
func (c *Cipher) Decrypt(data []byte) (string, error) {
	ns := c.aead.NonceSize()
	if len(data) < ns {
		return "", errors.New("шифротекст повреждён")
	}
	plain, err := c.aead.Open(nil, data[:ns], data[ns:], nil)
	if err != nil {
		return "", errors.New("не удалось расшифровать секрет (не тот мастер-ключ?)")
	}
	return string(plain), nil
}

// Token — выпущенный токен доступа к шлюзу.
type Token struct {
	Plain  string // полное значение, показывается пользователю один раз
	Prefix string // префикс для отображения в списке
	Hash   string // sha256 от полного значения
}

// NewToken генерирует токен вида nr_<32 байта base32url>.
func NewToken() (Token, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return Token{}, err
	}
	plain := "nr_" + base64.RawURLEncoding.EncodeToString(buf)
	return Token{Plain: plain, Prefix: plain[:11], Hash: HashToken(plain)}, nil
}

// HashToken считает sha256 токена в hex.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// KeyHint возвращает узнаваемую часть ключа Напарника для интерфейса.
// Значимая часть ключа — начало (5oShdAqK_...), но у ключей одного кабинета
// префикс совпадает, поэтому для длинных ключей добавляем и хвост.
func KeyHint(key string) string {
	key = strings.TrimSpace(key)
	switch {
	case len(key) >= 16:
		return key[:8] + "…" + key[len(key)-4:]
	case len(key) >= 12:
		return key[:8] + "…"
	case len(key) > 4:
		return key[:4] + "…"
	case key == "":
		return "—"
	}
	return strings.Repeat("•", len(key))
}

// ConstantTimeEqual сравнивает строки без утечки по времени.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Параметры argon2id для пароля администратора.
const (
	argonTime    = 2
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16
)

// HashPassword возвращает PHC-строку argon2id.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword сверяет пароль с PHC-строкой.
func VerifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}
