package config

import (
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestLoad_RequiresAPIKey(t *testing.T) {
	cfg, err := Load()
	assert.Nil(t, cfg)
	assert.EqualError(t, err, "API_KEY environment variable must be set")
}

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("API_KEY", "test-key")

	cfg, err := Load()
	assert.NoError(t, err)

	assert.Equal(t, "8000", cfg.ServerPort)
	assert.Equal(t, "test-key", cfg.APIKey)
	assert.Equal(t, "unix:///var/run/docker.sock", cfg.DockerSock)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, "text", cfg.LogFormat)
	assert.Equal(t, "./data/accelero.db", cfg.DatabasePath)
	assert.Equal(t, 1*time.Hour, cfg.StatusCleanupInterval)
	assert.Equal(t, 24*time.Hour, cfg.StatusMaxAge)

	expectedWorkers := clamp(runtime.NumCPU()*2, 2, 50)
	assert.Equal(t, expectedWorkers, cfg.WorkerCount)

	expectedQueue := clamp(expectedWorkers*15, 50, 1000)
	assert.Equal(t, expectedQueue, cfg.QueueSize)
}

func TestLoad_CustomValues(t *testing.T) {
	t.Setenv("API_KEY", "custom-key")
	t.Setenv("SERVER_PORT", "9090")
	t.Setenv("DOCKER_SOCK", "unix:///tmp/docker.sock")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_FORMAT", "json")
	t.Setenv("DATABASE_PATH", "/tmp/test.db")
	t.Setenv("STATUS_CLEANUP_INTERVAL", "30m")
	t.Setenv("STATUS_MAX_AGE", "12h")

	cfg, err := Load()
	assert.NoError(t, err)

	assert.Equal(t, "9090", cfg.ServerPort)
	assert.Equal(t, "custom-key", cfg.APIKey)
	assert.Equal(t, "unix:///tmp/docker.sock", cfg.DockerSock)
	assert.Equal(t, "debug", cfg.LogLevel)
	assert.Equal(t, "json", cfg.LogFormat)
	assert.Equal(t, "/tmp/test.db", cfg.DatabasePath)
	assert.Equal(t, 30*time.Minute, cfg.StatusCleanupInterval)
	assert.Equal(t, 12*time.Hour, cfg.StatusMaxAge)
}

func TestLoad_WorkerCalculation(t *testing.T) {
	t.Setenv("API_KEY", "test-key")
	t.Setenv("WORKER_COUNT", "10")

	cfg, err := Load()
	assert.NoError(t, err)
	assert.Equal(t, 10, cfg.WorkerCount)
}

func TestLoad_QueueCalculation(t *testing.T) {
	t.Setenv("API_KEY", "test-key")
	t.Setenv("QUEUE_SIZE", "200")

	cfg, err := Load()
	assert.NoError(t, err)
	assert.Equal(t, 200, cfg.QueueSize)
}

func TestHasLegacyConfig(t *testing.T) {
	t.Setenv("API_KEY", "test-key")

	t.Run("all legacy vars set", func(t *testing.T) {
		t.Setenv("REPO_URL", "https://example.com/repo.git")
		t.Setenv("REPO_USERNAME", "user")
		t.Setenv("REPO_TOKEN", "token")
		t.Setenv("COMPOSE_PATH", "docker-compose.yml")

		cfg, err := Load()
		assert.NoError(t, err)
		assert.True(t, cfg.HasLegacyConfig())
	})

	t.Run("missing REPO_URL", func(t *testing.T) {
		t.Setenv("REPO_USERNAME", "user")
		t.Setenv("REPO_TOKEN", "token")
		t.Setenv("COMPOSE_PATH", "docker-compose.yml")

		cfg, err := Load()
		assert.NoError(t, err)
		assert.False(t, cfg.HasLegacyConfig())
	})

	t.Run("missing COMPOSE_PATH", func(t *testing.T) {
		t.Setenv("REPO_URL", "https://example.com/repo.git")
		t.Setenv("REPO_USERNAME", "user")
		t.Setenv("REPO_TOKEN", "token")

		cfg, err := Load()
		assert.NoError(t, err)
		assert.False(t, cfg.HasLegacyConfig())
	})

	t.Run("none set", func(t *testing.T) {
		cfg, err := Load()
		assert.NoError(t, err)
		assert.False(t, cfg.HasLegacyConfig())
	})
}
