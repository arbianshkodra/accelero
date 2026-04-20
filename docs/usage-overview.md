# Usage Overview

## Installation

### Docker (recommended)

```bash
docker run -d \
  --name accelero \
  -p 8000:8000 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /var/lib/accelero:/var/lib/accelero \
  -e DATABASE_PATH=/var/lib/accelero/accelero.db \
  -e STACKS_DATA_DIR=/var/lib/accelero/stacks \
  -e API_KEY=your-secure-key \
  arbianshkodra/accelero
```

The `/var/lib/accelero` volume persists the SQLite database *and* each stack's cloned repo across container restarts.

**Why same-path bind mount.** Accelero clones each stack's gitops repo into `$STACKS_DATA_DIR/<stack_id>/repo/` and uses those files as the source for any compose `volumes:` entry that references a relative path (e.g. `./Caddyfile:/etc/caddy/Caddyfile`). Docker resolves bind-mount source paths on the **host**, not inside Accelero's container — so the path has to exist at the same absolute location on both sides. A named volume like `-v accelero-data:/data` won't work here: the host-side path (`/var/lib/docker/volumes/accelero-data/_data`) doesn't match the in-container path. Stacks without relative bind mounts still work, but you lose the ability to commit files like Caddyfiles / nginx configs / TLS certs to your gitops repo and have them mounted into containers.

### Binary

Download the appropriate binary from [GitHub Releases](https://github.com/arbianshkodra/accelero/releases), then:

```bash
export API_KEY=your-secure-key
./accelero
```

Accelero supports Linux (amd64, arm64, arm, 386), macOS (amd64, arm64), and Windows (amd64, arm64, arm, 386).

## Managing Stacks

All API endpoints (except `/health`) require the `X-API-KEY` header.

### Create a Stack

```bash
curl -X POST http://localhost:8000/api/v1/stacks \
  -H "Content-Type: application/json" \
  -H "X-API-KEY: your-secure-key" \
  -d '{
    "name": "my-app",
    "repo_url": "https://github.com/your-org/your-gitops-repo",
    "repo_username": "your-username",
    "repo_token": "ghp_your-github-token",
    "repo_branch": "main",
    "compose_path": "docker-compose.yaml",
    "auto_deploy": true,
    "reconcile_interval_seconds": 300
  }'
```

**Fields:**

| Field | Required | Description |
|-------|----------|-------------|
| `name` | Yes | Unique name for the stack |
| `repo_url` | Yes | Git repository URL |
| `repo_username` | No | Git auth username |
| `repo_token` | No | Git auth token/password |
| `repo_branch` | No | Branch to clone (default: repo default branch) |
| `compose_path` | Yes | Path to docker-compose.yaml within the repo |
| `service_filter` | No | Comma-separated list of services to deploy (default: all) |
| `auto_deploy` | No | Auto-deploy when drift is detected (default: false) |
| `reconcile_interval_seconds` | No | Drift check interval in seconds (0 = disabled) |
| `docker_username` | No | Docker registry username for private images |
| `docker_password` | No | Docker registry password |
| `docker_registry` | No | Docker registry address |

### List Stacks

```bash
curl http://localhost:8000/api/v1/stacks \
  -H "X-API-KEY: your-secure-key"
```

### Update a Stack

```bash
curl -X PUT http://localhost:8000/api/v1/stacks/{id-or-name} \
  -H "Content-Type: application/json" \
  -H "X-API-KEY: your-secure-key" \
  -d '{"auto_deploy": false}'
```

Only the fields you include will be updated.

### Delete a Stack

```bash
curl -X DELETE http://localhost:8000/api/v1/stacks/{id-or-name} \
  -H "X-API-KEY: your-secure-key"
```

## Deployments

### Trigger a Deployment

```bash
curl -X POST http://localhost:8000/api/v1/stacks/{id-or-name}/deploy \
  -H "X-API-KEY: your-secure-key"
```

The deployment runs asynchronously. Check the deployment history for progress.

### View Deployment History

```bash
curl http://localhost:8000/api/v1/stacks/{id-or-name}/deployments \
  -H "X-API-KEY: your-secure-key"
```

Returns the 50 most recent deployments with status, trigger, git commit, and any errors.

### Check Drift

```bash
curl http://localhost:8000/api/v1/stacks/{id-or-name}/drift \
  -H "X-API-KEY: your-secure-key"
```

Returns a drift report showing any differences between the desired state (git) and actual state (Docker).

### Preview a Deployment

```bash
curl -X POST http://localhost:8000/api/v1/stacks/{id-or-name}/preview \
  -H "X-API-KEY: your-secure-key"
```

Dry-run the next deploy: Accelero runs the same drift check as `/drift` and translates each difference into the action a real deploy would take (`create`, `recreate`, `restart`, `remove`, `error`). No containers are touched — handy for PR review, CI gates, and "what's about to happen" checks before hitting `/deploy`. See the [API reference](api-reference.md#preview-deployment) for the full response shape and action taxonomy.

## Deployment Strategies

Accelero supports three ways to trigger deployments. You can use any combination.

### Strategy 1: Webhook (push-based)

Your CI/CD pipeline or git platform sends a webhook to Accelero after a commit or image push. The deployment happens immediately.

```bash
# In your CI pipeline, after git push:
curl -X POST https://your-accelero:8000/webhook \
  -H "Content-Type: application/json" \
  -H "X-API-KEY: your-secure-key" \
  -d '{"stack": "my-app"}'
```

If `stack` is omitted, the "default" stack is deployed. If no "default" stack exists, the first available stack is used.

**Best for:** Fast feedback loops, CI/CD integration, immediate deploys.

### Strategy 2: Reconciliation (pull-based, true GitOps)

Accelero periodically polls the git repository, compares the desired state against running containers, and auto-deploys when drift is detected. No webhook needed — just push to git and Accelero picks it up.

```bash
curl -X POST http://localhost:8000/api/v1/stacks \
  -H "Content-Type: application/json" \
  -H "X-API-KEY: your-secure-key" \
  -d '{
    "name": "my-app",
    "repo_url": "https://github.com/org/repo",
    "compose_path": "docker-compose.yaml",
    "auto_deploy": true,
    "reconcile_interval_seconds": 300
  }'
```

With `auto_deploy: true` and a 300-second interval, Accelero checks every 5 minutes and deploys automatically if anything has changed.

**Best for:** True GitOps workflows, self-healing infrastructure, catching manual drift.

### Strategy 3: Manual API call

Trigger a deployment on demand via the API:

```bash
curl -X POST http://localhost:8000/api/v1/stacks/my-app/deploy \
  -H "X-API-KEY: your-secure-key"
```

**Best for:** Controlled rollouts, debugging, ad-hoc deploys.

### Recommended: Webhook + Reconciliation

Use webhooks for immediate deploys when you push, and reconciliation as a safety net. This way:

- Deploys happen fast when you push (webhook)
- If a webhook is missed, the reconciler catches it within the next interval
- If someone manually changes a container, the reconciler detects the drift and corrects it
- You can check drift at any time: `GET /api/v1/stacks/my-app/drift`

## Deployment Flow

When a deployment is triggered (via API, webhook, or reconciliation), Accelero:

1. **Clones** the git repository (shallow, depth=1)
2. **Validates** the compose file path (directory traversal protection)
3. **Interpolates** `${VAR}` references using the repo's `.env` file (see [Variable interpolation](#variable-interpolation-env-file))
4. **Parses** the docker-compose.yaml
5. **Resolves** service dependencies (topological sort with cycle detection)
6. **Captures** the pre-deployment state of all services
7. **Creates networks** defined in the compose file
8. **Deploys** each service in dependency order:
    - Pulls the image (with per-stack registry credentials)
    - Creates and starts a new container (labeled `managed-by: accelero`)
    - Waits for the health check to pass
    - Removes the old container
9. **Rolls back** on failure using the captured pre-deployment state
10. **Records** the deployment in SQLite

## Observability

### Prometheus metrics

Accelero exposes a Prometheus-format metrics endpoint at `GET /metrics` (no authentication — Prometheus scrape convention; metrics never contain payloads or secrets). Scrape it the usual way:

```yaml
# prometheus.yml
scrape_configs:
  - job_name: accelero
    static_configs:
      - targets: ['accelero.internal:8000']
```

Useful metrics out of the box:

- `accelero_deployments_total{stack,trigger,status}` — how many deploys, broken out by outcome
- `accelero_deployment_duration_seconds` — histogram of deploy latency
- `accelero_drift_detected_total{stack,type}` — drift items observed by the reconciler (preview calls intentionally do not count here)
- `accelero_reconcile_cycles_total{stack}` — reconciler activity per stack
- `accelero_http_requests_total{method,path,status}` — HTTP traffic; `path` uses route templates (`/api/v1/stacks/{id}`) to keep cardinality bounded
- `accelero_stacks{status}` — gauge of stacks in each lifecycle state
- `accelero_reconciler_loops` — gauge of active reconciler goroutines

Standard `go_*` / `process_*` collectors are included for runtime health. The full list, including label cardinality notes, lives in the [API reference](api-reference.md#metrics).

## Variable Interpolation (`.env` file)

Accelero supports the same `${VAR}` interpolation syntax docker-compose uses. Put a `.env` file next to your `docker-compose.yaml` in your git repo, and Accelero substitutes variable references before parsing.

### Example

**`docker-compose.yaml`:**
```yaml
services:
  web:
    image: ${REGISTRY:-ghcr.io}/${APP}:${TAG}
    environment:
      - DB_URL=${DB_URL:?DB_URL is required}
      - DEBUG=$DEBUG
```

**`.env`:**
```
APP=my-app
TAG=1.2.3
DB_URL=postgres://db/app
DEBUG=on
# REGISTRY intentionally absent — falls back to ":-default"
```

Accelero resolves `${REGISTRY:-ghcr.io}/${APP}:${TAG}` to `ghcr.io/my-app:1.2.3` before deploy. If `DB_URL` were missing, the deploy fails fast with the error message — no containers are touched.

### Supported forms

| Form | Behaviour |
|------|-----------|
| `$VAR`, `${VAR}` | Value of `VAR`, empty string if unset |
| `${VAR-default}` | Default if `VAR` is unset |
| `${VAR:-default}` | Default if `VAR` is unset **or** empty |
| `${VAR?error}` | Error if `VAR` is unset |
| `${VAR:?error}` | Error if `VAR` is unset **or** empty |
| `${VAR+value}` | Replacement if `VAR` is set (even empty) |
| `${VAR:+value}` | Replacement if `VAR` is set **and** non-empty |
| `$$` | Literal `$` |

### `.env` file syntax

```
# Comments start with #
KEY=value
QUOTED="value with  spaces  and \n escapes"
LITERAL='no $escapes and \n stays literal'
EMPTY=
TRAILING=value  # inline comment (unquoted values)
export EXPORTED=works_too
```

- Double-quoted values support `\n`, `\r`, `\t`, `\\`, `\"` escape sequences
- Single-quoted values are literal (no escapes)
- Unquoted values are trimmed; a `#` preceded by whitespace starts an inline comment
- Leading `export` is allowed (convenient when the `.env` is also shell-sourced)

### Lookup order

Accelero looks for `.env` in this order, using the first one found:

1. Next to the compose file: `<repo>/<compose-dir>/.env`
2. At the repo root: `<repo>/.env`

A missing `.env` is not an error — compose files without `${...}` references work unchanged.

### Multi-environment pattern

Use one git branch per environment, each with its own `.env`, and one Accelero stack per branch:

```bash
# Staging stack → staging branch's .env
curl -X POST /api/v1/stacks \
  -d '{"name":"app-staging","repo_url":"...","repo_branch":"staging", ...}'

# Production stack → main branch's .env
curl -X POST /api/v1/stacks \
  -d '{"name":"app-prod","repo_url":"...","repo_branch":"main", ...}'
```

Because Accelero treats git as the source of truth, promoting from staging to prod is a PR merge — never a manual config change.

### ⚠️ Secrets do not belong in this `.env`

The word ".env" is overloaded, so this point is worth making bluntly:

- The `.env` described on this page is a **compose interpolation** file. Its job is to fill in declarative config — image tags, registry names, replicas, public ports, feature flags — things that describe **what to deploy** and belong in git for the same reason the compose file does.
- A **runtime** `.env` (the one people mean when they say "don't commit .env") contains passwords, API keys, database URLs. It is **never** the same file and **must not** be committed to git.

**Rule of thumb:** if leaking the value would be a security incident, it does not belong in the `.env` next to your compose.

Today, the supported patterns for actual secrets are:

| Kind of secret | Where it goes |
|----------------|---------------|
| Git credentials (to clone the GitOps repo) | `repo_username` / `repo_token` on the Accelero stack (stored in SQLite) |
| Docker registry credentials | `docker_username` / `docker_password` on the Accelero stack |
| Application secrets (DB password, API keys) | A separate file referenced via `env_file:` in your compose service, populated by the host at deploy time (mounted volume, external tool, etc.). This file is **not** interpolated and is **not** committed to git |

Accelero does not yet natively integrate with external secret stores. The plan is to land Phase 4's secrets track (encrypted-at-rest per-stack secrets + pluggable Vault/KMS/Secrets Manager backends + `${secret:...}` interpolation syntax) so application secrets get a first-class home that doesn't require a committed file at all. Until then, treat interpolation `.env` as config-only and keep secrets in an adjacent `env_file:` you populate through your host's own provisioning.

## Supported compose fields

Accelero parses the subset of docker-compose fields listed below. Anything outside this list is silently ignored at parse time — so a real compose file only fails on genuine misconfiguration, not on newer/niche keys.

### Service-level

| Field | Accepts | Notes |
|-------|---------|-------|
| `image` | string (required) | The image reference Accelero pulls and runs |
| `command` | string or list | Override the image's CMD |
| `entrypoint` | string or list | Override the image's ENTRYPOINT |
| `working_dir` | string | Container working directory |
| `user` | string | `uid`, `uid:gid`, or `name:group` |
| `hostname` | string | |
| `domainname` | string | |
| `environment` | list of `KEY=VALUE` or map | Inline env vars |
| `env_file` | list of paths | Paths relative to the compose file |
| `ports` | list of strings **or** long-form maps | Short: `"[host_ip:]host_port:container_port[/proto]"`; long-form fields: `target`, `published`, `protocol`, `host_ip` |
| `expose` | list of strings or ints | Exposed (not published) ports |
| `volumes` | list (`"host:container[:mode]"` or `"volume_name:container[:mode]"`) | Bind mounts and named-volume references both supported. Named volumes must also be declared at the top level (see below). |
| `networks` | list | Must exist at the top-level `networks:` block |
| `depends_on` | list of service names **or** map with conditions | Short form → `service_started`; long form accepts `condition: service_healthy` / `service_completed_successfully` and blocks the dependent's deploy until satisfied |
| `labels` | map | Merged with Accelero's `managed-by` / `accelero-stack` labels |
| `restart` | `no` / `always` / `on-failure` / `unless-stopped` | |
| `healthcheck` | object (`test`, `interval`, `timeout`, `retries`, `start_period`) | Overrides image-level healthcheck |
| `stop_grace_period` | duration (`30s`, `2m`) | Timeout before force-kill on stop; replaces the old hardcoded 10s |
| `stop_signal` | string (`SIGTERM`, `SIGHUP`, …) | Signal sent to stop the container gracefully |
| `dns` | string or list | |
| `dns_search` | string or list | |
| `extra_hosts` | list (`"host:ip"`) or map | |
| `cap_add` / `cap_drop` | list | Linux capabilities |
| `privileged` | bool | Privileged mode |
| `tmpfs` | string, list, or map | Map values become mount options (`size=64m`) |
| `shm_size` | size string (`256m`, `1g`) | |
| `init` | bool | Run a minimal init process inside the container |
| `mem_limit` | size string | Memory ceiling |
| `cpu_limit` | float | CPU quota in fractional cores (e.g. `0.5`) |
| `pull_policy` | `always`, `missing`, `if_not_present`, `never` | See [pull policy](#pull-policy) below; `build` is rejected |
| `logging` | `{driver, options}` | Maps 1:1 to Docker's `LogConfig` — e.g. `json-file` with `max-size` / `max-file` options |
| `deploy` | object | Only `replicas` is acted on — see [Replicas](#replicas) below. `mode` parses but is informational; other `deploy.*` fields (resources, restart_policy) are ignored |

### Top-level

| Field | Notes |
|-------|-------|
| `services` | Required |
| `networks` | Accelero creates missing networks with the declared driver and `driver_opts` |
| `volumes` | Accelero creates missing named volumes idempotently with `driver` / `driver_opts` / `labels`; supports `external: true` and `name:` overrides. See below. |
| `version` | Parsed but not enforced (docker-compose itself has dropped the schema-version gate) |

### `depends_on` conditions

Accelero supports both the short and long forms, and actually waits on the long-form conditions before deploying the dependent service.

**Short form** — ordering only, no waiting:

```yaml
services:
  app:
    depends_on:
      - db
      - redis
```

**Long form** — per-dependency condition:

```yaml
services:
  app:
    depends_on:
      db:
        condition: service_healthy
      migrate:
        condition: service_completed_successfully
```

| Condition | Accelero behaviour |
|-----------|-------------------|
| `service_started` (default) | Topological order is enough — dependencies always deploy first |
| `service_healthy` | Block the dependent's deploy until Docker reports `State.Health.Status == "healthy"` for the dependency. Errors if the dependency has no `healthcheck`. Timeout: 2 minutes. |
| `service_completed_successfully` | Block the dependent's deploy until the dependency's container has exited with code 0 (for one-shot init/migration containers). Timeout: 2 minutes. |

Unknown conditions fail the deploy with a clear error rather than silently ignoring. `required: false` and `restart: true` parse but are not yet acted on.

### Ports

Both short and long forms are accepted. The short form covers the common cases in a single string; the long form is useful when you need to spell out `host_ip` or mix protocols across several bindings.

```yaml
services:
  web:
    ports:
      # Short form: [host_ip:]host_port:container_port[/proto]
      - "8080:80"                   # 0.0.0.0:8080 -> 80/tcp
      - "127.0.0.1:5353:53/udp"     # loopback only, UDP
      - "9090"                      # exposed only, not published

      # Long form
      - target: 80
        published: 8081
        protocol: tcp
        host_ip: 127.0.0.1
```

A bare container port (no host port) is exposed but not published — same effect as `expose:`.

### Logging

`logging` maps directly onto Docker's `LogConfig`:

```yaml
services:
  web:
    image: nginx:1.27.1
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
```

Any driver supported by your Docker engine works (`json-file`, `local`, `journald`, `syslog`, `fluentd`, `gelf`, `awslogs`, etc.); Accelero passes the options through unchanged.

### Bind mounts from the gitops repo

Compose `volumes:` entries that reference a relative path are resolved against the stack's cloned repo directory, so files committed alongside your `docker-compose.yaml` can be mounted into containers:

```yaml
services:
  proxy:
    image: caddy:2-alpine
    command: ["caddy", "run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"]
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - ./certs:/etc/caddy/certs:ro
```

The clone lives at `$STACKS_DATA_DIR/<stack_id>/repo/` and is re-cloned (fresh) on every deploy, then persisted for the lifetime of the stack. Deleting the stack via `DELETE /api/v1/stacks/{id}` removes the directory.

**Rules:**

| Source form | Behaviour |
|-------------|-----------|
| `./Caddyfile` | Resolved against the cloned repo dir → `<stacks_dir>/<stack_id>/repo/Caddyfile` |
| `./conf/nginx.conf` | Same — nested paths allowed, resolved lexically |
| `../outside` | Rejected at deploy time (directory traversal) |
| `/etc/ssl/certs` | Absolute host path — passed through unchanged; bind-mounts whatever lives on the host |
| `pg_data` (bare) | Interpreted as a named-volume reference, not a bind mount — matches compose convention |

**Limitations worth knowing:**

- **Changing a bind-mounted file without changing the compose spec won't recreate the container.** Each deploy re-clones the repo, so the new content is on disk — but the container keeps its existing mount until something in the compose changes (e.g. the service's image tag). To force a reload of a mounted config file, either bump a service label / env var in the compose, or recreate the stack.
- **Running Accelero in Docker requires a same-path bind mount** for the stacks data dir. See [Installation / Docker](#docker-recommended) for the flag pattern.

### Named volumes

Accelero treats the top-level `volumes:` section as **desired state** and creates each declared internal volume idempotently before any service starts. Volumes are **never auto-deleted** — they hold state, so a stack delete today won't destroy them (a future `--remove-volumes` flag will make that explicit).

```yaml
services:
  db:
    image: postgres:16
    volumes:
      - pg_data:/var/lib/postgresql/data   # named volume reference
      - /host/bind:/etc/config             # bind mount (pass-through)

volumes:
  pg_data:                                 # managed by Accelero
    driver: local
    driver_opts:
      type: ext4
      device: /dev/sda1
  shared_cache:                            # pre-existing, Accelero doesn't own
    external: true
    name: real_volume_name                 # optional explicit name
```

**Naming rules (in priority order):**

| Config | Docker-side name | Who creates/deletes |
|--------|------------------|---------------------|
| `external: true` | `cfg.Name` if set, else the compose key | User — Accelero only verifies existence |
| `name: explicit` | `cfg.Name` verbatim | Accelero |
| default | `accelero_<stack>_<logical>` (prevents cross-stack collisions, mirrors docker-compose project prefix) | Accelero |

Managed volumes are labelled `managed-by=accelero` and `accelero-stack=<name>` so scoped cleanup won't touch anything the user owns.

**Drift detection:** a declared named volume missing from the host shows up as a drift item (`type: "missing"` for internal, `type: "missing_external"` for external). Extra volumes on the host that Accelero didn't declare are ignored.

**State survives redeploys.** Changing a service's image tag rebuilds the container but re-attaches the same volume — data persists.

### Zero-downtime deployments

Zero-downtime is the whole point: pushing a new image tag should not interrupt traffic. For that to actually hold, you need **two things together**:

1. **The backend service runs as replicas** (`deploy.replicas: N` with `expose:` only, no published host port). Accelero rolls replicas one at a time — create new, wait healthy, remove one old.
2. **Something in front holds the host port** and routes to the replicas via Docker's internal DNS. Typically a reverse proxy (Caddy, Traefik, nginx). The proxy itself never rolls; only the backend does.

Without the proxy in front, externally-reachable services cannot be zero-downtime deployed under Docker — only one container at a time can hold a host port. An image bump on a service that publishes `"80:80"` directly either **fails with a port conflict** (Docker rejects the new container's bind) or **has a measurable outage** during container swap. There's no path around this at the Docker level; it has to be solved by traffic indirection.

See `samples/` in the accelero repo for a working end-to-end example (nginx + Caddy + replicas + a scripted proof). The pattern boils down to:

```yaml
services:
  web:
    image: nginx:1.27.1-alpine
    expose: ["80"]
    networks: [app]
    healthcheck:
      test: ["CMD", "wget", "-q", "--spider", "http://localhost/"]
      interval: 5s
    deploy:
      replicas: 3

  proxy:
    image: caddy:2-alpine
    command: ["caddy", "reverse-proxy", "--from", ":80", "--to", "web:80"]
    ports: ["80:80"]        # only the proxy binds the host port
    depends_on: [web]
    networks: [app]

networks:
  app: { driver: bridge }
```

On an image bump of `web`, Accelero rolls the three replicas while Caddy keeps resolving `web` to whichever ones are healthy via Docker DNS.

### Replicas

`deploy.replicas` runs N interchangeable containers for a service. Accelero uses a rolling strategy: it creates a new replica, waits for it to become healthy, then removes one of the old replicas. With a proxy in front (see "Zero-downtime deployments" above), at least N−1 replicas stay up at any point in the rollout.

```yaml
services:
  web:
    image: nginx:1.27.1
    expose:
      - "80"
    networks: [app]
    deploy:
      replicas: 3
```

Each replica gets a unique container name (`<service>_<index>_<timestamp>`) and the labels `accelero-service=<name>` and `accelero-replica=<index>` for observability:

```bash
$ docker ps --filter label=accelero-service=web --format '{{.Names}} {{.Label "accelero-replica"}}'
web_0_... 0
web_1_... 1
web_2_... 2
```

**Scaling.** Change `replicas:` in git and redeploy. Accelero keeps healthy replicas running on the target image, adds more when scaling up, and removes surplus ones when scaling down.

**Idempotent re-deploys.** Re-running `deploy` when the stack is already in sync (N healthy replicas on the target image) is a no-op — no containers are recreated.

**`depends_on` with replicated dependencies.**
- `service_healthy`: any replica healthy unblocks dependents (matches Swarm's semantics; Docker DNS round-robins to healthy endpoints as soon as one is up).
- `service_completed_successfully`: all replicas must have exited with code 0.

#### Static host ports collide with replicas

Accelero rejects any deploy where `replicas > 1` and the service publishes a static host port:

```yaml
services:
  web:
    image: nginx:1.27.1
    ports:
      - "80:80"          # ❌ static host port
    deploy:
      replicas: 3         # ❌ would collide on host port 80
```

The deploy fails up front with:

> `service "web" has replicas=2 with a static published host port; replicas would collide on the host port. Use \`expose:\` + a reverse proxy, or reduce replicas to 1`

This mirrors docker-compose's non-swarm behaviour. The GitOps-correct pattern for replicated services is `expose:` (internal-only) plus a reverse-proxy or load-balancer service (also managed by Accelero) that fans out to the `accelero-service` label of the replicated service.

### Pull policy

Accelero defaults to pulling on every deploy, matching modern docker-compose behaviour. Override per-service with `pull_policy`:

| Value | Behaviour |
|-------|-----------|
| `always` (default if unset) | Pull on every deploy, regardless of local cache |
| `missing` / `if_not_present` | Skip the pull if the image is already present on the host |
| `never` | Never pull; fail the deploy if the image is missing |
| `build` | Rejected — Accelero is a CD tool, not a builder; build your images in CI |

## Upgrading from Legacy Mode

If you were using the older single-stack Accelero with environment variables (`REPO_URL`, `REPO_USERNAME`, etc.), those still work. On first startup, Accelero automatically creates a "default" stack from those values. You can then manage it through the API like any other stack.
