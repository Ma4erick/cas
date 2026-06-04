package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	ID          string    `json:"id"`
	Role        string    `json:"role"`
	Content     string    `json:"content"`
	Sender      string    `json:"sender,omitempty"`
	SenderColor string    `json:"senderColor,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
}

type Session struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	WorkDir      string    `json:"workDir"`
	Messages     []Message `json:"messages"`
	CreatedAt    time.Time `json:"createdAt"`
	MessageCount int       `json:"messageCount,omitempty"` // cached count from DB
	mu             sync.RWMutex
	streaming      bool
	cloning        bool
	messagesLoaded bool
	cancelStream   context.CancelFunc
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
	UnreadCount  int       `json:"unreadCount"`
}

// ---------------------------------------------------------------------------
// SessionManager
// ---------------------------------------------------------------------------

type SessionManager struct {
	sessions    map[string]*Session
	mu          sync.RWMutex
	hub         *Hub
	projectsDir string // central location for all project folders
	model       anthropic.Model
}

func (sm *SessionManager) Model() anthropic.Model { return sm.model }
func (sm *SessionManager) ProjectsDir() string    { return sm.projectsDir }

func NewSessionManager(hub *Hub) *SessionManager {
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
		hub:         hub,
		projectsDir: projectsDir,
		model:       defaultModel(),
	}
	sm.loadSessions()

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

// casDir returns the hidden .cas directory for a session's project.
func casDir(workDir string) string {
	return filepath.Join(workDir, ".cas")
}

// ensureGitIgnore adds .cas/ to the project's .gitignore if not already present.
func ensureGitIgnore(workDir string) {
	gitignorePath := filepath.Join(workDir, ".gitignore")
	entry := ".cas/"
	data, err := os.ReadFile(gitignorePath)
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == entry {
				return
			}
		}
		f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_WRONLY, 0644)
		if err == nil {
			defer f.Close()
			if content := string(data); len(content) > 0 && !strings.HasSuffix(content, "\n") {
				f.WriteString("\n")
			}
			f.WriteString(entry + "\n")
		}
	} else {
		os.WriteFile(gitignorePath, []byte(entry+"\n"), 0644)
	}
}

// persistSession writes session metadata to DB (or falls back to disk if no DB).
func (sm *SessionManager) persistSession(session *Session) {
	if DB != nil {
		ctx := context.Background()
		session.mu.RLock()
		id, name, workDir, createdAt := session.ID, session.Name, session.WorkDir, session.CreatedAt
		session.mu.RUnlock()
		if err := DBCreateSession(ctx, id, name, workDir, createdAt); err != nil {
			log.Printf("failed to persist session %s: %v", id, err)
		}
		return
	}
	sm.saveToDisk(session)
}

// saveToDisk is kept as a fallback when no DB is configured.
func (sm *SessionManager) saveToDisk(session *Session) {
	session.mu.RLock()
	data, err := json.Marshal(session)
	path := filepath.Join(casDir(session.WorkDir), "session.json")
	session.mu.RUnlock()
	if err != nil {
		log.Printf("failed to marshal session %s: %v", session.ID, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return
	}
	ensureGitIgnore(session.WorkDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return
	}
	os.Rename(tmp, path)
}

// persistMessage writes a message to DB (or falls back to re-saving the whole session).
func (sm *SessionManager) persistMessage(session *Session, msg Message) {
	if DB != nil {
		logVerbose("db write: message %s in session %s (role=%s)", msg.ID, session.ID, msg.Role)
		if err := DBAddMessage(context.Background(), session.ID, msg.ID, msg.Role, msg.Content, msg.Sender, msg.SenderColor, msg.Timestamp); err != nil {
			log.Printf("failed to persist message %s: %v", msg.ID, err)
		}
		// Mark session as read for all users currently connected to it.
		for _, uid := range sm.hub.ActiveUserIDs(session.ID) {
			MarkSessionRead(context.Background(), uid, session.ID)
		}
		return
	}
	sm.persistSession(session)
}

func (sm *SessionManager) loadSessions() {
	if DB != nil {
		sm.loadFromDB()
		return
	}
	sm.loadFromDisk()
}

func (sm *SessionManager) loadFromDB() {
	ctx := context.Background()
	// Load metadata only — messages are lazy loaded when a user opens a session.
	dbSessions, err := DBListSessions(ctx)
	if err != nil {
		log.Printf("failed to load sessions from DB: %v", err)
		return
	}
	for _, ds := range dbSessions {
		sm.sessions[ds.ID] = &Session{
			ID:           ds.ID,
			Name:         ds.Name,
			WorkDir:      ds.WorkDir,
			CreatedAt:    ds.CreatedAt,
			MessageCount: ds.MessageCount,
			Messages:     nil, // loaded on demand
		}
	}

	// Migrate any remaining disk sessions not yet in DB.
	migrated := 0
	entries, _ := os.ReadDir(sm.projectsDir)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(sm.projectsDir, entry.Name(), ".cas", "session.json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s Session
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}
		if _, exists := sm.sessions[s.ID]; exists {
			continue // already in DB
		}
		if s.WorkDir == "" {
			s.WorkDir = filepath.Join(sm.projectsDir, entry.Name())
		}
		// Migrate to DB
		if err := DBCreateSession(ctx, s.ID, s.Name, s.WorkDir, s.CreatedAt); err == nil {
			for _, m := range s.Messages {
				DBAddMessage(ctx, s.ID, m.ID, m.Role, m.Content, m.Sender, m.SenderColor, m.Timestamp)
			}
			sm.sessions[s.ID] = &s
			migrated++
		}
	}
	if migrated > 0 {
		log.Printf("migrated %d session(s) from disk to DB", migrated)
	}
	log.Printf("loaded %d session(s) from DB", len(sm.sessions))
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
		path := filepath.Join(sm.projectsDir, entry.Name(), ".cas", "session.json")
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

	if DB != nil {
		if err := DBDeleteSession(context.Background(), sessionID); err != nil {
			log.Printf("failed to delete session from DB %s: %v", sessionID, err)
		}
	} else {
		path := filepath.Join(casDir(session.WorkDir), "session.json")
		os.Remove(path)
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
		count := s.MessageCount
		if s.messagesLoaded {
			count = len(s.Messages)
		}
		list = append(list, SessionSummary{
			ID:           s.ID,
			Name:         s.Name,
			WorkDir:      s.WorkDir,
			CreatedAt:    s.CreatedAt,
			MessageCount: count,
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
		if DB != nil {
			for _, id := range removed {
				DBDeleteSession(context.Background(), id)
			}
		}
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
func (sm *SessionManager) cloneInBackground(session *Session, repoURL string, githubToken string, githubPAT string) {
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
		sm.persistSession(session)
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
		_ = githubPAT // used below if OAuth token fails with auth error
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
			// OAuth token failed — retry with PAT if available.
			if githubToken != "" && githubPAT != "" {
				broadcast("⚠️ OAuth token failed (possibly pending SSO approval) — retrying with Personal Access Token…", false)
				patURL := injectToken(repoURL, githubPAT)
				cmd2 := exec.CommandContext(ctx, "git", "clone", "--progress",
					"--config", "credential.helper=",
					"--config", "core.askPass=echo",
					patURL, dest)
				cmd2.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0",
					"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=no")
				var out2 bytes.Buffer
				cmd2.Stdout = &out2
				cmd2.Stderr = &out2
				if err2 := cmd2.Run(); err2 == nil {
					// PAT clone succeeded.
					session.mu.Lock()
					session.WorkDir = dest
					session.mu.Unlock()
					sm.saveToDisk(session)
					sm.hub.BroadcastSessionList(sm.sessionList())
					verb := "cloned"
					broadcast(fmt.Sprintf("✅ Successfully %s into `%s` using Personal Access Token. Ready to go.", verb, dest), false)
					return
				}
			}

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
	if DB != nil {
		DBUpdateSessionWorkDir(context.Background(), session.ID, dest)
	} else {
		sm.saveToDisk(session)
	}
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
		cloning:   repoURL != "", // prevent premature pruning before clone creates workDir
	}

	sm.mu.Lock()
	sm.sessions[session.ID] = session
	sm.mu.Unlock()

	sm.persistSession(session)
	sm.hub.BroadcastSessionList(sm.sessionList())

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(session)

	// Kick off the clone in the background now that the session exists.
	// Set cloning = true BEFORE launching the goroutine so pruneDeletedSessions
	// doesn't remove the session before the workDir is created.
	if repoURL != "" {
		session.mu.Lock()
		session.cloning = true
		session.mu.Unlock()

		// Prefer DB-stored tokens over the browser-sent one.
		// Try OAuth token first; PAT is passed as fallback for SSO-gated orgs.
		githubToken := req.GithubToken
		githubPAT := ""
		if DB != nil {
			if uid := userIDFromRequest(r); uid != "" {
				oauthToken, pat := GetUserGitHubTokens(r.Context(), uid)
				if oauthToken != "" {
					githubToken = oauthToken
				}
				githubPAT = pat
			}
		}
		go sm.cloneInBackground(session, repoURL, githubToken, githubPAT)
	}
}

func (sm *SessionManager) DeleteMessage(w http.ResponseWriter, r *http.Request, sessionID, msgID string) {
	session, ok := sm.GetSession(sessionID)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	var req struct {
		Sender string `json:"sender"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	session.mu.Lock()
	found := false
	var deletedContent, deletedWorkDir string
	for i, m := range session.Messages {
		if m.ID == msgID {
			if req.Sender != "" && m.Sender != req.Sender {
				session.mu.Unlock()
				http.Error(w, "you can only delete your own messages", http.StatusForbidden)
				return
			}
			deletedContent = m.Content
			deletedWorkDir = session.WorkDir
			session.Messages = append(session.Messages[:i], session.Messages[i+1:]...)
			found = true
			break
		}
	}
	session.mu.Unlock()

	if !found {
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}

	// If the deleted message was an upload, remove the file from disk.
	if uploadFile := extractUploadPath(deletedContent); uploadFile != "" && deletedWorkDir != "" {
		fullPath := filepath.Join(casDir(deletedWorkDir), uploadFile)
		if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
			log.Printf("failed to remove upload file %s: %v", fullPath, err)
		} else {
			log.Printf("removed upload file: %s", fullPath)
		}
	}

	if DB != nil {
		DBDeleteMessage(context.Background(), msgID)
	} else {
		sm.saveToDisk(session)
	}
	sm.hub.BroadcastToSession(sessionID, WSMessage{
		Type:      "message_deleted",
		MessageID: msgID,
	})
	sm.hub.BroadcastSessionList(sm.sessionList())
	w.WriteHeader(http.StatusNoContent)
}

// GitStatus holds the git health of a session's working directory.
type GitStatus struct {
	IsRepo          bool   `json:"isRepo"`
	Uncommitted     int    `json:"uncommitted"`
	Unpushed        int    `json:"unpushed"`
	Branch          string `json:"branch"`
}

type AdminSessionInfo struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	WorkDir      string    `json:"workDir"`
	MessageCount int       `json:"messageCount"`
	CreatedAt    time.Time `json:"createdAt"`
	Git          GitStatus `json:"git"`
}

func gitStatus(workDir string) GitStatus {
	gs := GitStatus{}
	if workDir == "" {
		return gs
	}

	// Check if it's a git repo
	out, err := exec.Command("git", "-C", workDir, "rev-parse", "--is-inside-work-tree").Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return gs
	}
	gs.IsRepo = true

	// Current branch
	if b, err := exec.Command("git", "-C", workDir, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		gs.Branch = strings.TrimSpace(string(b))
	}

	// Uncommitted changes
	if u, err := exec.Command("git", "-C", workDir, "status", "--porcelain").Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(u)), "\n") {
			if strings.TrimSpace(line) != "" {
				gs.Uncommitted++
			}
		}
	}

	// Unpushed commits (requires upstream to be set)
	if p, err := exec.Command("git", "-C", workDir, "log", "@{u}..HEAD", "--oneline").Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(p)), "\n") {
			if strings.TrimSpace(line) != "" {
				gs.Unpushed++
			}
		}
	}

	return gs
}

func (sm *SessionManager) AdminListSessions(w http.ResponseWriter, r *http.Request) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	var result []AdminSessionInfo
	for _, s := range sm.sessions {
		s.mu.RLock()
		info := AdminSessionInfo{
			ID:           s.ID,
			Name:         s.Name,
			WorkDir:      s.WorkDir,
			MessageCount: len(s.Messages),
			CreatedAt:    s.CreatedAt,
			Git:          gitStatus(s.WorkDir),
		}
		s.mu.RUnlock()
		result = append(result, info)
	}

	// Sort by creation date, oldest first
	for i := 0; i < len(result)-1; i++ {
		for j := i + 1; j < len(result); j++ {
			if result[j].CreatedAt.Before(result[i].CreatedAt) {
				result[i], result[j] = result[j], result[i]
			}
		}
	}

	json.NewEncoder(w).Encode(result)
}

func (sm *SessionManager) AdminDeleteSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.URL.Query().Get("confirm") != "true" {
		http.Error(w, "confirmation required: add ?confirm=true", http.StatusBadRequest)
		return
	}

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

	workDir := session.WorkDir

	// Remove the .cas directory (session data + uploads) but leave project files.
	casPath := casDir(workDir)
	if err := os.RemoveAll(casPath); err != nil && !os.IsNotExist(err) {
		log.Printf("failed to remove .cas dir for session %s: %v", sessionID, err)
	}

	// If the working directory is empty (no project files), remove it entirely.
	entries, _ := os.ReadDir(workDir)
	if len(entries) == 0 {
		os.Remove(workDir)
	}

	sm.hub.BroadcastSessionList(sm.sessionList())
	log.Printf("admin deleted session %s (%s)", sessionID, workDir)
	w.WriteHeader(http.StatusNoContent)
}

func (sm *SessionManager) CancelStream(w http.ResponseWriter, r *http.Request, sessionID string) {
	session, ok := sm.GetSession(sessionID)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	session.mu.Lock()
	if session.cancelStream != nil {
		session.cancelStream()
		session.cancelStream = nil
	}
	session.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (sm *SessionManager) ListSessions(w http.ResponseWriter, r *http.Request) {
	sm.pruneDeletedSessions()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sm.sessionList())
}

func (sm *SessionManager) ListSessionsWithUnread(w http.ResponseWriter, r *http.Request, unread map[string]int) {
	sm.pruneDeletedSessions()
	list := sm.sessionList()
	for i, s := range list {
		if n, ok := unread[s.ID]; ok {
			list[i].UnreadCount = n
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

const maxUploadSize = 10 << 20 // 10 MB

var allowedUploadTypes = map[string]string{
	".pdf":  "PDF",
	".png":  "Image",
	".jpg":  "Image",
	".jpeg": "Image",
	".gif":  "Image",
	".webp": "Image",
	".svg":  "Image",
	".txt":  "Text",
	".md":   "Markdown",
	".csv":  "CSV",
	".json": "JSON",
	".doc":  "Word Document",
	".docx": "Word Document",
	".xls":  "Spreadsheet",
	".xlsx": "Spreadsheet",
	".pptx": "Presentation",
}

func (sm *SessionManager) UploadFile(w http.ResponseWriter, r *http.Request, sessionID string) {
	session, ok := sm.GetSession(sessionID)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize+1024)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		http.Error(w, "file too large — maximum size is 10 MB", http.StatusRequestEntityTooLarge)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(header.Filename))
	fileType, allowed := allowedUploadTypes[ext]
	if !allowed {
		http.Error(w, fmt.Sprintf("file type %q not allowed", ext), http.StatusBadRequest)
		return
	}

	// Store in session .cas/uploads/
	uploadDir := filepath.Join(casDir(session.WorkDir), "uploads")
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		http.Error(w, "could not create uploads directory", http.StatusInternalServerError)
		return
	}

	// Sanitise filename and avoid collisions
	safeName := filepath.Base(header.Filename)
	dest := filepath.Join(uploadDir, safeName)
	if _, err := os.Stat(dest); err == nil {
		// File already exists — prefix with timestamp
		safeName = fmt.Sprintf("%d_%s", time.Now().Unix(), safeName)
		dest = filepath.Join(uploadDir, safeName)
	}

	out, err := os.Create(dest)
	if err != nil {
		http.Error(w, "could not save file", http.StatusInternalServerError)
		return
	}
	defer out.Close()
	written, err := io.Copy(out, file)
	if err != nil {
		http.Error(w, "could not write file", http.StatusInternalServerError)
		return
	}

	sender := r.FormValue("sender")
	uploadAnthropicKey := r.FormValue("anthropicKey")
	if sender == "" {
		sender = "Anonymous"
	}

	// Broadcast as a user message so all teammates see it.
	content := fmt.Sprintf("📎 **%s** uploaded **%s** (%s, %s)\n`.cas/uploads/%s`",
		sender, header.Filename, fileType, formatBytes(written), safeName)
	msg := Message{
		ID:        uuid.New().String(),
		Role:      "user",
		Content:   content,
		Sender:    sender,
		Timestamp: time.Now(),
	}
	session.AddMessage(msg)
	sm.hub.BroadcastToSession(sessionID, WSMessage{Type: "user_message", Message: &msg})
	sm.persistSession(session)
	sm.hub.BroadcastSessionList(sm.sessionList())

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"filename": safeName,
		"path":     ".cas/uploads/" + safeName,
		"type":     fileType,
	})

	// Auto-prompt the agent to read and summarise the file.
	go func() {
		prompt := fmt.Sprintf("[System]: %s uploaded a file: `.cas/uploads/%s` (%s, %s). Please read it and provide a brief summary so the team can ask questions about it.",
			sender, safeName, fileType, formatBytes(written))
		agentMsg := Message{
			ID:        uuid.New().String(),
			Role:      "user",
			Content:   prompt,
			Sender:    "system",
			Timestamp: time.Now(),
		}
		session.AddMessage(agentMsg)
		sm.persistSession(session)
		sm.streamResponse(sessionID, session, uploadAnthropicKey, "", nil)
	}()
}

// ServeFile serves an uploaded file from the session's uploads directory.
func (sm *SessionManager) ServeFile(w http.ResponseWriter, r *http.Request, sessionID, filename string) {
	session, ok := sm.GetSession(sessionID)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	// Prevent path traversal
	clean := filepath.Base(filename)
	path := filepath.Join(casDir(session.WorkDir), "uploads", clean)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, r, path)
}

// extractUploadPath parses the upload-relative path from an upload message.
// e.g. "📎 **x** uploaded **y** (PDF, 1 MB)\n`.cas/uploads/foo.pdf`" → "uploads/foo.pdf"
func extractUploadPath(content string) string {
	const prefix = "📎 "
	if !strings.HasPrefix(content, prefix) {
		return ""
	}
	// Find the backtick-enclosed path
	start := strings.Index(content, "`.cas/")
	if start == -1 {
		return ""
	}
	start += len("`")
	end := strings.Index(content[start:], "`")
	if end == -1 {
		return ""
	}
	full := content[start : start+end] // e.g. ".cas/uploads/foo.pdf"
	// Return the part after ".cas/" so it can be joined with casDir()
	return strings.TrimPrefix(full, ".cas/")
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// compactSession summarises the session history using Claude and replaces
// old messages with the summary + the last 5 messages for continuity.
func (sm *SessionManager) compactSession(sessionID string, session *Session, anthropicKey, modelID string, userEnv []string) {
	broadcast := func(text string) {
		msg := Message{
			ID: uuid.New().String(), Role: "assistant", Content: text,
			Sender: "CAS", Timestamp: time.Now(),
		}
		session.AddMessage(msg)
		sm.hub.BroadcastToSession(sessionID, WSMessage{Type: "user_message", Message: &msg})
		sm.persistMessage(session, msg)
		sm.hub.BroadcastSessionList(sm.sessionList())
	}

	msgs := session.GetMessages()
	if len(msgs) < 10 {
		broadcast("ℹ️ Session is short enough — no need to compact yet.")
		return
	}

	broadcast("⏳ Compacting session history…")

	if anthropicKey == "" && DB != nil {
		if uid := ""; uid != "" {
			anthropicKey, _ = GetUserAnthropicKey(context.Background(), uid)
		}
	}
	if anthropicKey == "" {
		broadcast("❌ No Anthropic API key available to compact this session.")
		return
	}

	model := sm.model
	if modelID != "" && allowedModels[modelID] {
		model = anthropic.Model(modelID)
	}

	// Build a transcript of the conversation to summarise.
	var transcript strings.Builder
	transcript.WriteString("Please produce a comprehensive structured summary of the following conversation. Include:\n")
	transcript.WriteString("- What was being worked on (project, problem, goal)\n")
	transcript.WriteString("- Key decisions made and why\n")
	transcript.WriteString("- Files created or modified\n")
	transcript.WriteString("- Commands run and their outcomes\n")
	transcript.WriteString("- Current state of the work\n")
	transcript.WriteString("- What was left to do or in progress\n\n")
	transcript.WriteString("CONVERSATION:\n\n")
	for _, m := range msgs {
		prefix := "[Agent]"
		if m.Role == "user" {
			prefix = fmt.Sprintf("[%s]", m.Sender)
		}
		transcript.WriteString(fmt.Sprintf("%s: %s\n\n", prefix, m.Content))
	}

	client := anthropic.NewClient(option.WithAPIKey(anthropicKey))
	resp, err := client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     model,
		MaxTokens: 4096,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(transcript.String())),
		},
	})
	if err != nil {
		broadcast(fmt.Sprintf("❌ Compaction failed: %s", err))
		return
	}

	summary := ""
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			summary = t.Text
		}
	}
	if summary == "" {
		broadcast("❌ Claude returned an empty summary.")
		return
	}

	// Keep the last 5 messages for continuity.
	keepFrom := len(msgs) - 5
	if keepFrom < 0 {
		keepFrom = 0
	}
	retained := msgs[keepFrom:]

	// Replace all messages with [summary] + retained.
	summaryMsg := Message{
		ID:        uuid.New().String(),
		Role:      "assistant",
		Content:   "## 📋 Session Summary\n\n" + summary,
		Sender:    "CAS",
		Timestamp: msgs[0].Timestamp, // preserve original start time
	}

	session.mu.Lock()
	session.Messages = append([]Message{summaryMsg}, retained...)
	session.mu.Unlock()

	// Persist to DB — delete old messages and insert the new set.
	if DB != nil {
		ctx := context.Background()
		DB.Exec(ctx, `DELETE FROM messages WHERE session_id = $1`, sessionID)
		for _, m := range session.GetMessages() {
			DBAddMessage(ctx, sessionID, m.ID, m.Role, m.Content, m.Sender, m.SenderColor, m.Timestamp)
		}
	} else {
		sm.saveToDisk(session)
	}

	sm.hub.BroadcastToSession(sessionID, WSMessage{
		Type:    "history",
		History: session.GetMessages(),
	})
	sm.hub.BroadcastSessionList(sm.sessionList())
	sm.hub.BroadcastToSession(sessionID, WSMessage{
		Type: "system",
		Text: fmt.Sprintf("Session compacted — %d messages replaced with a summary + last 5", len(msgs)),
	})
}

// userIDFromRequest extracts the cas-user-id cookie value.
func userIDFromRequest(r *http.Request) string {
	if c, err := r.Cookie("cas-user-id"); err == nil {
		return c.Value
	}
	return ""
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
		Content      string `json:"content"`
		Sender       string `json:"sender"`
		AnthropicKey string `json:"anthropicKey"`
		Model        string `json:"model"`
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
		sm.persistSession(session)
		sm.hub.BroadcastSessionList(sm.sessionList())
		sm.hub.BroadcastToSession(sessionID, WSMessage{
			Type:  "work_dir_changed",
			Delta: abs,
		})
		w.WriteHeader(http.StatusAccepted)
		return
	}

	senderColor := ""
	if DB != nil {
		if uid := userIDFromRequest(r); uid != "" {
			senderColor = GetUserColor(r.Context(), uid)
		}
	}

	userMsg := Message{
		ID:          uuid.New().String(),
		Role:        "user",
		Content:     req.Content,
		Sender:      req.Sender,
		SenderColor: senderColor,
		Timestamp:   time.Now(),
	}
	session.AddMessage(userMsg)
	sm.persistMessage(session, userMsg)

	sm.hub.BroadcastToSession(sessionID, WSMessage{
		Type:    "user_message",
		Message: &userMsg,
	})
	sm.hub.BroadcastSessionList(sm.sessionList())

	w.WriteHeader(http.StatusAccepted)

	// Messages starting with @ are teammate callouts — skip the agent.
	if !strings.HasPrefix(strings.TrimSpace(req.Content), "@") {
		anthropicKey := strings.TrimSpace(req.AnthropicKey)
		model := strings.TrimSpace(req.Model)

		// If DB is available, prefer the server-stored key over the browser-sent one.
		if DB != nil {
			if userID := userIDFromRequest(r); userID != "" {
				if dbKey, err := GetUserAnthropicKey(r.Context(), userID); err == nil && dbKey != "" {
					anthropicKey = dbKey
				}
				if row := DB.QueryRow(r.Context(), `SELECT model FROM users WHERE id = $1`, userID); row != nil {
					var dbModel string
					if err := row.Scan(&dbModel); err == nil && dbModel != "" {
						model = dbModel
					}
				}
			}
		}
		userEnv := []string{}
		if DB != nil {
			if uid := userIDFromRequest(r); uid != "" {
				userEnv = UserShellEnv(r.Context(), uid)
			}
		}

		// Handle /compact before triggering a regular stream.
		if strings.TrimSpace(req.Content) == "/compact" {
			go sm.compactSession(sessionID, session, anthropicKey, model, userEnv)
			return
		}

		go sm.streamResponse(sessionID, session, anthropicKey, model, userEnv)
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

	makeTool("bash", "Run an allowed shell command in the project directory. Allowed: go run/build/test/vet/fmt, git (all common ops), gh pr/issue/repo/run/release/auth status, koda init/update/plugin/tool/backend, curl (for REST APIs — Jira, Confluence, GitHub API etc.).",
		map[string]interface{}{
			"command": map[string]interface{}{"type": "string", "description": "The shell command to run."},
		}, []string{"command"}),
}

// ---------------------------------------------------------------------------
// Tool execution
// ---------------------------------------------------------------------------

var allowedCommands = []string{
	// Go
	"go run", "go build", "go test", "go vet", "go fmt",
	// Git
	"git status", "git diff", "git add", "git commit", "git log", "git push", "git pull",
	"git fetch", "git checkout", "git branch", "git merge", "git rebase", "git stash",
	"git remote", "git show", "git reset",
	// GitHub CLI
	"gh pr", "gh issue", "gh repo", "gh run", "gh release", "gh auth status",
	// Koda marketplace CLI
	"koda init", "koda update", "koda plugin", "koda tool", "koda backend",
	// HTTP — for Jira, Confluence, and other REST APIs
	"curl",
	// Debugging
	"echo", "printenv", "env", "which", "cat", "ls", "pwd",
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

func (sm *SessionManager) executeTool(name string, rawInput json.RawMessage, workDir string, userEnv []string) (string, bool) {
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
		c.Env = append(os.Environ(), userEnv...)
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

func defaultModel() anthropic.Model {
	if m := os.Getenv("CAS_MODEL"); m != "" && allowedModels[m] {
		log.Printf("CAS default model: %s", m)
		return anthropic.Model(m)
	}
	return anthropic.ModelClaudeSonnet4_6
}

// buildSystemPrompt constructs the system prompt for the agent.
// It injects CLAUDE.md (project standards) and any SKILL.md files found in
// plugins/ or .claude/skills/ directories, following the Koda convention.
func buildSystemPrompt(workDir string, userEnv []string) []anthropic.TextBlockParam {
	// Build credential hints based on which env vars are actually set.
	credHints := ""
	for _, e := range userEnv {
		switch {
		case strings.HasPrefix(e, "GH_TOKEN="):
			credHints += "\n- GitHub: $GH_TOKEN is set — use it with `gh` CLI or as an Authorization header."
		case strings.HasPrefix(e, "ATLASSIAN_BEARER_TOKEN="):
			credHints += "\n- Atlassian (OAuth): $ATLASSIAN_BEARER_TOKEN is set. Use Bearer auth:\n  curl -s -H \"Authorization: Bearer $ATLASSIAN_BEARER_TOKEN\" \"https://api.atlassian.com/ex/jira/<cloud-id>/rest/api/3/issue/TICKET\"\n  Or use $ATLASSIAN_DOMAIN if known: curl -s -H \"Authorization: Bearer $ATLASSIAN_BEARER_TOKEN\" \"https://$ATLASSIAN_DOMAIN/rest/api/3/issue/TICKET\""
		case strings.HasPrefix(e, "ATLASSIAN_API_TOKEN="):
			credHints += "\n- Atlassian: $ATLASSIAN_API_TOKEN, $ATLASSIAN_USER_EMAIL, and $ATLASSIAN_DOMAIN are set. Use basic auth:\n  curl -s -u \"$ATLASSIAN_USER_EMAIL:$ATLASSIAN_API_TOKEN\" \"https://$ATLASSIAN_DOMAIN/rest/api/3/issue/TICKET-123\""
		}
	}

	credSection := ""
	if credHints != "" {
		credSection = "\n\nThe user's credentials are already available as environment variables in your shell — never ask the user to paste tokens or secrets:" + credHints
	}

	base := fmt.Sprintf(
		"You are a collaborative coding assistant with access to the project at %s. "+
			"Multiple team members share this session — each user message is prefixed with [Name]:. "+
			"You can read and edit files, run allowed shell commands, and use git."+
			"%s"+
			"\nWhen making changes, explain what you're doing. Be concise and collaborative.",
		workDir, credSection,
	)

	blocks := []anthropic.TextBlockParam{{Text: base}}

	// Inject CLAUDE.md if present — provides project-specific standards (Koda convention).
	if claudeMD, err := os.ReadFile(filepath.Join(workDir, "CLAUDE.md")); err == nil && len(claudeMD) > 0 {
		log.Printf("system prompt: injecting CLAUDE.md from %s", workDir)
		blocks = append(blocks, anthropic.TextBlockParam{
			Text: "## Project Standards (CLAUDE.md)\n\n" + string(claudeMD),
		})
	}

	// Inject SKILL.md files from plugins/ and .claude/skills/ directories.
	skillDirs := []string{
		filepath.Join(workDir, "plugins"),
		filepath.Join(workDir, ".claude", "skills"),
	}
	for _, dir := range skillDirs {
		skills, _ := filepath.Glob(filepath.Join(dir, "*", "SKILL.md"))
		skills2, _ := filepath.Glob(filepath.Join(dir, "*", "*", "SKILL.md"))
		skills = append(skills, skills2...)
		for _, skillPath := range skills {
			content, err := os.ReadFile(skillPath)
			if err != nil || len(content) == 0 {
				continue
			}
			relPath, _ := filepath.Rel(workDir, skillPath)
			log.Printf("system prompt: injecting skill %s", relPath)
			blocks = append(blocks, anthropic.TextBlockParam{
				Text: fmt.Sprintf("## Skill: %s\n\n%s", relPath, string(content)),
			})
		}
	}

	return blocks
}

var allowedModels = map[string]bool{
	"claude-opus-4-8":    true,
	"claude-opus-4-6":    true,
	"claude-sonnet-4-6":  true,
	"claude-haiku-4-5":   true,
}

func (sm *SessionManager) streamResponse(sessionID string, session *Session, anthropicKey string, modelID string, userEnv []string) {
	ctx, cancel := context.WithCancel(context.Background())

	session.mu.Lock()
	session.streaming = true
	session.cancelStream = cancel
	session.mu.Unlock()

	defer func() {
		cancel()
		session.mu.Lock()
		session.streaming = false
		session.cancelStream = nil
		session.mu.Unlock()
	}()

	// Require a user-provided Anthropic API key.
	if anthropicKey == "" {
		sm.hub.BroadcastToSession(sessionID, WSMessage{
			Type:  "error",
			Error: "No Anthropic API key set. Please enter your key (sk-ant-…) in the account area at the bottom of the sidebar.",
		})
		return
	}
	client := anthropic.NewClient(option.WithAPIKey(anthropicKey))

	// Use the user's chosen model if valid, otherwise fall back to default.
	model := sm.model
	if modelID != "" && allowedModels[modelID] {
		model = anthropic.Model(modelID)
	}

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

		systemBlocks := buildSystemPrompt(workDir, userEnv)

		stream := client.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
			Model:     model,
			MaxTokens: 8096,
			Tools:     casTools,
			System:    systemBlocks,
			Messages:  apiMessages,
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
			if ctx.Err() != nil {
				// Cancelled by user — send stream_end cleanly and stop.
				log.Printf("stream cancelled for session %s", sessionID)
				sm.hub.BroadcastToSession(sessionID, WSMessage{
					Type:      "stream_end",
					MessageID: msgID,
				})
				sm.hub.BroadcastToSession(sessionID, WSMessage{
					Type: "system",
					Text: "Agent stopped",
				})
				return
			}
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
			sm.persistMessage(session, assistantMsg)
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
			output, isErr := sm.executeTool(tc.name, tc.input, workDir, userEnv)

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
