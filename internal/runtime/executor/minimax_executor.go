// Package executor provides ProviderExecutor implementations for various AI providers.
package executor

import (
	"bufio"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	minimaxauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/minimax"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// MiniMaxExecutor is a stateless executor for MiniMax web chat API.
// It communicates with agent-stream.minimax.io using JWT Bearer auth.
type MiniMaxExecutor struct {
	cfg *config.Config
}

// NewMiniMaxExecutor creates a new MiniMax executor.
func NewMiniMaxExecutor(cfg *config.Config) *MiniMaxExecutor {
	return &MiniMaxExecutor{cfg: cfg}
}

// Identifier returns the executor identifier.
func (e *MiniMaxExecutor) Identifier() string { return "minimax" }

// getMiniMaxToken extracts the JWT token from auth metadata.
func getMiniMaxToken(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["access_token"].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// getMiniMaxSessionID extracts the session ID from auth metadata or attributes.
func getMiniMaxSessionID(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	// Check attributes first (runtime-set)
	if auth.Attributes != nil {
		if v := auth.Attributes["session_id"]; v != "" {
			return v
		}
	}
	// Fall back to metadata (persisted)
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["session_id"].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// PrepareRequest injects MiniMax credentials into the outgoing HTTP request.
func (e *MiniMaxExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token := getMiniMaxToken(auth)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

// HttpRequest injects MiniMax credentials into the request and executes it.
func (e *MiniMaxExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("minimax executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// miniMaxMessage is the custom request format for MiniMax web chat API.
type miniMaxMessage struct {
	Content   string          `json:"content"`
	Model     miniMaxModel    `json:"model"`
	TurnID    string          `json:"turn_id"`
	EnableTeam bool           `json:"enable_team"`
	Worktree  bool            `json:"worktreeMode"`
}

type miniMaxModel struct {
	ProviderID string `json:"provider_id"`
	ModelID    string `json:"model_id"`
	Variant    string `json:"variant"`
}

// buildMiniMaxPayload converts an OpenAI-format payload to MiniMax custom format.
func (e *MiniMaxExecutor) buildMiniMaxPayload(body []byte, baseModel string, auth *cliproxyauth.Auth) ([]byte, error) {
	// Extract the user message from messages array
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() || len(messages.Array()) == 0 {
		return nil, fmt.Errorf("minimax executor: no messages in request")
	}

	// Find the last user message
	msgs := messages.Array()
	var lastUserContent string
	for i := len(msgs) - 1; i >= 0; i-- {
		role := msgs[i].Get("role").String()
		if role == "user" {
			content := msgs[i].Get("content")
			if content.IsObject() || content.IsArray() {
				// Multipart content - extract text parts
				var parts []string
				for _, part := range content.Array() {
					if part.Get("type").String() == "text" {
						parts = append(parts, part.Get("text").String())
					}
				}
				if len(parts) > 0 {
					lastUserContent = strings.Join(parts, "\n")
				} else {
					lastUserContent = content.Raw
				}
			} else {
				lastUserContent = content.String()
			}
			break
		}
	}

	if lastUserContent == "" {
		return nil, fmt.Errorf("minimax executor: no user message found")
	}

	// Generate turn_id (UUID v4)
	turnID := uuid.New().String()

	// Map OpenAI model to MiniMax model ID
	modelID := baseModel
	if modelID == "" {
		modelID = "MiniMax-M3"
	}

	// Build the custom payload
	msg := miniMaxMessage{
		Content:    lastUserContent,
		Model: miniMaxModel{
			ProviderID: "minimax",
			ModelID:    modelID,
			Variant:    "",
		},
		TurnID:     turnID,
		EnableTeam: true,
		Worktree:   false,
	}

	return json.Marshal(msg)
}

// Execute performs a non-streaming chat completion request to MiniMax.
func (e *MiniMaxExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	token := getMiniMaxToken(auth)
	if token == "" {
		return resp, fmt.Errorf("minimax executor: no JWT token available")
	}

	sessionID := getMiniMaxSessionID(auth)
	if sessionID == "" {
		return resp, fmt.Errorf("minimax executor: no session ID available")
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := bytes.Clone(originalPayloadSource)
	_ = originalPayload
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), false)

	// Build MiniMax custom payload
	minimaxBody, err := e.buildMiniMaxPayload(body, baseModel, auth)
	if err != nil {
		return resp, fmt.Errorf("minimax executor: failed to build payload: %w", err)
	}

	url := fmt.Sprintf("%s/archon/api/v1/session/%s/message", minimaxauth.MiniMaxStreamBaseURL, sessionID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(minimaxBody))
	if err != nil {
		return resp, fmt.Errorf("minimax executor: failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Accept", "application/json")

	var authLabel, authType, authValue string
	if auth != nil {
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      minimaxBody,
		Provider:  e.Identifier(),
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("minimax executor: close response body error: %v", errClose)
		}
	}()

	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("minimax request error, status: %d, body: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return resp, err
	}

	// Read the full response (SSE or JSON)
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)

	// Parse SSE response to extract content
	responseContent := parseMiniMaxSSEResponse(data)

	// Build OpenAI-compatible response
	openaiResp := buildOpenAIResponse(baseModel, responseContent)
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, body, openaiResp, nil)
	resp = cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}
	return resp, nil
}

// ExecuteStream performs a streaming chat completion request to MiniMax.
func (e *MiniMaxExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	token := getMiniMaxToken(auth)
	if token == "" {
		return nil, fmt.Errorf("minimax executor: no JWT token available")
	}

	sessionID := getMiniMaxSessionID(auth)
	if sessionID == "" {
		return nil, fmt.Errorf("minimax executor: no session ID available")
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := bytes.Clone(originalPayloadSource)
	_ = originalPayload
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), true)

	// Build MiniMax custom payload
	minimaxBody, err := e.buildMiniMaxPayload(body, baseModel, auth)
	if err != nil {
		return nil, fmt.Errorf("minimax executor: failed to build payload: %w", err)
	}

	url := fmt.Sprintf("%s/archon/api/v1/session/%s/message", minimaxauth.MiniMaxStreamBaseURL, sessionID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(minimaxBody))
	if err != nil {
		return nil, fmt.Errorf("minimax executor: failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Accept", "text/event-stream")

	var authLabel, authType, authValue string
	if auth != nil {
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      minimaxBody,
		Provider:  e.Identifier(),
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("minimax stream request error, status: %d, body: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("minimax executor: close response body error: %v", errClose)
		}
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("minimax executor: close response body error: %v", errClose)
			}
		}()

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 1_048_576) // 1MB buffer
		var param any

		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)

			// Check for SSE data lines
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 || trimmed[0] != '{' {
				continue
			}

			// Parse MiniMax SSE JSON
			var sseEvent struct {
				Event string `json:"event"`
				Data  struct {
					Content string `json:"content"`
					Done    bool   `json:"done"`
				} `json:"data"`
			}
			if err := json.Unmarshal(trimmed, &sseEvent); err != nil {
				continue
			}

			if sseEvent.Event == "done" || sseEvent.Data.Done {
				// Final chunk with usage
				chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, body, []byte("[DONE]"), &param)
				for i := range chunks {
					out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
				}
				continue
			}

			if sseEvent.Data.Content != "" {
				// Build OpenAI delta chunk
				delta := buildOpenAIStreamDelta(baseModel, sseEvent.Data.Content)
				chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, body, delta, &param)
				for i := range chunks {
					out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
				}
			}
		}

		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
	}()

	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// Refresh attempts to refresh the MiniMax JWT token.
// MiniMax JWT tokens expire and require re-login via the web interface.
// This method checks if the token is still valid and reports expiry.
func (e *MiniMaxExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("minimax executor: refresh called")
	if auth == nil {
		return nil, fmt.Errorf("minimax executor: auth is nil")
	}

	token := getMiniMaxToken(auth)
	if token == "" {
		return nil, fmt.Errorf("minimax executor: no token to refresh")
	}

	// Validate the token against MiniMax API
	svc := minimaxauth.NewMiniMaxAuth(e.cfg)
	valid, err := svc.ValidateKey(ctx, token)
	if err != nil {
		log.Warnf("minimax executor: token validation error: %v", err)
		// If validation fails due to network, assume valid
		return auth, nil
	}

	if !valid {
		return nil, fmt.Errorf("minimax executor: JWT token expired, please re-login via 'cliproxy --minimax-login'")
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["type"] = "minimax"
	auth.Metadata["last_refresh"] = time.Now().Format(time.RFC3339)
	return auth, nil
}

// CountTokens estimates token count for MiniMax requests.
func (e *MiniMaxExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	modelName := gjson.GetBytes(body, "model").String()
	if strings.TrimSpace(modelName) == "" {
		modelName = baseModel
	}

	// Use simple token counting (MiniMax doesn't have public tokenizer)
	count := estimateTokenCount(body)
	usageJSON := helps.BuildOpenAIUsageJSON(int64(count))
	translated := sdktranslator.TranslateTokenCount(ctx, to, from, int64(count), usageJSON)
	return cliproxyexecutor.Response{Payload: []byte(translated)}, nil
}

// parseMiniMaxSSEResponse extracts content from MiniMax SSE response.
func parseMiniMaxSSEResponse(data []byte) string {
	var contentParts []string
	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			continue
		}
		var sseEvent struct {
			Event string `json:"event"`
			Data  struct {
				Content string `json:"content"`
			} `json:"data"`
		}
		if err := json.Unmarshal(trimmed, &sseEvent); err != nil {
			continue
		}
		if sseEvent.Data.Content != "" {
			contentParts = append(contentParts, sseEvent.Data.Content)
		}
	}
	return strings.Join(contentParts, "")
}

// buildOpenAIResponse builds an OpenAI-compatible non-streaming response.
func buildOpenAIResponse(model, content string) []byte {
	resp := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:8]),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}
	b, _ := json.Marshal(resp)
	return b
}

// buildOpenAIStreamDelta builds an OpenAI-compatible streaming delta chunk.
func buildOpenAIStreamDelta(model, content string) []byte {
	chunk := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:8]),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index": 0,
				"delta": map[string]any{
					"content": content,
				},
			},
		},
	}
	b, _ := json.Marshal(chunk)
	return b
}

// estimateTokenCount provides a rough token count estimate.
func estimateTokenCount(body []byte) int {
	text := string(body)
	// Rough estimate: ~4 chars per token for Chinese/English mixed text
	return len(text) / 4
}

// ensure minimax_executor.go is built with uuid dependency
var _ = uuid.New
