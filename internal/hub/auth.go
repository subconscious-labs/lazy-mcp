package hub

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
)

type authUser struct {
	Name           string
	Role           string
	AllowedServers []string
}

type userCtxKey struct{}

func userFromContext(ctx context.Context) *authUser {
	u, _ := ctx.Value(userCtxKey{}).(*authUser)
	return u
}

func withUser(ctx context.Context, u *authUser) context.Context {
	return context.WithValue(ctx, userCtxKey{}, u)
}

func bearerToken(r *http.Request) string {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, p) {
		return ""
	}
	return strings.TrimSpace(h[len(p):])
}

// authMiddleware enforces Bearer authentication. When auth.enabled is
// false, a synthetic admin is injected so downstream logic can rely on
// userFromContext returning non-nil.
func (h *Hub) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ac := h.cfg.Get().Auth
		if !ac.Enabled {
			next.ServeHTTP(w, r.WithContext(withUser(r.Context(),
				&authUser{Name: "local", Role: "admin"})))
			return
		}

		tok := bearerToken(r)
		if tok == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="lazy-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		sum := sha256.Sum256([]byte(tok))
		for _, u := range ac.Users {
			want, err := hex.DecodeString(u.TokenSHA256)
			if err != nil || len(want) != len(sum) {
				continue
			}
			if subtle.ConstantTimeCompare(want, sum[:]) == 1 {
				next.ServeHTTP(w, r.WithContext(withUser(r.Context(),
					&authUser{
						Name:           u.Name,
						Role:           u.Role,
						AllowedServers: u.AllowedServers,
					})))
				return
			}
		}
		h.metricsAuthFailures()
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}
