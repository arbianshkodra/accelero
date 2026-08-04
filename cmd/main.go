package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/arbianshkodra/accelero/internal/audit"
	"github.com/arbianshkodra/accelero/internal/backup"
	"github.com/arbianshkodra/accelero/internal/breaker"
	"github.com/arbianshkodra/accelero/internal/config"
	"github.com/arbianshkodra/accelero/internal/dockerhost"
	"github.com/arbianshkodra/accelero/internal/handler"
	"github.com/arbianshkodra/accelero/internal/metrics"
	"github.com/arbianshkodra/accelero/internal/middleware"
	"github.com/arbianshkodra/accelero/internal/notify"
	"github.com/arbianshkodra/accelero/internal/rbac"
	"github.com/arbianshkodra/accelero/internal/reconciler"
	"github.com/arbianshkodra/accelero/internal/retry"
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

	// 2. Apply a pending restore (staged by POST /admin/restore) BEFORE
	// opening the database, so the file swap happens while nothing holds it
	// open — the only safe moment for SQLite.
	applyPendingRestore(cfg.DatabasePath)

	// Open the database.
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
		switch {
		case os.Getenv("ACCELERO_ENCRYPTION_KEY_FILE") != "":
			source = "ACCELERO_ENCRYPTION_KEY_FILE"
		case os.Getenv("ACCELERO_ENCRYPTION_KEY_VAULT") != "":
			source = "ACCELERO_ENCRYPTION_KEY_VAULT (unwrapped via Vault Transit)"
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

	// Multi-host: this daemon is the default host (stacks with an empty
	// host_id target it); additional hosts are registered in the store and
	// their clients built lazily on first use.
	hostManager := dockerhost.NewManager(cli, func(id string) (*dockerhost.Host, error) {
		h, err := db.GetDockerHost(id)
		if err != nil || h == nil {
			return nil, err
		}
		return &dockerhost.Host{
			ID: h.ID, Name: h.Name, Endpoint: h.Endpoint,
			TLSCA: h.TLSCA, TLSCert: h.TLSCert, TLSKey: h.TLSKey,
		}, nil
	})
	defer hostManager.Close()

	// 4. Create core components.
	retryPolicy := retry.Policy{
		MaxAttempts: cfg.RetryMaxAttempts,
		BaseDelay:   cfg.RetryBaseDelay,
		MaxDelay:    cfg.RetryMaxDelay,
	}
	deployer := stack.NewDeployer(cli, db, cfg.StacksDataDir)
	deployer.SetRetryPolicy(retryPolicy)
	deployer.SetHostClients(hostManager)
	rec := reconciler.New(db, cli, deployer)
	rec.SetRetryPolicy(retryPolicy)
	rec.SetHostClients(hostManager)
	rec.SetCircuitBreaker(breaker.New(cfg.CircuitBreakerThreshold, cfg.CircuitBreakerCooldown))
	if cfg.RetryMaxAttempts > 1 {
		logrus.Infof("Transient-operation retries enabled: up to %d attempts, backoff %s..%s",
			cfg.RetryMaxAttempts, cfg.RetryBaseDelay, cfg.RetryMaxDelay)
	}
	if cfg.CircuitBreakerThreshold > 0 {
		logrus.Infof("Auto-deploy circuit breaker enabled: trips after %d consecutive failures, %s cooldown",
			cfg.CircuitBreakerThreshold, cfg.CircuitBreakerCooldown)
	}

	// Shared audit recorder — writes to the same SQLite store. When
	// notification destinations are configured, wrap it so notable
	// deploy/drift events are also pushed out (deployer/reconciler need no
	// changes — they already record these events).
	notifyCfg := notify.Config{
		WebhookURL: cfg.NotifyWebhookURL,
		SlackURL:   cfg.NotifySlackWebhookURL,
		DiscordURL: cfg.NotifyDiscordWebhookURL,
		TeamsURL:   cfg.NotifyTeamsWebhookURL,
	}
	if cfg.NotifySMTPHost != "" {
		notifyCfg.Email = &notify.EmailConfig{
			Host:     cfg.NotifySMTPHost,
			Port:     cfg.NotifySMTPPort,
			Username: cfg.NotifySMTPUsername,
			Password: cfg.NotifySMTPPassword,
			From:     cfg.NotifyEmailFrom,
			To:       cfg.NotifyEmailTo,
		}
	}
	notifier := notify.New(notifyCfg)
	auditRecorder := notify.WrapRecorder(audit.NewStoreRecorder(db), notifier)
	deployer.SetAudit(auditRecorder)
	rec.SetAudit(auditRecorder)
	if notifier.Enabled() {
		logrus.Info("Notifications enabled for deploy/drift events")
	}

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
		cleanupLoop(ctx, db, hostManager, cfg)
	}()

	// 9. Start the deployment cleanup routine (prune old DB records).
	wg.Add(1)
	go func() {
		defer wg.Done()
		deploymentCleanupLoop(ctx, db, cfg)
	}()

	// 9a. Start the approval-timeout sweep if enabled — auto-rejects
	// deploys held for approval that have waited longer than the timeout.
	if cfg.ApprovalTimeout > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			approvalTimeoutLoop(ctx, deployer, cfg)
		}()
		logrus.Infof("Approval-timeout sweep enabled (timeout %s)", cfg.ApprovalTimeout)
	}

	// 9b. Start the scheduled-backup routine if enabled.
	if cfg.BackupInterval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			backupLoop(ctx, db, cfg)
		}()
		encNote := ""
		if cfg.BackupEncryptionPassphrase != "" {
			encNote = " (age-encrypted)"
		}
		if cfg.BackupS3Bucket != "" {
			encNote += " + S3 bucket " + cfg.BackupS3Bucket
		}
		logrus.Infof("Scheduled backups enabled: every %s to %s (keeping newest %d)%s",
			cfg.BackupInterval, cfg.BackupDir, cfg.BackupKeep, encNote)
	}

	// 10. Set up HTTP server.
	r := mux.NewRouter()
	r.Use(middleware.Metrics)

	// HSTS only when we're actually serving TLS. Setting it over plain
	// HTTP is at best ignored, at worst misleading; gating on the cert
	// presence keeps the header truthful.
	tlsEnabled := cfg.TLSCertFile != "" && cfg.TLSKeyFile != ""
	if tlsEnabled && cfg.TLSHSTSMaxAge > 0 {
		r.Use(middleware.NewHSTS(cfg.TLSHSTSMaxAge))
	}

	// Defensive response headers (nosniff, frame-options, referrer-policy,
	// plus a locked-down CSP by default). Valid over both HTTP and HTTPS,
	// so applied unconditionally — unlike HSTS, which requires real TLS.
	r.Use(middleware.NewSecurityHeaders(cfg.ContentSecurityPolicy))

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
		// Multi-host: introspect a stack on the host it deploys to, and fan
		// the root resource browsers across every host.
		DockerForHost: func(hostID string) (handler.DockerClient, error) {
			return hostManager.ClientFor(hostID)
		},
		DockerHosts: func() ([]handler.HostClient, error) {
			entries := allHostClients(db, hostManager)
			out := make([]handler.HostClient, 0, len(entries))
			for _, e := range entries {
				out = append(out, handler.HostClient{ID: e.id, Name: e.name, Docker: e.cli})
			}
			return out, nil
		},
		VolumeBrowser:     volumepkg.NewDockerBrowser(cli, ""),
		// The browse/write helper container must run on the same daemon the
		// volume lives on, so build a browser per host on demand (cheap — the
		// underlying client is already cached by the host manager).
		VolumeBrowserForHost: func(hostID string) (volumepkg.Browser, error) {
			hostCli, err := hostManager.ClientFor(hostID)
			if err != nil {
				return nil, err
			}
			return volumepkg.NewDockerBrowser(hostCli, ""), nil
		},
		AllowVolumeWrites: cfg.AllowVolumeWrites,
		EncryptionEnabled: cipher.Enabled(),
		BackupEncryptor:   backup.NewEncryptor(cfg.BackupEncryptionPassphrase),
		DatabasePath:      cfg.DatabasePath,
		AllowRestore:      cfg.AllowRestore,
		BackupPassphrase:  cfg.BackupEncryptionPassphrase,
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
	// RBAC key lookup: hash the presented key, resolve its role, and record
	// last-used (best-effort). The env API_KEY is handled inside the
	// middleware as the bootstrap admin key.
	keyLookup := func(raw string) (rbac.Identity, bool, error) {
		k, err := db.GetAPIKeyByHash(store.HashAPIKey(raw))
		if err != nil {
			return rbac.Identity{}, false, err
		}
		if k == nil || k.Disabled {
			return rbac.Identity{}, false, nil
		}
		role, ok := rbac.ParseRole(k.Role)
		if !ok {
			return rbac.Identity{}, false, nil
		}
		var grants map[string]rbac.Role
		for stackID, roleStr := range k.StackGrants {
			if gr, ok := rbac.ParseRole(roleStr); ok {
				if grants == nil {
					grants = make(map[string]rbac.Role, len(k.StackGrants))
				}
				grants[stackID] = gr
			}
		}
		go func() { _ = db.TouchAPIKey(k.ID, time.Now()) }()
		return rbac.Identity{Name: k.Name, Role: role, StackGrants: grants}, true, nil
	}
	// Resolve a URL stack token (id or name) to the canonical stack ID for
	// per-stack grant lookups in the auth middleware.
	resolveStack := func(token string) (string, bool) {
		if st, err := db.GetStack(token); err == nil && st != nil {
			return st.ID, true
		}
		if st, _ := db.GetStackByName(token); st != nil {
			return st.ID, true
		}
		return "", false
	}
	apiKeyAuth := middleware.NewAPIKeyAuth(cfg.APIKey, keyLookup, resolveStack)
	authChain := func(next http.Handler) http.Handler {
		return apiKeyAuth(rateLimit(next))
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

	if tlsEnabled {
		logrus.Infof("Starting HTTPS server on :%s (cert=%s)", cfg.ServerPort, cfg.TLSCertFile)
		if err := server.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile); err != nil && err != http.ErrServerClosed {
			logrus.Fatalf("HTTPS server error: %v", err)
		}
	} else {
		logrus.Infof("Starting HTTP server on :%s (TLS disabled; set TLS_CERT_FILE+TLS_KEY_FILE to enable)", cfg.ServerPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logrus.Fatalf("Server error: %v", err)
		}
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
// cleanupLoop prunes accelero-labelled Docker resources on every host — the
// default daemon plus each registered one — so a remote host doesn't accumulate
// dangling images/volumes forever. A failure on one host is logged and the
// sweep continues to the rest.
func cleanupLoop(ctx context.Context, db store.Store, hostManager *dockerhost.Manager, cfg *config.Config) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logrus.Info("Cleanup routine stopped")
			return
		case <-ticker.C:
			logrus.Info("Running Docker resource cleanup")
			for _, host := range allHostClients(db, hostManager) {
				if err := service.CleanupResources(ctx, host.cli); err != nil {
					logrus.Errorf("Cleanup error on host %s: %v", host.name, err)
				}
			}
		}
	}
}

// hostEntry is a Docker host paired with its client.
type hostEntry struct {
	id   string
	name string
	cli  *client.Client
}

// allHostClients returns the default host first, then every registered host
// whose client can be built. Hosts that fail to resolve are logged and skipped
// so one bad entry can't break a whole sweep or listing.
func allHostClients(db store.Store, hostManager *dockerhost.Manager) []hostEntry {
	out := []hostEntry{{id: "", name: handler.DefaultHostName, cli: hostManager.Default()}}

	hosts, err := db.ListDockerHosts()
	if err != nil {
		logrus.Errorf("failed to list docker hosts: %v", err)
		return out
	}
	for _, h := range hosts {
		cli, err := hostManager.ClientFor(h.ID)
		if err != nil {
			logrus.Warnf("skipping docker host %s (%s): %v", h.Name, h.Endpoint, err)
			continue
		}
		out = append(out, hostEntry{id: h.ID, name: h.Name, cli: cli})
	}
	return out
}

// deploymentCleanupLoop prunes old deployment and audit records on the
// same cadence. The two have very different retention defaults (deploy
// history: 24h; audit: 90d) but sharing the ticker keeps the number of
// background goroutines down — the cleanup work is all cheap DELETE
// queries.
// applyPendingRestore swaps in a snapshot staged by POST /admin/restore. It
// runs at startup, before the DB is opened, so the file replacement happens
// while nothing holds the database open — the only safe moment for SQLite. The
// previous database is preserved as <dbPath>.pre-restore-<ts> as a safety net.
func applyPendingRestore(dbPath string) {
	staged := dbPath + ".restore"
	if _, err := os.Stat(staged); err != nil {
		return // nothing staged
	}
	logrus.Warnf("Pending database restore found (%s) — applying before startup", staged)

	ts := time.Now().UTC().Format("20060102T150405Z")
	if _, err := os.Stat(dbPath); err == nil {
		bak := dbPath + ".pre-restore-" + ts
		if err := os.Rename(dbPath, bak); err != nil {
			logrus.Fatalf("restore: could not move current DB aside: %v", err)
		}
		logrus.Infof("restore: previous database preserved at %s", bak)
	}
	// Drop stale WAL/SHM sidecars of the old DB so the restored file opens clean.
	for _, s := range []string{"-wal", "-shm"} {
		_ = os.Remove(dbPath + s)
	}
	if err := os.Rename(staged, dbPath); err != nil {
		logrus.Fatalf("restore: could not move staged snapshot into place: %v", err)
	}
	logrus.Infof("restore: applied staged snapshot to %s", dbPath)
}

// backupLoop writes a consistent database snapshot to cfg.BackupDir every
// cfg.BackupInterval, retaining the newest cfg.BackupKeep. It runs one backup
// immediately on start so operators get a snapshot without waiting a full
// interval, then ticks. Failures are logged, not fatal.
func backupLoop(ctx context.Context, db store.Store, cfg *config.Config) {
	enc := backup.NewEncryptor(cfg.BackupEncryptionPassphrase)
	s3 := backup.S3Config{
		Endpoint:  cfg.BackupS3Endpoint,
		Region:    cfg.BackupS3Region,
		Bucket:    cfg.BackupS3Bucket,
		Prefix:    cfg.BackupS3Prefix,
		AccessKey: cfg.BackupS3AccessKey,
		SecretKey: cfg.BackupS3SecretKey,
		UseSSL:    cfg.BackupS3UseSSL,
	}
	runBackup := func() {
		path, err := backup.RunOnce(ctx, db, cfg.BackupDir, cfg.BackupKeep, time.Now(), enc)
		if err != nil {
			logrus.Errorf("Scheduled backup failed: %v", err)
			return
		}
		logrus.Infof("Wrote scheduled backup: %s", path)

		// Best-effort offsite copy — a failed upload must not fail the
		// (already-written) local backup.
		if s3.Enabled() {
			key, err := s3.UploadFile(ctx, path, filepath.Base(path))
			if err != nil {
				logrus.Errorf("Scheduled backup: S3 upload failed: %v", err)
			} else {
				logrus.Infof("Scheduled backup: uploaded to s3 bucket %s as %s", s3.Bucket, key)
			}
		}
	}

	runBackup() // once at startup

	ticker := time.NewTicker(cfg.BackupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logrus.Info("Scheduled backup routine stopped")
			return
		case <-ticker.C:
			runBackup()
		}
	}
}

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

// approvalTimeoutLoop periodically auto-rejects deploys held for approval
// longer than cfg.ApprovalTimeout. The check cadence is capped at 1h (and
// floored at 1m) so short timeouts are honoured reasonably promptly without
// busy-looping on long ones.
func approvalTimeoutLoop(ctx context.Context, deployer *stack.Deployer, cfg *config.Config) {
	interval := cfg.ApprovalTimeout
	if interval > time.Hour {
		interval = time.Hour
	}
	if interval < time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logrus.Info("Approval-timeout sweep stopped")
			return
		case <-ticker.C:
			n, err := deployer.ExpirePendingApprovals(ctx, cfg.ApprovalTimeout)
			if err != nil {
				logrus.Errorf("Approval-timeout sweep error: %v", err)
			} else if n > 0 {
				logrus.Infof("Auto-rejected %d timed-out approval(s)", n)
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
