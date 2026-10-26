package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

const reloadCooldown = 5 * time.Second

func (h *Handlers) Reload(ctx context.Context, _ map[string]any) (string, error) {
	user, sess := sessionUser(ctx)
	if sess != nil && sess.Role() != "admin" {
		return "", fmt.Errorf("reload: admin role required (current: %q, user: %q)", sess.Role(), user)
	}
	if h.reloadFn == nil {
		return "", fmt.Errorf("reload: not available (this build has no config file to reload)")
	}

	h.reloadMu.Lock()
	if since := time.Since(h.lastReload); since < reloadCooldown {
		remaining := (reloadCooldown - since).Round(time.Second)
		h.reloadMu.Unlock()
		return "", fmt.Errorf("reload: too soon, wait %s", remaining)
	}
	h.lastReload = time.Now()
	h.reloadMu.Unlock()

	warnings, err := h.reloadFn()
	if err != nil {
		return "", fmt.Errorf("reload: %w", err)
	}

	cfg := h.cfg.Get()
	names := make([]string, 0, len(cfg.EnabledServers()))
	for _, s := range cfg.EnabledServers() {
		names = append(names, s.Name)
	}
	sort.Strings(names)

	out := map[string]any{
		"ok":      true,
		"servers": len(cfg.Servers),
		"enabled": len(cfg.EnabledServers()),
		"names":   names,
	}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}

	h.log.Info("reload: applied via meta_reload",
		"user", user, "servers", len(cfg.Servers), "enabled", len(cfg.EnabledServers()))
	return string(b), nil
}
