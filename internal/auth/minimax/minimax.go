// Package minimax provides authentication and token management for MiniMax (agent.minimax.io) API.
// It handles JWT token obtained via OAuth2 login (GitHub/Google) for the web chat API.
package minimax

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	// MiniMaxAPIBaseURL is the base URL for MiniMax web chat API.
	MiniMaxAPIBaseURL = "https://agent.minimax.io"

	// MiniMaxStreamBaseURL is the base URL for streaming API.
	MiniMaxStreamBaseURL = "https://agent-stream.minimax.io"

	// MiniMaxAuthBaseURL is the base URL for authentication.
	MiniMaxAuthBaseURL = "https://account.minimax.io"

	// defaultHTTPTimeout is the default timeout for HTTP requests.
	defaultHTTPTimeout = 30 * time.Second

	// refreshThresholdSeconds is when to consider token needing refresh (5 minutes before expiry).
	refreshThresholdSeconds = 300
)

// MiniMaxTokenData represents the JWT token data for MiniMax API auth.
type MiniMaxTokenData struct {
	// AccessToken is the JWT Bearer token from _token cookie/localStorage.
	AccessToken string `json:"access_token"`
	// SessionID is the current chat session ID (from URL parameter).
	SessionID string `json:"session_id,omitempty"`
	// UserID is the MiniMax user ID.
	UserID string `json:"user_id,omitempty"`
	// ExpiresAt is the Unix timestamp when the token expires.
	ExpiresAt int64 `json:"expires_at"`
	// TokenType indicates the type of token, typically "Bearer".
	TokenType string `json:"token_type"`
}

// MiniMaxAuth manages authentication and token handling for the MiniMax web chat API.
type MiniMaxAuth struct {
	httpClient *http.Client
	cfg        *config.Config
}

// NewMiniMaxAuth creates a new MiniMaxAuth instance with a proxy-configured HTTP client.
func NewMiniMaxAuth(cfg *config.Config) *MiniMaxAuth {
	client := &http.Client{Timeout: defaultHTTPTimeout}
	if cfg != nil {
		client = util.SetProxy(&cfg.SDKConfig, client)
	}
	return &MiniMaxAuth{
		httpClient: client,
		cfg:        cfg,
	}
}

// ValidateKey verifies if a JWT token is valid by checking against the API.
func (ma *MiniMaxAuth) ValidateKey(ctx context.Context, token string) (bool, error) {
	if strings.TrimSpace(token) == "" {
		return false, fmt.Errorf("minimax: token is empty")
	}

	// Try a lightweight API call to validate the token
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, MiniMaxAuthBaseURL+"/v1/api/user/device/register", nil)
	if err != nil {
		return false, fmt.Errorf("minimax: failed to create validation request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := ma.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("minimax: validation request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("minimax: close body error: %v", errClose)
		}
	}()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		return true, nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return false, nil
	}

	body, _ := io.ReadAll(resp.Body)
	return false, fmt.Errorf("minimax: unexpected validation status %d: %s", resp.StatusCode, string(body))
}

// CreateTokenStorage creates a MiniMaxTokenStorage from token data.
func (ma *MiniMaxAuth) CreateTokenStorage(tokenData *MiniMaxTokenData) *MiniMaxTokenStorage {
	expired := ""
	if tokenData.ExpiresAt > 0 {
		expired = time.Unix(tokenData.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	return &MiniMaxTokenStorage{
		AccessToken: tokenData.AccessToken,
		SessionID:   tokenData.SessionID,
		UserID:      tokenData.UserID,
		Expired:     expired,
		Type:        "minimax",
	}
}

// UpdateTokenStorage updates an existing token storage with new token data.
func (ma *MiniMaxAuth) UpdateTokenStorage(storage *MiniMaxTokenStorage, tokenData *MiniMaxTokenData) {
	storage.AccessToken = tokenData.AccessToken
	if tokenData.SessionID != "" {
		storage.SessionID = tokenData.SessionID
	}
	if tokenData.UserID != "" {
		storage.UserID = tokenData.UserID
	}
	if tokenData.ExpiresAt > 0 {
		storage.Expired = time.Unix(tokenData.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	storage.LastRefresh = time.Now().Format(time.RFC3339)
}

// NewHTTPClient returns a new HTTP client configured with proxy settings.
func (ma *MiniMaxAuth) NewHTTPClient() *http.Client {
	client := &http.Client{Timeout: defaultHTTPTimeout}
	if ma.cfg != nil {
		client = util.SetProxy(&ma.cfg.SDKConfig, client)
	}
	return client
}
