package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func makeReloadDecisionConfig() *config.Config {
	return &config.Config{
		SDKConfig: config.SDKConfig{
			APIKeys: []string{"local-key"},
		},
		Debug: false,
		GeminiKey: []config.GeminiKey{
			{APIKey: "g1", BaseURL: "https://gemini.example"},
		},
	}
}

func TestShouldRunFullServerUpdate(t *testing.T) {
	base := makeReloadDecisionConfig()
	sameRef := base
	equalCopy := makeReloadDecisionConfig()
	diff := makeReloadDecisionConfig()
	diff.Debug = true

	if shouldRunFullServerUpdate(base, nil) {
		t.Fatal("expected nil next config to skip full update")
	}
	if !shouldRunFullServerUpdate(nil, base) {
		t.Fatal("expected nil previous config to force full update")
	}
	if shouldRunFullServerUpdate(base, sameRef) {
		t.Fatal("expected same config pointer to skip full update")
	}
	if shouldRunFullServerUpdate(base, equalCopy) {
		t.Fatal("expected equal config copy to skip full update")
	}
	if !shouldRunFullServerUpdate(base, diff) {
		t.Fatal("expected config diff to trigger full update")
	}
}

func TestShouldRebindExecutorsForConfig(t *testing.T) {
	base := makeReloadDecisionConfig()
	sameRef := base

	if shouldRebindExecutorsForConfig(base, nil) {
		t.Fatal("expected nil next config to skip executor rebind")
	}
	if !shouldRebindExecutorsForConfig(nil, base) {
		t.Fatal("expected nil previous config to trigger executor rebind")
	}
	if shouldRebindExecutorsForConfig(base, sameRef) {
		t.Fatal("expected same config pointer to skip executor rebind")
	}

	nonExecutorDiff := makeReloadDecisionConfig()
	nonExecutorDiff.Debug = true
	if shouldRebindExecutorsForConfig(base, nonExecutorDiff) {
		t.Fatal("expected debug-only diff to skip executor rebind")
	}

	executorDiff := makeReloadDecisionConfig()
	executorDiff.GeminiKey = []config.GeminiKey{
		{APIKey: "g2", BaseURL: "https://gemini.example"},
	}
	if !shouldRebindExecutorsForConfig(base, executorDiff) {
		t.Fatal("expected credential diff to trigger executor rebind")
	}
}

func TestFingerprintAnyMarshalFailure(t *testing.T) {
	if fp, ok := fingerprintAny(func() {}); ok {
		t.Fatalf("expected marshal failure for function type, got ok=true fingerprint=%q", fp)
	}
}
