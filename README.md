# at

A minimal self-hosted deployment platform. Drop a project folder into `projects/`, scan it, and deploy it as a Docker container behind a Caddy reverse proxy.

Single binary, single server, no fluff.

## How it works

```
projects/<name>/  →  POST /api/apps/scan  →  docker build  →  Start container  →  Caddy proxy update
```

State is persisted in SQLite. Only one container runs per app at a time — a new deploy always replaces the previous one.

## Requirements

- Go 1.22+
- Docker
- [Caddy](https://caddyserver.com) (for reverse proxy + automatic HTTPS)

## Quick start

**1. Start Caddy**

```bash
docker-compose up -d
```

**2. Build and run**

```bash
make run
```

The dashboard is available at `http://localhost:8080`.

**3. Add an app**

Place your project folder (with a `Dockerfile`) inside the `projects/` directory:

```
projects/
  myapp/
    Dockerfile
    ...
```

Then trigger a scan — either via the dashboard **Scan** button or:

```bash
curl -X POST http://localhost:8080/api/apps/scan
```

`at` registers each new subdirectory as an app, assigning it a domain and a host port automatically. You can optionally provide an `at.json` in the project folder to override defaults (see [Per-project config](#per-project-config)).

**4. Deploy**

Click **Deploy** next to the app in the dashboard, or:

```bash
curl -X POST http://localhost:8080/api/apps/<id>/deploy
```

Once it turns **running**, the app is reachable at its assigned domain through Caddy.

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `AT_PORT` | `8080` | Dashboard / API port |
| `AT_DATA_DIR` | `./data` | Directory for the SQLite database |
| `AT_PROJECTS_DIR` | `./projects` | Root directory scanned for project folders |
| `AT_CADDY_ADMIN` | `http://localhost:2019` | Caddy admin API URL |
| `AT_PORT_RANGE_START` | `10000` | First host port allocated to apps (increments per app) |
| `AT_BASE_DOMAIN` | _(unset)_ | Base domain for automatic subdomain routing (see below) |
| `AT_UPSTREAM_HOST` | `localhost` | Host Caddy uses to reach app containers. Set to `host.docker.internal` when Caddy runs in Docker (e.g. Docker Desktop on Windows/Mac). |

## Per-project config

Each project folder may contain an optional `at.json`:

```json
{ "domain": "myapp.example.com", "container_port": 3000, "env_vars": {"KEY": "val"} }
```

| Field | Default | Description |
|---|---|---|
| `domain` | `<name>.<AT_BASE_DOMAIN>` or `<name>.localhost` | Domain Caddy routes to this app |
| `container_port` | `8080` | Port your app listens on inside the container |
| `env_vars` | `{}` | Environment variables passed to the container at runtime |

`at.json` is only read at scan time when first registering an app. After registration, settings live in the database and are editable via `PUT /api/apps/{id}`.

## Local development

You can run `at` entirely on your own machine without a public domain or TLS.

**1. Start Caddy**

```bash
docker-compose up -d
```

Caddy listens on ports 80 and 443 locally. Its admin API is exposed on port 2019.

**2. Run `at`**

```bash
AT_BASE_DOMAIN=localhost make run
```

Setting `AT_BASE_DOMAIN=localhost` means each app gets an automatic domain of `<name>.localhost`. No DNS setup needed — `*.localhost` resolves to `127.0.0.1` on most systems (macOS, Linux, Windows WSL2).

**3. Add an app**

Drop a folder with a `Dockerfile` into `projects/`, then open `http://localhost:8080` and click **Scan**. The domain is assigned automatically as `<name>.localhost`.

**4. Deploy and open**

Click **Deploy**. Once it turns **running**, open `http://<name>.localhost` in your browser.

> HTTPS is automatically disabled for `localhost` and `*.localhost` domains — Caddy serves plain HTTP for these.

**Without `AT_BASE_DOMAIN`** — you can also omit it and set a domain manually per app via the dashboard or `PUT /api/apps/{id}`. The behavior is the same.

**Ports** — Caddy proxies `<name>.localhost:80` → container on its assigned host port. You never need to expose app ports directly.

### Windows

The setup is the same, with a few differences:

**Requirements:** [Docker Desktop](https://www.docker.com/products/docker-desktop/) instead of plain Docker. Make sure it is running before starting Caddy.

**`*.localhost` DNS:** Chrome and Edge resolve `*.localhost` to `127.0.0.1` natively — no extra setup needed for those browsers. Firefox and other browsers do not, and the Windows `hosts` file does not support wildcards, so you need one entry per app:

1. Open Notepad as Administrator.
2. Open `C:\Windows\System32\drivers\etc\hosts`.
3. Add a line for each app:

```
127.0.0.1  myapp.localhost
127.0.0.1  anotherapp.localhost
```

**Running `at`:** use PowerShell or Git Bash:

```powershell
# PowerShell
$env:AT_BASE_DOMAIN="localhost"; $env:AT_UPSTREAM_HOST="host.docker.internal"; go run ./cmd/at
```

```bash
# Git Bash
AT_BASE_DOMAIN=localhost AT_UPSTREAM_HOST=host.docker.internal go run ./cmd/at
```

`AT_UPSTREAM_HOST=host.docker.internal` is required because Caddy runs inside a Docker container and cannot reach `localhost` on the Windows host. `host.docker.internal` is a special DNS name Docker Desktop provides that resolves to the host machine. Setting this also causes app containers to bind on `0.0.0.0` instead of `127.0.0.1` so the connection from Caddy is accepted.

**Caddy admin API:** the included `Caddyfile` binds the admin API to `0.0.0.0:2019` so Docker Desktop on Windows can reach it from the host. Don't change this.

---

## Subdomain routing with `AT_BASE_DOMAIN`

When `AT_BASE_DOMAIN` is set, each app's domain is automatically assigned as `<name>.<base_domain>`. You don't need to set a domain when apps are scanned.

**Example:** set `AT_BASE_DOMAIN=apps.example.com` and add a folder named `api` — it will be reachable at `api.apps.example.com`.

**DNS setup:** point a wildcard CNAME at your server:

```
*.apps.example.com  CNAME  your-server.example.com
```

or a wildcard A record:

```
*.apps.example.com  A  <server-ip>
```

Caddy will automatically obtain TLS certificates for each app domain. For local development using `localhost` or `*.localhost` domains, HTTPS is skipped automatically.

## HTTPS

Caddy handles TLS automatically:

- Public domains get a Let's Encrypt certificate.
- `localhost`, `*.localhost`, `*.local`, and bare IPs skip HTTPS automatically.

No configuration needed.

## API

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/apps` | List apps with latest deployment status |
| `GET` | `/api/apps/{id}` | Get app |
| `PUT` | `/api/apps/{id}` | Update app (domain, container_port, env_vars) |
| `DELETE` | `/api/apps/{id}` | Delete app and stop its container |
| `GET` | `/api/apps/{id}/deployments` | List last 20 deployments |
| `POST` | `/api/apps/{id}/deploy` | Trigger a deploy |
| `POST` | `/api/apps/scan` | Scan projects directory for new apps |
| `GET` | `/api/deployments/{id}` | Get deployment detail and logs |
| `GET` | `/api/status` | Server status (Caddy reachability) |
| `GET` | `/api/config` | Server config (base domain) |

## Project layout

```
cmd/at/          Entry point
internal/
  api/           HTTP handlers + embedded dashboard (index.html)
  deploy/        Build and deploy pipeline
  proxy/         Caddy admin API client
  store/         SQLite models and queries
docker-compose.yml  Caddy setup
Makefile
```
