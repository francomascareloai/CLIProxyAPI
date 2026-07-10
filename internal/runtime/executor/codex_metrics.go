package executor

import (
	"strconv"
	"sync/atomic"
	"time"
)

var codexMetricBucketBoundsMS = [...]uint64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000}

type codexMetricHistogram struct {
	count  atomic.Uint64
	sumNS  atomic.Uint64
	buckets [len(codexMetricBucketBoundsMS)]atomic.Uint64
}

func (h *codexMetricHistogram) observe(d time.Duration) {
	if d <= 0 {
		return
	}
	ns := uint64(d.Nanoseconds())
	h.count.Add(1)
	h.sumNS.Add(ns)
	ms := uint64(d / time.Millisecond)
	if d%time.Millisecond != 0 {
		ms++
	}
	for i, upper := range codexMetricBucketBoundsMS {
		if ms <= upper {
			h.buckets[i].Add(1)
		}
	}
}

func (h *codexMetricHistogram) snapshot(prefix string, out map[string]uint64) {
	if h == nil {
		return
	}
	out[prefix+"_total"] = h.count.Load()
	out[prefix+"_ns_sum"] = h.sumNS.Load()
	for i, upper := range codexMetricBucketBoundsMS {
		out[prefix+"_ms_le_"+strconv.FormatUint(upper, 10)+"_total"] = h.buckets[i].Load()
	}
}

type codexRuntimeMetrics struct {
	httpNonStreamPrepare        codexMetricHistogram
	httpNonStreamTTFB           codexMetricHistogram
	httpNonStreamCompletionWait codexMetricHistogram
	httpNonStreamTail           codexMetricHistogram
	httpCompactPrepare          codexMetricHistogram
	httpCompactTTFB             codexMetricHistogram
	httpCompactTail             codexMetricHistogram

	strategyLegacyStream      atomic.Uint64
	strategyCompactAuto       atomic.Uint64
	strategyCompactForce      atomic.Uint64
	strategyWebsocket         atomic.Uint64
	compactAutoFallback       atomic.Uint64
	websocketFallback         atomic.Uint64
	httpNonStreamCompleted    atomic.Uint64
	httpNonStreamMissingDone  atomic.Uint64
	httpNonStreamEventError   atomic.Uint64
	httpNonStreamBytes        atomic.Uint64
}

var globalCodexRuntimeMetrics codexRuntimeMetrics

func noteCodexStrategyLegacyStream() {
	globalCodexRuntimeMetrics.strategyLegacyStream.Add(1)
}

func noteCodexStrategyCompactAuto() {
	globalCodexRuntimeMetrics.strategyCompactAuto.Add(1)
}

func noteCodexStrategyCompactForce() {
	globalCodexRuntimeMetrics.strategyCompactForce.Add(1)
}

func noteCodexStrategyWebsocket() {
	globalCodexRuntimeMetrics.strategyWebsocket.Add(1)
}

func noteCodexCompactAutoFallback() {
	globalCodexRuntimeMetrics.compactAutoFallback.Add(1)
}

func noteCodexWebsocketFallback() {
	globalCodexRuntimeMetrics.websocketFallback.Add(1)
}

func observeCodexHTTPNonStreamPrepare(d time.Duration) {
	globalCodexRuntimeMetrics.httpNonStreamPrepare.observe(d)
}

func observeCodexHTTPNonStreamTTFB(d time.Duration) {
	globalCodexRuntimeMetrics.httpNonStreamTTFB.observe(d)
}

func observeCodexHTTPNonStreamCompletionWait(d time.Duration) {
	globalCodexRuntimeMetrics.httpNonStreamCompletionWait.observe(d)
}

func observeCodexHTTPNonStreamTail(d time.Duration) {
	globalCodexRuntimeMetrics.httpNonStreamTail.observe(d)
}

func observeCodexHTTPCompactPrepare(d time.Duration) {
	globalCodexRuntimeMetrics.httpCompactPrepare.observe(d)
}

func observeCodexHTTPCompactTTFB(d time.Duration) {
	globalCodexRuntimeMetrics.httpCompactTTFB.observe(d)
}

func observeCodexHTTPCompactTail(d time.Duration) {
	globalCodexRuntimeMetrics.httpCompactTail.observe(d)
}

func noteCodexHTTPNonStreamCompleted() {
	globalCodexRuntimeMetrics.httpNonStreamCompleted.Add(1)
}

func noteCodexHTTPNonStreamMissingCompleted() {
	globalCodexRuntimeMetrics.httpNonStreamMissingDone.Add(1)
}

func noteCodexHTTPNonStreamEventError() {
	globalCodexRuntimeMetrics.httpNonStreamEventError.Add(1)
}

func addCodexHTTPNonStreamBytes(n int) {
	if n <= 0 {
		return
	}
	globalCodexRuntimeMetrics.httpNonStreamBytes.Add(uint64(n))
}

func CodexRuntimeMetricsSnapshot() map[string]uint64 {
	out := map[string]uint64{
		"cliproxy_codex_strategy_legacy_stream_total":           globalCodexRuntimeMetrics.strategyLegacyStream.Load(),
		"cliproxy_codex_strategy_compact_auto_total":            globalCodexRuntimeMetrics.strategyCompactAuto.Load(),
		"cliproxy_codex_strategy_compact_force_total":           globalCodexRuntimeMetrics.strategyCompactForce.Load(),
		"cliproxy_codex_strategy_websocket_total":               globalCodexRuntimeMetrics.strategyWebsocket.Load(),
		"cliproxy_codex_strategy_compact_auto_fallback_total":   globalCodexRuntimeMetrics.compactAutoFallback.Load(),
		"cliproxy_codex_strategy_websocket_fallback_total":      globalCodexRuntimeMetrics.websocketFallback.Load(),
		"cliproxy_codex_http_nonstream_completed_total":         globalCodexRuntimeMetrics.httpNonStreamCompleted.Load(),
		"cliproxy_codex_http_nonstream_missing_completed_total": globalCodexRuntimeMetrics.httpNonStreamMissingDone.Load(),
		"cliproxy_codex_http_nonstream_event_error_total":       globalCodexRuntimeMetrics.httpNonStreamEventError.Load(),
		"cliproxy_codex_http_nonstream_bytes_total":             globalCodexRuntimeMetrics.httpNonStreamBytes.Load(),
	}
	globalCodexRuntimeMetrics.httpNonStreamPrepare.snapshot("cliproxy_codex_http_nonstream_prepare", out)
	globalCodexRuntimeMetrics.httpNonStreamTTFB.snapshot("cliproxy_codex_http_nonstream_ttfb", out)
	globalCodexRuntimeMetrics.httpNonStreamCompletionWait.snapshot("cliproxy_codex_http_nonstream_completion_wait", out)
	globalCodexRuntimeMetrics.httpNonStreamTail.snapshot("cliproxy_codex_http_nonstream_tail", out)
	globalCodexRuntimeMetrics.httpCompactPrepare.snapshot("cliproxy_codex_http_compact_prepare", out)
	globalCodexRuntimeMetrics.httpCompactTTFB.snapshot("cliproxy_codex_http_compact_ttfb", out)
	globalCodexRuntimeMetrics.httpCompactTail.snapshot("cliproxy_codex_http_compact_tail", out)
	return out
}
