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

	"github.com/google/uuid"
)

//go:embed static
var staticFiles embed.FS

func main() {
	portFlag := flag.String("port", "8080", "Port to listen on")
	flag.Parse()

	// Load ~/.cas.env for config (CAS_PROJECTS_DIR, DATABASE_URL, CAS_SECRET etc.)
	if path := loadDotEnv(); path != "" {
		log.Printf("loaded config from %s", path)
	}

	// Connect to Postgres if DATABASE_URL is set.
	ctx := context.Background()
	if err := ConnectDB(ctx); err != nil {
		log.Fatalf("database connection failed: %v", err)
	}

	hub := NewHub()
	go hub.Run()

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
			sm.ListSessions(w, r)
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

	ip := localIP()
	log.Printf("CAS running at http://localhost:%s", *portFlag)
	log.Printf("Share with teammates: http://%s:%s", ip, *portFlag)
	log.Fatal(http.ListenAndServe(":"+*portFlag, mux))
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

// getUserID reads or creates the cas-user-id cookie for persistent identity.
func getUserID(w http.ResponseWriter, r *http.Request) string {
	if cookie, err := r.Cookie("cas-user-id"); err == nil && cookie.Value != "" {
		return cookie.Value
	}
	id := uuid.New().String()
	http.SetCookie(w, &http.Cookie{
		Name:     "cas-user-id",
		Value:    id,
		Path:     "/",
		MaxAge:   int((365 * 24 * time.Hour).Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	return id
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

