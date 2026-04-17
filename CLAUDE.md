# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make build          # compile → ./bin/at
make run            # build + run binary
make dev            # go run ./cmd/at (no binary output)
make tidy           # go mod tidy

go build ./...      # verify compilation
```

No test suite exists. Verify changes with `go build ./...`.

**Running locally (Windows):**
```bash
# Git Bash
AT_BASE_DOMAIN=localhost AT_UPSTREAM_HOST=host.docker.internal go run ./cmd/at

# PowerShell
$env:AT_BASE_DOMAIN="localhost"; $env:AT_UPSTREAM_HOST="host.docker.internal"; go run ./cmd/at
```
Requires Docker Desktop running and Caddy started via `docker-compose up -d`.

## Architecture

`at` is a personal deployment platform: drop a project folder into `projects/`, scan it, and deploy it as a Docker container behind a Caddy reverse proxy. State lives entirely in SQLite.

### Request → Deploy flow

```
POST /api/apps/{id}/deploy
  → pipeline.Deploy(appID)           [deploy/pipeline.go]
      → db.CreateDeployment()        [store/store.go]
      → goroutine: runDeploy()
          → os.Stat(projects/<name>) — verify project dir exists
          → docker build -t at-<name>:<depID[:8]> ./projects/<name>/
          → docker stop/rm at-<name> (old container)
          → docker create + start at-<name> (new container, bound to host port)
          → db.UpdateDeploymentStatus("running")
          → caddy.Sync()             [proxy/caddy.go]
```

### Project discovery flow

```
startup / POST /api/apps/scan
  → pipeline.ScanProjects()
      → os.ReadDir(projectsDir)
      → for each subdir not in DB:
          read optional at.json for {domain, container_port, env_vars}
          db.CreateApp() — auto-assigns host port (max+1 from portRangeStart)
```

### Key design decisions

- **One container per app** — container is always named `at-<appname>`. A new deploy stops+removes the old one and starts a new one.
- **Port assignment** — each app gets a unique `host_port` = `MAX(host_port)+1`, starting from `AT_PORT_RANGE_START` (default 10000). Never reused.
- **Per-app deploy lock** — `sync.Map` of buffered channels (size 1) prevents concurrent deploys for the same app.
- **Caddy sync** — after every deploy, all running-app routes are re-pushed to Caddy's admin API (`/load`). Caddy config is stateless from `at`'s perspective — fully rebuilt each sync.
- **`AT_UPSTREAM_HOST`** — when Caddy runs in Docker (Windows/Mac), set to `host.docker.internal`; this also switches container port binding from `127.0.0.1` to `0.0.0.0`.

### Package responsibilities

| Package | File | Responsibility |
|---|---|---|
| `cmd/at` | `main.go` | Wire everything: env vars, SQLite, Docker client, Caddy, pipeline; scan projects; reconcile on startup |
| `internal/store` | `store.go` | SQLite schema, all DB queries, `App`/`Deployment` structs, schema migration |
| `internal/deploy` | `pipeline.go` | Docker build+run lifecycle, project scanning, per-app locking, Caddy sync |
| `internal/api` | `api.go` | HTTP handlers, route registration; `index.html` is embedded via `web.go` |
| `internal/proxy` | `caddy.go` | Caddy admin API client — builds JSON config and POSTs to `/load` |

### SQLite schema (current)

**`apps`**: `id, name, domain, container_port, host_port, env_vars, created_at, updated_at`
**`deployments`**: `id, app_id, commit_sha, status, image_id, container_id, logs, error, started_at, finished_at`

Deployment `status` values: `pending → building → running` (or `failed` / `stopped`).

The `migrate()` function in `store.go` auto-drops legacy `git_url`, `git_branch`, `webhook_secret` columns from pre-redesign databases.

### Per-project `at.json` (optional)

Each project folder may contain `at.json`:
```json
{ "domain": "myapp.example.com", "container_port": 3000, "env_vars": {"KEY": "val"} }
```
Only read at scan time when first registering an app. After registration, settings live in the DB and are editable via `PUT /api/apps/{id}`.

### Environment variables

| Var | Default | Notes |
|---|---|---|
| `AT_PROJECTS_DIR` | `./projects` | Root directory scanned for project folders |
| `AT_DATA_DIR` | `./data` | SQLite DB location |
| `AT_PORT` | `8080` | Dashboard/API listen port |
| `AT_CADDY_ADMIN` | `http://localhost:2019` | Caddy admin API |
| `AT_PORT_RANGE_START` | `10000` | First host port allocated |
| `AT_BASE_DOMAIN` | _(unset)_ | Auto-assigns `<name>.<base_domain>` on scan; falls back to `<name>.localhost` |
| `AT_UPSTREAM_HOST` | `localhost` | How Caddy reaches containers; use `host.docker.internal` on Windows/Mac with Docker Desktop |
