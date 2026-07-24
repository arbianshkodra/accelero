package rbac

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRoleSatisfies(t *testing.T) {
	assert.True(t, RoleAdmin.Satisfies(RoleViewer))
	assert.True(t, RoleAdmin.Satisfies(RoleOperator))
	assert.True(t, RoleAdmin.Satisfies(RoleAdmin))
	assert.True(t, RoleOperator.Satisfies(RoleViewer))
	assert.False(t, RoleOperator.Satisfies(RoleAdmin))
	assert.False(t, RoleViewer.Satisfies(RoleOperator))
	// An unknown role satisfies nothing.
	assert.False(t, Role("bogus").Satisfies(RoleViewer))
}

func TestParseRole(t *testing.T) {
	for _, in := range []string{"admin", "ADMIN", " Operator ", "viewer"} {
		_, ok := ParseRole(in)
		assert.True(t, ok, in)
	}
	for _, in := range []string{"", "root", "superuser"} {
		_, ok := ParseRole(in)
		assert.False(t, ok, in)
	}
}

func TestRequiredRole(t *testing.T) {
	cases := []struct {
		method, path string
		want         Role
	}{
		{http.MethodGet, "/api/v1/stacks", RoleViewer},
		{http.MethodGet, "/api/v1/stacks/x/containers/c/logs", RoleViewer},
		{http.MethodPost, "/api/v1/stacks", RoleOperator},
		{http.MethodPost, "/api/v1/stacks/x/deploy", RoleOperator},
		{http.MethodDelete, "/api/v1/stacks/x", RoleOperator},
		{http.MethodPost, "/api/v1/stacks/x/deployments/d/approve", RoleOperator},
		{http.MethodGet, "/api/v1/stacks/x/containers/c/exec", RoleOperator}, // mutating GET
		{http.MethodPost, "/api/v1/admin/backup", RoleAdmin},
		{http.MethodPost, "/api/v1/admin/restore", RoleAdmin},
		{http.MethodPost, "/api/v1/apikeys", RoleAdmin},
		{http.MethodGet, "/api/v1/apikeys", RoleAdmin},
		{http.MethodDelete, "/api/v1/apikeys/abc", RoleAdmin},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, RequiredRole(c.method, c.path), "%s %s", c.method, c.path)
	}
}

func TestIdentityContext(t *testing.T) {
	_, ok := IdentityFromContext(context.Background())
	assert.False(t, ok)

	ctx := WithIdentity(context.Background(), Identity{Name: "ops", Role: RoleOperator})
	id, ok := IdentityFromContext(ctx)
	assert.True(t, ok)
	assert.Equal(t, "ops", id.Name)
	assert.Equal(t, RoleOperator, id.Role)
}

func TestRoleNone(t *testing.T) {
	assert.True(t, RoleNone.Valid(), "none is an assignable role")
	assert.False(t, RoleNone.Satisfies(RoleViewer), "none satisfies nothing")
	r, ok := ParseRole("none")
	assert.True(t, ok)
	assert.Equal(t, RoleNone, r)
}

func TestEffectiveRole(t *testing.T) {
	id := Identity{
		Name: "team", Role: RoleNone,
		StackGrants: map[string]Role{"sid-prod": RoleOperator, "sid-stg": RoleViewer},
	}
	assert.Equal(t, RoleOperator, id.EffectiveRole("sid-prod"), "grant overrides base")
	assert.Equal(t, RoleViewer, id.EffectiveRole("sid-stg"))
	assert.Equal(t, RoleNone, id.EffectiveRole("sid-other"), "no grant → base")
	assert.Equal(t, RoleNone, id.EffectiveRole(""), "non-stack → base")

	// No grants at all → base everywhere.
	plain := Identity{Name: "ops", Role: RoleOperator}
	assert.Equal(t, RoleOperator, plain.EffectiveRole("sid-prod"))
}

func TestStackTokenFromPath(t *testing.T) {
	cases := map[string]string{
		"/api/v1/stacks/prod/deploy":               "prod",
		"/api/v1/stacks/prod":                      "prod",
		"/api/v1/stacks/prod/containers/c/logs":    "prod",
		"/api/v1/stacks":                           "",
		"/api/v1/apikeys":                          "",
		"/api/v1/approvals":                        "",
		"/health":                                  "",
	}
	for path, want := range cases {
		assert.Equal(t, want, StackTokenFromPath(path), path)
	}
}
