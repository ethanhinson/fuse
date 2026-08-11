package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
)

// authServerMeta holds OAuth Authorization Server Metadata (RFC 8414).
type authServerMeta struct {
	Issuer                        string   `json:"issuer"`
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	RegistrationEndpoint          string   `json:"registration_endpoint,omitempty"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
}

// tokenSet stores OAuth tokens on disk.
type tokenSet struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry"`
	ClientID     string    `json:"client_id,omitempty"`     // from dynamic registration
	ClientSecret string    `json:"client_secret,omitempty"` // from dynamic registration
}

// discoverAuthMeta fetches the OAuth Authorization Server Metadata for serverURL.
func discoverAuthMeta(serverURL string) (*authServerMeta, error) {
	metaURL := strings.TrimRight(serverURL, "/") + "/.well-known/oauth-authorization-server"
	resp, err := http.Get(metaURL) //nolint:noctx
	if err != nil {
		return nil, fmt.Errorf("oauth discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth discovery: HTTP %d", resp.StatusCode)
	}
	var meta authServerMeta
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("oauth discovery decode: %w", err)
	}
	return &meta, nil
}

// generatePKCE generates a PKCE code verifier and challenge (S256 method).
func generatePKCE() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", fmt.Errorf("pkce random: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return verifier, challenge, nil
}

// dynamicRegister performs OAuth 2.0 Dynamic Client Registration (RFC 7591).
func dynamicRegister(meta *authServerMeta, serverName string) (clientID, clientSecret string, err error) {
	if meta.RegistrationEndpoint == "" {
		return "", "", fmt.Errorf("server has no registration_endpoint")
	}
	body, _ := json.Marshal(map[string]any{
		"client_name":                "fuse-mcp-" + serverName,
		"redirect_uris":              []string{"http://localhost"},
		"grant_types":                []string{"authorization_code"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "client_secret_post",
	})
	resp, err := http.Post(meta.RegistrationEndpoint, "application/json", bytes.NewReader(body)) //nolint:noctx
	if err != nil {
		return "", "", fmt.Errorf("dynamic registration: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("dynamic registration: HTTP %d", resp.StatusCode)
	}
	var reg struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil {
		return "", "", fmt.Errorf("dynamic registration decode: %w", err)
	}
	return reg.ClientID, reg.ClientSecret, nil
}

// runBrowserFlow runs the full OAuth authorization code + PKCE browser flow.
func runBrowserFlow(meta *authServerMeta, clientID, clientSecret string, scopes []string) (*tokenSet, error) {
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return nil, err
	}

	// Start local callback server on a random port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("oauth callback listener: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://localhost:%d/callback", port)

	state := func() string {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		return base64.RawURLEncoding.EncodeToString(b)
	}()

	// Build authorization URL.
	params := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	if len(scopes) > 0 {
		params.Set("scope", strings.Join(scopes, " "))
	}
	authURL := meta.AuthorizationEndpoint + "?" + params.Encode()

	// Channel to receive the authorization code.
	type callbackResult struct {
		code string
		err  error
	}
	codeCh := make(chan callbackResult, 1)

	mux := http.NewServeMux()
	srv := &http.Server{Handler: mux}
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			codeCh <- callbackResult{err: fmt.Errorf("oauth state mismatch")}
			return
		}
		if errMsg := q.Get("error"); errMsg != "" {
			http.Error(w, errMsg, http.StatusBadRequest)
			codeCh <- callbackResult{err: fmt.Errorf("oauth error: %s", errMsg)}
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			codeCh <- callbackResult{err: fmt.Errorf("oauth: no code in callback")}
			return
		}
		fmt.Fprintln(w, "Authentication successful. You may close this window.")
		codeCh <- callbackResult{code: code}
	})

	go srv.Serve(ln) //nolint:errcheck

	// Open browser.
	fmt.Fprintf(os.Stderr, "[mcp/oauth] Opening browser for authorization: %s\n", authURL)
	openBrowser(authURL)

	// Wait for callback with timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var code string
	select {
	case <-ctx.Done():
		_ = srv.Shutdown(context.Background())
		return nil, fmt.Errorf("oauth: browser flow timed out after 2 minutes")
	case result := <-codeCh:
		_ = srv.Shutdown(context.Background())
		if result.err != nil {
			return nil, result.err
		}
		code = result.code
	}

	// Exchange code for tokens.
	return exchangeCode(meta, clientID, clientSecret, code, redirectURI, verifier)
}

// exchangeCode exchanges an authorization code for tokens.
func exchangeCode(meta *authServerMeta, clientID, clientSecret, code, redirectURI, verifier string) (*tokenSet, error) {
	body := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	}
	if clientSecret != "" {
		body.Set("client_secret", clientSecret)
	}
	resp, err := http.PostForm(meta.TokenEndpoint, body) //nolint:noctx
	if err != nil {
		return nil, fmt.Errorf("oauth token exchange: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth token exchange: HTTP %d", resp.StatusCode)
	}
	return decodeTokenResponse(resp)
}

// refreshTokens uses a refresh token to obtain a new access token.
func refreshTokens(meta *authServerMeta, clientID, clientSecret, refreshToken string) (*tokenSet, error) {
	body := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	}
	if clientSecret != "" {
		body.Set("client_secret", clientSecret)
	}
	resp, err := http.PostForm(meta.TokenEndpoint, body) //nolint:noctx
	if err != nil {
		return nil, fmt.Errorf("oauth refresh: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth refresh: HTTP %d", resp.StatusCode)
	}
	return decodeTokenResponse(resp)
}

// decodeTokenResponse parses a token endpoint response into a tokenSet.
func decodeTokenResponse(resp *http.Response) (*tokenSet, error) {
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("oauth token decode: %w", err)
	}
	expiry := time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	if tr.ExpiresIn == 0 {
		expiry = time.Now().Add(1 * time.Hour) // default if not provided
	}
	return &tokenSet{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		Expiry:       expiry,
	}, nil
}

// tokenFilePath returns the path to the token file for the given server name.
// If override is non-empty, it is used directly.
func tokenFilePath(serverName string, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("token file path: %w", err)
	}
	return filepath.Join(home, ".fuse", "mcp-tokens", serverName+".json"), nil
}

// loadTokens reads and unmarshals a tokenSet from disk.
// Returns nil (no error) if the file does not exist.
func loadTokens(path string) (*tokenSet, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load tokens: %w", err)
	}
	var ts tokenSet
	if err := json.Unmarshal(data, &ts); err != nil {
		return nil, fmt.Errorf("load tokens decode: %w", err)
	}
	return &ts, nil
}

// saveTokens marshals a tokenSet and writes it to disk, creating directories as needed.
func saveTokens(path string, ts *tokenSet) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("save tokens mkdir: %w", err)
	}
	data, err := json.MarshalIndent(ts, "", "  ")
	if err != nil {
		return fmt.Errorf("save tokens marshal: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("save tokens write: %w", err)
	}
	return nil
}

// GetAccessToken resolves the bearer token to use for an HTTP MCP server.
// It is the main entry point for OAuth logic.
func GetAccessToken(serverName, serverURL string, cfg config.MCPAuthConfig) (string, error) {
	switch cfg.Type {
	case "", "none":
		return "", nil
	case "bearer":
		return cfg.ClientSecret, nil
	case "oauth2":
		return getOAuth2Token(serverName, serverURL, cfg)
	case "identity", "oauth-exchange":
		// Identity-propagation tiers (change #52/#59) carry NO dial-time static token:
		// the per-call, audience-bound delegation token is minted from the loop
		// initiator's identity at MCPTool.Execute and injected onto the outbound
		// request by the egress seam (applyCallAuth), never baked into the client. So
		// the transport dials with an empty token — the handshake (initialize/tools/list)
		// runs unauthenticated or under whatever the server permits pre-auth, and each
		// tools/call then presents its own delegated token. Returning ("", nil) here lets
		// these servers dial at all (previously they failed with "unknown auth type").
		return "", nil
	default:
		return "", fmt.Errorf("unknown auth type %q", cfg.Type)
	}
}

// getOAuth2Token handles the full oauth2 token acquisition lifecycle.
func getOAuth2Token(serverName, serverURL string, cfg config.MCPAuthConfig) (string, error) {
	tokPath, err := tokenFilePath(serverName, cfg.TokenFile)
	if err != nil {
		return "", err
	}

	// Try to load existing tokens.
	ts, err := loadTokens(tokPath)
	if err != nil {
		return "", err
	}

	if ts != nil && ts.AccessToken != "" && time.Now().Before(ts.Expiry.Add(-30*time.Second)) {
		return ts.AccessToken, nil
	}

	// Discover authorization server metadata.
	meta, err := discoverAuthMeta(serverURL)
	if err != nil {
		return "", err
	}

	// Try refresh if we have a refresh token.
	if ts != nil && ts.RefreshToken != "" {
		clientID := cfg.ClientID
		clientSecret := cfg.ClientSecret
		if ts.ClientID != "" {
			clientID = ts.ClientID
		}
		if ts.ClientSecret != "" {
			clientSecret = ts.ClientSecret
		}
		refreshed, err := refreshTokens(meta, clientID, clientSecret, ts.RefreshToken)
		if err == nil {
			// Preserve dynamic registration credentials.
			if ts.ClientID != "" {
				refreshed.ClientID = ts.ClientID
				refreshed.ClientSecret = ts.ClientSecret
			}
			if saveErr := saveTokens(tokPath, refreshed); saveErr != nil {
				fmt.Fprintf(os.Stderr, "[mcp/oauth] warning: could not save tokens: %v\n", saveErr)
			}
			return refreshed.AccessToken, nil
		}
		fmt.Fprintf(os.Stderr, "[mcp/oauth] refresh failed (%v), starting browser flow\n", err)
	}

	// Determine client credentials — use dynamic registration if no client_id configured.
	clientID := cfg.ClientID
	clientSecret := cfg.ClientSecret
	var regClientID, regClientSecret string
	if clientID == "" {
		regClientID, regClientSecret, err = dynamicRegister(meta, serverName)
		if err != nil {
			return "", fmt.Errorf("dynamic registration: %w", err)
		}
		clientID = regClientID
		clientSecret = regClientSecret
	}

	// Run full browser flow.
	newTS, err := runBrowserFlow(meta, clientID, clientSecret, cfg.Scopes)
	if err != nil {
		return "", err
	}

	// Persist dynamic registration credentials alongside tokens.
	if regClientID != "" {
		newTS.ClientID = regClientID
		newTS.ClientSecret = regClientSecret
	}

	if saveErr := saveTokens(tokPath, newTS); saveErr != nil {
		fmt.Fprintf(os.Stderr, "[mcp/oauth] warning: could not save tokens: %v\n", saveErr)
	}

	return newTS.AccessToken, nil
}

// openBrowser tries to open the given URL in the default browser.
func openBrowser(urlStr string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", urlStr)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", urlStr)
	default:
		cmd = exec.Command("xdg-open", urlStr)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "[mcp/oauth] could not open browser automatically.\nPlease open this URL manually:\n  %s\n", urlStr)
		return
	}
	go cmd.Wait() //nolint:errcheck
}
