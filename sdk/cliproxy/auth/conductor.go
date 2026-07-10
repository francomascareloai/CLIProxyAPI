package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// ProviderExecutor defines the contract required by Manager to execute provider calls.
type ProviderExecutor interface {
	// Identifier returns the provider key handled by this executor.
	Identifier() string
	// Execute handles non-streaming execution and returns the provider response payload.
	Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	// ExecuteStream handles streaming execution and returns a StreamResult containing
	// upstream headers and a channel of provider chunks.
	ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
	// Refresh attempts to refresh provider credentials and returns the updated auth state.
	Refresh(ctx context.Context, auth *Auth) (*Auth, error)
	// CountTokens returns the token count for the given request.
	CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	// HttpRequest injects provider credentials into the supplied HTTP request and executes it.
	// Callers must close the response body when non-nil.
	HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error)
}

// ExecutionSessionCloser allows executors to release per-session runtime resources.
type ExecutionSessionCloser interface {
	CloseExecutionSession(sessionID string)
}

const (
	// CloseAllExecutionSessionsID asks an executor to release all active execution sessions.
	// Executors that do not support this marker may ignore it.
	CloseAllExecutionSessionsID = "__all_execution_sessions__"
)

// RefreshEvaluator allows runtime state to override refresh decisions.
type RefreshEvaluator interface {
	ShouldRefresh(now time.Time, auth *Auth) bool
}

const (
	refreshCheckInterval  = 5 * time.Second
	refreshMaxConcurrency = 16
	refreshPendingBackoff = time.Minute
	refreshFailureBackoff = 5 * time.Minute
	quotaBackoffBase      = time.Second
	quotaBackoffMax       = 30 * time.Minute

	// deactivatedWorkspaceBackoff is a long quarantine window used when the upstream
	// explicitly indicates the credential's workspace is deactivated.
	deactivatedWorkspaceBackoff = 30 * 24 * time.Hour

	// refreshTokenReusedBackoff is a long quarantine window used when an OAuth refresh
	// fails with refresh_token_reused (requires re-login).
	refreshTokenReusedBackoff = 30 * 24 * time.Hour

	refreshLockStaleAfter = 10 * time.Minute
)

var adaptiveLatencyBucketsMS = [...]int64{
	50,
	100,
	200,
	400,
	800,
	1200,
	2000,
	3000,
	5000,
	8000,
	12000,
	20000,
	30000,
}

var quotaCooldownDisabled atomic.Bool

// SetQuotaCooldownDisabled toggles quota cooldown scheduling globally.
func SetQuotaCooldownDisabled(disable bool) {
	quotaCooldownDisabled.Store(disable)
}

func quotaCooldownDisabledForAuth(auth *Auth) bool {
	if auth != nil {
		if override, ok := auth.DisableCoolingOverride(); ok {
			return override
		}
	}
	return quotaCooldownDisabled.Load()
}

// Result captures execution outcome used to adjust auth state.
type Result struct {
	// AuthID references the auth that produced this result.
	AuthID string
	// Provider is copied for convenience when emitting hooks.
	Provider string
	// Model is the upstream model identifier used for the request.
	Model string
	// Success marks whether the execution succeeded.
	Success bool
	// RetryAfter carries a provider supplied retry hint (e.g. 429 retryDelay).
	RetryAfter *time.Duration
	// Error describes the failure when Success is false.
	Error *Error
}

// Selector chooses an auth candidate for execution.
type Selector interface {
	Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error)
}

// Hook captures lifecycle callbacks for observing auth changes.
type Hook interface {
	// OnAuthRegistered fires when a new auth is registered.
	OnAuthRegistered(ctx context.Context, auth *Auth)
	// OnAuthUpdated fires when an existing auth changes state.
	OnAuthUpdated(ctx context.Context, auth *Auth)
	// OnResult fires when execution result is recorded.
	OnResult(ctx context.Context, result Result)
}

// NoopHook provides optional hook defaults.
type NoopHook struct{}

// OnAuthRegistered implements Hook.
func (NoopHook) OnAuthRegistered(context.Context, *Auth) {}

// OnAuthUpdated implements Hook.
func (NoopHook) OnAuthUpdated(context.Context, *Auth) {}

// OnResult implements Hook.
func (NoopHook) OnResult(context.Context, Result) {}

// Manager orchestrates auth lifecycle, selection, execution, and persistence.
type Manager struct {
	store     Store
	executors map[string]ProviderExecutor
	selector  Selector
	hook      Hook
	mu        sync.RWMutex
	auths     map[string]*Auth
	scheduler *authScheduler
	// providerOffsets tracks per-model provider rotation state for multi-provider routing.
	providerOffsets map[string]int

	// refreshSF deduplicates concurrent refresh attempts per auth (single-flight).
	refreshSF singleflight.Group

	// Retry controls request retry behavior.
	requestRetry        atomic.Int32
	maxRetryCredentials atomic.Int32
	maxRetryInterval    atomic.Int64

	// oauthModelAlias stores global OAuth model alias mappings (alias -> upstream name) keyed by channel.
	oauthModelAlias atomic.Value

	// apiKeyModelAlias caches resolved model alias mappings for API-key auths.
	// Keyed by auth.ID, value is alias(lower) -> upstream model (including suffix).
	apiKeyModelAlias atomic.Value

	// modelPoolOffsets tracks per-auth alias pool rotation state.
	modelPoolOffsets map[string]int

	// runtimeConfig stores the latest application config for request-time decisions.
	// It is initialized in NewManager; never Load() before first Store().
	runtimeConfig atomic.Value

	// Optional HTTP RoundTripper provider injected by host.
	rtProvider RoundTripperProvider

	// providerLimiter controls per-provider inflight backpressure.
	providerLimiterMu sync.Mutex
	providerLimiter   map[string]chan struct{}
	// providerAdaptive tracks dynamic inflight window when adaptive limiter is enabled.
	providerAdaptiveLimit         map[string]int
	providerAdaptiveSuccessStreak map[string]int
	providerAdaptiveRollbackUntil map[string]time.Time
	providerAdaptiveSLOWindow     map[string]*providerAdaptiveSLOState
	// providerCircuit stores per-provider breaker state.
	providerCircuitMu sync.Mutex
	providerCircuit   map[string]*providerCircuitState
	// providerResilience counters for observability.
	providerBackpressureCount atomic.Uint64
	providerCircuitOpenCount  atomic.Uint64
	providerAdaptiveIncCount  atomic.Uint64
	providerAdaptiveDecCount  atomic.Uint64
	providerAdaptiveRollback  atomic.Uint64

	// conductor counters for hot-path stage observability.
	conductorSelectCount   atomic.Uint64
	conductorPermitCount   atomic.Uint64
	conductorRetryCount    atomic.Uint64
	conductorSelectLatency atomic.Uint64
	conductorPermitLatency atomic.Uint64
	conductorRetryWaitNS   atomic.Uint64

	// Auto refresh state
	refreshCancel    context.CancelFunc
	refreshSemaphore chan struct{}
}

type providerResilienceSettings struct {
	circuitBreakerEnabled bool
	failureThreshold      int
	halfOpenMaxRequests   int
	openStateDuration     time.Duration
	maxInflight           int
	adaptiveLimiter       bool
	adaptiveProviders     map[string]struct{}
	adaptiveMinInflight   int
	adaptiveMaxInflight   int
	adaptiveSuccessWindow int
	adaptiveAdditiveStep  int
	adaptiveBackoffFactor float64
	adaptiveAutoRollback  bool
	adaptiveRollbackWin   time.Duration
	adaptiveRollbackMinN  int
	adaptiveRollbackP95MS int
	adaptiveRollbackErr   float64
	adaptiveRollback429   float64
	adaptiveRollback5xx   float64
}

type providerCircuitState struct {
	consecutiveFailures int
	openUntil           time.Time
	halfOpenRemaining   int
}

type providerAdaptiveSLOState struct {
	windowStart    time.Time
	samples        uint64
	errors         uint64
	http429        uint64
	http5xx        uint64
	latencyBuckets [len(adaptiveLatencyBucketsMS)]uint64
}

// NewManager constructs a manager with optional custom selector and hook.
func NewManager(store Store, selector Selector, hook Hook) *Manager {
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	if hook == nil {
		hook = NoopHook{}
	}
	manager := &Manager{
		store:                         store,
		executors:                     make(map[string]ProviderExecutor),
		selector:                      selector,
		hook:                          hook,
		auths:                         make(map[string]*Auth),
		providerOffsets:               make(map[string]int),
		modelPoolOffsets:              make(map[string]int),
		providerLimiter:               make(map[string]chan struct{}),
		providerAdaptiveLimit:         make(map[string]int),
		providerAdaptiveSuccessStreak: make(map[string]int),
		providerAdaptiveRollbackUntil: make(map[string]time.Time),
		providerAdaptiveSLOWindow:     make(map[string]*providerAdaptiveSLOState),
		providerCircuit:               make(map[string]*providerCircuitState),
		refreshSemaphore: make(chan struct{}, refreshMaxConcurrency),
	}
	// atomic.Value requires non-nil initial value.
	manager.runtimeConfig.Store(&internalconfig.Config{})
	manager.apiKeyModelAlias.Store(apiKeyModelAliasTable(nil))
	manager.scheduler = newAuthScheduler(selector)
	return manager
}

func isBuiltInSelector(selector Selector) bool {
	switch selector.(type) {
	case *RoundRobinSelector, *FillFirstSelector:
		return true
	default:
		return false
	}
}

func (m *Manager) syncSchedulerFromSnapshot(auths []*Auth) {
	if m == nil || m.scheduler == nil {
		return
	}
	m.scheduler.rebuild(auths)
}

func (m *Manager) syncScheduler() {
	if m == nil || m.scheduler == nil {
		return
	}
	m.syncSchedulerFromSnapshot(m.snapshotAuths())
}

// RefreshSchedulerEntry re-upserts a single auth into the scheduler so that its
// supportedModelSet is rebuilt from the current global model registry state.
// This must be called after models have been registered for a newly added auth,
// because the initial scheduler.upsertAuth during Register/Update runs before
// registerModelsForAuth and therefore snapshots an empty model set.
func (m *Manager) RefreshSchedulerEntry(authID string) {
	if m == nil || m.scheduler == nil || authID == "" {
		return
	}
	m.mu.RLock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		m.mu.RUnlock()
		return
	}
	snapshot := auth.Clone()
	m.mu.RUnlock()
	m.scheduler.upsertAuth(snapshot)
}

func (m *Manager) SetSelector(selector Selector) {
	if m == nil {
		return
	}
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	m.mu.Lock()
	m.selector = selector
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.setSelector(selector)
		m.syncScheduler()
	}
}

// SetStore swaps the underlying persistence store.
func (m *Manager) SetStore(store Store) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = store
}

// SetRoundTripperProvider register a provider that returns a per-auth RoundTripper.
func (m *Manager) SetRoundTripperProvider(p RoundTripperProvider) {
	m.mu.Lock()
	m.rtProvider = p
	m.mu.Unlock()
}

// SetConfig updates the runtime config snapshot used by request-time helpers.
// Callers should provide the latest config on reload so per-credential alias mapping stays in sync.
func (m *Manager) SetConfig(cfg *internalconfig.Config) {
	if m == nil {
		return
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.runtimeConfig.Store(cfg)
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
}

func (m *Manager) lookupAPIKeyUpstreamModel(authID, requestedModel string) string {
	if m == nil {
		return ""
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ""
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return ""
	}
	table, _ := m.apiKeyModelAlias.Load().(apiKeyModelAliasTable)
	if table == nil {
		return ""
	}
	byAlias := table[authID]
	if len(byAlias) == 0 {
		return ""
	}
	key := strings.ToLower(thinking.ParseSuffix(requestedModel).ModelName)
	if key == "" {
		key = strings.ToLower(requestedModel)
	}
	resolved := strings.TrimSpace(byAlias[key])
	if resolved == "" {
		return ""
	}
	return preserveRequestedModelSuffix(requestedModel, resolved)
}

func isAPIKeyAuth(auth *Auth) bool {
	if auth == nil {
		return false
	}
	kind, _ := auth.AccountInfo()
	return strings.EqualFold(strings.TrimSpace(kind), "api_key")
}

func isOpenAICompatAPIKeyAuth(auth *Auth) bool {
	if !isAPIKeyAuth(auth) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return true
	}
	if auth.Attributes == nil {
		return false
	}
	return strings.TrimSpace(auth.Attributes["compat_name"]) != ""
}

func openAICompatProviderKey(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if providerKey := strings.TrimSpace(auth.Attributes["provider_key"]); providerKey != "" {
			return strings.ToLower(providerKey)
		}
		if compatName := strings.TrimSpace(auth.Attributes["compat_name"]); compatName != "" {
			return strings.ToLower(compatName)
		}
	}
	return strings.ToLower(strings.TrimSpace(auth.Provider))
}

func openAICompatModelPoolKey(auth *Auth, requestedModel string) string {
	base := strings.TrimSpace(thinking.ParseSuffix(requestedModel).ModelName)
	if base == "" {
		base = strings.TrimSpace(requestedModel)
	}
	return strings.ToLower(strings.TrimSpace(auth.ID)) + "|" + openAICompatProviderKey(auth) + "|" + strings.ToLower(base)
}

func (m *Manager) nextModelPoolOffset(key string, size int) int {
	if m == nil || size <= 1 {
		return 0
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.modelPoolOffsets == nil {
		m.modelPoolOffsets = make(map[string]int)
	}
	offset := m.modelPoolOffsets[key]
	if offset >= 2_147_483_640 {
		offset = 0
	}
	m.modelPoolOffsets[key] = offset + 1
	if size <= 0 {
		return 0
	}
	return offset % size
}

func rotateStrings(values []string, offset int) []string {
	if len(values) <= 1 {
		return values
	}
	if offset <= 0 {
		out := make([]string, len(values))
		copy(out, values)
		return out
	}
	offset = offset % len(values)
	out := make([]string, 0, len(values))
	out = append(out, values[offset:]...)
	out = append(out, values[:offset]...)
	return out
}

func (m *Manager) resolveOpenAICompatUpstreamModelPool(auth *Auth, requestedModel string) []string {
	if m == nil || !isOpenAICompatAPIKeyAuth(auth) {
		return nil
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return nil
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	providerKey := ""
	compatName := ""
	if auth.Attributes != nil {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider)
	if entry == nil {
		return nil
	}
	return resolveModelAliasPoolFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func preserveRequestedModelSuffix(requestedModel, resolved string) string {
	return preserveResolvedModelSuffix(resolved, thinking.ParseSuffix(requestedModel))
}

func (m *Manager) executionModelCandidates(auth *Auth, routeModel string) []string {
	return m.prepareExecutionModels(auth, routeModel)
}

func (m *Manager) prepareExecutionModels(auth *Auth, routeModel string) []string {
	requestedModel := rewriteModelForAuth(routeModel, auth)
	requestedModel = m.applyOAuthModelAlias(auth, requestedModel)
	if pool := m.resolveOpenAICompatUpstreamModelPool(auth, requestedModel); len(pool) > 0 {
		if len(pool) == 1 {
			return pool
		}
		offset := m.nextModelPoolOffset(openAICompatModelPoolKey(auth, requestedModel), len(pool))
		return rotateStrings(pool, offset)
	}
	resolved := m.applyAPIKeyModelAlias(auth, requestedModel)
	if strings.TrimSpace(resolved) == "" {
		resolved = requestedModel
	}
	return []string{resolved}
}

func discardStreamChunks(ch <-chan cliproxyexecutor.StreamChunk) {
	if ch == nil {
		return
	}
	go func() {
		for range ch {
		}
	}()
}

func readStreamBootstrap(ctx context.Context, ch <-chan cliproxyexecutor.StreamChunk) ([]cliproxyexecutor.StreamChunk, bool, error) {
	if ch == nil {
		return nil, true, nil
	}
	buffered := make([]cliproxyexecutor.StreamChunk, 0, 1)
	for {
		var (
			chunk cliproxyexecutor.StreamChunk
			ok    bool
		)
		if ctx != nil {
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case chunk, ok = <-ch:
			}
		} else {
			chunk, ok = <-ch
		}
		if !ok {
			return buffered, true, nil
		}
		if chunk.Err != nil {
			return nil, false, chunk.Err
		}
		buffered = append(buffered, chunk)
		if len(chunk.Payload) > 0 {
			return buffered, false, nil
		}
	}
}

func (m *Manager) wrapStreamResult(ctx context.Context, auth *Auth, provider, routeModel string, headers http.Header, buffered []cliproxyexecutor.StreamChunk, remaining <-chan cliproxyexecutor.StreamChunk) *cliproxyexecutor.StreamResult {
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		var failed bool
		forward := true
		emit := func(chunk cliproxyexecutor.StreamChunk) bool {
			if chunk.Err != nil && !failed {
				failed = true
				rerr := &Error{Message: chunk.Err.Error()}
				if se, ok := errors.AsType[cliproxyexecutor.StatusError](chunk.Err); ok && se != nil {
					rerr.HTTPStatus = se.StatusCode()
				}
				m.MarkResult(ctx, Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: false, Error: rerr})
			}
			if !forward {
				return false
			}
			if ctx == nil {
				out <- chunk
				return true
			}
			select {
			case <-ctx.Done():
				forward = false
				return false
			case out <- chunk:
				return true
			}
		}
		for _, chunk := range buffered {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		for chunk := range remaining {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		if !failed {
			m.MarkResult(ctx, Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: true})
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}
}

func (m *Manager) executeStreamWithModelPool(ctx context.Context, executor ProviderExecutor, auth *Auth, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, routeModel string) (*cliproxyexecutor.StreamResult, error) {
	if executor == nil {
		return nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	execModels := m.prepareExecutionModels(auth, routeModel)
	var lastErr error
	for idx, execModel := range execModels {
		execReq := req
		execReq.Model = execModel
		streamResult, errStream := executor.ExecuteStream(ctx, auth, execReq, opts)
		if errStream != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				return nil, errCtx
			}
			rerr := &Error{Message: errStream.Error()}
			if se, ok := errors.AsType[cliproxyexecutor.StatusError](errStream); ok && se != nil {
				rerr.HTTPStatus = se.StatusCode()
			}
			result := Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: false, Error: rerr}
			result.RetryAfter = retryAfterFromError(errStream)
			m.MarkResult(ctx, result)
			if isRequestInvalidError(errStream) {
				return nil, errStream
			}
			lastErr = errStream
			continue
		}

		buffered, closed, bootstrapErr := readStreamBootstrap(ctx, streamResult.Chunks)
		if bootstrapErr != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				discardStreamChunks(streamResult.Chunks)
				return nil, errCtx
			}
			if isRequestInvalidError(bootstrapErr) {
				rerr := &Error{Message: bootstrapErr.Error()}
				if se, ok := errors.AsType[cliproxyexecutor.StatusError](bootstrapErr); ok && se != nil {
					rerr.HTTPStatus = se.StatusCode()
				}
				result := Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: false, Error: rerr}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				m.MarkResult(ctx, result)
				discardStreamChunks(streamResult.Chunks)
				return nil, bootstrapErr
			}
			if idx < len(execModels)-1 {
				rerr := &Error{Message: bootstrapErr.Error()}
				if se, ok := errors.AsType[cliproxyexecutor.StatusError](bootstrapErr); ok && se != nil {
					rerr.HTTPStatus = se.StatusCode()
				}
				result := Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: false, Error: rerr}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				m.MarkResult(ctx, result)
				discardStreamChunks(streamResult.Chunks)
				lastErr = bootstrapErr
				continue
			}
			errCh := make(chan cliproxyexecutor.StreamChunk, 1)
			errCh <- cliproxyexecutor.StreamChunk{Err: bootstrapErr}
			close(errCh)
			return m.wrapStreamResult(ctx, auth.Clone(), provider, routeModel, streamResult.Headers, nil, errCh), nil
		}

		if closed && len(buffered) == 0 {
			emptyErr := &Error{Code: "empty_stream", Message: "upstream stream closed before first payload", Retryable: true}
			result := Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: false, Error: emptyErr}
			m.MarkResult(ctx, result)
			if idx < len(execModels)-1 {
				lastErr = emptyErr
				continue
			}
			errCh := make(chan cliproxyexecutor.StreamChunk, 1)
			errCh <- cliproxyexecutor.StreamChunk{Err: emptyErr}
			close(errCh)
			return m.wrapStreamResult(ctx, auth.Clone(), provider, routeModel, streamResult.Headers, nil, errCh), nil
		}

		remaining := streamResult.Chunks
		if closed {
			closedCh := make(chan cliproxyexecutor.StreamChunk)
			close(closedCh)
			remaining = closedCh
		}
		return m.wrapStreamResult(ctx, auth.Clone(), provider, routeModel, streamResult.Headers, buffered, remaining), nil
	}
	if lastErr == nil {
		lastErr = &Error{Code: "auth_not_found", Message: "no upstream model available"}
	}
	return nil, lastErr
}

func (m *Manager) rebuildAPIKeyModelAliasFromRuntimeConfig() {
	if m == nil {
		return
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rebuildAPIKeyModelAliasLocked(cfg)
}

func (m *Manager) rebuildAPIKeyModelAliasLocked(cfg *internalconfig.Config) {
	if m == nil {
		return
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	out := make(apiKeyModelAliasTable)
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		if strings.TrimSpace(auth.ID) == "" {
			continue
		}
		kind, _ := auth.AccountInfo()
		if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
			continue
		}

		byAlias := make(map[string]string)
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		switch provider {
		case "gemini":
			if entry := resolveGeminiAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "claude":
			if entry := resolveClaudeAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "codex":
			if entry := resolveCodexAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "vertex":
			if entry := resolveVertexAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		default:
			// OpenAI-compat uses config selection from auth.Attributes.
			providerKey := ""
			compatName := ""
			if auth.Attributes != nil {
				providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
				compatName = strings.TrimSpace(auth.Attributes["compat_name"])
			}
			if compatName != "" || strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
				if entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider); entry != nil {
					compileAPIKeyModelAliasForModels(byAlias, entry.Models)
				}
			}
		}

		if len(byAlias) > 0 {
			out[auth.ID] = byAlias
		}
	}

	m.apiKeyModelAlias.Store(out)
}

func compileAPIKeyModelAliasForModels[T interface {
	GetName() string
	GetAlias() string
}](out map[string]string, models []T) {
	if out == nil {
		return
	}
	for i := range models {
		alias := strings.TrimSpace(models[i].GetAlias())
		name := strings.TrimSpace(models[i].GetName())
		if alias == "" || name == "" {
			continue
		}
		aliasKey := strings.ToLower(thinking.ParseSuffix(alias).ModelName)
		if aliasKey == "" {
			aliasKey = strings.ToLower(alias)
		}
		// Config priority: first alias wins.
		if _, exists := out[aliasKey]; exists {
			continue
		}
		out[aliasKey] = name
		// Also allow direct lookup by upstream name (case-insensitive), so lookups on already-upstream
		// models remain a cheap no-op.
		nameKey := strings.ToLower(thinking.ParseSuffix(name).ModelName)
		if nameKey == "" {
			nameKey = strings.ToLower(name)
		}
		if nameKey != "" {
			if _, exists := out[nameKey]; !exists {
				out[nameKey] = name
			}
		}
		// Preserve config suffix priority by seeding a base-name lookup when name already has suffix.
		nameResult := thinking.ParseSuffix(name)
		if nameResult.HasSuffix {
			baseKey := strings.ToLower(strings.TrimSpace(nameResult.ModelName))
			if baseKey != "" {
				if _, exists := out[baseKey]; !exists {
					out[baseKey] = name
				}
			}
		}
	}
}

// SetRetryConfig updates retry attempts, credential retry limit and cooldown wait interval.
func (m *Manager) SetRetryConfig(retry int, maxRetryInterval time.Duration, maxRetryCredentials int) {
	if m == nil {
		return
	}
	if retry < 0 {
		retry = 0
	}
	if maxRetryCredentials < 0 {
		maxRetryCredentials = 0
	}
	if maxRetryInterval < 0 {
		maxRetryInterval = 0
	}
	m.requestRetry.Store(int32(retry))
	m.maxRetryCredentials.Store(int32(maxRetryCredentials))
	m.maxRetryInterval.Store(maxRetryInterval.Nanoseconds())
}

// RegisterExecutor registers a provider executor with the manager.
func (m *Manager) RegisterExecutor(executor ProviderExecutor) {
	if executor == nil {
		return
	}
	provider := strings.TrimSpace(executor.Identifier())
	if provider == "" {
		return
	}

	var replaced ProviderExecutor
	m.mu.Lock()
	replaced = m.executors[provider]
	m.executors[provider] = executor
	m.mu.Unlock()

	if replaced == nil || replaced == executor {
		return
	}
	if closer, ok := replaced.(ExecutionSessionCloser); ok && closer != nil {
		closer.CloseExecutionSession(CloseAllExecutionSessionsID)
	}
}

// UnregisterExecutor removes the executor associated with the provider key.
func (m *Manager) UnregisterExecutor(provider string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return
	}
	m.mu.Lock()
	delete(m.executors, provider)
	m.mu.Unlock()
}

// Register inserts a new auth entry into the manager.
func (m *Manager) Register(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil {
		return nil, nil
	}
	if auth.ID == "" {
		auth.ID = uuid.NewString()
	}
	auth.EnsureIndex()
	authClone := auth.Clone()
	m.mu.Lock()
	m.auths[auth.ID] = authClone
	m.mu.Unlock()
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(authClone)
	}
	_ = m.persist(ctx, auth)
	m.hook.OnAuthRegistered(ctx, auth.Clone())
	return auth.Clone(), nil
}

// Update replaces an existing auth entry and notifies hooks.
func (m *Manager) Update(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil || auth.ID == "" {
		return nil, nil
	}
	m.mu.Lock()
	if existing, ok := m.auths[auth.ID]; ok && existing != nil {
		if !auth.indexAssigned && auth.Index == "" {
			auth.Index = existing.Index
			auth.indexAssigned = existing.indexAssigned
		}
		if len(auth.ModelStates) == 0 && len(existing.ModelStates) > 0 {
			auth.ModelStates = existing.ModelStates
		}
	}
	auth.EnsureIndex()
	authClone := auth.Clone()
	m.auths[auth.ID] = authClone
	m.mu.Unlock()
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(authClone)
	}
	_ = m.persist(ctx, auth)
	m.hook.OnAuthUpdated(ctx, auth.Clone())
	return auth.Clone(), nil
}

// Load resets manager state from the backing store.
func (m *Manager) Load(ctx context.Context) error {
	m.mu.Lock()
	if m.store == nil {
		m.mu.Unlock()
		return nil
	}
	items, err := m.store.List(ctx)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	m.auths = make(map[string]*Auth, len(items))
	for _, auth := range items {
		if auth == nil || auth.ID == "" {
			continue
		}
		auth.EnsureIndex()
		m.auths[auth.ID] = auth.Clone()
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.rebuildAPIKeyModelAliasLocked(cfg)
	m.mu.Unlock()
	m.syncScheduler()
	return nil
}

// Execute performs a non-streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) Execute(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	_, maxRetryCredentials, maxWait := m.retrySettings()

	var lastErr error
	for attempt := 0; ; attempt++ {
		resp, errExec := m.executeMixedOnce(ctx, normalized, req, opts, maxRetryCredentials)
		if errExec == nil {
			return resp, nil
		}
		lastErr = errExec
		wait, shouldRetry := m.shouldRetryAfterError(errExec, attempt, normalized, req.Model, maxWait)
		if !shouldRetry {
			break
		}
		m.conductorRetryCount.Add(1)
		m.conductorRetryWaitNS.Add(uint64(wait.Nanoseconds()))
		if errWait := waitForCooldown(ctx, wait); errWait != nil {
			return cliproxyexecutor.Response{}, errWait
		}
	}
	if lastErr != nil {
		return cliproxyexecutor.Response{}, lastErr
	}
	return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
}

// ExecuteCount performs a non-streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteCount(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	_, maxRetryCredentials, maxWait := m.retrySettings()

	var lastErr error
	for attempt := 0; ; attempt++ {
		resp, errExec := m.executeCountMixedOnce(ctx, normalized, req, opts, maxRetryCredentials)
		if errExec == nil {
			return resp, nil
		}
		lastErr = errExec
		wait, shouldRetry := m.shouldRetryAfterError(errExec, attempt, normalized, req.Model, maxWait)
		if !shouldRetry {
			break
		}
		m.conductorRetryCount.Add(1)
		m.conductorRetryWaitNS.Add(uint64(wait.Nanoseconds()))
		if errWait := waitForCooldown(ctx, wait); errWait != nil {
			return cliproxyexecutor.Response{}, errWait
		}
	}
	if lastErr != nil {
		return cliproxyexecutor.Response{}, lastErr
	}
	return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
}

// ExecuteStream performs a streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteStream(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return nil, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	_, maxRetryCredentials, maxWait := m.retrySettings()

	var lastErr error
	for attempt := 0; ; attempt++ {
		result, errStream := m.executeStreamMixedOnce(ctx, normalized, req, opts, maxRetryCredentials)
		if errStream == nil {
			return result, nil
		}
		lastErr = errStream
		wait, shouldRetry := m.shouldRetryAfterError(errStream, attempt, normalized, req.Model, maxWait)
		if !shouldRetry {
			break
		}
		m.conductorRetryCount.Add(1)
		m.conductorRetryWaitNS.Add(uint64(wait.Nanoseconds()))
		if errWait := waitForCooldown(ctx, wait); errWait != nil {
			return nil, errWait
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
}

func (m *Manager) executeMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int) (cliproxyexecutor.Response, error) {
	if len(providers) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)
	tried := make(map[string]struct{})
	var lastErr error
	for {
		if maxRetryCredentials > 0 && len(tried) >= maxRetryCredentials {
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		selectStartedAt := time.Now()
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, routeModel, opts, tried)
		m.conductorSelectCount.Add(1)
		m.conductorSelectLatency.Add(uint64(time.Since(selectStartedAt).Nanoseconds()))
		if errPick != nil {
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, errPick
		}

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, req.Model)
		publishSelectedAuthMetadata(opts.Metadata, auth.ID)

		tried[auth.ID] = struct{}{}
		permitStartedAt := time.Now()
		releaseProviderPermit, errPermit := m.acquireProviderPermit(provider)
		m.conductorPermitCount.Add(1)
		m.conductorPermitLatency.Add(uint64(time.Since(permitStartedAt).Nanoseconds()))
		if errPermit != nil {
			lastErr = errPermit
			continue
		}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		models := m.prepareExecutionModels(auth, routeModel)
		var authErr error
		for _, upstreamModel := range models {
			execReq := req
			execReq.Model = upstreamModel
			execStartedAt := time.Now()
			resp, errExec := executor.Execute(execCtx, auth, execReq, opts)
			execLatency := time.Since(execStartedAt)
			m.recordProviderResult(provider, errExec, execLatency)
			result := Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: errExec == nil}
			if errExec != nil {
				if errCtx := execCtx.Err(); errCtx != nil {
					releaseProviderPermit()
					return cliproxyexecutor.Response{}, errCtx
				}
				result.Error = &Error{Message: errExec.Error()}
				if se, ok := errors.AsType[cliproxyexecutor.StatusError](errExec); ok && se != nil {
					result.Error.HTTPStatus = se.StatusCode()
				}
				result.Error.Code = extractErrorCode(errExec)
				if ra := retryAfterFromError(errExec); ra != nil {
					result.RetryAfter = ra
				}
				m.MarkResult(execCtx, result)
				if isRequestInvalidError(errExec) {
					releaseProviderPermit()
					return cliproxyexecutor.Response{}, errExec
				}
				authErr = errExec
				continue
			}
			releaseProviderPermit()
			m.MarkResult(execCtx, result)
			return resp, nil
		}
		releaseProviderPermit()
		if authErr != nil {
			if isRequestInvalidError(authErr) {
				return cliproxyexecutor.Response{}, authErr
			}
			lastErr = authErr
			continue
		}
	}
}

func (m *Manager) executeCountMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int) (cliproxyexecutor.Response, error) {
	if len(providers) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)
	tried := make(map[string]struct{})
	var lastErr error
	for {
		if maxRetryCredentials > 0 && len(tried) >= maxRetryCredentials {
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		selectStartedAt := time.Now()
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, routeModel, opts, tried)
		m.conductorSelectCount.Add(1)
		m.conductorSelectLatency.Add(uint64(time.Since(selectStartedAt).Nanoseconds()))
		if errPick != nil {
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, errPick
		}

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, req.Model)
		publishSelectedAuthMetadata(opts.Metadata, auth.ID)

		tried[auth.ID] = struct{}{}
		permitStartedAt := time.Now()
		releaseProviderPermit, errPermit := m.acquireProviderPermit(provider)
		m.conductorPermitCount.Add(1)
		m.conductorPermitLatency.Add(uint64(time.Since(permitStartedAt).Nanoseconds()))
		if errPermit != nil {
			lastErr = errPermit
			continue
		}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		models := m.prepareExecutionModels(auth, routeModel)
		var authErr error
		for _, upstreamModel := range models {
			execReq := req
			execReq.Model = upstreamModel
			execStartedAt := time.Now()
			resp, errExec := executor.CountTokens(execCtx, auth, execReq, opts)
			execLatency := time.Since(execStartedAt)
			m.recordProviderResult(provider, errExec, execLatency)
			result := Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: errExec == nil}
			if errExec != nil {
				if errCtx := execCtx.Err(); errCtx != nil {
					releaseProviderPermit()
					return cliproxyexecutor.Response{}, errCtx
				}
				result.Error = &Error{Message: errExec.Error()}
				if se, ok := errors.AsType[cliproxyexecutor.StatusError](errExec); ok && se != nil {
					result.Error.HTTPStatus = se.StatusCode()
				}
				result.Error.Code = extractErrorCode(errExec)
				if ra := retryAfterFromError(errExec); ra != nil {
					result.RetryAfter = ra
				}
				m.hook.OnResult(execCtx, result)
				if isRequestInvalidError(errExec) {
					releaseProviderPermit()
					return cliproxyexecutor.Response{}, errExec
				}
				authErr = errExec
				continue
			}
			releaseProviderPermit()
			m.hook.OnResult(execCtx, result)
			return resp, nil
		}
		releaseProviderPermit()
		if authErr != nil {
			if isRequestInvalidError(authErr) {
				return cliproxyexecutor.Response{}, authErr
			}
			lastErr = authErr
			continue
		}
	}
}

func (m *Manager) executeStreamMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int) (*cliproxyexecutor.StreamResult, error) {
	if len(providers) == 0 {
		return nil, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)
	tried := make(map[string]struct{})
	var lastErr error
	for {
		if maxRetryCredentials > 0 && len(tried) >= maxRetryCredentials {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		selectStartedAt := time.Now()
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, routeModel, opts, tried)
		m.conductorSelectCount.Add(1)
		m.conductorSelectLatency.Add(uint64(time.Since(selectStartedAt).Nanoseconds()))
		if errPick != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, errPick
		}

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, req.Model)
		publishSelectedAuthMetadata(opts.Metadata, auth.ID)

		tried[auth.ID] = struct{}{}
		permitStartedAt := time.Now()
		releaseProviderPermit, errPermit := m.acquireProviderPermit(provider)
		m.conductorPermitCount.Add(1)
		m.conductorPermitLatency.Add(uint64(time.Since(permitStartedAt).Nanoseconds()))
		if errPermit != nil {
			lastErr = errPermit
			continue
		}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		streamStartedAt := time.Now()
		streamResult, errStream := m.executeStreamWithModelPool(execCtx, executor, auth, provider, req, opts, routeModel)
		if errStream != nil {
			releaseProviderPermit()
			m.recordProviderResult(provider, errStream, time.Since(streamStartedAt))
			if errCtx := execCtx.Err(); errCtx != nil {
				return nil, errCtx
			}
			if isRequestInvalidError(errStream) {
				return nil, errStream
			}
			lastErr = errStream
			continue
		}
		out := make(chan cliproxyexecutor.StreamChunk)
		go func(streamCtx context.Context, streamProvider string, streamChunks <-chan cliproxyexecutor.StreamChunk, release func(), startedAt time.Time) {
			defer close(out)
			defer release()
			var failed bool
			forward := true
			for chunk := range streamChunks {
				if chunk.Err != nil && !failed {
					failed = true
					m.recordProviderResult(streamProvider, chunk.Err, time.Since(startedAt))
				}
				if !forward {
					continue
				}
				if streamCtx == nil {
					out <- chunk
					continue
				}
				select {
				case <-streamCtx.Done():
					forward = false
				case out <- chunk:
				}
			}
			if !failed {
				m.recordProviderResult(streamProvider, nil, time.Since(startedAt))
			}
		}(execCtx, provider, streamResult.Chunks, releaseProviderPermit, streamStartedAt)
		return &cliproxyexecutor.StreamResult{
			Headers: streamResult.Headers,
			Chunks:  out,
		}, nil
	}
}

func ensureRequestedModelMetadata(opts cliproxyexecutor.Options, requestedModel string) cliproxyexecutor.Options {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return opts
	}
	if hasRequestedModelMetadata(opts.Metadata) {
		return opts
	}
	if len(opts.Metadata) == 0 {
		opts.Metadata = map[string]any{cliproxyexecutor.RequestedModelMetadataKey: requestedModel}
		return opts
	}
	meta := make(map[string]any, len(opts.Metadata)+1)
	for k, v := range opts.Metadata {
		meta[k] = v
	}
	meta[cliproxyexecutor.RequestedModelMetadataKey] = requestedModel
	opts.Metadata = meta
	return opts
}

func hasRequestedModelMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case []byte:
		return strings.TrimSpace(string(v)) != ""
	default:
		return false
	}
}

func pinnedAuthIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.PinnedAuthMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch val := raw.(type) {
	case string:
		return strings.TrimSpace(val)
	case []byte:
		return strings.TrimSpace(string(val))
	default:
		return ""
	}
}

func publishSelectedAuthMetadata(meta map[string]any, authID string) {
	if len(meta) == 0 {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	meta[cliproxyexecutor.SelectedAuthMetadataKey] = authID
	if callback, ok := meta[cliproxyexecutor.SelectedAuthCallbackMetadataKey].(func(string)); ok && callback != nil {
		callback(authID)
	}
}

func rewriteModelForAuth(model string, auth *Auth) string {
	if auth == nil || model == "" {
		return model
	}
	prefix := strings.TrimSpace(auth.Prefix)
	if prefix == "" {
		return model
	}
	needle := prefix + "/"
	if !strings.HasPrefix(model, needle) {
		return model
	}
	return strings.TrimPrefix(model, needle)
}

func (m *Manager) applyAPIKeyModelAlias(auth *Auth, requestedModel string) string {
	if m == nil || auth == nil {
		return requestedModel
	}

	kind, _ := auth.AccountInfo()
	if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
		return requestedModel
	}

	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return requestedModel
	}

	// Fast path: lookup per-auth mapping table (keyed by auth.ID).
	if resolved := m.lookupAPIKeyUpstreamModel(auth.ID, requestedModel); resolved != "" {
		return preventCodexDowngrade(requestedModel, resolved)
	}

	// Slow path: scan config for the matching credential entry and resolve alias.
	// This acts as a safety net if mappings are stale or auth.ID is missing.
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	upstreamModel := ""
	switch provider {
	case "gemini":
		upstreamModel = resolveUpstreamModelForGeminiAPIKey(cfg, auth, requestedModel)
	case "claude":
		upstreamModel = resolveUpstreamModelForClaudeAPIKey(cfg, auth, requestedModel)
	case "codex":
		upstreamModel = resolveUpstreamModelForCodexAPIKey(cfg, auth, requestedModel)
	case "vertex":
		upstreamModel = resolveUpstreamModelForVertexAPIKey(cfg, auth, requestedModel)
	default:
		upstreamModel = resolveUpstreamModelForOpenAICompatAPIKey(cfg, auth, requestedModel)
	}

	// Return upstream model if found, otherwise return requested model.
	if upstreamModel != "" {
		return preventCodexDowngrade(requestedModel, upstreamModel)
	}
	return requestedModel
}

func preventCodexDowngrade(requestedModel, resolvedModel string) string {
	requestedModel = strings.TrimSpace(requestedModel)
	resolvedModel = strings.TrimSpace(resolvedModel)
	if requestedModel == "" || resolvedModel == "" {
		if resolvedModel != "" {
			return resolvedModel
		}
		return requestedModel
	}
	requestedBase := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(requestedModel).ModelName))
	resolvedBase := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(resolvedModel).ModelName))
	if strings.HasPrefix(requestedBase, "gpt-5.3-codex") && strings.HasPrefix(resolvedBase, "gpt-5.2-codex") {
		log.Warnf("blocked codex model downgrade: requested=%s resolved=%s", requestedModel, resolvedModel)
		return requestedModel
	}
	return resolvedModel
}

// APIKeyConfigEntry is a generic interface for API key configurations.
type APIKeyConfigEntry interface {
	GetAPIKey() string
	GetBaseURL() string
}

func resolveAPIKeyConfig[T APIKeyConfigEntry](entries []T, auth *Auth) *T {
	if auth == nil || len(entries) == 0 {
		return nil
	}
	attrKey, attrBase := "", ""
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range entries {
		entry := &entries[i]
		cfgKey := strings.TrimSpace((*entry).GetAPIKey())
		cfgBase := strings.TrimSpace((*entry).GetBaseURL())
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range entries {
			entry := &entries[i]
			if strings.EqualFold(strings.TrimSpace((*entry).GetAPIKey()), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func resolveGeminiAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.GeminiKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.GeminiKey, auth)
}

func resolveClaudeAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.ClaudeKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.ClaudeKey, auth)
}

func resolveCodexAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.CodexKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.CodexKey, auth)
}

func resolveVertexAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.VertexCompatKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.VertexCompatAPIKey, auth)
}

func resolveUpstreamModelForGeminiAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveGeminiAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForClaudeAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveClaudeAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForCodexAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveCodexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForVertexAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveVertexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForOpenAICompatAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	providerKey := ""
	compatName := ""
	if auth != nil && len(auth.Attributes) > 0 {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	if compatName == "" && !strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return ""
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

type apiKeyModelAliasTable map[string]map[string]string

func resolveOpenAICompatConfig(cfg *internalconfig.Config, providerKey, compatName, authProvider string) *internalconfig.OpenAICompatibility {
	if cfg == nil {
		return nil
	}
	candidates := make([]string, 0, 3)
	if v := strings.TrimSpace(compatName); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(providerKey); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(authProvider); v != "" {
		candidates = append(candidates, v)
	}
	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		for _, candidate := range candidates {
			if candidate != "" && strings.EqualFold(strings.TrimSpace(candidate), compat.Name) {
				return compat
			}
		}
	}
	return nil
}

func asModelAliasEntries[T interface {
	GetName() string
	GetAlias() string
}](models []T) []modelAliasEntry {
	if len(models) == 0 {
		return nil
	}
	out := make([]modelAliasEntry, 0, len(models))
	for i := range models {
		out = append(out, models[i])
	}
	return out
}

func (m *Manager) normalizeProviders(providers []string) []string {
	if len(providers) == 0 {
		return nil
	}
	result := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		p := strings.TrimSpace(strings.ToLower(provider))
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		result = append(result, p)
	}
	return result
}

func (m *Manager) retrySettings() (int, int, time.Duration) {
	if m == nil {
		return 0, 0, 0
	}
	return int(m.requestRetry.Load()), int(m.maxRetryCredentials.Load()), time.Duration(m.maxRetryInterval.Load())
}

func (m *Manager) providerResilienceSettings() providerResilienceSettings {
	settings := providerResilienceSettings{
		circuitBreakerEnabled: true,
		failureThreshold:      internalconfig.DefaultProviderCBThreshold,
		halfOpenMaxRequests:   internalconfig.DefaultProviderHalfOpenMax,
		openStateDuration:     time.Duration(internalconfig.DefaultProviderOpenStateMS) * time.Millisecond,
		maxInflight:           internalconfig.DefaultProviderMaxInFlight,
		adaptiveLimiter:       false,
		adaptiveMinInflight:   internalconfig.DefaultProviderAdaptiveMin,
		adaptiveMaxInflight:   internalconfig.DefaultProviderAdaptiveMax,
		adaptiveSuccessWindow: internalconfig.DefaultProviderAdaptiveWin,
		adaptiveAdditiveStep:  internalconfig.DefaultProviderAdaptiveStep,
		adaptiveBackoffFactor: internalconfig.DefaultProviderAdaptiveDecay,
		adaptiveAutoRollback:  false,
		adaptiveRollbackWin:   time.Duration(internalconfig.DefaultAdaptiveRollbackWinMS) * time.Millisecond,
		adaptiveRollbackMinN:  internalconfig.DefaultAdaptiveRollbackMinN,
		adaptiveRollbackP95MS: internalconfig.DefaultAdaptiveRollbackP95MS,
		adaptiveRollbackErr:   internalconfig.DefaultAdaptiveRollbackErr,
		adaptiveRollback429:   internalconfig.DefaultAdaptiveRollback429,
		adaptiveRollback5xx:   internalconfig.DefaultAdaptiveRollback5xx,
	}
	if m == nil {
		return settings
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return settings
	}
	settings.circuitBreakerEnabled = cfg.ProviderResilience.CircuitBreakerEnabled
	if cfg.ProviderResilience.FailureThreshold > 0 {
		settings.failureThreshold = cfg.ProviderResilience.FailureThreshold
	}
	if cfg.ProviderResilience.HalfOpenMaxRequests > 0 {
		settings.halfOpenMaxRequests = cfg.ProviderResilience.HalfOpenMaxRequests
	}
	if cfg.ProviderResilience.OpenStateMS > 0 {
		settings.openStateDuration = time.Duration(cfg.ProviderResilience.OpenStateMS) * time.Millisecond
	}
	if cfg.ProviderResilience.MaxInflightPerProvider > 0 {
		settings.maxInflight = cfg.ProviderResilience.MaxInflightPerProvider
	}
	settings.adaptiveLimiter = cfg.ProviderResilience.AdaptiveLimiterEnabled
	if len(cfg.ProviderResilience.AdaptiveProviders) > 0 {
		settings.adaptiveProviders = make(map[string]struct{}, len(cfg.ProviderResilience.AdaptiveProviders))
		for _, provider := range cfg.ProviderResilience.AdaptiveProviders {
			key := strings.TrimSpace(strings.ToLower(provider))
			if key == "" {
				continue
			}
			settings.adaptiveProviders[key] = struct{}{}
		}
	}
	if cfg.ProviderResilience.AdaptiveMinInflight > 0 {
		settings.adaptiveMinInflight = cfg.ProviderResilience.AdaptiveMinInflight
	}
	if cfg.ProviderResilience.AdaptiveMaxInflight > 0 {
		settings.adaptiveMaxInflight = cfg.ProviderResilience.AdaptiveMaxInflight
	}
	if cfg.ProviderResilience.AdaptiveSuccessWindow > 0 {
		settings.adaptiveSuccessWindow = cfg.ProviderResilience.AdaptiveSuccessWindow
	}
	if cfg.ProviderResilience.AdaptiveAdditiveStep > 0 {
		settings.adaptiveAdditiveStep = cfg.ProviderResilience.AdaptiveAdditiveStep
	}
	if cfg.ProviderResilience.AdaptiveBackoffFactor > 0 && cfg.ProviderResilience.AdaptiveBackoffFactor < 1 {
		settings.adaptiveBackoffFactor = cfg.ProviderResilience.AdaptiveBackoffFactor
	}
	settings.adaptiveAutoRollback = cfg.ProviderResilience.AdaptiveAutoRollbackEnabled
	if cfg.ProviderResilience.AdaptiveAutoRollbackWindowMS > 0 {
		settings.adaptiveRollbackWin = time.Duration(cfg.ProviderResilience.AdaptiveAutoRollbackWindowMS) * time.Millisecond
	}
	if cfg.ProviderResilience.AdaptiveAutoRollbackMinSamples > 0 {
		settings.adaptiveRollbackMinN = cfg.ProviderResilience.AdaptiveAutoRollbackMinSamples
	}
	if cfg.ProviderResilience.AdaptiveAutoRollbackMaxP95MS > 0 {
		settings.adaptiveRollbackP95MS = cfg.ProviderResilience.AdaptiveAutoRollbackMaxP95MS
	}
	if cfg.ProviderResilience.AdaptiveAutoRollbackMaxErrRate > 0 && cfg.ProviderResilience.AdaptiveAutoRollbackMaxErrRate < 1 {
		settings.adaptiveRollbackErr = cfg.ProviderResilience.AdaptiveAutoRollbackMaxErrRate
	}
	if cfg.ProviderResilience.AdaptiveAutoRollbackMax429Rate > 0 && cfg.ProviderResilience.AdaptiveAutoRollbackMax429Rate < 1 {
		settings.adaptiveRollback429 = cfg.ProviderResilience.AdaptiveAutoRollbackMax429Rate
	}
	if cfg.ProviderResilience.AdaptiveAutoRollbackMax5xxRate > 0 && cfg.ProviderResilience.AdaptiveAutoRollbackMax5xxRate < 1 {
		settings.adaptiveRollback5xx = cfg.ProviderResilience.AdaptiveAutoRollbackMax5xxRate
	}
	if settings.adaptiveMinInflight < 1 {
		settings.adaptiveMinInflight = 1
	}
	if settings.adaptiveMaxInflight <= 0 || settings.adaptiveMaxInflight > settings.maxInflight {
		settings.adaptiveMaxInflight = settings.maxInflight
	}
	if settings.adaptiveMaxInflight < settings.adaptiveMinInflight {
		settings.adaptiveMaxInflight = settings.adaptiveMinInflight
	}
	return settings
}

func (s providerResilienceSettings) adaptiveLimiterEnabledForProvider(provider string) bool {
	if !s.adaptiveLimiter {
		return false
	}
	if len(s.adaptiveProviders) == 0 {
		return true
	}
	_, ok := s.adaptiveProviders[provider]
	return ok
}

func (m *Manager) adaptiveLimiterEnabledForProviderLocked(provider string, settings providerResilienceSettings, now time.Time) bool {
	if !settings.adaptiveLimiterEnabledForProvider(provider) {
		return false
	}
	until, ok := m.providerAdaptiveRollbackUntil[provider]
	if !ok {
		return true
	}
	if now.Before(until) {
		return false
	}
	delete(m.providerAdaptiveRollbackUntil, provider)
	return true
}

func (m *Manager) adaptiveInflightLimitLocked(provider string, settings providerResilienceSettings) int {
	if !settings.adaptiveLimiter {
		return settings.maxInflight
	}
	limit := m.providerAdaptiveLimit[provider]
	if limit <= 0 {
		limit = settings.adaptiveMaxInflight
	}
	if limit < settings.adaptiveMinInflight {
		limit = settings.adaptiveMinInflight
	}
	if limit > settings.adaptiveMaxInflight {
		limit = settings.adaptiveMaxInflight
	}
	m.providerAdaptiveLimit[provider] = limit
	return limit
}

func shouldAdaptiveBackoffFromError(err error) bool {
	if err == nil {
		return false
	}
	if isRequestInvalidError(err) {
		return false
	}
	status := statusCodeFromError(err)
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound:
		return false
	case http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusTooEarly,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	// Unknown status (transport/network errors) -> conservative backoff.
	if status <= 0 {
		return true
	}
	return false
}

func adaptiveLatencyBucketIndex(latency time.Duration) int {
	if latency < 0 {
		latency = 0
	}
	latencyMS := latency.Milliseconds()
	for i := range adaptiveLatencyBucketsMS {
		if latencyMS <= adaptiveLatencyBucketsMS[i] {
			return i
		}
	}
	return len(adaptiveLatencyBucketsMS) - 1
}

func adaptiveWindowP95MS(state *providerAdaptiveSLOState) int {
	if state == nil || state.samples == 0 {
		return 0
	}
	target := (95*state.samples + 99) / 100
	var cumulative uint64
	for i := range state.latencyBuckets {
		cumulative += state.latencyBuckets[i]
		if cumulative >= target {
			return int(adaptiveLatencyBucketsMS[i])
		}
	}
	return int(adaptiveLatencyBucketsMS[len(adaptiveLatencyBucketsMS)-1])
}

func (m *Manager) evaluateAdaptiveRollbackLocked(provider string, settings providerResilienceSettings, err error, latency time.Duration, now time.Time) {
	if !settings.adaptiveAutoRollback || settings.adaptiveRollbackWin <= 0 {
		return
	}
	state := m.providerAdaptiveSLOWindow[provider]
	if state == nil || now.Sub(state.windowStart) >= settings.adaptiveRollbackWin {
		state = &providerAdaptiveSLOState{
			windowStart: now,
		}
		m.providerAdaptiveSLOWindow[provider] = state
	}

	state.samples++
	if err != nil {
		state.errors++
	}
	status := statusCodeFromError(err)
	if status == http.StatusTooManyRequests {
		state.http429++
	}
	if status >= 500 && status <= 599 {
		state.http5xx++
	}
	state.latencyBuckets[adaptiveLatencyBucketIndex(latency)]++

	if int(state.samples) < settings.adaptiveRollbackMinN {
		return
	}

	samples := float64(state.samples)
	errRate := float64(state.errors) / samples
	http429Rate := float64(state.http429) / samples
	http5xxRate := float64(state.http5xx) / samples
	p95MS := adaptiveWindowP95MS(state)

	if p95MS <= settings.adaptiveRollbackP95MS &&
		errRate <= settings.adaptiveRollbackErr &&
		http429Rate <= settings.adaptiveRollback429 &&
		http5xxRate <= settings.adaptiveRollback5xx {
		return
	}

	m.providerAdaptiveRollbackUntil[provider] = now.Add(settings.adaptiveRollbackWin)
	delete(m.providerAdaptiveLimit, provider)
	delete(m.providerAdaptiveSuccessStreak, provider)
	delete(m.providerAdaptiveSLOWindow, provider)
	m.providerAdaptiveRollback.Add(1)

	log.Warnf(
		"adaptive limiter rollback triggered for provider=%s (p95_ms=%d err_rate=%.3f http429_rate=%.3f http5xx_rate=%.3f hold_ms=%d)",
		provider,
		p95MS,
		errRate,
		http429Rate,
		http5xxRate,
		settings.adaptiveRollbackWin.Milliseconds(),
	)
}

func (m *Manager) acquireProviderPermit(provider string) (func(), error) {
	if m == nil {
		return func() {}, nil
	}
	settings := m.providerResilienceSettings()
	provider = strings.TrimSpace(strings.ToLower(provider))
	if provider == "" {
		return func() {}, nil
	}
	if settings.circuitBreakerEnabled {
		now := time.Now()
		m.providerCircuitMu.Lock()
		state := m.providerCircuit[provider]
		if state == nil {
			state = &providerCircuitState{}
			m.providerCircuit[provider] = state
		}
		if !state.openUntil.IsZero() {
			if state.openUntil.After(now) {
				m.providerCircuitMu.Unlock()
				m.providerCircuitOpenCount.Add(1)
				return nil, &Error{
					Code:       "provider_circuit_open",
					Message:    "provider circuit breaker is open",
					Retryable:  true,
					HTTPStatus: http.StatusServiceUnavailable,
				}
			}
			if state.halfOpenRemaining <= 0 {
				state.halfOpenRemaining = settings.halfOpenMaxRequests
			}
			if state.halfOpenRemaining > 0 {
				state.halfOpenRemaining--
			}
		}
		m.providerCircuitMu.Unlock()
	}

	if settings.maxInflight <= 0 {
		return func() {}, nil
	}
	m.providerLimiterMu.Lock()
	limiter := m.providerLimiter[provider]
	if limiter == nil {
		limiter = make(chan struct{}, settings.maxInflight)
		m.providerLimiter[provider] = limiter
	} else if cap(limiter) != settings.maxInflight {
		limiter = make(chan struct{}, settings.maxInflight)
		m.providerLimiter[provider] = limiter
		delete(m.providerAdaptiveLimit, provider)
		delete(m.providerAdaptiveSuccessStreak, provider)
	}
	currentInflightLimit := settings.maxInflight
	if m.adaptiveLimiterEnabledForProviderLocked(provider, settings, time.Now()) {
		currentInflightLimit = m.adaptiveInflightLimitLocked(provider, settings)
	}
	if len(limiter) >= currentInflightLimit {
		m.providerBackpressureCount.Add(1)
		m.providerLimiterMu.Unlock()
		return nil, &Error{
			Code:       "provider_backpressure",
			Message:    "provider inflight limit exceeded",
			Retryable:  true,
			HTTPStatus: http.StatusServiceUnavailable,
		}
	}
	select {
	case limiter <- struct{}{}:
		m.providerLimiterMu.Unlock()
		return func() {
			select {
			case <-limiter:
			default:
			}
		}, nil
	default:
		m.providerLimiterMu.Unlock()
		m.providerBackpressureCount.Add(1)
		return nil, &Error{
			Code:       "provider_backpressure",
			Message:    "provider inflight limit exceeded",
			Retryable:  true,
			HTTPStatus: http.StatusServiceUnavailable,
		}
	}
}

func (m *Manager) recordProviderResult(provider string, err error, latency time.Duration) {
	if m == nil {
		return
	}
	settings := m.providerResilienceSettings()
	provider = strings.TrimSpace(strings.ToLower(provider))
	if provider == "" {
		return
	}
	now := time.Now()

	if settings.circuitBreakerEnabled {
		m.providerCircuitMu.Lock()
		state := m.providerCircuit[provider]
		if state == nil {
			state = &providerCircuitState{}
			m.providerCircuit[provider] = state
		}
		if err == nil {
			state.consecutiveFailures = 0
			state.openUntil = time.Time{}
			state.halfOpenRemaining = 0
		} else if !isRequestInvalidError(err) {
			status := statusCodeFromError(err)
			if status != http.StatusBadRequest {
				// Half-open state = circuit was open but cooldown (openUntil) has expired.
				// In half-open we must honor halfOpenMaxRequests probe attempts before
				// re-tripping: previously a single half-open failure re-opened the circuit
				// immediately (because consecutiveFailures was already >= threshold), which
				// made halfOpenMaxRequests ineffective and left the circuit stuck re-opening
				// on every probe for providers that fail consistently (e.g. Openference 403).
				inHalfOpen := !state.openUntil.IsZero() && !state.openUntil.After(now)
				if inHalfOpen && state.halfOpenRemaining > 0 {
					// Half-open probe failed but attempts remain: stay half-open and let the
					// next probe through (halfOpenRemaining was already decremented at acquire).
					// Only re-trip once all probes are exhausted.
				} else {
					state.consecutiveFailures++
					if state.consecutiveFailures >= settings.failureThreshold {
						state.openUntil = now.Add(settings.openStateDuration)
						state.halfOpenRemaining = 0
					}
				}
			}
		}
		m.providerCircuitMu.Unlock()
	}

	if !m.adaptiveLimiterEnabledForProviderLocked(provider, settings, now) {
		return
	}

	m.providerLimiterMu.Lock()
	defer m.providerLimiterMu.Unlock()
	current := m.adaptiveInflightLimitLocked(provider, settings)
	if err == nil {
		streak := m.providerAdaptiveSuccessStreak[provider] + 1
		if streak >= settings.adaptiveSuccessWindow {
			next := current + settings.adaptiveAdditiveStep
			if next > settings.adaptiveMaxInflight {
				next = settings.adaptiveMaxInflight
			}
			if next > current {
				current = next
				m.providerAdaptiveIncCount.Add(1)
			}
			streak = 0
		}
		m.providerAdaptiveSuccessStreak[provider] = streak
		m.providerAdaptiveLimit[provider] = current
		m.evaluateAdaptiveRollbackLocked(provider, settings, nil, latency, now)
		return
	}

	m.providerAdaptiveSuccessStreak[provider] = 0
	if !shouldAdaptiveBackoffFromError(err) {
		m.providerAdaptiveLimit[provider] = current
		m.evaluateAdaptiveRollbackLocked(provider, settings, err, latency, now)
		return
	}
	next := int(float64(current) * settings.adaptiveBackoffFactor)
	if next >= current {
		next = current - 1
	}
	if next < settings.adaptiveMinInflight {
		next = settings.adaptiveMinInflight
	}
	if next < current {
		m.providerAdaptiveDecCount.Add(1)
	}
	m.providerAdaptiveLimit[provider] = next
	m.evaluateAdaptiveRollbackLocked(provider, settings, err, latency, now)
}

func (m *Manager) closestCooldownWait(providers []string, model string, attempt int) (time.Duration, bool) {
	if m == nil || len(providers) == 0 {
		return 0, false
	}
	now := time.Now()
	defaultRetry := int(m.requestRetry.Load())
	if defaultRetry < 0 {
		defaultRetry = 0
	}
	providerSet := make(map[string]struct{}, len(providers))
	for i := range providers {
		key := strings.TrimSpace(strings.ToLower(providers[i]))
		if key == "" {
			continue
		}
		providerSet[key] = struct{}{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var (
		found   bool
		minWait time.Duration
	)
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		effectiveRetry := defaultRetry
		if override, ok := auth.RequestRetryOverride(); ok {
			effectiveRetry = override
		}
		if effectiveRetry < 0 {
			effectiveRetry = 0
		}
		if attempt >= effectiveRetry {
			continue
		}
		blocked, reason, next := isAuthBlockedForModel(auth, model, now)
		if !blocked || next.IsZero() || reason == blockReasonDisabled {
			continue
		}
		wait := next.Sub(now)
		if wait < 0 {
			continue
		}
		if !found || wait < minWait {
			minWait = wait
			found = true
		}
	}
	return minWait, found
}

func (m *Manager) shouldRetryAfterError(err error, attempt int, providers []string, model string, maxWait time.Duration) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	if maxWait <= 0 {
		return 0, false
	}
	if status := statusCodeFromError(err); status == http.StatusOK {
		return 0, false
	}
	if isRequestInvalidError(err) {
		return 0, false
	}
	wait, found := m.closestCooldownWait(providers, model, attempt)
	if !found || wait > maxWait {
		return 0, false
	}
	return wait, true
}

func waitForCooldown(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// MarkResult records an execution result and notifies hooks.
func (m *Manager) MarkResult(ctx context.Context, result Result) {
	if result.AuthID == "" {
		return
	}

	shouldResumeModel := false
	shouldSuspendModel := false
	suspendReason := ""
	clearModelQuota := false
	setModelQuota := false
	var authSnapshot *Auth

	m.mu.Lock()
	if auth, ok := m.auths[result.AuthID]; ok && auth != nil {
		now := time.Now()

		if result.Success {
			if result.Model != "" {
				state := ensureModelState(auth, result.Model)
				resetModelState(state, now)
				updateAggregatedAvailability(auth, now)
				if !hasModelError(auth, now) {
					auth.LastError = nil
					auth.StatusMessage = ""
					auth.Status = StatusActive
				}
				auth.UpdatedAt = now
				shouldResumeModel = true
				clearModelQuota = true
			} else {
				clearAuthStateOnSuccess(auth, now)
			}
		} else {
			if result.Model != "" {
				state := ensureModelState(auth, result.Model)
				state.Unavailable = true
				state.Status = StatusError
				state.UpdatedAt = now
				if result.Error != nil {
					state.LastError = cloneError(result.Error)
					state.StatusMessage = result.Error.Message
					auth.LastError = cloneError(result.Error)
					auth.StatusMessage = result.Error.Message
				}

				if isDeactivatedWorkspaceError(result.Error) {
					// Permanent workspace deactivation: disable the entire auth so it stops being selected.
					state.Status = StatusDisabled
					state.Unavailable = true
					state.NextRetryAfter = now.Add(deactivatedWorkspaceBackoff)
					quarantineAuth(auth, now, result.Error, StatusDisabled, "deactivated_workspace", deactivatedWorkspaceBackoff)
					updateAggregatedAvailability(auth, now)
					suspendReason = "deactivated_workspace"
					shouldSuspendModel = true
				} else {
					statusCode := statusCodeFromResult(result.Error)
					switch statusCode {
					case 401:
						next := now.Add(30 * time.Minute)
						state.NextRetryAfter = next
						suspendReason = "unauthorized"
						shouldSuspendModel = true
					case 402, 403:
						next := now.Add(30 * time.Minute)
						state.NextRetryAfter = next
						suspendReason = "payment_required"
						shouldSuspendModel = true
					case 404:
						next := now.Add(12 * time.Hour)
						state.NextRetryAfter = next
						suspendReason = "not_found"
						shouldSuspendModel = true
					case 429:
						var next time.Time
						backoffLevel := state.Quota.BackoffLevel
						if result.RetryAfter != nil {
							next = now.Add(*result.RetryAfter)
						} else {
							cooldown, nextLevel := nextQuotaCooldown(backoffLevel, quotaCooldownDisabledForAuth(auth))
							if cooldown > 0 {
								next = now.Add(cooldown)
							}
							backoffLevel = nextLevel
						}
						state.NextRetryAfter = next
						state.Quota = QuotaState{
							Exceeded:      true,
							Reason:        "quota",
							NextRecoverAt: next,
							BackoffLevel:  backoffLevel,
						}
						suspendReason = "quota"
						shouldSuspendModel = true
						setModelQuota = true
					case 408, 500, 502, 503, 504:
						if quotaCooldownDisabledForAuth(auth) {
							state.NextRetryAfter = time.Time{}
						} else {
							next := now.Add(1 * time.Minute)
							state.NextRetryAfter = next
						}
					default:
						state.NextRetryAfter = time.Time{}
					}

					auth.Status = StatusError
					auth.UpdatedAt = now
					updateAggregatedAvailability(auth, now)
				}
			} else {
				applyAuthFailureState(auth, result.Error, result.RetryAfter, now)
			}
		}

		_ = m.persist(ctx, auth)
		authSnapshot = auth.Clone()
	}
	m.mu.Unlock()
	if m.scheduler != nil && authSnapshot != nil {
		m.scheduler.upsertAuth(authSnapshot)
	}

	if clearModelQuota && result.Model != "" {
		registry.GetGlobalRegistry().ClearModelQuotaExceeded(result.AuthID, result.Model)
	}
	if setModelQuota && result.Model != "" {
		registry.GetGlobalRegistry().SetModelQuotaExceeded(result.AuthID, result.Model)
	}
	if shouldResumeModel {
		registry.GetGlobalRegistry().ResumeClientModel(result.AuthID, result.Model)
	} else if shouldSuspendModel {
		registry.GetGlobalRegistry().SuspendClientModel(result.AuthID, result.Model, suspendReason)
	}

	m.hook.OnResult(ctx, result)
}

func ensureModelState(auth *Auth, model string) *ModelState {
	if auth == nil || model == "" {
		return nil
	}
	if auth.ModelStates == nil {
		auth.ModelStates = make(map[string]*ModelState)
	}
	if state, ok := auth.ModelStates[model]; ok && state != nil {
		return state
	}
	state := &ModelState{Status: StatusActive}
	auth.ModelStates[model] = state
	return state
}

func resetModelState(state *ModelState, now time.Time) {
	if state == nil {
		return
	}
	state.Unavailable = false
	state.Status = StatusActive
	state.StatusMessage = ""
	state.NextRetryAfter = time.Time{}
	state.LastError = nil
	state.Quota = QuotaState{}
	state.UpdatedAt = now
}

func updateAggregatedAvailability(auth *Auth, now time.Time) {
	if auth == nil || len(auth.ModelStates) == 0 {
		return
	}
	allUnavailable := true
	earliestRetry := time.Time{}
	quotaExceeded := false
	quotaRecover := time.Time{}
	maxBackoffLevel := 0
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		stateUnavailable := false
		if state.Status == StatusDisabled {
			stateUnavailable = true
		} else if state.Unavailable {
			if state.NextRetryAfter.IsZero() {
				stateUnavailable = false
			} else if state.NextRetryAfter.After(now) {
				stateUnavailable = true
				if earliestRetry.IsZero() || state.NextRetryAfter.Before(earliestRetry) {
					earliestRetry = state.NextRetryAfter
				}
			} else {
				state.Unavailable = false
				state.NextRetryAfter = time.Time{}
			}
		}
		if !stateUnavailable {
			allUnavailable = false
		}
		if state.Quota.Exceeded {
			quotaExceeded = true
			if quotaRecover.IsZero() || (!state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.Before(quotaRecover)) {
				quotaRecover = state.Quota.NextRecoverAt
			}
			if state.Quota.BackoffLevel > maxBackoffLevel {
				maxBackoffLevel = state.Quota.BackoffLevel
			}
		}
	}
	auth.Unavailable = allUnavailable
	if allUnavailable {
		auth.NextRetryAfter = earliestRetry
	} else {
		auth.NextRetryAfter = time.Time{}
	}
	if quotaExceeded {
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		auth.Quota.NextRecoverAt = quotaRecover
		auth.Quota.BackoffLevel = maxBackoffLevel
	} else {
		auth.Quota.Exceeded = false
		auth.Quota.Reason = ""
		auth.Quota.NextRecoverAt = time.Time{}
		auth.Quota.BackoffLevel = 0
	}
}

func hasModelError(auth *Auth, now time.Time) bool {
	if auth == nil || len(auth.ModelStates) == 0 {
		return false
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.LastError != nil {
			return true
		}
		if state.Status == StatusError {
			if state.Unavailable && (state.NextRetryAfter.IsZero() || state.NextRetryAfter.After(now)) {
				return true
			}
		}
	}
	return false
}

func clearAuthStateOnSuccess(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	auth.Unavailable = false
	auth.Status = StatusActive
	auth.StatusMessage = ""
	auth.Quota.Exceeded = false
	auth.Quota.Reason = ""
	auth.Quota.NextRecoverAt = time.Time{}
	auth.Quota.BackoffLevel = 0
	auth.LastError = nil
	auth.NextRetryAfter = time.Time{}
	auth.UpdatedAt = now
}

func cloneError(err *Error) *Error {
	if err == nil {
		return nil
	}
	return &Error{
		Code:       err.Code,
		Message:    err.Message,
		Retryable:  err.Retryable,
		HTTPStatus: err.HTTPStatus,
	}
}

func statusCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	type statusCoder interface {
		StatusCode() int
	}
	var sc statusCoder
	if errors.As(err, &sc) && sc != nil {
		return sc.StatusCode()
	}
	return 0
}

func retryAfterFromError(err error) *time.Duration {
	if err == nil {
		return nil
	}
	type retryAfterProvider interface {
		RetryAfter() *time.Duration
	}
	rap, ok := err.(retryAfterProvider)
	if !ok || rap == nil {
		return nil
	}
	retryAfter := rap.RetryAfter()
	if retryAfter == nil {
		return nil
	}
	return new(*retryAfter)
}

func isDeactivatedWorkspaceError(err *Error) bool {
	if err == nil {
		return false
	}
	code := strings.ToLower(strings.TrimSpace(err.Code))
	if code == "deactivated_workspace" {
		return true
	}
	msg := strings.ToLower(err.Message)
	return strings.Contains(msg, "deactivated_workspace") || strings.Contains(msg, "deactivated workspace")
}

func isRefreshTokenReusedErrorMessage(message string) bool {
	msg := strings.ToLower(strings.TrimSpace(message))
	if msg == "" {
		return false
	}
	return strings.Contains(msg, "refresh_token_reused") || strings.Contains(msg, "refresh token reused") || strings.Contains(msg, "token_reused") || strings.Contains(msg, "token has already been used")
}

func quarantineAuth(auth *Auth, now time.Time, resultErr *Error, status Status, statusMessage string, backoff time.Duration) {
	if auth == nil {
		return
	}
	if backoff <= 0 {
		backoff = refreshFailureBackoff
	}
	auth.Disabled = true
	auth.Status = status
	auth.StatusMessage = statusMessage
	auth.Unavailable = true
	auth.NextRetryAfter = now.Add(backoff)
	auth.NextRefreshAfter = now.Add(backoff)
	auth.LastError = cloneError(resultErr)
	auth.UpdatedAt = now
}

func markAuthReloginRequired(auth *Auth, now time.Time, cause error) {
	if auth == nil {
		return
	}
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	// Do not hard-disable: keep semantic distinction between "disabled" and
	// "unavailable until re-login".
	auth.Disabled = false
	auth.Status = StatusError
	auth.StatusMessage = "relogin_required"
	auth.Unavailable = true
	auth.NextRetryAfter = now.Add(refreshTokenReusedBackoff)
	auth.NextRefreshAfter = now.Add(refreshTokenReusedBackoff)
	auth.LastError = &Error{Code: extractErrorCode(cause), Message: msg}
	auth.UpdatedAt = now
}

func extractErrorCode(err error) string {
	if err == nil {
		return ""
	}
	type codeProvider interface{ Code() string }
	var cp codeProvider
	if errors.As(err, &cp) && cp != nil {
		code := strings.TrimSpace(cp.Code())
		if code != "" {
			return code
		}
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return ""
	}
	// Heuristic extraction for common patterns:
	// - "code: message"  (e.g., "deactivated_workspace: ...")
	// - "...\"code\":\"X\"..." JSON-ish payload in error string
	if idx := strings.Index(msg, ":"); idx > 0 {
		left := strings.TrimSpace(msg[:idx])
		if left != "" && len(left) <= 64 && !strings.ContainsAny(left, " \t\n\r{}[]\"") {
			return left
		}
	}
	lower := strings.ToLower(msg)
	for _, known := range []string{"deactivated_workspace", "refresh_token_reused", "token_reused"} {
		if strings.Contains(lower, known) {
			return known
		}
	}
	return ""
}

type refreshLockHandle struct {
	path string
}

func (h *refreshLockHandle) release() {
	if h == nil || h.path == "" {
		return
	}
	_ = os.Remove(h.path)
}

func (m *Manager) refreshLockDir() string {
	if m == nil {
		return ""
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return ""
	}
	dir := strings.TrimSpace(cfg.AuthDir)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, ".refresh_locks")
}

func refreshLockPath(lockDir string, authID string) string {
	if lockDir == "" || strings.TrimSpace(authID) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(authID))
	return filepath.Join(lockDir, "refresh-"+hex.EncodeToString(sum[:8])+
		".lock")
}

func tryAcquireRefreshLock(lockDir string, authID string, now time.Time) (*refreshLockHandle, bool) {
	lockPath := refreshLockPath(lockDir, authID)
	if lockPath == "" {
		return nil, true
	}
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, true
	}
	file, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		_, _ = file.WriteString(strconv.FormatInt(now.Unix(), 10))
		_ = file.Close()
		return &refreshLockHandle{path: lockPath}, true
	}
	// If a lock exists but is stale, best-effort delete.
	if info, statErr := os.Stat(lockPath); statErr == nil {
		if now.Sub(info.ModTime()) > refreshLockStaleAfter {
			_ = os.Remove(lockPath)
			file, err2 := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err2 == nil {
				_, _ = file.WriteString(strconv.FormatInt(now.Unix(), 10))
				_ = file.Close()
				return &refreshLockHandle{path: lockPath}, true
			}
		}
	}
	return nil, false
}

func statusCodeFromResult(err *Error) int {
	if err == nil {
		return 0
	}
	return err.StatusCode()
}

// isRequestInvalidError returns true if the error represents a client request
// error that should not be retried. Specifically, it treats 400 responses with
// "invalid_request_error" and all 422 responses as request-shape failures,
// where switching auths or pooled upstream models will not help.
func isRequestInvalidError(err error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromError(err)
	switch status {
	case http.StatusBadRequest:
		return strings.Contains(err.Error(), "invalid_request_error")
	case http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

func applyAuthFailureState(auth *Auth, resultErr *Error, retryAfter *time.Duration, now time.Time) {
	if auth == nil {
		return
	}

	// Hard-disable when upstream signals the workspace has been deactivated.
	if isDeactivatedWorkspaceError(resultErr) {
		quarantineAuth(auth, now, resultErr, StatusDisabled, "deactivated_workspace", deactivatedWorkspaceBackoff)
		return
	}

	auth.Unavailable = true
	auth.Status = StatusError
	auth.UpdatedAt = now
	if resultErr != nil {
		auth.LastError = cloneError(resultErr)
		if resultErr.Message != "" {
			auth.StatusMessage = resultErr.Message
		}
	}
	statusCode := statusCodeFromResult(resultErr)
	switch statusCode {
	case 401:
		auth.StatusMessage = "unauthorized"
		auth.NextRetryAfter = now.Add(30 * time.Minute)
	case 402, 403:
		auth.StatusMessage = "payment_required"
		auth.NextRetryAfter = now.Add(30 * time.Minute)
	case 404:
		auth.StatusMessage = "not_found"
		auth.NextRetryAfter = now.Add(12 * time.Hour)
	case 429:
		auth.StatusMessage = "quota exhausted"
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		var next time.Time
		if retryAfter != nil {
			next = now.Add(*retryAfter)
		} else {
			cooldown, nextLevel := nextQuotaCooldown(auth.Quota.BackoffLevel, quotaCooldownDisabledForAuth(auth))
			if cooldown > 0 {
				next = now.Add(cooldown)
			}
			auth.Quota.BackoffLevel = nextLevel
		}
		auth.Quota.NextRecoverAt = next
		auth.NextRetryAfter = next
	case 408, 500, 502, 503, 504:
		auth.StatusMessage = "transient upstream error"
		if quotaCooldownDisabledForAuth(auth) {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(1 * time.Minute)
		}
	default:
		if auth.StatusMessage == "" {
			auth.StatusMessage = "request failed"
		}
	}
}

// nextQuotaCooldown returns the next cooldown duration and updated backoff level for repeated quota errors.
func nextQuotaCooldown(prevLevel int, disableCooling bool) (time.Duration, int) {
	if prevLevel < 0 {
		prevLevel = 0
	}
	if disableCooling {
		return 0, prevLevel
	}
	cooldown := quotaBackoffBase * time.Duration(1<<prevLevel)
	if cooldown < quotaBackoffBase {
		cooldown = quotaBackoffBase
	}
	if cooldown >= quotaBackoffMax {
		return quotaBackoffMax, prevLevel
	}
	return cooldown, prevLevel + 1
}

// List returns all auth entries currently known by the manager.
func (m *Manager) List() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		list = append(list, auth.Clone())
	}
	return list
}

// ResilienceMetricsSnapshot returns provider resilience counters for observability.
func (m *Manager) ResilienceMetricsSnapshot() map[string]uint64 {
	if m == nil {
		return nil
	}
	m.providerLimiterMu.Lock()
	var adaptiveLimitSum uint64
	for _, limit := range m.providerAdaptiveLimit {
		if limit > 0 {
			adaptiveLimitSum += uint64(limit)
		}
	}
	adaptiveProviders := uint64(len(m.providerAdaptiveLimit))
	now := time.Now()
	var adaptiveRollbackActive uint64
	for provider, until := range m.providerAdaptiveRollbackUntil {
		if now.Before(until) {
			adaptiveRollbackActive++
			continue
		}
		delete(m.providerAdaptiveRollbackUntil, provider)
	}
	m.providerLimiterMu.Unlock()
	settings := m.providerResilienceSettings()
	adaptiveEnabled := uint64(0)
	if settings.adaptiveLimiter {
		adaptiveEnabled = 1
	}
	return map[string]uint64{
		"cliproxy_provider_backpressure_reject_total":   m.providerBackpressureCount.Load(),
		"cliproxy_provider_circuit_open_total":          m.providerCircuitOpenCount.Load(),
		"cliproxy_provider_adaptive_increase_total":     m.providerAdaptiveIncCount.Load(),
		"cliproxy_provider_adaptive_decrease_total":     m.providerAdaptiveDecCount.Load(),
		"cliproxy_provider_adaptive_limit_sum":          adaptiveLimitSum,
		"cliproxy_provider_adaptive_provider_count":     adaptiveProviders,
		"cliproxy_provider_adaptive_enabled":            adaptiveEnabled,
		"cliproxy_provider_adaptive_rollback_total":     m.providerAdaptiveRollback.Load(),
		"cliproxy_provider_adaptive_rollback_active":    adaptiveRollbackActive,
		"cliproxy_conductor_select_total":               m.conductorSelectCount.Load(),
		"cliproxy_conductor_select_ns_sum":              m.conductorSelectLatency.Load(),
		"cliproxy_conductor_permit_total":               m.conductorPermitCount.Load(),
		"cliproxy_conductor_permit_ns_sum":              m.conductorPermitLatency.Load(),
		"cliproxy_conductor_retry_total":                m.conductorRetryCount.Load(),
		"cliproxy_conductor_retry_wait_ns_sum":          m.conductorRetryWaitNS.Load(),
	}
}

// GetByID retrieves an auth entry by its ID.

func (m *Manager) GetByID(id string) (*Auth, bool) {
	if id == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth, ok := m.auths[id]
	if !ok {
		return nil, false
	}
	return auth.Clone(), true
}

// Executor returns the registered provider executor for a provider key.
func (m *Manager) Executor(provider string) (ProviderExecutor, bool) {
	if m == nil {
		return nil, false
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil, false
	}

	m.mu.RLock()
	executor, okExecutor := m.executors[provider]
	if !okExecutor {
		lowerProvider := strings.ToLower(provider)
		if lowerProvider != provider {
			executor, okExecutor = m.executors[lowerProvider]
		}
	}
	m.mu.RUnlock()

	if !okExecutor || executor == nil {
		return nil, false
	}
	return executor, true
}

// CloseExecutionSession asks all registered executors to release the supplied execution session.
func (m *Manager) CloseExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if m == nil || sessionID == "" {
		return
	}

	m.mu.RLock()
	executors := make([]ProviderExecutor, 0, len(m.executors))
	for _, exec := range m.executors {
		executors = append(executors, exec)
	}
	m.mu.RUnlock()

	for i := range executors {
		if closer, ok := executors[i].(ExecutionSessionCloser); ok && closer != nil {
			closer.CloseExecutionSession(sessionID)
		}
	}
}

func (m *Manager) useSchedulerFastPath() bool {
	if m == nil || m.scheduler == nil {
		return false
	}
	return isBuiltInSelector(m.selector)
}

func shouldRetrySchedulerPick(err error) bool {
	if err == nil {
		return false
	}
	var cooldownErr *modelCooldownError
	if errors.As(err, &cooldownErr) {
		return true
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	return authErr.Code == "auth_not_found" || authErr.Code == "auth_unavailable"
}

func (m *Manager) pickNextLegacy(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)

	m.mu.RLock()
	executor, okExecutor := m.executors[provider]
	if !okExecutor {
		m.mu.RUnlock()
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	candidates := make([]*Auth, 0, len(m.auths))
	modelKey := strings.TrimSpace(model)
	// Always use base model name (without thinking suffix) for auth matching.
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}
	registryRef := registry.GetGlobalRegistry()
	for _, candidate := range m.auths {
		if candidate.Provider != provider || candidate.Disabled {
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if modelKey != "" && registryRef != nil && !registryRef.ClientSupportsModel(candidate.ID, modelKey) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		m.mu.RUnlock()
		return nil, nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	selected, errPick := m.selector.Pick(ctx, provider, model, opts, candidates)
	if errPick != nil {
		m.mu.RUnlock()
		return nil, nil, errPick
	}
	if selected == nil {
		m.mu.RUnlock()
		return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	authCopy := selected.Clone()
	m.mu.RUnlock()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, nil
}

func (m *Manager) pickNext(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	if !m.useSchedulerFastPath() {
		return m.pickNextLegacy(ctx, provider, model, opts, tried)
	}
	executor, okExecutor := m.Executor(provider)
	if !okExecutor {
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	selected, errPick := m.scheduler.pickSingle(ctx, provider, model, opts, tried)
	if errPick != nil && model != "" && shouldRetrySchedulerPick(errPick) {
		m.syncScheduler()
		selected, errPick = m.scheduler.pickSingle(ctx, provider, model, opts, tried)
	}
	if errPick != nil {
		return nil, nil, errPick
	}
	if selected == nil {
		return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	authCopy := selected.Clone()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, nil
}

func (m *Manager) pickNextMixedLegacy(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)

	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		p := strings.TrimSpace(strings.ToLower(provider))
		if p == "" {
			continue
		}
		providerSet[p] = struct{}{}
	}
	if len(providerSet) == 0 {
		return nil, nil, "", &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	m.mu.RLock()
	candidates := make([]*Auth, 0, len(m.auths))
	modelKey := strings.TrimSpace(model)
	// Always use base model name (without thinking suffix) for auth matching.
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}
	registryRef := registry.GetGlobalRegistry()
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled {
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(candidate.Provider))
		if providerKey == "" {
			continue
		}
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if _, ok := m.executors[providerKey]; !ok {
			continue
		}
		if modelKey != "" && registryRef != nil && !registryRef.ClientSupportsModel(candidate.ID, modelKey) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		m.mu.RUnlock()
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	selected, errPick := m.selector.Pick(ctx, "mixed", model, opts, candidates)
	if errPick != nil {
		m.mu.RUnlock()
		return nil, nil, "", errPick
	}
	if selected == nil {
		m.mu.RUnlock()
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	providerKey := strings.TrimSpace(strings.ToLower(selected.Provider))
	executor, okExecutor := m.executors[providerKey]
	if !okExecutor {
		m.mu.RUnlock()
		return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	authCopy := selected.Clone()
	m.mu.RUnlock()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, providerKey, nil
}

func (m *Manager) pickNextMixed(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	if !m.useSchedulerFastPath() {
		return m.pickNextMixedLegacy(ctx, providers, model, opts, tried)
	}

	eligibleProviders := make([]string, 0, len(providers))
	seenProviders := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		providerKey := strings.TrimSpace(strings.ToLower(provider))
		if providerKey == "" {
			continue
		}
		if _, seen := seenProviders[providerKey]; seen {
			continue
		}
		if _, okExecutor := m.Executor(providerKey); !okExecutor {
			continue
		}
		seenProviders[providerKey] = struct{}{}
		eligibleProviders = append(eligibleProviders, providerKey)
	}
	if len(eligibleProviders) == 0 {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}

	selected, providerKey, errPick := m.scheduler.pickMixed(ctx, eligibleProviders, model, opts, tried)
	if errPick != nil && model != "" && shouldRetrySchedulerPick(errPick) {
		m.syncScheduler()
		selected, providerKey, errPick = m.scheduler.pickMixed(ctx, eligibleProviders, model, opts, tried)
	}
	if errPick != nil {
		return nil, nil, "", errPick
	}
	if selected == nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	executor, okExecutor := m.Executor(providerKey)
	if !okExecutor {
		return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	authCopy := selected.Clone()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, providerKey, nil
}

func (m *Manager) persist(ctx context.Context, auth *Auth) error {
	if m.store == nil || auth == nil {
		return nil
	}
	if shouldSkipPersist(ctx) {
		return nil
	}
	if auth.Attributes != nil {
		if v := strings.ToLower(strings.TrimSpace(auth.Attributes["runtime_only"])); v == "true" {
			return nil
		}
	}
	// Skip persistence when metadata is absent (e.g., runtime-only auths).
	if auth.Metadata == nil {
		return nil
	}
	_, err := m.store.Save(ctx, auth)
	return err
}

// StartAutoRefresh launches a background loop that evaluates auth freshness
// every few seconds and triggers refresh operations when required.
// Only one loop is kept alive; starting a new one cancels the previous run.
func (m *Manager) StartAutoRefresh(parent context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = refreshCheckInterval
	}
	// Clamp lower bound to avoid overly tight refresh loops.
	if interval < refreshCheckInterval {
		interval = refreshCheckInterval
	}
	if m.refreshCancel != nil {
		m.refreshCancel()
		m.refreshCancel = nil
	}
	ctx, cancel := context.WithCancel(parent)
	m.refreshCancel = cancel
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		m.checkRefreshes(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.checkRefreshes(ctx)
			}
		}
	}()
}

// StopAutoRefresh cancels the background refresh loop, if running.
func (m *Manager) StopAutoRefresh() {
	if m.refreshCancel != nil {
		m.refreshCancel()
		m.refreshCancel = nil
	}
}

func (m *Manager) checkRefreshes(ctx context.Context) {
	// log.Debugf("checking refreshes")
	now := time.Now()
	snapshot := m.snapshotAuths()
	for _, a := range snapshot {
		typ, _ := a.AccountInfo()
		if typ != "api_key" {
			if !m.shouldRefresh(a, now) {
				continue
			}
			log.Debugf("checking refresh for %s, %s, %s", a.Provider, a.ID, typ)

			if exec := m.executorFor(a.Provider); exec == nil {
				continue
			}
			if !m.markRefreshPending(a.ID, now) {
				continue
			}
			go m.refreshAuthWithLimit(ctx, a.ID)
		}
	}
}

func (m *Manager) refreshAuthWithLimit(ctx context.Context, id string) {
	if m.refreshSemaphore == nil {
		m.refreshAuth(ctx, id)
		return
	}
	select {
	case m.refreshSemaphore <- struct{}{}:
		defer func() { <-m.refreshSemaphore }()
	case <-ctx.Done():
		return
	}
	m.refreshAuth(ctx, id)
}

func (m *Manager) snapshotAuths() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Auth, 0, len(m.auths))
	for _, a := range m.auths {
		out = append(out, a.Clone())
	}
	return out
}

func (m *Manager) shouldRefresh(a *Auth, now time.Time) bool {
	if a == nil || a.Disabled {
		return false
	}
	if !a.NextRefreshAfter.IsZero() && now.Before(a.NextRefreshAfter) {
		return false
	}
	if evaluator, ok := a.Runtime.(RefreshEvaluator); ok && evaluator != nil {
		return evaluator.ShouldRefresh(now, a)
	}

	lastRefresh := a.LastRefreshedAt
	if lastRefresh.IsZero() {
		if ts, ok := authLastRefreshTimestamp(a); ok {
			lastRefresh = ts
		}
	}

	expiry, hasExpiry := a.ExpirationTime()

	if interval := authPreferredInterval(a); interval > 0 {
		if hasExpiry && !expiry.IsZero() {
			if !expiry.After(now) {
				return true
			}
			if expiry.Sub(now) <= interval {
				return true
			}
		}
		if lastRefresh.IsZero() {
			return true
		}
		return now.Sub(lastRefresh) >= interval
	}

	provider := strings.ToLower(a.Provider)
	lead := ProviderRefreshLead(provider, a.Runtime)
	if lead == nil {
		return false
	}
	if *lead <= 0 {
		if hasExpiry && !expiry.IsZero() {
			return now.After(expiry)
		}
		return false
	}
	if hasExpiry && !expiry.IsZero() {
		return time.Until(expiry) <= *lead
	}
	if !lastRefresh.IsZero() {
		return now.Sub(lastRefresh) >= *lead
	}
	return true
}

func authPreferredInterval(a *Auth) time.Duration {
	if a == nil {
		return 0
	}
	if d := durationFromMetadata(a.Metadata, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	if d := durationFromAttributes(a.Attributes, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	return 0
}

func durationFromMetadata(meta map[string]any, keys ...string) time.Duration {
	if len(meta) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if dur := parseDurationValue(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func durationFromAttributes(attrs map[string]string, keys ...string) time.Duration {
	if len(attrs) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := attrs[key]; ok {
			if dur := parseDurationString(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func parseDurationValue(val any) time.Duration {
	switch v := val.(type) {
	case time.Duration:
		if v <= 0 {
			return 0
		}
		return v
	case int:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int32:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint32:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint64:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case float32:
		if v <= 0 {
			return 0
		}
		return time.Duration(float64(v) * float64(time.Second))
	case float64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v * float64(time.Second))
	case json.Number:
		if i, err := v.Int64(); err == nil {
			if i <= 0 {
				return 0
			}
			return time.Duration(i) * time.Second
		}
		if f, err := v.Float64(); err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
	case string:
		return parseDurationString(v)
	}
	return 0
}

func parseDurationString(raw string) time.Duration {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	if dur, err := time.ParseDuration(s); err == nil && dur > 0 {
		return dur
	}
	if secs, err := strconv.ParseFloat(s, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	return 0
}

func authLastRefreshTimestamp(a *Auth) (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	if a.Metadata != nil {
		if ts, ok := lookupMetadataTime(a.Metadata, "last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"); ok {
			return ts, true
		}
	}
	if a.Attributes != nil {
		for _, key := range []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"} {
			if val := strings.TrimSpace(a.Attributes[key]); val != "" {
				if ts, ok := parseTimeValue(val); ok {
					return ts, true
				}
			}
		}
	}
	return time.Time{}, false
}

func lookupMetadataTime(meta map[string]any, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if ts, ok1 := parseTimeValue(val); ok1 {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func (m *Manager) markRefreshPending(id string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	auth, ok := m.auths[id]
	if !ok || auth == nil || auth.Disabled {
		return false
	}
	if !auth.NextRefreshAfter.IsZero() && now.Before(auth.NextRefreshAfter) {
		return false
	}
	auth.NextRefreshAfter = now.Add(refreshPendingBackoff)
	m.auths[id] = auth
	return true
}

func (m *Manager) refreshAuth(ctx context.Context, id string) {
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	if id == "" || m == nil {
		return
	}

	_, _, _ = m.refreshSF.Do(id, func() (any, error) {
		m.mu.RLock()
		auth := m.auths[id]
		var exec ProviderExecutor
		if auth != nil {
			exec = m.executors[executorKeyFromAuth(auth)]
		}
		m.mu.RUnlock()
		if auth == nil || exec == nil {
			return nil, nil
		}

		now := time.Now()
		lockDir := m.refreshLockDir()
		lock, ok := tryAcquireRefreshLock(lockDir, id, now)
		if !ok {
			return nil, nil
		}
		defer lock.release()

		cloned := auth.Clone()
		updated, err := exec.Refresh(ctx, cloned)
		if err != nil && errors.Is(err, context.Canceled) {
			log.Debugf("refresh canceled for %s, %s", auth.Provider, auth.ID)
			return nil, nil
		}
		log.Debugf("refreshed %s, %s, %v", auth.Provider, auth.ID, err)
		now = time.Now()

		if err != nil {
			m.mu.Lock()
			if current := m.auths[id]; current != nil {
				if shouldDisableAutoRefreshOnError(current.Provider, err) {
					// Stop hammering a known-bad refresh token; require operator re-login.
					markAuthReloginRequired(current, now, err)
				} else {
					current.NextRefreshAfter = now.Add(refreshFailureBackoff)
					current.LastError = &Error{Message: err.Error()}
					current.UpdatedAt = now
				}
				m.auths[id] = current
				_ = m.persist(ctx, current)
			}
			m.mu.Unlock()
			return nil, err
		}

		if updated == nil {
			updated = cloned
		}
		// Preserve runtime created by the executor during Refresh.
		// If executor didn't set one, fall back to the previous runtime.
		if updated.Runtime == nil {
			updated.Runtime = auth.Runtime
		}
		updated.LastRefreshedAt = now
		updated.NextRefreshAfter = time.Time{}
		updated.LastError = nil
		updated.UpdatedAt = now
		_, _ = m.Update(ctx, updated)
		return nil, nil
	})
}

func (m *Manager) executorFor(provider string) ProviderExecutor {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.executors[provider]
}

// roundTripperContextKey is an unexported context key type to avoid collisions.
type roundTripperContextKey struct{}

// roundTripperFor retrieves an HTTP RoundTripper for the given auth if a provider is registered.
func (m *Manager) roundTripperFor(auth *Auth) http.RoundTripper {
	m.mu.RLock()
	p := m.rtProvider
	m.mu.RUnlock()
	if p == nil || auth == nil {
		return nil
	}
	return p.RoundTripperFor(auth)
}

// RoundTripperProvider defines a minimal provider of per-auth HTTP transports.
type RoundTripperProvider interface {
	RoundTripperFor(auth *Auth) http.RoundTripper
}

// RequestPreparer is an optional interface that provider executors can implement
// to mutate outbound HTTP requests with provider credentials.
type RequestPreparer interface {
	PrepareRequest(req *http.Request, auth *Auth) error
}

func shouldDisableAutoRefreshOnError(provider string, err error) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return false
	}
	if err == nil {
		return false
	}
	return isRefreshTokenReusedErrorMessage(err.Error())
}

func executorKeyFromAuth(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		providerKey := strings.TrimSpace(auth.Attributes["provider_key"])
		compatName := strings.TrimSpace(auth.Attributes["compat_name"])
		if compatName != "" {
			if providerKey == "" {
				providerKey = compatName
			}
			return strings.ToLower(providerKey)
		}
	}
	return strings.ToLower(strings.TrimSpace(auth.Provider))
}

// logEntryWithRequestID returns a logrus entry with request_id field if available in context.
func logEntryWithRequestID(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	if reqID := logging.GetRequestID(ctx); reqID != "" {
		return log.WithField("request_id", reqID)
	}
	return log.NewEntry(log.StandardLogger())
}

func debugLogAuthSelection(entry *log.Entry, auth *Auth, provider string, model string) {
	if !log.IsLevelEnabled(log.DebugLevel) {
		return
	}
	if entry == nil || auth == nil {
		return
	}
	accountType, accountInfo := auth.AccountInfo()
	proxyInfo := auth.ProxyInfo()
	suffix := ""
	if proxyInfo != "" {
		suffix = " " + proxyInfo
	}
	switch accountType {
	case "api_key":
		entry.Debugf("Use API key %s for model %s%s", util.HideAPIKey(accountInfo), model, suffix)
	case "oauth":
		ident := formatOauthIdentity(auth, provider, accountInfo)
		entry.Debugf("Use OAuth %s for model %s%s", ident, model, suffix)
	}
}

func formatOauthIdentity(auth *Auth, provider string, accountInfo string) string {
	if auth == nil {
		return ""
	}
	// Prefer the auth's provider when available.
	providerName := strings.TrimSpace(auth.Provider)
	if providerName == "" {
		providerName = strings.TrimSpace(provider)
	}
	// Only log the basename to avoid leaking host paths.
	// FileName may be unset for some auth backends; fall back to ID.
	authFile := strings.TrimSpace(auth.FileName)
	if authFile == "" {
		authFile = strings.TrimSpace(auth.ID)
	}
	if authFile != "" {
		authFile = filepath.Base(authFile)
	}
	parts := make([]string, 0, 3)
	if providerName != "" {
		parts = append(parts, "provider="+providerName)
	}
	if authFile != "" {
		parts = append(parts, "auth_file="+authFile)
	}
	if len(parts) == 0 {
		return accountInfo
	}
	return strings.Join(parts, " ")
}

// InjectCredentials delegates per-provider HTTP request preparation when supported.
// If the registered executor for the auth provider implements RequestPreparer,
// it will be invoked to modify the request (e.g., add headers).
func (m *Manager) InjectCredentials(req *http.Request, authID string) error {
	if req == nil || authID == "" {
		return nil
	}
	m.mu.RLock()
	a := m.auths[authID]
	var exec ProviderExecutor
	if a != nil {
		exec = m.executors[executorKeyFromAuth(a)]
	}
	m.mu.RUnlock()
	if a == nil || exec == nil {
		return nil
	}
	if p, ok := exec.(RequestPreparer); ok && p != nil {
		return p.PrepareRequest(req, a)
	}
	return nil
}

// PrepareHttpRequest injects provider credentials into the supplied HTTP request.
func (m *Manager) PrepareHttpRequest(ctx context.Context, auth *Auth, req *http.Request) error {
	if m == nil {
		return &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	if auth == nil {
		return &Error{Code: "auth_not_found", Message: "auth is nil"}
	}
	if req == nil {
		return &Error{Code: "invalid_request", Message: "http request is nil"}
	}
	if ctx != nil {
		*req = *req.WithContext(ctx)
	}
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	preparer, ok := exec.(RequestPreparer)
	if !ok || preparer == nil {
		return &Error{Code: "not_supported", Message: "executor does not support http request preparation"}
	}
	return preparer.PrepareRequest(req, auth)
}

// NewHttpRequest constructs a new HTTP request and injects provider credentials into it.
func (m *Manager) NewHttpRequest(ctx context.Context, auth *Auth, method, targetURL string, body []byte, headers http.Header) (*http.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	method = strings.TrimSpace(method)
	if method == "" {
		method = http.MethodGet
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, targetURL, reader)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		httpReq.Header = headers.Clone()
	}
	if errPrepare := m.PrepareHttpRequest(ctx, auth, httpReq); errPrepare != nil {
		return nil, errPrepare
	}
	return httpReq, nil
}

// HttpRequest injects provider credentials into the supplied HTTP request and executes it.
func (m *Manager) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	if m == nil {
		return nil, &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	if auth == nil {
		return nil, &Error{Code: "auth_not_found", Message: "auth is nil"}
	}
	if req == nil {
		return nil, &Error{Code: "invalid_request", Message: "http request is nil"}
	}
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return nil, &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return nil, &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	return exec.HttpRequest(ctx, auth, req)
}
