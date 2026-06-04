# CAS — Collaborative Agent Sessions

> **The browser becomes the coding agent.**

CAS turns a web browser into a shared interface for a remote AI coding agent. Instead of each developer running a local AI assistant in isolation, the entire team connects to a single server where Claude can read files, write code, run commands, and commit to git — while everyone watches and participates in real time.

There is nothing to install on a user's machine. Any device with a browser can participate.

---

## How it works

```
┌─────────────────────────────────────────────────┐
│               CAS Server                        │
│  Go binary · Git · PostgreSQL · Redis           │
│  Holds the code. Runs the tools.                │
│  Streams everything to every browser.           │
└────────────────┬────────────────────────────────┘
                 │  WebSocket + HTTP
       ┌─────────┼─────────┐
       ▼         ▼         ▼
   Browser    Browser    Browser
   (Dev 1)   (Dev 2)   (Dev 3)
                              ↑
                        Any device.
                        No install.
```

The **browser** is the terminal — where users send instructions, see responses, and watch the agent work.  
The **server** is the execution environment — it holds the code, runs git, reads and writes files.  
The **Anthropic API** is the intelligence — Claude processes requests and decides what tools to call.

Each user authenticates with their own username and password. API keys are stored encrypted in PostgreSQL — never on the client.

---

## Features

- **Authentication** — Username/password login with bcrypt. Default `CAS/CAS` user created on fresh install. SSO integration planned
- **Persistent user profiles** — Display name, email, colour, model, and encrypted API keys stored in PostgreSQL. Profile follows the user across any browser or device
- **Shared sessions** — Multiple users in the same session via WebSocket. Everyone sees messages, agent responses, and tool activity in real time
- **Agentic coding** — The agent reads and writes files, runs shell commands, and operates git directly on the server's project folders. Interrupt mid-stream with the Stop button
- **Per-user Anthropic key and model** — Each user brings their own API key (stored server-side, encrypted). Choose Opus, Sonnet, or Haiku from the profile settings
- **Per-user message colour** — Each user picks a display colour for their messages
- **GitHub integration** — Paste a GitHub URL to clone a repo. Connect via GitHub OAuth (one-click) or paste a PAT. Token stored encrypted in DB, injected as `GH_TOKEN` for all `gh` CLI commands
- **Atlassian integration** — Connect via Atlassian OAuth or paste an API token. Stored encrypted in DB, injected as `ATLASSIAN_API_TOKEN` / `ATLASSIAN_BEARER_TOKEN` for Jira/Confluence REST API calls. Domain configured server-wide via `ATLASSIAN_DOMAIN`
- **Koda marketplace support** — Sessions automatically inject `CLAUDE.md` and `SKILL.md` files from the project into the agent's system prompt, enabling OutSystems coding standards and Koda workflow commands (`koda init`, `koda plugin add`, etc.)
- **Per-session working directories** — Each session points at a different project or repo on the server
- **Live tool visibility** — File edits and command output appear as collapsible blocks in the chat, visible to all teammates
- **File uploads** — Upload PDFs, images, and documents into the session. The agent reads and summarises them automatically
- **@mention autocomplete** — Type `@` to get a filtered dropdown of registered users, Slack-style
- **Unread message badges** — Per-user unread counts tracked in PostgreSQL, accurate across sessions and devices
- **Join / leave notices** — Fires only on genuine first join or explicit leave — not on page reloads or session switching
- **Admin panel** — Session management with git health checks (uncommitted/unpushed) and safe deletion with confirmation
- **Stop agent** — Cancel a running agent response mid-stream
- **Session search** — Search all sessions in the Discover section by name
- **Session compaction** — Type `/compact` to summarise the full session history with Claude, replacing old messages with a structured summary + last 5 messages. Saves tokens and keeps context relevant for long-running sessions
- **Compact reminder** — An amber banner appears when opening a session with 50+ messages or one inactive for more than a day, suggesting `/compact`
- **Date separators** — The first message of each day shows a styled date pill (Today / Yesterday / full date), using each user's local timezone
- **Multi-pod ready** — Redis pub/sub broadcasts WebSocket events across all pod instances
- **Claude Code integration** — `/cas` and `/cas-pull` slash commands (with authentication) to push and pull context between a local Claude Code session and CAS

---

## Server Requirements

- **Go 1.23+** — to build the binary
- **Git** — for cloning repos and agent git operations
- **PostgreSQL** — user profiles, session history, messages, membership
- **Redis** — WebSocket pub/sub for multi-pod deployments (optional for single-pod)

**Claude Code is not required on the server.** It is an optional integration for individual developers.

---

## Getting Started

```bash
git clone https://github.com/Ma4erick/cas
cd cas
go build -o cas .
./cas
```

The server starts at `http://localhost:8080`. On first run with a database configured, a default `CAS / CAS` user is created automatically.

### Verbose logging

```bash
./cas --verbose
```

Logs all HTTP requests, WebSocket events, DB writes, and Redis pub/sub activity.

---

## Authentication

CAS uses username/password authentication with bcrypt password hashing.

- **Login** — username + password
- **Register** — create an account via the Register link on the login screen (to be replaced with SSO)
- **Default user** — `CAS / CAS` is created automatically on a fresh install if no registered users exist
- **Logout** — available in the profile settings (⚙ next to your name)

---

## User Profile

Each user configures their profile via the **⚙ icon** next to their name at the bottom of the sidebar:

| Setting | Description |
|---|---|
| **Display name** | Shown alongside your messages |
| **Email** | Your account email — used for Atlassian API authentication |
| **Colour** | Your message colour — visible to everyone in sessions |
| **Model** | Choose Opus, Sonnet, or Haiku per user |
| **Anthropic API Key** | `sk-ant-…` — encrypted and stored in PostgreSQL |
| **GitHub** | Click **Connect with GitHub** (OAuth) or paste a PAT. Encrypted in DB, injected as `GH_TOKEN` for `gh` CLI commands. Button shows **✓ Connected as @username** when authenticated |
| **Atlassian** | Click **Connect with Atlassian** (OAuth 2.0) or paste an API token. Encrypted in DB, injected as `ATLASSIAN_API_TOKEN`. Note: enterprise orgs require an admin to approve the OAuth app before OAuth can be used |

All settings persist across browsers and devices once saved.

---

## Configuration

Server behaviour is controlled via `~/.cas.env` or environment variables:

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | — | PostgreSQL connection string (required for persistent profiles) |
| `CAS_SECRET` | — | 32-byte secret for AES-256-GCM encryption of API keys (required with DB) |
| `REDIS_URL` | — | Redis connection string for multi-pod WebSocket pub/sub |
| `CAS_PROJECTS_DIR` | `~/cas-projects` | Root folder for all project working directories |
| `CAS_MODEL` | `claude-sonnet-4-6` | Server fallback model |
| `ATLASSIAN_DOMAIN` | — | Org-wide Atlassian domain e.g. `yourcompany.atlassian.net` — injected for all users |
| `CAS_BASE_URL` | `http://<host>` | Public base URL of CAS — used as the OAuth redirect URI. Required for Atlassian OAuth |
| `GITHUB_CLIENT_ID` | — | GitHub OAuth App client ID — enables one-click Connect with GitHub in user profiles |
| `GITHUB_CLIENT_SECRET` | — | GitHub OAuth App client secret |
| `ATLASSIAN_CLIENT_ID` | — | Atlassian OAuth 2.0 (3LO) client ID — enables Connect with Atlassian in user profiles |
| `ATLASSIAN_CLIENT_SECRET` | — | Atlassian OAuth 2.0 client secret |
| `SHOW_TOKEN_FIELDS` | `false` | Set to `true` to show manual API token paste fields in the profile modal |

```
# ~/.cas.env
DATABASE_URL=postgresql://user@localhost:5432/cas?sslmode=disable
CAS_SECRET=your-32-character-random-secret
REDIS_URL=redis://localhost:6379
CAS_PROJECTS_DIR=/home/user/projects
CAS_MODEL=claude-sonnet-4-6
ATLASSIAN_DOMAIN=yourcompany.atlassian.net
```

CAS creates the database and runs all schema migrations automatically on startup.

---

## Sessions

### Creating a Session

Press **⌘K** or click **✦ New Session**. Options:

- **GitHub URL** — CAS clones the repo into `CAS_PROJECTS_DIR/<repo-name>`. Uses the user's encrypted GitHub token for private repos
- **Existing folder** — Browse folders already in `CAS_PROJECTS_DIR` via the dropdown
- **Session name only** — A new empty folder is created automatically

### Sidebar sections

| Section | Contents |
|---|---|
| **My Sessions** | Sessions you are currently joined to. Sortable by activity, A–Z, Z–A |
| **Recent Sessions** | Sessions you have previously joined and left |
| **Discover** | Latest 10 sessions you have never joined, with live search |

Unread message badges appear on sessions with new messages since you last viewed them.

### Session Data

Project files live in `CAS_PROJECTS_DIR`. Session history and messages are stored in PostgreSQL. Uploads live in a hidden `.cas/uploads/` folder inside the project (gitignored automatically).

### Changing the Working Directory

Send `/cd /path/to/project` in any session to switch its working directory.

---

## Agent Capabilities

The agent has access to the following tools within each session's working directory:

| Tool | Description |
|---|---|
| `read_file` | Read any file in the project |
| `write_file` | Create or overwrite files |
| `list_files` | Browse the directory tree |
| `bash` | Run allowed shell commands |

### Allowed Shell Commands

```
go run · go build · go test · go vet · go fmt
git fetch · git checkout · git branch · git merge · git rebase
git status · git diff · git add · git commit · git log · git push · git pull
git stash · git remote · git show · git reset
```

---

## Claude Code Integration

Two slash commands bridge local Claude Code sessions with CAS:

| Command | Direction | Description |
|---|---|---|
| `/cas My session` | Claude Code → CAS | Export your local conversation as a structured handoff into a new CAS session |
| `/cas-pull byg` | CAS → Claude Code | Pull a CAS session's context into your local Claude Code instance |

**Install:**

```bash
cp .claude/commands/cas.md ~/.claude/commands/cas.md
cp .claude/commands/cas-pull.md ~/.claude/commands/cas-pull.md
```

---

## Architecture

```
browser (index.html)
    │  WebSocket + HTTP (authenticated via cas-user-id cookie)
    ▼
main.go    ── HTTP routes, auth endpoints, admin, embedded static files
hub.go     ── WebSocket hub, Redis pub/sub subscriber, join/leave broadcast
session.go ── Session lifecycle, Claude API streaming, tool execution, git ops
db.go      ── PostgreSQL: users, sessions, messages, membership, unread tracking
redis.go   ── Redis connection and channel constants
crypto.go  ── AES-256-GCM encryption/decryption for stored credentials
```

- **Single binary** — static files are embedded at build time. Rebuild with `go build` after any change
- **Per-user Anthropic clients** — a fresh client is created per request using the user's decrypted key from DB
- **Messages lazy-loaded** — session metadata loads at startup; messages load from DB on first WS connection
- **Redis pub/sub** — all WebSocket broadcasts go through Redis, enabling true multi-pod deployments

---

## Multi-Pod Deployment (K8s)

```
                 ┌──────────────────────┐
                 │    Load Balancer      │
                 └──────────┬───────────┘
                            │
         ┌──────────────────┼──────────────────┐
         ▼                  ▼                  ▼
    ┌─────────┐       ┌─────────┐       ┌─────────┐
    │ CAS Pod │       │ CAS Pod │       │ CAS Pod │
    └────┬────┘       └────┬────┘       └────┬────┘
         │                 │                 │
    ┌────▼─────────────────▼─────────────────▼────┐
    │              PostgreSQL                      │
    │  users · sessions · messages · membership   │
    └─────────────────────────────────────────────┘
    ┌─────────────────────────────────────────────┐
    │              Redis                           │
    │  WebSocket pub/sub across pods              │
    └─────────────────────────────────────────────┘
    ┌─────────────────────────────────────────────┐
    │    Shared NFS / EFS (CAS_PROJECTS_DIR)       │
    │    git repos · uploads · source files       │
    └─────────────────────────────────────────────┘
```

Each pod connects to the same PostgreSQL and Redis on startup. WebSocket events published by any pod are received by all pods and forwarded to their local clients.

---

## Deployment

```bash
go build -o cas .

DATABASE_URL=postgresql://user:pass@db:5432/cas \
CAS_SECRET=your-secret \
REDIS_URL=redis://redis:6379 \
CAS_PROJECTS_DIR=/mnt/efs/cas-projects \
./cas --port 8080
```

Point a reverse proxy (nginx, Caddy) at port 8080 to expose over HTTPS. HTTPS is required in production — API keys transit between browser and server on initial setup.
