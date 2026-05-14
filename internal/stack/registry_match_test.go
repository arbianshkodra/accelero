package stack

import (
	"testing"

	"github.com/arbianshkodra/accelero/internal/store"
	"github.com/stretchr/testify/assert"
)

func TestRegistryFromImage(t *testing.T) {
	cases := []struct {
		image string
		want  string
	}{
		// Hub default — no slash at all, or first segment is a Hub namespace.
		{"nginx:1.27", "docker.io"},
		{"alpine", "docker.io"},
		{"library/nginx", "docker.io"},
		{"foo/bar:latest", "docker.io"},

		// First segment carries a `.` or `:` or is exactly `localhost`.
		{"ghcr.io/foo/bar", "ghcr.io"},
		{"ghcr.io/foo/bar:tag", "ghcr.io"},
		{"registry.example.com/foo", "registry.example.com"},
		{"registry.example.com:5000/foo", "registry.example.com:5000"},
		{"localhost:5000/foo/bar", "localhost:5000"},

		// Hub aliases stay verbatim — canonical match is the matcher's job.
		{"index.docker.io/library/nginx", "index.docker.io"},
	}
	for _, tc := range cases {
		t.Run(tc.image, func(t *testing.T) {
			assert.Equal(t, tc.want, registryFromImage(tc.image))
		})
	}
}

func TestPickRegistryAuth_ExplicitMatch(t *testing.T) {
	creds := []*store.StackRegistry{
		{Server: "ghcr.io", Username: "ghuser", Password: "ghpw"},
		{Server: "docker.io", Username: "dh", Password: "dhpw"},
	}
	u, p, s := pickRegistryAuth("ghcr.io/foo/bar:tag", creds, nil)
	assert.Equal(t, "ghuser", u)
	assert.Equal(t, "ghpw", p)
	assert.Equal(t, "ghcr.io", s)
}

func TestPickRegistryAuth_NoMatchReturnsAnonymousWithImageRegistry(t *testing.T) {
	// An unmatched image still gets the daemon-side ServerAddress so
	// the pull request goes to the right registry; we just don't
	// authenticate.
	creds := []*store.StackRegistry{
		{Server: "ghcr.io", Username: "u", Password: "p"},
	}
	u, p, s := pickRegistryAuth("registry.gitlab.com/foo:tag", creds, nil)
	assert.Empty(t, u)
	assert.Empty(t, p)
	assert.Equal(t, "registry.gitlab.com", s)
}

func TestPickRegistryAuth_LegacyFallback(t *testing.T) {
	// No matching cred in the list, but the legacy single-credential
	// field on the stack does match — operator hasn't migrated yet,
	// pulls should still work.
	legacy := &store.StackRegistry{Server: "ghcr.io", Username: "old", Password: "olderpw"}
	u, p, _ := pickRegistryAuth("ghcr.io/x:y", nil, legacy)
	assert.Equal(t, "old", u)
	assert.Equal(t, "olderpw", p)
}

func TestPickRegistryAuth_ExplicitWinsOverLegacy(t *testing.T) {
	// Operator wrote a per-stack credential AND left the old field
	// populated. The new API entry must take precedence.
	creds := []*store.StackRegistry{
		{Server: "ghcr.io", Username: "new", Password: "newpw"},
	}
	legacy := &store.StackRegistry{Server: "ghcr.io", Username: "old", Password: "oldpw"}
	u, _, _ := pickRegistryAuth("ghcr.io/x:y", creds, legacy)
	assert.Equal(t, "new", u)
}

func TestPickRegistryAuth_HubAliases(t *testing.T) {
	// Credential saved under "docker.io" must auth pulls of
	// "index.docker.io/library/nginx" — operators shouldn't have to
	// know which Hub alias their `docker login` produced.
	creds := []*store.StackRegistry{
		{Server: "docker.io", Username: "hubuser", Password: "hubpw"},
	}

	for _, ref := range []string{
		"nginx:1.27",
		"library/nginx",
		"index.docker.io/library/nginx",
	} {
		t.Run(ref, func(t *testing.T) {
			u, _, _ := pickRegistryAuth(ref, creds, nil)
			assert.Equal(t, "hubuser", u, "Hub-default image should pick the Hub credential")
		})
	}
}

func TestPickRegistryAuth_LegacyEmptyUsernameTreatedAsAbsent(t *testing.T) {
	// Existing behaviour: stack has a DockerRegistry value but no
	// username (config artifact). Don't try to auth — public pull.
	legacy := &store.StackRegistry{Server: "ghcr.io", Username: "", Password: ""}
	u, _, _ := pickRegistryAuth("ghcr.io/x:y", nil, legacy)
	assert.Empty(t, u)
}
