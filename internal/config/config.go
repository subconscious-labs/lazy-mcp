package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// ProxyConfig holds proxy-level tunables.
type ProxyConfig struct {
	ToolPrefix       string        `yaml:"tool_prefix"`
	MaxSearchResults int           `yaml:"max_search_results"`
	Warmup           bool          `yaml:"warmup"`
	IdleTTL          time.Duration `yaml:"idle_ttl"`
	ToolCacheTTL     time.Duration `yaml:"tool_cache_ttl"`
	ExecTimeout      time.Duration `yaml:"exec_timeout"`
	// InitializeTimeout bounds the MCP handshake (initialize request +
	// initialized notification) when starting a backend. Slow-starting
	// backends (cold caches, remote dependencies probed on startup) may
	// need more than the default. Default: 10s.
	InitializeTimeout time.Duration `yaml:"initialize_timeout"`
	// MaxResultBytes is a hard safety net. If result exceeds this AND page_size is
	// also larger, the response is truncated with truncated=true + hint.
	// Set to 0 to disable. Default: 256 KiB.
	MaxResultBytes int `yaml:"max_result_bytes"`
	// DefaultPageSize is used when meta_call is invoked without explicit page_size.
	// Set to 0 to disable pagination (return full result unless MaxResultBytes kicks in).
	// Default: 0 (disabled).
	DefaultPageSize int `yaml:"default_page_size"`
	// CursorTTL is how long paged cursors are kept in memory between calls.
	// Default: 5 minutes.
	CursorTTL time.Duration `yaml:"cursor_ttl"`
	// ShutdownTimeout is the grace period for in-flight requests on SIGINT/SIGTERM.
	// Default: 5 seconds.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// CircuitBreaker isolates misbehaving backends.
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`
}

// CircuitBreakerConfig controls per-backend failure isolation.
type CircuitBreakerConfig struct {
	// Enabled defaults to true when omitted. Explicitly set `enabled: false`
	// to disable.
	Enabled          *bool         `yaml:"enabled"`
	FailureThreshold int           `yaml:"failure_threshold"`
	Cooldown         time.Duration `yaml:"cooldown"`
}

// IsEnabled returns true unless explicitly disabled.
func (c CircuitBreakerConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// ServerConfig describes a single stdio MCP backend.
type ServerConfig struct {
	Name           string            `yaml:"name"`
	Transport      string            `yaml:"transport"` // "stdio"
	Command        string            `yaml:"command"`
	Args           []string          `yaml:"args"`
	Description    string            `yaml:"description"`
	Tags           []string          `yaml:"tags"`
	Env            map[string]string `yaml:"env"`
	Autostart      bool              `yaml:"autostart"`
	Enabled        bool              `yaml:"enabled"`
	DangerousTools []string          `yaml:"dangerous_tools"`
}

// Config is the root config struct.
type Config struct {
	Version int            `yaml:"version"`
	Proxy   ProxyConfig    `yaml:"proxy"`
	Servers []ServerConfig `yaml:"servers"`
}

// defaults applies reasonable defaults.
func (c *Config) defaults() {
	if c.Proxy.ToolPrefix == "" {
		c.Proxy.ToolPrefix = "meta"
	}
	if c.Proxy.MaxSearchResults == 0 {
		c.Proxy.MaxSearchResults = 8
	}
	if c.Proxy.IdleTTL == 0 {
		c.Proxy.IdleTTL = 10 * time.Minute
	}
	if c.Proxy.ToolCacheTTL == 0 {
		c.Proxy.ToolCacheTTL = 30 * time.Minute
	}
	if c.Proxy.ExecTimeout == 0 {
		c.Proxy.ExecTimeout = 60 * time.Second
	}
	if c.Proxy.InitializeTimeout == 0 {
		c.Proxy.InitializeTimeout = 10 * time.Second
	}
	if c.Proxy.MaxResultBytes == 0 {
		c.Proxy.MaxResultBytes = 262144 // 256 KiB
	}
	// DefaultPageSize intentionally defaults to 0 (disabled);
	// user opts in explicitly via config or per-call page_size.
	if c.Proxy.CursorTTL == 0 {
		c.Proxy.CursorTTL = 5 * time.Minute
	}
	if c.Proxy.ShutdownTimeout == 0 {
		c.Proxy.ShutdownTimeout = 5 * time.Second
	}
	if c.Proxy.CircuitBreaker.FailureThreshold <= 0 {
		c.Proxy.CircuitBreaker.FailureThreshold = 5
	}
	if c.Proxy.CircuitBreaker.Cooldown <= 0 {
		c.Proxy.CircuitBreaker.Cooldown = 30 * time.Second
	}
}

// EnabledServers returns only enabled servers.
func (c *Config) EnabledServers() []ServerConfig {
	var out []ServerConfig
	for _, s := range c.Servers {
		if s.Enabled {
			out = append(out, s)
		}
	}
	return out
}

// Server returns server config by name.
func (c *Config) Server(name string) (ServerConfig, bool) {
	for _, s := range c.Servers {
		if s.Name == name {
			return s, true
		}
	}
	return ServerConfig{}, false
}

// Load reads and parses a YAML config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	cfg.defaults()
	return &cfg, nil
}
