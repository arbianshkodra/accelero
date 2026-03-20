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
| `missing` | Service defined in compose but no container running |
| `image_mismatch` | Container running different image than declared |
| `stopped` | Container exists but is not in `running` state |
| `unhealthy` | Container is failing its health check |
| `extra` | Container exists for a service not defined in compose |

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

**Response:** `200 OK`
```json
{"status": "ok"}
```

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
