package cmd

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	log "github.com/sirupsen/logrus"
)

// DoMiniMaxLogin triggers the browser-based login flow for MiniMax (agent.minimax.io)
// and saves the JWT token to the auth directory.
//
// Unlike Kimi/Qwen which use OAuth device code flow, MiniMax web chat uses a
// JWT token obtained via GitHub/Google OAuth through the browser. The user must
// log in via the web interface and copy the _token value from localStorage.
func DoMiniMaxLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}

	manager := newAuthManager()
	authOpts := &sdkAuth.LoginOptions{
		NoBrowser: options.NoBrowser,
		Metadata:  map[string]string{},
		Prompt:    options.Prompt,
	}

	record, savedPath, err := manager.Login(context.Background(), "minimax", cfg, authOpts)
	if err != nil {
		log.Errorf("MiniMax authentication failed: %v", err)
		return
	}

	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	if record != nil && record.Label != "" {
		fmt.Printf("Authenticated as %s\n", record.Label)
	}
	fmt.Println("MiniMax authentication successful!")
	fmt.Println()
	fmt.Println("To use MiniMax models, add to config.yaml:")
	fmt.Println("  openai-compatibility:")
	fmt.Println("    - name: \"minimax\"")
	fmt.Println("      prefix: \"mm\"")
	fmt.Println("      base-url: \"https://agent.minimax.io/archon\"")
	fmt.Println("      api-key-entries:")
	fmt.Println("        - api-key: \"minimax-token\"")
	fmt.Println("      models:")
	fmt.Println("        - name: \"MiniMax-M3\"")
	fmt.Println("          alias: \"MiniMax-M3\"")
	fmt.Println("        - name: \"MiniMax-M2.7\"")
	fmt.Println("          alias: \"MiniMax-M2.7\"")
}
