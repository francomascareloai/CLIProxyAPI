package api

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	gin "github.com/gin-gonic/gin"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/managementasset"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

const validManagementHTMLFixture = `<!doctype html>
<html>
  <head>
    <meta charset="utf-8" />
    <title>management</title>
    <!-- compatibility: usage_aggregates_v2 rolling_windows_v1 usage_journal_v1 __periodFallback -->
  </head>
  <body>
    <div id="root"></div>
    <script type="module">
      const usage_aggregates_v2 = true;
      const rolling_windows_v1 = true;
      const usage_journal_v1 = true;
      const __periodFallback = "fallback";
      const routes = [{ path: "/settings", to: "/config" }];
      const restoreSession = () => true;
      const login = async () => true;
      const checkAuth = async () => true;
      const logout = () => true;
      globalThis.__contract = {
        usage_aggregates_v2,
        rolling_windows_v1,
        usage_journal_v1,
        __periodFallback,
        routes,
        restoreSession,
        login,
        checkAuth,
        logout,
      };
      const root = document.getElementById("root");
      if (!root) throw new Error("missing root");
      const createRoot = (node) => ({ render(value) { node.dataset.rendered = String(Boolean(value)); } });
      createRoot(root).render(globalThis.__contract);
    </script>
  </body>
</html>`

func newTestServer(t *testing.T) *Server {
	t.Helper()

	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("failed to create auth dir: %v", err)
	}

	cfg := &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{
			APIKeys: []string{"test-key"},
		},
		Port:                   0,
		AuthDir:                authDir,
		Debug:                  true,
		LoggingToFile:          false,
		UsageStatisticsEnabled: false,
	}

	authManager := auth.NewManager(nil, nil, nil)
	accessManager := sdkaccess.NewManager()

	configPath := filepath.Join(tmpDir, "config.yaml")
	return NewServer(cfg, authManager, accessManager, configPath)
}

func TestAmpProviderModelRoutes(t *testing.T) {
	testCases := []struct {
		name         string
		path         string
		wantStatus   int
		wantContains string
	}{
		{
			name:         "openai root models",
			path:         "/api/provider/openai/models",
			wantStatus:   http.StatusOK,
			wantContains: `"object":"list"`,
		},
		{
			name:         "groq root models",
			path:         "/api/provider/groq/models",
			wantStatus:   http.StatusOK,
			wantContains: `"object":"list"`,
		},
		{
			name:         "openai models",
			path:         "/api/provider/openai/v1/models",
			wantStatus:   http.StatusOK,
			wantContains: `"object":"list"`,
		},
		{
			name:         "anthropic models",
			path:         "/api/provider/anthropic/v1/models",
			wantStatus:   http.StatusOK,
			wantContains: `"data"`,
		},
		{
			name:         "google models v1",
			path:         "/api/provider/google/v1/models",
			wantStatus:   http.StatusOK,
			wantContains: `"models"`,
		},
		{
			name:         "google models v1beta",
			path:         "/api/provider/google/v1beta/models",
			wantStatus:   http.StatusOK,
			wantContains: `"models"`,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			server := newTestServer(t)

			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Authorization", "Bearer test-key")

			rr := httptest.NewRecorder()
			server.engine.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("unexpected status code for %s: got %d want %d; body=%s", tc.path, rr.Code, tc.wantStatus, rr.Body.String())
			}
			if body := rr.Body.String(); !strings.Contains(body, tc.wantContains) {
				t.Fatalf("response body for %s missing %q: %s", tc.path, tc.wantContains, body)
			}
		})
	}
}

func TestServeManagementControlPanel_RefreshesIncompatibleRuntimeAsset(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	restoreBundled := managementasset.SetBundledManagementHTMLForTests([]byte(validManagementHTMLFixture))
	defer restoreBundled()
	restoreFallbackURL := managementasset.SetFallbackManagementURLForTests("http://127.0.0.1:1/unreachable")
	defer restoreFallbackURL()

	originalWD, errGetwd := os.Getwd()
	if errGetwd != nil {
		t.Fatalf("failed to get current working directory: %v", errGetwd)
	}

	tmpDir := t.TempDir()
	if errChdir := os.Chdir(tmpDir); errChdir != nil {
		t.Fatalf("failed to switch working directory: %v", errChdir)
	}
	defer func() {
		if errChdirBack := os.Chdir(originalWD); errChdirBack != nil {
			t.Fatalf("failed to restore working directory: %v", errChdirBack)
		}
	}()

	managementasset.ResetSyncThrottleForTests()
	server := newTestServer(t)
	assetPath := filepath.Join(filepath.Dir(server.configFilePath), "static", "management.html")
	if err := os.MkdirAll(filepath.Dir(assetPath), 0o755); err != nil {
		t.Fatalf("mkdir asset dir: %v", err)
	}
	if err := os.WriteFile(assetPath, []byte("<html>stale asset without usage_aggregates_v2 marker</html>"), 0o644); err != nil {
		t.Fatalf("write stale asset: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/management.html", nil)
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status code: got %d want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "usage_aggregates_v2") {
		t.Fatalf("expected refreshed management asset to contain usage_aggregates_v2 marker")
	}
	if !strings.Contains(body, "rolling_windows_v1") {
		t.Fatalf("expected refreshed management asset to contain rolling_windows_v1 marker")
	}
	if !strings.Contains(body, "usage_journal_v1") {
		t.Fatalf("expected refreshed management asset to contain usage_journal_v1 marker")
	}
	if !strings.Contains(body, "__periodFallback") {
		t.Fatalf("expected refreshed management asset to contain period fallback marker")
	}
	persisted, err := os.ReadFile(assetPath)
	if err != nil {
		t.Fatalf("read refreshed asset: %v", err)
	}
	if !strings.Contains(string(persisted), "compatibility: usage_aggregates_v2 rolling_windows_v1 usage_journal_v1 __periodFallback") {
		t.Fatalf("expected runtime asset on disk to be refreshed with compatibility marker")
	}
}

func TestServeManagementControlPanel_RejectsMarkerOnlyInvalidJavaScript(t *testing.T) {
	managementasset.ResetSyncThrottleForTests()
	restoreBundled := managementasset.SetBundledManagementHTMLForTests([]byte("<html>bundled broken</html>"))
	defer restoreBundled()
	restoreFallbackURL := managementasset.SetFallbackManagementURLForTests("http://127.0.0.1:1/unreachable")
	defer restoreFallbackURL()
	server := newTestServer(t)
	assetPath := filepath.Join(filepath.Dir(server.configFilePath), "static", "management.html")
	if err := os.MkdirAll(filepath.Dir(assetPath), 0o755); err != nil {
		t.Fatalf("mkdir asset dir: %v", err)
	}
	invalid := `<html><body><!-- compatibility: usage_aggregates_v2 rolling_windows_v1 usage_journal_v1 __periodFallback --><div id="root"></div><script type="module">const usage_aggregates_v2 = true; const rolling_windows_v1 = true; const usage_journal_v1 = true; const __periodFallback = "fallback"; const createRoot = ; createRoot(document.getElementById("root"));</script></body></html>`
	if err := os.WriteFile(assetPath, []byte(invalid), 0o644); err != nil {
		t.Fatalf("write invalid asset: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/management.html", nil)
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unexpected status code: got %d want %d; body=%s", rr.Code, http.StatusServiceUnavailable, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "<html") {
		t.Fatalf("expected controlled error, got HTML body: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "management control panel unavailable") {
		t.Fatalf("expected controlled management error body, got: %s", rr.Body.String())
	}
}

func TestServeManagementControlPanel_AutocuresInvalidRuntimeAssetWithBundledValid(t *testing.T) {
	managementasset.ResetSyncThrottleForTests()
	server := newTestServer(t)
	assetPath := filepath.Join(filepath.Dir(server.configFilePath), "static", "management.html")
	if err := os.MkdirAll(filepath.Dir(assetPath), 0o755); err != nil {
		t.Fatalf("mkdir asset dir: %v", err)
	}
	invalidRuntime := strings.Replace(validManagementHTMLFixture, `const createRoot = (node) => ({ render(value) { node.dataset.rendered = String(Boolean(value)); } });`, `function xW(value) { return value; } function xW(value, mode = Date.now()) { return value + String(mode); } const createRoot = (node) => ({ render(value) { node.dataset.rendered = String(Boolean(value)); } });`, 1)
	if err := os.WriteFile(assetPath, []byte(invalidRuntime), 0o644); err != nil {
		t.Fatalf("seed invalid runtime asset: %v", err)
	}
	restoreBundled := managementasset.SetBundledManagementHTMLForTests([]byte(validManagementHTMLFixture))
	defer restoreBundled()
	restoreFallbackURL := managementasset.SetFallbackManagementURLForTests("http://127.0.0.1:1/unreachable")
	defer restoreFallbackURL()

	req := httptest.NewRequest(http.MethodGet, "/management.html", nil)
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status code: got %d want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "createRoot(root).render(globalThis.__contract)") {
		t.Fatalf("expected bundled asset to be served after autocure")
	}
	persisted, err := os.ReadFile(assetPath)
	if err != nil {
		t.Fatalf("read refreshed asset: %v", err)
	}
	if string(persisted) != validManagementHTMLFixture {
		t.Fatalf("expected runtime asset to be replaced by bundled valid asset")
	}
}

func TestEnsureLatestManagementHTML_RemoteInvalidDoesNotOverwriteValidLocal(t *testing.T) {
	managementasset.ResetSyncThrottleForTests()
	server := newTestServer(t)
	assetPath := filepath.Join(filepath.Dir(server.configFilePath), "static", "management.html")
	if err := os.MkdirAll(filepath.Dir(assetPath), 0o755); err != nil {
		t.Fatalf("mkdir asset dir: %v", err)
	}
	validLocal := strings.Replace(validManagementHTMLFixture, "fallback", "local-valid", 1)
	if err := os.WriteFile(assetPath, []byte(validLocal), 0o644); err != nil {
		t.Fatalf("write valid local asset: %v", err)
	}
	remoteInvalid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/release":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"assets":[{"name":"management.html","browser_download_url":"http://` + r.Host + `/download"}]}`))
		case "/download":
			_, _ = w.Write([]byte(strings.Replace(validManagementHTMLFixture, `const createRoot = (node) => ({ render(value) { node.dataset.rendered = String(Boolean(value)); } });`, `function xW(v) { return v; } function xW(v, mode = Date.now()) { return v + String(mode); } const createRoot = (node) => ({ render(value) { node.dataset.rendered = String(Boolean(value)); } });`, 1)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer remoteInvalid.Close()
	releaseURL := remoteInvalid.URL + "/release"
	if ok := managementasset.EnsureLatestManagementHTML(nil, filepath.Dir(assetPath), "", releaseURL); !ok {
		t.Fatalf("expected local valid asset to remain available")
	}
	persisted, err := os.ReadFile(assetPath)
	if err != nil {
		t.Fatalf("read local asset: %v", err)
	}
	if string(persisted) != validLocal {
		t.Fatalf("expected valid local asset to be preserved when remote candidate is invalid")
	}
}

func TestServeManagementControlPanel_FailsClosedWhenNoValidAssetExists(t *testing.T) {
	managementasset.ResetSyncThrottleForTests()
	server := newTestServer(t)
	assetPath := filepath.Join(filepath.Dir(server.configFilePath), "static", "management.html")
	if err := os.MkdirAll(filepath.Dir(assetPath), 0o755); err != nil {
		t.Fatalf("mkdir asset dir: %v", err)
	}
	if err := os.WriteFile(assetPath, []byte("<html>broken</html>"), 0o644); err != nil {
		t.Fatalf("write broken asset: %v", err)
	}
	restoreBundled := managementasset.SetBundledManagementHTMLForTests([]byte("<html>bundled broken</html>"))
	defer restoreBundled()
	restoreFallbackURL := managementasset.SetFallbackManagementURLForTests("http://127.0.0.1:1/unreachable")
	defer restoreFallbackURL()

	req := httptest.NewRequest(http.MethodGet, "/management.html", nil)
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unexpected status code: got %d want %d; body=%s", rr.Code, http.StatusServiceUnavailable, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "<html") {
		t.Fatalf("expected controlled error body, got HTML: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "management control panel unavailable") {
		t.Fatalf("expected controlled management error body, got: %s", rr.Body.String())
	}
}

func TestManagementControlPanel_SmokeHeadlessSettingsPage(t *testing.T) {
	chromePath, err := exec.LookPath("google-chrome-stable")
	if err != nil {
		chromePath, err = exec.LookPath("google-chrome")
	}
	if err != nil {
		t.Skip("google-chrome not available")
	}

	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	server := newTestServer(t)
	assetPath := filepath.Join(filepath.Dir(server.configFilePath), "static", "management.html")
	if err := os.MkdirAll(filepath.Dir(assetPath), 0o755); err != nil {
		t.Fatalf("mkdir asset dir: %v", err)
	}
	if err := os.WriteFile(assetPath, []byte(validManagementHTMLFixture), 0o644); err != nil {
		t.Fatalf("write valid asset: %v", err)
	}

	httpServer := httptest.NewServer(server.engine)
	defer httpServer.Close()

	profileDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "chrome.log")
	domPath := filepath.Join(t.TempDir(), "dom.html")
	args := []string{
		"--headless=new",
		"--disable-gpu",
		"--enable-logging=stderr",
		"--v=1",
		"--user-data-dir=" + profileDir,
		"--virtual-time-budget=8000",
		"--dump-dom",
		httpServer.URL + "/management.html#/settings",
	}
	cmd := exec.Command(chromePath, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run headless chrome: %v\nstderr=%s", err, stderr.String())
	}
	if err := os.WriteFile(logPath, stderr.Bytes(), 0o644); err != nil {
		t.Fatalf("write chrome log: %v", err)
	}
	if err := os.WriteFile(domPath, stdout.Bytes(), 0o644); err != nil {
		t.Fatalf("write chrome dom: %v", err)
	}
	if strings.Contains(stderr.String(), "SyntaxError") {
		t.Fatalf("unexpected SyntaxError in chrome log: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "Identifier 'xW' has already been declared") {
		t.Fatalf("duplicate xW regression detected: %s", stderr.String())
	}
	rootMatch := regexp.MustCompile(`(?s)<div id="root"([^>]*)>(.*?)</div>`).FindSubmatch(stdout.Bytes())
	if len(rootMatch) < 2 {
		t.Fatalf("expected #root element in DOM, dom=%s", stdout.String())
	}
	if !bytes.Contains(rootMatch[1], []byte(`data-rendered="true"`)) && len(bytes.TrimSpace(rootMatch[2])) == 0 {
		t.Fatalf("expected rendered #root content or render marker, dom=%s", stdout.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("/config")) {
		t.Fatalf("expected settings route to render /config redirect or equivalent, dom=%s", stdout.String())
	}
}

func TestManagementControlPanel_RequestAutocuresBrokenRuntimeAsset(t *testing.T) {
	managementasset.ResetSyncThrottleForTests()
	server := newTestServer(t)
	assetPath := filepath.Join(filepath.Dir(server.configFilePath), "static", "management.html")
	if err := os.MkdirAll(filepath.Dir(assetPath), 0o755); err != nil {
		t.Fatalf("mkdir asset dir: %v", err)
	}
	if err := os.WriteFile(assetPath, []byte("BROKEN\n"), 0o644); err != nil {
		t.Fatalf("write broken asset: %v", err)
	}
	restoreBundled := managementasset.SetBundledManagementHTMLForTests([]byte(validManagementHTMLFixture))
	defer restoreBundled()
	restoreFallbackURL := managementasset.SetFallbackManagementURLForTests("http://127.0.0.1:1/unreachable")
	defer restoreFallbackURL()

	req := httptest.NewRequest(http.MethodGet, "/management.html", nil)
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status code: got %d want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	persisted, err := os.ReadFile(assetPath)
	if err != nil {
		t.Fatalf("read autocured asset: %v", err)
	}
	if string(persisted) != validManagementHTMLFixture {
		t.Fatalf("expected broken runtime asset to be autocured on request")
	}
	servedHash := fmt.Sprintf("%x", sha256.Sum256(rr.Body.Bytes()))
	persistedHash := fmt.Sprintf("%x", sha256.Sum256(persisted))
	if servedHash != persistedHash {
		t.Fatalf("expected served asset hash %s to match persisted hash %s", servedHash, persistedHash)
	}
}

func TestDefaultRequestLoggerFactory_UsesResolvedLogDirectory(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")

	originalWD, errGetwd := os.Getwd()
	if errGetwd != nil {
		t.Fatalf("failed to get current working directory: %v", errGetwd)
	}

	tmpDir := t.TempDir()
	if errChdir := os.Chdir(tmpDir); errChdir != nil {
		t.Fatalf("failed to switch working directory: %v", errChdir)
	}
	defer func() {
		if errChdirBack := os.Chdir(originalWD); errChdirBack != nil {
			t.Fatalf("failed to restore working directory: %v", errChdirBack)
		}
	}()

	// Force ResolveLogDirectory to fallback to auth-dir/logs by making ./logs not a writable directory.
	if errWriteFile := os.WriteFile(filepath.Join(tmpDir, "logs"), []byte("not-a-directory"), 0o644); errWriteFile != nil {
		t.Fatalf("failed to create blocking logs file: %v", errWriteFile)
	}

	configDir := filepath.Join(tmpDir, "config")
	if errMkdirConfig := os.MkdirAll(configDir, 0o755); errMkdirConfig != nil {
		t.Fatalf("failed to create config dir: %v", errMkdirConfig)
	}
	configPath := filepath.Join(configDir, "config.yaml")

	authDir := filepath.Join(tmpDir, "auth")
	if errMkdirAuth := os.MkdirAll(authDir, 0o700); errMkdirAuth != nil {
		t.Fatalf("failed to create auth dir: %v", errMkdirAuth)
	}

	cfg := &proxyconfig.Config{
		SDKConfig: proxyconfig.SDKConfig{
			RequestLog: false,
		},
		AuthDir:           authDir,
		ErrorLogsMaxFiles: 10,
	}

	logger := defaultRequestLoggerFactory(cfg, configPath)
	fileLogger, ok := logger.(*internallogging.FileRequestLogger)
	if !ok {
		t.Fatalf("expected *FileRequestLogger, got %T", logger)
	}

	errLog := fileLogger.LogRequestWithOptions(
		"/v1/chat/completions",
		http.MethodPost,
		map[string][]string{"Content-Type": []string{"application/json"}},
		[]byte(`{"input":"hello"}`),
		http.StatusBadGateway,
		map[string][]string{"Content-Type": []string{"application/json"}},
		[]byte(`{"error":"upstream failure"}`),
		nil,
		nil,
		nil,
		true,
		"issue-1711",
		time.Now(),
		time.Now(),
	)
	if errLog != nil {
		t.Fatalf("failed to write forced error request log: %v", errLog)
	}

	authLogsDir := filepath.Join(authDir, "logs")
	authEntries, errReadAuthDir := os.ReadDir(authLogsDir)
	if errReadAuthDir != nil {
		t.Fatalf("failed to read auth logs dir %s: %v", authLogsDir, errReadAuthDir)
	}
	foundErrorLogInAuthDir := false
	for _, entry := range authEntries {
		if strings.HasPrefix(entry.Name(), "error-") && strings.HasSuffix(entry.Name(), ".log") {
			foundErrorLogInAuthDir = true
			break
		}
	}
	if !foundErrorLogInAuthDir {
		t.Fatalf("expected forced error log in auth fallback dir %s, got entries: %+v", authLogsDir, authEntries)
	}

	configLogsDir := filepath.Join(configDir, "logs")
	configEntries, errReadConfigDir := os.ReadDir(configLogsDir)
	if errReadConfigDir != nil && !os.IsNotExist(errReadConfigDir) {
		t.Fatalf("failed to inspect config logs dir %s: %v", configLogsDir, errReadConfigDir)
	}
	for _, entry := range configEntries {
		if strings.HasPrefix(entry.Name(), "error-") && strings.HasSuffix(entry.Name(), ".log") {
			t.Fatalf("unexpected forced error log in config dir %s", configLogsDir)
		}
	}
}
