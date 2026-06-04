package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB is the global connection pool.
var DB *pgxpool.Pool

// ConnectDB opens a connection pool and runs schema migrations.
// It automatically creates the target database if it does not exist.
func ConnectDB(ctx context.Context) error {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Printf("DATABASE_URL not set — user profiles will use localStorage fallback")
		return nil
	}

	if err := ensureDatabase(ctx, url); err != nil {
		return fmt.Errorf("could not ensure database exists: %w", err)
	}

	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		return err
	}
	config.MaxConns = 10
	config.MinConns = 2
	config.MaxConnLifetime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return err
	}
	if err := pool.Ping(ctx); err != nil {
		return err
	}

	DB = pool
	log.Printf("connected to Postgres")
	return runMigrations(ctx)
}

// ensureDatabase connects to the postgres system database and creates
// the target database if it does not already exist.
func ensureDatabase(ctx context.Context, url string) error {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return err
	}

	targetDB := cfg.Database
	if targetDB == "" {
		targetDB = "cas"
	}

	// Build admin URL pointing at the postgres system database.
	// Replace the database name in the URL string directly.
	adminURL := strings.Replace(url, "/"+targetDB, "/postgres", 1)

	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		// Can't reach postgres admin DB — target DB may already exist, proceed.
		log.Printf("could not connect to postgres admin db (%v) — assuming database exists", err)
		return nil
	}
	defer conn.Close(ctx)

	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, targetDB,
	).Scan(&exists); err != nil {
		return err
	}

	if !exists {
		if _, err := conn.Exec(ctx, `CREATE DATABASE "`+targetDB+`"`); err != nil {
			return fmt.Errorf("failed to create database %q: %w", targetDB, err)
		}
		log.Printf("created database %q", targetDB)
	} else {
		log.Printf("database %q already exists", targetDB)
	}
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS users (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL DEFAULT '',
    model            TEXT NOT NULL DEFAULT 'claude-sonnet-4-6',
    anthropic_key    TEXT,
    github_token     TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS teams (
    id          TEXT PRIMARY KEY,
    name        TEXT UNIQUE NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS team_members (
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    team_id     TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    role        TEXT NOT NULL DEFAULT 'member',
    joined_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, team_id)
);

CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    work_dir    TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS messages (
    id          TEXT PRIMARY KEY,
    session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    role        TEXT NOT NULL,
    content     TEXT NOT NULL,
    sender      TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS messages_session_id_idx ON messages(session_id, created_at);

CREATE TABLE IF NOT EXISTS session_members (
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id  TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'joined',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, session_id)
);
`

func runMigrations(ctx context.Context) error {
	_, err := DB.Exec(ctx, schema)
	if err != nil {
		return err
	}
	log.Printf("database schema ready")
	return nil
}

// UserProfile holds a user's persisted settings.
type UserProfile struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Model           string    `json:"model"`
	AnthropicKeySet bool      `json:"anthropicKeySet"`
	AnthropicKeyHint string   `json:"anthropicKeyHint"` // last 4 chars masked
	GithubTokenSet  bool      `json:"githubTokenSet"`
	GithubTokenHint string    `json:"githubTokenHint"`
	CreatedAt       time.Time `json:"createdAt"`
	LastSeen        time.Time `json:"lastSeen"`
}

// GetOrCreateUser loads a user profile by ID, creating it if it doesn't exist.
func GetOrCreateUser(ctx context.Context, userID string) (*UserProfile, error) {
	_, err := DB.Exec(ctx, `
		INSERT INTO users (id) VALUES ($1)
		ON CONFLICT (id) DO UPDATE SET last_seen = NOW()
	`, userID)
	if err != nil {
		return nil, err
	}
	return GetUser(ctx, userID)
}

// GetUser loads a user profile including masked key hints.
func GetUser(ctx context.Context, userID string) (*UserProfile, error) {
	row := DB.QueryRow(ctx, `
		SELECT id, name, model, anthropic_key, github_token, created_at, last_seen
		FROM users WHERE id = $1
	`, userID)

	var p UserProfile
	var anthropicKey, githubToken *string
	err := row.Scan(&p.ID, &p.Name, &p.Model, &anthropicKey, &githubToken, &p.CreatedAt, &p.LastSeen)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if anthropicKey != nil && *anthropicKey != "" {
		plain, _ := Decrypt(*anthropicKey)
		p.AnthropicKeySet = plain != ""
		p.AnthropicKeyHint = maskKey(plain)
	}
	if githubToken != nil && *githubToken != "" {
		plain, _ := Decrypt(*githubToken)
		p.GithubTokenSet = plain != ""
		p.GithubTokenHint = maskKey(plain)
	}
	return &p, nil
}

// UpdateUser saves name, model, and optionally encrypted keys.
// Pass empty string for a key to leave it unchanged.
func UpdateUser(ctx context.Context, userID, name, model, anthropicKey, githubToken string) error {
	if anthropicKey != "" {
		enc, err := Encrypt(anthropicKey)
		if err != nil {
			return err
		}
		if _, err := DB.Exec(ctx, `UPDATE users SET anthropic_key = $1 WHERE id = $2`, enc, userID); err != nil {
			return err
		}
	}
	if githubToken != "" {
		enc, err := Encrypt(githubToken)
		if err != nil {
			return err
		}
		if _, err := DB.Exec(ctx, `UPDATE users SET github_token = $1 WHERE id = $2`, enc, userID); err != nil {
			return err
		}
	}
	_, err := DB.Exec(ctx, `
		UPDATE users SET name = $1, model = $2, last_seen = NOW() WHERE id = $3
	`, name, model, userID)
	return err
}

// GetUserAnthropicKey returns the decrypted Anthropic key for a user.
func GetUserAnthropicKey(ctx context.Context, userID string) (string, error) {
	var enc *string
	err := DB.QueryRow(ctx, `SELECT anthropic_key FROM users WHERE id = $1`, userID).Scan(&enc)
	if err == pgx.ErrNoRows || enc == nil {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return Decrypt(*enc)
}

// GetUserGithubToken returns the decrypted GitHub token for a user.
func GetUserGithubToken(ctx context.Context, userID string) (string, error) {
	var enc *string
	err := DB.QueryRow(ctx, `SELECT github_token FROM users WHERE id = $1`, userID).Scan(&enc)
	if err == pgx.ErrNoRows || enc == nil {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return Decrypt(*enc)
}

// GetUserSessionStatus returns all session statuses for a user.
func GetUserSessionStatuses(ctx context.Context, userID string) (map[string]string, error) {
	rows, err := DB.Query(ctx, `
		SELECT session_id, status FROM session_members WHERE user_id = $1
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]string)
	for rows.Next() {
		var sid, status string
		if err := rows.Scan(&sid, &status); err != nil {
			return nil, err
		}
		result[sid] = status
	}
	return result, nil
}

// SetUserSessionStatus upserts a session membership status for a user.
func SetUserSessionStatus(ctx context.Context, userID, sessionID, status string) error {
	_, err := DB.Exec(ctx, `
		INSERT INTO session_members (user_id, session_id, status, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (user_id, session_id) DO UPDATE
		SET status = $3, updated_at = NOW()
	`, userID, sessionID, status)
	return err
}

// ── Session DB functions ──────────────────────────────────────────────────────

func DBCreateSession(ctx context.Context, id, name, workDir string, createdAt interface{}) error {
	_, err := DB.Exec(ctx,
		`INSERT INTO sessions (id, name, work_dir, created_at) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (id) DO NOTHING`,
		id, name, workDir, createdAt)
	return err
}

func DBUpdateSessionWorkDir(ctx context.Context, id, workDir string) error {
	_, err := DB.Exec(ctx, `UPDATE sessions SET work_dir = $1 WHERE id = $2`, workDir, id)
	return err
}

func DBDeleteSession(ctx context.Context, id string) error {
	_, err := DB.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	return err
}

type DBSession struct {
	ID        string
	Name      string
	WorkDir   string
	CreatedAt time.Time
}

func DBListSessions(ctx context.Context) ([]DBSession, error) {
	rows, err := DB.Query(ctx, `SELECT id, name, work_dir, created_at FROM sessions ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []DBSession
	for rows.Next() {
		var s DBSession
		if err := rows.Scan(&s.ID, &s.Name, &s.WorkDir, &s.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, nil
}

// ── Message DB functions ──────────────────────────────────────────────────────

func DBAddMessage(ctx context.Context, sessionID, id, role, content, sender string, createdAt interface{}) error {
	_, err := DB.Exec(ctx,
		`INSERT INTO messages (id, session_id, role, content, sender, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (id) DO NOTHING`,
		id, sessionID, role, content, sender, createdAt)
	return err
}

func DBGetMessages(ctx context.Context, sessionID string) ([]struct {
	ID        string
	Role      string
	Content   string
	Sender    string
	CreatedAt time.Time
}, error) {
	rows, err := DB.Query(ctx,
		`SELECT id, role, content, sender, created_at FROM messages
		 WHERE session_id = $1 ORDER BY created_at ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []struct {
		ID        string
		Role      string
		Content   string
		Sender    string
		CreatedAt time.Time
	}
	for rows.Next() {
		var m struct {
			ID        string
			Role      string
			Content   string
			Sender    string
			CreatedAt time.Time
		}
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &m.Sender, &m.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, nil
}

func DBDeleteMessage(ctx context.Context, id string) error {
	_, err := DB.Exec(ctx, `DELETE FROM messages WHERE id = $1`, id)
	return err
}

func DBGetMessageCount(ctx context.Context, sessionID string) (int, error) {
	var count int
	err := DB.QueryRow(ctx, `SELECT COUNT(*) FROM messages WHERE session_id = $1`, sessionID).Scan(&count)
	return count, err
}
