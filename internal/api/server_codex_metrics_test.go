package api

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
)

func TestMetricsHandlerIncludesCodexRuntimeCounters(t *testing.T) {
	server := newTestServer(t)
	server.SetRuntimeMetricsProvider(func() map[string]uint64 {
		return executor.CodexRuntimeMetricsSnapshot()
	})

	body := runMetricsRequest(t, server).Body.String()
	for _, line := range []string{
		"# HELP cliproxy_codex_strategy_legacy_stream_total Total Codex non-stream requests executed via legacy SSE-over-HTTP path.",
		"cliproxy_codex_strategy_legacy_stream_total 0",
		"# HELP cliproxy_codex_http_nonstream_completed_total Total Codex HTTP non-stream requests completed after first response.completed event.",
		"cliproxy_codex_http_nonstream_completed_total 0",
	} {
		if !strings.Contains(body, line) {
			t.Fatalf("missing metrics line %q in body:\n%s", line, body)
		}
	}
}
