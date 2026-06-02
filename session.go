package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Data types
// ---------------------------------------------------------------------------

type Message struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Sender    string    `json:"sender,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

type Session struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	WorkDir   string    `json:"workDir"`
	Messages  []Message `json:"messages"`
	CreatedAt time.Time `json:"createdAt"`
	mu        sync.RWMutex
	streaming bool
	cloning   bool
}

func (s *Session) AddMessage(msg Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = append(s.Messages, msg)
}

func (s *Session) GetMessages() []Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Message, len(s.Messages))
	copy(out, s.Messages)
	return out
}

func (s *Session) toAPIMessages() []anthropic.MessageParam {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var params []anthropic.MessageParam
	for _, msg := range s.Messages {
		content := msg.Content
		if msg.Role == "user" && msg.Sender != "" {
			content = fmt.Sprintf("[%s]: %s", msg.Sender, content)
		}
		if msg.Role == "user" {
			params = append(params, anthropic.NewUserMessage(anthropic.NewTextBlock(content)))
		} else {
			params = append(params, anthropic.NewAssistantMessage(anthropic.NewTextBlock(content)))
		}
	}
	return params
}

type SessionSummary struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	WorkDir      string    `json:"workDir"`
	CreatedAt    time.Time `json:"createdAt"`
	MessageCount int       `json:"messageCount"`
}

// ---------------------------------------------------------------------------
// SessionManager
// ---------------------------------------------------------------------------

type SessionManager struct {
	sessions    map[string]*Session
	mu          sync.RWMutex
	client      anthropic.Client
	hub         *Hub
	projectsDir string // central location for all project folders
	model       anthropic.Model
}

func (sm *SessionManager) Model() anthropic.Model { return sm.model }
func (sm *SessionManager) ProjectsDir() string    { return sm.projectsDir }

func NewSessionManager(apiKey string, hub *Hub) *SessionManager {
	client := anthropic.NewClient(option.WithAPIKey(apiKey))

	projectsDir := os.Getenv("CAS_PROJECTS_DIR")
	if projectsDir == "" {
		home, _ := os.UserHomeDir()
		projectsDir = filepath.Join(home, "cas-projects")
	}
	if abs, err := filepath.Abs(projectsDir); err == nil {
		projectsDir = abs
	}
	if err := os.MkdirAll(projectsDir, 0755); err != nil {
		log.Fatalf("failed to create projects directory %s: %v", projectsDir, err)
	}
	log.Printf("CAS projects directory: %s", projectsDir)

	sm := &SessionManager{
		sessions:    make(map[string]*Session),
		client:      client,
		hub:         hub,
		projectsDir: projectsDir,
		model:       anthropic.ModelClaudeSonnet4_6,
	}
	sm.loadFromDisk()

	// Background ticker: prune sessions whose folders have been manually removed.
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			sm.pruneDeletedSessions()
		}
	}()

	return sm
}

// ---------------------------------------------------------------------------
// Disk persistence
// ---------------------------------------------------------------------------

func (sm *SessionManager) sessionPath(session *Session) string {
	return filepath.Join(session.WorkDir, ".cas-session.json")
}

// ensureGitIgnore adds .cas-session.json to the project's .gitignore if not already present.
func ensureGitIgnore(dir string) {
	gitignorePath := filepath.Join(dir, ".gitignore")
	entry := ".cas-session.json"

	data, err := os.ReadFile(gitignorePath)
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == entry {
				return // already present
			}
		}
		// Append with a newline
		f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_WRONLY, 0644)
		if err == nil {
			defer f.Close()
			content := string(data)
			if len(content) > 0 && !strings.HasSuffix(content, "\n") {
				f.WriteString("\n")
			}
			f.WriteString(entry + "\n")
		}
	} else {
		// No .gitignore yet — create one
		os.WriteFile(gitignorePath, []byte(entry+"\n"), 0644)
	}
}

func (sm *SessionManager) saveToDisk(session *Session) {
	session.mu.RLock()
	data, err := json.Marshal(session)
	path := sm.sessionPath(session)
	session.mu.RUnlock()
	if err != nil {
		log.Printf("failed to marshal session %s: %v", session.ID, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		log.Printf("failed to create dir for session %s: %v", session.ID, err)
		return
	}
	ensureGitIgnore(filepath.Dir(path))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("failed to write session %s: %v", session.ID, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("failed to rename session file %s: %v", session.ID, err)
	}
}

func (sm *SessionManager) loadFromDisk() {
	loaded := 0
	entries, err := os.ReadDir(sm.projectsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(sm.projectsDir, entry.Name(), ".cas-session.json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s Session
		if err := json.Unmarshal(data, &s); err != nil {
			log.Printf("failed to parse session file %s: %v", path, err)
			continue
		}
		if s.WorkDir == "" {
			s.WorkDir = filepath.Join(sm.projectsDir, entry.Name())
		}
		sm.sessions[s.ID] = &s
		loaded++
	}
	log.Printf("loaded %d session(s) from disk", loaded)
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func (sm *SessionManager) GetSession(id string) (*Session, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	s, ok := sm.sessions[id]
	return s, ok
}

func (sm *SessionManager) DeleteSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	sm.mu.Lock()
	session, ok := sm.sessions[sessionID]
	if ok {
		delete(sm.sessions, sessionID)
	}
	sm.mu.Unlock()

	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	if err := os.Remove(sm.sessionPath(session)); err != nil && !os.IsNotExist(err) {
		log.Printf("failed to delete session file %s: %v", sessionID, err)
	}

	sm.hub.BroadcastSessionList(sm.sessionList())
	w.WriteHeader(http.StatusNoContent)
}

func (sm *SessionManager) sessionList() []SessionSummary {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	list := make([]SessionSummary, 0, len(sm.sessions))
	for _, s := range sm.sessions {
		s.mu.RLock()
		list = append(list, SessionSummary{
			ID:           s.ID,
			Name:         s.Name,
			WorkDir:      s.WorkDir,
			CreatedAt:    s.CreatedAt,
			MessageCount: len(s.Messages),
		})
		s.mu.RUnlock()
	}
	return list
}

// pruneDeletedSessions removes any sessions whose working directory no longer exists.
func (sm *SessionManager) pruneDeletedSessions() {
	sm.mu.Lock()
	var removed []string
	for id, s := range sm.sessions {
		s.mu.RLock()
		dir := s.WorkDir
		cloning := s.cloning
		s.mu.RUnlock()
		if cloning {
			continue // folder may be temporarily absent during clone
		}
		if dir != "" {
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				delete(sm.sessions, id)
				removed = append(removed, id)
			}
		}
	}
	sm.mu.Unlock()

	if len(removed) > 0 {
		log.Printf("pruned %d session(s) with missing project folders", len(removed))
		sm.hub.BroadcastSessionList(sm.sessionList())
	}
}

// injectToken returns an authenticated clone URL when a GitHub token is available.
// e.g. https://github.com/org/repo -> https://oauth2:<token>@github.com/org/repo
func injectToken(rawURL, token string) string {
	if token == "" {
		return rawURL
	}
	for _, prefix := range []string{"https://github.com/", "https://www.github.com/"} {
		if strings.HasPrefix(rawURL, prefix) {
			return "https://x-access-token:" + token + "@github.com/" + strings.TrimPrefix(rawURL, prefix)
		}
	}
	return rawURL
}

func isAuthError(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "authentication failed") ||
		strings.Contains(lower, "invalid username or token") ||
		strings.Contains(lower, "could not read username") ||
		strings.Contains(lower, "permission denied") ||
		strings.Contains(lower, "repository not found")
}

// repoNameFromURL extracts the repo name from a GitHub URL.
// e.g. "https://github.com/org/my-app.git" -> "my-app"
func repoNameFromURL(rawURL string) string {
	rawURL = strings.TrimSuffix(strings.TrimSpace(rawURL), ".git")
	parts := strings.Split(rawURL, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return "repo"
}

// cloneInBackground clones or pulls repoURL in a goroutine, streaming progress
// into the session chat via WebSocket system messages.
func (sm *SessionManager) cloneInBackground(session *Session, repoURL string, githubToken string) {
	session.mu.Lock()
	session.cloning = true
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		session.cloning = false
		session.mu.Unlock()
	}()

	sessionID := session.ID
	dest := filepath.Join(sm.projectsDir, repoNameFromURL(repoURL))

	broadcast := func(text string, isErr bool) {
		msg := Message{
			ID:        uuid.New().String(),
			Role:      "assistant",
			Content:   text,
			Sender:    "CAS",
			Timestamp: time.Now(),
		}
		session.AddMessage(msg)
		sm.saveToDisk(session)
		sm.hub.BroadcastToSession(sessionID, WSMessage{Type: "user_message", Message: &msg})
		sm.hub.BroadcastSessionList(sm.sessionList())
	}

	isPull := false
	if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
		isPull = true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var cmd *exec.Cmd
	if isPull {
		broadcast(fmt.Sprintf("⟳ Pulling latest changes for `%s`…", repoNameFromURL(repoURL)), false)
		cmd = exec.CommandContext(ctx, "git", "-C", dest, "pull")
	} else {
		broadcast(fmt.Sprintf("⬇ Cloning `%s` into `%s`…", repoURL, dest), false)
		// Remove any existing folder right here — after broadcast (which calls
		// saveToDisk and recreates the dir) but before git clone runs.
		if _, err := os.Stat(dest); err == nil {
			if err := os.RemoveAll(dest); err != nil {
				broadcast(fmt.Sprintf("❌ Could not clear destination folder: %s", err), true)
				return
			}
		}
		cloneURL := injectToken(repoURL, githubToken)
		cmd = exec.CommandContext(ctx, "git", "clone",
			"--progress",
			"--config", "credential.helper=",
			"--config", "core.askPass=echo",
			cloneURL, dest)
		cmd.Env = append(os.Environ(),
			"GIT_TERMINAL_PROMPT=0",
			"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=no",
		)
	}

	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))

	if err != nil {
		if isAuthError(output) {
			var msg string
			if githubToken != "" {
				msg = "❌ Authentication failed — your GitHub token doesn't have access to this repository.\n\nCheck that your token has the correct scopes (`repo` for private repos) and is authorised for the organisation if SSO is enabled.\n\nUpdate your token in the account area at the bottom-left of the sidebar."
			} else {
				msg = "❌ Authentication failed — this looks like a private repository.\n\nEnter your GitHub Personal Access Token in the account area at the bottom-left of the sidebar, then try again.\n\nGenerate a token at: https://github.com/settings/tokens (requires `repo` scope)."
			}
			broadcast(msg, true)
		} else {
			msg := fmt.Sprintf("❌ Git operation failed: %s", err)
			if output != "" {
				msg += "\n```\n" + output + "\n```"
			}
			broadcast(msg, true)
		}
		return
	}

	// Update the session's WorkDir now that clone succeeded.
	session.mu.Lock()
	session.WorkDir = dest
	session.mu.Unlock()
	sm.saveToDisk(session)
	sm.hub.BroadcastSessionList(sm.sessionList())

	verb := "pulled"
	if !isPull {
		verb = "cloned"
	}
	broadcast(fmt.Sprintf("✅ Successfully %s into `%s`. Ready to go.", verb, dest), false)
}

// sanitizeFolderName converts a session name into a safe directory name.
func sanitizeFolderName(name string) string {
	replacer := strings.NewReplacer(" ", "-", "/", "-", "\\", "-", ":", "-", "*", "-", "?", "-", "\"", "-", "<", "-", ">", "-", "|", "-")
	return strings.Trim(replacer.Replace(strings.ToLower(name)), "-")
}

func (sm *SessionManager) CreateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		FolderName  string `json:"folderName"`
		RepoURL     string `json:"repoURL"`
		GithubToken string `json:"githubToken"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	// If a GitHub URL is provided, derive the name and folder from the repo.
	// The actual clone runs in the background after the session is created.
	repoURL := strings.TrimSpace(req.RepoURL)
	if repoURL != "" {
		if strings.TrimSpace(req.Name) == "" {
			req.Name = repoNameFromURL(repoURL)
		}
		if strings.TrimSpace(req.FolderName) == "" {
			req.FolderName = repoNameFromURL(repoURL)
		}
	}

	if strings.TrimSpace(req.Name) == "" {
		req.Name = "New Session"
	}

	// Reject duplicate session names.
	sm.mu.RLock()
	for _, s := range sm.sessions {
		s.mu.RLock()
		exists := strings.EqualFold(s.Name, strings.TrimSpace(req.Name))
		s.mu.RUnlock()
		if exists {
			sm.mu.RUnlock()
			http.Error(w, "a session named \""+strings.TrimSpace(req.Name)+"\" already exists", http.StatusConflict)
			return
		}
	}
	sm.mu.RUnlock()

	// Determine working directory — always within projectsDir.
	var workDir string
	if folder := strings.TrimSpace(req.FolderName); folder != "" {
		// User picked an existing folder.
		workDir = filepath.Join(sm.projectsDir, filepath.Base(folder))
	} else {
		// Auto-create a folder from the session name.
		workDir = filepath.Join(sm.projectsDir, sanitizeFolderName(req.Name))
		if err := os.MkdirAll(workDir, 0755); err != nil {
			http.Error(w, "failed to create project folder: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	session := &Session{
		ID:        uuid.New().String(),
		Name:      req.Name,
		WorkDir:   workDir,
		Messages:  []Message{},
		CreatedAt: time.Now(),
	}

	sm.mu.Lock()
	sm.sessions[session.ID] = session
	sm.mu.Unlock()

	sm.saveToDisk(session)
	sm.hub.BroadcastSessionList(sm.sessionList())

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(session)

	// Kick off the clone in the background now that the session exists.
	if repoURL != "" {
		go sm.cloneInBackground(session, repoURL, req.GithubToken)
	}
}

func (sm *SessionManager) ListSessions(w http.ResponseWriter, r *http.Request) {
	sm.pruneDeletedSessions()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sm.sessionList())
}

func (sm *SessionManager) SendMessage(w http.ResponseWriter, r *http.Request, sessionID string) {
	session, ok := sm.GetSession(sessionID)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	session.mu.Lock()
	if session.streaming {
		session.mu.Unlock()
		http.Error(w, "agent is currently responding", http.StatusConflict)
		return
	}
	session.mu.Unlock()

	var req struct {
		Content string `json:"content"`
		Sender  string `json:"sender"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Content) == "" {
		http.Error(w, "content required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Sender) == "" {
		req.Sender = "Anonymous"
	}

	// Handle /cd command to change the session's working directory.
	if strings.HasPrefix(strings.TrimSpace(req.Content), "/cd ") {
		newDir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(req.Content), "/cd "))
		abs, err := filepath.Abs(newDir)
		if err != nil {
			http.Error(w, "invalid path: "+err.Error(), http.StatusBadRequest)
			return
		}
		if _, err := os.Stat(abs); err != nil {
			http.Error(w, "directory not found: "+abs, http.StatusBadRequest)
			return
		}
		session.mu.Lock()
		session.WorkDir = abs
		session.mu.Unlock()
		sm.saveToDisk(session)
		sm.hub.BroadcastSessionList(sm.sessionList())
		sm.hub.BroadcastToSession(sessionID, WSMessage{
			Type:  "work_dir_changed",
			Delta: abs,
		})
		w.WriteHeader(http.StatusAccepted)
		return
	}

	userMsg := Message{
		ID:        uuid.New().String(),
		Role:      "user",
		Content:   req.Content,
		Sender:    req.Sender,
		Timestamp: time.Now(),
	}
	session.AddMessage(userMsg)

	sm.hub.BroadcastToSession(sessionID, WSMessage{
		Type:    "user_message",
		Message: &userMsg,
	})
	sm.saveToDisk(session)
	sm.hub.BroadcastSessionList(sm.sessionList())

	w.WriteHeader(http.StatusAccepted)

	// Messages starting with @ are teammate callouts — skip the agent.
	if !strings.HasPrefix(strings.TrimSpace(req.Content), "@") {
		go sm.streamResponse(sessionID, session)
	}
}

// ---------------------------------------------------------------------------
// Tool definitions
// ---------------------------------------------------------------------------

func makeTool(name, description string, props map[string]interface{}, required []string) anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
		Name:        name,
		Description: anthropic.String(description),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: props,
			Required:   required,
		},
	}}
}

var casTools = []anthropic.ToolUnionParam{
	makeTool("read_file", "Read the contents of a file in the project.",
		map[string]interface{}{
			"path": map[string]interface{}{"type": "string", "description": "Path relative to project root."},
		}, []string{"path"}),

	makeTool("write_file", "Write or overwrite a file in the project with the given content.",
		map[string]interface{}{
			"path":    map[string]interface{}{"type": "string", "description": "Path relative to project root."},
			"content": map[string]interface{}{"type": "string", "description": "Full content to write."},
		}, []string{"path", "content"}),

	makeTool("list_files", "List files in a directory of the project.",
		map[string]interface{}{
			"path": map[string]interface{}{"type": "string", "description": "Directory path relative to project root. Use '.' for root."},
		}, []string{"path"}),

	makeTool("bash", "Run an allowed shell command in the project directory. Allowed: go run/build/test/vet/fmt, git status/diff/add/commit/log/push/pull/fetch/checkout/branch/merge/rebase/stash/remote/show/reset.",
		map[string]interface{}{
			"command": map[string]interface{}{"type": "string", "description": "The shell command to run."},
		}, []string{"command"}),
}

// ---------------------------------------------------------------------------
// Tool execution
// ---------------------------------------------------------------------------

var allowedCommands = []string{
	"go run", "go build", "go test", "go vet", "go fmt",
	"git status", "git diff", "git add", "git commit", "git log", "git push", "git pull",
	"git fetch", "git checkout", "git branch", "git merge", "git rebase", "git stash",
	"git remote", "git show", "git reset",
}

func isAllowed(cmd string) bool {
	trimmed := strings.TrimSpace(cmd)
	for _, prefix := range allowedCommands {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

func (sm *SessionManager) executeTool(name string, rawInput json.RawMessage, workDir string) (string, bool) {
	var input map[string]string
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return fmt.Sprintf("invalid tool input: %v", err), true
	}

	switch name {
	case "read_file":
		path := filepath.Join(workDir, filepath.Clean(input["path"]))
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Sprintf("error reading file: %v", err), true
		}
		return string(data), false

	case "write_file":
		path := filepath.Join(workDir, filepath.Clean(input["path"]))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return fmt.Sprintf("error creating directories: %v", err), true
		}
		if err := os.WriteFile(path, []byte(input["content"]), 0644); err != nil {
			return fmt.Sprintf("error writing file: %v", err), true
		}
		return fmt.Sprintf("wrote %s", input["path"]), false

	case "list_files":
		dir := filepath.Join(workDir, filepath.Clean(input["path"]))
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Sprintf("error listing directory: %v", err), true
		}
		var lines []string
		for _, e := range entries {
			if e.IsDir() {
				lines = append(lines, e.Name()+"/")
			} else {
				lines = append(lines, e.Name())
			}
		}
		return strings.Join(lines, "\n"), false

	case "bash":
		cmd := input["command"]
		if !isAllowed(cmd) {
			return fmt.Sprintf("command not allowed: %q\nAllowed: go run/build/test/vet/fmt, git status/diff/add/commit/log/push/pull", cmd), true
		}
		var out bytes.Buffer
		c := exec.Command("sh", "-c", cmd)
		c.Dir = workDir
		c.Stdout = &out
		c.Stderr = &out
		err := c.Run()
		result := out.String()
		if result == "" {
			result = "(no output)"
		}
		return result, err != nil

	default:
		return fmt.Sprintf("unknown tool: %s", name), true
	}
}

// ---------------------------------------------------------------------------
// Streaming response with tool use loop
// ---------------------------------------------------------------------------

func (sm *SessionManager) streamResponse(sessionID string, session *Session) {
	session.mu.Lock()
	session.streaming = true
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		session.streaming = false
		session.mu.Unlock()
	}()

	// Snapshot the working directory at the start of this response.
	session.mu.RLock()
	workDir := session.WorkDir
	session.mu.RUnlock()
	if workDir == "" {
		workDir = sm.projectsDir
	}

	// Build a local copy of the API conversation that we can extend with
	// tool results without writing them to the persistent session history.
	apiMessages := session.toAPIMessages()

	for {
		msgID := uuid.New().String()
		sm.hub.BroadcastToSession(sessionID, WSMessage{
			Type:      "stream_start",
			MessageID: msgID,
		})

		stream := sm.client.Messages.NewStreaming(context.Background(), anthropic.MessageNewParams{
			Model:     sm.model,
			MaxTokens: 8096,
			Tools:     casTools,
			System: []anthropic.TextBlockParam{
				{
					Text: fmt.Sprintf(
						"You are a collaborative coding assistant with access to the project at %s. "+
							"Multiple team members share this session — each user message is prefixed with [Name]:. "+
							"You can read and edit files, run allowed shell commands, and use git. "+
							"When making changes, explain what you're doing. Be concise and collaborative.",
						workDir,
					),
				},
			},
			Messages: apiMessages,
		})

		// Accumulate text and tool_use blocks from this turn.
		var textBuf strings.Builder
		type pendingTool struct {
			id    string
			name  string
			input json.RawMessage
		}
		var toolCalls []pendingTool
		var currentToolID, currentToolName string
		var currentToolInput strings.Builder

		for stream.Next() {
			event := stream.Current()
			switch e := event.AsAny().(type) {

			case anthropic.ContentBlockStartEvent:
				switch b := e.ContentBlock.AsAny().(type) {
				case anthropic.ToolUseBlock:
					currentToolID = b.ID
					currentToolName = b.Name
					currentToolInput.Reset()
				}

			case anthropic.ContentBlockDeltaEvent:
				switch d := e.Delta.AsAny().(type) {
				case anthropic.TextDelta:
					textBuf.WriteString(d.Text)
					sm.hub.BroadcastToSession(sessionID, WSMessage{
						Type:      "stream_chunk",
						MessageID: msgID,
						Delta:     d.Text,
					})
				case anthropic.InputJSONDelta:
					currentToolInput.WriteString(d.PartialJSON)
				}

			case anthropic.ContentBlockStopEvent:
				if currentToolID != "" {
					toolCalls = append(toolCalls, pendingTool{
						id:    currentToolID,
						name:  currentToolName,
						input: json.RawMessage(currentToolInput.String()),
					})
					currentToolID = ""
					currentToolName = ""
					currentToolInput.Reset()
				}
			}
		}

		if err := stream.Err(); err != nil {
			log.Printf("stream error for session %s: %v", sessionID, err)
			sm.hub.BroadcastToSession(sessionID, WSMessage{
				Type:  "error",
				Error: err.Error(),
			})
			return
		}

		sm.hub.BroadcastToSession(sessionID, WSMessage{
			Type:      "stream_end",
			MessageID: msgID,
		})

		// Save the assistant's text to the session (visible in history).
		if textBuf.Len() > 0 {
			assistantMsg := Message{
				ID:        msgID,
				Role:      "assistant",
				Content:   textBuf.String(),
				Timestamp: time.Now(),
			}
			session.AddMessage(assistantMsg)
			sm.saveToDisk(session)
			sm.hub.BroadcastSessionList(sm.sessionList())
		}

		// No tool calls — we're done.
		if len(toolCalls) == 0 {
			break
		}

		// Build assistant message for the API (text + tool_use blocks).
		var assistantBlocks []anthropic.ContentBlockParamUnion
		if textBuf.Len() > 0 {
			assistantBlocks = append(assistantBlocks, anthropic.NewTextBlock(textBuf.String()))
		}
		var resultBlocks []anthropic.ContentBlockParamUnion
		for _, tc := range toolCalls {
			// NewToolUseBlock signature: (id string, input any, name string)
			assistantBlocks = append(assistantBlocks, anthropic.NewToolUseBlock(tc.id, tc.input, tc.name))

			// Broadcast tool call to teammates.
			sm.hub.BroadcastToSession(sessionID, WSMessage{
				Type:      "tool_call",
				ToolName:  tc.name,
				ToolInput: string(tc.input),
			})

			// Execute the tool.
			output, isErr := sm.executeTool(tc.name, tc.input, workDir)

			// Broadcast result to teammates.
			sm.hub.BroadcastToSession(sessionID, WSMessage{
				Type:       "tool_result",
				ToolName:   tc.name,
				ToolOutput: output,
				IsError:    isErr,
			})

			resultBlocks = append(resultBlocks, anthropic.NewToolResultBlock(tc.id, output, isErr))
		}

		apiMessages = append(apiMessages,
			anthropic.NewAssistantMessage(assistantBlocks...),
			anthropic.NewUserMessage(resultBlocks...),
		)
	}
}
