// Package config хранит настройки сервиса: слушатели, таймауты, режимы потока.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Kind слушателя.
const (
	KindGateway = "gateway"
	KindAdmin   = "admin"
)

// Listener — один сетевой вход сервиса.
type Listener struct {
	Addr     string `yaml:"addr" json:"addr"`
	Kind     string `yaml:"kind" json:"kind"`
	CertFile string `yaml:"cert_file,omitempty" json:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty" json:"key_file,omitempty"`
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
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// DBPath возвращает путь к файлу базы.
func (c *Config) DBPath() string { return filepath.Join(c.DataDir, "admin.db") }

// GatewayAddrs возвращает адреса gateway-слушателей — их показываем в сниппетах подключения.
func (c *Config) GatewayAddrs() []string {
	var out []string
	for _, l := range c.Listeners {
		if l.Kind == KindGateway {
			out = append(out, l.Addr)
		}
	}
	return out
}

// IsLoopback сообщает, слушает ли адрес только локальную машину.
func IsLoopback(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}
