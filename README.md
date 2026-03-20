<div align="center">
  <img src="./docs/images/logo.jpg" width="450" />

  # Accelero
  GitOps-powered Docker deployment automation with zero downtime.
</div>

## What is Accelero?

Accelero is a lightweight, single-binary tool that brings GitOps to Docker Compose environments. It manages multiple deployment stacks, each backed by a git repository containing a `docker-compose.yaml`. Accelero continuously reconciles the desired state in git against the actual running containers, deploying changes automatically with zero downtime.

Think of it as **ArgoCD/Flux for Docker Compose** — git is the source of truth, drift is detected and corrected, and deployments are fully automated.

## Features

- **Multi-stack management** — Manage multiple independent stacks via REST API
- **GitOps reconciliation** — Periodic drift detection: desired state (git) vs actual state (Docker), auto-deploy on drift
- **Flexible triggers** — Deploy via webhook (push), reconciliation (pull), or manual API call
- **Zero-downtime deployments** — New containers are health-checked before old ones are removed
- **Automatic rollback** — Pre-deployment state captured and restored on failure
- **Dependency resolution** — Services deployed in correct order based on `depends_on`
- **SQLite persistence** — Deployment history and stack config survive restarts
- **Single binary** — No external dependencies beyond Docker

## Quick Start

### Option 1: Docker (recommended)

```bash
docker run -d \
  --name accelero \
  -p 8000:8000 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v accelero-data:/data \
  -e API_KEY=your-secure-key \
  arbianshkodra/accelero
```

### Option 2: Binary

```bash
# Download from GitHub Releases, then:
export API_KEY=your-secure-key
./accelero
```

### Create a Stack

```bash
curl -X POST http://localhost:8000/api/v1/stacks \
  -H "Content-Type: application/json" \
  -H "X-API-KEY: your-secure-key" \
  -d '{
    "name": "my-app",
    "repo_url": "https://github.com/your-org/your-gitops-repo",
    "repo_username": "your-username",
    "repo_token": "your-token",
    "compose_path": "docker-compose.yaml",
    "auto_deploy": true,
    "reconcile_interval_seconds": 300
  }'
```

### Trigger a Deployment

```bash
curl -X POST http://localhost:8000/api/v1/stacks/my-app/deploy \
  -H "X-API-KEY: your-secure-key"
```

### Check for Drift

```bash
curl http://localhost:8000/api/v1/stacks/my-app/drift \
  -H "X-API-KEY: your-secure-key"
```

## API Reference

All endpoints (except `/health`) require the `X-API-KEY` header.

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/api/v1/stacks` | Create a stack |
| `GET` | `/api/v1/stacks` | List all stacks |
| `GET` | `/api/v1/stacks/{id}` | Get a stack (by ID or name) |
| `PUT` | `/api/v1/stacks/{id}` | Update a stack |
| `DELETE` | `/api/v1/stacks/{id}` | Delete a stack |
| `POST` | `/api/v1/stacks/{id}/deploy` | Trigger deployment |
| `GET` | `/api/v1/stacks/{id}/deployments` | List deployment history |
| `GET` | `/api/v1/stacks/{id}/drift` | Check drift |
| `POST` | `/webhook` | Legacy webhook (deploys default stack) |
| `GET` | `/health` | Health check (no auth) |
| `GET` | `/status` | Stack summaries |

## Configuration

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `API_KEY` | Yes | — | API key for authentication |
| `SERVER_PORT` | No | `8000` | HTTP server port |
| `DOCKER_SOCK` | No | `unix:///var/run/docker.sock` | Docker socket path |
| `DATABASE_PATH` | No | `./data/accelero.db` | SQLite database path |
| `LOG_LEVEL` | No | `info` | Log level (debug, info, warn, error) |
| `LOG_FORMAT` | No | `text` | Log format (text, json) |

See `sample.env` for the full list including legacy single-stack variables.

## Legacy Mode

If you're upgrading from an older version that used `REPO_URL`, `REPO_USERNAME`, etc. as environment variables, Accelero will automatically create a "default" stack from those values on first startup. No changes needed.

## Documentation

Full documentation: [accelero.sh/docs](https://accelero.sh/docs)

## License

See [LICENSE.md](LICENSE.md).
