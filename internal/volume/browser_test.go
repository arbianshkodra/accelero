package volume

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cleanSubPath is the security boundary — test it directly since a bug
// here would let a caller browse anything under the helper container's
// root filesystem. The logic has to reject *any* `..` in the input
// before path.Clean silently collapses it.

func TestCleanSubPath_NormalisesAndAddsLeadingSlash(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "/"},
		{"/", "/"},
		{"config", "/config"},
		{"/config/", "/config"},
		{"/config/app.yml", "/config/app.yml"},
		{"config//nested///file.yml", "/config/nested/file.yml"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := cleanSubPath(c.in)
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestCleanSubPath_RejectsParentRef(t *testing.T) {
	cases := []string{
		"/..",
		"/../etc/passwd",
		"/config/../../../etc",
		"/foo/..",
		"..",            // no leading slash, still has ..
		"/a/b/../c",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			_, err := cleanSubPath(c)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "..")
		})
	}
}

func TestCleanSubPath_AllowsDoubleDotInName(t *testing.T) {
	// A filename containing ".." but not as a path segment is fine.
	cases := []string{
		"/foo..bar",
		"/config/my..conf",
		"/..file",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			_, err := cleanSubPath(c)
			assert.NoError(t, err)
		})
	}
}
