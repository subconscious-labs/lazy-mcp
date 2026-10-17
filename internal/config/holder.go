package config

import "sync/atomic"

// Holder carries the current *Config and allows atomic replacement.
// All components that need config at request time hold a *Holder and
// call Get() rather than caching the Config directly, so SIGHUP can
// swap the configuration without restarting the process.
type Holder struct {
	p atomic.Pointer[Config]
}

// NewHolder wraps cfg.
func NewHolder(cfg *Config) *Holder {
	h := &Holder{}
	h.p.Store(cfg)
	return h
}

// Get returns the current config. Always non-nil once constructed.
func (h *Holder) Get() *Config { return h.p.Load() }

// Swap replaces the current config and returns the previous one.
func (h *Holder) Swap(cfg *Config) *Config { return h.p.Swap(cfg) }
