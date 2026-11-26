package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "./data" {
		t.Fatalf("data_dir: %q", cfg.DataDir)
	}
	if len(cfg.Listeners) != 2 {
		t.Fatalf("ожидалось 2 слушателя, получено %d", len(cfg.Listeners))
	}
	if cfg.BudgetPollInterval.D() != 5*time.Minute {
		t.Fatalf("интервал опроса: %v", cfg.BudgetPollInterval.D())
	}
	if cfg.StreamMode != "final" {
		t.Fatalf("режим потока: %q", cfg.StreamMode)
	}
}

func TestLoadFileAndNormalize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
data_dir: ` + dir + `
stream_mode: неизвестный
listeners:
  - addr: 0.0.0.0:5000
    kind: gateway
  - addr: 127.0.0.1:5001
    kind: admin
session_ttl: 30m
request_timeout: 2m
warn_percent: 70
crit_percent: 60
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StreamMode != "final" {
		t.Fatalf("неизвестный режим должен сбрасываться в final, получено %q", cfg.StreamMode)
	}
	if cfg.SessionTTL.D() != 30*time.Minute {
		t.Fatalf("ttl: %v", cfg.SessionTTL.D())
	}
	if cfg.CritPercent != 95 {
		t.Fatalf("критический порог должен быть скорректирован, получено %v", cfg.CritPercent)
	}
	if len(cfg.GatewayAddrs()) != 1 || cfg.GatewayAddrs()[0] != "0.0.0.0:5000" {
		t.Fatalf("gateway-адреса: %v", cfg.GatewayAddrs())
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("NAPARNIK_ADMIN_DATA_DIR", "/tmp/nk")
	t.Setenv("NAPARNIK_ADMIN_STREAM_MODE", "deltas")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/tmp/nk" {
		t.Fatalf("data_dir из env: %q", cfg.DataDir)
	}
	if cfg.StreamMode != "deltas" {
		t.Fatalf("stream_mode из env: %q", cfg.StreamMode)
	}
}

func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8080":  true,
		"localhost:8080":  true,
		"[::1]:8080":      true,
		"0.0.0.0:8080":    false,
		"192.168.1.10:80": false,
	}
	for addr, want := range cases {
		if got := IsLoopback(addr); got != want {
			t.Fatalf("%s: ожидалось %v, получено %v", addr, want, got)
		}
	}
}
