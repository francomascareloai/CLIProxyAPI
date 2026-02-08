package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigOptional_DefaultsAuthReloadWindows(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 9001\nauth-dir: /tmp/auth\n"), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional failed: %v", err)
	}
	if cfg.AuthReloadDebounceMS != 250 {
		t.Fatalf("expected default auth reload debounce 250ms, got %d", cfg.AuthReloadDebounceMS)
	}
	if cfg.AuthReloadMaxCoalesceMS != 1000 {
		t.Fatalf("expected default auth reload max coalesce 1000ms, got %d", cfg.AuthReloadMaxCoalesceMS)
	}
}

func TestLoadConfigOptional_SanitizesAuthReloadWindows(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	content := "port: 9001\nauth-dir: /tmp/auth\nauth-reload-debounce-ms: 5\nauth-reload-max-coalesce-ms: 10\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional failed: %v", err)
	}
	if cfg.AuthReloadDebounceMS != 25 {
		t.Fatalf("expected clamped auth reload debounce 25ms, got %d", cfg.AuthReloadDebounceMS)
	}
	if cfg.AuthReloadMaxCoalesceMS != 25 {
		t.Fatalf("expected max coalesce to clamp to debounce (25ms), got %d", cfg.AuthReloadMaxCoalesceMS)
	}
}

func TestLoadConfigOptional_DefaultsModelsCacheAndUpstreamHTTP(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 9001\nauth-dir: /tmp/auth\n"), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional failed: %v", err)
	}
	if cfg.ModelsCache.Strategy != DefaultModelsCacheStrategy {
		t.Fatalf("expected models cache strategy %q, got %q", DefaultModelsCacheStrategy, cfg.ModelsCache.Strategy)
	}
	if cfg.ModelsCache.TTLMs != DefaultModelsCacheTTLMs {
		t.Fatalf("expected models cache ttl %d, got %d", DefaultModelsCacheTTLMs, cfg.ModelsCache.TTLMs)
	}
	if cfg.UpstreamHTTP.MaxIdleConns != DefaultUpstreamMaxIdleConns {
		t.Fatalf("expected max idle conns %d, got %d", DefaultUpstreamMaxIdleConns, cfg.UpstreamHTTP.MaxIdleConns)
	}
	if cfg.UpstreamHTTP.MaxIdleConnsPerHost != DefaultUpstreamIdlePerHost {
		t.Fatalf("expected max idle conns per host %d, got %d", DefaultUpstreamIdlePerHost, cfg.UpstreamHTTP.MaxIdleConnsPerHost)
	}
	if cfg.ProviderResilience.FailureThreshold != DefaultProviderCBThreshold {
		t.Fatalf("expected provider failure threshold %d, got %d", DefaultProviderCBThreshold, cfg.ProviderResilience.FailureThreshold)
	}
	if cfg.ProviderResilience.MaxInflightPerProvider != DefaultProviderMaxInFlight {
		t.Fatalf("expected max inflight per provider %d, got %d", DefaultProviderMaxInFlight, cfg.ProviderResilience.MaxInflightPerProvider)
	}
	if cfg.ProviderResilience.AdaptiveLimiterEnabled {
		t.Fatalf("expected adaptive limiter default disabled")
	}
	if cfg.ProviderResilience.AdaptiveMinInflight != DefaultProviderAdaptiveMin {
		t.Fatalf("expected adaptive min inflight %d, got %d", DefaultProviderAdaptiveMin, cfg.ProviderResilience.AdaptiveMinInflight)
	}
	if cfg.ProviderResilience.AdaptiveMaxInflight != DefaultProviderAdaptiveMax {
		t.Fatalf("expected adaptive max inflight %d, got %d", DefaultProviderAdaptiveMax, cfg.ProviderResilience.AdaptiveMaxInflight)
	}
	if cfg.ProviderResilience.AdaptiveSuccessWindow != DefaultProviderAdaptiveWin {
		t.Fatalf("expected adaptive success window %d, got %d", DefaultProviderAdaptiveWin, cfg.ProviderResilience.AdaptiveSuccessWindow)
	}
	if cfg.ProviderResilience.AdaptiveAdditiveStep != DefaultProviderAdaptiveStep {
		t.Fatalf("expected adaptive additive step %d, got %d", DefaultProviderAdaptiveStep, cfg.ProviderResilience.AdaptiveAdditiveStep)
	}
	if cfg.ProviderResilience.AdaptiveBackoffFactor != DefaultProviderAdaptiveDecay {
		t.Fatalf("expected adaptive backoff factor %.2f, got %.2f", DefaultProviderAdaptiveDecay, cfg.ProviderResilience.AdaptiveBackoffFactor)
	}
	if len(cfg.ProviderResilience.AdaptiveProviders) != 0 {
		t.Fatalf("expected adaptive providers default empty, got %v", cfg.ProviderResilience.AdaptiveProviders)
	}
	if cfg.ProviderResilience.AdaptiveAutoRollbackEnabled {
		t.Fatalf("expected adaptive auto rollback default disabled")
	}
	if cfg.ProviderResilience.AdaptiveAutoRollbackWindowMS != DefaultAdaptiveRollbackWinMS {
		t.Fatalf("expected adaptive rollback window %d, got %d", DefaultAdaptiveRollbackWinMS, cfg.ProviderResilience.AdaptiveAutoRollbackWindowMS)
	}
	if cfg.ProviderResilience.AdaptiveAutoRollbackMinSamples != DefaultAdaptiveRollbackMinN {
		t.Fatalf("expected adaptive rollback min samples %d, got %d", DefaultAdaptiveRollbackMinN, cfg.ProviderResilience.AdaptiveAutoRollbackMinSamples)
	}
	if cfg.ProviderResilience.AdaptiveAutoRollbackMaxP95MS != DefaultAdaptiveRollbackP95MS {
		t.Fatalf("expected adaptive rollback max p95 %d, got %d", DefaultAdaptiveRollbackP95MS, cfg.ProviderResilience.AdaptiveAutoRollbackMaxP95MS)
	}
	if cfg.ProviderResilience.AdaptiveAutoRollbackMaxErrRate != DefaultAdaptiveRollbackErr {
		t.Fatalf("expected adaptive rollback max err rate %.2f, got %.2f", DefaultAdaptiveRollbackErr, cfg.ProviderResilience.AdaptiveAutoRollbackMaxErrRate)
	}
	if cfg.ProviderResilience.AdaptiveAutoRollbackMax429Rate != DefaultAdaptiveRollback429 {
		t.Fatalf("expected adaptive rollback max http429 rate %.2f, got %.2f", DefaultAdaptiveRollback429, cfg.ProviderResilience.AdaptiveAutoRollbackMax429Rate)
	}
	if cfg.ProviderResilience.AdaptiveAutoRollbackMax5xxRate != DefaultAdaptiveRollback5xx {
		t.Fatalf("expected adaptive rollback max http5xx rate %.2f, got %.2f", DefaultAdaptiveRollback5xx, cfg.ProviderResilience.AdaptiveAutoRollbackMax5xxRate)
	}
	if cfg.AutoProfile.Enable {
		t.Fatalf("expected auto-profile default disabled")
	}
	if cfg.AutoProfile.LatencyThresholdMS != DefaultAutoProfileLatencyMS {
		t.Fatalf("expected auto-profile latency threshold %d, got %d", DefaultAutoProfileLatencyMS, cfg.AutoProfile.LatencyThresholdMS)
	}
	if cfg.AutoProfile.CooldownMS != DefaultAutoProfileCooldownMS {
		t.Fatalf("expected auto-profile cooldown %d, got %d", DefaultAutoProfileCooldownMS, cfg.AutoProfile.CooldownMS)
	}
	if cfg.AutoProfile.MaxFiles != DefaultAutoProfileMaxFiles {
		t.Fatalf("expected auto-profile max files %d, got %d", DefaultAutoProfileMaxFiles, cfg.AutoProfile.MaxFiles)
	}
	if len(cfg.AutoProfile.TriggerStatuses) == 0 {
		t.Fatal("expected auto-profile default trigger statuses")
	}
}

func TestLoadConfigOptional_SanitizesModelsCacheAndUpstreamHTTP(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	content := "" +
		"port: 9001\n" +
		"auth-dir: /tmp/auth\n" +
		"models-cache:\n" +
		"  strategy: invalid\n" +
		"  ttl-ms: 1\n" +
		"upstream-http:\n" +
		"  max-idle-conns: -1\n" +
		"  max-idle-conns-per-host: 0\n" +
		"  max-conns-per-host: 0\n" +
		"  idle-conn-timeout-ms: 0\n" +
		"  tls-handshake-timeout-ms: 0\n" +
		"  response-header-timeout-ms: 0\n" +
		"  expect-continue-timeout-ms: 0\n"
	content += "" +
		"provider-resilience:\n" +
		"  failure-threshold: 0\n" +
		"  half-open-max-requests: 0\n" +
		"  open-state-ms: 0\n" +
		"  max-inflight-per-provider: 0\n" +
		"  adaptive-providers: [\" CODEx \", \"codex\", \"\", \"gemini-cli\"]\n" +
		"  adaptive-min-inflight: 999999\n" +
		"  adaptive-max-inflight: -1\n" +
		"  adaptive-success-window: 0\n" +
		"  adaptive-additive-step: 0\n" +
		"  adaptive-backoff-factor: 2.0\n" +
		"  adaptive-auto-rollback-window-ms: 1\n" +
		"  adaptive-auto-rollback-min-samples: 1\n" +
		"  adaptive-auto-rollback-max-p95-ms: 1\n" +
		"  adaptive-auto-rollback-max-error-rate: 2.0\n" +
		"  adaptive-auto-rollback-max-http429-rate: 2.0\n" +
		"  adaptive-auto-rollback-max-http5xx-rate: 2.0\n" +
		"auto-profile:\n" +
		"  output-dir: \"\"\n" +
		"  latency-threshold-ms: 1\n" +
		"  cooldown-ms: 1\n" +
		"  max-files: 1\n" +
		"  trigger-statuses: [99, 700, 500, 500]\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional failed: %v", err)
	}
	if cfg.ModelsCache.Strategy != DefaultModelsCacheStrategy {
		t.Fatalf("expected sanitized models cache strategy %q, got %q", DefaultModelsCacheStrategy, cfg.ModelsCache.Strategy)
	}
	if cfg.ModelsCache.TTLMs != 25 {
		t.Fatalf("expected clamped models cache ttl 25ms, got %d", cfg.ModelsCache.TTLMs)
	}
	if cfg.UpstreamHTTP.MaxIdleConns != DefaultUpstreamMaxIdleConns {
		t.Fatalf("expected default max idle conns %d, got %d", DefaultUpstreamMaxIdleConns, cfg.UpstreamHTTP.MaxIdleConns)
	}
	if cfg.UpstreamHTTP.ResponseHeaderTimeoutMS != DefaultUpstreamRespHdrMS {
		t.Fatalf("expected default response header timeout %d, got %d", DefaultUpstreamRespHdrMS, cfg.UpstreamHTTP.ResponseHeaderTimeoutMS)
	}
	if cfg.ProviderResilience.FailureThreshold != DefaultProviderCBThreshold {
		t.Fatalf("expected default provider threshold %d, got %d", DefaultProviderCBThreshold, cfg.ProviderResilience.FailureThreshold)
	}
	if cfg.ProviderResilience.HalfOpenMaxRequests != DefaultProviderHalfOpenMax {
		t.Fatalf("expected default half-open max %d, got %d", DefaultProviderHalfOpenMax, cfg.ProviderResilience.HalfOpenMaxRequests)
	}
	if cfg.ProviderResilience.AdaptiveMinInflight != cfg.ProviderResilience.MaxInflightPerProvider {
		t.Fatalf("expected adaptive min to clamp to max inflight (%d), got %d", cfg.ProviderResilience.MaxInflightPerProvider, cfg.ProviderResilience.AdaptiveMinInflight)
	}
	if cfg.ProviderResilience.AdaptiveMaxInflight != cfg.ProviderResilience.MaxInflightPerProvider {
		t.Fatalf("expected adaptive max to clamp to max inflight (%d), got %d", cfg.ProviderResilience.MaxInflightPerProvider, cfg.ProviderResilience.AdaptiveMaxInflight)
	}
	if cfg.ProviderResilience.AdaptiveSuccessWindow != DefaultProviderAdaptiveWin {
		t.Fatalf("expected adaptive success window default %d, got %d", DefaultProviderAdaptiveWin, cfg.ProviderResilience.AdaptiveSuccessWindow)
	}
	if cfg.ProviderResilience.AdaptiveAdditiveStep != DefaultProviderAdaptiveStep {
		t.Fatalf("expected adaptive additive step default %d, got %d", DefaultProviderAdaptiveStep, cfg.ProviderResilience.AdaptiveAdditiveStep)
	}
	if cfg.ProviderResilience.AdaptiveBackoffFactor != DefaultProviderAdaptiveDecay {
		t.Fatalf("expected adaptive backoff factor default %.2f, got %.2f", DefaultProviderAdaptiveDecay, cfg.ProviderResilience.AdaptiveBackoffFactor)
	}
	if len(cfg.ProviderResilience.AdaptiveProviders) != 2 ||
		cfg.ProviderResilience.AdaptiveProviders[0] != "codex" ||
		cfg.ProviderResilience.AdaptiveProviders[1] != "gemini-cli" {
		t.Fatalf("expected normalized adaptive providers [codex gemini-cli], got %v", cfg.ProviderResilience.AdaptiveProviders)
	}
	if cfg.ProviderResilience.AdaptiveAutoRollbackWindowMS != 10000 {
		t.Fatalf("expected adaptive rollback window clamped to 10000ms, got %d", cfg.ProviderResilience.AdaptiveAutoRollbackWindowMS)
	}
	if cfg.ProviderResilience.AdaptiveAutoRollbackMinSamples != 5 {
		t.Fatalf("expected adaptive rollback min samples clamped to 5, got %d", cfg.ProviderResilience.AdaptiveAutoRollbackMinSamples)
	}
	if cfg.ProviderResilience.AdaptiveAutoRollbackMaxP95MS != 50 {
		t.Fatalf("expected adaptive rollback max p95 clamped to 50ms, got %d", cfg.ProviderResilience.AdaptiveAutoRollbackMaxP95MS)
	}
	if cfg.ProviderResilience.AdaptiveAutoRollbackMaxErrRate != DefaultAdaptiveRollbackErr {
		t.Fatalf("expected adaptive rollback max err rate default %.2f, got %.2f", DefaultAdaptiveRollbackErr, cfg.ProviderResilience.AdaptiveAutoRollbackMaxErrRate)
	}
	if cfg.ProviderResilience.AdaptiveAutoRollbackMax429Rate != DefaultAdaptiveRollback429 {
		t.Fatalf("expected adaptive rollback max http429 rate default %.2f, got %.2f", DefaultAdaptiveRollback429, cfg.ProviderResilience.AdaptiveAutoRollbackMax429Rate)
	}
	if cfg.ProviderResilience.AdaptiveAutoRollbackMax5xxRate != DefaultAdaptiveRollback5xx {
		t.Fatalf("expected adaptive rollback max http5xx rate default %.2f, got %.2f", DefaultAdaptiveRollback5xx, cfg.ProviderResilience.AdaptiveAutoRollbackMax5xxRate)
	}
	if cfg.AutoProfile.LatencyThresholdMS != 1000 {
		t.Fatalf("expected auto-profile latency threshold clamped to 1000ms, got %d", cfg.AutoProfile.LatencyThresholdMS)
	}
	if cfg.AutoProfile.CooldownMS != 10000 {
		t.Fatalf("expected auto-profile cooldown clamped to 10000ms, got %d", cfg.AutoProfile.CooldownMS)
	}
	if cfg.AutoProfile.MaxFiles != 5 {
		t.Fatalf("expected auto-profile max files clamped to 5, got %d", cfg.AutoProfile.MaxFiles)
	}
	if len(cfg.AutoProfile.TriggerStatuses) != 1 || cfg.AutoProfile.TriggerStatuses[0] != 500 {
		t.Fatalf("expected normalized trigger statuses [500], got %v", cfg.AutoProfile.TriggerStatuses)
	}
}
