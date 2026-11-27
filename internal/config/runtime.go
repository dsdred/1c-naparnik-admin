package config

import (
	"slices"
	"sync"
	"sync/atomic"
)

// Source supplies the current configuration to request and background workers.
type Source interface {
	Snapshot() *Config
}

// Snapshot supplies a static configuration. Readers treat it as immutable.
func (c *Config) Snapshot() *Config { return c }

// Runtime holds immutable snapshots and serializes configuration updates.
type Runtime struct {
	mu      sync.Mutex
	current atomic.Pointer[Config]
}

func NewRuntime(initial *Config) *Runtime {
	r := &Runtime{}
	r.current.Store(copyConfig(initial))
	return r
}

func (r *Runtime) Snapshot() *Config { return r.current.Load() }

// Update stages a change and publishes it only when apply succeeds.
func (r *Runtime) Update(apply func(*Config) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := copyConfig(r.Snapshot())
	if err := apply(next); err != nil {
		return err
	}
	r.current.Store(next)
	return nil
}

func copyConfig(cfg *Config) *Config {
	next := *cfg
	next.Listeners = slices.Clone(cfg.Listeners)
	return &next
}
