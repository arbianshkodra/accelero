package compose

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// expandEq is a small helper to keep the modifier tables readable.
func expandEq(t *testing.T, input string, vars map[string]string, want string) {
	t.Helper()
	got, err := Expand(input, vars)
	require.NoErrorf(t, err, "input=%q", input)
	assert.Equalf(t, want, got, "input=%q", input)
}

func TestExpand_PlainSubstitution(t *testing.T) {
	vars := map[string]string{
		"TAG":      "1.27.0",
		"REGISTRY": "ghcr.io/example",
	}

	cases := map[string]string{
		"nginx:${TAG}":          "nginx:1.27.0",
		"nginx:$TAG":            "nginx:1.27.0",
		"${REGISTRY}/app:${TAG}": "ghcr.io/example/app:1.27.0",
		"$REGISTRY/app:$TAG":     "ghcr.io/example/app:1.27.0",
		"no-vars":                "no-vars",
		"${UNSET}":               "",
		"${TAG}-${TAG}":          "1.27.0-1.27.0",
		"prefix-${TAG}-suffix":   "prefix-1.27.0-suffix",
	}

	for input, want := range cases {
		expandEq(t, input, vars, want)
	}
}

func TestExpand_DollarEscape(t *testing.T) {
	got, err := Expand("literal $$ is kept; $$VAR too", map[string]string{"VAR": "ignored"})
	require.NoError(t, err)
	assert.Equal(t, "literal $ is kept; $VAR too", got)
}

func TestExpand_DefaultModifiers(t *testing.T) {
	vars := map[string]string{
		"SET":   "x",
		"EMPTY": "",
		// UNSET is intentionally absent
	}

	// :- — default if unset OR empty
	expandEq(t, "${SET:-fallback}", vars, "x")
	expandEq(t, "${EMPTY:-fallback}", vars, "fallback")
	expandEq(t, "${UNSET:-fallback}", vars, "fallback")

	// - — default only if unset
	expandEq(t, "${SET-fallback}", vars, "x")
	expandEq(t, "${EMPTY-fallback}", vars, "")
	expandEq(t, "${UNSET-fallback}", vars, "fallback")
}

func TestExpand_AlternateModifiers(t *testing.T) {
	vars := map[string]string{"SET": "x", "EMPTY": ""}

	// :+ — replacement when SET AND non-empty
	expandEq(t, "${SET:+replacement}", vars, "replacement")
	expandEq(t, "${EMPTY:+replacement}", vars, "")
	expandEq(t, "${UNSET:+replacement}", vars, "")

	// + — replacement when SET (even empty)
	expandEq(t, "${SET+replacement}", vars, "replacement")
	expandEq(t, "${EMPTY+replacement}", vars, "replacement")
	expandEq(t, "${UNSET+replacement}", vars, "")
}

func TestExpand_RequiredModifiers(t *testing.T) {
	vars := map[string]string{"SET": "x", "EMPTY": ""}

	// :? — error when unset OR empty
	out, err := Expand("${SET:?required}", vars)
	require.NoError(t, err)
	assert.Equal(t, "x", out)

	_, err = Expand("${EMPTY:?must be set}", vars)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "EMPTY")
	assert.Contains(t, err.Error(), "must be set")

	_, err = Expand("${UNSET:?must be set}", vars)
	require.Error(t, err)

	// ? — error only when truly unset
	out, err = Expand("${EMPTY?required}", vars)
	require.NoError(t, err)
	assert.Equal(t, "", out)

	_, err = Expand("${UNSET?required}", vars)
	require.Error(t, err)
}

func TestExpand_RequiredWithEmptyMessage(t *testing.T) {
	_, err := Expand("${UNSET?}", map[string]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UNSET")
}

func TestExpand_DefaultValueCanContainSpecialChars(t *testing.T) {
	// Default values are copied verbatim — they are not re-interpolated.
	got, err := Expand("${UNSET:-some default with : and - and ?}", map[string]string{})
	require.NoError(t, err)
	assert.Equal(t, "some default with : and - and ?", got)
}

func TestExpand_MalformedBrace(t *testing.T) {
	// An empty ${} or a key that doesn't match [A-Za-z_][A-Za-z0-9_]* is invalid.
	_, err := Expand("${}", map[string]string{})
	require.Error(t, err)

	_, err = Expand("${123STARTS_WITH_DIGIT}", map[string]string{})
	require.Error(t, err)

	_, err = Expand("${HAS!BANG}", map[string]string{})
	require.Error(t, err)
}

func TestExpand_UnclosedBraceIsLiteral(t *testing.T) {
	// An unmatched ${ ... doesn't match the tokenizer, so it's left as-is.
	got, err := Expand("${UNCLOSED", map[string]string{})
	require.NoError(t, err)
	assert.Equal(t, "${UNCLOSED", got)
}

func TestExpand_MultilineYAMLExample(t *testing.T) {
	yaml := `services:
  web:
    image: ${REGISTRY:-ghcr.io}/app:${TAG}
    environment:
      - DB_URL=${DB_URL:?DB_URL is required}
      - DEBUG=$DEBUG
`
	vars := map[string]string{
		"TAG":    "1.2.3",
		"DB_URL": "postgres://x/y",
		"DEBUG":  "", // set but empty
		// REGISTRY unset — should fall back to default
	}

	out, err := Expand(yaml, vars)
	require.NoError(t, err)
	assert.True(t, strings.Contains(out, "image: ghcr.io/app:1.2.3"), "got: %s", out)
	assert.True(t, strings.Contains(out, "DB_URL=postgres://x/y"))
	assert.True(t, strings.Contains(out, "DEBUG="))
}

func TestExpand_ReturnsFirstError(t *testing.T) {
	// When multiple required-vars fail, the first one wins.
	_, err := Expand("${A:?first}${B:?second}", map[string]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "A")
	assert.NotContains(t, err.Error(), "second")
}

func TestExpandBytes(t *testing.T) {
	out, err := ExpandBytes([]byte("x=$X"), map[string]string{"X": "y"})
	require.NoError(t, err)
	assert.Equal(t, []byte("x=y"), out)
}
