package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/arbianshkodra/accelero/internal/audit"
	"github.com/arbianshkodra/accelero/internal/config"
	"github.com/arbianshkodra/accelero/internal/handler"
	"github.com/arbianshkodra/accelero/internal/metrics"
	"github.com/arbianshkodra/accelero/internal/middleware"
	"github.com/arbianshkodra/accelero/internal/reconciler"
	"github.com/arbianshkodra/accelero/internal/secrets"
	"github.com/arbianshkodra/accelero/internal/service"
	"github.com/arbianshkodra/accelero/internal/stack"
	"github.com/arbianshkodra/accelero/internal/store"
	volumepkg "github.com/arbianshkodra/accelero/internal/volume"
	"github.com/moby/moby/client"
	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
)

// Build-time variables injected via ldflags.
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	// 1. Load config.
	cfg, err := config.Load()
	if err != nil {
		logrus.Fatalf("Configuration error: %v", err)
	}
	initLogging(cfg)
	logrus.Infof("Accelero %s (commit: %s, built: %s)", version, commit, date)

	// 2. Open the database.
	db, err := store.NewSQLiteStore(cfg.DatabasePath)
	if err != nil {
		logrus.Fatalf("Database error: %v", err)
	}
	defer db.Close()
	logrus.Info("Database initialized")

	// 2b. Load the at-rest encryption cipher.  Missing env var is
	// explicitly allowed (dev + backward-compat) — we just warn. A
	// malformed env var is a fatal misconfiguration: the operator
	// asked for encryption and we shouldn't silently fall back to
	// plaintext.
	cipher, err := secrets.LoadCipherFromEnv()
	if err != nil {
		logrus.Fatalf("Secrets error: %v", err)
	}
	if cipher.Enabled() {
		db.SetCipher(cipher)
		source := "ACCELERO_ENCRYPTION_KEY"
		if os.Getenv("ACCELERO_ENCRYPTION_KEY_FILE") != "" {
			source = "ACCELERO_ENCRYPTION_KEY_FILE"
		}
		logrus.Infof("At-rest encryption enabled (%s)", source)
	} else {
		logrus.Warn("At-rest encryption DISABLED — repo tokens and Docker passwords stored as plaintext. " +
			"Set ACCELERO_ENCRYPTION_KEY (base64 inline) or ACCELERO_ENCRYPTION_KEY_FILE (path to base64 file) to enable. " +
			"Generate one with: openssl rand -base64 32")
	}

	// 3. Create Docker client and verify the daemon. API version negotiation
	// is now enabled by default on the Moby client (it used to require an
	// explicit Opt). We still call Ping below to confirm reachability.
	cli, err := client.New(
		client.WithHost(cfg.DockerSock),
	)
	if err != nil {
		logrus.Fatalf("Failed to create Docker client: %v", err)
	}
	defer cli.Close()

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if _, err := cli.Ping(pingCtx, client.PingOptions{}); err != nil {
		logrus.Fatalf("Cannot connect to Docker daemon at %s: %v", cfg.DockerSock, err)
	}
	logrus.Info("Connected to Docker daemon")

	// 4. Create core components.
	deployer := stack.NewDeployer(cli, db, cfg.StacksDataDir)
	rec := reconciler.New(db, cli, deployer)

	// Shared audit recorder — writes to the same SQLite store.
	auditRecorder := audit.NewStoreRecorder(db)
	deployer.SetAudit(auditRecorder)
	rec.SetAudit(auditRecorder)

	// 5. Migrate legacy env-var config to a "default" stack if needed.
	migrateLegacyConfig(cfg, db)

	// 6. Create a root context for the application lifetime.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup

	// 7. Start the reconciler.
	rec.Start(ctx)

	// 8. Start the cleanup routine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		cleanupLoop(ctx, cli, cfg)
	}()

	// 9. Start the deployment cleanup routine (prune old DB records).
	wg.Add(1)
	go func() {
		defer wg.Done()
		deploymentCleanupLoop(ctx, db, cfg)
	}()

	// 10. Set up HTTP server.
	r := mux.NewRouter()
	r.Use(middleware.Metrics)

	// /metrics is exposed unauthenticated — this is the convention Prometheus
	// scrapers rely on.  No secrets leak; Accelero metrics describe rates and
	// durations, never payload contents.
	r.Handle("/metrics", metrics.Handler()).Methods("GET")

	h := &handler.Handler{
		Store:             db,
		Deployer:          deployer,
		Reconciler:        rec,
		Audit:             auditRecorder,
		Docker:            cli,
		VolumeBrowser:     volumepkg.NewDockerBrowser(cli, ""),
		AllowVolumeWrites: cfg.AllowVolumeWrites,
		EncryptionEnabled: cipher.Enabled(),
		DockerPing: func(ctx context.Context) error {
			_, err := cli.Ping(ctx, client.PingOptions{})
			return err
		},
	}

	if cfg.WebhookSecret != "" {
		h.WebhookAuth = middleware.NewWebhookSignature(cfg.WebhookSecret)
		logrus.Info("Webhook HMAC-SHA256 signature verification enabled (WEBHOOK_SECRET set)")
	}

	// Compose rate limiting onto auth so only authenticated requests
	// count against the bucket (and we can key per API key). A zero
	// RateLimitRPS yields an identity middleware — no allocations per
	// request, no behaviour change for existing deployments.
	rateLimit := middleware.NewRateLimit(cfg.RateLimitRPS, cfg.RateLimitBurst)
	if cfg.RateLimitRPS > 0 {
		logrus.Infof("Rate limiting enabled: %.2f req/s per API key, burst %d",
			cfg.RateLimitRPS, cfg.RateLimitBurst)
	}
	authChain := func(next http.Handler) http.Handler {
		return middleware.APIKeyAuth(rateLimit(next))
	}

	// Register all routes — handler applies auth middleware where needed.
	h.RegisterRoutes(r, authChain)

	// Start the stack-gauge refresher; inexpensive enough to run every 15s.
	wg.Add(1)
	go func() {
		defer wg.Done()
		refreshStackGauges(ctx, db)
	}()

	server := &http.Server{
		Addr:    ":" + cfg.ServerPort,
		Handler: r,
	}

	// 11. Signal handling for graceful shutdown.
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs
		logrus.Info("Shutdown signal received")

		// Flip /readyz to "draining" before tearing anything down so
		// upstream load balancers stop routing new traffic here first.
		h.SetShuttingDown()

		// Cancel the root context — stops reconciler, cleanup routines.
		cancel()

		// Shutdown HTTP server with a timeout.
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logrus.Errorf("HTTP server shutdown error: %v", err)
		}
	}()

	logrus.Infof("Starting server on :%s", cfg.ServerPort)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logrus.Fatalf("Server error: %v", err)
	}

	// Wait for background goroutines to finish.
	rec.Stop()
	wg.Wait()

	logrus.Info("Accelero stopped")
}

// initLogging sets up logrus based on config.
func initLogging(cfg *config.Config) {
	level, err := logrus.ParseLevel(cfg.LogLevel)
	if err != nil {
		level = logrus.InfoLevel
	}
	logrus.SetLevel(level)

	if cfg.LogFormat == "json" {
		logrus.SetFormatter(&logrus.JSONFormatter{TimestampFormat: "2006-01-02T15:04:05"})
	} else {
		logrus.SetFormatter(&logrus.TextFormatter{
			FullTimestamp:   true,
			TimestampFormat: "2006-01-02T15:04:05",
		})
	}
}

// migrateLegacyConfig creates a "default" stack from the old-style env vars
// so that existing users are not broken by the migration to multi-stack.
func migrateLegacyConfig(cfg *config.Config, db store.Store) {
	if !cfg.HasLegacyConfig() {
		return
	}

	existing, _ := db.GetStackByName("default")
	if existing != nil {
		return // already migrated
	}

	logrus.Info("Migrating legacy environment variables to a default stack")

	id, err := generateBootstrapID()
	if err != nil {
		logrus.Fatalf("Failed to generate ID: %v", err)
	}

	now := time.Now()
	s := &store.Stack{
		ID:               id,
		Name:             "default",
		RepoURL:          cfg.LegacyRepoURL,
		RepoUsername:     cfg.LegacyRepoUsername,
		RepoToken:        cfg.LegacyRepoToken,
		RepoBranch:       cfg.LegacyRepoBranch,
		ComposePath:      cfg.LegacyComposePath,
		ServiceFilter:    cfg.LegacyServiceNames,
		AutoDeploy:       true,
		Status:           store.StackStatusActive,
		DockerUsername:   cfg.LegacyDockerUsername,
		DockerPassword:   cfg.LegacyDockerPassword,
		DockerRegistry:   cfg.LegacyDockerRegistry,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	if err := db.CreateStack(s); err != nil {
		logrus.Fatalf("Failed to create default stack: %v", err)
	}
	logrus.Info("Default stack created from legacy configuration")
}

// cleanupLoop runs Docker resource cleanup on a 24h interval, scoped to
// accelero-managed resources only.
func cleanupLoop(ctx context.Context, cli *client.Client, cfg *config.Config) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logrus.Info("Cleanup routine stopped")
			return
		case <-ticker.C:
			logrus.Info("Running Docker resource cleanup")
			if err := service.CleanupResources(ctx, cli); err != nil {
				logrus.Errorf("Cleanup error: %v", err)
			}
		}
	}
}

// deploymentCleanupLoop prunes old deployment and audit records on the
// same cadence. The two have very different retention defaults (deploy
// history: 24h; audit: 90d) but sharing the ticker keeps the number of
// background goroutines down — the cleanup work is all cheap DELETE
// queries.
func deploymentCleanupLoop(ctx context.Context, db store.Store, cfg *config.Config) {
	ticker := time.NewTicker(cfg.StatusCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logrus.Info("Deployment cleanup routine stopped")
			return
		case <-ticker.C:
			n, err := db.CleanupOldDeployments(cfg.StatusMaxAge)
			if err != nil {
				logrus.Errorf("Deployment cleanup error: %v", err)
			} else if n > 0 {
				logrus.Infof("Cleaned up %d old deployment records", n)
			}

			// AuditMaxAge == 0 disables audit retention — rows are kept
			// forever. Helpful for compliance contexts that require it.
			if cfg.AuditMaxAge > 0 {
				n, err := db.CleanupOldAuditEntries(cfg.AuditMaxAge)
				if err != nil {
					logrus.Errorf("Audit cleanup error: %v", err)
				} else if n > 0 {
					logrus.Infof("Cleaned up %d old audit entries", n)
				}
			}
		}
	}
}

func generateBootstrapID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// refreshStackGauges refreshes the accelero_stacks gauge on a 15-second
// cadence.  Cheap to compute (one store query), and it means the /metrics
// endpoint always returns a fresh snapshot without cramming a DB query into
// the scrape handler.
func refreshStackGauges(ctx context.Context, db store.Store) {
	tick := func() {
		stacks, err := db.ListStacks()
		if err != nil {
			return
		}
		counts := map[string]int{}
		for _, s := range stacks {
			counts[s.Status]++
		}
		metrics.SetStackCounts(counts)
	}

	tick() // populate immediately so scrapes right after boot have data

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}
