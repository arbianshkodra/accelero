# Sample GitOps repo

Everything in this directory represents a **user's GitOps repo** — not Accelero's own configuration. Fork it (or copy it into a new repo), point an Accelero stack at it, and you get a working zero-downtime deployment end-to-end.

## What this sample demonstrates

- **`.env` variable interpolation** — `${NGINX_TAG:-nginx:1.27.1-alpine}` style substitution, with `:?required` for fields that must be set.
- **Caddy in front** — a reverse-proxy service holds the host port; replicated backends expose only inside the Docker network. This is the pattern that makes zero-downtime actually work (see `docs/usage-overview.md#zero-downtime-deployments` for why).
- **`deploy.replicas`** — `web` runs three interchangeable replicas. Accelero rolling-updates them one at a time: create new, wait healthy, remove one old, repeat. At least two replicas stay serving traffic throughout.
- **Healthchecks that actually work** — `wget --spider` on an alpine nginx image (the debian ones ship neither `wget` nor `curl`).

## Deploy it via Accelero

```bash
# 1. Put this directory on a git remote your Accelero can reach.
#    Or, for a local smoke test, commit it to a local path Accelero can read.

# 2. Create the stack
curl -X POST http://localhost:8000/api/v1/stacks \
  -H "Content-Type: application/json" \
  -H "X-API-KEY: $ACCELERO_API_KEY" \
  -d '{
    "name": "sample",
    "repo_url": "https://github.com/you/your-gitops-repo",
    "compose_path": "docker-compose.yaml",
    "auto_deploy": true,
    "reconcile_interval_seconds": 60
  }'

# 3. Trigger the first deploy
curl -X POST http://localhost:8000/api/v1/stacks/sample/deploy \
  -H "X-API-KEY: $ACCELERO_API_KEY"
```

Expected containers after the initial deploy (15-25s):

```
proxy_0_*        caddy:2-alpine               Up
web_0_*          nginx:1.27.1-alpine  (healthy)
web_1_*          nginx:1.27.1-alpine  (healthy)
web_2_*          nginx:1.27.1-alpine  (healthy)
```

`curl http://localhost/` hits Caddy, which resolves `web` via Docker's embedded DNS and round-robins to a healthy replica.

## Prove zero-downtime yourself

```bash
# Terminal 1 — send real traffic for 45 seconds, count anything non-2xx.
(
  t_end=$(($(date +%s) + 45))
  ok=0
  fail=0
  while [ $(date +%s) -lt $t_end ]; do
    code=$(curl -s -o /dev/null --max-time 2 -w '%{http_code}' http://127.0.0.1/)
    [ "$code" = "200" ] && ok=$((ok+1)) || fail=$((fail+1))
  done
  echo "ok=$ok fail=$fail"
)
```

```bash
# Terminal 2 (within the 45s window) — bump the nginx tag.
sed -i '' 's/1.27.1/1.27.2/' .env
git add . && git commit -m "bump nginx to 1.27.2"
git push
curl -X POST http://localhost:8000/api/v1/stacks/sample/deploy \
  -H "X-API-KEY: $ACCELERO_API_KEY"
```

The `web` service is rolling-replaced one replica at a time. Caddy keeps serving from whichever replicas are healthy. Expected output from terminal 1:

```
ok=5580 fail=0
```

(Actual measurement from a local run: 5580 requests over 45s, all 200s, while rolling from `nginx:1.27.2-alpine` → `nginx:1.27.3-alpine` across 3 replicas. Rolling update took ~19s of that window.)

## Why `caddy reverse-proxy --to web:80` and not a Caddyfile?

The `caddy reverse-proxy` CLI subcommand handles this minimal pattern in one line, with no external config file. For anything more than a single upstream — TLS, multiple hosts, headers, rate limits — you'd switch to a Caddyfile and mount it into the proxy container. Mounting files from the GitOps repo into a managed container is a separate Accelero feature (tracked in the ROADMAP); for now, this sample stays with the CLI form.

## When does this pattern NOT apply?

**Single-container services with static host ports** (`ports: "80:80"`, no `deploy.replicas`) cannot be zero-downtime deployed under Docker — the host port can only be held by one container at a time. An image bump for such a service either fails with a port conflict or has a measurable outage during container swap. If you need zero-downtime for an externally-reachable service, put a proxy (Caddy, Traefik, nginx — your choice) in front like this sample does, and let the backend use `expose:` only.
