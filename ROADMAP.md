# Accelero Roadmap

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

## In Progress

### Phase 2 — Compose Compatibility, Observability & Management

**Docker Compose compatibility (high impact):**
- [ ] `.env` variable substitution (`${VAR}`, `${VAR:-default}`, `${VAR:?error}`)
- [ ] `entrypoint`, `working_dir`, `user` fields
- [ ] `depends_on` map form with conditions (`condition: service_healthy`)
- [ ] `deploy.replicas` for container scaling
- [ ] Named volumes (top-level `volumes:` section with `docker volume create`)
- [ ] `logging` driver and options
- [ ] `stop_grace_period` (currently hardcoded to 10s)
- [ ] Long-form `ports` syntax (`{target: 80, published: 8080, protocol: tcp}`)

**Docker Compose compatibility (medium impact):**
- [ ] `extra_hosts`, `hostname`, `domainname`, `dns`, `dns_search`
- [ ] `cap_add`, `cap_drop`, `privileged`
- [ ] `tmpfs`, `shm_size`, `init`
- [ ] `expose` (expose ports without publishing)
- [ ] `pull_policy` (always, never, missing)

**Observability & management:**
- [ ] Container management endpoints (logs, restart, stop)
- [ ] WebSocket support for real-time log streaming and deployment progress
- [ ] Prometheus metrics endpoint (`/metrics`)
- [ ] Structured request ID propagation through deployment chain
- [ ] Deployment diff preview (show what will change before deploying)

### Phase 3 — Production Hardening
- [ ] Rate limiting on API endpoints
- [ ] TLS support (native or documentation for reverse proxy)
- [ ] Webhook signature verification (Docker Hub, GHCR, Harbor formats)
- [ ] Retry with exponential backoff for transient failures (image pulls, network)
- [ ] Notification integrations (Slack, Discord, generic webhook on deploy events)
- [ ] Approval gates for production stacks

### Phase 4 — Web UI
- [ ] Dashboard with stack overview, deployment history, container grid
- [ ] Log viewer with search and filtering
- [ ] Drift visualization
- [ ] Stack creation/editing form
- [ ] Deployment trigger and rollback buttons

### Phase 5 — Multi-Host
- [ ] Lightweight agent that runs on each Docker host
- [ ] Central server orchestrates agents
- [ ] Host grouping by environment
- [ ] Rolling deployments across hosts
