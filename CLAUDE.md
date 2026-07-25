# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Accelero is a GitOps-based Docker deployment automation tool that enables zero-downtime deployments. It manages multiple "stacks" — each stack is a git repository + docker-compose file that defines the desired state of services on a Docker host. Accelero continuously reconciles the desired state (git) against the actual state (running containers) and deploys to converge.

## Build and Development Commands

### Building Binaries
```bash
# Build cross-platform binaries for all supported architectures
./scripts/build_binaries.sh
```

### Building Docker Images
```bash
# Build and publish multi-architecture Docker images
./scripts/build_docker_images.sh

# Build local image
docker build -t accelero:latest --build-arg TARGETARCH=amd64 .
```

### Testing
```bash
# Run all tests
go test ./...

# Run tests with verbose output
go test -v ./...

# Run tests for a specific package
go test ./internal/handler
go test ./internal/service
go test ./internal/utils
```

### Running the Application
```bash
# Run locally (requires Docker daemon and API_KEY env var)
API_KEY=your-key go run ./cmd/main.go

# Build and run binary
go build -o accelero ./cmd/main.go
./accelero
```

### Documentation
```bash
# Serve documentation locally (requires mkdocs)
mkdocs serve

# Build documentation
mkdocs build
```

## Architecture

### Core Components

- **`cmd/main.go`**: Entry point — loads config, opens SQLite DB, creates Docker client, wires deployer/reconciler/handlers, runs HTTP server with graceful shutdown
- **`internal/config/`**: Centralized configuration loaded from environment variables with defaults
- **`internal/store/`**: Persistence layer (SQLite) — stores stacks, deployments, and managed containers
- **`internal/stack/`**: Stack deployer — the core deployment orchestrator: git clone, compose parsing, dependency resolution, container lifecycle, rollback
- **`internal/compose/`**: Compose-file preprocessing — `.env` loading (`LoadDotEnv`) and docker-compose-compatible `${VAR}` interpolation (`Expand`, `ExpandBytes`). Used by both stack/ and reconciler/ before `yaml.Unmarshal`.
- **`internal/reconciler/`**: GitOps reconciliation engine — per-stack loops that detect drift and optionally auto-deploy
- **`internal/handler/`**: HTTP API handlers — stack CRUD, deployment triggers, drift checks, legacy webhook, health/status
- **`internal/middleware/`**: HTTP middleware — RBAC authentication + authorization (`NewAPIKeyAuth`: env key = admin, store-backed role-scoped keys, per-request role check), and Prometheus metrics recorder (request count + latency, labelled by mux route template so `/stacks/{id}` doesn't explode cardinality)
- **`internal/dockerhost/`**: multi-host client manager — `Manager.ClientFor(hostID)` lazily builds and caches one Docker client per registered host (empty ID → the default `DOCKER_SOCK` client), `Ping` validates a host at registration. In-memory PEM TLS is wired via `WithHTTPClient` (the client's `WithTLSClientConfig` wants file paths, and we don't spill keys to disk). Defines its own `Host` type so it doesn't import `store`.
- **`internal/rbac/`**: role model (`admin`⊇`operator`⊇`viewer`⊇`none`), request `Identity` (base role + per-stack grants) on context, `EffectiveRole(stackID)`, the method+path authorization policy (`RequiredRole`), and `StackTokenFromPath`. No internal deps, so middleware/handler/audit/store import it freely.
- **`internal/metrics/`**: Prometheus collectors and helpers. Own registry (not the default), exposing deployment counters/duration, drift events, HTTP traffic, and gauges for active reconciler loops + stack counts by status. `Handler()` returns the `/metrics` exposition handler.
- **`internal/service/`**: Shared types (ComposeService, HealthCheck) and Docker resource cleanup
- **`internal/network/`**: Docker network management (idempotent creation)
- **`internal/utils/`**: Utility functions for port mapping, env file loading, health checks

### Data Model

- **Stack**: A deployable unit — git repo URL + compose file path + branch + credentials + reconciliation settings
- **Deployment**: A single deployment attempt for a stack — status, trigger, git commit, changes, errors
- **ManagedContainer**: Tracks which Docker containers Accelero manages (labeled `managed-by=accelero`)

### Key Features

- **Multi-stack management**: Manage multiple independent stacks via REST API
- **GitOps reconciliation**: Periodic drift detection comparing git desired state vs actual Docker state
- **`.env` variable interpolation**: Full docker-compose `${VAR}`, `${VAR:-default}`, `${VAR:?error}`, `${VAR+value}`, `$$` syntax. `.env` lookup order: next to compose file → repo root.
- **Broad compose field support**: `entrypoint`, `working_dir`, `user`, `hostname`, `domainname`, `dns`, `dns_search`, `extra_hosts`, `cap_add`, `cap_drop`, `privileged`, `tmpfs`, `shm_size`, `init`, `expose`, `stop_grace_period`, `stop_signal`, `pull_policy` (always/missing/never; build rejected), `logging` (driver + options), long-form `ports` with per-binding `host_ip` and `protocol`, top-level `volumes:` (named volumes with scoped naming `accelero_<stack>_<name>`, idempotent create, `external: true` verification, never auto-delete), `deploy.replicas` (rolling create/remove, scale up/down, rejects replicas>1 with static published ports). See docs/usage-overview.md#supported-compose-fields for the full table.
- **Zero-downtime deployments**: New containers health-checked before old ones removed
- **Correct rollback**: Pre-deployment state captured BEFORE deploying, restored on failure
- **Per-service deployment locking**: Prevents concurrent deploys of the same service
- **Dependency resolution**: Topological sort with transitive dependency expansion and cycle detection. Long-form `depends_on` conditions (`service_started`, `service_healthy`, `service_completed_successfully`) are enforced — a dependent service's deploy blocks until its dependency's condition is satisfied.
- **Scoped resource cleanup**: Only prunes Docker resources labeled `managed-by=accelero`
- **Shallow git clones**: Depth=1 for fast repo fetching
- **SQLite persistence**: Deployment history, stack config, container tracking survive restarts
- **Legacy compatibility**: Old env-var config (REPO_URL etc.) auto-migrated to a "default" stack
- **Prometheus metrics**: `/metrics` (unauthenticated, scrape convention) exposes deployment counters/duration, drift events by type, HTTP traffic keyed by route template, and gauges for active reconcile loops + stack counts. Drift counters only increment from reconciler observations, not `/preview` calls.
- **Notifications**: notable deploy/drift/approval lifecycle events pushed to a generic JSON webhook, Slack, Discord, Microsoft Teams, and/or email (`NOTIFY_WEBHOOK_URL` / `NOTIFY_SLACK_WEBHOOK_URL` / `NOTIFY_DISCORD_WEBHOOK_URL` / `NOTIFY_TEAMS_WEBHOOK_URL` / `NOTIFY_SMTP_*`+`NOTIFY_EMAIL_*`). Each sink fires independently, best-effort, async, never blocks a deploy. Implemented as an `audit.Recorder` wrapper (`internal/notify`, `notify.New(notify.Config{...})`) so no deployer/reconciler changes were needed.
- **Multi-host**: the `DOCKER_SOCK` daemon is the implicit **default host**; additional daemons are registered via `/api/v1/hosts` (`unix://`/`tcp://`, optional mutual TLS — `tls_key` encrypted at rest, never serialised) and selected per stack with `stacks.host_id` (empty = default, so existing stacks are unaffected). `internal/dockerhost.Manager.ClientFor(hostID)` builds+caches one client per host. **Host-aware:** deploy (`Deployer.forHost` — a shallow Deployer copy with `cli` swapped, so the ~20 `d.cli` call sites are untouched; lock state is behind a pointer to stay shared); drift/reconcile/`/preview` (`Reconciler.dockerFor` → threaded into `checkDrift`/`listStackContainers`); all stack-scoped introspection (`Handler.dockerForStack` → a local `dkr` in each of the 9 handlers, also passed into `resolveStackContainer`; unreachable host → **502**); the root browsers fan out via `Handler.hostClients()` (default host + every registered host, each result tagged with a `host` field — `ManagedImage`/`ManagedVolume`/`ManagedNetwork` all carry it; one dead host is skipped with a warn, all-dead is a 500); and `cleanupLoop` prunes every host (`allHostClients` in main.go). **Still default-host-only:** volume browse/write — the `volumepkg.Browser` helper container runs on the default daemon and volume names aren't unique per host, so `volumeIsManaged` deliberately checks only there (see its comment). Registration pings before persisting; deleting an in-use host is 409.
- **RBAC**: named, role-scoped API keys (`none` ⊂ `viewer` ⊂ `operator` ⊂ `admin`). The `API_KEY` env is the bootstrap admin key; additional keys are stored SHA-256-hashed in SQLite (raw value shown once). Per-request authorization by method+path (`internal/rbac.RequiredRole`): GET→viewer, mutations→operator, `/admin/*`+`/apikeys`+`/hosts`→admin (hosts admin-only for reads too), `/exec`→operator. Enforced in `middleware.NewAPIKeyAuth`; the caller identity rides on the request context and becomes the audit actor. Keys managed via `/api/v1/apikeys` (admin-only). **Per-stack grants:** a key's `role` is the base (non-stack endpoints + ungranted stacks); optional `stack_grants` (`{stack: role}`, stored keyed by canonical stack ID) override it per stack. The middleware resolves the URL stack token → ID and applies `Identity.EffectiveRole(stackID)`. A `none` base yields a strictly-scoped key (only its granted stacks).
- **Approval gates**: stacks with `requires_approval=true` hold every deploy (manual/webhook/reconcile) as a `pending_approval` deployment + notification; an operator approves (runs it) or rejects via the API, and unactioned approvals auto-expire after `APPROVAL_TIMEOUT`. Deduped to one open approval per stack. See the `APPROVAL_TIMEOUT` entry for internals.
- **Deploy preview**: `POST /api/v1/stacks/{id}/preview` is a read-only dry run that reuses the reconciler's drift check and maps each drift item to the action the next deploy would take (`create`, `recreate`, `restart`, `remove`, `error`).

### Environment Variables

Required:
- `API_KEY`: **REQUIRED** — Secure API key for authentication. This is the bootstrap **admin** key (always full access). Additional role-scoped keys (`admin`/`operator`/`viewer`) are created at runtime via `POST /api/v1/apikeys` and stored SHA-256-hashed. See RBAC under Key Features.

Optional:
- `SERVER_PORT`: HTTP server port (default: 8000)
- `DOCKER_SOCK`: Docker socket path (default: unix:///var/run/docker.sock)
- `DATABASE_PATH`: SQLite database path (default: ./data/accelero.db)
- `STACKS_DATA_DIR`: Root directory for per-stack cloned repos (default: ./data/stacks). Used as the bind-mount source when a compose service references `./path/from/repo`. When Accelero runs in Docker, this path must be the same inside and outside the container — bind-mount the host dir at the same path.
- `ALLOW_VOLUME_WRITES`: Set to `true` to enable `POST /volumes/{name}/files` (emergency file write into a managed volume). Default `false`. Every call is audited as `volume.write` regardless of outcome.
- `ALLOW_RESTORE`: Set to `true` to enable `POST /api/v1/admin/restore` (upload a snapshot to replace the DB). Default `false` (foot-gun: wrong file = total data loss). Restore validates + **stages** the upload; the file swap happens at the **next restart** (`applyPendingRestore` in `cmd/main.go`, before the DB is opened), preserving the prior DB as `<DATABASE_PATH>.pre-restore-<ts>`. Accepts plaintext `.db` or age `.db.age` (decrypted via `X-Backup-Passphrase` header or `BACKUP_ENCRYPTION_PASSPHRASE`). Validation is `store.ValidateBackupFile` (SQLite magic + `PRAGMA integrity_check` + `stacks` table). Audited as `admin.restore`.
- `LOG_LEVEL`: Logging level (default: info)
- `LOG_FORMAT`: Log format — "json" or "text" (default: text)
- `WORKER_COUNT`: Worker goroutine count override (default: 2 * CPU cores, min: 2, max: 50)
- `QUEUE_SIZE`: Task queue buffer size override (default: 15 * workers, min: 50, max: 1000)
- `BACKUP_INTERVAL` / `BACKUP_DIR` / `BACKUP_KEEP`: scheduled local DB backups. `BACKUP_INTERVAL` (Go duration, default `0` = disabled) writes a consistent `VACUUM INTO` snapshot on startup and every interval into `BACKUP_DIR` (default `./data/backups`), retaining the newest `BACKUP_KEEP` (default `7`; `0` keeps all — prunes only `accelero-backup-*.db`/`.db.age`). Same snapshot as `POST /admin/backup`. Implemented in `internal/backup` (`RunOnce` + `Prune`); driven by `backupLoop` in `cmd/main.go`.
- `BACKUP_S3_*`: optional S3-compatible remote destination for scheduled snapshots (AWS S3, R2, MinIO, B2, GCS-interop). `BACKUP_S3_BUCKET` enables it; each scheduled snapshot is uploaded (best-effort — a failed upload never fails the local backup) after it's written, so encryption applies to the uploaded copy too. Vars: `BACKUP_S3_BUCKET`, `BACKUP_S3_ENDPOINT` (host[:port], empty=AWS), `BACKUP_S3_REGION` (default us-east-1), `BACKUP_S3_ACCESS_KEY_ID`, `BACKUP_S3_SECRET_ACCESS_KEY`, `BACKUP_S3_PREFIX`, `BACKUP_S3_USE_SSL` (default true). Implemented in `internal/backup/s3.go` (`S3Config.UploadFile` via `minio-go`); called from `backupLoop`. Remote retention is via bucket lifecycle policy — Accelero only uploads.
- `BACKUP_ENCRYPTION_PASSPHRASE`: when set, age-encrypts every snapshot (scheduled + `POST /admin/backup`) with a scrypt passphrase → standard age stream, files suffixed `.db.age`, decryptable with `age -d`. Whole-file wrap, independent of `ACCELERO_ENCRYPTION_KEY` (which encrypts DB *fields*). Implemented in `internal/backup` (`Encryptor`, `NewEncryptor`, `EncryptFile` using `filippo.io/age`); wired via `backupLoop` (scheduled) and `Handler.BackupEncryptor` (HTTP). Unset = plaintext snapshots.
- `STATUS_CLEANUP_INTERVAL`: Deployment record cleanup interval (default: 1h) — shared with audit cleanup
- `STATUS_MAX_AGE`: Max deployment record age (default: 24h)
- `AUDIT_MAX_AGE`: Max audit entry age before cleanup (default: 90 days). Set to `0` to disable retention (useful for compliance contexts that require indefinite retention)
- `ACCELERO_ENCRYPTION_KEY`: Base64-encoded 32-byte master key, inline. When set, `repo_token` and `docker_password` are encrypted at rest in SQLite with AES-256-GCM (versioned ciphertext format `v1:<nonce>:<ct>`). Legacy plaintext rows are read transparently; migrate them via `POST /api/v1/admin/encrypt-existing`. Unset = plaintext (dev default, logged as a warning).
- `ACCELERO_ENCRYPTION_KEY_FILE`: Path to a file containing the base64-encoded 32-byte key. Preferred over the inline env var in production — env vars leak through `docker inspect`, `ps`, systemd unit files, and shell history; a file mounted as a Docker/K8s secret doesn't. Trailing whitespace/newlines are trimmed. Empty file = fatal error (likely a broken secret mount, not a deliberate "disable"). Setting both this and `ACCELERO_ENCRYPTION_KEY` is a fatal configuration error.
- `RATE_LIMIT_RPS`: Sustained refill rate (tokens/second) for the per-API-key token bucket. Default `0` disables rate limiting entirely. Set to e.g. `10` to allow 10 req/s with the burst below.
- `RATE_LIMIT_BURST`: Bucket capacity — how many requests a quiescent caller can send at once before throttling kicks in. Defaults to `max(2*RATE_LIMIT_RPS, 10)` when RPS is set but burst isn't. Ignored when `RATE_LIMIT_RPS<=0`.
- `RETRY_MAX_ATTEMPTS` / `RETRY_BASE_DELAY` / `RETRY_MAX_DELAY`: bounded exponential-backoff retry (with full jitter) for the transient operations in the deploy/reconcile paths — image pulls, git clones, Docker network creation. Defaults `3` / `1s` / `30s`. `RETRY_MAX_ATTEMPTS=1` disables retrying (clamped to `>=1`). Backoff for attempt *n* is a uniform draw in `[0, min(base·2^(n-1), max)]`. Non-retryable failures fail fast: image pulls short-circuit on unauthorized / permission-denied / not-found (`cerrdefs.Is*`); git clones on auth-required/failed, repo-not-found, reference-not-found (`gitutil.IsPermanentCloneError`). Implemented in `internal/retry` (`retry.Do` + `retry.Permanent`); wired via `Deployer.SetRetryPolicy` / `Reconciler.SetRetryPolicy` from `main.go`.
- `CIRCUIT_BREAKER_THRESHOLD` / `CIRCUIT_BREAKER_COOLDOWN`: auto-deploy circuit breaker for stacks that keep failing across reconcile cycles. Defaults `5` / `10m`; `THRESHOLD=0` disables. The reconciler now reconciles `error`-status stacks too (not just `active`) so a failed stack **self-heals** once its repo is fixed; the breaker throttles those retries. After N consecutive reconcile-deploy failures the breaker trips open (one `stack.circuit_breaker.opened` audit entry, `accelero_circuit_breaker_tripped_total`), auto-deploys are skipped (`accelero_auto_deploys_skipped_total`) until the cooldown, then a half-open trial runs; success closes it. **Manual deploys are never gated.** Implemented in `internal/breaker`; wired via `Reconciler.SetCircuitBreaker`. Which statuses reconcile is `reconciler.shouldReconcile` (active + error). Note: `runLoop` no longer re-reads the stack at loop start (it takes the object from `startStackLoop`) — a transient DB error there used to permanently kill a stack's loop.
- `APPROVAL_TIMEOUT`: approval-gate expiry for stacks with `requires_approval=true`. A held deploy left unactioned longer than this is auto-rejected (`approval.timed_out`). Go duration, default `24h`; `0` disables expiry (approvals wait forever). A background sweep (`approvalTimeoutLoop` in `cmd/main.go`, cadence capped at 1h / floored at 1m) calls `Deployer.ExpirePendingApprovals`. Approval gates: a `requires_approval` stack holds every deploy trigger (manual/webhook/reconcile) as a `pending_approval` deployment + `approval.requested` audit instead of running; at most one open approval per stack (deduped in `Deployer.requestApproval` via `store.GetPendingApproval`). Approve/reject via API; the gate lives at the top of `Deployer.Deploy` with the execution core factored into `runDeployment` (shared by the direct and approved paths). `deploy.start` is suppressed for gated stacks so notifications don't show "started" + "awaiting approval" together.
- `NOTIFY_WEBHOOK_URL` / `NOTIFY_SLACK_WEBHOOK_URL` / `NOTIFY_DISCORD_WEBHOOK_URL` / `NOTIFY_TEAMS_WEBHOOK_URL`: optional notification destinations for notable deploy/drift lifecycle events. `NOTIFY_WEBHOOK_URL` receives the full Event JSON (`type`/`stack`/`outcome`/`message`/`fields`/`time`); `NOTIFY_SLACK_WEBHOOK_URL` receives a Slack `{"text": ...}` payload; `NOTIFY_DISCORD_WEBHOOK_URL` a Discord `{"content": ...}`; `NOTIFY_TEAMS_WEBHOOK_URL` a Microsoft Teams `MessageCard`. An **email/SMTP** sink (`NOTIFY_SMTP_HOST`/`NOTIFY_SMTP_PORT` default 587/`NOTIFY_SMTP_USERNAME`/`NOTIFY_SMTP_PASSWORD`/`NOTIFY_EMAIL_FROM`/`NOTIFY_EMAIL_TO` comma-separated) sends one plain-text message per event — enabled when host+from+to are set; port 465=implicit TLS, else STARTTLS when offered, PLAIN auth only when a username is set (`internal/notify/email.go`, stdlib `net/smtp`, injectable transport for tests). Each sink fires independently. `notify.New` takes a `notify.Config` struct (one field per sink; email is a `*EmailConfig`). Events notified: deploy start/complete/failed/rolled_back, drift detected, drift auto_deployed (read/secret-CRUD/other audit ops are deliberately not notified). Delivery is best-effort + async (one goroutine per sink, 10s timeout, no retries) — a failing sink never blocks or fails a deploy. Implemented in `internal/notify` by **wrapping the shared `audit.Recorder`** (`notify.New` + `notify.WrapRecorder` in `cmd/main.go`), so the deployer/reconciler are untouched — they already record these audit events. Both unset = no-op (`Notifier.Enabled()==false`, base recorder returned unchanged).
- `WEBHOOK_SECRET`: Shared HMAC-SHA256 secret for the legacy `POST /webhook` endpoint. When set, callers must sign the raw request body and send the hex digest as `X-Hub-Signature-256: sha256=<hex>` (GitHub webhook format). Replaces the API-key check on `/webhook` only; `POST /api/v1/webhook` still requires an API key. Unset = legacy API-key behavior.
- `TLS_CERT_FILE` / `TLS_KEY_FILE`: PEM-encoded certificate (or chain) and private key paths. Both must be set together; only one set is a fatal startup error (the operator asked for TLS but didn't finish wiring it). Unset = plain HTTP (the default — many deployments terminate TLS at a reverse proxy).
- `TLS_HSTS_MAX_AGE`: `Strict-Transport-Security` `max-age` value in seconds. Default `31536000` (1 year). Set to `0` to disable the header — useful behind a TLS-terminating proxy that already sets HSTS at the edge. Ignored when TLS is off (HSTS over plain HTTP is meaningless). Accelero deliberately omits `includeSubDomains` and `preload` — those are host-level decisions the operator should make at the edge, not something a single service imposes on neighbours sharing the hostname.
- `CONTENT_SECURITY_POLICY`: value of the `Content-Security-Policy` header, set on every response over both HTTP and HTTPS. Default `default-src 'none'; frame-ancestors 'none'` (Accelero serves no browser UI, so nothing legitimate needs to load). Set to an *explicitly empty* string to omit the header (parsed via `os.LookupEnv`, so unset ≠ empty) — useful behind an edge proxy that owns the policy. Regardless of this value, three companion headers are always sent: `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`.

Legacy (backward-compatible, auto-creates "default" stack):
- `REPO_URL`, `REPO_USERNAME`, `REPO_TOKEN`, `REPO_BRANCH`, `COMPOSE_PATH`
- `SERVICE_NAMES`, `DOCKER_USERNAME`, `DOCKER_PASSWORD`, `DOCKER_REGISTRY`

### API Endpoints

**Stack Management:**
- `POST /api/v1/stacks` — Create a stack
- `GET /api/v1/stacks` — List all stacks
- `GET /api/v1/stacks/{id}` — Get a stack (by ID or name)
- `PUT /api/v1/stacks/{id}` — Update a stack
- `DELETE /api/v1/stacks/{id}` — Delete a stack

**Deployments:**
- `POST /api/v1/stacks/{id}/deploy` — Trigger deployment
- `GET /api/v1/stacks/{id}/deployments` — List deployment history
- `GET /api/v1/stacks/{id}/drift` — Check drift (desired vs actual state)
- `POST /api/v1/stacks/{id}/preview` — Dry-run: show actions the next deploy would take

**Approval gates (stacks with `requires_approval=true`):**
- `POST /api/v1/stacks/{id}/deployments/{deployId}/approve` — Run a held deploy (202, async rollout). Audited `approval.granted`.
- `POST /api/v1/stacks/{id}/deployments/{deployId}/reject` — Cancel it; optional body `{"reason"}`. Audited `approval.rejected`.
- `GET /api/v1/approvals` — List all `pending_approval` deployments across every stack, oldest first.

**Container introspection (read-only):**
- `GET /api/v1/stacks/{id}/containers` — List containers managed for the stack
- `GET /api/v1/stacks/{id}/containers/{cid}` — Inspect a container (state/config/networks/mounts; env values redacted when the key looks like a secret)
- `GET /api/v1/stacks/{id}/containers/{cid}/logs` — Tail logs with `tail` (max 10000) / `since` (Go duration) / `timestamps` query params; stdout+stderr demuxed on non-TTY containers
- `GET /api/v1/stacks/{id}/containers/{cid}/logs/stream` — WebSocket upgrade, follows logs in real time. Same tail/since/timestamps surface as the one-shot endpoint. 30s ping cadence; normal-closure frame when the daemon stream ends. Authentication via X-API-KEY header on the upgrade request — browsers need a proxy or token flow.
- `GET /api/v1/stacks/{id}/containers/{cid}/stats` — One-shot resource sample: CPU% (docker-stats formula), memory (page cache subtracted), per-iface network, block I/O, PIDs. ~1s latency (daemon gathers two samples for a valid CPU delta).
- `GET /api/v1/stacks/{id}/containers/{cid}/stats/stream` — WebSocket upgrade, one ContainerStatsSample per daemon tick (~1s). First sample typically reports cpu.percent=0 until the daemon has a prior sample to diff against. Same ping/close semantics as logs/stream.

**Debug mutations (audited, use sparingly):**
- `POST /api/v1/stacks/{id}/containers/{cid}/restart` — Docker-level restart. Optional `?t=<seconds>` grace period (-1 = wait forever, 0 = immediate kill). Returns 202. Every call writes a `container.restart` audit entry regardless of outcome. Bypasses GitOps — for state changes commit to git + redeploy instead.
- `GET /api/v1/stacks/{id}/containers/{cid}/exec` — WebSocket exec. Query params: `cmd` (repeatable), `tty` (default true), `user`, `workdir`. Binary frames both directions; text frames from the client are JSON control messages (`{"type":"resize","rows":N,"cols":N}` → ExecResize). TTY mode copies output raw; non-TTY mode demuxes Docker's stdcopy framing server-side so clients see clean merged stdout+stderr either way. CloseNormalClosure with `exit_code=N` in the reason text. Two audit rows per session: `container.exec_start` (in_progress) on connect, `container.exec_end` (success if exit=0, failure otherwise) on disconnect.
- `GET /api/v1/stacks/{id}/events` — SSE stream of Docker events filtered to this stack's managed resources. Each event is projected from Docker's raw format with accelero-service / accelero-replica surfaced as first-class fields. 25s keepalive comment during quiet periods; default type filter is `container` (opt in to `network`/`volume`/`image` via `?types=`).

**Resource browsers (root-level, read-only):**
- `GET /api/v1/images` — images referenced by accelero-managed containers, with back-references (stack/service/replica/container). Walks containers to derive "managed" since images don't carry labels.
- `GET /api/v1/volumes` — volumes labelled managed-by=accelero.
- `GET /api/v1/volumes/{name}/browse` — list files in a managed volume or (with `?download=true`) stream one file's contents. Read-only; backed by an ephemeral busybox helper container mounted at `/volume:ro`. 10MB download cap, 10k-entry listing cap, `..` paths rejected, volumes.browse/read audited.
- `POST /api/v1/volumes/{name}/files?path=<p>&mode=<oct>` — write file bytes into a managed volume (emergency patch affordance). Gated behind `ALLOW_VOLUME_WRITES=true`; 403 otherwise. 10MB body cap, parent dirs auto-created, `..` rejected. Audited as `volume.write`.
- `GET /api/v1/networks` — networks labelled managed-by=accelero. **Pre-existing networks from older accelero versions are unlabelled** and won't appear until the stack is recreated (Docker won't add labels to live networks).
- All three accept `?stack=<name>` to narrow to one stack.
- All three **span every Docker host** (default + registered) and tag each result with a `host` field (`default` for the `DOCKER_SOCK` daemon). An unreachable host is skipped with a warn log; only an all-hosts failure is a 500. Volume **browse/write** is still default-host-only (see Multi-host under Key Features).

**Per-stack secrets:**
- `POST /api/v1/stacks/{id}/secrets` — upsert a secret `{name, value}`. Name must match `[A-Z_][A-Z0-9_]*` (≤128 chars), value is non-empty (≤64 KiB). Returns 201 on first write, 200 on rewrite (rotation). Value is encrypted at rest via the existing cipher. Audited as `stack.secret.set` with metadata `rewrote_existing`.
- `GET /api/v1/stacks/{id}/secrets` — list names + `created_at` / `updated_at`. Values are DELIBERATELY redacted; the list endpoint NEVER returns values.
- `DELETE /api/v1/stacks/{id}/secrets/{name}` — 204 on success, 404 if absent. Audited as `stack.secret.delete` in both outcomes (operators want the trail to include failed deletes during incidents).
- Secrets cascade-delete with their parent stack (explicit cleanup in `DeleteStack`; modernc.org/sqlite doesn't honour `_foreign_keys=ON` in the DSN so we don't rely on the FK).
- **Deploy-time injection:** secrets are merged into each managed container's `Env` slice at create time. Secrets shadow compose-file env on key collision (operator intent beats compose default), and overrides happen *in place* in the env slice — order is stable across unrelated changes. Rotation = `POST /secrets` then trigger a deploy: the new value takes effect on the next container create. Values never appear in deploy logs (only a `Injected N per-stack secret(s)` debug line).
- **Drift detection on rotation:** the deployer persists `SHA-256` of the secret set on every successful deploy as `stack.SecretsHash`. The reconciler hashes the *current* set on each cycle; a mismatch surfaces as a `secrets_changed` drift item (service `(secrets)`, action `recreate` in `/preview`). When auto-deploy is on, this triggers an automatic redeploy. The hash is computed once at the top of `Deploy` so every service in the rollout sees the same snapshot — a mid-deploy rotation is caught by the next reconcile rather than splitting one rollout across two secret sets. A `secrets_changed` deploy forces every container to be recreated even when image+health would otherwise allow a skip; without that, the new env values would never reach the container (Docker has no in-place env update).

**Per-stack registry credentials (multi-registry):**
- `POST /api/v1/stacks/{id}/registries` — upsert a registry credential `{server, username, password}`. `server` is a bare `host[:port]`; URL prefixes (`http(s)://`), spaces, and slashes are rejected at the API boundary. Password is encrypted at rest via the existing cipher. 201 on first write, 200 on rewrite.
- `GET /api/v1/stacks/{id}/registries` — list `server` / `username` / timestamps. **Passwords are redacted** — the deploy path is the only thing that ever reads them.
- `DELETE /api/v1/stacks/{id}/registries/{server}` — 204 / 404. Audited as `stack.registry.delete` in both outcomes.
- **Image pull credential selection:** the deployer derives a registry hostname from the image reference (`ghcr.io/foo/bar` → `ghcr.io`; bare `nginx:1.27` or `library/nginx` → `docker.io`) and picks the matching per-stack credential. Hub aliases (`docker.io`, `index.docker.io`, `registry-1.docker.io`, `registry.hub.docker.com`) are treated as equivalent so operators don't have to know which one `docker login` produced. If no per-stack credential matches, the legacy single-credential fields on the stack record are tried; if those don't match either, the pull is anonymous (which is the right thing for public images). Per-stack entries always win over the legacy fields when both match the same registry.

**Docker hosts (multi-host, admin-only — reads included):**
- `POST /api/v1/hosts` — Register a daemon `{name, endpoint, tls_ca?, tls_cert?, tls_key?}`. `endpoint` must be `unix://` or `tcp://` (`ssh://` unsupported); TLS fields are all-or-nothing. **Pinged before persisting** (unreachable → 400, audited as a failed `host.create`). Audited `host.create`.
- `GET /api/v1/hosts` — List hosts; TLS material never returned, only `tls_enabled`.
- `DELETE /api/v1/hosts/{id}` — Remove a host; **409 while any stack targets it** (`CountStacksOnHost`). Audited `host.delete` (failure audited too).
- `host_id` on stack create/update accepts a host **name or id** (resolved to canonical id via `Handler.resolveHostID`); `""` = default host.

**API keys (RBAC, admin-only):**
- `POST /api/v1/apikeys` — Create a role-scoped key `{name, role, stack_grants?}` (role ∈ none/viewer/operator/admin; `stack_grants` is an optional `{stack-name-or-id: role}` map resolved to canonical stack IDs, rejecting unknown stacks/roles). Returns the raw key **once** (`acc_<hex>`); only its SHA-256 hash is stored. Audited `apikey.create`.
- `GET /api/v1/apikeys` — List keys (name/role/timestamps; key + hash never returned).
- `DELETE /api/v1/apikeys/{id}` — Revoke a key. 204/404. Audited `apikey.delete` (failure audited too).

**Admin:**
- `POST /api/v1/admin/encrypt-existing` — one-shot migration that re-saves any stack whose `repo_token` or `docker_password` is still in pre-encryption plaintext. Requires `ACCELERO_ENCRYPTION_KEY`; returns 400 when encryption is disabled. Idempotent (second call returns `stacks_migrated: 0`). Audited as `admin.encrypt-existing`.
- `POST /api/v1/admin/restore` — upload a snapshot (plaintext `.db` or age `.db.age`) to replace the DB. Gated by `ALLOW_RESTORE`; validates + stages, applies on next restart (see `ALLOW_RESTORE` above). Audited as `admin.restore`.
- `POST /api/v1/admin/backup` — streams a consistent SQLite snapshot as a file download (`Content-Disposition: attachment; filename="accelero-backup-<UTC>.db"`). Produced via `Store.Backup` → SQLite `VACUUM INTO` (safe against the live WAL DB) into a temp dir, streamed, then cleaned up. The snapshot contains all persisted data incl. secrets (encrypted at rest only if `ACCELERO_ENCRYPTION_KEY` is set). Audited as `admin.backup` with `bytes` in metadata (500 + failure audit on error).

**Audit log (append-only):**
- `GET /api/v1/audit` — filters: stack (id or name), actor, operation, since (Go duration), limit (≤1000). Newest first. Immutable at the store layer — no write/update/delete path.
- Entries emitted today: stack.create/update/delete (actor api-key); deploy.start (api-key, in_progress; suppressed for approval-gated stacks); deploy.complete/failed/rolled_back (system:deployer, with duration + changes metadata); drift.detected (system:reconciler, one per cycle with drift, per-type counts in metadata); drift.auto_deployed (system:reconciler, when auto-deploy fires); approval.requested (system:deployer, in_progress) / approval.granted / approval.rejected (actor = key name) / approval.timed_out (system:deployer) for the approval-gate lifecycle; apikey.create / apikey.delete (RBAC key management); host.create / host.delete (multi-host; TLS material never in metadata). The **actor** on request-derived entries is now the authenticated key's name (bootstrap env key = `env-admin`), not the old `api-key` placeholder.
- Retention: time-based cleanup on the existing deployment-cleanup cadence. Keep 90 days by default (`AUDIT_MAX_AGE`); set to 0 to disable.

**Legacy:**
- `POST /webhook` — Trigger deployment (targets "default" stack or stack specified in payload)

**System:**
- `GET /health` — Health check, kept for backward compatibility (unauthenticated)
- `GET /healthz` — Kubernetes-style liveness probe, alias for `/health` (unauthenticated)
- `GET /readyz` — Kubernetes-style readiness probe: DB ping + Docker ping + shutdown-in-progress flag; 503 if any check fails (unauthenticated)
- `GET /status` — Stack summaries
- `GET /metrics` — Prometheus exposition (unauthenticated)

### Deployment Flow

1. API request triggers deployment for a stack
2. Repository cloned (shallow, depth=1) and compose path validated against directory traversal
3. Compose file parsed, services filtered, dependency graph resolved via topological sort
4. Pre-deployment state of ALL services captured before any mutation
5. Docker networks created
6. Services deployed in dependency order, each under per-service lock:
   - Image pulled (with per-stack registry credentials)
   - New container created with `managed-by=accelero` label
   - Health check polled (no unconditional sleep)
   - Old containers removed only after new one is healthy
7. On failure: rollback restores pre-deployment state using saved snapshots
8. Deployment record and container tracking updated in SQLite

### Reconciliation Flow

1. Per-stack goroutine runs on configurable interval
2. Clones repo, parses compose file (desired state)
3. Lists Docker containers filtered by `managed-by=accelero` + `accelero-stack=<name>` labels (actual state)
4. Compares: missing services, image mismatches, stopped/unhealthy containers, extra containers, missing networks
5. If `auto_deploy` is true and drift detected, triggers deployment — unless the stack's circuit breaker is open (see `CIRCUIT_BREAKER_*`)
6. Drift reports available via API for manual inspection

Both `active` and `error` stacks are reconciled (`reconciler.shouldReconcile`); reconciling `error` stacks lets a failed deploy self-heal once its repo is fixed, with the circuit breaker throttling retries. `paused`/`deploying` stacks are left alone.

### Testing Strategy

- `handler/webhook_test.go`: Tests stack CRUD API, legacy webhook, health endpoint using mock store/deployer; also covers the `/admin/encrypt-existing` endpoint (disabled-state 400, happy path, idempotency) and `/admin/backup` (streams octet-stream snapshot with attachment filename + Content-Length, success/failure audit with `bytes`)
- `store/sqlite_test.go`: `TestBackup_ProducesReadableSnapshot` — `VACUUM INTO` writes a non-empty file that opens as a valid store with the original data intact; `TestValidateBackupFile` — accepts a real Accelero snapshot, rejects junk (bad magic), a non-Accelero SQLite DB (missing `stacks`), and a missing file
- `handler/webhook_test.go` (restore): `AdminRestore` — disabled → 403 + audited; valid upload → 202 + staged file validates; junk → 400, nothing staged; age-encrypted upload decrypts + stages
- `backup/backup_test.go`: `RunOnce` writes a timestamped snapshot / creates the dir / propagates backup errors; `Prune` keeps newest N, `keep<=0` retains all, fewer-than-keep is a no-op, and it never touches non-`accelero-backup-*.db` files
- `backup/s3_test.go`: `S3Config.Enabled` (bucket required); `UploadFile` fails fast on a missing local file
- `backup/encrypt_test.go`: `NewEncryptor` nop vs age (Enabled/Ext); nop passes through; age produces a standard `age-encryption.org/v1` stream that round-trips with the passphrase and fails on a wrong one; `RunOnce` with a passphrase writes `.db.age` (no plaintext left in the dir) that decrypts to the snapshot; `Prune` matches `.db.age` files
- `handler/webhook_test.go` (backup): `AdminBackup` encrypted path — `.db.age` filename, `age`-stream body (no plaintext), decrypts back to the snapshot, audited success
- `service/service_test.go`: Tests ParseDuration and EnvVars unmarshaling
- `utils/utils_test.go`: Tests SplitServiceNames and ContainsServiceName
- `secrets/secrets_test.go`: AES-256-GCM round-trips, tamper detection, legacy plaintext passthrough, fail-closed when the key is missing, malformed-key handling in `LoadCipherFromEnv`
- `store/sqlite_test.go`: `TestEncryption_*` verifies DB columns actually hold `v1:` ciphertext (raw SQL) and that legacy plaintext rows stay readable after attaching a cipher
- `handler/multihost_test.go`: a stack with `host_id` is introspected on **that** host and not the default one; a default-host stack never resolves a host; unreachable host → 502; `/volumes` + `/networks` fan out with correct per-result `host` attribution; one failing host still returns the others; all hosts failing → 500; `hostClients()` collapses to the default host when multi-host isn't wired
- `dockerhost/manager_test.go`: empty host ID / nil lookup → default client; per-host lazy build + cache (lookup called once, distinct clients per host); unknown host and lookup error surface; partial TLS triple and garbage PEM rejected; `Close` drops the cache
- `store/sqlite_test.go`: `TestDockerHost_CRUD` (create/get-by-id/get-by-name/nil-miss/list/delete-true-then-false); `TestDockerHost_TLSKeyEncryptedAtRest` (raw column holds `v1:` ciphertext, read path decrypts); `TestStackHostID_PersistedAndCounted` (host_id round-trips, `CountStacksOnHost` reflects moves)
- `handler/hosts_test.go`: create validation (missing fields, non-unix/tcp scheme, partial TLS triple), duplicate name 409, unreachable host → 400 + not persisted + audited, list redacts all TLS material but exposes `tls_enabled`, delete 204→404, delete refused 409 while a stack targets the host, stack create resolves `host_id` by name and rejects an unknown host
- `rbac/rbac_test.go`: `Role.Satisfies` hierarchy (incl. unknown role grants nothing); `ParseRole` normalisation/validation; `RequiredRole` policy table (GET→viewer, mutations→operator, `/admin/*`+`/apikeys`→admin, `/exec`→operator); identity context round-trip; `none` role (valid, satisfies nothing); `EffectiveRole` (grant overrides base, base fallback, non-stack→base); `StackTokenFromPath`
- `middleware/auth_test.go`: env key → admin identity; missing→401; unknown→403; viewer blocked from mutations; operator blocked from `/admin/*` and `/apikeys`; `/exec` requires operator; lookup error→500; unauthorized log carries request_id; per-stack grants (operator on prod deploys, viewer on staging denied deploy, ungranted + base none denied, collection uses base role)
- `store/sqlite_test.go`: `TestAPIKey_CRUD` (create/get-by-hash/nil-miss/touch/list/delete-true-then-false, `acc_` prefix) and `TestGenerateAPIKey_Unique`
- `handler/apikeys_test.go`: create returns raw `acc_` key once + persists only the hash + audits `apikey.create`; name/role validation (400); list redacts the hash; delete 204 then 404 (both audited); `stack_grants` resolved name→canonical-id, unknown-stack and bad-grant-role rejected (400)
- `store/sqlite_test.go`: `TestAPIKey_StackGrantsRoundTrip` (grants JSON persists/reads back; no-grants key → empty map)
- `middleware/ratelimit_test.go`: disabled → identity middleware; burst-then-block with 429 + `Retry-After`; per-key isolation; refill over time (via injected clock); missing API key passes through
- `middleware/webhook_signature_test.go`: empty secret → identity middleware; valid HMAC passes and body is restored for the handler; missing/malformed/wrong-algo/wrong-length/tampered/wrong-secret all 401; empty body with empty-body signature passes; oversized body returns 413
- `middleware/hsts_test.go`: maxAge≤0 → identity middleware (no header); enabled sets `Strict-Transport-Security: max-age=<n>`; header value matches the configured number; no `includeSubDomains` / `preload` (left to edge config)
- `middleware/security_headers_test.go`: companion headers (`nosniff` / `X-Frame-Options: DENY` / `no-referrer`) always set — including when CSP is empty; CSP set verbatim when non-empty; CSP omitted when empty; next handler always invoked
- `retry/retry_test.go`: success-first-try (no retry); retries-then-succeeds; budget exhaustion returns last error with exact attempt count; `MaxAttempts<1` collapses to one try; `Permanent` short-circuits and unwraps to the original error; context-cancel error not retried; cancel between attempts stops the loop; already-cancelled ctx skips fn entirely; `backoffDuration` respects the `MaxDelay` cap under full jitter
- `gitutil/errors_test.go`: `IsPermanentCloneError` true for auth-required/failed, repo-not-found, reference-not-found (incl. wrapped); false for transient network errors and nil
- `breaker/breaker_test.go`: disabled (threshold 0) and nil breaker always allow + no-op; trips after N consecutive failures; success resets the streak; open blocks until cooldown then half-open; half-open success closes, half-open failure re-opens without re-reporting the trip; cooldown boundary; per-key isolation (all via an injected clock)
- `reconciler/reconciler_test.go`: `shouldReconcile` — active + error reconcile (error so failed stacks self-heal), paused + deploying do not
- `store/sqlite_test.go`: `TestStackSecret_*` covers upsert first-write / rewrite updated_at bump / delete-missing false / delete-existing true / cascade-on-stack-delete / encrypted-in-DB
- `handler/webhook_test.go` (secrets subsection): 201 first-write / 200 rewrite; invalid-name regex enforcement; empty-value rejected with DELETE hint; unknown stack 404; list redacts values (no `value` field in JSON); delete-success audited as success; delete-missing audited as failure with `not found`
- `stack/secrets_test.go`: `mergeSecretsIntoEnv` — no-secrets passthrough; appends new keys preserving compose order; secret overrides compose value *in place*; bare-key compose entries (`HOME`) get replaced with explicit values when a matching secret exists; source slice is not mutated; empty-value secret still overrides
- `store/secret_hash_test.go`: `HashStackSecrets` — empty set → empty string (matches zero-value SecretsHash on fresh stacks); stable under input ordering; rotation produces a different hash; length-prefixing prevents `(name="AB", value="CD")` colliding with `(name="A", value="BCD")`; add/remove a key changes the hash
- `stack/approval_test.go`: the approval gate without Docker — `Deploy` on a `requires_approval` stack holds (`pending_approval`, stack stays `active`, nothing executes); repeated triggers dedupe to one open approval; `RejectDeployment` sets rejected + errors on a non-pending target; `ApproveDeployment` errors on non-pending / unknown ids; `ExpirePendingApprovals` rejects only approvals older than maxAge (0 disables), leaving fresh ones
- `store/sqlite_test.go`: `TestStackRequiresApproval_Persisted` (requires_approval round-trips through INSERT/UPDATE/scan); `TestPendingApprovals` (`GetPendingApproval` dedupe + nil when none, `ListPendingApprovals` oldest-first excluding non-pending)
- `handler/webhook_test.go` (approvals): approve happy-path (202 + `approval.granted` audit) / unknown 404 / non-pending 409; reject happy-path (200 + `approval.rejected` audit, body shows rejected) / non-pending 409; `GET /approvals` lists the queue
- `notify/notify_test.go`: `New(Config{})` disabled with no URLs / enabled with any single sink; webhook sink gets the full Event JSON, Slack `{"text":...}`, Discord `{"content":...}`, Teams a `MessageCard` (all against `httptest.Server`); `email_test.go` covers `EmailConfig.enabled()`, `New` enabling the email sink only on a complete config, RFC5322 header/body construction (`buildEmail`), and `smtpSink.Send` invoking an injected transport; `WrapRecorder` notifies only on deploy/drift ops while persisting *all* entries through to the base recorder; `WrapRecorder` returns the base unchanged when the notifier is disabled; `eventFromAudit` emits the right emoji + backtick-quoted stack for each notable op and returns `ok=false` for others
- `stack/registry_match_test.go`: `registryFromImage` (Hub-default + `.`/`:`/`localhost` first-segment detection); `pickRegistryAuth` — explicit per-stack match; anonymous fallback when no match (server still set from image); legacy single-credential fallback; explicit wins over legacy on conflict; Hub aliases (`docker.io` ↔ `index.docker.io` ↔ `registry-1.docker.io`); empty legacy username treated as absent
