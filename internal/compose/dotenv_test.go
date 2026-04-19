package compose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func parse(t *testing.T, body string) map[string]string {
	t.Helper()
	vars, err := ParseDotEnv(strings.NewReader(body))
	require.NoError(t, err)
	return vars
}

func TestParseDotEnv_Basic(t *testing.T) {
	vars := parse(t, `
# a comment
KEY=value
ANOTHER=two words
_UNDERSCORE=ok
MIXED123=fine
`)
	assert.Equal(t, "value", vars["KEY"])
	assert.Equal(t, "two words", vars["ANOTHER"])
	assert.Equal(t, "ok", vars["_UNDERSCORE"])
	assert.Equal(t, "fine", vars["MIXED123"])
}

func TestParseDotEnv_EmptyAndBlankLines(t *testing.T) {
	vars := parse(t, "\n\n\n# only comments\n\nKEY=v\n\n")
	assert.Len(t, vars, 1)
	assert.Equal(t, "v", vars["KEY"])
}

func TestParseDotEnv_InlineComment(t *testing.T) {
	vars := parse(t, "KEY=value  # trailing comment\n")
	assert.Equal(t, "value", vars["KEY"])
}

func TestParseDotEnv_HashInValueIsLiteralWithoutSpace(t *testing.T) {
	// docker-compose treats a '#' inside an unquoted value as a comment
	// only when preceded by whitespace.  So `color#red` is literal.
	vars := parse(t, "COLOR=red#tag\n")
	assert.Equal(t, "red#tag", vars["COLOR"])
}

func TestParseDotEnv_EmptyValue(t *testing.T) {
	vars := parse(t, "EMPTY=\nANOTHER=value\n")
	got, ok := vars["EMPTY"]
	assert.True(t, ok)
	assert.Equal(t, "", got)
	assert.Equal(t, "value", vars["ANOTHER"])
}

func TestParseDotEnv_DoubleQuoted(t *testing.T) {
	vars := parse(t, `
DBL="hello world"
WITH_ESCAPES="line1\nline2\ttab\\back\"quote"
`)
	assert.Equal(t, "hello world", vars["DBL"])
	assert.Equal(t, "line1\nline2\ttab\\back\"quote", vars["WITH_ESCAPES"])
}

func TestParseDotEnv_SingleQuoted(t *testing.T) {
	vars := parse(t, `RAW='no $escapes and \n stays literal'`+"\n")
	assert.Equal(t, `no $escapes and \n stays literal`, vars["RAW"])
}

func TestParseDotEnv_QuotedWithTrailingComment(t *testing.T) {
	vars := parse(t, `KEY="value"  # explanation`+"\n")
	assert.Equal(t, "value", vars["KEY"])
}

func TestParseDotEnv_ExportPrefix(t *testing.T) {
	vars := parse(t, "export KEY=value\nexport   OTHER=two\n")
	assert.Equal(t, "value", vars["KEY"])
	assert.Equal(t, "two", vars["OTHER"])
}

func TestParseDotEnv_MalformedLine_NoEquals(t *testing.T) {
	_, err := ParseDotEnv(strings.NewReader("just a line\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected KEY=VALUE")
}

func TestParseDotEnv_MalformedLine_InvalidKey(t *testing.T) {
	_, err := ParseDotEnv(strings.NewReader("1STARTS_WITH_DIGIT=x\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid key")
}

func TestParseDotEnv_UnterminatedQuote(t *testing.T) {
	_, err := ParseDotEnv(strings.NewReader(`KEY="not closed` + "\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unterminated")
}

func TestLoadDotEnv_MissingFileReturnsNil(t *testing.T) {
	vars, err := LoadDotEnv(filepath.Join(t.TempDir(), "nope"))
	require.NoError(t, err)
	assert.Nil(t, vars)
}

func TestLoadDotEnv_ReadsFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	require.NoError(t, os.WriteFile(path, []byte("TAG=1.0\nREGISTRY=ghcr.io\n"), 0o644))

	vars, err := LoadDotEnv(path)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"TAG": "1.0", "REGISTRY": "ghcr.io"}, vars)
}

func TestLoadDotEnv_ErrorWrapsPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	require.NoError(t, os.WriteFile(path, []byte("BAD LINE\n"), 0o644))

	_, err := LoadDotEnv(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
	assert.Contains(t, err.Error(), ":1:")
}
