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

## Scheduled backups

Accelero can write periodic consistent snapshots of its SQLite database to a local directory (the same `VACUUM INTO` snapshot as the on-demand `POST /api/v1/admin/backup`). Disabled by default.

| Variable | Default | Description |
|----------|---------|-------------|
| `BACKUP_INTERVAL` | `0` *(disabled)* | How often to write a snapshot (Go duration, e.g. `6h`, `24h`). `0` disables the scheduler. When set, one snapshot is also written immediately on startup. |
| `BACKUP_DIR` | `./data/backups` | Directory the snapshots are written to (created if missing). |
| `BACKUP_KEEP` | `7` | Retain only the newest N snapshots; older ones are pruned after each backup. `0` keeps all. Only files named `accelero-backup-*.db`/`.db.age` are ever pruned. |
| `BACKUP_ENCRYPTION_PASSPHRASE` | *(unset — plaintext)* | When set, every snapshot (scheduled **and** `POST /admin/backup`) is [age](https://age-encryption.org)-encrypted with this passphrase. |

Snapshots are named `accelero-backup-<UTC timestamp>.db` (or `.db.age` when encrypted). Restore is a file swap: stop Accelero, copy a snapshot over the file at `DATABASE_PATH`, and start again.

**Encryption.** A snapshot contains everything Accelero persists — including repo tokens and per-stack secrets — so on-disk backups are a real exposure. Set `BACKUP_ENCRYPTION_PASSPHRASE` and every snapshot is written as a standard **age** stream (`.db.age`). Decrypt anywhere with the [`age`](https://github.com/FiloSottile/age) CLI:

```bash
age -d -o accelero.db accelero-backup-20260101T000000Z.db.age   # prompts for the passphrase
```

This is independent of `ACCELERO_ENCRYPTION_KEY` (which encrypts individual DB *fields*); backup encryption wraps the whole snapshot file. Without the passphrase set, snapshots are plaintext — point `BACKUP_DIR` somewhere protected.

**Remote destination (S3-compatible).** Set `BACKUP_S3_BUCKET` and each scheduled snapshot is uploaded off-host after it's written locally (so encryption applies to the uploaded copy too). Works with AWS S3, Cloudflare R2, MinIO, Backblaze B2, and GCS (interop mode).

| Variable | Default | Description |
|----------|---------|-------------|
| `BACKUP_S3_BUCKET` | *(unset — no upload)* | Target bucket. Setting it enables S3 upload for scheduled backups. |
| `BACKUP_S3_ENDPOINT` | *(AWS: `s3.<region>.amazonaws.com`)* | `host[:port]`, no scheme. Set for R2/MinIO/B2/GCS-interop. |
| `BACKUP_S3_REGION` | `us-east-1` | Signing region. |
| `BACKUP_S3_ACCESS_KEY_ID` / `BACKUP_S3_SECRET_ACCESS_KEY` | — | Credentials. |
| `BACKUP_S3_PREFIX` | *(none)* | Optional object-key prefix, e.g. `accelero/`. |
| `BACKUP_S3_USE_SSL` | `true` | HTTPS. Set `false` only for a local plaintext endpoint (e.g. dev MinIO). |

Upload is best-effort: a failed upload logs an error but never fails the local backup. **Remote retention is not managed by Accelero** — configure a bucket lifecycle policy to expire old objects (the idiomatic S3 approach). `BACKUP_KEEP` only prunes the local directory.

**Restore.** `POST /api/v1/admin/restore` uploads a snapshot (plaintext or `.db.age`) to replace the database. It's gated behind `ALLOW_RESTORE=true` (a wrong file is total data loss) and applies on the **next restart** — Accelero can't safely swap an open SQLite file live, so it validates + stages the upload and swaps it at startup, preserving the previous DB as `<DATABASE_PATH>.pre-restore-<timestamp>`. See the [API reference](api-reference.md#restore-the-database).

| Variable | Default | Description |
|----------|---------|-------------|
| `ALLOW_RESTORE` | `false` | Set to `true` to enable `POST /admin/restore`. Off by default because a bad restore wipes all data. |

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

## Approval gates

A stack created (or updated) with `requires_approval: true` holds every deploy behind a manual approval step instead of running it immediately. This applies to all triggers — manual `POST /deploy`, the legacy webhook, and reconcile auto-deploy.

When a deploy is triggered on such a stack, Accelero creates a deployment in the `pending_approval` state, emits an `approval.requested` audit event (delivered as a notification), and does **not** execute. At most one approval is open per stack at a time — repeated triggers (e.g. the reconcile loop firing every cycle) return the existing one rather than spawning duplicates or re-notifying.

An operator then acts via the API:

| Endpoint | Effect |
|----------|--------|
| `POST /api/v1/stacks/{id}/deployments/{deployId}/approve` | Runs the held deploy (202; rollout is async). Audited `approval.granted`. |
| `POST /api/v1/stacks/{id}/deployments/{deployId}/reject` | Cancels it. Optional body `{"reason": "..."}`. Audited `approval.rejected`. |
| `GET /api/v1/approvals` | Lists all pending approvals across every stack, oldest first. |

| Variable | Default | Description |
|----------|---------|-------------|
| `APPROVAL_TIMEOUT` | `24h` | How long a pending approval waits before being auto-rejected (`approval.timed_out`). Go duration. `0` disables expiry — approvals wait indefinitely. |

Each lifecycle event (`approval.requested` / `granted` / `rejected` / `timed_out`) is written to the audit log and, if notifications are configured, delivered to the webhook/Slack sinks. `deploy.start` is deliberately *not* emitted for a gated stack, so you don't get a contradictory "deploy started" + "awaiting approval" pair.

## Notifications

Accelero can push notable lifecycle events to external destinations. Set any of the URLs below to enable that sink; sinks fire independently and with none set, notifications are off (zero overhead).

| Variable | Default | Description |
|----------|---------|-------------|
| `NOTIFY_WEBHOOK_URL` | *(unset — disabled)* | Generic webhook. Receives the full event as JSON via `POST`. |
| `NOTIFY_SLACK_WEBHOOK_URL` | *(unset — disabled)* | Slack [incoming webhook](https://api.slack.com/messaging/webhooks). Receives a `{"text": "<message>"}` payload. |
| `NOTIFY_DISCORD_WEBHOOK_URL` | *(unset — disabled)* | Discord [webhook](https://support.discord.com/hc/en-us/articles/228383668). Receives a `{"content": "<message>"}` payload. |
| `NOTIFY_TEAMS_WEBHOOK_URL` | *(unset — disabled)* | Microsoft Teams incoming webhook. Receives a `MessageCard` (`{"@type":"MessageCard", ..., "text":"<message>"}`), the portable form across connector versions. |

**Email (SMTP).** The email sink is enabled when `NOTIFY_SMTP_HOST`, `NOTIFY_EMAIL_FROM`, and `NOTIFY_EMAIL_TO` are all set. It sends one plain-text message per event.

| Variable | Default | Description |
|----------|---------|-------------|
| `NOTIFY_SMTP_HOST` | *(unset — disabled)* | SMTP server hostname. Setting it (with FROM + TO) enables the email sink. |
| `NOTIFY_SMTP_PORT` | `587` | Port. `465` uses implicit TLS; any other port uses STARTTLS when the server advertises it (else plaintext). |
| `NOTIFY_SMTP_USERNAME` / `NOTIFY_SMTP_PASSWORD` | — | Credentials. When a username is set, PLAIN auth is used. Omit both for an unauthenticated relay. |
| `NOTIFY_EMAIL_FROM` | *(unset)* | Envelope + header `From` address. |
| `NOTIFY_EMAIL_TO` | *(unset)* | Comma-separated recipient list. |

**Events notified:** deploy started, completed, failed, rolled back; drift detected; auto-deploy triggered. Read-only, secret-CRUD, and other audit operations are deliberately *not* notified (avoids channel spam).

**Delivery semantics:** best-effort and asynchronous — each sink fires in its own goroutine with a 10s timeout. A failing or slow sink logs a warning and is never allowed to block or fail a deploy. There are no retries and no ordering guarantees.

**Generic webhook payload** (`NOTIFY_WEBHOOK_URL`):

```json
{
  "type": "deploy.failed",
  "stack": "web",
  "outcome": "failure",
  "message": "❌ Deploy failed for stack `web`: git clone failed: ...",
  "fields": { "changes": "3" },
  "time": "2026-07-21T10:04:05Z"
}
```

`type` is the audit operation (`deploy.start`, `deploy.complete`, `deploy.failed`, `deploy.rolled_back`, `drift.detected`, `drift.auto_deployed`); `fields` mirrors the audit metadata. Implemented in `internal/notify` by wrapping the shared audit recorder, so the deployer and reconciler need no changes — anything that records one of these audit events is notified automatically.

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
