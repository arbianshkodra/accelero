package config

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
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

	// AllowVolumeWrites gates the POST /api/v1/volumes/{name}/files
	// endpoint. It's a foot-gun — writing arbitrary bytes into a
	// managed volume is the kind of operation that will cause a
	// production outage the one time the operator got the path
	// wrong — so it's off by default. Set ALLOW_VOLUME_WRITES=true
	// to enable. Every write is still audited.
	AllowVolumeWrites bool

	// RateLimitRPS / RateLimitBurst configure the per-API-key token
	// bucket applied after authentication. RateLimitRPS<=0 disables
	// the limiter entirely (the default — existing deployments keep
	// their current behaviour until an operator opts in). RateLimitBurst
	// defaults to max(2*RateLimitRPS, 10) when the operator sets RPS
	// but not burst, giving legitimate bursty clients roughly a
	// two-second allowance.
	RateLimitRPS   float64
	RateLimitBurst int

	// WebhookSecret is the shared secret used to verify HMAC-SHA256
	// signatures on the legacy /webhook endpoint. Callers sign the raw
	// request body with this secret and pass the hex digest as
	// X-Hub-Signature-256: sha256=<hex>. Empty = no signature check,
	// /webhook continues to require an API key as before.
	WebhookSecret string

	// TLSCertFile / TLSKeyFile point at PEM-encoded certificate (or
	// chain) and private key. Both must be set together; either alone
	// is a startup error. Unset = plain HTTP (the default — many
	// deployments terminate TLS at a reverse proxy and don't want
	// Accelero doing it twice).
	TLSCertFile string
	TLSKeyFile  string

	// TLSHSTSMaxAge controls the max-age value of the
	// Strict-Transport-Security header set on every TLS response.
	// Defaults to 31536000 (one year, the IETF baseline). Set to 0
	// to disable the header entirely — useful when Accelero sits
	// behind a TLS-terminating proxy and the operator wants HSTS
	// configured at the edge instead. Ignored when TLS is off (HSTS
	// over plain HTTP is meaningless).
	TLSHSTSMaxAge int

	// ContentSecurityPolicy is the value of the Content-Security-Policy
	// header set on every response. Defaults to a locked-down policy
	// because Accelero serves no browser UI today — nothing legitimate
	// needs to load. Set CONTENT_SECURITY_POLICY="" (explicitly empty)
	// to omit the header, e.g. behind an edge proxy that sets its own;
	// leaving it unset keeps the default. The companion security headers
	// (X-Content-Type-Options, X-Frame-Options, Referrer-Policy) are
	// always sent regardless of this value.
	ContentSecurityPolicy string

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
		AllowVolumeWrites:     envOrDefault("ALLOW_VOLUME_WRITES", "false") == "true",
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

	cfg.RateLimitRPS = parseFloatOrDefault("RATE_LIMIT_RPS", 0)
	cfg.RateLimitBurst = parseIntOrDefault("RATE_LIMIT_BURST", 0)
	if cfg.RateLimitRPS > 0 && cfg.RateLimitBurst <= 0 {
		cfg.RateLimitBurst = int(math.Max(cfg.RateLimitRPS*2, 10))
	}

	cfg.WebhookSecret = os.Getenv("WEBHOOK_SECRET")

	cfg.TLSCertFile = strings.TrimSpace(os.Getenv("TLS_CERT_FILE"))
	cfg.TLSKeyFile = strings.TrimSpace(os.Getenv("TLS_KEY_FILE"))
	// One without the other is a misconfiguration: the operator
	// asked for TLS but didn't finish the wiring. Fail loud rather
	// than silently fall back to plain HTTP and let the request
	// listener leak through unencrypted.
	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		return nil, fmt.Errorf("TLS_CERT_FILE and TLS_KEY_FILE must both be set or both be empty")
	}
	cfg.TLSHSTSMaxAge = parseIntOrDefault("TLS_HSTS_MAX_AGE", 31536000)

	// LookupEnv (not Getenv) so an explicitly-empty CONTENT_SECURITY_POLICY
	// disables the header, while leaving it unset keeps the secure default.
	if v, ok := os.LookupEnv("CONTENT_SECURITY_POLICY"); ok {
		cfg.ContentSecurityPolicy = v
	} else {
		cfg.ContentSecurityPolicy = "default-src 'none'; frame-ancestors 'none'"
	}

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

func parseFloatOrDefault(key string, fallback float64) float64 {
	if s := os.Getenv(key); s != "" {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	}
	return fallback
}

func parseIntOrDefault(key string, fallback int) int {
	if s := os.Getenv(key); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
	}
	return fallback
}
