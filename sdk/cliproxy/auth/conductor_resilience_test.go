package auth

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type resilienceExecutor struct {
	id           string
	blockCh      <-chan struct{}
	executeCalls atomic.Int32
	failErr      error
}

func (e *resilienceExecutor) Identifier() string { return e.id }

func (e *resilienceExecutor) Execute(_ context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.executeCalls.Add(1)
	if e.blockCh != nil {
		<-e.blockCh
	}
	if e.failErr != nil {
		return cliproxyexecutor.Response{}, e.failErr
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *resilienceExecutor) ExecuteStream(_ context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	ch := make(chan cliproxyexecutor.StreamChunk)
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (e *resilienceExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *resilienceExecutor) CountTokens(_ context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *resilienceExecutor) HttpRequest(_ context.Context, _ *Auth, _ *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func registerResilienceAuth(t *testing.T, manager *Manager, provider, id string) {
	t.Helper()
	_, err := manager.Register(context.Background(), &Auth{
		ID:       id,
		Provider: provider,
		Status:   StatusActive,
		Metadata: map[string]any{"email": id},
	})
	if err != nil {
		t.Fatalf("register auth failed: %v", err)
	}
}

func TestManagerBackpressurePerProvider(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		ProviderResilience: internalconfig.ProviderResilienceConfig{
			CircuitBreakerEnabled:  false,
			MaxInflightPerProvider: 1,
		},
	})

	block := make(chan struct{})
	exec := &resilienceExecutor{id: "codex", blockCh: block}
	manager.RegisterExecutor(exec)
	registerResilienceAuth(t, manager, "codex", "a1")

	firstDone := make(chan error, 1)
	go func() {
		_, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
		firstDone <- err
	}()

	time.Sleep(40 * time.Millisecond)
	_, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err == nil {
		close(block)
		t.Fatal("expected second concurrent execute to be backpressured")
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Code != "provider_backpressure" {
		close(block)
		t.Fatalf("expected provider_backpressure error, got %v", err)
	}

	close(block)
	if errFirst := <-firstDone; errFirst != nil {
		t.Fatalf("first execute should succeed, got %v", errFirst)
	}
}

func TestManagerCircuitBreakerOpensAfterThreshold(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		ProviderResilience: internalconfig.ProviderResilienceConfig{
			CircuitBreakerEnabled:  true,
			FailureThreshold:       2,
			HalfOpenMaxRequests:    1,
			OpenStateMS:            60000,
			MaxInflightPerProvider: 8,
		},
	})

	exec := &resilienceExecutor{
		id:      "codex",
		failErr: &Error{Code: "upstream_500", Message: "upstream failed", HTTPStatus: 500},
	}
	manager.RegisterExecutor(exec)
	_, err := manager.Register(context.Background(), &Auth{
		ID:       "a1",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"email":           "a1",
			"disable_cooling": true,
		},
	})
	if err != nil {
		t.Fatalf("register auth failed: %v", err)
	}

	for i := 0; i < 2; i++ {
		_, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
		if err == nil {
			t.Fatalf("attempt %d should fail", i+1)
		}
	}
	callsBeforeOpen := exec.executeCalls.Load()

	_, err = manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("expected provider_circuit_open after threshold")
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Code != "provider_circuit_open" {
		t.Fatalf("expected provider_circuit_open error, got %v", err)
	}
	if got := exec.executeCalls.Load(); got != callsBeforeOpen {
		t.Fatalf("expected no new executor calls when circuit is open, got %d -> %d", callsBeforeOpen, got)
	}
}

func TestManagerAdaptiveLimiterBackoffAndPermitCap(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		ProviderResilience: internalconfig.ProviderResilienceConfig{
			CircuitBreakerEnabled:  false,
			MaxInflightPerProvider: 4,
			AdaptiveLimiterEnabled: true,
			AdaptiveMinInflight:    1,
			AdaptiveMaxInflight:    4,
			AdaptiveSuccessWindow:  2,
			AdaptiveAdditiveStep:   1,
			AdaptiveBackoffFactor:  0.50,
		},
	})

	manager.recordProviderResult("codex", &Error{Code: "upstream_503", Message: "transient", HTTPStatus: http.StatusServiceUnavailable}, 120*time.Millisecond)
	metrics := manager.ResilienceMetricsSnapshot()
	if got := metrics["cliproxy_provider_adaptive_decrease_total"]; got != 1 {
		t.Fatalf("expected adaptive decrease total=1, got %d", got)
	}
	if got := metrics["cliproxy_provider_adaptive_limit_sum"]; got != 2 {
		t.Fatalf("expected adaptive limit sum=2 after one backoff, got %d", got)
	}
	if got := metrics["cliproxy_provider_adaptive_provider_count"]; got != 1 {
		t.Fatalf("expected adaptive provider count=1, got %d", got)
	}
	if got := metrics["cliproxy_provider_adaptive_enabled"]; got != 1 {
		t.Fatalf("expected adaptive enabled flag=1, got %d", got)
	}

	release1, err := manager.acquireProviderPermit("codex")
	if err != nil {
		t.Fatalf("first permit should pass after adaptive backoff, got %v", err)
	}
	release2, err := manager.acquireProviderPermit("codex")
	if err != nil {
		release1()
		t.Fatalf("second permit should pass after adaptive backoff, got %v", err)
	}
	_, err = manager.acquireProviderPermit("codex")
	if err == nil {
		release2()
		release1()
		t.Fatal("expected third permit to fail due adaptive inflight cap=2")
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Code != "provider_backpressure" {
		release2()
		release1()
		t.Fatalf("expected provider_backpressure on third permit, got %v", err)
	}
	release2()
	release1()
}

func TestManagerAdaptiveLimiterRecoversOnSuccessWindow(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		ProviderResilience: internalconfig.ProviderResilienceConfig{
			CircuitBreakerEnabled:  false,
			MaxInflightPerProvider: 4,
			AdaptiveLimiterEnabled: true,
			AdaptiveMinInflight:    1,
			AdaptiveMaxInflight:    4,
			AdaptiveSuccessWindow:  2,
			AdaptiveAdditiveStep:   1,
			AdaptiveBackoffFactor:  0.50,
		},
	})

	manager.recordProviderResult("codex", &Error{Code: "upstream_503", Message: "transient", HTTPStatus: http.StatusServiceUnavailable}, 100*time.Millisecond)
	manager.recordProviderResult("codex", nil, 50*time.Millisecond)
	manager.recordProviderResult("codex", nil, 50*time.Millisecond)

	metrics := manager.ResilienceMetricsSnapshot()
	if got := metrics["cliproxy_provider_adaptive_limit_sum"]; got != 3 {
		t.Fatalf("expected adaptive limit sum=3 after two successes, got %d", got)
	}
	if got := metrics["cliproxy_provider_adaptive_increase_total"]; got != 1 {
		t.Fatalf("expected adaptive increase total=1, got %d", got)
	}

	manager.recordProviderResult("codex", nil, 50*time.Millisecond)
	manager.recordProviderResult("codex", nil, 50*time.Millisecond)
	metrics = manager.ResilienceMetricsSnapshot()
	if got := metrics["cliproxy_provider_adaptive_limit_sum"]; got != 4 {
		t.Fatalf("expected adaptive limit sum to recover to max(4), got %d", got)
	}
	if got := metrics["cliproxy_provider_adaptive_increase_total"]; got != 2 {
		t.Fatalf("expected adaptive increase total=2 at max recovery, got %d", got)
	}
}

func TestManagerAdaptiveLimiterScopedProviders(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		ProviderResilience: internalconfig.ProviderResilienceConfig{
			CircuitBreakerEnabled:  false,
			MaxInflightPerProvider: 4,
			AdaptiveLimiterEnabled: true,
			AdaptiveProviders:      []string{"gemini"},
			AdaptiveMinInflight:    1,
			AdaptiveMaxInflight:    4,
			AdaptiveSuccessWindow:  2,
			AdaptiveAdditiveStep:   1,
			AdaptiveBackoffFactor:  0.50,
		},
	})

	manager.recordProviderResult("codex", &Error{Code: "upstream_503", Message: "transient", HTTPStatus: http.StatusServiceUnavailable}, 150*time.Millisecond)
	metrics := manager.ResilienceMetricsSnapshot()
	if got := metrics["cliproxy_provider_adaptive_decrease_total"]; got != 0 {
		t.Fatalf("expected adaptive decrease total=0 for provider out of scope, got %d", got)
	}
	if got := metrics["cliproxy_provider_adaptive_provider_count"]; got != 0 {
		t.Fatalf("expected adaptive provider count=0 for provider out of scope, got %d", got)
	}

	releases := make([]func(), 0, 4)
	for i := 0; i < 4; i++ {
		release, err := manager.acquireProviderPermit("codex")
		if err != nil {
			t.Fatalf("permit %d should pass with static cap=4, got %v", i+1, err)
		}
		releases = append(releases, release)
	}
	_, err := manager.acquireProviderPermit("codex")
	if err == nil {
		t.Fatal("expected fifth permit to fail at static inflight cap=4")
	}
	for i := len(releases) - 1; i >= 0; i-- {
		releases[i]()
	}
}

func TestManagerAdaptiveLimiterAutoRollbackByErrorRate(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		ProviderResilience: internalconfig.ProviderResilienceConfig{
			CircuitBreakerEnabled:          false,
			MaxInflightPerProvider:         4,
			AdaptiveLimiterEnabled:         true,
			AdaptiveProviders:              []string{"codex"},
			AdaptiveMinInflight:            1,
			AdaptiveMaxInflight:            4,
			AdaptiveSuccessWindow:          2,
			AdaptiveAdditiveStep:           1,
			AdaptiveBackoffFactor:          0.50,
			AdaptiveAutoRollbackEnabled:    true,
			AdaptiveAutoRollbackWindowMS:   60000,
			AdaptiveAutoRollbackMinSamples: 5,
			AdaptiveAutoRollbackMaxP95MS:   5000,
			AdaptiveAutoRollbackMaxErrRate: 0.30,
			AdaptiveAutoRollbackMax429Rate: 0.90,
			AdaptiveAutoRollbackMax5xxRate: 0.90,
		},
	})

	transientErr := &Error{Code: "upstream_503", Message: "transient", HTTPStatus: http.StatusServiceUnavailable}
	manager.recordProviderResult("codex", transientErr, 100*time.Millisecond)
	manager.recordProviderResult("codex", transientErr, 100*time.Millisecond)
	manager.recordProviderResult("codex", nil, 80*time.Millisecond)
	manager.recordProviderResult("codex", nil, 80*time.Millisecond)
	manager.recordProviderResult("codex", nil, 80*time.Millisecond)

	metrics := manager.ResilienceMetricsSnapshot()
	if got := metrics["cliproxy_provider_adaptive_rollback_total"]; got != 1 {
		t.Fatalf("expected adaptive rollback total=1, got %d", got)
	}
	if got := metrics["cliproxy_provider_adaptive_rollback_active"]; got != 1 {
		t.Fatalf("expected adaptive rollback active=1, got %d", got)
	}
	if got := metrics["cliproxy_provider_adaptive_provider_count"]; got != 0 {
		t.Fatalf("expected adaptive provider count reset to 0 after rollback, got %d", got)
	}

	releases := make([]func(), 0, 4)
	for i := 0; i < 4; i++ {
		release, err := manager.acquireProviderPermit("codex")
		if err != nil {
			t.Fatalf("permit %d should pass using static cap=4 while rollback is active, got %v", i+1, err)
		}
		releases = append(releases, release)
	}
	_, err := manager.acquireProviderPermit("codex")
	if err == nil {
		t.Fatal("expected fifth permit to fail at static cap=4")
	}
	for i := len(releases) - 1; i >= 0; i-- {
		releases[i]()
	}
}
