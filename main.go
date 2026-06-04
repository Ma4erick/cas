package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"flag"
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

// verboseHandler wraps a mux to log all HTTP requests.
func verboseHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if Verbose {
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
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"model":       string(sm.Model()),
			"projectsDir": sm.ProjectsDir(),
		})
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
				Name         string `json:"name"`
				Model        string `json:"model"`
				AnthropicKey string `json:"anthropicKey"`
				GithubToken  string `json:"githubToken"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			if err := UpdateUser(ctx, userID, req.Name, req.Model, req.AnthropicKey, req.GithubToken); err != nil {
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
		var folders []string
		for _, e := range entries {
			if e.IsDir() {
				folders = append(folders, e.Name())
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

	ip := localIP()
	log.Printf("CAS running at http://localhost:%s", *portFlag)
	log.Printf("Share with teammates: http://%s:%s", ip, *portFlag)
	var handler http.Handler = authedMux
	if Verbose {
		handler = verboseHandler(authedMux)
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

