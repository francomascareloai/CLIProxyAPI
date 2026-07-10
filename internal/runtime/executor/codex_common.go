package executor

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type codexPreparedRequest struct {
	baseModel       string
	from            sdktranslator.Format
	to              sdktranslator.Format
	originalPayload []byte
	body            []byte
	authID          string
	authLabel       string
	authType        string
	authValue       string
	apiKey          string
	baseURL         string
}

func (e *CodexExecutor) prepareCodexRequest(auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, to sdktranslator.Format, stream bool) (codexPreparedRequest, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	from := opts.SourceFormat
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, stream)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, stream)

	var err error
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return codexPreparedRequest{}, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	body = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel, "")
	body = normalizeCodexServiceTier(body)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	if stream {
		body, _ = sjson.SetBytes(body, "stream", true)
	} else {
		body, _ = sjson.DeleteBytes(body, "stream")
	}
	if to == sdktranslator.FromString("openai-response") || !stream {
		body, _ = sjson.DeleteBytes(body, "previous_response_id")
	}
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	if !gjson.GetBytes(body, "instructions").Exists() {
		body, _ = sjson.SetBytes(body, "instructions", "")
	}

	prepared := codexPreparedRequest{
		baseModel:       baseModel,
		from:            from,
		to:              to,
		originalPayload: originalPayload,
		body:            body,
		apiKey:          apiKey,
		baseURL:         baseURL,
	}
	if auth != nil {
		prepared.authID = auth.ID
		prepared.authLabel = auth.Label
		prepared.authType, prepared.authValue = auth.AccountInfo()
	}
	return prepared, nil
}

func recordCodexUpstreamRequest(ctx context.Context, cfg *config.Config, url string, method string, headers http.Header, body []byte, authID, authLabel, authType, authValue string) {
	helps.RecordAPIRequest(ctx, cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    method,
		Headers:   headers,
		Body:      body,
		Provider:  "codex",
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
}

func buildCodexResponsesURL(baseURL string, compact bool) string {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if compact {
		return base + "/responses/compact"
	}
	return base + "/responses"
}

func codexWSShouldReuseSession(sessionID string) bool {
	return strings.TrimSpace(sessionID) != ""
}

func codexSessionReconnectable(err error) bool {
	if err == nil {
		return false
	}
	if statusProvider, ok := err.(interface{ StatusCode() int }); ok && statusProvider != nil {
		switch statusProvider.StatusCode() {
		case http.StatusUpgradeRequired, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
			return false
		}
	}
	return true
}
