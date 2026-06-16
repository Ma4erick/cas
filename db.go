package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
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
    username         TEXT UNIQUE,
    password_hash    TEXT,
    name             TEXT NOT NULL DEFAULT '',
    model            TEXT NOT NULL DEFAULT 'claude-sonnet-4-6',
    anthropic_key    TEXT,
    github_token     TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS users_username_idx ON users (LOWER(username)) WHERE username IS NOT NULL;

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
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    status       TEXT NOT NULL DEFAULT 'joined',
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_read_at TIMESTAMPTZ,
    PRIMARY KEY (user_id, session_id)
);
`

const alterations = `
ALTER TABLE users ADD COLUMN IF NOT EXISTS username TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS password_hash TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS color TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS github_login TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS github_pat TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS atlassian_token TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS atlassian_refresh_token TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS atlassian_email TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS atlassian_domain TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN IF NOT EXISTS sender_color TEXT NOT NULL DEFAULT '';
ALTER TABLE session_members ADD COLUMN IF NOT EXISTS last_read_at TIMESTAMPTZ;
CREATE UNIQUE INDEX IF NOT EXISTS users_username_idx ON users (LOWER(username)) WHERE username IS NOT NULL;
UPDATE session_members SET last_read_at = updated_at WHERE last_read_at IS NULL;
DELETE FROM session_members WHERE session_id NOT IN (SELECT id FROM sessions);
DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'session_members_session_id_fkey'
  ) THEN
    ALTER TABLE session_members
      ADD CONSTRAINT session_members_session_id_fkey
      FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE;
  END IF;
END $$;
`

func runMigrations(ctx context.Context) error {
	if _, err := DB.Exec(ctx, schema); err != nil {
		return err
	}
	if _, err := DB.Exec(ctx, alterations); err != nil {
		return err
	}
	log.Printf("database schema ready")
	return seedDefaultUser(ctx)
}

// seedDefaultUser creates a default CAS/CAS user if no registered users exist.
// This ensures there is always a way to log in on a fresh install.
func seedDefaultUser(ctx context.Context) error {
	var count int
	DB.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE password_hash IS NOT NULL`).Scan(&count)
	if count > 0 {
		return nil
	}
	_, err := RegisterUser(ctx, "", "CAS", "CAS", "CAS")
	if err != nil {
		return fmt.Errorf("failed to create default user: %w", err)
	}
	log.Printf("created default user: username=CAS password=CAS — change this after first login")
	return nil
}

// UserProfile holds a user's persisted settings.
type UserProfile struct {
	ID                   string    `json:"id"`
	Name                 string    `json:"name"`
	Model                string    `json:"model"`
	Color                string    `json:"color"`
	AnthropicKeySet      bool      `json:"anthropicKeySet"`
	AnthropicKeyHint     string    `json:"anthropicKeyHint"`
	GithubTokenSet       bool      `json:"githubTokenSet"`
	GithubTokenHint      string    `json:"githubTokenHint"`
	GithubLogin          string    `json:"githubLogin"`
	GithubPATSet         bool      `json:"githubPATSet"`
	GithubPATHint        string    `json:"githubPATHint"`
	AtlassianTokenSet      bool      `json:"atlassianTokenSet"`
	AtlassianTokenHint     string    `json:"atlassianTokenHint"`
	AtlassianEmail         string    `json:"atlassianEmail"`
	AtlassianDomain        string    `json:"atlassianDomain"`
	AtlassianOAuthConnected bool     `json:"atlassianOAuthConnected"`
	CreatedAt            time.Time `json:"createdAt"`
	LastSeen             time.Time `json:"lastSeen"`
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
		SELECT id, name, model, color, anthropic_key, github_token, github_login, github_pat,
		       atlassian_token, atlassian_email, atlassian_domain, created_at, last_seen
		FROM users WHERE id = $1
	`, userID)

	var p UserProfile
	var anthropicKey, githubToken, githubPAT, atlassianToken *string
	err := row.Scan(&p.ID, &p.Name, &p.Model, &p.Color, &anthropicKey, &githubToken, &p.GithubLogin, &githubPAT,
		&atlassianToken, &p.AtlassianEmail, &p.AtlassianDomain, &p.CreatedAt, &p.LastSeen)
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
	if githubPAT != nil && *githubPAT != "" {
		plain, _ := Decrypt(*githubPAT)
		p.GithubPATSet = plain != ""
		p.GithubPATHint = maskKey(plain)
	}
	if atlassianToken != nil && *atlassianToken != "" {
		plain, _ := Decrypt(*atlassianToken)
		p.AtlassianTokenSet = plain != ""
		p.AtlassianTokenHint = maskKey(plain)
	}

	// AtlassianOAuthConnected is true only when connected via OAuth (has refresh token).
	var atlRefresh *string
	DB.QueryRow(ctx, `SELECT atlassian_refresh_token FROM users WHERE id = $1`, userID).Scan(&atlRefresh)
	p.AtlassianOAuthConnected = atlRefresh != nil && *atlRefresh != ""

	return &p, nil
}

// UpdateUser saves name, model, and optionally encrypted keys.
// Pass empty string for a key to leave it unchanged.
func UpdateUser(ctx context.Context, userID, name, model, anthropicKey, githubPAT, atlassianToken, atlassianEmail, atlassianDomain string) error {
	// Save manual PAT separately — never overwrites the OAuth token.
	if githubPAT != "" {
		enc, err := Encrypt(githubPAT)
		if err != nil {
			return err
		}
		if _, err := DB.Exec(ctx, `UPDATE users SET github_pat = $1 WHERE id = $2`, enc, userID); err != nil {
			return err
		}
	}
	if atlassianToken != "" {
		enc, err := Encrypt(atlassianToken)
		if err != nil {
			return err
		}
		if _, err := DB.Exec(ctx, `UPDATE users SET atlassian_token = $1 WHERE id = $2`, enc, userID); err != nil {
			return err
		}
	}
	if atlassianEmail != "" {
		if _, err := DB.Exec(ctx, `UPDATE users SET atlassian_email = $1 WHERE id = $2`, atlassianEmail, userID); err != nil {
			return err
		}
	}
	if atlassianDomain != "" {
		if _, err := DB.Exec(ctx, `UPDATE users SET atlassian_domain = $1 WHERE id = $2`, atlassianDomain, userID); err != nil {
			return err
		}
	}
	if anthropicKey != "" {
		enc, err := Encrypt(anthropicKey)
		if err != nil {
			return err
		}
		if _, err := DB.Exec(ctx, `UPDATE users SET anthropic_key = $1 WHERE id = $2`, enc, userID); err != nil {
			return err
		}
	}
	_, err := DB.Exec(ctx, `
		UPDATE users SET name = $1, model = $2, last_seen = NOW() WHERE id = $3
	`, name, model, userID)
	return err
}

// UserShellEnv returns environment variables to inject into shell commands for a user.
func UserShellEnv(ctx context.Context, userID string) []string {
	var env []string
	var ghEnc, atlEnc *string
	var atlEmail string
	DB.QueryRow(ctx, `SELECT github_token, atlassian_token, atlassian_email FROM users WHERE id = $1`, userID).
		Scan(&ghEnc, &atlEnc, &atlEmail)

	if ghEnc != nil {
		if plain, err := Decrypt(*ghEnc); err == nil && plain != "" {
			env = append(env, "GH_TOKEN="+plain)
			env = append(env, "GITHUB_TOKEN="+plain)
		}
	}
	if atlEnc != nil {
		if plain, err := Decrypt(*atlEnc); err == nil && plain != "" {
			env = append(env, "ATLASSIAN_API_TOKEN="+plain)
			// If this is an OAuth bearer token (refresh token present), also inject as bearer.
			var refresh *string
			DB.QueryRow(ctx, `SELECT atlassian_refresh_token FROM users WHERE id = $1`, userID).Scan(&refresh)
			if refresh != nil && *refresh != "" {
				env = append(env, "ATLASSIAN_BEARER_TOKEN="+plain)
			}
		}
	}
	if atlEmail != "" {
		env = append(env, "ATLASSIAN_USER_EMAIL="+atlEmail)
	}
	// ATLASSIAN_DOMAIN is org-wide — read from server environment.
	if domain := os.Getenv("ATLASSIAN_DOMAIN"); domain != "" {
		env = append(env, "ATLASSIAN_DOMAIN="+domain)
	}
	return env
}

// StoreGitHubOAuthToken saves an OAuth token and login as the user's GitHub credentials.
func StoreGitHubOAuthToken(ctx context.Context, userID, token, login string) error {
	enc, err := Encrypt(token)
	if err != nil {
		return err
	}
	_, err = DB.Exec(ctx, `UPDATE users SET github_token = $1, github_login = $2 WHERE id = $3`, enc, login, userID)
	return err
}

// StoreAtlassianOAuthTokens saves OAuth access + refresh tokens for a user.
func StoreAtlassianOAuthTokens(ctx context.Context, userID, accessToken, refreshToken, email, domain string) error {
	encAccess, err := Encrypt(accessToken)
	if err != nil {
		return err
	}
	encRefresh, err := Encrypt(refreshToken)
	if err != nil {
		return err
	}
	_, err = DB.Exec(ctx, `
		UPDATE users SET atlassian_token = $1, atlassian_refresh_token = $2,
		                 atlassian_email = $3, atlassian_domain = $4
		WHERE id = $5
	`, encAccess, encRefresh, email, domain, userID)
	return err
}

// RefreshAtlassianToken exchanges a refresh token for a new access token.
func RefreshAtlassianToken(ctx context.Context, userID string) (string, error) {
	var encRefresh *string
	if err := DB.QueryRow(ctx, `SELECT atlassian_refresh_token FROM users WHERE id = $1`, userID).Scan(&encRefresh); err != nil || encRefresh == nil {
		return "", fmt.Errorf("no refresh token stored")
	}
	refreshToken, err := Decrypt(*encRefresh)
	if err != nil || refreshToken == "" {
		return "", fmt.Errorf("failed to decrypt refresh token")
	}

	// Exchange refresh token for new access token.
	tokenURL := "https://auth.atlassian.com/oauth/token"
	body := fmt.Sprintf(`{"grant_type":"refresh_token","client_id":"%s","client_secret":"%s","refresh_token":"%s"}`,
		os.Getenv("ATLASSIAN_CLIENT_ID"), os.Getenv("ATLASSIAN_CLIENT_SECRET"), refreshToken)

	resp, err := http.Post(tokenURL, "application/json", strings.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || result.AccessToken == "" {
		return "", fmt.Errorf("failed to refresh token")
	}

	// Store the new tokens.
	encNew, _ := Encrypt(result.AccessToken)
	encNewRefresh, _ := Encrypt(result.RefreshToken)
	DB.Exec(ctx, `UPDATE users SET atlassian_token = $1, atlassian_refresh_token = $2 WHERE id = $3`,
		encNew, encNewRefresh, userID)

	return result.AccessToken, nil
}

// GetUserColor returns the user's chosen display colour.
func GetUserColor(ctx context.Context, userID string) string {
	var color string
	DB.QueryRow(ctx, `SELECT color FROM users WHERE id = $1`, userID).Scan(&color)
	return color
}

// UpdateUserColor saves the user's chosen display colour.
func UpdateUserColor(ctx context.Context, userID, color string) error {
	_, err := DB.Exec(ctx, `UPDATE users SET color = $1 WHERE id = $2`, color, userID)
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

// GetUserGitHubTokens returns the OAuth token and PAT for a user separately.
// Use oauthToken first; fall back to pat if oauth fails (e.g. pending SSO approval).
func GetUserGitHubTokens(ctx context.Context, userID string) (oauthToken, pat string) {
	var encOAuth, encPAT *string
	DB.QueryRow(ctx, `SELECT github_token, github_pat FROM users WHERE id = $1`, userID).Scan(&encOAuth, &encPAT)
	if encOAuth != nil {
		oauthToken, _ = Decrypt(*encOAuth)
	}
	if encPAT != nil {
		pat, _ = Decrypt(*encPAT)
	}
	return
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
// Always initialises last_read_at on first insert; preserves it on updates.
func SetUserSessionStatus(ctx context.Context, userID, sessionID, status string) error {
	_, err := DB.Exec(ctx, `
		INSERT INTO session_members (user_id, session_id, status, last_read_at, updated_at)
		VALUES ($1, $2, $3, NOW(), NOW())
		ON CONFLICT (user_id, session_id) DO UPDATE
		SET status       = EXCLUDED.status,
		    updated_at   = NOW(),
		    last_read_at = COALESCE(session_members.last_read_at, NOW())
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
	ID           string
	Name         string
	WorkDir      string
	CreatedAt    time.Time
	MessageCount int
}

func DBListSessions(ctx context.Context) ([]DBSession, error) {
	rows, err := DB.Query(ctx, `
		SELECT s.id, s.name, s.work_dir, s.created_at,
		       COUNT(m.id) AS message_count
		FROM sessions s
		LEFT JOIN messages m ON m.session_id = s.id
		GROUP BY s.id
		ORDER BY s.created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []DBSession
	for rows.Next() {
		var s DBSession
		if err := rows.Scan(&s.ID, &s.Name, &s.WorkDir, &s.CreatedAt, &s.MessageCount); err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, nil
}

// DBSearchSessions returns sessions matching a name query, limited to recent ones.
func DBSearchSessions(ctx context.Context, query string, limit int) ([]DBSession, error) {
	rows, err := DB.Query(ctx, `
		SELECT s.id, s.name, s.work_dir, s.created_at,
		       COUNT(m.id) AS message_count
		FROM sessions s
		LEFT JOIN messages m ON m.session_id = s.id
		WHERE s.name ILIKE '%' || $1 || '%'
		GROUP BY s.id
		ORDER BY s.created_at DESC
		LIMIT $2
	`, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []DBSession
	for rows.Next() {
		var s DBSession
		if err := rows.Scan(&s.ID, &s.Name, &s.WorkDir, &s.CreatedAt, &s.MessageCount); err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, nil
}

// ── Message DB functions ──────────────────────────────────────────────────────

func DBAddMessage(ctx context.Context, sessionID, id, role, content, sender, senderColor string, createdAt interface{}) error {
	_, err := DB.Exec(ctx,
		`INSERT INTO messages (id, session_id, role, content, sender, sender_color, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (id) DO NOTHING`,
		id, sessionID, role, content, sender, senderColor, createdAt)
	return err
}

func DBGetMessages(ctx context.Context, sessionID string) ([]struct {
	ID          string
	Role        string
	Content     string
	Sender      string
	SenderColor string
	CreatedAt   time.Time
}, error) {
	rows, err := DB.Query(ctx,
		`SELECT id, role, content, sender, sender_color, created_at FROM messages
		 WHERE session_id = $1 ORDER BY created_at ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []struct {
		ID          string
		Role        string
		Content     string
		Sender      string
		SenderColor string
		CreatedAt   time.Time
	}
	for rows.Next() {
		var m struct {
			ID          string
			Role        string
			Content     string
			Sender      string
			SenderColor string
			CreatedAt   time.Time
		}
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &m.Sender, &m.SenderColor, &m.CreatedAt); err != nil {
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

// MarkSessionRead records when a user last read a session.
func MarkSessionRead(ctx context.Context, userID, sessionID string) error {
	_, err := DB.Exec(ctx, `
		INSERT INTO session_members (user_id, session_id, status, last_read_at, updated_at)
		VALUES ($1, $2, 'joined', NOW(), NOW())
		ON CONFLICT (user_id, session_id) DO UPDATE
		SET last_read_at = NOW(), updated_at = NOW()
	`, userID, sessionID)
	return err
}

// GetUnreadCounts returns unread message counts per session for a user.
// Only counts messages that arrived after the user last read the session.
// If last_read_at is NULL (no baseline yet), unread = 0.
func GetUnreadCounts(ctx context.Context, userID string) (map[string]int, error) {
	rows, err := DB.Query(ctx, `
		SELECT sm.session_id,
		       COUNT(m.id) FILTER (
		           WHERE sm.last_read_at IS NOT NULL
		             AND m.created_at > sm.last_read_at
		       ) AS unread
		FROM session_members sm
		LEFT JOIN messages m ON m.session_id = sm.session_id
		WHERE sm.user_id = $1
		GROUP BY sm.session_id
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]int)
	for rows.Next() {
		var sid string
		var count int
		if err := rows.Scan(&sid, &count); err != nil {
			return nil, err
		}
		result[sid] = count
	}
	return result, nil
}

// GetUserIDsByMentionTokens resolves @mention tokens to user IDs.
// Each token is matched (case-insensitively) against:
//   - the username (always a single word)
//   - the full display name
//   - the first word of the display name (handles "@London" → "London Summers")
func GetUserIDsByMentionTokens(ctx context.Context, tokens []string) ([]string, error) {
	if len(tokens) == 0 {
		return nil, nil
	}
	rows, err := DB.Query(ctx, `
		SELECT DISTINCT id FROM users
		WHERE password_hash IS NOT NULL
		  AND (
		    LOWER(username) = ANY(SELECT LOWER(t) FROM UNNEST($1::text[]) t)
		    OR LOWER(name)  = ANY(SELECT LOWER(t) FROM UNNEST($1::text[]) t)
		    OR LOWER(SPLIT_PART(name, ' ', 1)) = ANY(SELECT LOWER(t) FROM UNNEST($1::text[]) t)
		  )
	`, tokens)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// InviteUserToSession sets a user's membership to "invited" so the session
// appears in their sidebar. Already-joined members are left untouched; users
// who previously left are re-invited so they see the mention.
func InviteUserToSession(ctx context.Context, userID, sessionID string) error {
	_, err := DB.Exec(ctx, `
		INSERT INTO session_members (user_id, session_id, status, last_read_at, updated_at)
		VALUES ($1, $2, 'invited', NOW(), NOW())
		ON CONFLICT (user_id, session_id) DO UPDATE
		SET status = 'invited', updated_at = NOW()
		WHERE session_members.status <> 'joined'
	`, userID, sessionID)
	return err
}

// ── Auth functions ────────────────────────────────────────────────────────────

// RegisterUser sets a username and bcrypt password on an existing user (by cookie ID)
// or creates a fresh user. Returns the user ID.
func RegisterUser(ctx context.Context, existingID, username, displayName, password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}

	// Check username not already taken.
	var taken bool
	DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE LOWER(username) = LOWER($1))`, username).Scan(&taken)
	if taken {
		return "", fmt.Errorf("username %q is already taken", username)
	}

	if displayName == "" {
		displayName = username
	}

	if existingID != "" {
		// Link credentials to the existing cookie-based account.
		_, err = DB.Exec(ctx, `
			UPDATE users SET username = $1, password_hash = $2, name = $3 WHERE id = $4
		`, username, string(hash), displayName, existingID)
		return existingID, err
	}

	// Create a new user.
	id := uuid.New().String()
	_, err = DB.Exec(ctx, `
		INSERT INTO users (id, username, password_hash, name)
		VALUES ($1, $2, $3, $4)
	`, id, username, string(hash), displayName)
	return id, err
}

// AuthenticateUser verifies a username/password and returns the user ID if valid.
func AuthenticateUser(ctx context.Context, username, password string) (string, error) {
	var id, hash string
	err := DB.QueryRow(ctx, `
		SELECT id, password_hash FROM users
		WHERE LOWER(username) = LOWER($1) AND password_hash IS NOT NULL
	`, username).Scan(&id, &hash)
	if err == pgx.ErrNoRows {
		return "", fmt.Errorf("invalid username or password")
	}
	if err != nil {
		return "", err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return "", fmt.Errorf("invalid username or password")
	}
	// Update last_seen.
	DB.Exec(ctx, `UPDATE users SET last_seen = NOW() WHERE id = $1`, id)
	return id, nil
}

// UserHasPassword returns true if the user has set up a username/password.
func UserHasPassword(ctx context.Context, userID string) bool {
	var has bool
	DB.QueryRow(ctx, `SELECT password_hash IS NOT NULL FROM users WHERE id = $1`, userID).Scan(&has)
	return has
}
