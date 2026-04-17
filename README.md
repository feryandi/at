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
| `AT_OAUTH_POLICY` | _(unset)_ | caddy-security authorization policy name. When set, every app route injected into Caddy requires authentication via that policy. |
| `AT_OAUTH_PORTAL_URL` | _(unset)_ | Base URL of the caddy-security auth portal (e.g. `https://at.example.com/auth`). When set, the dashboard shows the logged-in user and a sign-out link. |

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

## Production deployment (Linux, native Caddy)

This is the recommended setup for a real server — Caddy runs as a native systemd service (not Docker), and `at` runs as its own service alongside it.

### 1. Install Caddy

```bash
sudo apt install -y debian-keyring debian-archive-keyring apt-transport-https
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo apt update && sudo apt install caddy
```

### 2. Configure Caddyfile

`/etc/caddy/Caddyfile` — handles your dashboard domain, and provides a catch-all HTTP→HTTPS redirect so app subdomains upgrade correctly:

```caddyfile
{
    admin 0.0.0.0:2019
}

at.example.com {
    reverse_proxy localhost:8080
}

# Redirect all other HTTP traffic to HTTPS.
# at-managed app routes live on :443 — this upgrades plain HTTP requests.
http:// {
    redir https://{host}{uri} permanent
}
```

> **Do not use a catch-all `:80` block** (the default Caddy template has one). It intercepts all HTTP traffic before host matching and sends everything to the dashboard.

```bash
sudo systemctl reload caddy
```

### 3. DNS — wildcard subdomain

Point a wildcard A record and a bare A record at your server's IP at your DNS provider:

```
A  *.at       →  <server-ip>
A  at         →  <server-ip>
```

This makes `genesis.at.example.com`, `api.at.example.com`, etc. all resolve to your server. Caddy matches them by `Host` header and routes to the right container.

### 4. Build `at`

```bash
make build
```

### 5. systemd service

Create `/etc/systemd/system/at.service`:

```ini
[Unit]
Description=at deployment platform
After=network.target

[Service]
User=<your-user>
Group=<your-user>
WorkingDirectory=/path/to/at
ExecStart=/path/to/at/bin/at
Restart=always
RestartSec=5s
Environment=AT_BASE_DOMAIN=at.example.com

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now at
```

### Important: set `AT_BASE_DOMAIN` before first scan

Apps are assigned their domain **at scan time**, not at deploy time. If `AT_BASE_DOMAIN` is not set when you first scan a project, the app gets `<name>.localhost` instead of `<name>.at.example.com`. Fix it after the fact:

```bash
curl -X PUT http://localhost:8080/api/apps/<id> \
  -H "Content-Type: application/json" \
  -d '{"domain": "myapp.at.example.com"}'
```

Then redeploy to push the corrected route to Caddy.

---

## Securing the dashboard with Google SSO

The `at` dashboard has no authentication built in. If it is exposed on a public domain, anyone can access it. The recommended approach is to protect it at the Caddy level using the [`caddy-security`](https://github.com/greenpau/caddy-security) plugin, which adds a Google OAuth2 login gate with no extra services required.

### 1. Build Caddy with the caddy-security plugin

The stock Caddy binary does not include third-party plugins. Use [`xcaddy`](https://github.com/caddyserver/xcaddy) to build a custom binary.

**Install xcaddy:**

```bash
GOPATH=$HOME/go go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest
```

**Build Caddy with caddy-security** (match the version already installed — check with `caddy version`):

```bash
cd /tmp
GOPATH=$HOME/go $HOME/go/bin/xcaddy build v2.11.2 \
  --with github.com/greenpau/caddy-security \
  --output /tmp/caddy-new
```

**Verify the plugin is included:**

```bash
/tmp/caddy-new list-modules | grep security
# should print: security
```

**Replace the system binary** (this causes ~1 second of downtime):

```bash
sudo cp /usr/bin/caddy /usr/bin/caddy.bak   # keep a backup
sudo systemctl stop caddy
sudo cp /tmp/caddy-new /usr/bin/caddy
sudo systemctl start caddy
```

### 2. Create a Google OAuth app

1. Go to [console.cloud.google.com](https://console.cloud.google.com) and create or select a project.
2. Navigate to **APIs & Services → OAuth consent screen**
   - User type: **External**
   - Fill in app name and your email
   - Add scopes: `openid`, `email`, `profile`
   - Add your Gmail address as a **test user**
3. Navigate to **APIs & Services → Credentials → Create Credentials → OAuth 2.0 Client ID**
   - Application type: **Web application**
   - Authorized redirect URI:
     ```
     https://at.example.com/auth/oauth2/google/authorization-code-callback
     ```
     (replace `at.example.com` with your actual dashboard domain)
4. Copy the **Client ID** and **Client Secret**.

### 3. Generate a JWT signing key

This key signs the session tokens issued after login. Generate a random one:

```bash
openssl rand -hex 32
```

Keep this value — losing it invalidates all active sessions.

### 4. Update your Caddyfile

Replace `/etc/caddy/Caddyfile` with the following, substituting all `<placeholder>` values:

```caddyfile
{
    admin 0.0.0.0:2019

    security {
        oauth identity provider google {
            realm google
            driver google
            client_id <your-google-client-id>
            client_secret <your-google-client-secret>
            scopes openid email profile
        }

        authentication portal myportal {
            crypto default token lifetime 86400
            crypto key sign-verify <your-random-hex-key>
            enable identity provider google
            cookie domain at.example.com

            transform user {
                match realm google
                match email you@gmail.com
                action add role authp/user
            }

            ui {
                custom css path /var/lib/at/data/theme/auth.css
            }
        }

        authorization policy mypolicy {
            set auth url https://at.example.com/auth/
            allow roles authp/user
        }
    }
}

at.example.com {
    route /auth* {
        authenticate with myportal
    }

    route {
        authorize with mypolicy
        reverse_proxy localhost:8080
    }
}

# Redirect all other HTTP traffic to HTTPS so app subdomains (*.at.example.com)
# get upgraded — Caddy's app routes only live on :443.
http:// {
    redir https://{host}{uri} permanent
}
```

**Key fields:**

| Field | Description |
|---|---|
| `client_id` | From Google Cloud Console (Credentials page) |
| `client_secret` | From Google Cloud Console — treat as a password, never commit to git |
| `crypto key sign-verify` | Random hex string from `openssl rand -hex 32` |
| `cookie domain` | Your dashboard domain (no `https://` prefix) |
| `match email` | Gmail address(es) allowed to log in — add one `match email` line per user |
| `set auth url` | Full URL to the portal, must match `cookie domain` |

### 5. Apply the at login theme

`at` writes a custom CSS file to `{AT_DATA_DIR}/theme/auth.css` on every startup. It overrides caddy-security's portal styles to match the `at` dashboard: dark background, monospace branding, blue accent buttons.

The Caddyfile snippet in step 4 already includes the `ui` block pointing to this file. If you use a different `AT_DATA_DIR`, update the path accordingly:

```caddyfile
ui {
    custom css path /your/data/dir/theme/auth.css
}
```

The default path (when `AT_DATA_DIR=/var/lib/at/data`) is `/var/lib/at/data/theme/auth.css`. The file is regenerated on each `at` restart, so theme updates ship automatically.

### 6. Protect app subdomains too

By default the SSO gate only covers the dashboard domain. To require login on all deployed app subdomains as well, set `AT_OAUTH_POLICY` to the name of the authorization policy you defined in the Caddyfile (`mypolicy` in the example above). `at` will then prepend the authentication handler to every app route it injects into Caddy.

Add it to your systemd service file:

```ini
Environment=AT_OAUTH_POLICY=mypolicy
```

Then reload:

```bash
sudo systemctl daemon-reload
sudo systemctl restart at
```

### 7. Show logged-in user in the dashboard

Set `AT_OAUTH_PORTAL_URL` to the base URL of your auth portal so the dashboard can display the logged-in user and a sign-out link:

```ini
Environment=AT_OAUTH_PORTAL_URL=https://at.example.com/auth
```

This requires `inject headers with claims` in the Caddyfile authorization policy (so caddy-security forwards user info as request headers to `at`):

```caddyfile
authorization policy mypolicy {
    set auth url https://at.example.com/auth/
    crypto key verify <your-random-hex-key>
    allow roles authp/user
    inject headers with claims
}
```

When configured, the dashboard header shows an avatar, the user's name, and a **Sign out** link. When `AT_OAUTH_PORTAL_URL` is unset, the header is unchanged — the feature is fully opt-in.

### 8. Validate and reload

```bash
sudo caddy validate --config /etc/caddy/Caddyfile
sudo systemctl reload caddy
```

Visiting `https://at.example.com` will now redirect to the themed Google login page. Only the email address(es) listed in `match email` blocks can authenticate.

> **Keep secrets out of git.** The `client_secret` and `crypto key sign-verify` values must never be committed. If you version your Caddyfile, use environment variable substitution (`{$MY_VAR}`) or a secrets manager to inject them at runtime.

---

## How `at` manages Caddy routes

`at` uses Caddy's admin API to add reverse proxy routes for deployed apps. The approach matters when Caddy also has a hand-written Caddyfile (dashboard site, TLS, redirects).

**The wrong approach** — `POST /load` replaces Caddy's **entire** config. Any Caddyfile-defined servers, TLS setup, or redirect rules are wiped on every deploy sync.

**The right approach** — read-modify-write:

1. `GET /config/` — fetch the full live config (preserves everything from the Caddyfile)
2. Remove any routes tagged `@id: "at-*"` (these are the routes `at` previously added)
3. Insert new app routes (tagged `@id: "at-<domain>"`) into the existing `:443` server
4. `POST /load` — reload with the merged config

This way `at` only touches its own routes and never disturbs the Caddyfile's server structure.

### Surviving Caddy restarts

When Caddy restarts it reloads from its Caddyfile, which means all dynamically injected app routes are lost. `at` recovers automatically: it re-syncs all running app routes to Caddy every 30 seconds. At worst, an app is unreachable for 30 seconds after a Caddy restart — no manual redeploy required.

---

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
| `GET` | `/api/config` | Server config (base domain, oauth portal URL) |
| `GET` | `/api/whoami` | Logged-in user info from caddy-security headers (email, name); only meaningful when `AT_OAUTH_PORTAL_URL` is set |

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
