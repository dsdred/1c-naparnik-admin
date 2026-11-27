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
	current atomic.Pointer[revision]
}

type revision struct {
	cfg     *Config
	changed chan struct{}
}

func NewRuntime(initial *Config) *Runtime {
	r := &Runtime{}
	r.current.Store(&revision{cfg: copyConfig(initial), changed: make(chan struct{})})
	return r
}

func (r *Runtime) Snapshot() *Config { return r.current.Load().cfg }

// Watch returns a snapshot and a signal that closes when it is superseded.
func (r *Runtime) Watch() (*Config, <-chan struct{}) {
	current := r.current.Load()
	return current.cfg, current.changed
}

// Update stages a change and publishes it only when apply succeeds.
func (r *Runtime) Update(apply func(*Config) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.current.Load()
	next := copyConfig(current.cfg)
	if err := apply(next); err != nil {
		return err
	}
	r.current.Store(&revision{cfg: next, changed: make(chan struct{})})
	close(current.changed)
	return nil
}

func copyConfig(cfg *Config) *Config {
	next := *cfg
	next.Listeners = slices.Clone(cfg.Listeners)
	return &next
}
