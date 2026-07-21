<div align="center">
  <img src="./docs/images/logo.png" width="450" />

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
- **`.env` interpolation** — Full docker-compose `${VAR}` / `${VAR:-default}` / `${VAR:?required}` support
- **Broad compose compatibility** — `entrypoint`, `user`, `working_dir`, `hostname`, `dns`, `extra_hosts`, `cap_add`/`cap_drop`, `privileged`, `tmpfs`, `shm_size`, `init`, `expose`, `stop_grace_period`, `pull_policy`, healthchecks, resource limits
- **Zero-downtime deployments** — Rolling replica updates behind a reverse proxy (Caddy, Traefik, nginx). See [`samples/`](samples/) for a working end-to-end example with a scripted proof, and [`docs/usage-overview.md#zero-downtime-deployments`](docs/usage-overview.md#zero-downtime-deployments) for why a proxy is required.
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

### `.env` Interpolation

Your compose file can reference environment variables using docker-compose's syntax. Accelero resolves them from a `.env` file in the repo before parsing:

**`docker-compose.yaml`:**
```yaml
services:
  web:
    image: ${REGISTRY:-ghcr.io}/${APP}:${TAG}
    environment:
      - DEBUG=${DEBUG-off}
      - DB_URL=${DB_URL:?DB_URL is required}
```

**`.env`** (next to the compose file):
```
APP=my-app
TAG=1.2.3
DB_URL=postgres://db/app
```

Resolves to `ghcr.io/my-app:1.2.3` before deploy. See [Variable Interpolation](./docs/usage-overview.md#variable-interpolation-env-file) for the full syntax.

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

See [`.env.example`](./.env.example) for the full list including legacy single-stack variables.

## Legacy Mode

If you're upgrading from an older version that used `REPO_URL`, `REPO_USERNAME`, etc. as environment variables, Accelero will automatically create a "default" stack from those values on first startup. No changes needed.

## Documentation

Full documentation: [accelero.sh/docs](https://accelero.sh/docs)

## Contributing

Contributions are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md) for how to build, test, and open a pull request.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
