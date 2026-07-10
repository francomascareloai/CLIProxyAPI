package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorExecute_NonStreamReturnsOnFirstCompletedEvent(t *testing.T) {
	requestCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if got := r.URL.Path; got != "/responses" {
			t.Fatalf("path = %q, want %q", got, "/responses")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_123\",\"object\":\"response\",\"model\":\"gpt-5\",\"status\":\"completed\",\"created_at\":1700000000,\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_late\"}}\n")
	}))
	defer upstream.Close()

	exec := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": upstream.URL}}
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5",
		Payload: []byte(`{"model":"gpt-5","input":"hi"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if requestCount != 1 {
		t.Fatalf("requests = %d, want 1", requestCount)
	}
	body := string(resp.Payload)
	if !strings.Contains(body, `"id":"resp_123"`) {
		t.Fatalf("payload missing first completion id: %s", body)
	}
	if strings.Contains(body, `resp_late`) {
		t.Fatalf("payload should ignore later completion events: %s", body)
	}
}

func TestCodexExecutorExecute_NonStreamMissingCompletionReturns408(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n")
	}))
	defer upstream.Close()

	exec := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": upstream.URL}}
	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5",
		Payload: []byte(`{"model":"gpt-5","input":"hi"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	statusProvider, ok := err.(interface{ StatusCode() int })
	if !ok || statusProvider.StatusCode() != http.StatusRequestTimeout {
		t.Fatalf("status = %v, want %d", err, http.StatusRequestTimeout)
	}
}

func TestCodexExecutorExecute_CompactAutoFallsBackToLegacyOn404(t *testing.T) {
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/responses/compact":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"type":"not_found","message":"no compact"}}`)
		case "/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fallback\",\"object\":\"response\",\"model\":\"gpt-5\",\"status\":\"completed\",\"created_at\":1700000000,\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	exec := NewCodexExecutor(&config.Config{CodexKey: []config.CodexKey{{APIKey: "test-key", BaseURL: upstream.URL, NonStreamStrategy: "compact_auto"}}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": upstream.URL, "non_stream_strategy": "compact_auto"}}
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5",
		Payload: []byte(`{"model":"gpt-5","input":"hi"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got, want := strings.Join(paths, ","), "/responses/compact,/responses"; got != want {
		t.Fatalf("paths = %q, want %q", got, want)
	}
	if !bytes.Contains(resp.Payload, []byte(`"id":"resp_fallback"`)) {
		t.Fatalf("unexpected payload: %s", string(resp.Payload))
	}
}

func TestCodexExecutorExecute_CompactAutoNormalizesFastServiceTier(t *testing.T) {
	var compactBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/responses/compact" {
			compactBody = append([]byte(nil), body...)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-5.4","service_tier":"default","output":[{"type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"pong"}]}]}`))
	}))
	defer upstream.Close()

	exec := NewCodexExecutor(&config.Config{CodexKey: []config.CodexKey{{APIKey: "test-key", BaseURL: upstream.URL, NonStreamStrategy: "compact_auto"}}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": upstream.URL, "non_stream_strategy": "compact_auto"}}
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"ping","service_tier":"fast"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if len(resp.Payload) == 0 {
		t.Fatal("expected non-empty payload")
	}
	if got := gjson.GetBytes(compactBody, "service_tier").String(); got != "priority" {
		t.Fatalf("compact request service_tier = %q, want %q", got, "priority")
	}
}
