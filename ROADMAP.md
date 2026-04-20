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

---

## In Progress

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

---

## Planned

### Phase 3 — GitOps Operator Experience

Goal: operators can observe, debug, and audit GitOps-managed workloads without needing separate tools like `docker logs`, `docker stats`, or SSH to the host.

**Read-only runtime introspection (no mutation):**
- [x] `GET /stacks/{id}/containers` — list containers with status, labels, image, ports, replica index
- [x] `GET /stacks/{id}/containers/{cid}` — inspect container details (state, config, networks, mounts; env redacted by key)
- [x] `GET /stacks/{id}/containers/{cid}/logs` — one-shot tail with `tail` / `since` / `timestamps`; follow via WebSocket is a separate ticket below
- [x] `GET /stacks/{id}/containers/{cid}/logs/stream` — WebSocket follow; stdout+stderr demuxed server-side, 30s ping, normal-closure on daemon EOF, X-API-KEY auth on upgrade
- [ ] `GET /stacks/{id}/containers/{cid}/logs/stream` — WebSocket for live logs
- [x] `GET /stacks/{id}/containers/{cid}/stats` — CPU%, memory used/limit/%, per-iface network rx/tx, block I/O totals, PIDs (snapshot). Streaming is a separate WebSocket ticket below.
- [x] `GET /stacks/{id}/events` — SSE stream of Docker events filtered to this stack's managed resources (accelero-service / accelero-replica surfaced). Stack-wide view is the common one; a daemon-wide `/events` variant across all stacks can come later if needed.

**Resource browsers (read-only, filtered to accelero-managed):**
- [x] `GET /api/v1/images` — images referenced by managed containers with back-references (stack/service/replica/container). `?stack=<name>` narrows.
- [x] `GET /api/v1/volumes` — volumes labelled managed-by=accelero with driver/mount/labels. `?stack=<name>` narrows.
- [ ] `GET /volumes/{name}/browse` — read-only file browser for volumes (debugging)
- [x] `GET /api/v1/networks` — networks labelled managed-by=accelero with driver/scope/options. Also fixes a pre-existing bug where networks created by accelero were missing management labels. `?stack=<name>` narrows.

**Exceptional debug operations (audited, flagged as drift):**
- [ ] `POST /containers/{cid}/restart` — logged + audit trail (pure GitOps: don't use; prefer git revert)
- [ ] `POST /containers/{cid}/exec` — interactive shell via WebSocket (audit-logged, treated as drift event)
- [ ] `POST /volumes/{name}/write` — emergency file write (off by default, requires `--allow-volume-writes` flag)

**Audit log:**
- [ ] Persistent audit log in SQLite — every API action with timestamp, user, operation, resource
- [ ] `GET /audit` with filtering by stack, user, time range
- [ ] Audit entries for: deploys, drifts detected, drifts auto-corrected, exec sessions, restarts
- [ ] Immutable — append-only table, no update/delete API

---

### Phase 4 — Security & Multi-Tenancy

Goal: run Accelero in team/enterprise environments with multiple users, scoped permissions, and encrypted secrets.

**Identity & access:**
- [ ] User accounts with password auth (bcrypt)
- [ ] Multiple API tokens per user (named, revocable, scoped)
- [ ] OAuth2/OIDC integration (GitHub, Google, generic)
- [ ] Session-based auth for the UI
- [ ] LDAP integration (optional, via plugin)

**RBAC:**
- [ ] Roles: `admin`, `operator`, `viewer`, `none`
- [ ] Per-stack permissions (team X can deploy stack A but only view stack B)
- [ ] Team grouping
- [ ] Environment-based scoping (`prod` stacks require `admin` role)

**Secrets — guiding principle:** the interpolation `.env` in a GitOps repo is for declarative config only (tags, registries, ports, flags) and is committed to git. Actual secrets must never land there. The items below give secrets a first-class home that does not require committing anything sensitive.

**Secrets at rest (inside Accelero):**
- [ ] Encrypt the sensitive fields already stored in SQLite: `repo_token`, `docker_password`, user passwords. Per-field AEAD (e.g. XChaCha20-Poly1305) keyed by a master key.
- [ ] Master key sources: env var (simple setups), file path, or KMS (AWS KMS, GCP KMS, HashiCorp Vault Transit).
- [ ] Automatic key rotation: new writes use the current key; old reads transparently re-encrypt on next write.
- [ ] Online migration path from existing plaintext rows (one-shot admin endpoint that re-encrypts in place).

**Per-stack secrets API:**
- [ ] `POST /api/v1/stacks/{id}/secrets` — submit secret key/value pairs encrypted at rest; listed via the API without exposing values (write-only fields).
- [ ] Secrets injected into containers at deploy time via a host-side `env_file:` that Accelero materialises under `/run/accelero/<stack>/secrets.env` (tmpfs, short-lived, readable only by the managed container).
- [ ] Audit log entry for every secret create/update/delete, with the actor and the key name (never the value).

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
- [ ] Native TLS support (cert files or Let's Encrypt)
- [ ] Rate limiting on API endpoints (token bucket, per-key)
- [ ] CSRF protection for session-based UI
- [ ] Content Security Policy headers

**Registry management:**
- [ ] Multi-registry support with encrypted credentials at rest
- [ ] Registry-specific webhook signature verification (Docker Hub, GHCR, Harbor, generic HMAC)
- [ ] ECR/GCR/ACR IAM-based authentication
- [ ] Browse registry tags (for UI dropdown / approval workflows)

---

### Phase 5 — Operations & Integrations

Goal: make Accelero production-grade for teams that need notifications, approvals, and disaster recovery.

**Notifications:**
- [ ] Slack, Discord, Microsoft Teams integrations
- [ ] Generic webhook (POST JSON to configured URL)
- [ ] Email (SMTP)
- [ ] Event types: deploy started/completed/failed/rolled-back, drift detected, auto-deploy triggered, approval requested
- [ ] Per-stack notification routing (stack A → #prod channel, stack B → email)
- [ ] Notification templates (customizable content)

**Approval gates:**
- [ ] Stacks can require approval before deploy (`requires_approval: true`)
- [ ] Approval via API, Slack interactive message, or UI button
- [ ] Approval timeouts (auto-reject after N minutes)
- [ ] Approval audit trail

**Reliability:**
- [ ] Retry with exponential backoff: image pulls, git clones, network operations
- [ ] Configurable retry budget per operation type
- [ ] Circuit breaker for repeatedly-failing stacks

**Backup & disaster recovery:**
- [ ] `POST /admin/backup` — one-shot backup of SQLite
- [ ] Scheduled backups (cron-like) to local path, S3, GCS, or arbitrary SCP
- [ ] Backup encryption (age / gpg)
- [ ] Restore wizard (load backup, verify, swap)

**Templates (git-first, not in-app):**
- [ ] Curated list of public GitOps-ready sample repos (fork-to-deploy pattern)
- [ ] `POST /stacks/from-template` — create a stack from a template URL

---

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
