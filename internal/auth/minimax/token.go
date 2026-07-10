// Package minimax provides authentication and token management for MiniMax web chat API.
package minimax

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
)

// MiniMaxTokenStorage stores OAuth2 JWT token information for MiniMax API authentication.
type MiniMaxTokenStorage struct {
	// AccessToken is the JWT Bearer token used for authenticating API requests.
	AccessToken string `json:"access_token"`
	// SessionID is the current chat session ID.
	SessionID string `json:"session_id,omitempty"`
	// UserID is the MiniMax user identifier.
	UserID string `json:"user_id,omitempty"`
	// LastRefresh is the timestamp of the last token refresh.
	LastRefresh string `json:"last_refresh,omitempty"`
	// Type indicates the authentication provider type, always "minimax" for this storage.
	Type string `json:"type"`
	// Expired is the RFC3339 timestamp when the access token expires, if known.
	Expired string `json:"expired,omitempty"`
}

// SaveTokenToFile serializes the MiniMax token storage to a JSON file.
func (ts *MiniMaxTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = "minimax"

	if err := os.MkdirAll(filepath.Dir(authFilePath), 0700); err != nil {
		return fmt.Errorf("failed to create directory: %v", err)
	}

	f, err := os.Create(authFilePath)
	if err != nil {
		return fmt.Errorf("failed to create token file: %w", err)
	}
	defer func() {
		_ = f.Close()
	}()

	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(ts); err != nil {
		return fmt.Errorf("failed to write token to file: %w", err)
	}
	return nil
}

// IsExpired checks if the token has expired.
func (ts *MiniMaxTokenStorage) IsExpired() bool {
	if ts.Expired == "" {
		return false // No expiry set, assume valid
	}
	t, err := time.Parse(time.RFC3339, ts.Expired)
	if err != nil {
		return true // Has expiry string but can't parse
	}
	// Consider expired if within refresh threshold
	return time.Now().Add(time.Duration(refreshThresholdSeconds) * time.Second).After(t)
}

// NeedsRefresh checks if the token should be refreshed.
func (ts *MiniMaxTokenStorage) NeedsRefresh() bool {
	return ts.IsExpired()
}
