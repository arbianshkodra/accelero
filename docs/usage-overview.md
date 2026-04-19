# Usage Overview

## Installation

### Docker (recommended)

```bash
docker run -d \
  --name accelero \
  -p 8000:8000 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v accelero-data:/data \
  -e API_KEY=your-secure-key \
  arbianshkodra/accelero
```

The `/data` volume persists the SQLite database across container restarts.

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

## Upgrading from Legacy Mode

If you were using the older single-stack Accelero with environment variables (`REPO_URL`, `REPO_USERNAME`, etc.), those still work. On first startup, Accelero automatically creates a "default" stack from those values. You can then manage it through the API like any other stack.
