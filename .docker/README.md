<p align="center">
  <img src="https://raw.githubusercontent.com/arbianshkodra/accelero/dev/docs/images/logo.png" width="420" />
</p>

# Accelero

**GitOps-powered Docker deployment automation with zero downtime.**

Accelero is a lightweight, single-binary tool that brings GitOps to Docker
Compose environments. It manages multiple deployment stacks — each backed by a
git repository containing a `docker-compose.yaml` — and continuously reconciles
the desired state in git against the actual running containers, deploying
changes automatically with zero downtime. Think of it as **ArgoCD/Flux for
Docker Compose**.

## Supported tags

- `latest` — the most recent release (multi-arch: `amd64`, `arm64`, `arm`, `386`)
- `x.y.z` — a specific release (e.g. `0.6.0`)

## Quick start

```bash
docker run -d \
  --name accelero \
  -p 8000:8000 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v accelero-data:/data \
  -e API_KEY=your-secure-key \
  arbianshkodra/accelero
```

Then create a stack:

```bash
curl -X POST http://localhost:8000/api/v1/stacks \
  -H "Content-Type: application/json" \
  -H "X-API-KEY: your-secure-key" \
  -d '{
    "name": "my-app",
    "repo_url": "https://github.com/your-org/your-gitops-repo",
    "compose_path": "docker-compose.yaml",
    "auto_deploy": true,
    "reconcile_interval_seconds": 300
  }'
```

## Features

- **GitOps reconciliation** — periodic drift detection (git vs. Docker) with auto-deploy
- **Zero-downtime deployments** — health-checked rolling updates, automatic rollback
- **Broad compose compatibility** — most real-world compose fields, `${VAR}` interpolation, named volumes, replicas
- **Observability** — container logs/stats (incl. live WebSocket streams), exec, Docker events (SSE), resource browsers, Prometheus `/metrics`
- **Security** — native TLS + HSTS, per-stack secrets, at-rest encryption, per-API-key rate limiting, HMAC-signed webhooks, append-only audit log
- **Reliability** — retries with backoff, an auto-deploy circuit breaker, and self-healing of failed stacks
- **Single binary** — SQLite persistence, no dependencies beyond Docker

## Configuration

Only `API_KEY` is required. See the full configuration reference and docs:

- 📖 Documentation: https://accelero.sh/docs
- 🔧 Configuration: https://github.com/arbianshkodra/accelero/blob/dev/docs/configuration.md
- 💻 Source: https://github.com/arbianshkodra/accelero

## License

[Apache License 2.0](https://github.com/arbianshkodra/accelero/blob/dev/LICENSE)
