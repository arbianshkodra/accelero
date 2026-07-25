# Accelero Roadmap

## Vision

Accelero is the **ArgoCD/Flux equivalent for Docker Compose environments**.

Git is the single source of truth. Every deployment, every configuration change, flows through a git commit. Accelero continuously reconciles the desired state in git against the actual state of your Docker host(s) and corrects drift automatically.

Unlike Portainer (UI-first, click-to-deploy, imperative), Accelero is **git-first and declarative**. We adopt operational tooling from Portainer (logs, stats, observability, RBAC, audit) where it supports GitOps workflows, and skip features that encourage state drift (in-UI container edits, image building, imperative changes).

## Design Principles

1. **Git is the source of truth.** Nothing modifies desired state except git commits.
2. **Reconciliation over imperative.** Accelero watches, compares, converges.
3. **Observability without mutation.** View logs, stats, containers, volumes — but don't edit. To change state, change git.
4. **Debug operations are exceptional.** `exec` and write access to volumes exist for incident response, but are always audit-logged and flagged as drift.
5. **Labeled resources only.** Accelero only manages what it created (`managed-by=accelero`). Never touch the user's other Docker resources.
6. **Single binary, zero ops overhead.** SQLite for state, no external dependencies beyond Docker.

---

## Completed

### Phase 0 — Bug Fixes & Foundation
- [x] Fix rollback capturing post-failure state instead of pre-deployment state
- [x] Unify inconsistent `containsServiceName` implementations (strict prefix match)
- [x] Fix `env_file` references broken by git clone cleanup deleting all non-compose files
- [x] Add per-service deployment locking to prevent concurrent deploys of same service
- [x] Propagate context through all Docker/git operations for cancellation and timeouts
- [x] Add graceful shutdown coordination (WaitGroup for in-flight deployments)
- [x] Scope Docker resource cleanup to `managed-by=accelero` labeled resources only
- [x] Validate COMPOSE_PATH against directory traversal
- [x] Remove unconditional 5-second sleep in health check polling
- [x] Use shallow git clones (depth=1) and suppress stdout output
- [x] Reuse Docker client instead of creating redundant clients per deployment
- [x] Close Docker client on shutdown

### Phase 1 — Multi-Stack Architecture
- [x] Centralized config package (environment variables with defaults)
- [x] SQLite persistence layer (stacks, deployments, managed containers)
- [x] Stack deployer with dependency resolution (topological sort, cycle detection)
- [x] GitOps reconciliation engine (per-stack drift detection, auto-deploy)
- [x] REST API for stack CRUD, deployment triggers, drift checks
- [x] Legacy env-var backward compatibility (auto-migrates to "default" stack)
- [x] Configurable server port
- [x] Container labeling (`managed-by=accelero`, `accelero-stack=<name>`)
- [x] CI/CD pipeline (split lint/test on PRs, build/release on tags, docs deploy)

### Phase 2 — Compose Compatibility & Core Observability

Goal: accept any reasonable real-world compose file, and surface enough runtime state to diagnose a deployment.

**Docker Compose compatibility (high impact):**
- [x] `.env` variable substitution (`${VAR}`, `${VAR:-default}`, `${VAR:?error}`, `${VAR:+value}`, `$$`) — next-to-compose and repo-root lookup
- [x] `entrypoint`, `working_dir`, `user` fields
- [x] `stop_grace_period` (replaces the old hardcoded 10s)
- [x] `stop_signal`
- [x] `depends_on` map form with conditions (`service_started`, `service_healthy`, `service_completed_successfully`)
- [x] `deploy.replicas` for container scaling (rolling deploys, scale up/down, rejects replicas>1 with static published ports)
- [x] Named volumes (top-level `volumes:` section with scoped naming, idempotent `docker volume create`, `external: true` verification, drift detection — never auto-delete)
- [x] `logging` driver and options
- [x] Long-form `ports` syntax (`{target: 80, published: 8080, protocol: tcp, host_ip: 127.0.0.1}`); also fixes protocol and host-IP handling in short form

**Docker Compose compatibility (medium impact):**
- [x] `extra_hosts`, `hostname`, `domainname`, `dns`, `dns_search`
- [x] `cap_add`, `cap_drop`, `privileged`
- [x] `tmpfs`, `shm_size`, `init`
- [x] `expose` (expose ports without publishing)
- [x] `pull_policy` (`always` / `missing` / `if_not_present` / `never`; `build` rejected)

**Observability basics:**
- [x] Structured request/deployment ID propagation through all log entries (request_id attached by middleware; deploy/reconcile/stack enrich via logctx; security events in auth middleware carry request_id)
- [x] Prometheus metrics endpoint (`/metrics`) — deployments count, duration, failures, drift events, active reconcile loops
- [x] Deployment diff preview (`POST /stacks/{id}/preview` — show what would change without deploying)
- [x] `/healthz` and `/readyz` endpoints (distinct from `/health`)

### Phase 3 — GitOps Operator Experience

Goal: operators can observe, debug, and audit GitOps-managed workloads without needing separate tools like `docker logs`, `docker stats`, or SSH to the host.

**Read-only runtime introspection (no mutation):**
- [x] `GET /stacks/{id}/containers` — list containers with status, labels, image, ports, replica index
- [x] `GET /stacks/{id}/containers/{cid}` — inspect container details (state, config, networks, mounts; env redacted by key)
- [x] `GET /stacks/{id}/containers/{cid}/logs` — one-shot tail with `tail` / `since` / `timestamps`; follow via WebSocket is a separate ticket below
- [x] `GET /stacks/{id}/containers/{cid}/logs/stream` — WebSocket follow; stdout+stderr demuxed server-side, 30s ping, normal-closure on daemon EOF, X-API-KEY auth on upgrade
- [x] `GET /stacks/{id}/containers/{cid}/stats` — CPU%, memory used/limit/%, per-iface network rx/tx, block I/O totals, PIDs (snapshot).
- [x] `GET /stacks/{id}/containers/{cid}/stats/stream` — WebSocket follow: one ContainerStatsSample per daemon tick (~1s). Same ping/close semantics as logs/stream.
- [x] `GET /stacks/{id}/events` — SSE stream of Docker events filtered to this stack's managed resources (accelero-service / accelero-replica surfaced). Stack-wide view is the common one; a daemon-wide `/events` variant across all stacks can come later if needed.

**Resource browsers (read-only, filtered to accelero-managed):**
- [x] `GET /api/v1/images` — images referenced by managed containers with back-references (stack/service/replica/container). `?stack=<name>` narrows.
- [x] `GET /api/v1/volumes` — volumes labelled managed-by=accelero with driver/mount/labels. `?stack=<name>` narrows.
- [x] `GET /api/v1/volumes/{name}/browse` — read-only file browser backed by an ephemeral busybox helper. List directory contents (10k-entry cap) or `?download=true` to stream a single file (10MB cap). `..` paths rejected. Audited as `volume.browse` / `volume.read`.
- [x] `GET /api/v1/networks` — networks labelled managed-by=accelero with driver/scope/options. Also fixes a pre-existing bug where networks created by accelero were missing management labels. `?stack=<name>` narrows.

**Exceptional debug operations (audited, flagged as drift):**
- [x] `POST /api/v1/stacks/{id}/containers/{cid}/restart` — Docker-level restart, optional `?t=` grace period. Always audited (`container.restart` entry) even on Docker-side failure. Pure GitOps reminder: for config/image/replica changes, commit to git + redeploy instead.
- [x] `GET /api/v1/stacks/{id}/containers/{cid}/exec` — WebSocket exec. cmd/tty/user/workdir query params; binary frames both directions; text frames from client = JSON control messages (`{"type":"resize","rows":N,"cols":N}` wired to ExecResize). TTY mode raw-copies output; non-TTY mode demuxes Docker's stdcopy framing server-side. CloseNormalClosure with `exit_code=N` in the reason. Two audit rows per session (`container.exec_start` + `container.exec_end` with outcome/exit_code/duration).
- [x] `POST /api/v1/volumes/{name}/files?path=<p>&mode=<oct>` — emergency file write. Off by default; opt in via `ALLOW_VOLUME_WRITES=true`. Same helper-container pattern as the browser, mounted RW for the single call. 10MB body cap, parent dirs auto-created, `..` rejected, audited as `volume.write` with path/size/mode metadata.

**Audit log:**
- [x] Persistent audit log in SQLite — every notable write / system event with timestamp, actor, operation, resource, outcome, metadata
- [x] `GET /api/v1/audit` with filtering by stack (id or name), actor, operation, since, limit
- [x] Audit entries for: stack CRUD; deploy start/complete/failed; drift detected (one per cycle, per-type counts in metadata); drift auto-deployed. Exec sessions / container restarts will hook the same recorder when those endpoints land.
- [x] Immutable at the store layer — only Create + List exposed, no update/delete path
- [x] Time-based retention — `AUDIT_MAX_AGE` (default 90d; `0` disables). Runs on the existing `STATUS_CLEANUP_INTERVAL` cadence (default hourly).

---

## In Progress

### Phase 4 — Security & Multi-Tenancy

Goal: run Accelero in team/enterprise environments with multiple users, scoped permissions, and encrypted secrets.

**Identity & access:**
- [ ] User accounts with password auth (bcrypt)
- [ ] Multiple API tokens per user (named, revocable, scoped)
- [ ] OAuth2/OIDC integration (GitHub, Google, generic)
- [ ] Session-based auth for the UI
- [ ] LDAP integration (optional, via plugin)

**RBAC:**
- [x] Roles: `admin` ⊇ `operator` ⊇ `viewer`. Named, role-scoped API keys stored hashed (SHA-256) in SQLite; the `API_KEY` env stays as the bootstrap admin key. Per-request authorization by method+path policy (`internal/rbac.RequiredRole`): GET → viewer, mutations → operator, `/admin/*` + `/apikeys` → admin, `/exec` → operator. Managed via `POST/GET/DELETE /api/v1/apikeys` (admin-only; raw key shown once). Audit actor is now the key name. Enforced in `middleware.NewAPIKeyAuth`.
- [x] Per-stack permissions (team X can deploy stack A but only view stack B) — API keys carry a base role plus optional per-stack grants (`stack_grants: {stack: role}`); the effective role for a stack request is the grant or the base role. A `none` base makes a strictly-scoped key. Grants stored by canonical stack ID; the auth middleware resolves the URL stack token and applies `Identity.EffectiveRole`.
- [ ] Team grouping
- [ ] Environment-based scoping (`prod` stacks require `admin` role)

**Secrets — guiding principle:** the interpolation `.env` in a GitOps repo is for declarative config only (tags, registries, ports, flags) and is committed to git. Actual secrets must never land there. The items below give secrets a first-class home that does not require committing anything sensitive.

**Secrets at rest (inside Accelero):**
- [x] Encrypt `repo_token` and `docker_password` in SQLite with AES-256-GCM, versioned ciphertext (`v1:<nonce>:<ct>`), master key from `ACCELERO_ENCRYPTION_KEY` (base64-encoded 32 bytes). Legacy plaintext rows read transparently. User-password encryption will ride on the identity work above.
- [x] File-path master key source — `ACCELERO_ENCRYPTION_KEY_FILE` points at a file whose contents are the base64-encoded key. Docker/K8s-secret friendly; trailing whitespace trimmed; empty/missing file is a startup error. Setting both the file and the inline env var is rejected.
- [ ] KMS master key sources (AWS KMS, GCP KMS, HashiCorp Vault Transit).
- [ ] Automatic key rotation: new writes use the current key; old reads transparently re-encrypt on next write.
- [x] Online migration path from existing plaintext rows — `POST /api/v1/admin/encrypt-existing` re-saves any row still in plaintext so the cipher kicks in on write. Idempotent; 400 if the server has no key attached.

**Per-stack secrets API:**
- [x] `POST/GET/DELETE /api/v1/stacks/{id}/secrets` — upsert / list (names + timestamps only; values redacted) / delete. Name matches POSIX env-var grammar (`[A-Z_][A-Z0-9_]*`). Values encrypted at rest via the existing cipher; raw DB column holds `v1:<nonce>:<ct>`. Secrets cascade-delete with their parent stack.
- [x] Secrets injected into managed containers at deploy time. Implementation merges the stack's secrets into each container's `Env` slice at create time (rather than the originally-planned tmpfs `env_file` mount — same effective security boundary, simpler surface, no host-fs interaction). Secrets shadow compose-file env on key collision; override happens in place to keep env order stable across rotations. Values never logged.
- [x] Drift detection on secret rotation. Successful deploys persist a SHA-256 of the secret set on the stack record; the reconciler hashes the current set on each cycle and emits `secrets_changed` drift when they differ. Auto-deploy stacks redeploy automatically; the redeploy forces every container to be recreated so the new env values land (Docker has no in-place env update). `/preview` shows `action: recreate` for the `(secrets)` pseudo-service.
- [x] Audit log entry for every secret set/delete, with the actor and the key name (never the value). Delete-missing is audited as a failure so the trail captures attempted cleanup.

**External secret backends:**
- [ ] HashiCorp Vault (KV v2 + dynamic secrets) — short-lived token or AppRole auth.
- [ ] AWS Secrets Manager + IAM role or access key.
- [ ] GCP Secret Manager + service account.
- [ ] Azure Key Vault (lower priority).
- [ ] Kubernetes ExternalSecrets-style abstraction so backends are pluggable.

**Secret references in compose:**
- [ ] `${secret:vault:path/to/key}` / `${secret:aws:arn/...}` / `${secret:stack:MY_KEY}` syntax resolved at deploy time, never substituted back into the compose file on disk.
- [ ] Resolution happens after `${VAR}` interpolation so plain-text defaults cannot accidentally expose secret names.
- [ ] A `secrets:` section on the Stack resource maps short references to backend-specific paths, so the compose stays portable.
- [ ] Cache + TTL per backend so a failing backend doesn't break deploys longer than necessary.

**Secrets UI / CLI ergonomics:**
- [ ] `accelero secrets set/get/list/rotate` CLI for day-to-day ops.
- [ ] Web UI "mask on display, reveal on click" for existing values; new values are write-only.
- [ ] Drift report flags secrets referenced in compose but not resolvable at reconcile time (vs. a noisy deploy failure).

**Transport & network security:**
- [x] Native TLS support via `TLS_CERT_FILE` + `TLS_KEY_FILE` (PEM-encoded cert/chain + key). Both must be set together — only one is a fatal startup error rather than a silent downgrade. HSTS header (`Strict-Transport-Security: max-age=<TLS_HSTS_MAX_AGE>`) emitted on every response while TLS is on; default 1 year, `0` disables. Accelero deliberately omits `includeSubDomains`/`preload` — those are edge-level decisions. Let's Encrypt / auto-renewal is intentionally out of scope; cert provisioning is a host-level concern. Operators who want it should put Caddy / Traefik / nginx in front and leave TLS_* unset.
- [x] Rate limiting on API endpoints — per-API-key token bucket, applied after authentication. Configurable via `RATE_LIMIT_RPS` / `RATE_LIMIT_BURST`; disabled by default. Rejected requests are 429 with `Retry-After` + JSON body, counted in `accelero_rate_limited_requests_total` by route template.
- [ ] CSRF protection for session-based UI
- [x] Security response headers — configurable `Content-Security-Policy` via `CONTENT_SECURITY_POLICY` (default `default-src 'none'; frame-ancestors 'none'`, since Accelero serves no UI; explicit empty string omits it for edge-proxy setups). Always sends `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, and `Referrer-Policy: no-referrer` — valid over HTTP and HTTPS, so unconditional (unlike HSTS).

**Registry management:**
- [x] Multi-registry support with encrypted credentials at rest. `POST/GET/DELETE /api/v1/stacks/{id}/registries`; passwords encrypted via the existing cipher (same `v1:<nonce>:<ct>` format as `repo_token`); list endpoint redacts passwords. Deployer picks the credential by matching the image reference's registry hostname; Hub aliases (`docker.io` / `index.docker.io` / `registry-1.docker.io` / `registry.hub.docker.com`) are treated as equivalent. Legacy single-credential fields on the stack record remain supported and are tried as a fallback. No matching credential = anonymous pull.
- [x] Generic HMAC-SHA256 webhook signature verification via `WEBHOOK_SECRET` and `X-Hub-Signature-256` (GitHub / Gitea / Gogs / CI format). Replaces the API-key check on `/webhook` so external senders can authenticate without smuggling the API key; rejections bump `accelero_webhook_signature_rejected_total`; body capped at 1 MiB. Registry-specific wire formats (Docker Hub, Harbor, ECR) are still open.
- [ ] ECR/GCR/ACR IAM-based authentication
- [ ] Browse registry tags (for UI dropdown / approval workflows)

---

### Phase 5 — Operations & Integrations

Goal: make Accelero production-grade for teams that need notifications, approvals, and disaster recovery.

**Notifications:**
- [x] Slack incoming-webhook integration (`NOTIFY_SLACK_WEBHOOK_URL`, `{"text": ...}`), Discord (`NOTIFY_DISCORD_WEBHOOK_URL`, `{"content": ...}`), and Microsoft Teams (`NOTIFY_TEAMS_WEBHOOK_URL`, MessageCard). Each sink fires independently; `notify.New` takes a `notify.Config` struct.
- [x] Generic webhook (POST full Event JSON to `NOTIFY_WEBHOOK_URL`). Best-effort, async, never blocks/fails a deploy. Implemented in `internal/notify` by wrapping the shared audit recorder — deployer/reconciler untouched.
- [x] Email (SMTP) — `NOTIFY_SMTP_*` / `NOTIFY_EMAIL_*`; one plain-text message per event. Implicit TLS (465) / STARTTLS / plaintext, optional PLAIN auth. Stdlib `net/smtp`, no new deps.
- [x] Event types: deploy started/completed/failed/rolled-back, drift detected, auto-deploy triggered. (Approval-requested pending the approval-gates work.)
- [ ] Per-stack notification routing (stack A → #prod channel, stack B → email)
- [ ] Notification templates (customizable content)

**Approval gates:**
- [x] Stacks can require approval before deploy (`requires_approval: true`). Every trigger (manual, webhook, reconcile auto-deploy) creates a `pending_approval` deployment and notifies instead of running; at most one open approval per stack (deduped, so a reconcile loop doesn't spawn one per cycle). Implemented as a gate at the top of `Deployer.Deploy` (`requestApproval` / shared `runDeployment`).
- [x] Approval via API — `POST /stacks/{id}/deployments/{deployId}/approve` (runs it) / `/reject` (optional `{"reason"}`); `GET /approvals` lists the cross-stack queue. Slack interactive message / UI button remain follow-ups.
- [x] Approval timeouts (auto-reject after the wait) — `APPROVAL_TIMEOUT` (default 24h, 0 disables); a background sweep rejects expired approvals as `approval.timed_out`.
- [x] Approval audit trail — `approval.requested` / `granted` / `rejected` / `timed_out`, each also delivered as a notification.

**Reliability:**
- [x] Retry with exponential backoff: image pulls, git clones, network operations. Bounded backoff + full jitter via `internal/retry` (`retry.Do` + `retry.Permanent`), wired into both the deploy and reconcile paths. Non-retryable failures (registry auth/not-found, git auth/repo-not-found/bad-branch) fail fast rather than burning the budget. Configurable via `RETRY_MAX_ATTEMPTS` / `RETRY_BASE_DELAY` / `RETRY_MAX_DELAY` (defaults 3 / 1s / 30s; attempts=1 disables).
- [ ] Configurable retry budget *per operation type* (today one global policy covers pulls/clones/network ops)
- [x] Circuit breaker for repeatedly-failing stacks. The reconciler now also reconciles `error`-status stacks (not just `active`), so a failed deploy self-heals once its repo is fixed; a per-stack breaker (`internal/breaker`) throttles those retries — after `CIRCUIT_BREAKER_THRESHOLD` consecutive failures it trips open, skips auto-deploys for `CIRCUIT_BREAKER_COOLDOWN`, then allows a half-open trial. Manual deploys are never gated. Trips audited (`stack.circuit_breaker.opened`) and metered (`accelero_circuit_breaker_tripped_total`, `accelero_auto_deploys_skipped_total`). Also hardened `runLoop` so a transient DB error at loop start no longer permanently kills a stack's reconcile loop.

**Backup & disaster recovery:**
- [x] `POST /admin/backup` — one-shot consistent SQLite snapshot (`VACUUM INTO`, safe against the live WAL DB) streamed as a file download; audited as `admin.backup`. Restore is a file swap at `DATABASE_PATH`.
- [x] Scheduled backups to a local path — `BACKUP_INTERVAL` runs a consistent snapshot on start and every interval into `BACKUP_DIR`, retaining the newest `BACKUP_KEEP` (disabled by default). Reuses the same `VACUUM INTO` snapshot as `/admin/backup`.
- [x] Remote backup destinations (S3-compatible) — `BACKUP_S3_*` uploads each scheduled snapshot to any S3 API endpoint (AWS S3, Cloudflare R2, MinIO, Backblaze B2, GCS interop) after it's written locally; best-effort, remote retention via bucket lifecycle. Verified live against MinIO. (Native GCS / SCP transports remain possible follow-ups.)
- [x] Backup encryption — `BACKUP_ENCRYPTION_PASSPHRASE` age-encrypts every snapshot (scheduled + `/admin/backup`) as a standard age stream (`.db.age`), decryptable anywhere with `age -d`. Whole-file wrap via `filippo.io/age`, independent of the field-level `ACCELERO_ENCRYPTION_KEY`.
- [x] Restore endpoint — `POST /admin/restore` (gated by `ALLOW_RESTORE`) uploads a snapshot (plaintext or age `.db.age`), decrypts + validates it (`integrity_check` + `stacks` table), and stages it; the swap happens safely on the next restart, preserving the prior DB as `<db>.pre-restore-<ts>`.

**Templates (git-first, not in-app):**
- [ ] Curated list of public GitOps-ready sample repos (fork-to-deploy pattern)
- [ ] `POST /stacks/from-template` — create a stack from a template URL

---

## Planned

### Phase 6 — Web UI

Goal: a browser UI that makes GitOps concrete. Not a Portainer clone — a GitOps-native dashboard.

**Core views:**
- [ ] Dashboard: stack overview with drift status, deployment activity, reconciliation timeline
- [ ] Stack detail: desired state (from git) vs actual state (from Docker), diff view, deployment history
- [ ] Drift visualization: tree/graph showing which services are drifted and why
- [ ] Deployment timeline: waterfall of services coming up, health checks, rollback events
- [ ] Live log viewer with search, filter, multi-container tail
- [ ] Container stats dashboard (CPU/mem/network/I/O graphs)
- [ ] Audit log viewer with filters

**GitOps-specific UX:**
- [ ] Stack creation wizard: instead of writing compose in-UI, walks user through "fork this template, commit, we'll reconcile from there"
- [ ] "Edit stack in git" buttons link directly to the compose file in the repo
- [ ] PR-aware deploys (show open PRs that would change a stack if merged)
- [ ] Commit-diff view showing what compose changes triggered a deploy

**Implementation:**
- [ ] Single-page app embedded in the Go binary (no separate server)
- [ ] Static assets built at release time, served by Go HTTP
- [ ] WebSocket for live updates (logs, stats, reconciliation events)
- [ ] Mobile-responsive

---

### Phase 7 — Multi-Host & Agents

Goal: manage multiple Docker hosts from a single Accelero control plane.

- [x] **Direct multi-host (no agent)** — register Docker daemons via `POST /api/v1/hosts` (`unix://` / `tcp://`, optional mutual TLS with `tls_key` encrypted at rest) and target one per stack with `host_id`. The `DOCKER_SOCK` daemon remains the implicit default host, so existing stacks are unaffected. Clients are built lazily and cached per host (`internal/dockerhost.Manager`); deploys, reconciliation, drift, and `/preview` run against the stack's host. Host registration pings before persisting, and deleting an in-use host is refused. Admin-only surface. *(`ssh://` endpoints still pending.)*
- [x] Container introspection + root browsers + cleanup loop are host-aware — stack-scoped endpoints (`/containers`, `logs`, `stats`, `exec`, `events`, `restart`) follow the stack's host (unreachable host → 502); `/images`, `/volumes`, `/networks` fan out across the default host plus every registered host with a `host` field on each result (one dead host is skipped, all-dead is a 500); the cleanup sweep prunes every host. *Volume browse/write remains default-host-only — the helper container runs there and volume names aren't unique across hosts.*
- [ ] `ssh://` host endpoints
- [ ] Lightweight Accelero agent binary per Docker host
- [ ] Central controller orchestrates agents (mTLS between controller and agents)
- [ ] Environment grouping: hosts grouped as `prod-us-east`, `staging`, etc.
- [ ] Rolling deployments across hosts (canary → subset → full)
- [ ] Edge agents (polling-based, for hosts behind firewalls)
- [ ] Host health monitoring
- [ ] Per-environment stack scoping
- [ ] Disaster failover: if host dies, re-deploy stacks on another host in the same group

---

## Explicitly Out of Scope

These are Portainer/other-tool features that conflict with Accelero's GitOps philosophy or domain:

**State drift-inducing features (git must be the only way to change state):**
- In-UI container configuration editing — any change must be a git commit
- In-UI compose file editing — use the git editor
- Creating containers outside stacks — everything is stack-managed
- Duplicating/cloning containers via UI — clone the git repo instead

**Outside Docker Compose domain:**
- Kubernetes support — ArgoCD and Flux already own this
- Docker Swarm orchestration — Swarm is on maintenance mode
- Nomad support — different tool, different problem
- Podman Pods — possible future consideration

**CI responsibilities (not a deployment tool's job):**
- Image building from Dockerfile
- Image pushing to registries
- Test execution before deploy (use CI gates instead)
- Artifact signing (use CI + cosign)

**Host/OS management:**
- Firewall configuration
- OS-level package management
- SSH key management
- Host system monitoring (use Prometheus/Grafana)

**Marketplace/ecosystem:**
- In-app template marketplace — templates should be git-forkable repos
- Closed paid tier features — Accelero is open source, end to end

---

## Post-Roadmap Ideas (Phase 8+)

Too early to commit, but tracked:

- Progressive delivery (canary, blue-green, shadow traffic)
- SLO-based automatic rollback (if error rate > X, rollback)
- Cost tracking per stack
- Image vulnerability scanning integration (Trivy, Grype) as a gate
- Git-native approval PR workflow (require PR approval before reconciler deploys)
- GitHub Actions / GitLab CI integration that queries Accelero drift status
- OpenTelemetry traces for the full deploy pipeline
- GraphQL API alongside REST
