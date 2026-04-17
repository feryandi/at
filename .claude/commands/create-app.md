---
description: Create and deploy a new app to the `at` platform. Guides through project name, what to build, generates Dockerfile + source, registers, and deploys. Trigger when user says "create app", "new app", "add project", or similar.
argument-hint: [project-name] [description]
allowed-tools: [Read, Write, Glob, Grep, Bash]
---

# Create New App on `at`

You are helping the user create and deploy a brand-new app on the local `at` platform (Docker + Caddy reverse proxy). Follow every step below in order.

## Platform Quick Reference

- Projects live in `./projects/<name>/` — each must have a `Dockerfile`
- `at.json` (optional, at project root) declares `{ "container_port": N }` and optional `env_vars`
- Scan API: `POST http://localhost:8080/api/apps/scan`
- List apps: `GET http://localhost:8080/api/apps`
- Deploy: `POST http://localhost:8080/api/apps/{id}/deploy`
- Deployment status: `GET http://localhost:8080/api/deployments/{id}`
- Always use the smallest Alpine-based image available for the chosen language

---

## Step 1 — Gather Requirements

If `$ARGUMENTS` already provides a project name and/or description, use them and skip asking for what's already given. Otherwise ask the user — you may ask all missing questions in one message:

1. **Project name** — short, lowercase, hyphen-separated (e.g. `hello`, `my-api`). Must not already exist in `./projects/`.
2. **What to build** — a short description of the app (e.g. "a Node.js landing page", "a Python Flask API that returns the current time").
3. **Container port** — the port the app listens on inside the container (default: `3000`; only ask if not obvious from the app type).

Do not proceed to Step 2 until you have all three answers.

---

## Step 2 — Check for Name Conflicts

```bash
ls ./projects/
```

If `./projects/<name>/` already exists, tell the user and ask for a different name before continuing.

---

## Step 3 — Create the Project Files

Create `./projects/<name>/` containing at minimum:

### `Dockerfile`
- Always use an Alpine (or `-slim`) base image — pick the smallest official variant for the language.
- Multi-stage builds only when they meaningfully reduce the final image size.
- `EXPOSE <container_port>`
- `CMD` / `ENTRYPOINT` that starts the app.

### Application source
- Generate working, self-contained source code that matches the user's description.
- Keep dependencies minimal — prefer the standard library where reasonable.
- The app must listen on the port declared in `at.json` (read from `process.env.PORT` / `$PORT` / equivalent so the platform can override it).

### `at.json`
```json
{ "container_port": <N> }
```

---

## Step 4 — Register the App

```bash
curl -s -X POST http://localhost:8080/api/apps/scan
```

Then find the new app's ID:

```bash
curl -s http://localhost:8080/api/apps
```

Parse the JSON and extract the `id` for the app whose `name` matches the project name.

---

## Step 5 — Deploy

```bash
curl -s -X POST http://localhost:8080/api/apps/<id>/deploy
```

Save the returned `deployment_id`.

---

## Step 6 — Wait and Report

Poll the deployment status every 5 seconds (up to ~90 seconds) until `status` is `running` or `failed`:

```bash
curl -s http://localhost:8080/api/deployments/<deployment_id>
```

- If `running`: report success. Fetch the app's `domain` field from `GET http://localhost:8080/api/apps`, construct the full URL as `https://<domain>` (the `domain` field already contains the full hostname, e.g. `hello.at.feryand.in`), and present it as a clickable link so the user can open it directly. Include a brief summary of what was created.
- If `failed`: show the `error` and last 500 chars of `logs` so the user can debug.

---

## Guidelines

- Never skip the Dockerfile — every project must be containerised.
- Never use non-Alpine base images unless the user explicitly requests otherwise.
- Keep generated source code clean, minimal, and working out of the box.
- Do not ask unnecessary questions — infer sensible defaults from the description.
