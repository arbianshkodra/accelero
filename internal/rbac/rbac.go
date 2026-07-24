// Package rbac defines Accelero's role model, the request identity carried on
// context, and the per-request authorization policy. It has no internal
// dependencies so the middleware, handler, store, and audit packages can all
// import it without cycles.
package rbac

import (
	"context"
	"net/http"
	"strings"
)

// Role is a coarse permission level. Roles are hierarchical:
// admin ⊇ operator ⊇ viewer.
type Role string

const (
	RoleViewer   Role = "viewer"   // read-only
	RoleOperator Role = "operator" // deploy, approve, manage stacks/secrets/registries
	RoleAdmin    Role = "admin"    // everything, incl. /admin/* and key management
)

// level ranks roles for the ⊇ comparison. Unknown roles rank below viewer so a
// malformed role never grants access.
func (r Role) level() int {
	switch r {
	case RoleAdmin:
		return 3
	case RoleOperator:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}

// Valid reports whether r is one of the known roles.
func (r Role) Valid() bool { return r.level() > 0 }

// Satisfies reports whether a caller holding role r meets requirement req.
func (r Role) Satisfies(req Role) bool { return r.level() >= req.level() }

// ParseRole normalises and validates a role string. The empty string and any
// unknown value return (", false).
func ParseRole(s string) (Role, bool) {
	role := Role(strings.ToLower(strings.TrimSpace(s)))
	if !role.Valid() {
		return "", false
	}
	return role, true
}

// Identity is the authenticated caller attached to a request context.
type Identity struct {
	Name string // key name (or "env-admin" for the bootstrap env key)
	Role Role
}

type ctxKey struct{}

// WithIdentity returns a context carrying the caller identity.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// IdentityFromContext returns the caller identity and whether one was set.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

// RequiredRole returns the minimum role needed for a request, derived from its
// method and path. The policy is intentionally prefix/method based rather than
// a per-route table so new routes are covered by default (fail-closed for
// mutations):
//
//   - /api/v1/admin/* and /api/v1/apikeys*     → admin
//   - a path ending in /exec (a mutating GET)  → operator
//   - any other GET                            → viewer
//   - any other method (POST/PUT/DELETE/PATCH) → operator
func RequiredRole(method, path string) Role {
	if strings.HasPrefix(path, "/api/v1/admin/") || strings.HasPrefix(path, "/api/v1/apikeys") {
		return RoleAdmin
	}
	// Container exec is a mutation delivered over a WebSocket GET upgrade, so
	// it must not fall through to the viewer branch below.
	if strings.HasSuffix(path, "/exec") {
		return RoleOperator
	}
	if method == http.MethodGet || method == http.MethodHead {
		return RoleViewer
	}
	return RoleOperator
}
