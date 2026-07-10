package auth

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type refreshTestExecutor struct {
	provider string
	calls    atomic.Int32
	refresh  func(ctx context.Context, auth *Auth) (*Auth, error)
}

func (e *refreshTestExecutor) Identifier() string { return e.provider }

func (e *refreshTestExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *refreshTestExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	out := make(chan cliproxyexecutor.StreamChunk)
	close(out)
	return &cliproxyexecutor.StreamResult{Chunks: out}, nil
}

func (e *refreshTestExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *refreshTestExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *refreshTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.calls.Add(1)
	if e.refresh != nil {
		return e.refresh(ctx, auth)
	}
	return auth, nil
}

func TestManager_refreshAuth_SingleFlightAndLockfile(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{AuthDir: t.TempDir()})

	exec := &refreshTestExecutor{provider: "codex"}
	ready := make(chan struct{})
	exec.refresh = func(ctx context.Context, auth *Auth) (*Auth, error) {
		<-ready
		return auth, nil
	}
	mgr.RegisterExecutor(exec)

	_, _ = mgr.Register(context.Background(), &Auth{ID: "a1", Provider: "codex", Metadata: map[string]any{"type": "codex"}})

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			mgr.refreshAuth(context.Background(), "a1")
		}()
	}

	close(start)
	// Let goroutines pile up, then release refresh.
	time.Sleep(20 * time.Millisecond)
	close(ready)
	wg.Wait()

	if got := exec.calls.Load(); got != 1 {
		t.Fatalf("Refresh() calls=%d, want 1", got)
	}
}

func TestApplyAuthFailureState_DeactivatedWorkspace_DisablesAuth(t *testing.T) {
	now := time.Now()
	a := &Auth{ID: "a1", Provider: "claude"}
	err := &Error{HTTPStatus: 402, Code: "deactivated_workspace", Message: "deactivated_workspace"}
	applyAuthFailureState(a, err, nil, now, false)
	if !a.Disabled || a.Status != StatusDisabled {
		t.Fatalf("auth not disabled: Disabled=%v Status=%s", a.Disabled, a.Status)
	}
	if a.NextRetryAfter.IsZero() || !a.NextRetryAfter.After(now) {
		t.Fatalf("NextRetryAfter not set")
	}
}

func TestManager_refreshAuth_RefreshTokenReused_MarksReloginRequired(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{AuthDir: t.TempDir()})

	exec := &refreshTestExecutor{provider: "codex"}
	exec.refresh = func(ctx context.Context, auth *Auth) (*Auth, error) {
		return nil, &Error{Message: "refresh_token_reused"}
	}
	mgr.RegisterExecutor(exec)

	_, _ = mgr.Register(context.Background(), &Auth{ID: "a1", Provider: "codex", Metadata: map[string]any{"type": "codex"}})
	mgr.refreshAuth(context.Background(), "a1")

	updated, ok := mgr.GetByID("a1")
	if !ok {
		t.Fatalf("auth missing")
	}
	if updated.Disabled {
		t.Fatalf("expected auth not disabled")
	}
	if !updated.Unavailable || updated.Status != StatusError || updated.StatusMessage != "relogin_required" {
		t.Fatalf("expected relogin_required; got Disabled=%v Unavailable=%v Status=%s StatusMessage=%q", updated.Disabled, updated.Unavailable, updated.Status, updated.StatusMessage)
	}
	if updated.NextRefreshAfter.IsZero() || updated.NextRetryAfter.IsZero() {
		t.Fatalf("expected NextRefreshAfter and NextRetryAfter")
	}
}
