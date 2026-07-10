package executor

import (
	"context"
	"testing"

	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestPrepareCodexRequestStream(t *testing.T) {
	exec := NewCodexExecutor(&config.Config{})
	prepared, err := exec.prepareCodexRequest(
		&cliproxyauth.Auth{ID: "auth-1", Label: "codex", Attributes: map[string]string{"api_key": "k", "base_url": "http://example.com"}},
		cliproxyexecutor.Request{Model: "gpt-5", Payload: []byte(`{"model":"gpt-5","input":"hi"}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")},
		sdktranslator.FromString("codex"),
		true,
	)
	if err != nil {
		t.Fatalf("prepareCodexRequest error: %v", err)
	}
	if !gjson.GetBytes(prepared.body, "stream").Bool() {
		t.Fatal("expected stream=true")
	}
	if got := gjson.GetBytes(prepared.body, "model").String(); got != "gpt-5" {
		t.Fatalf("model = %q, want %q", got, "gpt-5")
	}
}

func TestPrepareCodexRequestNonStreamRemovesStream(t *testing.T) {
	exec := NewCodexExecutor(&config.Config{})
	prepared, err := exec.prepareCodexRequest(
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "k", "base_url": "http://example.com"}},
		cliproxyexecutor.Request{Model: "gpt-5", Payload: []byte(`{"model":"gpt-5","input":"hi","stream":true}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")},
		sdktranslator.FromString("openai-response"),
		false,
	)
	if err != nil {
		t.Fatalf("prepareCodexRequest error: %v", err)
	}
	if gjson.GetBytes(prepared.body, "stream").Exists() {
		t.Fatal("expected stream field to be removed")
	}
}

func TestPrepareCodexRequestStreamPreservesPreviousResponseIDForCodex(t *testing.T) {
	exec := NewCodexExecutor(&config.Config{})
	prepared, err := exec.prepareCodexRequest(
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "k", "base_url": "http://example.com"}},
		cliproxyexecutor.Request{Model: "gpt-5", Payload: []byte(`{"model":"gpt-5","previous_response_id":"resp-1","input":"hi"}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")},
		sdktranslator.FromString("codex"),
		true,
	)
	if err != nil {
		t.Fatalf("prepareCodexRequest error: %v", err)
	}
	if got := gjson.GetBytes(prepared.body, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %q, want %q", got, "resp-1")
	}
}

func TestBuildCodexWebsocketRequestBodyUsesAppendWhenPreviousResponseIDExists(t *testing.T) {
	payload := []byte(`{"previous_response_id":"resp-1","input":[{"type":"message","text":"hi"}]}`)
	got := buildCodexWebsocketRequestBody(payload, true)
	if gotType := gjson.GetBytes(got, "type").String(); gotType != "response.append" {
		t.Fatalf("type = %q, want %q", gotType, "response.append")
	}
	if !gjson.GetBytes(got, "input").IsArray() {
		t.Fatal("expected input array in append payload")
	}
}

func TestRecordCodexUpstreamRequestNoPanic(t *testing.T) {
	recordCodexUpstreamRequest(context.Background(), &config.Config{}, "http://example.com/responses", "POST", nil, []byte(`{}`), "a", "b", "c", "d")
}
