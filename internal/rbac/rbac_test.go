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
