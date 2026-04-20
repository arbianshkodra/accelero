package config

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"time"
)

// Config holds all server-level configuration.
// Stack-specific settings (git repos, compose paths) live in the store.
type Config struct {
	// Server
	ServerPort string

	// Authentication
	APIKey string

	// Docker
	DockerSock string

	// Worker Pool
	WorkerCount int
	QueueSize   int

	// Status Cleanup
	StatusCleanupInterval time.Duration
	StatusMaxAge          time.Duration
	// AuditMaxAge is how long to keep audit entries before the cleanup
	// loop drops them. Audit is compliance/investigation data — kept
	// considerably longer than deploy history by default (90 days vs 24h).
	AuditMaxAge time.Duration

	// Logging
	LogLevel  string
	LogFormat string

	// Database
	DatabasePath string

	// StacksDataDir is where cloned gitops repos are kept per stack:
	//   <StacksDataDir>/<stack_id>/repo/
	// Unlike the old /tmp-based clone, this dir is NOT deleted after a
	// successful deploy — compose services that bind-mount files from the
	// repo (e.g. `./Caddyfile:/etc/caddy/Caddyfile`) need a stable path.
	// For the same reason, when Accelero itself runs in a container this
	// path must exist at the *same* absolute location on the host and
	// inside the container; otherwise Docker (running on the host) will
	// look up the bind-mount source at the wrong place.
	StacksDataDir string

	// Legacy mode: if these are set, a default stack is auto-created on first run
	LegacyRepoURL      string
	LegacyRepoUsername  string
	LegacyRepoToken     string
	LegacyRepoBranch    string
	LegacyComposePath   string
	LegacyServiceNames  string
	LegacyDockerUsername string
	LegacyDockerPassword string
	LegacyDockerRegistry string
}

// Load reads configuration from environment variables and applies defaults.
func Load() (*Config, error) {
	cfg := &Config{
		ServerPort:            envOrDefault("SERVER_PORT", "8000"),
		APIKey:                os.Getenv("API_KEY"),
		DockerSock:            envOrDefault("DOCKER_SOCK", "unix:///var/run/docker.sock"),
		LogLevel:              envOrDefault("LOG_LEVEL", "info"),
		LogFormat:             envOrDefault("LOG_FORMAT", "text"),
		DatabasePath:          envOrDefault("DATABASE_PATH", "./data/accelero.db"),
		StacksDataDir:         envOrDefault("STACKS_DATA_DIR", "./data/stacks"),
		StatusCleanupInterval: parseDurationOrDefault("STATUS_CLEANUP_INTERVAL", 1*time.Hour),
		StatusMaxAge:          parseDurationOrDefault("STATUS_MAX_AGE", 24*time.Hour),
		AuditMaxAge:           parseDurationOrDefault("AUDIT_MAX_AGE", 90*24*time.Hour),

		// Legacy env vars for backward compatibility
		LegacyRepoURL:       os.Getenv("REPO_URL"),
		LegacyRepoUsername:   os.Getenv("REPO_USERNAME"),
		LegacyRepoToken:      os.Getenv("REPO_TOKEN"),
		LegacyRepoBranch:     os.Getenv("REPO_BRANCH"),
		LegacyComposePath:    os.Getenv("COMPOSE_PATH"),
		LegacyServiceNames:   os.Getenv("SERVICE_NAMES"),
		LegacyDockerUsername: os.Getenv("DOCKER_USERNAME"),
		LegacyDockerPassword: os.Getenv("DOCKER_PASSWORD"),
		LegacyDockerRegistry: os.Getenv("DOCKER_REGISTRY"),
	}

	cfg.WorkerCount = calculateWorkers()
	cfg.QueueSize = calculateQueueSize(cfg.WorkerCount)

	if cfg.APIKey == "" {
		return nil, fmt.Errorf("API_KEY environment variable must be set")
	}

	return cfg, nil
}

// HasLegacyConfig returns true if legacy single-stack env vars are set.
func (c *Config) HasLegacyConfig() bool {
	return c.LegacyRepoURL != "" && c.LegacyRepoUsername != "" &&
		c.LegacyRepoToken != "" && c.LegacyComposePath != ""
}

func calculateWorkers() int {
	if s := os.Getenv("WORKER_COUNT"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return clamp(n, 1, 50)
		}
	}
	return clamp(runtime.NumCPU()*2, 2, 50)
}

func calculateQueueSize(workers int) int {
	if s := os.Getenv("QUEUE_SIZE"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return clamp(n, 50, 1000)
		}
	}
	return clamp(workers*15, 50, 1000)
}

func clamp(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseDurationOrDefault(key string, fallback time.Duration) time.Duration {
	if s := os.Getenv(key); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
	}
	return fallback
}
