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

	// BackupInterval / BackupDir / BackupKeep configure scheduled local
	// database backups. BackupInterval <= 0 disables the scheduler (the
	// default; on-demand POST /admin/backup still works). When enabled, a
	// consistent snapshot is written to BackupDir every interval and only
	// the newest BackupKeep snapshots are retained (BackupKeep <= 0 keeps
	// all).
	BackupInterval time.Duration
	BackupDir      string
	BackupKeep     int

	// BackupEncryptionPassphrase, when non-empty, age-encrypts every backup
	// snapshot (scheduled and POST /admin/backup) with a scrypt passphrase.
	// Output files get a .age suffix and are decryptable anywhere with
	// `age -d` + the passphrase — important because snapshots contain
	// secrets (repo tokens, per-stack secrets). Empty = plaintext snapshots.
	BackupEncryptionPassphrase string

	// BackupS3* configure an optional S3-compatible remote destination for
	// scheduled snapshots (AWS S3, R2, MinIO, B2, GCS-interop). When
	// BackupS3Bucket is set, each scheduled snapshot is uploaded after it's
	// written locally. Remote retention is left to bucket lifecycle policies.
	BackupS3Bucket    string
	BackupS3Endpoint  string // host[:port], no scheme; empty = AWS
	BackupS3Region    string
	BackupS3Prefix    string
	BackupS3AccessKey string
	BackupS3SecretKey string
	BackupS3UseSSL    bool
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

	// AllowRestore gates POST /api/v1/admin/restore, which stages an
	// uploaded database snapshot to replace ALL current data on the next
	// restart. It's a foot-gun (wrong file = total data loss), so it's off
	// by default; set ALLOW_RESTORE=true to enable. Every attempt is audited.
	AllowRestore bool

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

	// RetryMaxAttempts / RetryBaseDelay / RetryMaxDelay configure the
	// bounded exponential-backoff retry applied to the transient
	// operations that fail intermittently on flaky networks — image
	// pulls, git clones, and Docker network creation. RetryMaxAttempts
	// is the total number of tries including the first; 1 disables
	// retrying. Backoff starts at RetryBaseDelay, doubles each attempt,
	// and is capped at RetryMaxDelay (with full jitter applied).
	RetryMaxAttempts int
	RetryBaseDelay   time.Duration
	RetryMaxDelay    time.Duration

	// CircuitBreakerThreshold / CircuitBreakerCooldown configure the
	// auto-deploy circuit breaker: after this many consecutive
	// reconcile-triggered deploy failures, a stack's breaker trips open
	// and auto-deploys are skipped until the cooldown elapses (then one
	// half-open trial is allowed). Threshold 0 disables the breaker.
	// Manual deploys are never gated.
	CircuitBreakerThreshold int
	CircuitBreakerCooldown  time.Duration

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
		AllowRestore:          envOrDefault("ALLOW_RESTORE", "false") == "true",
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

	// Retry budget for transient deploy-path operations. Clamp attempts
	// to >=1 so a nonsensical RETRY_MAX_ATTEMPTS=0 can't disable the
	// operation entirely — it just means "no retry".
	cfg.RetryMaxAttempts = parseIntOrDefault("RETRY_MAX_ATTEMPTS", 3)
	if cfg.RetryMaxAttempts < 1 {
		cfg.RetryMaxAttempts = 1
	}
	cfg.RetryBaseDelay = parseDurationOrDefault("RETRY_BASE_DELAY", 1*time.Second)
	cfg.RetryMaxDelay = parseDurationOrDefault("RETRY_MAX_DELAY", 30*time.Second)

	// Auto-deploy circuit breaker. Default enabled (threshold 5) — a stack
	// that fails 5 reconcile deploys in a row is almost certainly broken in
	// a way redeploying won't fix. Clamp to >=0; 0 disables.
	cfg.CircuitBreakerThreshold = parseIntOrDefault("CIRCUIT_BREAKER_THRESHOLD", 5)
	if cfg.CircuitBreakerThreshold < 0 {
		cfg.CircuitBreakerThreshold = 0
	}
	cfg.CircuitBreakerCooldown = parseDurationOrDefault("CIRCUIT_BREAKER_COOLDOWN", 10*time.Minute)

	// Scheduled local backups. Disabled by default (interval 0).
	cfg.BackupInterval = parseDurationOrDefault("BACKUP_INTERVAL", 0)
	cfg.BackupDir = envOrDefault("BACKUP_DIR", "./data/backups")
	cfg.BackupKeep = parseIntOrDefault("BACKUP_KEEP", 7)
	if cfg.BackupKeep < 0 {
		cfg.BackupKeep = 0
	}
	cfg.BackupEncryptionPassphrase = os.Getenv("BACKUP_ENCRYPTION_PASSPHRASE")
	cfg.BackupS3Bucket = os.Getenv("BACKUP_S3_BUCKET")
	cfg.BackupS3Endpoint = os.Getenv("BACKUP_S3_ENDPOINT")
	cfg.BackupS3Region = os.Getenv("BACKUP_S3_REGION")
	cfg.BackupS3Prefix = os.Getenv("BACKUP_S3_PREFIX")
	cfg.BackupS3AccessKey = os.Getenv("BACKUP_S3_ACCESS_KEY_ID")
	cfg.BackupS3SecretKey = os.Getenv("BACKUP_S3_SECRET_ACCESS_KEY")
	cfg.BackupS3UseSSL = envOrDefault("BACKUP_S3_USE_SSL", "true") == "true"

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
