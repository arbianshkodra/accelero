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

## Legacy Webhook

### Trigger Webhook
`POST /webhook`

Legacy endpoint for backward compatibility. Triggers a deployment for the stack specified in the payload, or the "default" stack if none is specified.

**Request body:**
```json
{"stack": "my-app"}
```

If `stack` is omitted, Accelero looks for a stack named "default", then falls back to the first available stack.

**Response:** `202 Accepted`

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
| `500` | Internal server error |
