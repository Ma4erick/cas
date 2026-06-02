# CAS — Collaborative Agent Sessions

A real-time collaborative coding assistant built in Go. Multiple teammates share a browser-based chat session backed by Claude, with the ability to read and edit files, run commands, and commit code — all on a shared server.

---

## Features

- **Shared sessions** — Multiple users connect to the same session via WebSocket. Everyone sees messages, agent responses, and tool activity in real time
- **Agentic coding** — The agent can read/write files, list directories, and run allowed shell commands (`go`, `git`) directly in the project
- **Per-session working directories** — Each session can point at a different project folder or GitHub repo
- **GitHub auto-clone** — Paste a GitHub URL when creating a session and CAS clones it automatically (or pulls if already cloned)
- **Live tool visibility** — File edits and command output appear inline in the chat so all teammates can see what the agent is doing
- **Persistent sessions** — Sessions and message history are saved to disk and restored on restart

---

## Getting Started

### Prerequisites

- Go 1.23+
- An [Anthropic API key](https://console.anthropic.com/)
- Git (for repo cloning and agent git operations)

### Install & Run

```bash
git clone https://github.com/your-org/cas-go
cd cas-go
go run .
```

The server starts at `http://localhost:8080`. Share the local network URL (printed on startup) with teammates.

### API Key

CAS looks for your Anthropic API key in this order:

1. `--key` flag: `go run . --key sk-ant-...`
2. `ANTHROPIC_API_KEY` environment variable
3. `~/.cas.env` file containing `ANTHROPIC_API_KEY=sk-ant-...`

---

## Configuration

| Environment Variable | Default | Description |
|---|---|---|
| `CAS_PROJECTS_DIR` | `~/cas-projects` | Central folder for all project working directories |

```bash
CAS_PROJECTS_DIR=/home/user/projects \
go run .
```

---

## Sessions

### Creating a Session

Click **+** in the sidebar. You can optionally provide:

- **GitHub URL** — CAS clones the repo into `CAS_PROJECTS_DIR/<repo-name>` and sets it as the working directory. If the repo is already cloned, it does a `git pull` instead
- **Local path** — Point the session at an existing folder on the server
- **Neither** — Uses `CAS_WORK_DIR`

### Changing the Working Directory

Send `/cd /path/to/project` in any session to switch its working directory. All teammates see a notice confirming the change.

---

## Agent Capabilities

The agent has access to the following tools within each session's working directory:

| Tool | Description |
|---|---|
| `read_file` | Read any file in the project |
| `write_file` | Create or overwrite files |
| `list_files` | Browse the directory tree |
| `bash` | Run allowed shell commands (see below) |

### Allowed Shell Commands

For security, `bash` is restricted to:

```
go run, go build, go test, go vet, go fmt
git status, git diff, git add, git commit, git log, git push, git pull
```

---

## Sharing Context from Claude Code

A `/cas` slash command is included for Claude Code users. It creates a new CAS session pre-populated with a structured summary of your current Claude Code conversation — useful for handing off work to teammates.

**Install globally:**

```bash
cp .claude/commands/cas.md ~/.claude/commands/cas.md
```

**Usage** (inside Claude Code):

```
/cas My feature branch
```

This creates a session named "My feature branch" in the running CAS instance and posts a handoff summary as the first message.

---

## Architecture

```
browser (index.html)
    │  WebSocket + HTTP
    ▼
main.go  ── HTTP routes, embedded static files
hub.go   ── WebSocket hub, client registry, broadcast
session.go ── Session management, Claude API streaming, tool execution
```

- **Static files are embedded** into the binary at build time (`//go:embed static`). Any frontend change requires a rebuild (`go run .`)
- **WebSocket connections** are per-session. `BroadcastAll` sends session-list updates to every connected client across all sessions
- **Tool results** are streamed back to all teammates as collapsible blocks in the chat, with the full output available on click
- **Sessions** are persisted as JSON files in `CAS_DATA_DIR`

---

## Deployment

CAS is a single self-contained binary. For a shared team environment:

```bash
# Build
go build -o cas .

# Run with environment config
CAS_WORK_DIR=/home/user/projects \
ANTHROPIC_API_KEY=sk-ant-... \
./cas --port 8080
```

Point a reverse proxy (nginx, Caddy) at port 8080 to expose it over HTTPS.

---

## Model

CAS uses `claude-sonnet-4-6` by default. The model name is displayed as the sender label in each session's chat.
