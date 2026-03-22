<p style="text-align: center;">
  <img alt="Accelero Logo" src="./images/logo.png" width="450" />
</p>
<h1 align="center">
  Accelero
</h1>

<p align="center">GitOps-powered Docker deployment automation with zero downtime.</p>

## What is Accelero?

Accelero brings [GitOps](https://codefresh.io/learn/gitops/) to Docker Compose environments. It manages multiple deployment **stacks** — each stack is a git repository containing a `docker-compose.yaml` that defines the desired state of your services. Accelero continuously compares what's in git against what's actually running on your Docker host, and deploys to converge.

**Think of it as ArgoCD for Docker Compose** — git is the source of truth, drift is detected and corrected, and deployments happen automatically with zero downtime.

## Quick Start

Run Accelero as a Docker container:

```bash
docker run -d \
  --name accelero \
  -p 8000:8000 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v accelero-data:/data \
  -e API_KEY=your-secure-key \
  arbianshkodra/accelero
```

Then create your first stack:

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

Accelero will clone your repo, parse the compose file, and deploy all services. Every 5 minutes it will check for drift and auto-deploy if anything has changed.

For detailed setup instructions, see the [Introduction](./introduction.md) and [Usage Overview](./usage-overview.md) sections.
