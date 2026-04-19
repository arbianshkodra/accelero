# Configuration

Accelero is configured through environment variables. Only `API_KEY` is required.

## Server Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `API_KEY` | *(required)* | API key for authenticating all API requests. Sent via `X-API-KEY` header. |
| `SERVER_PORT` | `8000` | HTTP server port |
| `DOCKER_SOCK` | `unix:///var/run/docker.sock` | Docker daemon socket path |
| `DATABASE_PATH` | `./data/accelero.db` | Path to the SQLite database file |

## Logging

| Variable | Default | Description |
|----------|---------|-------------|
| `LOG_LEVEL` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | `text` | Log output format: `text` (human-readable) or `json` (structured) |

## Performance Tuning

| Variable | Default | Description |
|----------|---------|-------------|
| `WORKER_COUNT` | `2 * CPU cores` | Number of worker goroutines (min: 2, max: 50) |
| `QUEUE_SIZE` | `15 * workers` | Task queue buffer size (min: 50, max: 1000) |

## Cleanup

| Variable | Default | Description |
|----------|---------|-------------|
| `STATUS_CLEANUP_INTERVAL` | `1h` | How often to clean up old deployment records from the database |
| `STATUS_MAX_AGE` | `24h` | Maximum age of completed deployment records before cleanup |

Docker resource cleanup (stopped containers, dangling images, unused volumes/networks) runs every 24 hours, scoped to resources labeled `managed-by=accelero`.

## Legacy Single-Stack Variables

These variables are supported for backward compatibility. If set, Accelero creates a "default" stack from them on first startup. For new deployments, use the REST API to create stacks instead.

| Variable | Description |
|----------|-------------|
| `REPO_URL` | Git repository URL |
| `REPO_USERNAME` | Git authentication username |
| `REPO_TOKEN` | Git authentication token |
| `REPO_BRANCH` | Git branch to clone |
| `COMPOSE_PATH` | Path to docker-compose.yaml in the repo |
| `SERVICE_NAMES` | Comma-separated service filter (empty = all) |
| `DOCKER_USERNAME` | Docker registry username |
| `DOCKER_PASSWORD` | Docker registry password |
| `DOCKER_REGISTRY` | Docker registry address |

## Sample Environment File

A ready-to-fill template lives at [`.env.example`](https://github.com/arbianshkodra/accelero/blob/dev/.env.example) in the repository. Copy it to `.env` and fill in the values:

```bash
cp .env.example .env
# edit .env
docker run --env-file .env ...
```

Do **not** confuse `.env.example` (configures the Accelero daemon itself) with a `.env` inside your GitOps repo next to `docker-compose.yaml` — that one feeds `${VAR}` interpolation in your compose. See [Variable Interpolation](./usage-overview.md#variable-interpolation-env-file).

## Docker Compose Example

```yaml
services:
  accelero:
    image: arbianshkodra/accelero:latest
    ports:
      - "8000:8000"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - accelero-data:/data
    environment:
      - API_KEY=your-secure-key
      - LOG_FORMAT=json
    restart: unless-stopped

volumes:
  accelero-data:
```
