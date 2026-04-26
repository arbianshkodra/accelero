# API Reference

All endpoints (except `GET /health`) require the `X-API-KEY` header for authentication.

## Stacks

### Create Stack
`POST /api/v1/stacks`

**Request body:**
```json
{
  "name": "my-app",
  "repo_url": "https://github.com/org/repo",
  "repo_username": "user",
  "repo_token": "token",
  "repo_branch": "main",
  "compose_path": "docker-compose.yaml",
  "service_filter": "",
  "auto_deploy": true,
  "reconcile_interval_seconds": 300,
  "docker_username": "",
  "docker_password": "",
  "docker_registry": ""
}
```

**Response:** `201 Created`
```json
{
  "id": "a1b2c3d4...",
  "name": "my-app",
  "repo_url": "https://github.com/org/repo",
  "status": "active",
  "auto_deploy": true,
  "reconcile_interval_seconds": 300,
  "created_at": "2025-01-15T10:30:00Z",
  "updated_at": "2025-01-15T10:30:00Z"
}
```

---

### List Stacks
`GET /api/v1/stacks`

**Response:** `200 OK`
```json
[
  {
    "id": "a1b2c3d4...",
    "name": "my-app",
    "status": "active",
    "auto_deploy": true,
    "last_deployed_at": "2025-01-15T12:00:00Z",
    "git_commit": "abc1234"
  }
]
```

---

### Get Stack
`GET /api/v1/stacks/{id}`

The `{id}` parameter accepts either the stack ID or the stack name.

**Response:** `200 OK` — Full stack object

**Error:** `404 Not Found` if the stack doesn't exist

---

### Update Stack
`PUT /api/v1/stacks/{id}`

Only include the fields you want to change. All fields are optional.

**Request body:**
```json
{
  "auto_deploy": false,
  "reconcile_interval_seconds": 600
}
```

**Response:** `200 OK` — Updated stack object

---

### Delete Stack
`DELETE /api/v1/stacks/{id}`

**Response:** `200 OK`
```json
{"status": "deleted"}
```

---

## Deployments

### Trigger Deployment
`POST /api/v1/stacks/{id}/deploy`

Triggers an asynchronous deployment for the stack. Returns immediately with a `202 Accepted`.

**Response:** `202 Accepted`
```json
{
  "status": "accepted",
  "stack_id": "a1b2c3d4...",
  "message": "Deployment started. Check deployments for progress."
}
```

**Error:** `409 Conflict` if a deployment is already in progress for this stack.

---

### List Deployments
`GET /api/v1/stacks/{id}/deployments`

Returns the 50 most recent deployments for the stack, newest first.

**Response:** `200 OK`
```json
[
  {
    "id": "d1e2f3...",
    "stack_id": "a1b2c3d4...",
    "stack_name": "my-app",
    "status": "completed",
    "trigger": "manual",
    "git_commit": "abc1234def5678",
    "changes": "web -> nginx:1.27.0; api -> myapp:v2.1.0",
    "started_at": "2025-01-15T12:00:00Z",
    "completed_at": "2025-01-15T12:01:30Z"
  }
]
```

**Deployment statuses:** `pending`, `in_progress`, `completed`, `failed`, `rolled_back`

**Trigger types:** `webhook`, `reconcile`, `manual`

---

### Check Drift
`GET /api/v1/stacks/{id}/drift`

Performs a one-off drift check comparing the desired state (git compose file) against the actual running containers.

**Response:** `200 OK`
```json
{
  "stack_id": "a1b2c3d4...",
  "stack_name": "my-app",
  "checked_at": "2025-01-15T12:05:00Z",
  "has_drift": true,
  "drifts": [
    {
      "service_name": "web",
      "type": "image_mismatch",
      "expected": "nginx:1.27.0",
      "actual": "nginx:1.26.1",
      "message": "container abc123 has image nginx:1.26.1, expected nginx:1.27.0"
    },
    {
      "service_name": "worker",
      "type": "missing",
      "expected": "myapp-worker:latest",
      "message": "service worker is defined in compose but has no running container"
    }
  ]
}
```

**Drift types:**

| Type | Description |
|------|-------------|
| `missing` | Service defined in compose but no container running, or fewer containers than the declared `deploy.replicas` |
| `image_mismatch` | Container running different image than declared |
| `stopped` | Container exists but is not in `running` state |
| `unhealthy` | Container is failing its health check |
| `extra` | Container exists for a service not defined in compose, or surplus replicas beyond the declared `deploy.replicas` |
| `missing_external` | `external: true` resource (e.g. volume) is declared but missing on the host |

---

### List Containers
`GET /api/v1/stacks/{id}/containers`

Returns every container Accelero manages for the stack. Filtered by the `managed-by=accelero` + `accelero-stack=<name>` labels — unmanaged containers never appear, even if they share a name.

**Response:** `200 OK`
```json
[
  {
    "id": "0c6bece9f132...",
    "name": "web_0_1776690394074826000",
    "image": "nginx:1.27.2",
    "service": "web",
    "replica": 0,
    "state": "running",
    "status": "Up 2 minutes",
    "health": "healthy",
    "created_at": "2026-04-20T13:06:34Z",
    "ports": [{"container_port": 80, "protocol": "tcp", "host_port": 8080, "host_ip": "127.0.0.1"}],
    "labels": {"accelero-replica": "0", "accelero-service": "web", "accelero-stack": "my-app", "managed-by": "accelero"}
  }
]
```

Results are sorted by `service` then `replica` so the order is deterministic across scrapes.

**Errors:** `404 Not Found` if the stack doesn't exist; `503 Service Unavailable` if Docker introspection isn't configured on this server.

---

### Inspect Container
`GET /api/v1/stacks/{id}/containers/{cid}`

Full detail for a single container. `{cid}` accepts the full ID, the short 12-char prefix, or the container name.

**Security:** the container's `managed-by` + `accelero-stack` labels are verified before anything is returned. Guessing a container ID that belongs to a different stack returns `404 Not Found` — the error is intentionally indistinguishable from "doesn't exist" so callers cannot probe for foreign containers.

**Response:** `200 OK`
```json
{
  "id": "0c6bece9f132...",
  "name": "web_0_1776690394074826000",
  "image": "sha256:bc5eac5e...",
  "service": "web",
  "replica": 0,
  "state": "running",
  "created_at": "2026-04-20T13:06:34.112Z",
  "cmd": ["nginx", "-g", "daemon off;"],
  "entrypoint": ["/docker-entrypoint.sh"],
  "env": ["PATH=/usr/bin", "DB_PASSWORD=***", "LOG_LEVEL=info"],
  "working_dir": "/",
  "restart_count": 0,
  "restart_policy": "no",
  "started_at": "2026-04-20T13:06:34.138Z",
  "exit_code": 0,
  "networks": {
    "bridge": {"network_id": "abcd1234...", "ip_address": "172.17.0.3", "aliases": ["web"]}
  },
  "mounts": [],
  "labels": {"accelero-replica": "0", "accelero-service": "web", "accelero-stack": "my-app", "managed-by": "accelero"}
}
```

**Env redaction.** Any env var whose *key* matches `PASSWORD`, `TOKEN`, `SECRET`, `APIKEY` / `API_KEY`, `PRIVATE`, or `CREDENTIAL` (case-insensitive substring) has its value replaced with `***`. This is best-effort hygiene — the assumption remains that real secrets live in an `env_file:` populated by host tooling, not inline `environment:`.

---

### Container Logs
`GET /api/v1/stacks/{id}/containers/{cid}/logs`

Returns the container's log tail as `text/plain; charset=utf-8`. Stdout and stderr are combined in chronological order. Does not follow — WebSocket streaming is planned as a separate endpoint.

**Query parameters:**

| Param | Type | Default | Notes |
|-------|------|---------|-------|
| `tail` | integer | `100` | Number of most-recent lines to return. Capped at 10000. |
| `since` | Go duration | unset | `5m`, `30s`, `2h`. Only lines emitted within that window are returned. |
| `timestamps` | bool | `false` | Prefix every line with Docker's RFC3339Nano timestamp. |

Same stack-membership verification as `GET /containers/{cid}` — a `cid` from a different stack returns `404 Not Found`.

**Errors:** `400 Bad Request` for malformed `tail` / `since`; `404 Not Found` for missing / foreign-stack container; `503 Service Unavailable` when Docker introspection isn't configured.

---

### Audit Log
`GET /api/v1/audit`

Returns audit entries newest-first. Every notable write action — stack CRUD, deploy lifecycle, reconciler drift observations, auto-deploys — produces an immutable row. The table is append-only at the store layer; there is no write/delete API.

**Query parameters:**

| Param | Type | Notes |
|-------|------|-------|
| `stack` | string | Stack name or ID. Unknown values fall back to name-match so entries from already-deleted stacks still surface. |
| `actor` | string | Exact match on actor (`api-key`, `system:reconciler`, `system:deployer`). |
| `operation` | string | Exact match — see the operation table below. |
| `since` | Go duration | Only entries within the last N. `24h`, `5m`. |
| `limit` | integer | Default 100, store-capped at 1000. |

**Response:** `200 OK`
```json
[
  {
    "id": "3f8a9d...",
    "timestamp": "2026-04-20T21:06:59Z",
    "actor": "api-key",
    "remote_addr": "10.0.0.1:55555",
    "request_id": "cf4f29994be24c04",
    "operation": "stack.delete",
    "resource_type": "stack",
    "resource_id": "s1",
    "stack_id": "s1",
    "stack_name": "a",
    "outcome": "success"
  },
  {
    "id": "9b1e...",
    "timestamp": "2026-04-20T21:06:54Z",
    "actor": "system:deployer",
    "operation": "deploy.complete",
    "resource_type": "deployment",
    "resource_id": "d1",
    "stack_id": "s1",
    "stack_name": "a",
    "outcome": "success",
    "metadata": {
      "trigger": "manual",
      "duration_seconds": "1.830",
      "changes": "web -> nginx:1.27.1-alpine"
    }
  }
]
```

**Operation catalogue:**

| Operation | Actor | Notes |
|-----------|-------|-------|
| `stack.create` / `stack.update` / `stack.delete` | `api-key` | HTTP CRUD on stacks |
| `deploy.start` | `api-key` | Outcome `in_progress`; emitted at request time. Metadata: `trigger` (manual/webhook/reconcile). |
| `deploy.complete` / `deploy.failed` / `deploy.rolled_back` | `system:deployer` | Emitted at deployer-finish time. Metadata: `trigger`, `duration_seconds`, `changes` (on success), `git_commit`. |
| `drift.detected` | `system:reconciler` | One per reconcile cycle with drift (not per drift item — kept compact). Metadata: `drift_count`, per-type counts (`drift_type_missing`, `drift_type_image_mismatch`, etc.). |
| `drift.auto_deployed` | `system:reconciler` | Emitted when auto-deploy fires on drift. Metadata: `drift_count`. The resulting deploy then emits its own `deploy.*` entries. |
| `admin.encrypt-existing` | `api-key` | Re-saves pre-encryption plaintext rows through the cipher. Metadata: `stacks_migrated`, `stacks_failed`. Outcome is `failure` if any row failed. |
| `stack.secret.set` | `api-key` | Upsert of a per-stack secret. Metadata: `rewrote_existing`. **Value is never included.** |
| `stack.secret.delete` | `api-key` | Delete of a per-stack secret. Always recorded — failures carry `error_message: "not found"` when the caller tried to delete a non-existent key. |

**Retention.** Entries older than `AUDIT_MAX_AGE` (default 90 days) are pruned on the same cadence as the deployment-history cleanup (`STATUS_CLEANUP_INTERVAL`, default hourly). Set `AUDIT_MAX_AGE=0` to disable retention — useful when a compliance regime requires indefinite preservation.

**Correlation with logs.** Every entry carries the `request_id` (for HTTP-originated events) or a `system:<component>` actor (for reconciler/deployer events). The same `request_id` appears on every log line from the same request, so `grep cf4f29994be24c04` in logs gives the full context behind an audit row.

---

### List Managed Images
`GET /api/v1/images`

Returns every Docker image currently referenced by at least one accelero-managed container, with back-references to the containers using it. Docker images themselves don't carry management labels, so "managed" here means "used by a managed container" — the endpoint walks containers first, then intersects with the daemon's image list.

**Query parameters:**

| Param | Type | Notes |
|-------|------|-------|
| `stack` | string | Narrow usage to one stack name |

**Response:** `200 OK` (sorted by primary repo tag)
```json
[
  {
    "id": "sha256:a5127daff3d6...",
    "repo_tags": ["nginx:1.27.1-alpine"],
    "size_bytes": 71807554,
    "created_at": "2024-08-14T23:51:24Z",
    "used_by": [
      {"stack": "rb", "service": "web", "replica": 0, "container_id": "720e96..."},
      {"stack": "rb", "service": "web", "replica": 1, "container_id": "a9c4c2..."}
    ]
  }
]
```

**Errors:** `503 Service Unavailable` when Docker introspection isn't configured.

---

### List Managed Volumes
`GET /api/v1/volumes`

Volumes tagged `managed-by=accelero`. Narrow with `?stack=<name>`.

**Response:** `200 OK` (sorted by volume name)
```json
[
  {
    "name": "accelero_rb_pg_data",
    "driver": "local",
    "stack": "rb",
    "mount_point": "/var/lib/docker/volumes/accelero_rb_pg_data/_data",
    "created_at": "2026-04-20T17:17:53Z",
    "labels": {"accelero-stack": "rb", "managed-by": "accelero"}
  }
]
```

Daemon warnings from the volume list (rare; usually filesystem-level) are surfaced as log entries, not in the response.

---

### Browse a Managed Volume
`GET /api/v1/volumes/{name}/browse`

List files in a managed volume or download a single file's contents. Debug affordance for "what's actually in my postgres data dir" scenarios — read-only, audited, size-capped.

**How it works.** Accelero spawns an ephemeral `busybox:stable` helper container with the target volume mounted read-only at `/volume`, extracts the requested path via Docker's archive API, parses the tar stream, and tears the helper down. The image is auto-pulled on first use; subsequent calls reuse the cached copy. Direct file-system reads of `/var/lib/docker/volumes/...` aren't portable — Docker Desktop keeps those paths inside a VM — so the helper-container trick is the only mechanism that works everywhere.

**Query parameters:**

| Param | Type | Default | Notes |
|-------|------|---------|-------|
| `path` | string | `/` | Absolute within the volume. `..` segments are rejected. |
| `download` | bool | `false` | `true` streams file bytes; omitted returns a JSON listing. |

**List response** (`download=false`): `200 OK`, array of entries — direct children of the requested directory, no recursion.
```json
[
  {
    "name": "config",
    "path": "/config",
    "is_dir": true,
    "size_bytes": 0,
    "mode": "-rwxr-xr-x",
    "mod_time": "2026-04-20T21:40:29Z"
  },
  {
    "name": "greet.txt",
    "path": "/greet.txt",
    "is_dir": false,
    "size_bytes": 18,
    "mode": "-rw-r--r--",
    "mod_time": "2026-04-20T21:40:29Z"
  }
]
```

Listings are capped at 10,000 entries (silent truncation). If you need deeper views, browse directory-by-directory.

**Download response** (`download=true`): `200 OK` with `Content-Type: application/octet-stream`, `Content-Disposition: attachment; filename="..."`, and the file bytes as the body. Files larger than 10 MB are refused with `400` — extract those manually via `docker cp` instead.

**Security:**

- Only volumes labelled `managed-by=accelero` are browseable. Unmanaged volumes return `404` indistinguishably from missing volumes so callers can't probe the host.
- Path traversal (`..`) is rejected at the handler before the browser sees it, and again by the browser itself. Two layers.
- The helper container mounts the volume **read-only** — nothing the browser does can modify volume contents.
- Helper containers are cleaned up immediately when the request finishes, including on error paths. They carry `managed-by=accelero` + `accelero-helper=volume-browser` labels for leak detection.

**Audit.** Every call writes one entry: `volume.browse` for listings, `volume.read` for downloads. Both include the path in metadata; downloads additionally record `size_bytes`. Outcome reflects whether the underlying operation succeeded; failures are audited the same way as successes so the trail captures intent.

**Errors:** `400 Bad Request` for malformed paths (including `..`) or oversize downloads; `404 Not Found` for unmanaged or missing volumes; `503 Service Unavailable` when Docker or the browser isn't configured.

---

### Write a File to a Managed Volume
`POST /api/v1/volumes/{name}/files`

Write (create or overwrite) a single file inside a managed volume. **Disabled by default** — must be explicitly enabled with `ALLOW_VOLUME_WRITES=true` in Accelero's env. Every call is audited regardless of outcome.

This is an **emergency-patch affordance**. Not a distribution channel, not a config management system. Use it when you need to patch a Caddyfile at 3am because a cert expired, or drop a single env file into a volume before the next deploy picks it up. Anything persistent and observable belongs in your gitops repo; things committed to git survive a rebuild, things written through this endpoint don't.

**Query parameters:**

| Param | Type | Default | Notes |
|-------|------|---------|-------|
| `path` | string | **required** | Absolute within the volume; `..` segments rejected. Must point at a file, not a directory. |
| `mode` | octal | `0644` | POSIX file mode, e.g. `0600`, `0644`. |

**Request body:** raw file bytes. Content-Type is ignored and not stored. 10 MB cap enforced via `http.MaxBytesReader` — larger writes return `413 Request Entity Too Large` before the tar stream even starts.

**Behaviour:**

- **Parent dirs auto-created.** A request to write `/config/deep/nested.yml` creates `/config/` and `/config/deep/` as `0755` if they don't exist yet.
- **Overwrites existing files.** There is no "don't clobber" option.
- **Read-write helper.** Same ephemeral busybox helper the read-only browse uses, but mounted read-write for the duration of this single write. Torn down immediately after.

**Response:** `200 OK`
```json
{
  "status": "written",
  "path": "/config/deep/nested.yml",
  "size_bytes": 42
}
```

**Audit.** One `volume.write` entry per call with outcome (`success` or `failure`), path, size_bytes, and mode in metadata. Failures — including traversal rejections, oversize uploads, and daemon errors — also leave an audit row so the trail captures intent.

**Errors:** `400 Bad Request` for missing path, `..` segments, invalid `mode`, or any browser-side error; `403 Forbidden` when `ALLOW_VOLUME_WRITES` is off; `404 Not Found` for unmanaged volumes; `413 Request Entity Too Large` for bodies exceeding 10 MB; `503 Service Unavailable` when Docker isn't configured.

**Example:**
```bash
# Patch a Caddyfile and reload it — replace with docker exec nginx -s reload
# after the write since mounted file changes don't automatically trigger a
# config reload on the running container.
curl -X POST \
  -H "X-API-Key: $ACCELERO_API_KEY" \
  --data-binary @new-Caddyfile \
  "http://localhost:8000/api/v1/volumes/accelero_app_caddy/files?path=/Caddyfile&mode=0644"
```

---

### List Managed Networks
`GET /api/v1/networks`

Networks tagged `managed-by=accelero`. Narrow with `?stack=<name>`.

**Note on upgrades.** Networks created by accelero before the labelling fix landed will NOT appear here — Docker doesn't allow adding labels to a live network without a recreate, which would disrupt every container on it. Those networks remain functional; they just won't show up in this listing until the stack is torn down and re-deployed.

**Response:** `200 OK` (sorted by network name)
```json
[
  {
    "id": "1f1568dd73e6...",
    "name": "app",
    "driver": "bridge",
    "scope": "local",
    "stack": "rb",
    "created_at": "2026-04-20T17:31:00Z",
    "labels": {"accelero-stack": "rb", "managed-by": "accelero"}
  }
]
```

Connected-container enumeration is deferred — `/api/v1/stacks/{id}/containers` already answers "which containers belong to this stack." Ask in an issue if a per-network "who's on me" view would be useful.

---

### Stream Container Stats (WebSocket)
`GET /api/v1/stacks/{id}/containers/{cid}/stats/stream`

WebSocket upgrade that pushes one computed stats sample per Docker sampling tick (~1s cadence). Each message is the same `ContainerStatsSample` JSON shape as the one-shot `/stats` response, so a live graph and a polling dashboard can share one parser.

The first sample typically reports `cpu.percent=0` — Docker sends the initial sample before having a prior one to diff against. Subsequent samples populate `PreCPUStats` server-side and the CPU percentage reflects real usage.

**Authentication.** Same X-API-KEY on the upgrade GET as the rest of the API. See [Stream Container Logs (WebSocket)](#stream-container-logs-websocket) for browser-specific caveats.

**Lifecycle:**

- 30s application pings; read deadline 2x that interval.
- Clean `CloseNormalClosure` frame when the daemon closes its stats stream (container exit, removal).
- Client disconnect cancels the upstream Docker context; daemon releases within ~1s.

Same stack-membership verification as the other container endpoints: a cid from a different stack returns `404 Not Found` before the upgrade completes.

**Errors:** `404 Not Found` for missing / foreign-stack container; `503 Service Unavailable` when Docker introspection isn't configured.

**Example (websocat):**

```bash
websocat \
  -H "X-API-Key: $ACCELERO_API_KEY" \
  "ws://localhost:8000/api/v1/stacks/s/containers/$CID/stats/stream"
```

---

### Restart a Container
`POST /api/v1/stacks/{id}/containers/{cid}/restart`

Restart a single managed container. This is the first mutating debug endpoint — it bypasses the GitOps flow (no compose change, no deployment record) but is always audited.

**When to use it:** sparingly. Valid cases are "stuck process, give it another kick" and "temporarily cleared a wedged state for diagnosis." For anything that changes desired state — image tags, replica counts, config — commit to git and redeploy.

**Query parameters:**

| Param | Type | Default | Notes |
|-------|------|---------|-------|
| `t` | integer seconds | Docker default (10s) | Grace period before SIGKILL. `-1` waits forever, `0` kills immediately. |

**Response:** `202 Accepted`
```json
{
  "status": "accepted",
  "container_id": "25a7ef422c8e..."
}
```

The 202 reflects that Docker has *received* the restart command and begun stop/start — the container may still be transitioning when the response lands. Poll `/containers/{cid}` to see the new state once the transition completes.

Same stack-membership verification as the other container endpoints: a cid from a different stack returns `404 Not Found` and no Docker call is issued.

**Audit.** Every call writes one `container.restart` audit entry regardless of outcome (success or daemon error). The entry carries actor = `api-key`, resource_id = container ID, request_id (for log correlation), and metadata with the service name, replica index, and timeout if supplied.

**Errors:** `400 Bad Request` for malformed `t`; `404 Not Found` for missing / foreign-stack container; `500 Internal Server Error` when the Docker daemon refuses the operation (audit entry still written, with `outcome: failure` and the error message); `503 Service Unavailable` when Docker introspection isn't configured.

---

### Stream Events
`GET /api/v1/stacks/{id}/events`

Server-Sent Events stream of Docker events filtered to this stack's managed resources. Useful for live dashboards ("show me what's happening right now") and for wiring up notifications (`die` on a production replica → page on-call).

**Query parameters:**

| Param | Type | Default | Notes |
|-------|------|---------|-------|
| `since` | Go duration | unset | Only emit events newer than N ago, e.g. `5m`, `30s`. |
| `types` | CSV | `container` | Docker event types to include — any of `container`, `network`, `volume`, `image`. Default is container-only; opt in to the rest. |

**Response:** `200 OK` with `Content-Type: text/event-stream`. Each event is one SSE frame:

```
data: {"time":"2026-04-20T16:10:30.12Z","type":"container","action":"start","actor_id":"abc...","name":"web_0_...","image":"nginx:1.27.2-alpine","service":"web","replica":0,"attributes":{...}}

data: {"time":"2026-04-20T16:10:30.22Z","type":"container","action":"die","actor_id":"def...","name":"web_1_...","image":"nginx:1.27.1-alpine","service":"web","replica":1,"attributes":{...}}
```

Typical rolling update on a replicated service emits the expected cadence:

```
create / start  (new replica 0)
kill / stop / die / destroy  (old replica 1)
create / start  (new replica 1)
kill / stop / die / destroy  (old replica 0)
```

**Keepalive.** A comment line (`: keepalive\n\n`) is written every 25 seconds during quiet periods so idle timeouts on reverse proxies don't kill the stream. Clients using `EventSource` handle this transparently.

**Connection lifetime.** The stream runs until the client disconnects or the Docker daemon closes the underlying event stream. On disconnect, Accelero cancels the upstream events context and cleans up within ~1s.

**Errors:** `400 Bad Request` for malformed `since`; `404 Not Found` if the stack doesn't exist; `503 Service Unavailable` when Docker introspection isn't configured.

---

### Exec (WebSocket)
`GET /api/v1/stacks/{id}/containers/{cid}/exec`

Upgrade the connection to a WebSocket and run a command inside a container with stdin/stdout/stderr wired back to the client. The heaviest debug endpoint — the thing operators reach for when logs, stats, and restart couldn't answer the question.

**Query parameters:**

| Param | Type | Default | Notes |
|-------|------|---------|-------|
| `cmd` | string, **repeatable** | — | The command and its arguments. At least one required. Examples: `?cmd=sh`, `?cmd=sh&cmd=-c&cmd=ls+-la`. |
| `tty` | bool | `true` | Interactive shells need a TTY; non-interactive commands are better served by `tty=false`. |
| `user` | string | container default | Runs as this user inside the container. |
| `workdir` | string | container default | Starts in this working directory. |

**WebSocket protocol:**

- **Binary messages from the client** → stdin. Write anything you'd normally type into the command.
- **Text messages from the client** → control frames (JSON). See the "Control frames" subsection below.
- **Binary messages from the server** → container output. In TTY mode the daemon's output is a raw stream, copied through as-is (ANSI colour codes and all). In non-TTY mode stdout/stderr are multiplexed with Docker's 8-byte frame headers; the handler demuxes them server-side and merges both into one WS stream so callers see clean text either way.
- On exit: server sends a `CloseNormalClosure` frame with the exit code in the reason text, e.g. `"exit_code=0"` or `"exit_code=42"`.

**Control frames** (client → server, as WebSocket TextMessage, JSON body):

| Type | Payload | Notes |
|------|---------|-------|
| `resize` | `{"type":"resize","rows":40,"cols":120}` | Resizes the TTY via Docker's `ExecResize`. No-op in non-TTY mode so clients can send unconditionally. |

Unknown `type` values are silently ignored — clients can send forward-compatible frames without fear of tripping up older servers.

**Authentication.** Same `X-API-KEY` header as the rest of the API. Browsers can't set custom headers on `new WebSocket()` — for now, exec from a server-side process or via a proxy that injects the header.

**Lifecycle guarantees:**

- 30-second application pings, read deadline 2× that interval — detects half-closed TCPs the kernel hasn't noticed.
- Client disconnect closes the stdin write side so tools that exit on stdin EOF terminate cleanly; Docker tears the exec down immediately afterward.
- Container death closes the exec connection, which closes the WebSocket with the final exit code.

**Audit — two rows per session:**

- `container.exec_start` on connect — outcome `in_progress`, metadata includes `cmd`, `tty`, `user`, `workdir`, `exec_id`.
- `container.exec_end` on disconnect — outcome `success` if exit_code was 0 and the stream didn't error; `failure` otherwise. Metadata adds `exit_code` and `duration_seconds` and inherits the start row's cmd/user/workdir.

Pre-existing audit queries (`/audit?operation=container.exec_start`, `operation=container.exec_end`) give you the session history; the two rows share a `request_id` so `grep <request_id>` in logs stitches the timeline together.

**This bypasses GitOps.** Exec is a debug affordance, never a substitute for compose changes + redeploy. Anything observable-and-persistent — image tags, replica counts, config — belongs in git. Exec exists so you can answer *why* the pod behaved the way it did, not to change state in place. The audit trail makes that answerable after the fact.

**Errors:** `400 Bad Request` for missing `cmd`; `404 Not Found` for missing / foreign-stack container; `500 Internal Server Error` if `ExecCreate` or `ExecAttach` fails (the `exec_end` audit row still gets written with `outcome: failure`); `503 Service Unavailable` when Docker introspection isn't configured.

**Example (websocat):**
```bash
websocat \
  -H "X-API-Key: $ACCELERO_API_KEY" \
  "ws://localhost:8000/api/v1/stacks/demo/containers/$CID/exec?cmd=sh"
```

---

### Stream Container Logs (WebSocket)
`GET /api/v1/stacks/{id}/containers/{cid}/logs/stream`

WebSocket upgrade that follows a container's logs in real time. Each log line arrives as one text message. Non-TTY containers have their multiplexed stdout/stderr demuxed server-side so callers see clean text either way.

**Query parameters** (same surface as `/logs`):

| Param | Type | Default | Notes |
|-------|------|---------|-------|
| `tail` | integer | `100` | Backlog lines emitted before live tailing begins. Capped at 10000. |
| `since` | Go duration | unset | `5m`, `30s`, `2h`. |
| `timestamps` | bool | `false` | Prefix every line with RFC3339Nano timestamps. |

**Authentication.** Upgrade is a regular HTTP GET — `X-API-KEY` header is checked by the usual auth middleware. Browsers can't set custom headers on `new WebSocket(url)`; a short-lived-token flow for browser clients is planned.

**Lifecycle:**

- The server pings every 30s so the OS surfaces half-closed TCPs.
- The server closes with code `1000` (normal closure) when the daemon's log stream ends cleanly — container exit, explicit removal, etc.
- A read goroutine watches for client-initiated close, cancels the upstream Docker stream immediately, and the daemon releases within ~1s.

Same stack-membership verification as the rest of the container endpoints: a cid from a different stack returns `404 Not Found` at the upgrade stage, so the WebSocket handshake never completes.

**Errors:** `400 Bad Request` for malformed `tail` / `since`; `404 Not Found` for missing / foreign-stack container; `503 Service Unavailable` when Docker introspection isn't configured. All of these prevent the upgrade.

**Example (websocat):**

```bash
websocat \
  -H "X-API-Key: $ACCELERO_API_KEY" \
  "ws://localhost:8000/api/v1/stacks/ws/containers/$CID/logs/stream?tail=50&timestamps=true"
```

---

### Container Stats
`GET /api/v1/stacks/{id}/containers/{cid}/stats`

Returns a single resource-usage sample with CPU%, memory, per-interface network totals, block I/O totals, and PID count. No streaming — poll this endpoint from a dashboard (~1 second per call; the daemon intentionally collects two samples so the CPU delta is valid).

**Response:** `200 OK`
```json
{
  "container_id": "9b5adf3ee871...",
  "name": "web_0_1776691620235583000",
  "read_at": "2026-04-20T13:27:41.638Z",
  "cpu": {
    "percent": 100.02,
    "online_cpus": 14,
    "total_usage_ns": 4145907000,
    "system_usage_ns": 203373070000000
  },
  "memory": {
    "usage_bytes": 12500992,
    "limit_bytes": 134217728,
    "percent": 9.31
  },
  "networks": {
    "eth0": {"rx_bytes": 1172, "tx_bytes": 126}
  },
  "block_io": {"read_bytes": 0, "write_bytes": 12288},
  "pids": 16
}
```

**CPU percent** uses the same formula as `docker stats`:

```
(cpuDelta / systemDelta) * onlineCPUs * 100
```

Values above 100 are valid — they indicate the container is using more than one core's worth of CPU time.

**Memory usage** subtracts page cache (`cache` on cgroup v1, `file` on cgroup v2) before reporting, matching the CLI. This reflects the "real" working-set pressure rather than reclaimable cache.

Same stack-membership verification as `/containers/{cid}` — a cid from a different stack returns `404 Not Found`.

**Errors:** `404 Not Found` for missing / foreign-stack container; `503 Service Unavailable` when Docker introspection isn't configured.

---

### Preview Deployment
`POST /api/v1/stacks/{id}/preview`

Runs the same drift check as `/drift` and translates each drift item into the action the next deployment would take. No containers are touched — this is a read-only "what would happen if I deployed right now" endpoint, useful for PR review flows and pre-deploy sanity checks.

**Response:** `200 OK`
```json
{
  "stack_id": "a1b2c3d4...",
  "stack_name": "my-app",
  "checked_at": "2025-01-15T12:05:00Z",
  "has_changes": true,
  "actions": [
    {
      "service_name": "web",
      "action": "recreate",
      "reason": "container abc123 has image nginx:1.27.2, expected nginx:1.27.3",
      "expected": "nginx:1.27.3",
      "actual": "nginx:1.27.2"
    },
    {
      "service_name": "worker",
      "action": "create",
      "reason": "service worker is defined in compose but has no running container",
      "expected": "myapp-worker:latest"
    }
  ]
}
```

**Action types:**

| Action | Triggered by | Meaning |
|--------|--------------|---------|
| `create` | `missing` | New container will be started |
| `recreate` | `image_mismatch` | Existing container will be replaced |
| `restart` | `stopped`, `unhealthy` | Existing container will be restarted |
| `remove` | `extra` | Unmanaged container will be stopped/removed |
| `error` | `missing_external` | Deploy will fail — external resource (e.g. `external: true` volume) is absent |
| `inspect` | _unknown_ | Future drift type not yet mapped; deploy will still attempt to converge |

**Errors:** `404 Not Found` if the stack doesn't exist; `503 Service Unavailable` if the reconciler is not wired (indicates a misconfigured server).

---

## Per-Stack Secrets

Encrypted-at-rest key/value pairs scoped to a single stack. Values are stored as `v1:<nonce>:<ciphertext>` when `ACCELERO_ENCRYPTION_KEY` (or `ACCELERO_ENCRYPTION_KEY_FILE`) is configured, otherwise as plaintext — identical to how `repo_token` and `docker_password` are handled on the stack record itself.

The value leaves Accelero only through the deploy injection path described below. **The list endpoint never returns values** — that's by design, not a UI affordance, so a leaked API key can't be used to exfiltrate secrets.

Secrets cascade-delete with their parent stack.

### Deploy-time injection

At deploy time, every secret for the stack is merged into the `Env` slice on each managed container. Secrets **shadow compose-file env on key collision** — operator intent beats compose default. The override happens in place in the env list, so the final order is stable across unrelated secret changes (easier to diff via `docker inspect`).

Rotation flow:

1. `POST /api/v1/stacks/{id}/secrets` with the new value (returns 200 on rewrite).
2. Wait for the reconciler (`auto_deploy: true` redeploys automatically) or trigger one manually with `POST /api/v1/stacks/{id}/deploy`.
3. **The deploy recreates every replica** so the new env values land — Docker can't update env on a running container, so a normal "image hasn't changed" skip is overridden when secrets have changed.

Values never appear in deploy logs; only an injection count (`Injected N per-stack secret(s)`) is logged at debug level.

### Rotation drift

After a successful deploy, Accelero persists the SHA-256 of the secret set on the stack record. On every reconcile cycle, the current set is hashed and compared. A mismatch surfaces in the drift report as:

```json
{
  "service_name": "(secrets)",
  "type": "secrets_changed",
  "message": "stack secrets have been rotated since the last successful deploy; a redeploy will inject the new values"
}
```

In `/api/v1/stacks/{id}/preview` this maps to action `recreate`. With `auto_deploy: true` on the stack, the reconciler triggers a redeploy that clears the drift; with auto-deploy off, the operator triggers the deploy manually. The hash is updated only on a *successful* deploy — failures leave it untouched, so drift keeps firing until a deploy actually applies the new set.

### Set a Secret
`POST /api/v1/stacks/{id}/secrets`

Upserts a secret. Same key twice = rotation.

**Request body:**
```json
{"name": "DATABASE_URL", "value": "postgres://user:pass@db/app"}
```

**Constraints:**

| Field | Rule |
|-------|------|
| `name` | Required. Must match `^[A-Z_][A-Z0-9_]*$` (uppercase letters, digits, underscores; no leading digit). ≤128 characters. Matches POSIX env-var grammar because secrets are materialised into container env vars at deploy time. |
| `value` | Required, non-empty. ≤64 KiB (comfortably above a PEM cert or a 2 KB service-account JSON). Empty value is rejected with a hint to use DELETE instead. |

**Response:**
- `201 Created` on first write of a given name
- `200 OK` on rewrite (rotation)

```json
{"name": "DATABASE_URL", "created": true}
```

Audited as `stack.secret.set` with metadata `{"rewrote_existing": "true|false"}`. **The value never appears in audit metadata or in any response.**

**Errors:** `400` on malformed name / empty value / invalid JSON; `404` if the stack doesn't exist; `413` on an over-cap value.

### List Secrets
`GET /api/v1/stacks/{id}/secrets`

Returns every secret's name and timestamps in ASCII order by name.

**Response:** `200 OK`
```json
[
  {
    "name": "API_KEY",
    "stack_id": "s1",
    "created_at": "2026-04-23T21:08:15.210849Z",
    "updated_at": "2026-04-23T21:08:15.210849Z"
  },
  {
    "name": "DATABASE_URL",
    "stack_id": "s1",
    "created_at": "2026-04-23T21:08:15.194481Z",
    "updated_at": "2026-04-23T21:08:15.202883Z"
  }
]
```

No `value` field is ever present in the response shape.

### Delete a Secret
`DELETE /api/v1/stacks/{id}/secrets/{name}`

**Response:**
- `204 No Content` on success
- `404 Not Found` if the named secret doesn't exist

Both outcomes are audited as `stack.secret.delete`. Failures record `outcome: "failure"` with `error_message: "not found"` — the trail captures "someone tried to delete X" even when X was already gone, which is useful during incidents (e.g. a cleanup script running twice).

---

## Legacy Webhook

### Trigger Webhook
`POST /webhook`

Legacy endpoint for backward compatibility. Triggers a deployment for the stack specified in the payload, or the "default" stack if none is specified.

**Authentication.** By default `/webhook` requires an API key (`X-API-KEY` header). When `WEBHOOK_SECRET` is set on the server, the API-key check is **replaced** by HMAC-SHA256 signature verification — callers sign the raw body and pass the hex digest as `X-Hub-Signature-256: sha256=<hex>` (the GitHub webhook format; Gitea, Gogs, and most CI systems emit the same header). This lets external senders authenticate without knowing the API key. Rejected signatures return `401` with `{"error":"invalid webhook signature"}` and bump `accelero_webhook_signature_rejected_total`. `POST /api/v1/webhook` always requires the API key regardless.

**Request body:**
```json
{"stack": "my-app"}
```

If `stack` is omitted, Accelero looks for a stack named "default", then falls back to the first available stack.

**Signing example (GitHub-compatible):**

```bash
BODY='{"stack":"default"}'
SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$WEBHOOK_SECRET" | awk '{print $NF}')"
curl -X POST https://accelero.example.com/webhook \
  -H "Content-Type: application/json" \
  -H "X-Hub-Signature-256: $SIG" \
  -d "$BODY"
```

**Response:** `202 Accepted`

**Errors:** `401` on missing/malformed `X-Hub-Signature-256` or HMAC mismatch; `413` if the request body exceeds 1 MiB (signature verification requires buffering the whole body).

---

## Admin

### Encrypt Existing Stack Secrets
`POST /api/v1/admin/encrypt-existing`

One-shot migration that re-saves any stack whose `repo_token` or `docker_password` is still stored as pre-encryption plaintext. New stacks are encrypted on write automatically whenever `ACCELERO_ENCRYPTION_KEY` is set; this endpoint exists for upgrades from versions that stored those fields in plaintext.

**Requires `ACCELERO_ENCRYPTION_KEY`.** Without a master key the re-save would be a no-op and the endpoint returns `400` instead of silently doing nothing.

**Idempotent.** Rows already stored as ciphertext (prefix `v1:`) are skipped; running the call a second time returns `stacks_migrated: 0`.

**Response:** `200 OK`
```json
{
  "status": "ok",
  "stacks_migrated": 2
}
```

If any row fails to migrate (e.g. transient store error), the failed IDs are returned and the audit entry's outcome is `failure`:
```json
{
  "status": "ok",
  "stacks_migrated": 1,
  "stacks_failed": ["4bdf...e83"]
}
```

**Errors:** `400 Bad Request` when the server has no encryption key attached.

Audited as `admin.encrypt-existing` with `stacks_migrated` and `stacks_failed` counts in metadata.

---

### At-rest encryption — how it works

When a master key is configured — either inline via `ACCELERO_ENCRYPTION_KEY` (base64-encoded 32 bytes) or as a file path via `ACCELERO_ENCRYPTION_KEY_FILE` (contents = same base64) — Accelero transparently encrypts `repo_token` and `docker_password` before writing to SQLite and decrypts them when scanning back. Generate a key with `openssl rand -base64 32`. Prefer the file source in production; env vars leak through `docker inspect`, `ps`, systemd unit files, and shell history, while a file mounted as a Docker/K8s secret does not. Setting both sources is a fatal configuration error. The scheme is AES-256-GCM with a random 12-byte nonce per write and a versioned ciphertext format:

```
v1:<base64-nonce>:<base64-ciphertext>
```

The `v1:` prefix is the only flag we look at to decide whether a stored value is encrypted; anything else is treated as legacy plaintext and passed through to the caller. This makes rolling upgrades safe: stop the server, set the key, start, call `/admin/encrypt-existing`.

**Fail-closed.** Reading a `v1:` row on a server that has no key attached returns an error rather than handing back the raw ciphertext. Restart with the key, or recover from a backup that predates the encryption.

**Key rotation is not yet automatic.** See the [roadmap](https://github.com/arbianshkodra/accelero/blob/main/ROADMAP.md#phase-4--security--multi-tenancy).

---

## System

### Health Check
`GET /health`

**No authentication required.**

Liveness probe — returns 200 whenever the HTTP server is responsive. Does not check dependencies. Use `/readyz` for dependency-aware readiness.

**Response:** `200 OK`
```json
{"status": "ok"}
```

---

### Liveness (Kubernetes alias)
`GET /healthz`

**No authentication required.**

Alias for `/health`, following the Kubernetes probe convention. Same body, same status — pick whichever your tooling expects.

---

### Readiness
`GET /readyz`

**No authentication required.**

Returns 200 only when Accelero is actually ready to serve traffic:

- Database is reachable (SQLite ping)
- Docker daemon is reachable (Docker ping, 2s timeout)
- The process is not currently draining for graceful shutdown

**Success:** `200 OK`
```json
{
  "status": "ok",
  "checks": {
    "database": "ok",
    "docker":   "ok",
    "shutdown": "ok"
  }
}
```

**Failure:** `503 Service Unavailable`
```json
{
  "status": "not_ready",
  "checks": {
    "database": "ok",
    "docker":   "unreachable: Cannot connect to the Docker daemon",
    "shutdown": "ok"
  }
}
```

Use `/readyz` as a Kubernetes readinessProbe or as a load-balancer health check — it will flip to 503 the moment Accelero receives SIGTERM so upstream traffic drains before the HTTP server actually stops accepting connections.

---

### Status
`GET /status`

Returns a summary of all stacks.

**Response:** `200 OK`
```json
{
  "stacks": [
    {
      "id": "a1b2c3d4...",
      "name": "my-app",
      "status": "active",
      "last_deployed_at": "2025-01-15T12:00:00Z",
      "git_commit": "abc1234"
    }
  ],
  "total": 1
}
```

---

### Metrics
`GET /metrics`

**No authentication required** (Prometheus convention — scrape targets are unauthenticated, and Accelero's metrics never contain payloads or secrets).

Returns Prometheus exposition format with counters, histograms, and gauges for deployments, drift, HTTP traffic, and reconciler state. Use it as a scrape target in `prometheus.yml`.

**Key metrics:**

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `accelero_deployments_total` | counter | `stack`, `trigger`, `status` | Deployments observed, broken out by outcome |
| `accelero_deployment_duration_seconds` | histogram | `stack`, `trigger`, `status` | End-to-end deploy duration |
| `accelero_drift_detected_total` | counter | `stack`, `type` | Drift items seen by the reconciler |
| `accelero_reconcile_cycles_total` | counter | `stack` | Reconcile cycles that actually ran |
| `accelero_http_requests_total` | counter | `method`, `path`, `status` | HTTP traffic; `path` is the mux route template to keep cardinality bounded |
| `accelero_http_request_duration_seconds` | histogram | `method`, `path`, `status` | HTTP latency |
| `accelero_stacks` | gauge | `status` | Stacks currently in each lifecycle state |
| `accelero_reconciler_loops` | gauge | — | Reconciler goroutines currently running |

Standard `process_*` and `go_*` collectors are also exposed.

---

## Error Responses

All errors follow this format:

```json
{"error": "description of what went wrong"}
```

| Status | Meaning |
|--------|---------|
| `400` | Bad request (missing fields, invalid JSON) |
| `401` | Missing API key |
| `403` | Invalid API key |
| `404` | Resource not found |
| `409` | Conflict (duplicate name, deployment in progress) |
| `415` | Wrong Content-Type (must be `application/json`) |
| `429` | Rate limit exceeded. Response carries `Retry-After: <seconds>` header and a `{"error": "rate limit exceeded", "retry_after": "1s"}` body. Only emitted when `RATE_LIMIT_RPS>0` is configured — see [configuration](./configuration.md#rate-limiting). |
| `500` | Internal server error |
