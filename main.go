package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

)

//go:embed static
var staticFiles embed.FS

// Verbose enables detailed request/event logging when --verbose is passed.
var Verbose bool

// logVerbose logs only when --verbose is active.
func logVerbose(format string, args ...any) {
	if Verbose {
		log.Printf("[verbose] "+format, args...)
	}
}

// verboseHandler wraps a mux to log all HTTP requests, excluding high-frequency
// polling paths that would flood the log buffer and obscure useful entries.
func verboseHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if Verbose && r.Method != http.MethodGet {
			log.Printf("[http] %s %s %s", r.Method, r.URL.Path, r.RemoteAddr)
		}
		h.ServeHTTP(w, r)
	})
}

func main() {
	portFlag    := flag.String("port", "8080", "Port to listen on")
	verboseFlag := flag.Bool("verbose", false, "Stream detailed server activity to stdout")
	flag.Parse()

	Verbose = *verboseFlag
	if Verbose {
		log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
		log.Printf("verbose logging enabled")
	}

	// Load ~/.cas.env for config (CAS_PROJECTS_DIR, DATABASE_URL, CAS_SECRET etc.)
	if path := loadDotEnv(); path != "" {
		log.Printf("loaded config from %s", path)
	}

	// Connect to Postgres if DATABASE_URL is set.
	ctx := context.Background()
	if err := ConnectDB(ctx); err != nil {
		log.Fatalf("database connection failed: %v", err)
	}

	// Connect to Redis if REDIS_URL is set.
	if err := ConnectRedis(ctx); err != nil {
		log.Fatalf("redis connection failed: %v", err)
	}

	hub := NewHub()
	go hub.Run()
	hub.StartRedisSubscriber(ctx)

	sm := NewSessionManager(hub)

	mux := http.NewServeMux()

	staticFS, _ := fs.Sub(staticFiles, "static")
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		http.FileServer(http.FS(staticFS)).ServeHTTP(w, r)
	}))

	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		oauth := oauthConfigured()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model":           string(sm.Model()),
			"projectsDir":     sm.ProjectsDir(),
			"githubOAuth":     oauth["github"],
			"atlassianOAuth":  oauth["atlassian"],
			"showTokenFields": os.Getenv("SHOW_TOKEN_FIELDS") == "true",
		})
	})

	// GitHub OAuth
	mux.HandleFunc("/auth/github", func(w http.ResponseWriter, r *http.Request) {
		HandleGitHubLogin(w, r)
	})
	mux.HandleFunc("/auth/github/callback", func(w http.ResponseWriter, r *http.Request) {
		HandleGitHubCallback(w, r)
	})

	// Atlassian OAuth
	mux.HandleFunc("/auth/atlassian", func(w http.ResponseWriter, r *http.Request) {
		HandleAtlassianLogin(w, r)
	})
	mux.HandleFunc("/auth/atlassian/callback", func(w http.ResponseWriter, r *http.Request) {
		HandleAtlassianCallback(w, r)
	})

	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if DB == nil {
			http.Error(w, "database not configured", http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		userID, err := AuthenticateUser(r.Context(), req.Username, req.Password)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		setUserCookie(w, userID)
		profile, _ := GetUser(r.Context(), userID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(profile)
	})

	mux.HandleFunc("/api/auth/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if DB == nil {
			http.Error(w, "database not configured", http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Username    string `json:"username"`
			DisplayName string `json:"displayName"`
			Password    string `json:"password"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Username == "" || req.Password == "" {
			http.Error(w, "username and password are required", http.StatusBadRequest)
			return
		}
		if len(req.Password) < 8 {
			http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
			return
		}
		// Link to existing cookie-based account if present.
		existingID := ""
		if c, err := r.Cookie("cas-user-id"); err == nil {
			existingID = c.Value
		}
		userID, err := RegisterUser(r.Context(), existingID, req.Username, req.DisplayName, req.Password)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		setUserCookie(w, userID)
		profile, _ := GetUser(r.Context(), userID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(profile)
	})

	mux.HandleFunc("/api/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name:     "cas-user-id",
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/api/profile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if DB == nil {
			http.Error(w, `{"error":"database not configured"}`, http.StatusServiceUnavailable)
			return
		}

		userID := getUserID(w, r)
		ctx := r.Context()

		switch r.Method {
		case http.MethodGet:
			profile, err := GetOrCreateUser(ctx, userID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			// Include session statuses so frontend can restore sidebar state.
			statuses, _ := GetUserSessionStatuses(ctx, userID)
			type response struct {
				*UserProfile
				SessionStatuses map[string]string `json:"sessionStatuses"`
			}
			json.NewEncoder(w).Encode(response{profile, statuses})

		case http.MethodPut:
			var req struct {
				Name           string `json:"name"`
				Model          string `json:"model"`
				AnthropicKey   string `json:"anthropicKey"`
				GithubPAT      string `json:"githubToken"` // manual PAT, stored separately from OAuth token
				AtlassianToken string `json:"atlassianToken"`
				AtlassianEmail string `json:"atlassianEmail"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			if err := UpdateUser(ctx, userID, req.Name, req.Model, req.AnthropicKey, req.GithubPAT, req.AtlassianToken, req.AtlassianEmail, ""); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			profile, _ := GetUser(ctx, userID)
			json.NewEncoder(w).Encode(profile)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/users/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if DB == nil {
			json.NewEncoder(w).Encode([]string{})
			return
		}
		q := r.URL.Query().Get("q")
		rows, err := DB.Query(r.Context(), `
			SELECT COALESCE(NULLIF(name,''), username) as display
			FROM users
			WHERE password_hash IS NOT NULL
			  AND (LOWER(name) LIKE LOWER($1) OR LOWER(username) LIKE LOWER($1))
			ORDER BY display
			LIMIT 10
		`, "%"+q+"%")
		if err != nil {
			json.NewEncoder(w).Encode([]string{})
			return
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var n *string
			rows.Scan(&n)
			if n != nil && *n != "" {
				names = append(names, *n)
			}
		}
		json.NewEncoder(w).Encode(names)
	})

	mux.HandleFunc("/api/profile/color", func(w http.ResponseWriter, r *http.Request) {
		if DB == nil || r.Method != http.MethodPut {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		userID := getUserID(w, r)
		if userID == "" {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		var req struct{ Color string `json:"color"` }
		json.NewDecoder(r.Body).Decode(&req)
		UpdateUserColor(r.Context(), userID, req.Color)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/api/profile/session", func(w http.ResponseWriter, r *http.Request) {
		if DB == nil || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		userID := getUserID(w, r)
		var req struct {
			SessionID string `json:"sessionId"`
			Status    string `json:"status"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.SessionID != "" && (req.Status == "joined" || req.Status == "left") {
			SetUserSessionStatus(r.Context(), userID, req.SessionID, req.Status)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/api/admin/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			sm.AdminListSessions(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/admin/sessions/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		sessionID := strings.TrimPrefix(r.URL.Path, "/api/admin/sessions/")
		if r.Method == http.MethodDelete {
			sm.AdminDeleteSession(w, r, sessionID)
		} else {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/sessions/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if DB == nil {
			json.NewEncoder(w).Encode([]struct{}{})
			return
		}
		q := r.URL.Query().Get("q")
		results, err := DBSearchSessions(r.Context(), q, 20)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		type result struct {
			ID           string    `json:"id"`
			Name         string    `json:"name"`
			WorkDir      string    `json:"workDir"`
			MessageCount int       `json:"messageCount"`
			CreatedAt    time.Time `json:"createdAt"`
		}
		out := make([]result, 0, len(results))
		for _, s := range results {
			out = append(out, result{
				ID: s.ID, Name: s.Name, WorkDir: s.WorkDir,
				MessageCount: s.MessageCount, CreatedAt: s.CreatedAt,
			})
		}
		json.NewEncoder(w).Encode(out)
	})

	mux.HandleFunc("/api/folders", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		entries, err := os.ReadDir(sm.ProjectsDir())
		if err != nil {
			http.Error(w, "cannot read projects directory", http.StatusInternalServerError)
			return
		}
		// Build set of work_dir values used by active sessions so we only
		// show folders that belong to a live session. Orphaned directories
		// (from deleted sessions) are excluded and removed from disk.
		activeDirs := sm.ActiveWorkDirs()
		var folders []string
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			fullPath := filepath.Join(sm.ProjectsDir(), e.Name())
			if activeDirs[fullPath] {
				folders = append(folders, e.Name())
			} else {
				// Clean up orphaned directory left over from a deleted session.
				_ = os.RemoveAll(fullPath)
			}
		}
		json.NewEncoder(w).Encode(folders)
	})

	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		switch r.Method {
		case http.MethodGet:
			// Attach per-user unread counts when DB is available.
			if DB != nil {
				userID := getUserID(w, r)
				unread, _ := GetUnreadCounts(r.Context(), userID)
				sm.ListSessionsWithUnread(w, r, unread)
			} else {
				sm.ListSessions(w, r)
			}
		case http.MethodPost:
			sm.CreateSession(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		path := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
		parts := strings.SplitN(path, "/", 2)
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}
		sessionID, sub := parts[0], parts[1]
		switch sub {
		case "ws":
			hub.ServeWS(sm, sessionID, w, r)
		case "messages":
			if r.Method == http.MethodPost {
				sm.SendMessage(w, r, sessionID)
			} else {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		case "cancel":
			if r.Method == http.MethodPost {
				sm.CancelStream(w, r, sessionID)
			} else {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		case "upload":
			if r.Method == http.MethodPost {
				sm.UploadFile(w, r, sessionID)
			} else {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		case "delete":
			if r.Method == http.MethodDelete {
				sm.DeleteSession(w, r, sessionID)
			} else {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		case "approve-pr":
			if r.Method == http.MethodPost {
				approvePR(w, r, sessionID, sm, hub)
			} else if r.Method == http.MethodGet {
				w.Header().Set("Content-Type", "application/json")
				pr, err := GetPendingPRForSession(r.Context(), sessionID)
				if err != nil || pr == nil {
					json.NewEncoder(w).Encode(nil)
					return
				}
				json.NewEncoder(w).Encode(pr)
			} else {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		default:
			if strings.HasPrefix(sub, "messages/") {
				msgID := strings.TrimPrefix(sub, "messages/")
				if r.Method == http.MethodDelete {
					sm.DeleteMessage(w, r, sessionID, msgID)
				} else {
					http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				}
				return
			}
			if strings.HasPrefix(sub, "files/") {
				sm.ServeFile(w, r, sessionID, strings.TrimPrefix(sub, "files/"))
				return
			}
			http.NotFound(w, r)
		}
	})

	// Auth middleware wraps all /api/ routes except /api/auth/*.

	authedMux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/api/auth/") {
			requireAuth(mux).ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})

	// CORS middleware — allows the CAS PWA installed from any origin to reach
	// this backend. Credentials (cookies) are included so session auth works.
	corsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		authedMux.ServeHTTP(w, r)
	})

	ip := localIP()
	log.Printf("CAS running at http://localhost:%s", *portFlag)
	log.Printf("Share with teammates: http://%s:%s", ip, *portFlag)
	var handler http.Handler = corsHandler
	if Verbose {
		handler = verboseHandler(corsHandler)
	}
	log.Fatal(http.ListenAndServe(":"+*portFlag, handler))
}

// loadDotEnv reads KEY=VALUE pairs from ~/.cas.env (then ./.env) and sets
// any missing environment variables. Returns the path it loaded from.
func loadDotEnv() string {
	candidates := []string{
		filepath.Join(os.Getenv("HOME"), ".cas.env"),
		".env",
	}
	for _, path := range candidates {
		if loadEnvFile(path) {
			return path
		}
	}
	return ""
}

func loadEnvFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	loaded := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		// Only set if not already set in the environment.
		if k != "" && os.Getenv(k) == "" {
			os.Setenv(k, v)
			loaded = true
		}
	}
	return loaded
}

// getUserID reads the cas-user-id cookie. Does NOT auto-create one.
func getUserID(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie("cas-user-id"); err == nil && c.Value != "" {
		return c.Value
	}
	return ""
}

// setUserCookie sets the cas-user-id cookie to the given user ID.
func setUserCookie(w http.ResponseWriter, userID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "cas-user-id",
		Value:    userID,
		Path:     "/",
		MaxAge:   int((365 * 24 * time.Hour).Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// requireAuth returns 401 if the request has no valid cas-user-id cookie.
// When DB is not configured, all requests are allowed through (dev mode).
func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if DB == nil {
			next.ServeHTTP(w, r)
			return
		}
		userID := getUserID(w, r)
		if userID == "" {
			http.Error(w, `{"error":"unauthenticated"}`, http.StatusUnauthorized)
			return
		}
		// Verify user still exists in DB.
		var exists bool
		DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists)
		if !exists {
			http.SetCookie(w, &http.Cookie{Name: "cas-user-id", MaxAge: -1, Path: "/"})
			http.Error(w, `{"error":"unauthenticated"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// approvePR handles POST /api/sessions/{id}/approve-pr.
// The approver's GitHub token is used to submit an approving review and merge the PR.
func approvePR(w http.ResponseWriter, r *http.Request, sessionID string, sm *SessionManager, hub *Hub) {
	w.Header().Set("Content-Type", "application/json")
	ctx := r.Context()

	approverID := getUserID(w, r)
	if approverID == "" {
		http.Error(w, `{"error":"unauthenticated"}`, http.StatusUnauthorized)
		return
	}

	pr, err := GetPendingPRForSession(ctx, sessionID)
	if err != nil || pr == nil {
		http.Error(w, `{"error":"no pending PR for this session"}`, http.StatusNotFound)
		return
	}

	if pr.RequesterUserID == approverID {
		http.Error(w, `{"error":"you cannot approve your own PR"}`, http.StatusForbidden)
		return
	}

	// Get approver's GitHub token (OAuth first, PAT as fallback).
	oauthTok, pat := GetUserGitHubTokens(ctx, approverID)
	ghToken := oauthTok
	if ghToken == "" {
		ghToken = pat
	}
	if ghToken == "" {
		approverProfile, _ := GetUser(ctx, approverID)
		name := approverID
		if approverProfile != nil && approverProfile.Name != "" {
			name = approverProfile.Name
		}
		// Post an in-session notice and reject.
		notice := fmt.Sprintf("**%s** does not have a GitHub token configured. Go to Profile → GitHub Token to add one before approving.", name)
		sm.postSystemMessage(sessionID, notice, hub)
		http.Error(w, `{"error":"no GitHub token configured"}`, http.StatusUnprocessableEntity)
		return
	}

	approverProfile, _ := GetUser(ctx, approverID)
	approverName := approverID
	if approverProfile != nil && approverProfile.Name != "" {
		approverName = approverProfile.Name
	}

	// Submit approving review via GitHub API.
	// If the approver shares the same GitHub account as the requester (e.g. demo
	// mode with one GitHub account), skip the review and go straight to merge.
	if err := githubPRReview(ghToken, pr.Repo, pr.PRNumber, "APPROVE", "Approved via CAS"); err != nil {
		if strings.Contains(err.Error(), "approve your own pull request") {
			log.Printf("[pr] self-approval detected, skipping review step and merging directly")
		} else {
			log.Printf("[pr] review error: %v", err)
			http.Error(w, fmt.Sprintf(`{"error":%q}`, fmt.Sprintf("GitHub review failed: %v", err)), http.StatusBadGateway)
			return
		}
	}

	// Merge the PR.
	if err := githubPRMerge(ghToken, pr.Repo, pr.PRNumber); err != nil {
		log.Printf("[pr] merge error: %v", err)
		http.Error(w, fmt.Sprintf(`{"error":"GitHub merge failed: %v"}`, err), http.StatusBadGateway)
		return
	}

	_ = ApprovePendingPR(ctx, pr.ID)

	// Post confirmation message to session.
	notice := fmt.Sprintf("PR [#%d](%s) approved and merged by **%s**.", pr.PRNumber, pr.PRURL, approverName)
	sm.postSystemMessage(sessionID, notice, hub)

	// Broadcast pr_approved so the banner clears on all clients.
	hub.BroadcastToSession(sessionID, WSMessage{Type: "pr_approved", Text: pr.ID})

	json.NewEncoder(w).Encode(map[string]string{"status": "merged"})
}

// githubPRReview submits a review on a GitHub PR.
func githubPRReview(token, repo string, prNumber int, event, body string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d/reviews", repo, prNumber)
	payload, _ := json.Marshal(map[string]string{"event": event, "body": body})
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// githubPRMerge merges a GitHub PR using squash strategy.
func githubPRMerge(token, repo string, prNumber int) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d/merge", repo, prNumber)
	payload, _ := json.Marshal(map[string]string{"merge_method": "squash"})
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func localIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "localhost"
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return "localhost"
}

