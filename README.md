# CAS — Collaborative Agent Sessions

> **The browser becomes the coding agent.**

CAS turns a web browser into a shared interface for a remote AI coding agent. Instead of each developer running a local AI assistant in isolation, the entire team connects to a single server where Claude can read files, write code, run commands, and commit to git — while everyone watches and participates in real time.

There is nothing to install on a user's machine. Any device with a browser can participate.

---

## How it works

```
┌─────────────────────────────────────────┐
│           CAS Server                    │
│                                         │
│  Go binary · Git · ~/cas-projects/      │
│                                         │
│  Holds the code. Runs the tools.        │
│  Streams everything to every browser.   │
└────────────────┬────────────────────────┘
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

Each user authenticates with their own Anthropic API key, stored in their browser. The server never holds API keys. Rate limits and costs are per-user, not shared.

---

## Features

- **Shared sessions** — Multiple users in the same session via WebSocket. Everyone sees messages, agent responses, and tool activity in real time
- **Agentic coding** — The agent reads and writes files, runs shell commands, and operates git directly on the server's project folders
- **Per-user API keys** — Each user brings their own Anthropic key. No shared server key required
- **Model selection** — Each user chooses their own model (Opus, Sonnet, Haiku) from their profile
- **GitHub integration** — Paste a GitHub URL to clone a repo into a session. Private repos use the user's own GitHub token stored in their browser
- **Per-session working directories** — Each session can point at a different project or repo on the server
- **Live tool visibility** — File edits and command output appear as collapsible blocks in the chat, visible to all teammates
- **File uploads** — Upload PDFs, images, and documents into the session. The agent reads and summarises them automatically
- **Join / leave notices** — Teammates see who joins and leaves each session in real time
- **Claude Code integration** — `/cas` and `/cas-pull` slash commands to push and pull context between a local Claude Code session and CAS

---

## Server Requirements

The CAS server requires only:

- **Go 1.23+** — to build the binary
- **Git** — for cloning repos and agent git operations

**Claude Code is not required on the server.** It is an optional integration for individual developers who want to use the `/cas` and `/cas-pull` slash commands in their local Claude Code sessions.

---

## Getting Started

```bash
git clone https://github.com/Ma4erick/cas
cd cas
go build -o cas .
./cas
```

The server starts at `http://localhost:8080`. Share the local network URL (printed on startup) with teammates. Each user opens it in their browser — no install required on their end.

---

## User Setup (browser)

Each user configures their profile via the **⚙ icon** in the bottom-left of the sidebar:

| Setting | Description |
|---|---|
| **Display name** | Shown alongside your messages |
| **Model** | Choose Opus, Sonnet, or Haiku per user |
| **Anthropic API Key** | `sk-ant-…` — stored in your browser only, never on the server |
| **GitHub Token** | `ghp_…` — used for cloning private repos, stored in your browser only |

---

## Configuration

Server behaviour is controlled via `~/.cas.env` or environment variables:

| Variable | Default | Description |
|---|---|---|
| `CAS_PROJECTS_DIR` | `~/cas-projects` | Root folder for all project working directories |
| `CAS_MODEL` | `claude-sonnet-4-6` | Server fallback model (used when no user model is specified) |

```
# ~/.cas.env
CAS_PROJECTS_DIR=/home/user/projects
CAS_MODEL=claude-sonnet-4-6
```

---

## Sessions

### Creating a Session

Press **⌘K** or click **✦ New Session**. Options:

- **GitHub URL** — CAS clones the repo into `CAS_PROJECTS_DIR/<repo-name>`. Uses the user's GitHub token for private repos
- **Existing folder** — Browse and select a folder already in `CAS_PROJECTS_DIR`
- **Session name only** — A new empty folder is created automatically

### Session Data

All CAS metadata lives in a hidden `.cas/` folder inside each project:

```
~/cas-projects/my-app/
├── .cas/
│   ├── session.json     ← session history
│   └── uploads/         ← uploaded files
├── main.go              ← clean source code
└── .gitignore           ← .cas/ added automatically
```

The `.cas/` folder is automatically added to `.gitignore` on first write.

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

## Admin

Click **⚙** in the sidebar header to open the admin panel. It shows all sessions with their git health (uncommitted changes, unpushed commits) and allows permanent deletion with a confirmation step.

---

## Architecture

```
browser (index.html)
    │  WebSocket + HTTP
    ▼
main.go    ── HTTP routes, embedded static files, admin endpoints
hub.go     ── WebSocket hub, client registry, broadcast (join/leave/tools/stream)
session.go ── Session lifecycle, Claude API streaming, tool execution, git ops
```

- **Single binary** — static files are embedded at build time (`//go:embed static`). Rebuild with `go build` after any change
- **Per-user Anthropic clients** — a fresh client is created per request using the user's own API key
- **Per-user model selection** — model is chosen per user, sent with each message
- **WebSocket per session** — `BroadcastAll` sends session-list updates to every connected client across all sessions

---

## Deployment

```bash
go build -o cas .

CAS_PROJECTS_DIR=/home/user/projects ./cas --port 8080
```

Point a reverse proxy (nginx, Caddy) at port 8080 to expose over HTTPS. HTTPS is strongly recommended since API keys transit between browser and server.
