# Introduction

Accelero is a lightweight tool that automates Docker Compose deployments using a GitOps strategy. It manages one or more **stacks** — each stack points to a git repository containing a `docker-compose.yaml` — and ensures that the running containers on your Docker host always match what's declared in git.

## Key Concepts

### Stacks

A **stack** is the core unit in Accelero. It represents:

- A git repository URL (with credentials)
- A path to a `docker-compose.yaml` within that repo
- An optional branch and service filter
- Reconciliation settings (interval, auto-deploy)
- Docker registry credentials (if pulling from a private registry)

You can manage multiple stacks through the REST API. Each stack operates independently.

### GitOps Reconciliation

Accelero doesn't just deploy when triggered — it continuously monitors for **drift** between the desired state (your compose file in git) and the actual state (running Docker containers). Drift types include:

- **Missing** — A service defined in compose has no running container
- **Image mismatch** — A container is running a different image than declared
- **Stopped** — A container that should be running is stopped
- **Unhealthy** — A container is failing its health check
- **Extra** — A container exists that isn't defined in the compose file

When drift is detected and `auto_deploy` is enabled, Accelero automatically deploys to converge.

### Zero-Downtime Deployments

Accelero creates new containers and waits for them to pass health checks before removing old ones. If a new container fails to become healthy, it is cleaned up and the deployment is rolled back to the pre-deployment state.

### Rollback

Before deploying, Accelero captures the complete state of every affected service (container config, host config, network connections). If any service fails to deploy, all previously deployed services in that batch are rolled back using the captured snapshots — not the current (broken) state.

## How It Works

```
┌─────────────┐     ┌──────────────┐     ┌─────────────────┐
│  Git Repos   │────▶│  Reconciler  │────▶│  Docker Engine  │
│ (desired)    │◀────│  (compare &  │◀────│  (actual state) │
└─────────────┘     │   converge)  │     └─────────────────┘
                    └──────┬───────┘
                           │
                    ┌──────▼───────┐
                    │   SQLite DB  │
                    │  (history,   │
                    │   config,    │
                    │   stacks)    │
                    └──────┬───────┘
                           │
                ┌──────────▼──────────┐
                │   REST API          │
                │  (manage stacks,    │
                │   trigger deploys,  │
                │   check drift)      │
                └─────────────────────┘
```

1. You define your desired state in a `docker-compose.yaml` in a git repository
2. You create a stack in Accelero pointing to that repo
3. Accelero clones the repo, parses the compose file, and deploys the services
4. The reconciler periodically checks for drift and auto-deploys if configured
5. Webhooks or API calls can trigger deployments on demand
6. All deployment history is persisted in SQLite

## Sample GitOps Repository

A sample compose file is included in the `samples/` directory of this repo. You can also fork the [GitOps sample repository](https://github.com/arbianshkodra/accelero-sample-gitops/) as a template for your own projects.
