package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	gin "github.com/gin-gonic/gin"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func newBenchmarkServer(b *testing.B) *Server {
	b.Helper()
	gin.SetMode(gin.TestMode)

	tmpDir := b.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		b.Fatalf("failed to create auth dir: %v", err)
	}

	cfg := &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{
			APIKeys: []string{"test-key"},
		},
		Port:                   0,
		AuthDir:                authDir,
		Debug:                  false,
		LoggingToFile:          false,
		UsageStatisticsEnabled: false,
	}

	authManager := auth.NewManager(nil, nil, nil)
	accessManager := sdkaccess.NewManager()
	configPath := filepath.Join(tmpDir, "config.yaml")
	return NewServer(cfg, authManager, accessManager, configPath, WithMiddleware(func(c *gin.Context) {
		logging.SkipGinRequestLogging(c)
		c.Next()
	}))
}

func benchmarkModelsCacheHit(b *testing.B, userAgent string) {
	server := newBenchmarkServer(b)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}

	warmup := httptest.NewRecorder()
	server.engine.ServeHTTP(warmup, req)
	if warmup.Code != http.StatusOK {
		b.Fatalf("warmup failed with status %d", warmup.Code)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			b.Fatalf("unexpected status code: %d", rr.Code)
		}
	}
}

func BenchmarkUnifiedModelsCacheHitOpenAI(b *testing.B) {
	benchmarkModelsCacheHit(b, "curl/8.0")
}

func BenchmarkUnifiedModelsCacheHitClaude(b *testing.B) {
	benchmarkModelsCacheHit(b, "claude-cli/1.2.3")
}

func BenchmarkMetricsHandler(b *testing.B) {
	server := newBenchmarkServer(b)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			b.Fatalf("unexpected status code: %d", rr.Code)
		}
	}
}
