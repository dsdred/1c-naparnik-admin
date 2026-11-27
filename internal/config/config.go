// Package config хранит настройки сервиса: слушатели, таймауты, режимы потока.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Kind слушателя.
const (
	KindGateway = "gateway"
	KindAdmin   = "admin"
)

// DefaultModelID — публичный id модели, который шлюз показывает клиентам,
// если в конфиге не задан свой (model_id).
const DefaultModelID = "naparnik"

// Listener — один сетевой вход сервиса.
type Listener struct {
	Addr     string `yaml:"addr" json:"addr"`
	Kind     string `yaml:"kind" json:"kind"`
	CertFile string `yaml:"cert_file,omitempty" json:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty" json:"key_file,omitempty"`
	// Disabled выключает слушатель: запись остаётся в конфиге, порт не открывается.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled"`
}

// Duration — time.Duration, который читается из YAML как "5m", "2h".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("неверная длительность %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// D возвращает стандартный time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Config — полная конфигурация сервиса.
type Config struct {
	DataDir            string     `yaml:"data_dir" json:"data_dir"`
	UpstreamBase       string     `yaml:"upstream_base" json:"upstream_base"`
	ModelID            string     `yaml:"model_id" json:"model_id"`
	Listeners          []Listener `yaml:"listeners" json:"listeners"`
	StreamMode         string     `yaml:"stream_mode" json:"stream_mode"`
	AutoCompactAt      float64    `yaml:"auto_compact_at" json:"auto_compact_at"`
	BudgetPollInterval Duration   `yaml:"budget_poll_interval" json:"budget_poll_interval"`
	SessionTTL         Duration   `yaml:"session_ttl" json:"session_ttl"`
	RequestTimeout     Duration   `yaml:"request_timeout" json:"request_timeout"`
	AlertWebhook       string     `yaml:"alert_webhook,omitempty" json:"alert_webhook,omitempty"`
	WarnPercent        float64    `yaml:"warn_percent" json:"warn_percent"`
	CritPercent        float64    `yaml:"crit_percent" json:"crit_percent"`

	path string `yaml:"-"`
}

// Default возвращает конфигурацию по умолчанию.
func Default() *Config {
	return &Config{
		DataDir:            "./data",
		UpstreamBase:       "https://code.1c.ai/public/v1",
		ModelID:            DefaultModelID,
		StreamMode:         "final",
		AutoCompactAt:      0.85,
		BudgetPollInterval: Duration(5 * time.Minute),
		SessionTTL:         Duration(2 * time.Hour),
		RequestTimeout:     Duration(10 * time.Minute),
		WarnPercent:        80,
		CritPercent:        95,
		Listeners: []Listener{
			{Addr: "127.0.0.1:47831", Kind: KindGateway},
			{Addr: "127.0.0.1:47830", Kind: KindAdmin},
		},
	}
}

// Load читает конфиг из файла; отсутствие файла — не ошибка, берутся значения по умолчанию.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("разбор %s: %w", path, err)
			}
		case !os.IsNotExist(err):
			return nil, err
		}
	}
	cfg.path = path
	cfg.applyEnv()
	cfg.normalize()
	return cfg, nil
}

func (c *Config) applyEnv() {
	if v := os.Getenv("NAPARNIK_ADMIN_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("NAPARNIK_ADMIN_UPSTREAM"); v != "" {
		c.UpstreamBase = v
	}
	if v := os.Getenv("NAPARNIK_ADMIN_MODEL_ID"); v != "" {
		c.ModelID = v
	}
	if v := os.Getenv("NAPARNIK_ADMIN_STREAM_MODE"); v != "" {
		c.StreamMode = v
	}
}

func (c *Config) normalize() {
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if c.UpstreamBase == "" {
		c.UpstreamBase = "https://code.1c.ai/public/v1"
	}
	c.UpstreamBase = strings.TrimRight(c.UpstreamBase, "/")
	if strings.TrimSpace(c.ModelID) == "" {
		c.ModelID = DefaultModelID
	}
	c.ModelID = strings.TrimSpace(c.ModelID)
	if c.StreamMode != "deltas" {
		c.StreamMode = "final"
	}
	if c.BudgetPollInterval <= 0 {
		c.BudgetPollInterval = Duration(5 * time.Minute)
	}
	if c.SessionTTL <= 0 {
		c.SessionTTL = Duration(2 * time.Hour)
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = Duration(10 * time.Minute)
	}
	if c.WarnPercent <= 0 || c.WarnPercent > 100 {
		c.WarnPercent = 80
	}
	if c.CritPercent <= c.WarnPercent || c.CritPercent > 100 {
		c.CritPercent = 95
	}
	for i := range c.Listeners {
		if c.Listeners[i].Kind == "" {
			c.Listeners[i].Kind = KindGateway
		}
	}
	if len(c.Listeners) == 0 {
		c.Listeners = Default().Listeners
	}
}

// Save записывает конфиг обратно в файл (для правок из админки).
func (c *Config) Save() error {
	if c.path == "" {
		return fmt.Errorf("конфигурация не привязана к файлу")
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil && filepath.Dir(c.path) != "." {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(c.path), "."+filepath.Base(c.path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	for attempt := 0; ; attempt++ {
		err := os.Rename(f.Name(), c.path)
		if err == nil || runtime.GOOS != "windows" || !os.IsPermission(err) || attempt == 5 {
			return err
		}
		// Windows services such as antivirus scanners can briefly block replacement.
		time.Sleep(20 * time.Millisecond)
	}
}

// DBPath возвращает путь к файлу базы.
func (c *Config) DBPath() string { return filepath.Join(c.DataDir, "admin.db") }

// ModelName возвращает публичный id модели: настроенный или «naparnik» по умолчанию.
// Модель не уходит в API Напарника — это только имя, которое видят IDE.
func (c *Config) ModelName() string {
	if c != nil {
		if v := strings.TrimSpace(c.ModelID); v != "" {
			return v
		}
	}
	return DefaultModelID
}

// GatewayAddrs возвращает адреса gateway-слушателей — их показываем в сниппетах подключения.
func (c *Config) GatewayAddrs() []string {
	var out []string
	for _, l := range c.Listeners {
		if l.Kind == KindGateway && !l.Disabled {
			out = append(out, l.Addr)
		}
	}
	return out
}

// GatewayListener возвращает первый включённый gateway-слушатель: по нему
// собираются сниппеты подключения, в том числе схема http/https.
func (c *Config) GatewayListener() (Listener, bool) {
	for _, l := range c.Listeners {
		if l.Kind == KindGateway && !l.Disabled {
			return l, true
		}
	}
	return Listener{}, false
}

// TLS сообщает, работает ли слушатель по HTTPS.
func (l Listener) TLS() bool { return l.CertFile != "" && l.KeyFile != "" }

// IsLoopback сообщает, слушает ли адрес только локальную машину.
func IsLoopback(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// ValidateListeners проверяет структуру набора слушателей: адреса, типы, дубли
// включённых адресов и то, что после применения останется доступ к админке.
// Наличие файлов сертификатов проверяет вызывающий: при переносе на другую
// машину пути могут быть ещё не созданы.
func ValidateListeners(list []Listener) error {
	enabled, enabledAdmin := 0, 0
	seen := map[string]bool{}
	for _, l := range list {
		if _, _, err := net.SplitHostPort(l.Addr); err != nil {
			return fmt.Errorf("Неверный адрес: %s", l.Addr)
		}
		if l.Kind != KindGateway && l.Kind != KindAdmin {
			return fmt.Errorf("Неизвестный тип слушателя: %s", l.Kind)
		}
		if l.Disabled {
			// Выключенная запись порт не занимает, поэтому дубль адреса с ней допустим.
			continue
		}
		if seen[l.Addr] {
			return fmt.Errorf("Адрес %s уже занят другим включённым слушателем", l.Addr)
		}
		seen[l.Addr] = true
		enabled++
		if l.Kind == KindAdmin {
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

// NormalizeModelID проверяет публичный id модели: без пробелов и служебных
// символов, чтобы его можно было безопасно подставить в сниппеты подключения
// и JSON.
func NormalizeModelID(raw string) (string, error) {
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
