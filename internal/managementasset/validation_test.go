package managementasset

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestValidateManagementHTML_AcceptsValidHTML(t *testing.T) {
	if err := ValidateManagementHTML([]byte(validManagementHTMLFixture)); err != nil {
		t.Fatalf("expected valid management HTML, got error: %v", err)
	}
}

func TestValidateManagementHTML_RejectsMissingRoot(t *testing.T) {
	invalid := strings.Replace(validManagementHTMLFixture, `<div id="root"></div>`, `<div id="nope"></div>`, 1)
	if err := ValidateManagementHTML([]byte(invalid)); err == nil || !strings.Contains(err.Error(), "div#root") {
		t.Fatalf("expected missing root error, got: %v", err)
	}
}

func TestValidateManagementHTML_RejectsMissingMarkers(t *testing.T) {
	invalid := strings.ReplaceAll(validManagementHTMLFixture, "usage_journal_v1", "usage_journal_missing")
	if err := ValidateManagementHTML([]byte(invalid)); err == nil || !strings.Contains(err.Error(), "compatibility marker") {
		t.Fatalf("expected missing marker error, got: %v", err)
	}
}

func TestValidateManagementHTML_RejectsInvalidJavaScript(t *testing.T) {
	invalid := strings.Replace(validManagementHTMLFixture, `const createRoot = (node) => ({ render(value) { node.dataset.rendered = String(Boolean(value)); } });`, `const createRoot = ;`, 1)
	if err := ValidateManagementHTML([]byte(invalid)); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("expected invalid JS error, got: %v", err)
	}
}

func TestValidateManagementHTML_RejectsDuplicateTopLevelBinding(t *testing.T) {
	invalid := strings.Replace(validManagementHTMLFixture, `const createRoot = (node) => ({ render(value) { node.dataset.rendered = String(Boolean(value)); } });`, "function xW(value) { return value; }\n      function xW(value, mode = Date.now()) { return value + String(mode); }\n      const createRoot = (node) => ({ render(value) { node.dataset.rendered = String(Boolean(value)); } });", 1)
	if err := ValidateManagementHTML([]byte(invalid)); err == nil || !strings.Contains(err.Error(), `duplicate top-level binding "xW"`) {
		t.Fatalf("expected duplicate binding error, got: %v", err)
	}
}

func TestValidateManagementHTMLFile_AcceptsValidHTML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "management.html")
	if err := os.WriteFile(path, []byte(validManagementHTMLFixture), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := ValidateManagementHTMLFile(path); err != nil {
		t.Fatalf("expected valid file, got error: %v", err)
	}
}
