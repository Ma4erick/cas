package main

import (
	"bufio"
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
)

//go:embed static
var staticFiles embed.FS

func main() {
	portFlag := flag.String("port", "8080", "Port to listen on")
	flag.Parse()

	// Load ~/.cas.env for non-Anthropic config (CAS_PROJECTS_DIR, GITHUB_TOKEN etc.)
	if path := loadDotEnv(); path != "" {
		log.Printf("loaded config from %s", path)
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

