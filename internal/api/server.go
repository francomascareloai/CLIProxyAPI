// Package api provides the HTTP API server implementation for the CLI Proxy API.
// It includes the main server struct, routing setup, middleware for CORS and authentication,
// and integration with various AI API handlers (OpenAI, Claude, Gemini).
// The server supports hot-reloading of clients and configuration.
package api

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/access"
	configaccess "github.com/router-for-me/CLIProxyAPI/v7/internal/access/config_access"
	managementHandlers "github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/managementasset"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/safemode"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/gemini"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/openai"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/sync/singleflight"
	"gopkg.in/yaml.v3"
)

const oauthCallbackSuccessHTML = `<html><head><meta charset="utf-8"><title>Authentication successful</title><script>setTimeout(function(){window.close();},5000);</script></head><body><h1>Authentication successful!</h1><p>You can close this window.</p><p>This window will close automatically in 5 seconds.</p></body></html>`

const clientSummaryLogCooldown = 2 * time.Second
const processMetricsCacheTTL = 1 * time.Second
const autoProfileFilePrefix = "autoprofile-"

type modelsCacheEntry struct {
	payload   []byte
	version   uint64
	expiresAt time.Time
}

type runtimeMetricsProvider func() map[string]uint64

var corsExposedResponseHeaders = []string{
	"X-CPA-VERSION",
	"X-CPA-COMMIT",
	"X-CPA-BUILD-DATE",
	"X-CPA-SUPPORT-PLUGIN",
	"X-CPA-HOME-VERSION",
	"X-CPA-HOME-BUILD-DATE",
	"X-SERVER-VERSION",
	"X-SERVER-BUILD-DATE",
}

var corsExposedResponseHeadersJoined = strings.Join(corsExposedResponseHeaders, ", ")

const (
	exampleAPIKeyManagementPath = "/management.html"
	exampleAPIKeyManagementURL  = "/management.html?safe-mode=configure"
)

type serverOptionConfig struct {
	extraMiddleware       []gin.HandlerFunc
	engineConfigurator    func(*gin.Engine)
	routerConfigurator    func(*gin.Engine, *handlers.BaseAPIHandler, *config.Config)
	requestLoggerFactory  func(*config.Config, string) logging.RequestLogger
	localPassword         string
	keepAliveEnabled      bool
	keepAliveTimeout      time.Duration
	keepAliveOnTimeout    func()
	postAuthHook          auth.PostAuthHook
	postAuthPersistHook   auth.PostAuthHook
	pluginHost            *pluginhost.Host
	configReloadHook      func(context.Context, *config.Config)
	exampleAPIKeySafeMode bool
}

// ServerOption customises HTTP server construction.
type ServerOption func(*serverOptionConfig)

func defaultRequestLoggerFactory(cfg *config.Config, configPath string) logging.RequestLogger {
	configDir := filepath.Dir(configPath)
	logsDir := logging.ResolveLogDirectory(cfg)
	logger := logging.NewFileRequestLogger(cfg.RequestLog, logsDir, configDir, cfg.ErrorLogsMaxFiles)
	logger.SetHomeEnabled(cfg != nil && cfg.Home.Enabled)
	return logger
}

func effectiveSDKConfig(cfg *config.Config) *config.SDKConfig {
	if cfg == nil {
		return nil
	}
	sdkCfg := cfg.SDKConfig
	if cfg.CommercialMode {
		sdkCfg.RequestLog = false
	}
	return &sdkCfg
}

// WithMiddleware appends additional Gin middleware during server construction.
func WithMiddleware(mw ...gin.HandlerFunc) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.extraMiddleware = append(cfg.extraMiddleware, mw...)
	}
}

// WithEngineConfigurator allows callers to mutate the Gin engine prior to middleware setup.
func WithEngineConfigurator(fn func(*gin.Engine)) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.engineConfigurator = fn
	}
}

// WithRouterConfigurator appends a callback after default routes are registered.
func WithRouterConfigurator(fn func(*gin.Engine, *handlers.BaseAPIHandler, *config.Config)) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.routerConfigurator = fn
	}
}

// WithLocalManagementPassword stores a runtime-only management password accepted for localhost requests.
func WithLocalManagementPassword(password string) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.localPassword = password
	}
}

// WithKeepAliveEndpoint enables a keep-alive endpoint with the provided timeout and callback.
func WithKeepAliveEndpoint(timeout time.Duration, onTimeout func()) ServerOption {
	return func(cfg *serverOptionConfig) {
		if timeout <= 0 || onTimeout == nil {
			return
		}
		cfg.keepAliveEnabled = true
		cfg.keepAliveTimeout = timeout
		cfg.keepAliveOnTimeout = onTimeout
	}
}

// WithRequestLoggerFactory customises request logger creation.
func WithRequestLoggerFactory(factory func(*config.Config, string) logging.RequestLogger) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.requestLoggerFactory = factory
	}
}

// WithPostAuthHook registers a hook to be called after auth record creation.
func WithPostAuthHook(hook auth.PostAuthHook) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.postAuthHook = hook
	}
}

// WithPostAuthPersistHook registers a hook to be called after auth persistence.
func WithPostAuthPersistHook(hook auth.PostAuthHook) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.postAuthPersistHook = hook
	}
}

// WithPluginHost registers dynamic plugin HTTP adapters with the server.
func WithPluginHost(host *pluginhost.Host) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.pluginHost = host
	}
}

// WithConfigReloadHook registers a callback used after management saves config changes.
func WithConfigReloadHook(hook func(context.Context, *config.Config)) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.configReloadHook = hook
	}
}

// WithExampleAPIKeySafeMode blocks proxy API endpoints while template API keys remain configured.
func WithExampleAPIKeySafeMode() ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.exampleAPIKeySafeMode = true
	}
}

// Server represents the main API server.
// It encapsulates the Gin engine, HTTP server, handlers, and configuration.
type Server struct {
	// engine is the Gin web framework engine instance.
	engine *gin.Engine

	// server is the underlying HTTP server.
	server *http.Server

	// muxBaseListener is the shared TCP listener used to serve both HTTP and Redis protocol traffic.
	muxBaseListener net.Listener

	// muxHTTPListener receives HTTP connections selected by the multiplexer.
	muxHTTPListener *muxListener

	// handlers contains the API handlers for processing requests.
	handlers *handlers.BaseAPIHandler

	// cfg holds the current server configuration.
	cfg *config.Config

	// oldConfigYaml stores a YAML snapshot of the previous configuration for change detection.
	// This prevents issues when the config object is modified in place by Management API.
	oldConfigYaml []byte

	// accessManager handles request authentication providers.
	accessManager *sdkaccess.Manager

	// requestLogger is the request logger instance for dynamic configuration updates.
	requestLogger logging.RequestLogger
	loggerToggle  func(bool)

	// configFilePath is the absolute path to the YAML config file for persistence.
	configFilePath string

	// currentPath is the absolute path to the current working directory.
	currentPath string

	// wsRoutes tracks registered websocket upgrade paths.
	wsRouteMu     sync.Mutex
	wsRoutes      map[string]struct{}
	wsAuthChanged func(bool, bool)
	wsAuthEnabled atomic.Bool

	// management handler
	mgmt *managementHandlers.Handler

	// pluginHost owns dynamic plugin Management API route dispatch.
	pluginHost *pluginhost.Host

	// managementRoutesRegistered tracks whether the management routes have been attached to the engine.
	managementRoutesRegistered atomic.Bool
	// managementRoutesEnabled controls whether management endpoints serve real handlers.
	managementRoutesEnabled atomic.Bool

	// envManagementSecret indicates whether MANAGEMENT_PASSWORD is configured.
	envManagementSecret bool

	localPassword string

	keepAliveEnabled   bool
	keepAliveTimeout   time.Duration
	keepAliveOnTimeout func()
	keepAliveHeartbeat chan struct{}
	keepAliveStop      chan struct{}

	clientSummaryMu        sync.Mutex
	lastClientSummaryKey   string
	lastClientSummaryAt    time.Time
	fullUpdateCount        atomic.Uint64
	incrementalUpdateCount atomic.Uint64
	modelsCacheMu          sync.RWMutex
	openAIModelsCache      modelsCacheEntry
	claudeModelsCache      modelsCacheEntry
	modelsCacheVersion     atomic.Uint64
	modelsCacheBuildCount  atomic.Uint64
	modelsCacheInvalidated atomic.Uint64
	modelsCacheHit         atomic.Uint64
	modelsCacheMiss        atomic.Uint64
	modelsBuildGroup       singleflight.Group
	modelsBuildHook        func()
	runtimeMetricsMu       sync.RWMutex
	runtimeMetricsProvider runtimeMetricsProvider
	processMetricsMu       sync.Mutex
	processMetricsCachedAt time.Time
	processGoroutines      int
	processOpenFDs         int
	autoProfileMu          sync.Mutex
	autoProfileLastCapture time.Time
	autoProfileCaptured    atomic.Uint64
	autoProfileSkipped     atomic.Uint64
	autoProfileErrors      atomic.Uint64

	exampleAPIKeySafeModeEnabled bool
	exampleAPIKeySafeModeActive  atomic.Bool
}

// NewServer creates and initializes a new API server instance.
// It sets up the Gin engine, middleware, routes, and handlers.
//
// Parameters:
//   - cfg: The server configuration
//   - authManager: core runtime auth manager
//   - accessManager: request authentication manager
//
// Returns:
//   - *Server: A new server instance
func NewServer(cfg *config.Config, authManager *auth.Manager, accessManager *sdkaccess.Manager, configFilePath string, opts ...ServerOption) *Server {
	optionState := &serverOptionConfig{
		requestLoggerFactory: defaultRequestLoggerFactory,
	}
	for i := range opts {
		opts[i](optionState)
	}
	// Ensure built-in inline API key auth provider is always available.
	configaccess.Register(&cfg.SDKConfig)
	// Set gin mode
	if !cfg.Debug {
		gin.SetMode(gin.ReleaseMode)
	}

	// Create gin engine
	engine := gin.New()
	if optionState.engineConfigurator != nil {
		optionState.engineConfigurator(engine)
	}

	// Add middleware
	engine.Use(logging.GinLogrusLogger())
	engine.Use(logging.GinLogrusRecovery())
	for _, mw := range optionState.extraMiddleware {
		engine.Use(mw)
	}

	// Add request logging middleware (positioned after recovery, before auth)
	// Resolve logs directory relative to the configuration file directory.
	var requestLogger logging.RequestLogger
	var toggle func(bool)
	if !cfg.CommercialMode {
		if optionState.requestLoggerFactory != nil {
			requestLogger = optionState.requestLoggerFactory(cfg, configFilePath)
		}
		if requestLogger != nil {
			engine.Use(middleware.RequestLoggingMiddleware(requestLogger))
			if setter, ok := requestLogger.(interface{ SetEnabled(bool) }); ok {
				toggle = setter.SetEnabled
			}
		}
	}

	engine.Use(corsMiddleware())
	wd, err := os.Getwd()
	if err != nil {
		wd = configFilePath
	}

	envAdminPassword, envAdminPasswordSet := os.LookupEnv("MANAGEMENT_PASSWORD")
	envAdminPassword = strings.TrimSpace(envAdminPassword)
	envManagementSecret := envAdminPasswordSet && envAdminPassword != ""

	// Create server instance
	s := &Server{
		engine:              engine,
		handlers:            handlers.NewBaseAPIHandlers(effectiveSDKConfig(cfg), authManager),
		cfg:                 cfg,
		accessManager:       accessManager,
		requestLogger:       requestLogger,
		loggerToggle:        toggle,
		configFilePath:      configFilePath,
		currentPath:         wd,
		envManagementSecret: envManagementSecret,
		wsRoutes:            make(map[string]struct{}),
		pluginHost:          optionState.pluginHost,

		exampleAPIKeySafeModeEnabled: optionState.exampleAPIKeySafeMode,
	}
	s.wsAuthEnabled.Store(cfg.WebsocketAuth)
	s.exampleAPIKeySafeModeActive.Store(s.exampleAPIKeySafeModeRequired(cfg))
	s.handlers.SetPluginHost(optionState.pluginHost)
	if optionState.pluginHost != nil {
		optionState.pluginHost.SetModelExecutor(s.handlers)
		optionState.pluginHost.SetAuthManager(authManager)
	}
	// Save initial YAML snapshot
	s.oldConfigYaml, _ = yaml.Marshal(cfg)
	s.applyAccessConfig(nil, cfg)
	engine.Use(s.autoProfileMiddleware())
	if authManager != nil {
		authManager.SetRetryConfig(cfg.RequestRetry, time.Duration(cfg.MaxRetryInterval)*time.Second, cfg.MaxRetryCredentials)
	}
	managementasset.SetCurrentConfig(cfg)
	auth.SetQuotaCooldownDisabled(cfg.DisableCooling)
	auth.SetTransientErrorCooldownSeconds(cfg.TransientErrorCooldownSeconds)
	applySignatureCacheConfig(nil, cfg)
	// Initialize management handler
	s.mgmt = managementHandlers.NewHandler(cfg, configFilePath, authManager)
	s.mgmt.SetPluginHost(optionState.pluginHost)
	s.mgmt.SetConfigReloadHook(optionState.configReloadHook)
	if optionState.localPassword != "" {
		s.mgmt.SetLocalPassword(optionState.localPassword)
	}
	logDir := logging.ResolveLogDirectory(cfg)
	s.mgmt.SetLogDirectory(logDir)
	if optionState.postAuthHook != nil {
		s.mgmt.SetPostAuthHook(optionState.postAuthHook)
	}
	if optionState.postAuthPersistHook != nil {
		s.mgmt.SetPostAuthPersistHook(optionState.postAuthPersistHook)
	}
	s.localPassword = optionState.localPassword

	// Home heartbeat gate: when home is enabled, block all endpoints with 503 until the
	// subscribe-config heartbeat connection is healthy.
	engine.Use(s.homeHeartbeatMiddleware())
	engine.Use(s.exampleAPIKeySafeModeMiddleware())

	// Setup routes
	s.setupRoutes()

	// Apply additional router configurators from options
	if optionState.routerConfigurator != nil {
		optionState.routerConfigurator(engine, s.handlers, cfg)
	}

	// Register management routes when configuration or environment secrets are available,
	// or when a local management password is provided (e.g. TUI mode).
	hasManagementSecret := cfg.RemoteManagement.SecretKey != "" || envManagementSecret || s.localPassword != ""
	s.managementRoutesEnabled.Store(hasManagementSecret)
	redisqueue.SetEnabled(hasManagementSecret || (cfg != nil && cfg.Home.Enabled))
	if hasManagementSecret {
		s.registerManagementRoutes()
	}
	s.refreshPluginManagementRoutes()
	engine.NoRoute(s.pluginManagementNoRoute)

	if optionState.keepAliveEnabled {
		s.enableKeepAlive(optionState.keepAliveTimeout, optionState.keepAliveOnTimeout)
	}

	// Create HTTP server
	s.server = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler: engine,
	}

	return s
}

func (s *Server) homeHeartbeatMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || s.cfg == nil || !s.cfg.Home.Enabled {
			c.Next()
			return
		}
		if c != nil && c.Request != nil {
			path := c.Request.URL.Path
			if strings.HasPrefix(path, "/v0/management/") || path == "/v0/management" || strings.HasPrefix(path, "/v0/resource/plugins/") || path == "/management.html" {
				c.Next()
				return
			}
		}
		client := home.Current()
		if client == nil || !client.HeartbeatOK() {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		c.Next()
	}
}

func (s *Server) exampleAPIKeySafeModeRequired(cfg *config.Config) bool {
	return s != nil && s.exampleAPIKeySafeModeEnabled && cfg != nil && safemode.HasExampleAPIKeys(cfg.APIKeys)
}

func (s *Server) exampleAPIKeySafeModeMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || !s.exampleAPIKeySafeModeActive.Load() || c == nil || c.Request == nil || c.Request.URL == nil {
			c.Next()
			return
		}

		path := c.Request.URL.Path
		if path == exampleAPIKeyManagementPath && c.Query("safe-mode") == "configure" {
			c.Next()
			return
		}
		if (path == "/" || path == exampleAPIKeyManagementPath) && (c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead) {
			s.serveExampleAPIKeyWarningPage(c)
			return
		}
		if !isExampleAPIKeySafeModeProxyPath(path) {
			c.Next()
			return
		}

		c.Header("X-CPA-SAFE-MODE", "example-api-key")
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error":   "unsafe_example_api_key",
			"message": "Proxy API endpoints are disabled because api-keys contains template values. Open /management.html?safe-mode=configure, update api-keys in Management, then retry.",
		})
	}
}

func (s *Server) serveExampleAPIKeyWarningPage(c *gin.Context) {
	cfg := s.cfg
	var keys []string
	if cfg != nil {
		keys = safemode.ExampleAPIKeys(cfg.APIKeys)
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		c.Abort()
		return
	}
	c.String(http.StatusOK, safemode.ExampleAPIKeyWarningPageHTML(keys, exampleAPIKeyManagementURL))
	c.Abort()
}

func isExampleAPIKeySafeModeProxyPath(path string) bool {
	switch {
	case path == "/v1" || strings.HasPrefix(path, "/v1/"):
		return true
	case path == "/v1beta" || strings.HasPrefix(path, "/v1beta/"):
		return true
	case path == "/openai/v1" || strings.HasPrefix(path, "/openai/v1/"):
		return true
	case path == "/backend-api/codex" || strings.HasPrefix(path, "/backend-api/codex/"):
		return true
	default:
		return false
	}
}

// setupRoutes configures the API routes for the server.
// It defines the endpoints and associates them with their respective handlers.
func (s *Server) setupRoutes() {
	healthzHandler := func(c *gin.Context) {
		if c.Request.Method == http.MethodHead {
			c.Status(http.StatusOK)
			return
		}

		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
	s.engine.GET("/healthz", healthzHandler)
	s.engine.HEAD("/healthz", healthzHandler)

	s.engine.GET("/management.html", s.serveManagementControlPanel)
	s.engine.GET("/usage-extended.html", s.serveStaticAsset("usage-extended.html"))
	s.engine.GET("/dashboard", s.serveStaticAsset("index.html"))
	s.engine.GET("/index.html", s.serveStaticAsset("index.html"))
	s.engine.GET("/metrics", s.metricsHandler)
	openaiHandlers := openai.NewOpenAIAPIHandler(s.handlers)
	geminiHandlers := gemini.NewGeminiAPIHandler(s.handlers)
	claudeCodeHandlers := claude.NewClaudeCodeAPIHandler(s.handlers)
	openaiResponsesHandlers := openai.NewOpenAIResponsesAPIHandler(s.handlers)

	// OpenAI compatible API routes
	v1 := s.engine.Group("/v1")
	v1.Use(AuthMiddleware(s.accessManager))
	{
		v1.GET("/models", s.unifiedModelsHandler(openaiHandlers, claudeCodeHandlers))
		v1.POST("/chat/completions", openaiHandlers.ChatCompletions)
		v1.POST("/completions", openaiHandlers.Completions)
		v1.POST("/images/generations", openaiHandlers.ImagesGenerations)
		v1.POST("/images/edits", openaiHandlers.ImagesEdits)
		v1.POST("/videos", openaiHandlers.XAIVideosGenerations)
		v1.POST("/videos/generations", openaiHandlers.XAIVideosGenerations)
		v1.POST("/videos/edits", openaiHandlers.XAIVideosEdits)
		v1.POST("/videos/extensions", openaiHandlers.XAIVideosExtensions)
		v1.GET("/videos/:request_id", openaiHandlers.XAIVideosRetrieve)
		v1.POST("/messages", claudeCodeHandlers.ClaudeMessages)
		v1.POST("/messages/count_tokens", claudeCodeHandlers.ClaudeCountTokens)
		v1.GET("/responses", openaiResponsesHandlers.ResponsesWebsocket)
		v1.POST("/responses", openaiResponsesHandlers.Responses)
		v1.POST("/responses/compact", openaiResponsesHandlers.Compact)
	}

	openaiV1 := s.engine.Group("/openai/v1")
	openaiV1.Use(AuthMiddleware(s.accessManager))
	{
		openaiV1.POST("/videos", openaiHandlers.VideosCreate)
		openaiV1.GET("/videos/:video_id/content", openaiHandlers.VideosContent)
		openaiV1.GET("/videos/:video_id", openaiHandlers.VideosRetrieve)
	}

	// Codex CLI direct route aliases (chatgpt_base_url compatible)
	codexDirect := s.engine.Group("/backend-api/codex")
	codexDirect.Use(AuthMiddleware(s.accessManager))
	{
		codexDirect.GET("/responses", openaiResponsesHandlers.ResponsesWebsocket)
		codexDirect.POST("/responses", openaiResponsesHandlers.Responses)
		codexDirect.POST("/responses/compact", openaiResponsesHandlers.Compact)
	}

	// Gemini compatible API routes
	v1beta := s.engine.Group("/v1beta")
	v1beta.Use(AuthMiddleware(s.accessManager))
	{
		v1beta.GET("/models", s.geminiModelsHandler(geminiHandlers))
		v1beta.POST("/interactions", geminiHandlers.Interactions)
		v1beta.POST("/models/*action", geminiHandlers.GeminiHandler)
		v1beta.GET("/models/*action", s.geminiGetHandler(geminiHandlers))
	}

	// Root endpoint
	s.engine.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"message": "CLI Proxy API Server",
			"endpoints": []string{
				"POST /v1/chat/completions",
				"POST /v1/completions",
				"GET /v1/models",
			},
		})
	})

	// OAuth callback endpoints (reuse main server port)
	// These endpoints receive provider redirects and persist
	// the short-lived code/state for the waiting goroutine.
	s.engine.GET("/anthropic/callback", func(c *gin.Context) {
		code := c.Query("code")
		state := c.Query("state")
		errStr := c.Query("error")
		if errStr == "" {
			errStr = c.Query("error_description")
		}
		if state != "" {
			_, _ = managementHandlers.WriteOAuthCallbackFileForPendingSession(s.cfg.AuthDir, "anthropic", state, code, errStr)
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, oauthCallbackSuccessHTML)
	})

	s.engine.GET("/codex/callback", func(c *gin.Context) {
		code := c.Query("code")
		state := c.Query("state")
		errStr := c.Query("error")
		if errStr == "" {
			errStr = c.Query("error_description")
		}
		if state != "" {
			_, _ = managementHandlers.WriteOAuthCallbackFileForPendingSession(s.cfg.AuthDir, "codex", state, code, errStr)
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, oauthCallbackSuccessHTML)
	})

	s.engine.GET("/antigravity/callback", func(c *gin.Context) {
		code := c.Query("code")
		state := c.Query("state")
		errStr := c.Query("error")
		if errStr == "" {
			errStr = c.Query("error_description")
		}
		if state != "" {
			_, _ = managementHandlers.WriteOAuthCallbackFileForPendingSession(s.cfg.AuthDir, "antigravity", state, code, errStr)
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, oauthCallbackSuccessHTML)
	})

	s.engine.GET("/xai/callback", func(c *gin.Context) {
		code := c.Query("code")
		state := c.Query("state")
		errStr := c.Query("error")
		if errStr == "" {
			errStr = c.Query("error_description")
		}
		if state != "" {
			_, _ = managementHandlers.WriteOAuthCallbackFileForPendingSession(s.cfg.AuthDir, "xai", state, code, errStr)
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, oauthCallbackSuccessHTML)
	})

	// Management routes are registered lazily by registerManagementRoutes when a secret is configured.
}

// AttachWebsocketRoute registers a websocket upgrade handler on the primary Gin engine.
// The handler is served as-is without additional middleware beyond the standard stack already configured.
func (s *Server) AttachWebsocketRoute(path string, handler http.Handler) {
	if s == nil || s.engine == nil || handler == nil {
		return
	}
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		trimmed = "/v1/ws"
	}
	if !strings.HasPrefix(trimmed, "/") {
		trimmed = "/" + trimmed
	}
	s.wsRouteMu.Lock()
	if _, exists := s.wsRoutes[trimmed]; exists {
		s.wsRouteMu.Unlock()
		return
	}
	s.wsRoutes[trimmed] = struct{}{}
	s.wsRouteMu.Unlock()

	authMiddleware := AuthMiddleware(s.accessManager)
	conditionalAuth := func(c *gin.Context) {
		if !s.wsAuthEnabled.Load() {
			c.Next()
			return
		}
		authMiddleware(c)
	}
	finalHandler := func(c *gin.Context) {
		handler.ServeHTTP(c.Writer, c.Request)
		c.Abort()
	}

	s.engine.GET(trimmed, conditionalAuth, finalHandler)
}

func (s *Server) registerManagementRoutes() {
	if s == nil || s.engine == nil || s.mgmt == nil {
		return
	}
	if !s.managementRoutesRegistered.CompareAndSwap(false, true) {
		return
	}

	log.Info("management routes registered after secret key configuration")

	s.engine.POST("/v0/management/oauth-callback", s.managementAvailabilityMiddleware(), s.mgmt.PostOAuthCallback)
	s.engine.GET("/v0/management/oauth-callback", s.managementAvailabilityMiddleware(), s.mgmt.GetOAuthCallback)

	mgmt := s.engine.Group("/v0/management")
	mgmt.Use(s.managementAvailabilityMiddleware(), s.mgmt.Middleware())
	{
		mgmt.GET("/usage", s.mgmt.GetUsageStatistics)
		mgmt.GET("/usage/export", s.mgmt.ExportUsageStatistics)
		mgmt.POST("/usage/import", s.mgmt.ImportUsageStatistics)
		mgmt.GET("/usage/accounts", s.mgmt.GetAccountStats)
		mgmt.GET("/usage/export.csv", s.mgmt.ExportUsageCSV)
		mgmt.POST("/usage/compact", s.mgmt.CompactUsageData)
		mgmt.GET("/cooldown", s.mgmt.GetCooldownStatus)
		mgmt.GET("/config", s.mgmt.GetConfig)
		mgmt.GET("/config.yaml", s.mgmt.GetConfigYAML)
		mgmt.PUT("/config.yaml", s.mgmt.PutConfigYAML)
		mgmt.GET("/latest-version", s.mgmt.GetLatestVersion)
		mgmt.GET("/plugins", s.mgmt.ListPlugins)
		mgmt.GET("/plugin-store", s.mgmt.ListPluginStore)
		mgmt.POST("/plugin-store/:id/install", s.mgmt.InstallPluginFromStore)
		mgmt.DELETE("/plugins/:id", s.mgmt.DeletePlugin)
		mgmt.PATCH("/plugins/:id/enabled", s.mgmt.PatchPluginEnabled)
		mgmt.GET("/plugins/:id/config", s.mgmt.GetPluginConfig)
		mgmt.PUT("/plugins/:id/config", s.mgmt.PutPluginConfig)
		mgmt.PATCH("/plugins/:id/config", s.mgmt.PatchPluginConfig)

		mgmt.GET("/debug", s.mgmt.GetDebug)
		mgmt.PUT("/debug", s.mgmt.PutDebug)
		mgmt.PATCH("/debug", s.mgmt.PutDebug)

		mgmt.GET("/logging-to-file", s.mgmt.GetLoggingToFile)
		mgmt.PUT("/logging-to-file", s.mgmt.PutLoggingToFile)
		mgmt.PATCH("/logging-to-file", s.mgmt.PutLoggingToFile)

		mgmt.GET("/logs-max-total-size-mb", s.mgmt.GetLogsMaxTotalSizeMB)
		mgmt.PUT("/logs-max-total-size-mb", s.mgmt.PutLogsMaxTotalSizeMB)
		mgmt.PATCH("/logs-max-total-size-mb", s.mgmt.PutLogsMaxTotalSizeMB)

		mgmt.GET("/error-logs-max-files", s.mgmt.GetErrorLogsMaxFiles)
		mgmt.PUT("/error-logs-max-files", s.mgmt.PutErrorLogsMaxFiles)
		mgmt.PATCH("/error-logs-max-files", s.mgmt.PutErrorLogsMaxFiles)

		mgmt.GET("/usage-statistics-enabled", s.mgmt.GetUsageStatisticsEnabled)
		mgmt.PUT("/usage-statistics-enabled", s.mgmt.PutUsageStatisticsEnabled)
		mgmt.PATCH("/usage-statistics-enabled", s.mgmt.PutUsageStatisticsEnabled)
		mgmt.GET("/usage-journal-retention-days", s.mgmt.GetUsageJournalRetentionDays)
		mgmt.PUT("/usage-journal-retention-days", s.mgmt.PutUsageJournalRetentionDays)
		mgmt.PATCH("/usage-journal-retention-days", s.mgmt.PutUsageJournalRetentionDays)
		mgmt.GET("/usage-journal-replay-max-days", s.mgmt.GetUsageJournalReplayMaxDays)
		mgmt.PUT("/usage-journal-replay-max-days", s.mgmt.PutUsageJournalReplayMaxDays)
		mgmt.PATCH("/usage-journal-replay-max-days", s.mgmt.PutUsageJournalReplayMaxDays)

		mgmt.GET("/proxy-url", s.mgmt.GetProxyURL)
		mgmt.PUT("/proxy-url", s.mgmt.PutProxyURL)
		mgmt.PATCH("/proxy-url", s.mgmt.PutProxyURL)
		mgmt.DELETE("/proxy-url", s.mgmt.DeleteProxyURL)

		mgmt.POST("/api-call", s.mgmt.APICall)

		mgmt.GET("/quota-exceeded/switch-project", s.mgmt.GetSwitchProject)
		mgmt.PUT("/quota-exceeded/switch-project", s.mgmt.PutSwitchProject)
		mgmt.PATCH("/quota-exceeded/switch-project", s.mgmt.PutSwitchProject)

		mgmt.GET("/quota-exceeded/switch-preview-model", s.mgmt.GetSwitchPreviewModel)
		mgmt.PUT("/quota-exceeded/switch-preview-model", s.mgmt.PutSwitchPreviewModel)
		mgmt.PATCH("/quota-exceeded/switch-preview-model", s.mgmt.PutSwitchPreviewModel)
		mgmt.POST("/reset-quota", s.mgmt.ResetQuota)

		mgmt.GET("/api-keys", s.mgmt.GetAPIKeys)
		mgmt.PUT("/api-keys", s.mgmt.PutAPIKeys)
		mgmt.PATCH("/api-keys", s.mgmt.PatchAPIKeys)
		mgmt.DELETE("/api-keys", s.mgmt.DeleteAPIKeys)
		mgmt.GET("/api-key-usage", s.mgmt.GetAPIKeyUsage)
		mgmt.GET("/usage-queue", s.mgmt.GetUsageQueue)

		mgmt.GET("/gemini-api-key", s.mgmt.GetGeminiKeys)
		mgmt.PUT("/gemini-api-key", s.mgmt.PutGeminiKeys)
		mgmt.PATCH("/gemini-api-key", s.mgmt.PatchGeminiKey)
		mgmt.DELETE("/gemini-api-key", s.mgmt.DeleteGeminiKey)

		mgmt.GET("/interactions-api-key", s.mgmt.GetInteractionsKeys)
		mgmt.PUT("/interactions-api-key", s.mgmt.PutInteractionsKeys)
		mgmt.PATCH("/interactions-api-key", s.mgmt.PatchInteractionsKey)
		mgmt.DELETE("/interactions-api-key", s.mgmt.DeleteInteractionsKey)

		mgmt.GET("/logs", s.mgmt.GetLogs)
		mgmt.DELETE("/logs", s.mgmt.DeleteLogs)
		mgmt.GET("/request-error-logs", s.mgmt.GetRequestErrorLogs)
		mgmt.GET("/request-error-logs/:name", s.mgmt.DownloadRequestErrorLog)
		mgmt.GET("/request-log-by-id/:id", s.mgmt.GetRequestLogByID)
		mgmt.GET("/request-log", s.mgmt.GetRequestLog)
		mgmt.PUT("/request-log", s.mgmt.PutRequestLog)
		mgmt.PATCH("/request-log", s.mgmt.PutRequestLog)
		mgmt.GET("/ws-auth", s.mgmt.GetWebsocketAuth)
		mgmt.PUT("/ws-auth", s.mgmt.PutWebsocketAuth)
		mgmt.PATCH("/ws-auth", s.mgmt.PutWebsocketAuth)

		mgmt.GET("/request-retry", s.mgmt.GetRequestRetry)
		mgmt.PUT("/request-retry", s.mgmt.PutRequestRetry)
		mgmt.PATCH("/request-retry", s.mgmt.PutRequestRetry)
		mgmt.GET("/max-retry-interval", s.mgmt.GetMaxRetryInterval)
		mgmt.PUT("/max-retry-interval", s.mgmt.PutMaxRetryInterval)
		mgmt.PATCH("/max-retry-interval", s.mgmt.PutMaxRetryInterval)

		mgmt.GET("/force-model-prefix", s.mgmt.GetForceModelPrefix)
		mgmt.PUT("/force-model-prefix", s.mgmt.PutForceModelPrefix)
		mgmt.PATCH("/force-model-prefix", s.mgmt.PutForceModelPrefix)

		mgmt.GET("/routing/strategy", s.mgmt.GetRoutingStrategy)
		mgmt.PUT("/routing/strategy", s.mgmt.PutRoutingStrategy)
		mgmt.PATCH("/routing/strategy", s.mgmt.PutRoutingStrategy)

		mgmt.GET("/claude-api-key", s.mgmt.GetClaudeKeys)
		mgmt.PUT("/claude-api-key", s.mgmt.PutClaudeKeys)
		mgmt.PATCH("/claude-api-key", s.mgmt.PatchClaudeKey)
		mgmt.DELETE("/claude-api-key", s.mgmt.DeleteClaudeKey)

		mgmt.GET("/codex-api-key", s.mgmt.GetCodexKeys)
		mgmt.PUT("/codex-api-key", s.mgmt.PutCodexKeys)
		mgmt.PATCH("/codex-api-key", s.mgmt.PatchCodexKey)
		mgmt.DELETE("/codex-api-key", s.mgmt.DeleteCodexKey)

		mgmt.GET("/openai-compatibility", s.mgmt.GetOpenAICompat)
		mgmt.PUT("/openai-compatibility", s.mgmt.PutOpenAICompat)
		mgmt.PATCH("/openai-compatibility", s.mgmt.PatchOpenAICompat)
		mgmt.DELETE("/openai-compatibility", s.mgmt.DeleteOpenAICompat)

		mgmt.GET("/vertex-api-key", s.mgmt.GetVertexCompatKeys)
		mgmt.PUT("/vertex-api-key", s.mgmt.PutVertexCompatKeys)
		mgmt.PATCH("/vertex-api-key", s.mgmt.PatchVertexCompatKey)
		mgmt.DELETE("/vertex-api-key", s.mgmt.DeleteVertexCompatKey)

		mgmt.GET("/oauth-excluded-models", s.mgmt.GetOAuthExcludedModels)
		mgmt.PUT("/oauth-excluded-models", s.mgmt.PutOAuthExcludedModels)
		mgmt.PATCH("/oauth-excluded-models", s.mgmt.PatchOAuthExcludedModels)
		mgmt.DELETE("/oauth-excluded-models", s.mgmt.DeleteOAuthExcludedModels)

		mgmt.GET("/oauth-model-alias", s.mgmt.GetOAuthModelAlias)
		mgmt.PUT("/oauth-model-alias", s.mgmt.PutOAuthModelAlias)
		mgmt.PATCH("/oauth-model-alias", s.mgmt.PatchOAuthModelAlias)
		mgmt.DELETE("/oauth-model-alias", s.mgmt.DeleteOAuthModelAlias)

		mgmt.GET("/auth-files", s.mgmt.ListAuthFiles)
		mgmt.GET("/auth-files/models", s.mgmt.GetAuthFileModels)
		mgmt.GET("/model-definitions/:channel", s.mgmt.GetStaticModelDefinitions)
		mgmt.GET("/auth-files/download", s.mgmt.DownloadAuthFile)
		mgmt.POST("/auth-files", s.mgmt.UploadAuthFile)
		mgmt.DELETE("/auth-files", s.mgmt.DeleteAuthFile)
		mgmt.PATCH("/auth-files/status", s.mgmt.PatchAuthFileStatus)
		mgmt.PATCH("/auth-files/fields", s.mgmt.PatchAuthFileFields)
		mgmt.POST("/vertex/import", s.mgmt.ImportVertexCredential)

		mgmt.GET("/anthropic-auth-url", s.mgmt.RequestAnthropicToken)
		mgmt.GET("/codex-auth-url", s.mgmt.RequestCodexToken)
		mgmt.GET("/antigravity-auth-url", s.mgmt.RequestAntigravityToken)
		mgmt.GET("/kimi-auth-url", s.mgmt.RequestKimiToken)
		mgmt.GET("/xai-auth-url", s.mgmt.RequestXAIToken)
		mgmt.GET("/get-auth-status", s.mgmt.GetAuthStatus)
	}
}

func (s *Server) managementAvailabilityMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.managementAvailable(c) {
			return
		}
		c.Next()
	}
}

func (s *Server) managementAvailable(c *gin.Context) bool {
	if s == nil || s.cfg == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return false
	}
	if s.cfg.Home.Enabled {
		c.AbortWithStatus(http.StatusNotFound)
		return false
	}
	if !s.managementRoutesEnabled.Load() {
		c.AbortWithStatus(http.StatusNotFound)
		return false
	}
	return true
}

func (s *Server) refreshPluginManagementRoutes() {
	if s == nil || s.pluginHost == nil || s.engine == nil {
		return
	}
	s.pluginHost.RegisterManagementRoutes(context.Background(), s.registeredManagementRouteKeys())
}

// RefreshPluginManagementRoutes rebuilds plugin-owned Management API routes.
func (s *Server) RefreshPluginManagementRoutes() {
	s.refreshPluginManagementRoutes()
}

func (s *Server) registeredManagementRouteKeys() map[string]struct{} {
	out := make(map[string]struct{})
	if s == nil || s.engine == nil {
		return out
	}
	for _, route := range s.engine.Routes() {
		if strings.HasPrefix(route.Path, "/v0/management/") || route.Path == "/v0/management" {
			out[strings.ToUpper(strings.TrimSpace(route.Method))+" "+route.Path] = struct{}{}
		}
	}
	return out
}

func (s *Server) pluginManagementNoRoute(c *gin.Context) {
	if s == nil || c == nil || c.Request == nil || c.Request.URL == nil {
		if c != nil {
			c.AbortWithStatus(http.StatusNotFound)
		}
		return
	}
	path := c.Request.URL.Path
	if strings.HasPrefix(path, "/v0/resource/plugins/") {
		s.pluginResourceNoRoute(c)
		return
	}
	if path != "/v0/management" && !strings.HasPrefix(path, "/v0/management/") {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if s.pluginHost == nil || s.mgmt == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if !s.managementAvailable(c) {
		return
	}
	s.mgmt.Middleware()(c)
	if c.IsAborted() {
		return
	}
	if s.mgmt.ServePluginAuthURL(c) {
		c.Abort()
		return
	}
	if s.pluginHost.ServeManagementHTTP(c.Writer, c.Request) {
		c.Abort()
		return
	}
	c.AbortWithStatus(http.StatusNotFound)
}

func (s *Server) pluginResourceNoRoute(c *gin.Context) {
	if s == nil || c == nil || c.Request == nil || c.Request.URL == nil {
		if c != nil {
			c.AbortWithStatus(http.StatusNotFound)
		}
		return
	}
	if s.cfg == nil || s.cfg.Home.Enabled || s.pluginHost == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if s.pluginHost.ServeResourceHTTP(c.Writer, c.Request) {
		c.Abort()
		return
	}
	c.AbortWithStatus(http.StatusNotFound)
}

func (s *Server) serveManagementControlPanel(c *gin.Context) {
	cfg := s.cfg
	if cfg == nil || cfg.Home.Enabled || cfg.RemoteManagement.DisableControlPanel {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	staticDir := managementasset.StaticDir(s.configFilePath)
	if strings.TrimSpace(staticDir) == "" {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if !managementasset.EnsureLatestManagementHTML(context.Background(), staticDir, cfg.ProxyURL, cfg.RemoteManagement.PanelGitHubRepository) {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "management control panel unavailable"})
		return
	}
	filePath := managementasset.FilePath(s.configFilePath)
	if strings.TrimSpace(filePath) == "" {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	if _, err := os.Stat(filePath); err != nil {
		if os.IsNotExist(err) {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "management control panel unavailable"})
			return
		}
		log.WithError(err).Error("failed to stat management control panel asset")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	if err := managementasset.ValidateManagementHTMLFile(filePath); err != nil {
		log.WithError(err).Warn("refusing to serve invalid management control panel asset")
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "management control panel unavailable"})
		return
	}

	c.File(filePath)
}

// serveStaticAsset returns a handler that serves a static file from the static directory.
func (s *Server) serveStaticAsset(filename string) gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := s.cfg
		if cfg == nil || cfg.RemoteManagement.DisableControlPanel {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		staticDir := managementasset.StaticDir(s.configFilePath)
		if strings.TrimSpace(staticDir) == "" {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		filePath := filepath.Join(staticDir, filename)
		if _, err := os.Stat(filePath); err != nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.File(filePath)
	}
}

func (s *Server) enableKeepAlive(timeout time.Duration, onTimeout func()) {
	if timeout <= 0 || onTimeout == nil {
		return
	}

	s.keepAliveEnabled = true
	s.keepAliveTimeout = timeout
	s.keepAliveOnTimeout = onTimeout
	s.keepAliveHeartbeat = make(chan struct{}, 1)
	s.keepAliveStop = make(chan struct{}, 1)

	s.engine.GET("/keep-alive", s.handleKeepAlive)

	go s.watchKeepAlive()
}

func (s *Server) handleKeepAlive(c *gin.Context) {
	if s.localPassword != "" {
		provided := strings.TrimSpace(c.GetHeader("Authorization"))
		if provided != "" {
			parts := strings.SplitN(provided, " ", 2)
			if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
				provided = parts[1]
			}
		}
		if provided == "" {
			provided = strings.TrimSpace(c.GetHeader("X-Local-Password"))
		}
		if subtle.ConstantTimeCompare([]byte(provided), []byte(s.localPassword)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid password"})
			return
		}
	}

	s.signalKeepAlive()
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Server) signalKeepAlive() {
	if !s.keepAliveEnabled {
		return
	}
	select {
	case s.keepAliveHeartbeat <- struct{}{}:
	default:
	}
}

// SetRuntimeMetricsProvider registers an optional metrics provider.
// The provider is invoked on each /metrics scrape and should be lightweight.
func (s *Server) SetRuntimeMetricsProvider(provider func() map[string]uint64) {
	if s == nil {
		return
	}
	s.runtimeMetricsMu.Lock()
	s.runtimeMetricsProvider = provider
	s.runtimeMetricsMu.Unlock()
}

func (s *Server) snapshotRuntimeMetrics() map[string]uint64 {
	if s == nil {
		return nil
	}
	s.runtimeMetricsMu.RLock()
	provider := s.runtimeMetricsProvider
	s.runtimeMetricsMu.RUnlock()
	if provider == nil {
		return nil
	}
	values := provider()
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]uint64, len(values))
	for k, v := range values {
		out[strings.TrimSpace(k)] = v
	}
	return out
}

func writePromCounter(builder *strings.Builder, name, help string, value uint64) {
	fmt.Fprintf(builder, "# HELP %s %s\n", name, help)
	fmt.Fprintf(builder, "# TYPE %s counter\n", name)
	fmt.Fprintf(builder, "%s %d\n", name, value)
}

func writePromGauge(builder *strings.Builder, name, help string, value float64) {
	fmt.Fprintf(builder, "# HELP %s %s\n", name, help)
	fmt.Fprintf(builder, "# TYPE %s gauge\n", name)
	fmt.Fprintf(builder, "%s %.6f\n", name, value)
}

func (s *Server) metricsHandler(c *gin.Context) {
	now := time.Now()
	goroutines, openFDs := s.snapshotProcessMetrics(now)
	autoProfileEnabled := float64(0)
	if s != nil && s.cfg != nil && s.cfg.AutoProfile.Enable {
		autoProfileEnabled = 1
	}
	baseCounters := map[string]uint64{
		"cliproxy_models_cache_hit_total":          s.modelsCacheHit.Load(),
		"cliproxy_models_cache_miss_total":         s.modelsCacheMiss.Load(),
		"cliproxy_models_cache_build_total":        s.modelsCacheBuildCount.Load(),
		"cliproxy_models_cache_invalidate_total":   s.modelsCacheInvalidated.Load(),
		"cliproxy_server_full_update_total":        s.fullUpdateCount.Load(),
		"cliproxy_server_incremental_update_total": s.incrementalUpdateCount.Load(),
		"cliproxy_autoprofile_capture_total":       s.autoProfileCaptured.Load(),
		"cliproxy_autoprofile_cooldown_skip_total": s.autoProfileSkipped.Load(),
		"cliproxy_autoprofile_error_total":         s.autoProfileErrors.Load(),
	}
	baseGauges := map[string]float64{
		"cliproxy_models_cache_version_current":     float64(s.modelsCacheVersion.Load()),
		"cliproxy_models_cache_entries_current":     float64(s.modelsCacheEntries()),
		"cliproxy_process_goroutines_current":       float64(goroutines),
		"cliproxy_process_open_fds_current":         float64(openFDs),
		"cliproxy_models_cache_strategy_ttl_legacy": 0,
		"cliproxy_autoprofile_enabled":              autoProfileEnabled,
	}
	if s.modelsCacheStrategy() == "ttl_legacy" {
		baseGauges["cliproxy_models_cache_strategy_ttl_legacy"] = 1
	}
	if runtime := s.snapshotRuntimeMetrics(); len(runtime) > 0 {
		for key, value := range runtime {
			if key == "" {
				continue
			}
			baseCounters[key] = value
		}
	}

	help := map[string]string{
		"cliproxy_models_cache_hit_total":                       "Total number of /v1/models cache hits.",
		"cliproxy_models_cache_miss_total":                      "Total number of /v1/models cache misses.",
		"cliproxy_models_cache_build_total":                     "Total number of /v1/models payload builds.",
		"cliproxy_models_cache_invalidate_total":                "Total number of /v1/models cache invalidations.",
		"cliproxy_server_full_update_total":                     "Total number of full server config updates.",
		"cliproxy_server_incremental_update_total":              "Total number of incremental auth snapshot updates.",
		"cliproxy_models_cache_version_current":                 "Current cache version for /v1/models.",
		"cliproxy_models_cache_entries_current":                 "Current /v1/models cache entries in memory.",
		"cliproxy_process_goroutines_current":                   "Current number of goroutines in process.",
		"cliproxy_process_open_fds_current":                     "Current open file descriptors (best-effort).",
		"cliproxy_models_cache_strategy_ttl_legacy":             "Current /v1/models cache strategy flag (1=ttl_legacy, 0=versioned).",
		"cliproxy_models_cache_requests_total":                  "Total number of /v1/models cache lookup requests.",
		"cliproxy_models_cache_hit_ratio":                       "Current /v1/models cache hit ratio.",
		"cliproxy_watcher_config_reload_requested_total":        "Total number of watcher config reload requests.",
		"cliproxy_watcher_config_reload_executed_total":         "Total number of watcher config reload executions.",
		"cliproxy_watcher_config_reload_coalesced_total":        "Total number of watcher config reload coalesced events.",
		"cliproxy_watcher_auth_reload_requested_total":          "Total number of watcher auth reload requests.",
		"cliproxy_watcher_auth_reload_executed_total":           "Total number of watcher auth reload executions.",
		"cliproxy_watcher_auth_reload_coalesced_total":          "Total number of watcher auth reload coalesced events.",
		"cliproxy_reload_requested_total":                       "Total number of service reload callback requests.",
		"cliproxy_reload_executed_total":                        "Total number of service reload callback executions.",
		"cliproxy_reload_dropped_stale_total":                   "Total number of stale service reload callbacks dropped.",
		"cliproxy_provider_backpressure_reject_total":           "Total number of provider requests rejected due to inflight backpressure.",
		"cliproxy_provider_circuit_open_total":                  "Total number of provider requests rejected while circuit breaker was open.",
		"cliproxy_provider_adaptive_increase_total":             "Total number of adaptive provider inflight limit increases.",
		"cliproxy_provider_adaptive_decrease_total":             "Total number of adaptive provider inflight limit decreases.",
		"cliproxy_provider_adaptive_limit_sum":                  "Current sum of adaptive inflight limits across providers.",
		"cliproxy_provider_adaptive_provider_count":             "Current number of providers tracked by adaptive inflight limiter.",
		"cliproxy_provider_adaptive_enabled":                    "Adaptive provider inflight limiter feature flag (1=enabled, 0=disabled).",
		"cliproxy_provider_adaptive_rollback_total":             "Total number of adaptive limiter rollbacks triggered by SLO guardrails.",
		"cliproxy_provider_adaptive_rollback_active":            "Current number of providers under adaptive rollback hold window.",
		"cliproxy_conductor_select_total":                       "Total number of conductor auth selection attempts.",
		"cliproxy_conductor_select_ns_sum":                      "Total nanoseconds spent selecting auth candidates in conductor.",
		"cliproxy_conductor_permit_total":                       "Total number of conductor provider permit acquisition attempts.",
		"cliproxy_conductor_permit_ns_sum":                      "Total nanoseconds spent acquiring provider permits in conductor.",
		"cliproxy_conductor_retry_total":                        "Total number of conductor retry loops triggered after execution failures.",
		"cliproxy_conductor_retry_wait_ns_sum":                  "Total nanoseconds of retry cooldown waits scheduled by conductor.",
		"cliproxy_codex_strategy_legacy_stream_total":           "Total Codex non-stream requests executed via legacy SSE-over-HTTP path.",
		"cliproxy_codex_strategy_compact_auto_total":            "Total Codex non-stream requests attempted via compact_auto path.",
		"cliproxy_codex_strategy_compact_force_total":           "Total Codex requests forced through /responses/compact.",
		"cliproxy_codex_strategy_websocket_total":               "Total Codex requests executed via websocket transport.",
		"cliproxy_codex_strategy_compact_auto_fallback_total":   "Total Codex compact_auto fallbacks back to legacy path.",
		"cliproxy_codex_strategy_websocket_fallback_total":      "Total Codex websocket fallbacks back to HTTP path.",
		"cliproxy_codex_http_nonstream_completed_total":         "Total Codex HTTP non-stream requests completed after first response.completed event.",
		"cliproxy_codex_http_nonstream_missing_completed_total": "Total Codex HTTP non-stream requests that ended before response.completed.",
		"cliproxy_codex_http_nonstream_event_error_total":       "Total Codex HTTP non-stream event-level error payloads observed before completion.",
		"cliproxy_codex_http_nonstream_bytes_total":             "Total bytes processed by the incremental Codex HTTP non-stream parser.",
		"cliproxy_autoprofile_capture_total":                    "Total number of runtime auto-profile captures.",
		"cliproxy_autoprofile_cooldown_skip_total":              "Total number of auto-profile triggers skipped due to cooldown.",
		"cliproxy_autoprofile_error_total":                      "Total number of auto-profile capture errors.",
		"cliproxy_autoprofile_enabled":                          "Auto-profile feature flag (1=enabled, 0=disabled).",
	}

	hit := baseCounters["cliproxy_models_cache_hit_total"]
	miss := baseCounters["cliproxy_models_cache_miss_total"]
	requests := hit + miss
	hitRatio := 0.0
	if requests > 0 {
		hitRatio = float64(hit) / float64(requests)
	}

	builder := &strings.Builder{}
	counterKeys := make([]string, 0, len(baseCounters))
	for key := range baseCounters {
		counterKeys = append(counterKeys, key)
	}
	sort.Strings(counterKeys)
	for _, key := range counterKeys {
		desc := help[key]
		if desc == "" {
			desc = "CLIProxy runtime metric."
		}
		writePromCounter(builder, key, desc, baseCounters[key])
	}
	gaugeKeys := make([]string, 0, len(baseGauges))
	for key := range baseGauges {
		gaugeKeys = append(gaugeKeys, key)
	}
	sort.Strings(gaugeKeys)
	for _, key := range gaugeKeys {
		desc := help[key]
		if desc == "" {
			desc = "CLIProxy runtime metric."
		}
		writePromGauge(builder, key, desc, baseGauges[key])
	}
	writePromCounter(builder, "cliproxy_models_cache_requests_total", help["cliproxy_models_cache_requests_total"], requests)
	writePromGauge(builder, "cliproxy_models_cache_hit_ratio", help["cliproxy_models_cache_hit_ratio"], hitRatio)

	c.Data(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", []byte(builder.String()))
}

func (s *Server) autoProfileMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || s.cfg == nil || !s.cfg.AutoProfile.Enable {
			c.Next()
			return
		}
		cfg := s.cfg.AutoProfile
		startedAt := time.Now()
		c.Next()

		latency := time.Since(startedAt)
		status := c.Writer.Status()
		if !shouldAutoProfileCapture(cfg, status, latency) {
			return
		}

		now := time.Now()
		cooldown := time.Duration(cfg.CooldownMS) * time.Millisecond
		if !s.markAutoProfileCapture(now, cooldown) {
			return
		}

		go s.captureAutoProfile(now, cfg.OutputDir, cfg.MaxFiles, status, latency)
	}
}

func shouldAutoProfileCapture(cfg config.AutoProfileConfig, status int, latency time.Duration) bool {
	threshold := time.Duration(cfg.LatencyThresholdMS) * time.Millisecond
	if threshold > 0 && latency >= threshold {
		return true
	}
	for _, trigger := range cfg.TriggerStatuses {
		if trigger == status {
			return true
		}
	}
	return false
}

func (s *Server) markAutoProfileCapture(now time.Time, cooldown time.Duration) bool {
	if s == nil {
		return false
	}
	s.autoProfileMu.Lock()
	defer s.autoProfileMu.Unlock()
	if cooldown > 0 && !s.autoProfileLastCapture.IsZero() && now.Sub(s.autoProfileLastCapture) < cooldown {
		s.autoProfileSkipped.Add(1)
		return false
	}
	s.autoProfileLastCapture = now
	return true
}

func (s *Server) captureAutoProfile(now time.Time, outputDir string, maxFiles int, status int, latency time.Duration) {
	if s == nil {
		return
	}
	dir := s.resolveAutoProfileOutputDir(outputDir)
	if errMkdir := os.MkdirAll(dir, 0o755); errMkdir != nil {
		s.autoProfileErrors.Add(1)
		log.Warnf("auto-profile: failed to create output dir %s: %v", dir, errMkdir)
		return
	}

	latencyMS := latency.Milliseconds()
	if latencyMS < 0 {
		latencyMS = 0
	}
	baseName := fmt.Sprintf(
		"%s%s-status%d-latency%dms",
		autoProfileFilePrefix,
		now.UTC().Format("20060102-150405.000"),
		status,
		latencyMS,
	)
	heapPath := filepath.Join(dir, baseName+"-heap.pprof")
	goroutinePath := filepath.Join(dir, baseName+"-goroutine.pprof")

	runtime.GC()
	errHeap := writeRuntimeProfile("heap", heapPath, 0)
	errGoroutine := writeRuntimeProfile("goroutine", goroutinePath, 2)

	if errHeap != nil || errGoroutine != nil {
		s.autoProfileErrors.Add(1)
		log.Warnf(
			"auto-profile: capture completed with errors (heap=%v goroutine=%v, status=%d latency_ms=%d)",
			errHeap,
			errGoroutine,
			status,
			latencyMS,
		)
	} else {
		s.autoProfileCaptured.Add(1)
		log.Warnf(
			"auto-profile: captured profiles (status=%d latency_ms=%d heap=%s goroutine=%s)",
			status,
			latencyMS,
			heapPath,
			goroutinePath,
		)
	}
	s.trimAutoProfileFiles(dir, maxFiles)
}

func writeRuntimeProfile(profileName, filePath string, debug int) error {
	profile := pprof.Lookup(profileName)
	if profile == nil {
		return fmt.Errorf("runtime profile %s is unavailable", profileName)
	}
	file, errCreate := os.Create(filePath)
	if errCreate != nil {
		return errCreate
	}
	defer file.Close()
	return profile.WriteTo(file, debug)
}

func (s *Server) resolveAutoProfileOutputDir(configuredDir string) string {
	dir := strings.TrimSpace(configuredDir)
	if dir != "" {
		return dir
	}
	if base := util.WritablePath(); strings.TrimSpace(base) != "" {
		return filepath.Join(base, "profiles", "auto")
	}
	if s != nil && strings.TrimSpace(s.currentPath) != "" {
		return filepath.Join(s.currentPath, "profiles", "auto")
	}
	return filepath.Join(".", "profiles", "auto")
}

func (s *Server) trimAutoProfileFiles(dir string, maxFiles int) {
	if s == nil || maxFiles <= 0 {
		return
	}
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		s.autoProfileErrors.Add(1)
		log.Warnf("auto-profile: failed to list output dir %s: %v", dir, errRead)
		return
	}
	type profFile struct {
		path    string
		modTime time.Time
	}
	files := make([]profFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(entry.Name()))
		if !strings.HasPrefix(name, autoProfileFilePrefix) || !strings.HasSuffix(name, ".pprof") {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			continue
		}
		files = append(files, profFile{
			path:    filepath.Join(dir, entry.Name()),
			modTime: info.ModTime(),
		})
	}
	if len(files) <= maxFiles {
		return
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.After(files[j].modTime)
	})
	for _, oldFile := range files[maxFiles:] {
		if errRemove := os.Remove(oldFile.path); errRemove != nil && !os.IsNotExist(errRemove) {
			s.autoProfileErrors.Add(1)
			log.Warnf("auto-profile: failed to prune old profile %s: %v", oldFile.path, errRemove)
		}
	}
}

func (s *Server) snapshotProcessMetrics(now time.Time) (int, int) {
	if s == nil {
		return 0, 0
	}
	s.processMetricsMu.Lock()
	defer s.processMetricsMu.Unlock()
	if !s.processMetricsCachedAt.IsZero() && now.Sub(s.processMetricsCachedAt) < processMetricsCacheTTL {
		return s.processGoroutines, s.processOpenFDs
	}
	s.processGoroutines = runtime.NumGoroutine()
	s.processOpenFDs = countOpenFDs()
	s.processMetricsCachedAt = now
	return s.processGoroutines, s.processOpenFDs
}

func countOpenFDs() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	return len(entries)
}

func (s *Server) watchKeepAlive() {
	if !s.keepAliveEnabled {
		return
	}

	timer := time.NewTimer(s.keepAliveTimeout)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			log.Warnf("keep-alive endpoint idle for %s, shutting down", s.keepAliveTimeout)
			if s.keepAliveOnTimeout != nil {
				s.keepAliveOnTimeout()
			}
			return
		case <-s.keepAliveHeartbeat:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(s.keepAliveTimeout)
		case <-s.keepAliveStop:
			return
		}
	}
}

// isAnthropicModelsRequest reports whether a /v1/models request should be served in
// Anthropic format. Anthropic API clients send the Anthropic-Version header; Claude
// Code additionally uses a claude-cli User-Agent.
func isAnthropicModelsRequest(c *gin.Context) bool {
	if c.GetHeader("Anthropic-Version") != "" {
		return true
	}
	return strings.HasPrefix(c.GetHeader("User-Agent"), "claude-cli")
}

// unifiedModelsHandler creates a unified handler for the /v1/models endpoint
// that routes to different handlers based on the request.
// Anthropic API requests (Anthropic-Version header, or a claude-cli User-Agent)
// route to the Claude handler, otherwise they route to the OpenAI handler.
func (s *Server) unifiedModelsHandler(openaiHandler *openai.OpenAIAPIHandler, claudeHandler *claude.ClaudeCodeAPIHandler) gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, ok := c.Request.URL.Query()["client_version"]; ok {
			if s != nil && s.cfg != nil && s.cfg.Home.Enabled {
				s.handleHomeCodexClientModels(c)
				return
			}
			openaiHandler.OpenAIModels(c)
			return
		}

		useClaude := isAnthropicModelsRequest(c)
		now := time.Now()
		cacheVersion := s.modelsCacheVersion.Load()

		if payload, ok := s.readModelsCache(useClaude, cacheVersion, now); ok {
			s.modelsCacheHit.Add(1)
			c.Data(http.StatusOK, "application/json; charset=utf-8", payload)
			return
		}

		if s != nil && s.cfg != nil && s.cfg.Home.Enabled {
			s.handleHomeModels(c)
			return
		}

		s.modelsCacheMiss.Add(1)

		payload, errBuild := s.getOrBuildModelsPayload(useClaude, cacheVersion, openaiHandler, claudeHandler)
		if errBuild != nil {
			log.Errorf("failed to build /v1/models response payload: %v", errBuild)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build models response"})
			return
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", payload)
	}
}

func (s *Server) getOrBuildModelsPayload(useClaude bool, cacheVersion uint64, openaiHandler *openai.OpenAIAPIHandler, claudeHandler *claude.ClaudeCodeAPIHandler) ([]byte, error) {
	if s == nil {
		return nil, errors.New("server is nil")
	}
	bucket := "openai"
	if useClaude {
		bucket = "claude"
	}
	key := fmt.Sprintf("%s:%d", bucket, cacheVersion)
	built, errBuild, _ := s.modelsBuildGroup.Do(key, func() (any, error) {
		now := time.Now()
		if payload, ok := s.readModelsCache(useClaude, cacheVersion, now); ok {
			return payload, nil
		}
		payload, err := s.buildModelsPayload(useClaude, openaiHandler, claudeHandler)
		if err != nil {
			return nil, err
		}
		s.modelsCacheBuildCount.Add(1)
		if s.modelsCacheVersion.Load() == cacheVersion {
			s.writeModelsCache(useClaude, payload, cacheVersion, now)
		}
		return payload, nil
	})
	if errBuild != nil {
		return nil, errBuild
	}
	payload, ok := built.([]byte)
	if !ok {
		return nil, errors.New("invalid models payload type")
	}
	return payload, nil
}

func (s *Server) buildModelsPayload(useClaude bool, openaiHandler *openai.OpenAIAPIHandler, claudeHandler *claude.ClaudeCodeAPIHandler) ([]byte, error) {
	if s != nil && s.modelsBuildHook != nil {
		s.modelsBuildHook()
	}
	if useClaude {
		models := claudeHandler.Models()
		firstID := ""
		lastID := ""
		if len(models) > 0 {
			if id, ok := models[0]["id"].(string); ok {
				firstID = id
			}
			if id, ok := models[len(models)-1]["id"].(string); ok {
				lastID = id
			}
		}
		return json.Marshal(gin.H{
			"data":     models,
			"has_more": false,
			"first_id": firstID,
			"last_id":  lastID,
		})
	}

	allModels := openaiHandler.Models()
	filteredModels := make([]map[string]any, len(allModels))
	for i, model := range allModels {
		filteredModel := map[string]any{
			"id":     model["id"],
			"object": model["object"],
		}
		if created, exists := model["created"]; exists {
			filteredModel["created"] = created
		}
		if ownedBy, exists := model["owned_by"]; exists {
			filteredModel["owned_by"] = ownedBy
		}
		filteredModels[i] = filteredModel
	}
	return json.Marshal(gin.H{
		"object": "list",
		"data":   filteredModels,
	})
}

func (s *Server) readModelsCache(useClaude bool, version uint64, now time.Time) ([]byte, bool) {
	if s == nil {
		return nil, false
	}
	s.modelsCacheMu.RLock()
	var entry modelsCacheEntry
	if useClaude {
		entry = s.claudeModelsCache
	} else {
		entry = s.openAIModelsCache
	}
	s.modelsCacheMu.RUnlock()
	if len(entry.payload) == 0 {
		return nil, false
	}
	if s.modelsCacheStrategy() == "ttl_legacy" {
		if now.After(entry.expiresAt) {
			return nil, false
		}
		return entry.payload, true
	}
	if entry.version != version {
		return nil, false
	}
	return entry.payload, true
}

func (s *Server) writeModelsCache(useClaude bool, payload []byte, version uint64, now time.Time) {
	if s == nil || len(payload) == 0 {
		return
	}
	entry := modelsCacheEntry{
		payload:   append([]byte(nil), payload...),
		version:   version,
		expiresAt: now.Add(s.modelsCacheTTL()),
	}
	s.modelsCacheMu.Lock()
	if useClaude {
		s.claudeModelsCache = entry
	} else {
		s.openAIModelsCache = entry
	}
	s.modelsCacheMu.Unlock()
}

func (s *Server) invalidateModelsCache() {
	if s == nil {
		return
	}
	s.modelsCacheVersion.Add(1)
	s.modelsCacheInvalidated.Add(1)
	s.modelsCacheMu.Lock()
	s.openAIModelsCache = modelsCacheEntry{}
	s.claudeModelsCache = modelsCacheEntry{}
	s.modelsCacheMu.Unlock()
}

func (s *Server) modelsCacheStrategy() string {
	if s == nil || s.cfg == nil {
		return config.DefaultModelsCacheStrategy
	}
	strategy := strings.ToLower(strings.TrimSpace(s.cfg.ModelsCache.Strategy))
	if strategy == "ttl_legacy" {
		return strategy
	}
	return config.DefaultModelsCacheStrategy
}

func (s *Server) modelsCacheTTL() time.Duration {
	if s == nil || s.cfg == nil || s.cfg.ModelsCache.TTLMs <= 0 {
		return time.Duration(config.DefaultModelsCacheTTLMs) * time.Millisecond
	}
	return time.Duration(s.cfg.ModelsCache.TTLMs) * time.Millisecond
}

func (s *Server) modelsCacheEntries() uint64 {
	if s == nil {
		return 0
	}
	s.modelsCacheMu.RLock()
	defer s.modelsCacheMu.RUnlock()
	var count uint64
	if len(s.openAIModelsCache.payload) > 0 {
		count++
	}
	if len(s.claudeModelsCache.payload) > 0 {
		count++
	}
	return count
}


func (s *Server) handleHomeCodexClientModels(c *gin.Context) {
	entries, ok := s.loadHomeModelEntries(c)
	if !ok {
		return
	}

	models := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		model := map[string]any{
			"id":     entry.id,
			"object": "model",
		}
		if entry.created > 0 {
			model["created"] = entry.created
		}
		if entry.ownedBy != "" {
			model["owned_by"] = entry.ownedBy
		}
		if entry.displayName != "" {
			model["display_name"] = entry.displayName
			model["description"] = entry.displayName
		}
		models = append(models, model)
	}

	c.JSON(http.StatusOK, openai.CodexClientModelsResponse(models))
}

func (s *Server) geminiModelsHandler(geminiHandler *gemini.GeminiAPIHandler) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s != nil && s.cfg != nil && s.cfg.Home.Enabled {
			s.handleHomeGeminiModels(c)
			return
		}

		geminiHandler.GeminiModels(c)
	}
}

func (s *Server) geminiGetHandler(geminiHandler *gemini.GeminiAPIHandler) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s != nil && s.cfg != nil && s.cfg.Home.Enabled {
			s.handleHomeGeminiModel(c)
			return
		}

		geminiHandler.GeminiGetHandler(c)
	}
}

type homeModelEntry struct {
	id                  string
	created             int64
	ownedBy             string
	displayName         string
	contextLength       int
	maxCompletionTokens int
}

func (s *Server) handleHomeModels(c *gin.Context) {
	entries, ok := s.loadHomeModelEntries(c)
	if !ok {
		return
	}

	isClaude := isAnthropicModelsRequest(c)

	if isClaude {
		out := formatHomeClaudeModels(entries)
		firstID := ""
		lastID := ""
		if len(out) > 0 {
			if id, okID := out[0]["id"].(string); okID {
				firstID = id
			}
			if id, okID := out[len(out)-1]["id"].(string); okID {
				lastID = id
			}
		}
		c.JSON(http.StatusOK, gin.H{
			"data":     out,
			"has_more": false,
			"first_id": firstID,
			"last_id":  lastID,
		})
		return
	}

	filtered := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		model := map[string]any{
			"id":     entry.id,
			"object": "model",
		}
		if entry.created > 0 {
			model["created"] = entry.created
		}
		if entry.ownedBy != "" {
			model["owned_by"] = entry.ownedBy
		}
		filtered = append(filtered, model)
	}
	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   filtered,
	})
}

func formatHomeClaudeModels(entries []homeModelEntry) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		out = append(out, formatHomeClaudeModel(entry))
	}
	sort.SliceStable(out, func(i, j int) bool {
		di, _ := out[i]["display_name"].(string)
		dj, _ := out[j]["display_name"].(string)
		if di != dj {
			return di < dj
		}
		idi, _ := out[i]["id"].(string)
		idj, _ := out[j]["id"].(string)
		return idi < idj
	})
	return out
}

func formatHomeClaudeModel(entry homeModelEntry) map[string]any {
	displayName := entry.displayName
	if displayName == "" {
		displayName = entry.id
	}
	maxInput := entry.contextLength
	if maxInput <= 0 {
		maxInput = registry.DefaultClaudeMaxInputTokens
	}
	maxOutput := entry.maxCompletionTokens
	if maxOutput <= 0 {
		maxOutput = registry.DefaultClaudeMaxOutputTokens
	}
	model := map[string]any{
		"id":               util.EnsureClaudeModelIDPrefix(entry.id),
		"object":           "model",
		"owned_by":         entry.ownedBy,
		"type":             "model",
		"display_name":     displayName,
		"max_input_tokens": maxInput,
		"max_tokens":       maxOutput,
	}
	if entry.created > 0 {
		model["created_at"] = time.Unix(entry.created, 0).UTC().Format(time.RFC3339)
	}
	return model
}

func (s *Server) handleHomeGeminiModels(c *gin.Context) {
	entries, ok := s.loadHomeModelEntries(c)
	if !ok {
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"models": formatHomeGeminiModels(entries),
	})
}

func (s *Server) handleHomeGeminiModel(c *gin.Context) {
	entries, ok := s.loadHomeModelEntries(c)
	if !ok {
		return
	}

	action := strings.TrimPrefix(c.Param("action"), "/")
	action = strings.TrimSpace(action)
	for _, entry := range entries {
		if homeGeminiModelMatches(entry, action) {
			c.JSON(http.StatusOK, formatHomeGeminiModel(entry))
			return
		}
	}

	c.JSON(http.StatusNotFound, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: "Not Found",
			Type:    "not_found",
		},
	})
}

func (s *Server) loadHomeModelEntries(c *gin.Context) ([]homeModelEntry, bool) {
	if s == nil || c == nil || c.Request == nil {
		return nil, false
	}
	client := home.Current()
	if client == nil {
		c.JSON(http.StatusServiceUnavailable, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "home control center unavailable",
				Type:    "server_error",
			},
		})
		return nil, false
	}

	raw, errGet := client.GetModels(c.Request.Context(), c.Request.Header, c.Request.URL.Query())
	if errGet != nil {
		c.JSON(http.StatusBadGateway, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: errGet.Error(),
				Type:    "server_error",
			},
		})
		return nil, false
	}

	if statusCode, ok := homeModelsAuthStatus(raw); ok {
		c.JSON(statusCode, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: homeModelsErrorMessage(raw),
				Type:    "authentication_error",
			},
		})
		return nil, false
	}

	entries, errDecode := decodeHomeModels(raw)
	if errDecode != nil {
		c.JSON(http.StatusBadGateway, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: errDecode.Error(),
				Type:    "server_error",
			},
		})
		return nil, false
	}

	return entries, true
}

func formatHomeGeminiModels(entries []homeModelEntry) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		out = append(out, formatHomeGeminiModel(entry))
	}
	return out
}

func formatHomeGeminiModel(entry homeModelEntry) map[string]any {
	name := entry.id
	if !strings.HasPrefix(name, "models/") {
		name = "models/" + name
	}
	displayName := entry.displayName
	if displayName == "" {
		displayName = entry.id
	}
	return map[string]any{
		"name":                       name,
		"displayName":                displayName,
		"description":                displayName,
		"supportedGenerationMethods": []string{"generateContent"},
	}
}

func homeGeminiModelMatches(entry homeModelEntry, action string) bool {
	id := strings.TrimSpace(entry.id)
	if id == "" || action == "" {
		return false
	}
	normalizedAction := strings.TrimPrefix(action, "models/")
	normalizedID := strings.TrimPrefix(id, "models/")
	return action == id || action == "models/"+id || normalizedAction == normalizedID
}

// homeModelsAuthStatus inspects a home models response for an authentication/error envelope.
// It returns the HTTP status code to surface (401 for credential issues, 502 otherwise)
// and true when the payload is an error response rather than model data.
func homeModelsAuthStatus(raw []byte) (int, bool) {
	errType := homeModelsErrorType(raw)
	if errType == "" {
		return 0, false
	}
	if errType == "no_credentials" || errType == "invalid_credential" {
		return http.StatusUnauthorized, true
	}
	return http.StatusBadGateway, true
}

func homeModelsErrorType(raw []byte) string {
	top, ok := unmarshalHomeModelsTopLevel(raw)
	if !ok {
		return ""
	}
	rawErr, exists := top["error"]
	if !exists {
		return ""
	}
	var errObj struct {
		Type string `json:"type"`
	}
	if errUnmarshal := json.Unmarshal(rawErr, &errObj); errUnmarshal != nil {
		return ""
	}
	return strings.TrimSpace(errObj.Type)
}

func homeModelsErrorMessage(raw []byte) string {
	top, ok := unmarshalHomeModelsTopLevel(raw)
	if !ok {
		return "home models request failed"
	}
	rawErr, exists := top["error"]
	if !exists {
		return "home models request failed"
	}
	var errObj struct {
		Message string `json:"message"`
	}
	if errUnmarshal := json.Unmarshal(rawErr, &errObj); errUnmarshal != nil {
		return "home models request failed"
	}
	if msg := strings.TrimSpace(errObj.Message); msg != "" {
		return msg
	}
	return "home models request failed"
}

func unmarshalHomeModelsTopLevel(raw []byte) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var top map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &top); errUnmarshal != nil {
		return nil, false
	}
	return top, true
}

func decodeHomeModels(raw []byte) ([]homeModelEntry, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("home models payload is empty")
	}

	var bySection map[string][]map[string]any
	if err := json.Unmarshal(raw, &bySection); err != nil {
		return nil, fmt.Errorf("parse home models payload: %w", err)
	}
	if len(bySection) == 0 {
		return nil, fmt.Errorf("home models payload has no sections")
	}

	seen := make(map[string]struct{})
	out := make([]homeModelEntry, 0, 256)
	for _, models := range bySection {
		for _, model := range models {
			id, _ := model["id"].(string)
			id = strings.TrimSpace(id)
			if id == "" {
				name, _ := model["name"].(string)
				name = strings.TrimSpace(name)
				id = strings.TrimPrefix(name, "models/")
			}
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}

			ownedBy, _ := model["owned_by"].(string)
			ownedBy = strings.TrimSpace(ownedBy)
			displayName, _ := model["display_name"].(string)
			displayName = strings.TrimSpace(displayName)
			if displayName == "" {
				displayName, _ = model["displayName"].(string)
				displayName = strings.TrimSpace(displayName)
			}

			out = append(out, homeModelEntry{
				id:                  id,
				created:             homeModelInt64Value(model, "created"),
				ownedBy:             ownedBy,
				displayName:         displayName,
				contextLength:       int(homeModelInt64Value(model, "context_length", "contextLength", "inputTokenLimit", "max_input_tokens")),
				maxCompletionTokens: int(homeModelInt64Value(model, "max_completion_tokens", "maxCompletionTokens", "outputTokenLimit", "max_tokens")),
			})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	if len(out) == 0 {
		return nil, fmt.Errorf("home models payload contains no models")
	}
	return out, nil
}

func homeModelInt64Value(model map[string]any, keys ...string) int64 {
	for _, key := range keys {
		switch value := model[key].(type) {
		case float64:
			return int64(value)
		case int64:
			return value
		case int:
			return int64(value)
		case json.Number:
			if n, errInt := value.Int64(); errInt == nil {
				return n
			}
		case string:
			if n, errParse := strconv.ParseInt(strings.TrimSpace(value), 10, 64); errParse == nil {
				return n
			}
		}
	}
	return 0
}

// Start begins listening for and serving HTTP or HTTPS requests.
// It's a blocking call and will only return on an unrecoverable error.
//
// Returns:
//   - error: An error if the server fails to start
func (s *Server) Start() error {
	if s == nil || s.server == nil {
		return fmt.Errorf("failed to start HTTP server: server not initialized")
	}

	addr := s.server.Addr
	listener, errListen := net.Listen("tcp", addr)
	if errListen != nil {
		return fmt.Errorf("failed to start HTTP server: %v", errListen)
	}

	useTLS := s.cfg != nil && s.cfg.TLS.Enable
	if useTLS {
		certPath := strings.TrimSpace(s.cfg.TLS.Cert)
		keyPath := strings.TrimSpace(s.cfg.TLS.Key)
		if certPath == "" || keyPath == "" {
			if errClose := listener.Close(); errClose != nil {
				log.Errorf("failed to close listener after TLS validation failure: %v", errClose)
			}
			return fmt.Errorf("failed to start HTTPS server: tls.cert or tls.key is empty")
		}
		certPair, errLoad := tls.LoadX509KeyPair(certPath, keyPath)
		if errLoad != nil {
			if errClose := listener.Close(); errClose != nil {
				log.Errorf("failed to close listener after TLS key pair load failure: %v", errClose)
			}
			return fmt.Errorf("failed to start HTTPS server: %v", errLoad)
		}

		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{certPair},
			NextProtos:   []string{"h2", "http/1.1"},
		}
		s.server.TLSConfig = tlsConfig
		if errHTTP2 := http2.ConfigureServer(s.server, &http2.Server{}); errHTTP2 != nil {
			log.Warnf("failed to configure HTTP/2: %v", errHTTP2)
		}
		listener = tls.NewListener(listener, tlsConfig)
		log.Debugf("Starting API server on %s with TLS", addr)
	} else {
		log.Debugf("Starting API server on %s", addr)
	}

	httpListener := newMuxListener(listener.Addr(), 1024)
	s.muxBaseListener = listener
	s.muxHTTPListener = httpListener

	httpErrCh := make(chan error, 1)
	acceptErrCh := make(chan error, 1)

	go func() {
		httpErrCh <- s.server.Serve(httpListener)
	}()
	go func() {
		acceptErrCh <- s.acceptMuxConnections(listener, httpListener)
	}()

	select {
	case errServe := <-httpErrCh:
		if s.muxBaseListener != nil {
			if errClose := s.muxBaseListener.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
				log.Debugf("failed to close shared listener after HTTP serve exit: %v", errClose)
			}
		}
		if s.muxHTTPListener != nil {
			_ = s.muxHTTPListener.Close()
		}
		errAccept := <-acceptErrCh
		errServe = normalizeHTTPServeError(errServe)
		errAccept = normalizeListenerError(errAccept)
		if errServe != nil {
			return fmt.Errorf("failed to start HTTP server: %v", errServe)
		}
		if errAccept != nil {
			return fmt.Errorf("failed to start HTTP server: %v", errAccept)
		}
		return nil
	case errAccept := <-acceptErrCh:
		if s.muxHTTPListener != nil {
			_ = s.muxHTTPListener.Close()
		}
		if s.muxBaseListener != nil {
			if errClose := s.muxBaseListener.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
				log.Debugf("failed to close shared listener after accept loop exit: %v", errClose)
			}
		}
		errServe := <-httpErrCh
		errServe = normalizeHTTPServeError(errServe)
		errAccept = normalizeListenerError(errAccept)
		if errAccept != nil {
			return fmt.Errorf("failed to start HTTP server: %v", errAccept)
		}
		if errServe != nil {
			return fmt.Errorf("failed to start HTTP server: %v", errServe)
		}
		return nil
	}
}

// Stop gracefully shuts down the API server without interrupting any
// active connections.
//
// Parameters:
//   - ctx: The context for graceful shutdown
//
// Returns:
//   - error: An error if the server fails to stop
func (s *Server) Stop(ctx context.Context) error {
	log.Debug("Stopping API server...")

	if s.keepAliveEnabled {
		select {
		case s.keepAliveStop <- struct{}{}:
		default:
		}
	}

	if s.muxHTTPListener != nil {
		_ = s.muxHTTPListener.Close()
	}
	if s.muxBaseListener != nil {
		if errClose := s.muxBaseListener.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			log.Debugf("failed to close shared listener: %v", errClose)
		}
	}

	// Shutdown the HTTP server.
	if err := s.server.Shutdown(ctx); err != nil {
		return fmt.Errorf("failed to shutdown HTTP server: %v", err)
	}

	log.Debug("API server stopped")
	return nil
}

// corsMiddleware returns a Gin middleware handler that adds CORS headers
// to every response, allowing cross-origin requests.
//
// Returns:
//   - gin.HandlerFunc: The CORS middleware handler
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "*")
		c.Header("Access-Control-Expose-Headers", corsExposedResponseHeadersJoined)

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

func (s *Server) applyAccessConfig(oldCfg, newCfg *config.Config) bool {
	if s == nil || s.accessManager == nil || newCfg == nil {
		return false
	}
	if _, err := access.ApplyAccessProviders(s.accessManager, oldCfg, newCfg); err != nil {
		return false
	}
	return true
}

func (s *Server) countAuthEntriesFromStore(cfg *config.Config) int {
	if cfg == nil {
		return 0
	}
	tokenStore := sdkAuth.GetTokenStore()
	if dirSetter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok {
		dirSetter.SetBaseDir(cfg.AuthDir)
	}
	return util.CountAuthFiles(context.Background(), tokenStore)
}

func countProviderEntries(cfg *config.Config) (int, int, int, int, int) {
	if cfg == nil {
		return 0, 0, 0, 0, 0
	}
	geminiAPIKeyCount := len(cfg.GeminiKey)
	claudeAPIKeyCount := len(cfg.ClaudeKey)
	codexAPIKeyCount := len(cfg.CodexKey)
	vertexAICompatCount := len(cfg.VertexCompatAPIKey)
	openAICompatCount := 0
	for i := range cfg.OpenAICompatibility {
		entry := cfg.OpenAICompatibility[i]
		openAICompatCount += len(entry.APIKeyEntries)
	}
	return geminiAPIKeyCount, claudeAPIKeyCount, codexAPIKeyCount, vertexAICompatCount, openAICompatCount
}

func (s *Server) logClientSummary(cfg *config.Config, authEntries int, force bool) {
	if s == nil || cfg == nil {
		return
	}
	if authEntries < 0 {
		authEntries = s.countAuthEntriesFromStore(cfg)
	}

	geminiAPIKeyCount, claudeAPIKeyCount, codexAPIKeyCount, vertexAICompatCount, openAICompatCount := countProviderEntries(cfg)
	total := authEntries + geminiAPIKeyCount + claudeAPIKeyCount + codexAPIKeyCount + vertexAICompatCount + openAICompatCount
	summaryKey := fmt.Sprintf(
		"%d|%d|%d|%d|%d|%d",
		authEntries,
		geminiAPIKeyCount,
		claudeAPIKeyCount,
		codexAPIKeyCount,
		vertexAICompatCount,
		openAICompatCount,
	)

	now := time.Now()
	s.clientSummaryMu.Lock()
	if !force && summaryKey == s.lastClientSummaryKey && now.Sub(s.lastClientSummaryAt) < clientSummaryLogCooldown {
		s.clientSummaryMu.Unlock()
		return
	}
	s.lastClientSummaryKey = summaryKey
	s.lastClientSummaryAt = now
	s.clientSummaryMu.Unlock()

	log.Infof(
		"server clients and configuration updated: %d clients (%d auth entries + %d Gemini API keys + %d Claude API keys + %d Codex keys + %d Vertex-compat + %d OpenAI-compat)",
		total,
		authEntries,
		geminiAPIKeyCount,
		claudeAPIKeyCount,
		codexAPIKeyCount,
		vertexAICompatCount,
		openAICompatCount,
	)
}

// UpdateClients updates the server's client list and configuration.
// This method is called when the configuration or authentication tokens change.
//
// Parameters:
//   - clients: The new slice of AI service clients
//   - cfg: The new application configuration
func (s *Server) UpdateClients(cfg *config.Config) {
	start := time.Now()
	call := s.fullUpdateCount.Add(1)
	s.invalidateModelsCache()

	// Reconstruct old config from YAML snapshot to avoid reference sharing issues
	var oldCfg *config.Config
	if len(s.oldConfigYaml) > 0 {
		_ = yaml.Unmarshal(s.oldConfigYaml, &oldCfg)
	}

	// Update request logger enabled state if it has changed
	previousRequestLog := false
	if oldCfg != nil {
		previousRequestLog = oldCfg.RequestLog
	}
	if s.requestLogger != nil && (oldCfg == nil || previousRequestLog != cfg.RequestLog) {
		if s.loggerToggle != nil {
			s.loggerToggle(cfg.RequestLog)
		} else if toggler, ok := s.requestLogger.(interface{ SetEnabled(bool) }); ok {
			toggler.SetEnabled(cfg.RequestLog)
		}
	}

	if oldCfg == nil || oldCfg.Home.Enabled != cfg.Home.Enabled {
		if setter, ok := s.requestLogger.(interface{ SetHomeEnabled(bool) }); ok {
			setter.SetHomeEnabled(cfg.Home.Enabled)
		}
	}

	if oldCfg == nil || oldCfg.LoggingToFile != cfg.LoggingToFile || oldCfg.LogsMaxTotalSizeMB != cfg.LogsMaxTotalSizeMB {
		if err := logging.ConfigureLogOutput(cfg); err != nil {
			log.Errorf("failed to reconfigure log output: %v", err)
		}
	}

	if oldCfg == nil || oldCfg.UsageStatisticsEnabled != cfg.UsageStatisticsEnabled || oldCfg.UsageJournalRetentionDays != cfg.UsageJournalRetentionDays || oldCfg.UsageJournalReplayMaxDays != cfg.UsageJournalReplayMaxDays {
		usage.ConfigureUsageRuntime(cfg.UsageStatisticsEnabled, cfg.UsageJournalRetentionDays, cfg.UsageJournalReplayMaxDays)
	}

	if oldCfg == nil || oldCfg.UsageStatisticsEnabled != cfg.UsageStatisticsEnabled {
		redisqueue.SetUsageStatisticsEnabled(cfg.UsageStatisticsEnabled)
	}

	if oldCfg == nil || oldCfg.RedisUsageQueueRetentionSeconds != cfg.RedisUsageQueueRetentionSeconds {
		redisqueue.SetRetentionSeconds(cfg.RedisUsageQueueRetentionSeconds)
	}

	if s.requestLogger != nil && (oldCfg == nil || oldCfg.ErrorLogsMaxFiles != cfg.ErrorLogsMaxFiles) {
		if setter, ok := s.requestLogger.(interface{ SetErrorLogsMaxFiles(int) }); ok {
			setter.SetErrorLogsMaxFiles(cfg.ErrorLogsMaxFiles)
		}
	}

	if oldCfg == nil || oldCfg.DisableCooling != cfg.DisableCooling {
		auth.SetQuotaCooldownDisabled(cfg.DisableCooling)
	}
	if oldCfg == nil || oldCfg.TransientErrorCooldownSeconds != cfg.TransientErrorCooldownSeconds {
		auth.SetTransientErrorCooldownSeconds(cfg.TransientErrorCooldownSeconds)
	}

	if oldCfg != nil && oldCfg.DisableImageGeneration != cfg.DisableImageGeneration {
		log.Infof("disable-image-generation updated: %v -> %v", oldCfg.DisableImageGeneration, cfg.DisableImageGeneration)
	}

	applySignatureCacheConfig(oldCfg, cfg)

	if s.handlers != nil && s.handlers.AuthManager != nil {
		s.handlers.AuthManager.SetRetryConfig(cfg.RequestRetry, time.Duration(cfg.MaxRetryInterval)*time.Second, cfg.MaxRetryCredentials)
	}

	// Update log level dynamically when debug flag changes
	if oldCfg == nil || oldCfg.Debug != cfg.Debug {
		util.SetLogLevel(cfg)
	}

	prevSecretEmpty := true
	if oldCfg != nil {
		prevSecretEmpty = oldCfg.RemoteManagement.SecretKey == ""
	}
	newSecretEmpty := cfg.RemoteManagement.SecretKey == ""
	if s.envManagementSecret {
		s.registerManagementRoutes()
		if s.managementRoutesEnabled.CompareAndSwap(false, true) {
			log.Info("management routes enabled via MANAGEMENT_PASSWORD")
		} else {
			s.managementRoutesEnabled.Store(true)
		}
	} else {
		switch {
		case prevSecretEmpty && !newSecretEmpty:
			s.registerManagementRoutes()
			if s.managementRoutesEnabled.CompareAndSwap(false, true) {
				log.Info("management routes enabled after secret key update")
			} else {
				s.managementRoutesEnabled.Store(true)
			}
		case !prevSecretEmpty && newSecretEmpty:
			if s.managementRoutesEnabled.CompareAndSwap(true, false) {
				log.Info("management routes disabled after secret key removal")
			} else {
				s.managementRoutesEnabled.Store(false)
			}
		default:
			s.managementRoutesEnabled.Store(!newSecretEmpty)
		}
	}
	redisqueue.SetEnabled(s.managementRoutesEnabled.Load() || (cfg != nil && cfg.Home.Enabled))

	exampleAPIKeySafeModeRequired := s.exampleAPIKeySafeModeRequired(cfg)
	if exampleAPIKeySafeModeRequired {
		s.exampleAPIKeySafeModeActive.Store(true)
	}
	accessConfigApplied := s.applyAccessConfig(oldCfg, cfg)
	if accessConfigApplied || exampleAPIKeySafeModeRequired {
		s.exampleAPIKeySafeModeActive.Store(exampleAPIKeySafeModeRequired)
	}
	s.cfg = cfg
	s.wsAuthEnabled.Store(cfg.WebsocketAuth)
	if oldCfg != nil && s.wsAuthChanged != nil && oldCfg.WebsocketAuth != cfg.WebsocketAuth {
		s.wsAuthChanged(oldCfg.WebsocketAuth, cfg.WebsocketAuth)
	}
	managementasset.SetCurrentConfig(cfg)
	// Save YAML snapshot for next comparison
	s.oldConfigYaml, _ = yaml.Marshal(cfg)

	s.handlers.UpdateClients(effectiveSDKConfig(cfg))
	s.handlers.SetPluginHost(s.pluginHost)
	if s.pluginHost != nil {
		s.pluginHost.SetModelExecutor(s.handlers)
		s.pluginHost.SetAuthManager(s.handlers.AuthManager)
	}

	if s.mgmt != nil {
		s.mgmt.SetConfig(cfg)
		s.mgmt.SetAuthManager(s.handlers.AuthManager)
		s.mgmt.SetPluginHost(s.pluginHost)
	}
	s.refreshPluginManagementRoutes()

	authEntries := s.countAuthEntriesFromStore(cfg)
	s.logClientSummary(cfg, authEntries, true)
	log.Debugf("full UpdateClients completed in %dms (call=%d)", time.Since(start).Milliseconds(), call)
}

// UpdateAuthSnapshot applies auth-only refresh bookkeeping without re-running full config side effects.
func (s *Server) UpdateAuthSnapshot(cfg *config.Config, authEntries int) {
	if s == nil || cfg == nil {
		return
	}
	start := time.Now()
	call := s.incrementalUpdateCount.Add(1)
	s.invalidateModelsCache()

	s.cfg = cfg
	if s.handlers != nil {
		s.handlers.UpdateClients(&cfg.SDKConfig)
	}
	if s.mgmt != nil {
		s.mgmt.SetConfig(cfg)
	}
	s.logClientSummary(cfg, authEntries, false)
	log.Debugf("incremental UpdateAuthSnapshot completed in %dms (call=%d)", time.Since(start).Milliseconds(), call)
}

func (s *Server) SetWebsocketAuthChangeHandler(fn func(bool, bool)) {
	if s == nil {
		return
	}
	s.wsAuthChanged = fn
}

// (management handlers moved to internal/api/handlers/management)

// AuthMiddleware returns a Gin middleware handler that authenticates requests
// using the configured authentication providers. When no providers are available,
// it allows all requests (legacy behaviour).
func AuthMiddleware(manager *sdkaccess.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if manager == nil {
			c.Next()
			return
		}

		result, err := manager.Authenticate(c.Request.Context(), c.Request)
		if err == nil {
			if result != nil {
				c.Set("userApiKey", result.Principal)
				c.Set("accessProvider", result.Provider)
				if len(result.Metadata) > 0 {
					c.Set("accessMetadata", result.Metadata)
				}
			}
			c.Next()
			return
		}

		statusCode := err.HTTPStatusCode()
		if statusCode >= http.StatusInternalServerError {
			log.Errorf("authentication middleware error: %v", err)
		}
		c.AbortWithStatusJSON(statusCode, gin.H{"error": err.Message})
	}
}

func configuredSignatureCacheEnabled(cfg *config.Config) bool {
	if cfg != nil && cfg.AntigravitySignatureCacheEnabled != nil {
		return *cfg.AntigravitySignatureCacheEnabled
	}
	return true
}

func applySignatureCacheConfig(oldCfg, cfg *config.Config) {
	newVal := configuredSignatureCacheEnabled(cfg)
	newStrict := configuredSignatureBypassStrict(cfg)
	if oldCfg == nil {
		cache.SetSignatureCacheEnabled(newVal)
		cache.SetSignatureBypassStrictMode(newStrict)
		return
	}

	oldVal := configuredSignatureCacheEnabled(oldCfg)
	if oldVal != newVal {
		cache.SetSignatureCacheEnabled(newVal)
	}

	oldStrict := configuredSignatureBypassStrict(oldCfg)
	if oldStrict != newStrict {
		cache.SetSignatureBypassStrictMode(newStrict)
	}
}

func configuredSignatureBypassStrict(cfg *config.Config) bool {
	if cfg != nil && cfg.AntigravitySignatureBypassStrict != nil {
		return *cfg.AntigravitySignatureBypassStrict
	}
	return false
}
