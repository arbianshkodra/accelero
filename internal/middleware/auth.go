package middleware

import (
	"crypto/subtle"
	"net/http"

	"github.com/arbianshkodra/accelero/internal/logctx"
	"github.com/arbianshkodra/accelero/internal/rbac"
	"github.com/sirupsen/logrus"
)

// KeyLookup resolves a presented raw API key to a caller identity. It returns
// (identity, true, nil) on a match, (_, false, nil) when the key is unknown,
// and a non-nil error only on an infrastructure failure (e.g. DB down).
type KeyLookup func(rawKey string) (rbac.Identity, bool, error)

// StackResolver maps a stack id-or-name (as it appears in a URL) to the
// canonical stack ID used for per-stack grant lookups. Returns ok=false when
// no such stack exists.
type StackResolver func(idOrName string) (stackID string, ok bool)

// NewAPIKeyAuth builds the authentication + authorization middleware.
//
// Authentication: the presented X-API-KEY is matched first (constant-time)
// against envKey — the bootstrap key, which always maps to the admin role — and
// otherwise handed to lookup for a store-backed, role-scoped key.
//
// Authorization: the caller's *effective* role for the request must satisfy
// rbac.RequiredRole for the method and path. The effective role is the caller's
// per-stack grant when the path targets a stack (resolved via resolveStack),
// otherwise the base role. Missing key → 401; unknown key or insufficient role
// → 403. On success the identity is attached to the request context.
//
// resolveStack may be nil (per-stack grants then never apply — base role is
// used everywhere).
func NewAPIKeyAuth(envKey string, lookup KeyLookup, resolveStack StackResolver) func(http.Handler) http.Handler {
	if envKey == "" {
		logrus.Fatal("API_KEY environment variable must be set for security")
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log := logctx.FromContext(r.Context()).WithFields(logrus.Fields{
				"path":        r.URL.Path,
				"method":      r.Method,
				"remote_addr": r.RemoteAddr,
			})

			raw := r.Header.Get("X-API-KEY")
			if raw == "" {
				log.Warn("Unauthorized request: missing API key")
				http.Error(w, "API key is required", http.StatusUnauthorized)
				return
			}

			var id rbac.Identity
			if subtle.ConstantTimeCompare([]byte(raw), []byte(envKey)) == 1 {
				id = rbac.Identity{Name: "env-admin", Role: rbac.RoleAdmin}
			} else {
				found, ok, err := lookup(raw)
				if err != nil {
					log.WithError(err).Error("API key lookup failed")
					http.Error(w, "authentication error", http.StatusInternalServerError)
					return
				}
				if !ok {
					log.Warn("Unauthorized request: invalid API key")
					http.Error(w, "Invalid API key", http.StatusForbidden)
					return
				}
				id = found
			}

			required := rbac.RequiredRole(r.Method, r.URL.Path)

			// Resolve the target stack (if any) so per-stack grants can
			// override the base role for that stack.
			var stackID string
			if token := rbac.StackTokenFromPath(r.URL.Path); token != "" && resolveStack != nil {
				if resolved, ok := resolveStack(token); ok {
					stackID = resolved
				}
			}
			effective := id.EffectiveRole(stackID)

			if !effective.Satisfies(required) {
				log.WithFields(logrus.Fields{
					"actor":          id.Name,
					"base_role":      id.Role,
					"effective_role": effective,
					"stack_id":       stackID,
					"required_role":  required,
				}).Warn("Forbidden: insufficient role")
				http.Error(w, "insufficient role", http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r.WithContext(rbac.WithIdentity(r.Context(), id)))
		})
	}
}
