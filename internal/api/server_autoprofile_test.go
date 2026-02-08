package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestShouldAutoProfileCapture(t *testing.T) {
	cfg := config.AutoProfileConfig{
		LatencyThresholdMS: 200,
		TriggerStatuses:    []int{429, 500},
	}

	if !shouldAutoProfileCapture(cfg, 200, 250*time.Millisecond) {
		t.Fatal("expected capture by latency threshold")
	}
	if !shouldAutoProfileCapture(cfg, 429, 10*time.Millisecond) {
		t.Fatal("expected capture by status trigger")
	}
	if shouldAutoProfileCapture(cfg, 201, 10*time.Millisecond) {
		t.Fatal("did not expect capture for non-trigger status and low latency")
	}
}

func TestMarkAutoProfileCaptureCooldown(t *testing.T) {
	server := newTestServer(t)
	now := time.Now()
	cooldown := 500 * time.Millisecond

	if !server.markAutoProfileCapture(now, cooldown) {
		t.Fatal("expected first capture mark to pass")
	}
	if server.markAutoProfileCapture(now.Add(100*time.Millisecond), cooldown) {
		t.Fatal("expected second capture mark inside cooldown to be skipped")
	}
	if got := server.autoProfileSkipped.Load(); got != 1 {
		t.Fatalf("expected one skipped capture, got %d", got)
	}
	if !server.markAutoProfileCapture(now.Add(600*time.Millisecond), cooldown) {
		t.Fatal("expected capture mark to pass after cooldown")
	}
}

func TestCaptureAutoProfileWritesAndPrunes(t *testing.T) {
	server := newTestServer(t)
	tmpDir := t.TempDir()

	server.captureAutoProfile(time.Now(), tmpDir, 2, 503, 123*time.Millisecond)
	server.captureAutoProfile(time.Now().Add(1*time.Millisecond), tmpDir, 2, 504, 234*time.Millisecond)
	server.captureAutoProfile(time.Now().Add(2*time.Millisecond), tmpDir, 2, 429, 345*time.Millisecond)

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("failed to read profile dir: %v", err)
	}

	pprofFiles := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := strings.ToLower(entry.Name())
		if strings.HasPrefix(name, autoProfileFilePrefix) && strings.HasSuffix(name, ".pprof") {
			pprofFiles++
		}
	}
	if pprofFiles > 2 {
		t.Fatalf("expected at most 2 profile files after pruning, got %d", pprofFiles)
	}
	if got := server.autoProfileCaptured.Load(); got == 0 {
		t.Fatal("expected auto-profile captured counter to increase")
	}
}

func TestResolveAutoProfileOutputDir(t *testing.T) {
	server := newTestServer(t)
	custom := filepath.Join(t.TempDir(), "profiles")
	if got := server.resolveAutoProfileOutputDir(custom); got != custom {
		t.Fatalf("expected custom output dir %q, got %q", custom, got)
	}

	server.currentPath = t.TempDir()
	got := server.resolveAutoProfileOutputDir("")
	if !strings.Contains(got, "profiles") {
		t.Fatalf("expected default output dir under profiles, got %q", got)
	}
}
