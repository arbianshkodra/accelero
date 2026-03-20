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
3. **Parses** the docker-compose.yaml
4. **Resolves** service dependencies (topological sort with cycle detection)
5. **Captures** the pre-deployment state of all services
6. **Creates networks** defined in the compose file
7. **Deploys** each service in dependency order:
    - Pulls the image (with per-stack registry credentials)
    - Creates and starts a new container (labeled `managed-by: accelero`)
    - Waits for the health check to pass
    - Removes the old container
8. **Rolls back** on failure using the captured pre-deployment state
9. **Records** the deployment in SQLite

## Upgrading from Legacy Mode

If you were using the older single-stack Accelero with environment variables (`REPO_URL`, `REPO_USERNAME`, etc.), those still work. On first startup, Accelero automatically creates a "default" stack from those values. You can then manage it through the API like any other stack.
