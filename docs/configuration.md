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

## Retries

Transient operations in the deploy and reconcile paths — image pulls, git clones, and Docker network creation — are retried with bounded exponential backoff and full jitter. A registry blip or a dropped connection turns into a successful deploy on the second try instead of a failed one.

| Variable | Default | Description |
|----------|---------|-------------|
| `RETRY_MAX_ATTEMPTS` | `3` | Total attempts including the first. `1` disables retrying. Clamped to `>=1`. |
| `RETRY_BASE_DELAY` | `1s` | Backoff before the second attempt; doubles each subsequent attempt. Go duration (`500ms`, `2s`). |
| `RETRY_MAX_DELAY` | `30s` | Ceiling on any single backoff sleep, so exponential growth can't produce absurd waits. |

Backoff for attempt *n* is a uniform random draw in `[0, min(RETRY_BASE_DELAY × 2^(n-1), RETRY_MAX_DELAY)]` — full jitter spreads retries from concurrent deploys so they don't thundering-herd a recovering registry.

Failures that a retry cannot fix are **not** retried — they fail fast:

- Image pulls: unauthorized, permission denied, or image-not-found.
- Git clones: authentication required / failed, repository not found, branch/reference not found.

Everything else (connection resets, timeouts, upstream 5xx, DNS hiccups) is treated as transient and retried up to the budget.

## Auto-deploy circuit breaker

Where retries handle a transient failure *within* one operation, the circuit breaker handles a stack that keeps failing *across* reconcile cycles (a bad image reference, revoked credentials, a malformed compose). Such a stack is left in `error` status but the reconciler keeps retrying it, so it **self-heals** once the underlying problem is fixed (typically a git push) — the breaker throttles those retries so a persistently-broken stack isn't redeployed every cycle.

| Variable | Default | Description |
|----------|---------|-------------|
| `CIRCUIT_BREAKER_THRESHOLD` | `5` | Consecutive reconcile-triggered deploy failures before a stack's breaker trips open. `0` disables the breaker (stacks retry every cycle). |
| `CIRCUIT_BREAKER_COOLDOWN` | `10m` | How long the breaker stays open before allowing one half-open trial deploy. Go duration. |

Lifecycle for a failing stack:

1. **Closed** — auto-deploys run normally. Each consecutive failure increments a counter.
2. **Open** — after `CIRCUIT_BREAKER_THRESHOLD` consecutive failures the breaker trips; auto-deploys are skipped (counted in `accelero_auto_deploys_skipped_total`, one `stack.circuit_breaker.opened` audit entry) until the cooldown elapses.
3. **Half-open** — after the cooldown, one trial deploy is allowed. Success closes the breaker (stack returns to `active`); failure re-opens it for another cooldown.

**Manual deploys are never gated by the breaker** — an operator triggering `POST /stacks/{id}/deploy` always runs. Trips are exposed as `accelero_circuit_breaker_tripped_total{stack}`.

## Native TLS

| Variable | Default | Description |
|----------|---------|-------------|
| `TLS_CERT_FILE` | *(unset — plain HTTP)* | Path to a PEM-encoded certificate (or certificate chain). |
| `TLS_KEY_FILE` | *(unset)* | Path to the matching PEM-encoded private key. |
| `TLS_HSTS_MAX_AGE` | `31536000` | `Strict-Transport-Security: max-age=<n>` value, in seconds. Set to `0` to disable. |

Both `TLS_CERT_FILE` and `TLS_KEY_FILE` must be set together — only one set is a fatal startup error rather than a silent downgrade. Unset = plain HTTP, the default. Many deployments terminate TLS at a reverse proxy (Caddy, nginx, Traefik) and don't need Accelero doing it twice; this setting exists for the cases that *do* — air-gapped hosts, single-binary deploys, dev environments.

**HSTS** is set on every response only when TLS is on. Accelero deliberately omits the `includeSubDomains` and `preload` directives — they affect every other service sharing the same origin and are effectively un-undoable for `preload`. Configure those at your edge proxy if you want them.

Generate a self-signed certificate for local development:

```bash
openssl req -x509 -nodes -newkey rsa:2048 \
  -keyout key.pem -out cert.pem -days 365 -subj "/CN=localhost" \
  -addext "subjectAltName=DNS:localhost,IP:127.0.0.1"
```

## Security response headers

| Variable | Default | Description |
|----------|---------|-------------|
| `CONTENT_SECURITY_POLICY` | `default-src 'none'; frame-ancestors 'none'` | Value of the `Content-Security-Policy` header. Set to an explicit empty string to omit the header. |

On every response Accelero always sets three defensive headers — they have no downside for a JSON API:

- `X-Content-Type-Options: nosniff` — stops a browser second-guessing a declared `Content-Type` (e.g. rendering a JSON error as HTML).
- `X-Frame-Options: DENY` — clickjacking defence, honoured by clients that ignore CSP.
- `Referrer-Policy: no-referrer` — Accelero URLs carry stack ids/names; don't leak them via the `Referer` header.

The **Content-Security-Policy** header is configurable. The default is locked down (`default-src 'none'`) because Accelero serves no browser UI today — nothing legitimate needs to load. Setting `CONTENT_SECURITY_POLICY=` (explicitly empty) omits the CSP header while keeping the three companions above, which is what you want behind an edge proxy that sets its own policy. Leaving the variable unset keeps the secure default.

Unlike HSTS, these headers are valid over both HTTP and HTTPS, so they are always sent.

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
