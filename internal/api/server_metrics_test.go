package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func runMetricsRequest(t *testing.T, server *Server) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status code: got %d body=%s", rr.Code, rr.Body.String())
	}
	return rr
}

func TestMetricsHandlerExposesBaseCounters(t *testing.T) {
	server := newTestServer(t)
	server.modelsCacheHit.Add(3)
	server.modelsCacheMiss.Add(1)
	server.fullUpdateCount.Add(2)
	server.incrementalUpdateCount.Add(4)

	rr := runMetricsRequest(t, server)

	contentType := rr.Header().Get("Content-Type")
	if !strings.HasPrefix(contentType, "text/plain;") {
		t.Fatalf("unexpected content type: %q", contentType)
	}

	body := rr.Body.String()
	expectedLines := []string{
		"cliproxy_models_cache_hit_total 3",
		"cliproxy_models_cache_miss_total 1",
		"cliproxy_models_cache_build_total 0",
		"cliproxy_models_cache_invalidate_total 0",
		"cliproxy_models_cache_requests_total 4",
		"cliproxy_models_cache_hit_ratio 0.750000",
		"cliproxy_server_full_update_total 2",
		"cliproxy_server_incremental_update_total 4",
		"cliproxy_models_cache_strategy_ttl_legacy 0.000000",
		"cliproxy_autoprofile_capture_total 0",
		"cliproxy_autoprofile_cooldown_skip_total 0",
		"cliproxy_autoprofile_error_total 0",
		"cliproxy_autoprofile_enabled 0.000000",
	}
	for _, line := range expectedLines {
		if !strings.Contains(body, line) {
			t.Fatalf("missing metrics line %q in body:\n%s", line, body)
		}
	}
}

func TestMetricsHandlerIncludesRuntimeProviderCounters(t *testing.T) {
	server := newTestServer(t)
	providerCalls := 0
	server.SetRuntimeMetricsProvider(func() map[string]uint64 {
		providerCalls++
		return map[string]uint64{
			"cliproxy_watcher_auth_reload_requested_total": 5,
			"cliproxy_runtime_custom_total":                9,
			"   ":                                          42,
		}
	})

	runMetricsRequest(t, server)
	rr := runMetricsRequest(t, server)
	if providerCalls != 2 {
		t.Fatalf("expected metrics provider to run once per scrape, got %d", providerCalls)
	}

	body := rr.Body.String()
	expectedLines := []string{
		"# HELP cliproxy_watcher_auth_reload_requested_total Total number of watcher auth reload requests.",
		"cliproxy_watcher_auth_reload_requested_total 5",
		"# HELP cliproxy_runtime_custom_total CLIProxy runtime metric.",
		"cliproxy_runtime_custom_total 9",
	}
	for _, line := range expectedLines {
		if !strings.Contains(body, line) {
			t.Fatalf("missing metrics line %q in body:\n%s", line, body)
		}
	}
	if strings.Contains(body, " 42\n") {
		t.Fatalf("blank runtime metrics key should have been ignored:\n%s", body)
	}
}
