package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func benchmarkCodexExecutorNonStream(b *testing.B, compact bool) {
	b.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if compact {
			if r.URL.Path != "/responses/compact" {
				b.Fatalf("path = %q, want %q", r.URL.Path, "/responses/compact")
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"resp_compact","object":"response","model":"gpt-5","status":"completed","created_at":1700000000,"output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`)
			return
		}
		if r.URL.Path != "/responses" {
			b.Fatalf("path = %q, want %q", r.URL.Path, "/responses")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_123\",\"object\":\"response\",\"model\":\"gpt-5\",\"status\":\"completed\",\"created_at\":1700000000,\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n")
	}))
	defer upstream.Close()

	exec := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "bench-key", "base_url": upstream.URL}}
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: []byte(`{"model":"gpt-5","input":"hi"}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}
	if compact {
		opts.Alt = "responses/compact"
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := exec.Execute(context.Background(), auth, req, opts)
		if err != nil {
			b.Fatalf("Execute error: %v", err)
		}
		if len(resp.Payload) == 0 {
			b.Fatal("empty payload")
		}
	}
}

func BenchmarkCodexExecutorNonStreamIncremental(b *testing.B) {
	benchmarkCodexExecutorNonStream(b, false)
}

func BenchmarkCodexExecutorCompact(b *testing.B) {
	benchmarkCodexExecutorNonStream(b, true)
}
