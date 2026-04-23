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
- **`internal/middleware/`**: HTTP middleware — API key authentication with constant-time comparison, and Prometheus metrics recorder (request count + latency, labelled by mux route template so `/stacks/{id}` doesn't explode cardinality)
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
- **Deploy preview**: `POST /api/v1/stacks/{id}/preview` is a read-only dry run that reuses the reconciler's drift check and maps each drift item to the action the next deploy would take (`create`, `recreate`, `restart`, `remove`, `error`).

### Environment Variables

Required:
- `API_KEY`: **REQUIRED** — Secure API key for authentication

Optional:
- `SERVER_PORT`: HTTP server port (default: 8000)
- `DOCKER_SOCK`: Docker socket path (default: unix:///var/run/docker.sock)
- `DATABASE_PATH`: SQLite database path (default: ./data/accelero.db)
- `STACKS_DATA_DIR`: Root directory for per-stack cloned repos (default: ./data/stacks). Used as the bind-mount source when a compose service references `./path/from/repo`. When Accelero runs in Docker, this path must be the same inside and outside the container — bind-mount the host dir at the same path.
- `ALLOW_VOLUME_WRITES`: Set to `true` to enable `POST /volumes/{name}/files` (emergency file write into a managed volume). Default `false`. Every call is audited as `volume.write` regardless of outcome.
- `LOG_LEVEL`: Logging level (default: info)
- `LOG_FORMAT`: Log format — "json" or "text" (default: text)
- `WORKER_COUNT`: Worker goroutine count override (default: 2 * CPU cores, min: 2, max: 50)
- `QUEUE_SIZE`: Task queue buffer size override (default: 15 * workers, min: 50, max: 1000)
- `STATUS_CLEANUP_INTERVAL`: Deployment record cleanup interval (default: 1h) — shared with audit cleanup
- `STATUS_MAX_AGE`: Max deployment record age (default: 24h)
- `AUDIT_MAX_AGE`: Max audit entry age before cleanup (default: 90 days). Set to `0` to disable retention (useful for compliance contexts that require indefinite retention)
- `ACCELERO_ENCRYPTION_KEY`: Base64-encoded 32-byte master key. When set, `repo_token` and `docker_password` are encrypted at rest in SQLite with AES-256-GCM (versioned ciphertext format `v1:<nonce>:<ct>`). Legacy plaintext rows are read transparently; migrate them via `POST /api/v1/admin/encrypt-existing`. Unset = plaintext (dev default, logged as a warning).
- `RATE_LIMIT_RPS`: Sustained refill rate (tokens/second) for the per-API-key token bucket. Default `0` disables rate limiting entirely. Set to e.g. `10` to allow 10 req/s with the burst below.
- `RATE_LIMIT_BURST`: Bucket capacity — how many requests a quiescent caller can send at once before throttling kicks in. Defaults to `max(2*RATE_LIMIT_RPS, 10)` when RPS is set but burst isn't. Ignored when `RATE_LIMIT_RPS<=0`.

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

**Admin:**
- `POST /api/v1/admin/encrypt-existing` — one-shot migration that re-saves any stack whose `repo_token` or `docker_password` is still in pre-encryption plaintext. Requires `ACCELERO_ENCRYPTION_KEY`; returns 400 when encryption is disabled. Idempotent (second call returns `stacks_migrated: 0`). Audited as `admin.encrypt-existing`.

**Audit log (append-only):**
- `GET /api/v1/audit` — filters: stack (id or name), actor, operation, since (Go duration), limit (≤1000). Newest first. Immutable at the store layer — no write/update/delete path.
- Entries emitted today: stack.create/update/delete (actor api-key); deploy.start (api-key, in_progress); deploy.complete/failed/rolled_back (system:deployer, with duration + changes metadata); drift.detected (system:reconciler, one per cycle with drift, per-type counts in metadata); drift.auto_deployed (system:reconciler, when auto-deploy fires).
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
5. If `auto_deploy` is true and drift detected, triggers deployment
6. Drift reports available via API for manual inspection

### Testing Strategy

- `handler/webhook_test.go`: Tests stack CRUD API, legacy webhook, health endpoint using mock store/deployer; also covers the `/admin/encrypt-existing` endpoint (disabled-state 400, happy path, idempotency)
- `service/service_test.go`: Tests ParseDuration and EnvVars unmarshaling
- `utils/utils_test.go`: Tests SplitServiceNames and ContainsServiceName
- `secrets/secrets_test.go`: AES-256-GCM round-trips, tamper detection, legacy plaintext passthrough, fail-closed when the key is missing, malformed-key handling in `LoadCipherFromEnv`
- `store/sqlite_test.go`: `TestEncryption_*` verifies DB columns actually hold `v1:` ciphertext (raw SQL) and that legacy plaintext rows stay readable after attaching a cipher
- `middleware/ratelimit_test.go`: disabled → identity middleware; burst-then-block with 429 + `Retry-After`; per-key isolation; refill over time (via injected clock); missing API key passes through
