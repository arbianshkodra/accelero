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
