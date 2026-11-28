// Package transfer переносит настройки между экземплярами сервиса: ключи
// Напарника, токены, слушатели и настройки — одним файлом под паролем.
package transfer

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"naparnikadmin/internal/config"
	"naparnikadmin/internal/secrets"
	"naparnikadmin/internal/store"
)

// Format — метка файла переноса: по ней отличаем наш файл от чужого.
const Format = "naparnik-admin-transfer"

// Version — версия формата. Растёт при несовместимом изменении структуры.
const Version = 1

// MinPasswordLen — минимальная длина пароля шифрования файла.
const MinPasswordLen = 8

// ErrSaveConfig identifies a filesystem error while applying an import.
var ErrSaveConfig = errors.New("сохранение конфигурации")

// Параметры вывода ключа из пароля. Хранятся в файле, чтобы файл, созданный
// старым бинарником, читался новым.
const (
	kdfTime    = 3
	kdfMemory  = 64 * 1024
	kdfThreads = 4
	kdfKeyLen  = 32
	kdfSaltLen = 16
	fileCipher = "aes-256-gcm"
	nonceLen   = 12
	tagLen     = 16
)

// aad связывает шифртекст с форматом и версией: подмена версии в файле
// сделает его нечитаемым, а не «почти совместимым».
var aad = []byte("naparnik-admin-transfer/1")

// Key — ключ Напарника в файле переноса. Секрет лежит открытым текстом внутри
// зашифрованного контейнера: иначе на другой машине его не восстановить.
type Key struct {
	Name        string `json:"name"`
	Secret      string `json:"secret"`
	Hint        string `json:"hint,omitempty"`
	TariffLabel string `json:"tariff_label,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
}

// Token — токен доступа к шлюзу. SecretAvailable=false означает, что значение
// не сохранено в исходной базе (токен выпущен до появления шифртекста) —
// такой токен перенести нельзя, на новой машине его придётся выпустить заново.
type Token struct {
	KeyName         string     `json:"key_name"`
	Name            string     `json:"name"`
	Secret          string     `json:"secret,omitempty"`
	SecretAvailable bool       `json:"secret_available"`
	Provider        string     `json:"provider,omitempty"`
	StreamMode      string     `json:"stream_mode,omitempty"`
	ServerTools     string     `json:"server_tools,omitempty"`
	Enabled         bool       `json:"enabled"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
}

// Settings — настройки сервиса в том виде, в каком их видит админка.
type Settings struct {
	ModelID            string  `json:"model_id"`
	StreamMode         string  `json:"stream_mode"`
	AutoCompactAt      float64 `json:"auto_compact_at"`
	BudgetPollInterval string  `json:"budget_poll_interval"`
	SessionTTL         string  `json:"session_ttl"`
	RequestTimeout     string  `json:"request_timeout"`
	WarnPercent        float64 `json:"warn_percent"`
	CritPercent        float64 `json:"crit_percent"`
	AlertWebhook       string  `json:"alert_webhook,omitempty"`
	UpstreamBase       string  `json:"upstream_base"`
}

// Bundle — содержимое файла переноса до шифрования.
type Bundle struct {
	Format    string            `json:"format"`
	Version   int               `json:"version"`
	CreatedAt time.Time         `json:"created_at"`
	Keys      []Key             `json:"keys"`
	Tokens    []Token           `json:"tokens"`
	Listeners []config.Listener `json:"listeners"`
	Settings  Settings          `json:"settings"`
}

// Result — что получилось при импорте.
type Result struct {
	Keys       int
	Tokens     int
	Skipped    int // токены без сохранённого значения
	Listeners  int
	From       time.Time
	SourceKeys int
}

// envelope — внешняя оболочка файла: всё, что нужно для расшифровки.
type envelope struct {
	Format  string    `json:"format"`
	Version int       `json:"version"`
	KDF     kdfParams `json:"kdf"`
	Cipher  string    `json:"cipher"`
	Nonce   string    `json:"nonce"`
	Data    string    `json:"data"`
}

type kdfParams struct {
	Name    string `json:"name"`
	Salt    string `json:"salt"`
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory"`
	Threads uint8  `json:"threads"`
	KeyLen  uint32 `json:"key_len"`
}

// SettingsFrom снимает настройки из конфигурации.
func SettingsFrom(cfg *config.Config) Settings {
	return Settings{
		ModelID:            cfg.ModelName(),
		StreamMode:         cfg.StreamMode,
		AutoCompactAt:      cfg.AutoCompactAt,
		BudgetPollInterval: cfg.BudgetPollInterval.D().String(),
		SessionTTL:         cfg.SessionTTL.D().String(),
		RequestTimeout:     cfg.RequestTimeout.D().String(),
		WarnPercent:        cfg.WarnPercent,
		CritPercent:        cfg.CritPercent,
		AlertWebhook:       cfg.AlertWebhook,
		UpstreamBase:       cfg.UpstreamBase,
	}
}

// Apply переносит настройки в конфигурацию с проверкой значений: битый файл
// не должен оставить сервис с неработающими таймаутами.
func (s Settings) Apply(cfg *config.Config) error {
	if s.ModelID != "" {
		id, err := config.NormalizeModelID(s.ModelID)
		if err != nil {
			return err
		}
		cfg.ModelID = id
	}
	if s.StreamMode != "" {
		if s.StreamMode != "deltas" && s.StreamMode != "final" {
			return fmt.Errorf("неизвестный режим потока %q", s.StreamMode)
		}
		cfg.StreamMode = s.StreamMode
	}
	if s.AutoCompactAt > 0 && s.AutoCompactAt <= 1 {
		cfg.AutoCompactAt = s.AutoCompactAt
	}
	for _, f := range []struct {
		name   string
		value  string
		min    time.Duration
		target *config.Duration
	}{
		{"TTL сессии", s.SessionTTL, time.Second, &cfg.SessionTTL},
		{"Таймаут запроса", s.RequestTimeout, time.Second, &cfg.RequestTimeout},
		{"Период опроса бюджета", s.BudgetPollInterval, 30 * time.Second, &cfg.BudgetPollInterval},
	} {
		if f.value == "" {
			continue
		}
		d, err := time.ParseDuration(f.value)
		if err != nil {
			return fmt.Errorf("%s: неверный формат «%s»", f.name, f.value)
		}
		if d < f.min {
			return fmt.Errorf("%s: слишком мало, минимум %s", f.name, f.min)
		}
		*f.target = config.Duration(d)
	}
	if s.WarnPercent > 0 {
		cfg.WarnPercent = s.WarnPercent
	}
	if s.CritPercent > 0 {
		cfg.CritPercent = s.CritPercent
	}
	if cfg.WarnPercent <= 0 || cfg.CritPercent <= cfg.WarnPercent || cfg.CritPercent > 100 {
		return errors.New("пороги бюджета должны удовлетворять 0 < предупреждение < критический порог ≤ 100")
	}
	cfg.AlertWebhook = s.AlertWebhook
	if s.UpstreamBase != "" {
		base := strings.TrimRight(strings.TrimSpace(s.UpstreamBase), "/")
		target, err := url.Parse(base)
		if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Hostname() == "" ||
			target.User != nil || target.ForceQuery || target.RawQuery != "" || target.Fragment != "" {
			return errors.New("адрес API Напарника должен быть HTTP(S) URL без учётных данных, параметров и фрагмента")
		}
		cfg.UpstreamBase = base
	}
	return nil
}

// Export собирает бандл из текущего состояния: ключи и токены расшифровываются
// мастер-ключом этого экземпляра, чтобы на другой машине их можно было
// зашифровать заново.
func Export(ctx context.Context, st *store.Store, cipher *secrets.Cipher, cfg *config.Config) (*Bundle, error) {
	b := &Bundle{
		Format:    Format,
		Version:   Version,
		CreatedAt: time.Now(),
		Settings:  SettingsFrom(cfg),
		Listeners: append([]config.Listener(nil), cfg.Listeners...),
	}

	keys, err := st.ListKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("ключи: %w", err)
	}
	keyName := make(map[int64]string, len(keys))
	for _, k := range keys {
		enc, err := st.KeySecret(ctx, k.ID)
		if err != nil {
			return nil, fmt.Errorf("ключ %q: %w", k.Name, err)
		}
		plain, err := cipher.Decrypt(enc)
		if err != nil {
			return nil, fmt.Errorf("ключ %q: %w", k.Name, err)
		}
		b.Keys = append(b.Keys, Key{
			Name: k.Name, Secret: plain, Hint: k.KeyHint,
			TariffLabel: k.TariffLabel, Disabled: k.Disabled,
		})
		keyName[k.ID] = k.Name
	}

	tokens, err := st.ListTokens(ctx, 0)
	if err != nil {
		return nil, fmt.Errorf("токены: %w", err)
	}
	for _, t := range tokens {
		item := Token{
			KeyName: keyName[t.KeyID], Name: t.Name, Provider: t.Provider,
			StreamMode: t.StreamMode, ServerTools: t.ServerTools, Enabled: t.Enabled, RevokedAt: t.RevokedAt,
		}
		if enc, err := st.TokenSecret(ctx, t.ID); err == nil && enc != "" {
			if plain, err := cipher.Decrypt([]byte(enc)); err == nil {
				item.Secret = plain
				item.SecretAvailable = true
			}
		}
		b.Tokens = append(b.Tokens, item)
	}
	return b, nil
}

// Import заменяет ключи и токены содержимым бандла. Секреты шифруются
// мастер-ключом этого экземпляра, хеши токенов пересчитываются из значения —
// клиентам не нужно менять токены после переезда.
func Import(ctx context.Context, st *store.Store, cipher *secrets.Cipher, b *Bundle) (Result, error) {
	return ImportWithConfig(ctx, st, cipher, b, nil)
}

// ImportWithConfig также сохраняет подготовленную конфигурацию. Если файл
// сохранить не удастся, SQLite откатит замену ключей и токенов.
func ImportWithConfig(ctx context.Context, st *store.Store, cipher *secrets.Cipher, b *Bundle, cfg *config.Config) (Result, error) {
	if b.Format != "" && b.Format != Format {
		return Result{}, fmt.Errorf("это не файл переноса naparnik-admin (формат %q)", b.Format)
	}
	if b.Version > Version {
		return Result{}, fmt.Errorf("файл создан более новой версией (формат %d, поддерживается %d)", b.Version, Version)
	}

	keys := make([]store.TransferKey, 0, len(b.Keys))
	for _, k := range b.Keys {
		if k.Name == "" || k.Secret == "" {
			return Result{}, fmt.Errorf("ключ %q без названия или значения", k.Name)
		}
		enc, err := cipher.Encrypt(k.Secret)
		if err != nil {
			return Result{}, err
		}
		hint := k.Hint
		if hint == "" {
			hint = secrets.KeyHint(k.Secret)
		}
		keys = append(keys, store.TransferKey{
			Name: k.Name, Hint: hint, Enc: enc,
			TariffLabel: k.TariffLabel, Disabled: k.Disabled,
		})
	}

	res := Result{Keys: len(keys), SourceKeys: len(b.Keys), From: b.CreatedAt}
	tokens := make([]store.TransferToken, 0, len(b.Tokens))
	for _, t := range b.Tokens {
		if !t.SecretAvailable || t.Secret == "" {
			// Значение не сохранено в исходной базе: восстановить его нельзя.
			res.Skipped++
			continue
		}
		enc, err := cipher.Encrypt(t.Secret)
		if err != nil {
			return Result{}, err
		}
		prefix := t.Secret
		if len(prefix) > 11 {
			prefix = prefix[:11]
		}
		serverTools := t.ServerTools
		if serverTools == "" {
			serverTools = "auto"
		}
		tokens = append(tokens, store.TransferToken{
			KeyName: t.KeyName, Name: t.Name, Provider: t.Provider, Prefix: prefix,
			Hash: secrets.HashToken(t.Secret), Enc: string(enc),
			StreamMode: t.StreamMode, ServerTools: serverTools, Enabled: t.Enabled, RevokedAt: t.RevokedAt,
		})
	}
	res.Tokens = len(tokens)

	var stage func() (func() error, error)
	if cfg != nil {
		stage = func() (func() error, error) {
			undo, err := cfg.SaveReversible()
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrSaveConfig, err)
			}
			return undo, nil
		}
	}
	if err := st.ReplaceAll(ctx, keys, tokens, stage); err != nil {
		return Result{}, err
	}
	res.Listeners = len(b.Listeners)
	return res, nil
}

// Seal шифрует бандл паролем и возвращает содержимое файла.
func Seal(b *Bundle, password string) ([]byte, error) {
	if len(password) < MinPasswordLen {
		return nil, fmt.Errorf("пароль короче %d символов", MinPasswordLen)
	}
	plain, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, kdfSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	key := argon2.IDKey([]byte(password), salt, kdfTime, kdfMemory, kdfThreads, kdfKeyLen)
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed := aead.Seal(nil, nonce, plain, aad)

	env := envelope{
		Format:  Format,
		Version: Version,
		KDF: kdfParams{
			Name: "argon2id", Salt: base64.StdEncoding.EncodeToString(salt),
			Time: kdfTime, Memory: kdfMemory, Threads: kdfThreads, KeyLen: kdfKeyLen,
		},
		Cipher: fileCipher,
		Nonce:  base64.StdEncoding.EncodeToString(nonce),
		Data:   base64.StdEncoding.EncodeToString(sealed),
	}
	return json.MarshalIndent(env, "", "  ")
}

// Open расшифровывает файл переноса. Неверный пароль и повреждённый файл
// дают одну и ту же ошибку — какая именно часть не сошлась, знать не нужно.
func Open(data []byte, password string) (*Bundle, error) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, errors.New("файл повреждён или это не файл переноса")
	}
	if env.Format != Format {
		return nil, fmt.Errorf("это не файл переноса naparnik-admin (формат %q)", env.Format)
	}
	if env.Version > Version {
		return nil, fmt.Errorf("файл создан более новой версией (формат %d, поддерживается %d)", env.Version, Version)
	}
	// Версия 1 выпускалась только с этим набором параметров. Проверяем их
	// до вычисления ключа, как и размеры полей контейнера.
	if env.Cipher != fileCipher || env.KDF.Name != "argon2id" || env.KDF.Time != kdfTime ||
		env.KDF.Memory != kdfMemory || env.KDF.Threads != kdfThreads || env.KDF.KeyLen != kdfKeyLen {
		return nil, errors.New("файл повреждён: неверные параметры шифрования")
	}
	salt, err := base64.StdEncoding.DecodeString(env.KDF.Salt)
	if err != nil || len(salt) != kdfSaltLen {
		return nil, errors.New("файл повреждён: неверные параметры шифрования")
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil || len(nonce) != nonceLen {
		return nil, errors.New("файл повреждён: нет вектора инициализации")
	}
	sealed, err := base64.StdEncoding.DecodeString(env.Data)
	if err != nil || len(sealed) < tagLen {
		return nil, errors.New("файл повреждён: нет данных")
	}
	key := argon2.IDKey([]byte(password), salt, kdfTime, kdfMemory, kdfThreads, kdfKeyLen)
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, nonce, sealed, aad)
	if err != nil {
		return nil, errors.New("неверный пароль или файл повреждён")
	}
	var b Bundle
	if err := json.Unmarshal(plain, &b); err != nil {
		return nil, errors.New("файл повреждён: не разобрать содержимое")
	}
	return &b, nil
}

// WriteFile выгружает бандл в файл с правами 0600: внутри рабочие секреты.
func WriteFile(path string, b *Bundle, password string) error {
	data, err := Seal(b, password)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// ReadFile читает и расшифровывает файл переноса.
func ReadFile(path, password string) (*Bundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Open(data, password)
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
