package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ── Helpers ───────────────────────────────────────────────────────────────────

// generateOAuthState embeds the userID in the state so no separate cookie is needed.
// Format: base64(random16bytes):userID
func generateOAuthState(userID string) string {
	b := make([]byte, 16)
	rand.Read(b)
	nonce := base64.URLEncoding.EncodeToString(b)
	return nonce + ":" + userID
}

// extractUserIDFromState returns the userID embedded in the state parameter.
func extractUserIDFromState(state string) (string, bool) {
	parts := strings.SplitN(state, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func verifyOAuthState(w http.ResponseWriter, r *http.Request) (string, bool) {
	state := r.URL.Query().Get("state")
	userID, ok := extractUserIDFromState(state)
	if !ok {
		http.Error(w, "invalid OAuth state — please try signing in to CAS and trying again", http.StatusBadRequest)
		return "", false
	}
	return userID, true
}

// ── GitHub OAuth ──────────────────────────────────────────────────────────────

func HandleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	clientID := os.Getenv("GITHUB_CLIENT_ID")
	if clientID == "" {
		http.Error(w, "GitHub OAuth not configured — set GITHUB_CLIENT_ID in ~/.cas.env", http.StatusServiceUnavailable)
		return
	}
	userID := getUserID(w, r)
	if userID == "" {
		http.Error(w, "not authenticated with CAS — please log in first", http.StatusUnauthorized)
		return
	}
	state := generateOAuthState(userID)
	redirectURL := "https://github.com/login/oauth/authorize" +
		"?client_id=" + clientID +
		"&scope=repo,read:user" +
		"&state=" + url.QueryEscape(state)
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

func HandleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	userID, ok := verifyOAuthState(w, r)
	if !ok {
		return
	}
	if DB == nil {
		http.Error(w, "database not configured", http.StatusServiceUnavailable)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code from GitHub", http.StatusBadRequest)
		return
	}

	// Exchange code for access token.
	resp, err := http.PostForm("https://github.com/login/oauth/access_token", url.Values{
		"client_id":     {os.Getenv("GITHUB_CLIENT_ID")},
		"client_secret": {os.Getenv("GITHUB_CLIENT_SECRET")},
		"code":          {code},
	})
	if err != nil {
		http.Error(w, "failed to exchange GitHub token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	vals, _ := url.ParseQuery(string(body))
	accessToken := vals.Get("access_token")
	if accessToken == "" {
		http.Error(w, "no access token returned by GitHub", http.StatusInternalServerError)
		return
	}

	// Fetch the authenticated user's login.
	login, email := fetchGitHubUser(accessToken)
	log.Printf("GitHub OAuth: connected user %s (%s)", login, email)
	if err := StoreGitHubOAuthToken(r.Context(), userID, accessToken, login); err != nil {
		http.Error(w, "failed to store token: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Do not set atlassian_email from GitHub — keep it separate.

	log.Printf("GitHub OAuth: stored token for user %s as %s", userID, login)
	oauthSuccessPage(w, "github", map[string]string{"login": login})
}

func fetchGitHubUser(token string) (login, email string) {
	req, _ := http.NewRequest("GET", "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	var u struct {
		Login string `json:"login"`
		Email string `json:"email"`
	}
	json.NewDecoder(resp.Body).Decode(&u)
	return u.Login, u.Email
}

// ── Atlassian OAuth 2.0 (3LO) ────────────────────────────────────────────────

func HandleAtlassianLogin(w http.ResponseWriter, r *http.Request) {
	clientID := os.Getenv("ATLASSIAN_CLIENT_ID")
	if clientID == "" {
		http.Error(w, "Atlassian OAuth not configured — set ATLASSIAN_CLIENT_ID in ~/.cas.env", http.StatusServiceUnavailable)
		return
	}
	userID := getUserID(w, r)
	if userID == "" {
		http.Error(w, "not authenticated with CAS — please log in first", http.StatusUnauthorized)
		return
	}
	state := generateOAuthState(userID)

	baseURL := os.Getenv("CAS_BASE_URL")
	if baseURL == "" {
		baseURL = "http://" + r.Host
	}
	callbackURL := strings.TrimRight(baseURL, "/") + "/auth/atlassian/callback"

	redirectURL := "https://auth.atlassian.com/authorize" +
		"?audience=api.atlassian.com" +
		"&client_id=" + clientID +
		"&scope=" + url.QueryEscape("read:jira-work read:jira-user offline_access") +
		"&redirect_uri=" + url.QueryEscape(callbackURL) +
		"&state=" + state +
		"&response_type=code" +
		"&prompt=consent"
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

func HandleAtlassianCallback(w http.ResponseWriter, r *http.Request) {
	userID, ok := verifyOAuthState(w, r)
	if !ok {
		return
	}
	if DB == nil {
		http.Error(w, "database not configured", http.StatusServiceUnavailable)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code from Atlassian", http.StatusBadRequest)
		return
	}

	baseURL := os.Getenv("CAS_BASE_URL")
	if baseURL == "" {
		baseURL = "http://" + r.Host
	}
	callbackURL := strings.TrimRight(baseURL, "/") + "/auth/atlassian/callback"

	// Exchange code for tokens.
	payload := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     os.Getenv("ATLASSIAN_CLIENT_ID"),
		"client_secret": os.Getenv("ATLASSIAN_CLIENT_SECRET"),
		"code":          code,
		"redirect_uri":  callbackURL,
	}
	payloadBytes, _ := json.Marshal(payload)
	resp, err := http.Post("https://auth.atlassian.com/oauth/token",
		"application/json", bytes.NewReader(payloadBytes))
	if err != nil {
		http.Error(w, "failed to exchange Atlassian token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil || tokenResp.AccessToken == "" {
		http.Error(w, "invalid token response from Atlassian", http.StatusInternalServerError)
		return
	}

	// Fetch the user's email and accessible Atlassian sites.
	email, domain := fetchAtlassianUser(tokenResp.AccessToken)
	log.Printf("Atlassian OAuth: connected %s @ %s", email, domain)

	if err := StoreAtlassianOAuthTokens(r.Context(), userID, tokenResp.AccessToken, tokenResp.RefreshToken, email, domain); err != nil {
		http.Error(w, "failed to store token: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("Atlassian OAuth: stored token for user %s", userID)
	oauthSuccessPage(w, "atlassian", map[string]string{"email": email, "domain": domain})
}

func fetchAtlassianUser(accessToken string) (email, domain string) {
	// Get user profile.
	req, _ := http.NewRequest("GET", "https://api.atlassian.com/me", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		defer resp.Body.Close()
		var u struct {
			Email string `json:"email"`
		}
		json.NewDecoder(resp.Body).Decode(&u)
		email = u.Email
	}

	// Get accessible Atlassian sites to determine the domain.
	req2, _ := http.NewRequest("GET", "https://api.atlassian.com/oauth/token/accessible-resources", nil)
	req2.Header.Set("Authorization", "Bearer "+accessToken)
	req2.Header.Set("Accept", "application/json")
	if resp2, err := http.DefaultClient.Do(req2); err == nil {
		defer resp2.Body.Close()
		var sites []struct {
			URL string `json:"url"`
		}
		json.NewDecoder(resp2.Body).Decode(&sites)
		if len(sites) > 0 {
			// Extract domain from URL e.g. https://yourcompany.atlassian.net
			u, _ := url.Parse(sites[0].URL)
			if u != nil {
				domain = u.Host
			}
		}
	}
	return
}

// oauthSuccessPage renders a minimal page that postMessages the result to the
// opener window and closes the popup.
func oauthSuccessPage(w http.ResponseWriter, provider string, data map[string]string) {
	dataJSON, _ := json.Marshal(data)
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<!DOCTYPE html><html><body>
<script>
  if (window.opener) {
    window.opener.postMessage({type:'oauth-success',provider:%q,data:%s}, window.location.origin);
    window.close();
  } else {
    document.body.innerHTML = '<p style="font-family:sans-serif;padding:20px">✓ Connected. You can close this window.</p>';
  }
</script>
<p style="font-family:sans-serif;padding:20px;color:#23a559">✓ Connected successfully. Closing…</p>
</body></html>`, provider, string(dataJSON))
}

// ── Token injection for shell (OAuth bearer tokens use Authorization: Bearer) ─

// atlassianTokenType returns "bearer" if the user authenticated via OAuth,
// or "basic" if they pasted an API token. This affects how we inject credentials.
func atlassianTokenType(ctx context.Context, userID string) string {
	// If there's a refresh token stored, it's an OAuth token (bearer).
	var refresh *string
	DB.QueryRow(ctx, `SELECT atlassian_refresh_token FROM users WHERE id = $1`, userID).Scan(&refresh)
	if refresh != nil && *refresh != "" {
		return "bearer"
	}
	return "basic"
}

// oauthConfigured returns whether GitHub and/or Atlassian OAuth are set up server-side.
func oauthConfigured() map[string]bool {
	return map[string]bool{
		"github":    os.Getenv("GITHUB_CLIENT_ID") != "",
		"atlassian": os.Getenv("ATLASSIAN_CLIENT_ID") != "",
	}
}

// Dummy reference to time to avoid import error if unused
var _ = time.Second
var _ = strings.TrimSpace
var _ = fmt.Sprintf
