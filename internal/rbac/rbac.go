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
	RoleNone     Role = "none"     // deny-all base; only per-stack grants can elevate
	RoleViewer   Role = "viewer"   // read-only
	RoleOperator Role = "operator" // deploy, approve, manage stacks/secrets/registries
	RoleAdmin    Role = "admin"    // everything, incl. /admin/* and key management
)

// level ranks roles for the ⊇ comparison. none (and any unknown role) ranks
// below viewer so it never satisfies a requirement — a deny-all base or a
// malformed value fails closed.
func (r Role) level() int {
	switch r {
	case RoleAdmin:
		return 3
	case RoleOperator:
		return 2
	case RoleViewer:
		return 1
	default: // none + unknown
		return 0
	}
}

// Valid reports whether r is an assignable role (none, viewer, operator,
// admin). Note none is valid-but-powerless: it satisfies no requirement.
func (r Role) Valid() bool {
	switch r {
	case RoleNone, RoleViewer, RoleOperator, RoleAdmin:
		return true
	default:
		return false
	}
}

// Satisfies reports whether a caller holding role r meets requirement req.
func (r Role) Satisfies(req Role) bool { return r.level() >= req.level() }

// ParseRole normalises and validates a role string. The empty string and any
// unknown value return ("", false).
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
	Role Role    // base role: governs non-stack endpoints and stacks with no grant
	// StackGrants maps a stack ID to the role this caller has on that stack,
	// overriding the base role there. nil/empty means "base role everywhere".
	StackGrants map[string]Role
}

// EffectiveRole returns the caller's role for a specific stack: the per-stack
// grant if one exists, otherwise the base role. An empty stackID (non-stack or
// collection endpoint) always uses the base role.
func (id Identity) EffectiveRole(stackID string) Role {
	if stackID != "" && id.StackGrants != nil {
		if r, ok := id.StackGrants[stackID]; ok {
			return r
		}
	}
	return id.Role
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

// StackTokenFromPath returns the stack id-or-name segment of a stack-scoped
// path (/api/v1/stacks/{token}[/...]), or "" for the collection endpoint
// (/api/v1/stacks) and any non-stack path. The token still needs resolving to
// a canonical stack ID (it may be a name) before a grant lookup.
func StackTokenFromPath(path string) string {
	const prefix = "/api/v1/stacks/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := path[len(prefix):]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return rest
}
