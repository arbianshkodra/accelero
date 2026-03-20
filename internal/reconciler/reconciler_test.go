package reconciler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arbianshkodra/accelero/internal/service"
	"github.com/stretchr/testify/assert"
)

// ---------------------------------------------------------------------------
// imagesMatch
// ---------------------------------------------------------------------------

func TestImagesMatch_ExactMatch(t *testing.T) {
	assert.True(t, imagesMatch("nginx:1.27.0", "nginx:1.27.0"))
}

func TestImagesMatch_ImplicitLatest(t *testing.T) {
	assert.True(t, imagesMatch("nginx", "nginx:latest"))
}

func TestImagesMatch_RegistryPrefix(t *testing.T) {
	assert.True(t, imagesMatch("nginx:latest", "docker.io/library/nginx:latest"))
}

func TestImagesMatch_DockerIOShortPrefix(t *testing.T) {
	assert.True(t, imagesMatch("docker.io/nginx:1.0", "nginx:1.0"))
}

func TestImagesMatch_IndexDockerIO(t *testing.T) {
	assert.True(t, imagesMatch("index.docker.io/nginx:1.0", "nginx:1.0"))
}

func TestImagesMatch_DifferentTags(t *testing.T) {
	assert.False(t, imagesMatch("nginx:1.26", "nginx:1.27"))
}

func TestImagesMatch_DifferentImages(t *testing.T) {
	assert.False(t, imagesMatch("nginx:latest", "caddy:latest"))
}

func TestImagesMatch_DigestMatch(t *testing.T) {
	assert.True(t, imagesMatch("nginx@sha256:abc123", "nginx@sha256:abc123"))
}

func TestImagesMatch_DigestMismatch(t *testing.T) {
	assert.False(t, imagesMatch("nginx@sha256:abc", "nginx@sha256:def"))
}

// ---------------------------------------------------------------------------
// filterServices
// ---------------------------------------------------------------------------

func TestFilterServices_EmptyFilter(t *testing.T) {
	all := map[string]service.ComposeService{
		"web": {Image: "nginx:latest"},
		"api": {Image: "node:20"},
		"db":  {Image: "postgres:16"},
	}

	result := filterServices(all, "")
	assert.Equal(t, all, result)
}

func TestFilterServices_CommaSeparated(t *testing.T) {
	all := map[string]service.ComposeService{
		"web": {Image: "nginx:latest"},
		"api": {Image: "node:20"},
		"db":  {Image: "postgres:16"},
	}

	result := filterServices(all, "web,api")
	assert.Len(t, result, 2)
	assert.Contains(t, result, "web")
	assert.Contains(t, result, "api")
	assert.NotContains(t, result, "db")
}

func TestFilterServices_UnknownServiceIgnored(t *testing.T) {
	all := map[string]service.ComposeService{
		"web": {Image: "nginx:latest"},
	}

	result := filterServices(all, "web,nonexistent")
	assert.Len(t, result, 1)
	assert.Contains(t, result, "web")
}

func TestFilterServices_WhitespaceTrimmed(t *testing.T) {
	all := map[string]service.ComposeService{
		"web": {Image: "nginx:latest"},
		"api": {Image: "node:20"},
	}

	result := filterServices(all, "  web , api  ")
	assert.Len(t, result, 2)
	assert.Contains(t, result, "web")
	assert.Contains(t, result, "api")
}

// ---------------------------------------------------------------------------
// deriveServiceName
// ---------------------------------------------------------------------------

func TestDeriveServiceName_StandardPattern(t *testing.T) {
	name := deriveServiceName([]string{"/web_0_123456"}, "default")
	assert.Equal(t, "web", name)
}

func TestDeriveServiceName_SingleInstance(t *testing.T) {
	name := deriveServiceName([]string{"/myservice_1"}, "default")
	assert.Equal(t, "myservice", name)
}

func TestDeriveServiceName_NoUnderscore(t *testing.T) {
	name := deriveServiceName([]string{"/singlename"}, "default")
	assert.Equal(t, "", name)
}

func TestDeriveServiceName_EmptyNames(t *testing.T) {
	name := deriveServiceName([]string{}, "default")
	assert.Equal(t, "", name)
}

// ---------------------------------------------------------------------------
// shortID
// ---------------------------------------------------------------------------

func TestShortID_LongID(t *testing.T) {
	id := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	assert.Len(t, id, 64)

	result := shortID(id)
	assert.Equal(t, "a1b2c3d4e5f6", result)
	assert.Len(t, result, 12)
}

func TestShortID_ShortID(t *testing.T) {
	id := "a1b2c3"
	result := shortID(id)
	assert.Equal(t, "a1b2c3", result)
}

// ---------------------------------------------------------------------------
// secureTempDir
// ---------------------------------------------------------------------------

func TestSecureTempDir_CreatesDirectory(t *testing.T) {
	dir, err := secureTempDir("test-stack")
	assert.NoError(t, err)
	defer os.RemoveAll(dir)

	info, statErr := os.Stat(dir)
	assert.NoError(t, statErr)
	assert.True(t, info.IsDir())
}

func TestSecureTempDir_UnderOSTempDir(t *testing.T) {
	dir, err := secureTempDir("test-stack")
	assert.NoError(t, err)
	defer os.RemoveAll(dir)

	tmpDir := os.TempDir()
	relPath, relErr := filepath.Rel(tmpDir, dir)
	assert.NoError(t, relErr)
	assert.False(t, strings.HasPrefix(relPath, ".."), "directory should be under os.TempDir()")
}

func TestSecureTempDir_UniquePaths(t *testing.T) {
	dir1, err1 := secureTempDir("test-stack")
	assert.NoError(t, err1)
	defer os.RemoveAll(dir1)

	dir2, err2 := secureTempDir("test-stack")
	assert.NoError(t, err2)
	defer os.RemoveAll(dir2)

	assert.NotEqual(t, dir1, dir2)
}

func TestSecureTempDir_CleanupWithRemoveAll(t *testing.T) {
	dir, err := secureTempDir("test-stack")
	assert.NoError(t, err)

	// Write a file inside to make cleanup non-trivial.
	err = os.WriteFile(filepath.Join(dir, "dummy.txt"), []byte("test"), 0600)
	assert.NoError(t, err)

	err = os.RemoveAll(dir)
	assert.NoError(t, err)

	_, statErr := os.Stat(dir)
	assert.True(t, os.IsNotExist(statErr))
}
