package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/minimax"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// minimaxRefreshLead is the duration before token expiry when refresh should occur.
var minimaxRefreshLead = 5 * time.Minute

// MiniMaxAuthenticator implements the OAuth login flow for MiniMax (agent.minimax.io).
type MiniMaxAuthenticator struct{}

// NewMiniMaxAuthenticator constructs a new MiniMax authenticator.
func NewMiniMaxAuthenticator() Authenticator {
	return &MiniMaxAuthenticator{}
}

// Provider returns the provider key for minimax.
func (MiniMaxAuthenticator) Provider() string {
	return "minimax"
}

// RefreshLead returns the duration before token expiry when refresh should occur.
func (MiniMaxAuthenticator) RefreshLead() *time.Duration {
	return &minimaxRefreshLead
}

// Login initiates the MiniMax authentication flow by guiding the user through
// browser-based login to obtain a JWT token.
func (a MiniMaxAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	authSvc := minimax.NewMiniMaxAuth(cfg)

	fmt.Println("\n=== MiniMax Web Chat Authentication ===")
	fmt.Println()
	fmt.Println("To authenticate with MiniMax, you need to obtain your JWT token")
	fmt.Println("from the web interface. Follow these steps:")
	fmt.Println()
	fmt.Println("1. Open https://agent.minimax.io in a browser")
	fmt.Println("2. Log in with your account (GitHub/Google OAuth)")
	fmt.Println("3. Open DevTools (F12) → Application → Local Storage")
	fmt.Println("4. Copy the value of the '_token' key")
	fmt.Println("5. Paste it below")
	fmt.Println()
	fmt.Print("JWT Token: ")

	var token string
	if !opts.NoBrowser {
		if browser.IsAvailable() {
			fmt.Println("Opening browser automatically...")
			if errOpen := browser.OpenURL("https://agent.minimax.io"); errOpen != nil {
				log.Warnf("Failed to open browser automatically: %v", errOpen)
				fmt.Print("JWT Token: ")
				_, _ = fmt.Scanln(&token)
			} else {
				fmt.Println("Browser opened. Log in, then return here and paste your token.")
				fmt.Print("JWT Token: ")
				_, _ = fmt.Scanln(&token)
			}
		} else {
			_, _ = fmt.Scanln(&token)
		}
	} else {
		_, _ = fmt.Scanln(&token)
	}

	token = trimToken(token)
	if token == "" {
		return nil, fmt.Errorf("minimax: no token provided")
	}

	// Validate the token
	fmt.Println("\nValidating token...")
	valid, err := authSvc.ValidateKey(ctx, token)
	if err != nil {
		log.Warnf("minimax: token validation encountered an issue: %v", err)
		fmt.Println("⚠️  Could not validate token, but will save it anyway.")
	} else if !valid {
		return nil, fmt.Errorf("minimax: token is invalid or expired")
	} else {
		fmt.Println("✅ Token validated successfully!")
	}

	// Create token storage
	tokenData := &minimax.MiniMaxTokenData{
		AccessToken: token,
		ExpiresAt:   0, // JWT expiry unknown; will be validated at runtime
		TokenType:   "Bearer",
	}
	tokenStorage := authSvc.CreateTokenStorage(tokenData)

	// Build metadata
	metadata := map[string]any{
		"type":         "minimax",
		"access_token": token,
		"timestamp":    time.Now().UnixMilli(),
	}

	// Generate a unique filename
	fileName := fmt.Sprintf("minimax-%d.json", time.Now().UnixMilli())

	fmt.Println("\n✅ MiniMax authentication successful!")

	return &coreauth.Auth{
		ID:       fileName,
		Provider: a.Provider(),
		FileName: fileName,
		Label:    "MiniMax User",
		Storage:  tokenStorage,
		Metadata: metadata,
	}, nil
}

// trimToken removes whitespace and common prefixes from a JWT token.
func trimToken(token string) string {
	token = fmt.Sprintf("%s", token)
	// Remove common prefixes users might include
	prefixes := []string{"Bearer ", "bearer ", "token: ", "Token: "}
	for _, p := range prefixes {
		if len(token) > len(p) && token[:len(p)] == p {
			token = token[len(p):]
		}
	}
	// Trim whitespace and quotes
	for len(token) > 0 && (token[0] == ' ' || token[0] == '\t' || token[0] == '\n' || token[0] == '\r' || token[0] == '"' || token[0] == '\'') {
		token = token[1:]
	}
	for len(token) > 0 && (token[len(token)-1] == ' ' || token[len(token)-1] == '\t' || token[len(token)-1] == '\n' || token[len(token)-1] == '\r' || token[len(token)-1] == '"' || token[len(token)-1] == '\'') {
		token = token[:len(token)-1]
	}
	return token
}
