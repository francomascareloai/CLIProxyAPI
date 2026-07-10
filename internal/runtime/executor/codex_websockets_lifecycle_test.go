package executor

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestCodexSessionReconnectable(t *testing.T) {
	if codexSessionReconnectable(nil) {
		t.Fatal("nil error should not be reconnectable")
	}
	if codexSessionReconnectable(statusErr{code: http.StatusUpgradeRequired}) {
		t.Fatal("426 should not be reconnectable")
	}
	if !codexSessionReconnectable(errors.New("temporary network error")) {
		t.Fatal("generic transport error should be reconnectable")
	}
}

func TestCodexWSShouldReuseSession(t *testing.T) {
	if codexWSShouldReuseSession("") {
		t.Fatal("empty session must not reuse websocket state")
	}
	if !codexWSShouldReuseSession("sess-1") {
		t.Fatal("non-empty session should reuse websocket state")
	}
}

func TestCodexWebsocketsExecutorCloseExecutionSessionRemovesSession(t *testing.T) {
	exec := NewCodexWebsocketsExecutor(&config.Config{})
	sess := exec.getOrCreateSession("sess-1")
	if sess == nil {
		t.Fatal("expected session")
	}
	exec.CloseExecutionSession("sess-1")
	if got := exec.getOrCreateSession("sess-1"); got == sess {
		t.Fatal("expected closed session to be removed and recreated")
	}
}

func TestReadCodexWebsocketMessageHonorsContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := readCodexWebsocketMessage(ctx, &codexWebsocketSession{}, &websocket.Conn{}, make(chan codexWebsocketRead))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestCloseOnContextDoneReturnsImmediatelyForNilConn(t *testing.T) {
	done := closeOnContextDone(context.Background(), nil)
	select {
	case <-done:
		t.Fatal("done channel should stay open when helper is inert")
	case <-time.After(10 * time.Millisecond):
	}
}
