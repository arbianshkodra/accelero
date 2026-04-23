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

## Rate limiting

| Variable | Default | Description |
|----------|---------|-------------|
| `RATE_LIMIT_RPS` | `0` *(disabled)* | Sustained refill rate (tokens/second) for a per-API-key token bucket. Applied after authentication, so probes (`/health`, `/readyz`, `/metrics`) are unaffected. |
| `RATE_LIMIT_BURST` | `max(2*RPS, 10)` when RPS set | Bucket capacity — how many requests a quiescent caller may send at once. Clamped to `>=1`. |

When a caller exhausts their bucket, Accelero responds with `429 Too Many Requests`, a `Retry-After` header (seconds, `>=1`), and a JSON body:

```json
{"error": "rate limit exceeded", "retry_after": "1s"}
```

The rejection is counted in the `accelero_rate_limited_requests_total` Prometheus counter, labelled by mux route template (so high-cardinality stack IDs don't explode the label set).

## Webhook signature verification

| Variable | Default | Description |
|----------|---------|-------------|
| `WEBHOOK_SECRET` | *(unset — API-key auth on `/webhook`)* | Shared HMAC-SHA256 secret for the root-level `POST /webhook` endpoint. When set, callers must sign the raw request body and pass the hex digest as `X-Hub-Signature-256: sha256=<hex>` — the GitHub webhook format. Replaces the API-key check for `/webhook` so external senders (GitHub, Gitea, Gogs, CI) can authenticate without smuggling the API key into their webhook config. |

`POST /api/v1/webhook` still requires the API key regardless — that route stays a private operator surface. Rejected signatures bump `accelero_webhook_signature_rejected_total{reason}` and return `401 Unauthorized` with a generic `{"error":"invalid webhook signature"}` body (no reason detail leaks to the caller). The middleware caps request bodies at 1 MiB; larger bodies return `413 Request Entity Too Large`.

**Known limitation:** HMAC alone does not prevent replay. An attacker who captures a valid webhook payload can re-POST it later. Replay protection (timestamp in body + window check, or nonce tracking) is left to the caller or an upstream proxy for now.

Compute a signature with `openssl`:

```bash
BODY='{"stack":"default"}'
printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$WEBHOOK_SECRET"
```

## At-rest encryption

| Variable | Default | Description |
|----------|---------|-------------|
| `ACCELERO_ENCRYPTION_KEY` | *(unset — encryption disabled)* | Base64-encoded 32-byte master key, **inline**. When set, `repo_token` and `docker_password` are encrypted before being written to SQLite (AES-256-GCM) and decrypted on read. Generate with `openssl rand -base64 32`. |
| `ACCELERO_ENCRYPTION_KEY_FILE` | *(unset)* | **Path** to a file whose contents are the base64-encoded 32-byte key. Preferred in production — env vars leak through `docker inspect`, `ps`, systemd unit files, and shell history, while a file mounted as a Docker/K8s secret doesn't. Trailing whitespace/newlines are trimmed. Empty file = startup error (likely a broken secret mount). |

Setting both variables is a fatal configuration error — the two sources are mutually exclusive so a key rotation via the file can't silently be ignored.

Without either, these fields are stored as plaintext — fine for local development, strongly discouraged in shared/production environments. After setting the key for the first time on an existing deployment, call `POST /api/v1/admin/encrypt-existing` to migrate legacy plaintext rows. See [at-rest encryption](./api-reference.md#at-rest-encryption--how-it-works) for the full behaviour, including the fail-closed policy when the key is removed later.

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
