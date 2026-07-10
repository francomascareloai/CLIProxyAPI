package executor

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestCodexShouldUseWebsocketNonStream(t *testing.T) {
	httpExec := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"websockets": "true", "non_stream_strategy": "websocket"}}
	if !codexShouldUseWebsocketNonStream(context.Background(), auth, httpExec, cliproxyexecutor.Options{}) {
		t.Fatal("expected websocket non-stream strategy to use websocket transport")
	}
}

func TestCodexShouldUseWebsocketNonStreamRejectsCompactAlt(t *testing.T) {
	httpExec := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"websockets": "true", "non_stream_strategy": "websocket"}}
	if codexShouldUseWebsocketNonStream(context.Background(), auth, httpExec, cliproxyexecutor.Options{Alt: "responses/compact"}) {
		t.Fatal("expected compact alt to stay on HTTP transport")
	}
}
