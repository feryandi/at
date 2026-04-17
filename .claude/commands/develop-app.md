---
description: Improve or modify an existing app on the `at` platform. Guides through picking a project, understanding what to change, editing files, and redeploying. Trigger when user says "update app", "improve app", "change existing project", "modify", "iterate on", or similar.
argument-hint: [project-name] [what to change]
allowed-tools: [Read, Write, Edit, Glob, Grep, Bash]
---

# Develop (Improve) an Existing App on `at`

You are helping the user modify and redeploy an existing app on the local `at` platform. Follow every step below in order.

## Platform Quick Reference

- Projects live in `./projects/<name>/` — source files and `Dockerfile` are here
- `at.json` at project root holds `{ "container_port": N, "env_vars": {} }`
- List apps: `GET http://localhost:8080/api/apps`
- Deploy: `POST http://localhost:8080/api/apps/{id}/deploy`
- Deployment status: `GET http://localhost:8080/api/deployments/{id}`
- Always keep the smallest Alpine-based base image in the Dockerfile

---

## Step 1 — Identify the Target App

If `$ARGUMENTS` already names a project, use it. Otherwise:

1. List all registered apps:
   ```bash
   curl -s http://localhost:8080/api/apps
   ```
2. Show the user the available app names and ask which one they want to work on.

Do not proceed until you know the target project name and its `id`.

---

## Step 2 — Understand What to Change

If `$ARGUMENTS` includes a description of changes, use it. Otherwise ask the user in one message:

1. **What to change** — a clear description of the improvement or fix (e.g. "add a /health endpoint", "change the background colour to dark blue", "add rate limiting").
2. **Anything else** — port changes, new env vars, dependency additions, etc. (only ask if not obvious).

Read the current project files before drafting any changes:

```bash
ls ./projects/<name>/
```

Read `Dockerfile`, `at.json`, and all relevant source files so you fully understand the current state before touching anything.

---

## Step 3 — Plan and Confirm (for non-trivial changes)

If the change affects more than one or two lines, briefly tell the user:
- Which files will be modified / added / removed
- Any new dependencies being introduced

Ask for confirmation if the change is significant (e.g. restructuring, adding a dependency, changing the Dockerfile base image). Skip confirmation for small, obvious edits.

---

## Step 4 — Apply Changes

Edit only the files that need to change. Follow these rules:

- **Dockerfile**: Keep Alpine/slim base. Only change if the runtime genuinely needs it (e.g. adding a build step for a new dependency).
- **Source files**: Make targeted edits — don't rewrite files that don't need changing.
- **`at.json`**: Update only if `container_port` or `env_vars` actually changed.
- **New files**: Add only what is needed (e.g. a new route file, a static asset).
- Do not reformat or refactor code unrelated to the requested change.

---

## Step 5 — Redeploy

Trigger a new deployment:

```bash
curl -s -X POST http://localhost:8080/api/apps/<id>/deploy
```

Save the returned `deployment_id`.

---

## Step 6 — Wait and Report

Poll every 5 seconds (up to ~90 seconds) until `status` is `running` or `failed`:

```bash
curl -s http://localhost:8080/api/deployments/<deployment_id>
```

- If `running`: confirm success and summarise what changed. Fetch the app's `domain` field from `GET http://localhost:8080/api/apps`, construct the full URL as `https://<domain>` (the `domain` field already contains the full hostname, e.g. `hello.at.feryand.in`), and present it as a clickable link so the user can open the updated app directly.
- If `failed`: show the `error` and last 500 chars of `logs`. Offer to diagnose and fix the issue.

---

## Guidelines

- Always read existing files before editing — never assume their content.
- Make the smallest change that achieves the goal; don't improve unrelated things.
- If the user's request is ambiguous, clarify before making changes.
- If a change introduces a new dependency, make sure it's installed inside the Docker image (via the Dockerfile), not just on the host.
