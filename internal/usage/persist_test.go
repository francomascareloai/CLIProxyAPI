package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func resetUsageHMACKeyForTest() {
	usageHMACKeyOnce = sync.Once{}
	usageHMACKey = nil
	usageHMACKeyErr = nil
	usageHMACKeyLogOnce = sync.Once{}
}

func TestEnsureUsageHMACKey_IsStableAcrossReload(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	resetUsageHMACKeyForTest()

	key1, err := ensureUsageHMACKey()
	if err != nil {
		t.Fatalf("ensureUsageHMACKey() #1: %v", err)
	}
	if len(key1) != usageHMACKeyBytesSize {
		t.Fatalf("unexpected key length: %d", len(key1))
	}

	keyPath, err := usageHMACKeyPath()
	if err != nil {
		t.Fatalf("usageHMACKeyPath(): %v", err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key path: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("unexpected key perms: %v", got)
	}

	resetUsageHMACKeyForTest()
	key2, err := ensureUsageHMACKey()
	if err != nil {
		t.Fatalf("ensureUsageHMACKey() #2: %v", err)
	}
	if !bytes.Equal(key1, key2) {
		t.Fatalf("expected stable key across reload")
	}
}

func TestHashClientKey_IsStableAndDistinct(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	resetUsageHMACKeyForTest()

	id1 := HashClientKey("sk-test-123")
	id2 := HashClientKey("sk-test-123")
	id3 := HashClientKey("sk-test-456")

	if id1 == "" {
		t.Fatalf("expected non-empty id")
	}
	if id1 != id2 {
		t.Fatalf("expected stable id")
	}
	if id1 == id3 {
		t.Fatalf("expected distinct ids")
	}
}

func TestPersistedUsageStats_DoesNotContainPlainAPIKey(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	resetUsageHMACKeyForTest()

	stats := NewRequestStatistics()
	apiKey := "sk_plaintext_should_not_appear_1234567890"

	stats.Record(context.Background(), coreusage.Record{
		Provider:    "claude",
		Model:       "claude-3-5-sonnet",
		APIKey:      apiKey,
		RequestedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		Failed:      false,
		Detail: coreusage.Detail{
			InputTokens:  10,
			OutputTokens: 20,
			TotalTokens:  30,
		},
	})

	p := &UsagePersister{stats: stats}
	if err := p.flush(true); err != nil {
		t.Fatalf("flush: %v", err)
	}

	statsPath, err := usageStatsPath()
	if err != nil {
		t.Fatalf("usageStatsPath(): %v", err)
	}

	data, err := os.ReadFile(statsPath)
	if err != nil {
		t.Fatalf("read stats file: %v", err)
	}
	if bytes.Contains(data, []byte(apiKey)) {
		t.Fatalf("persisted stats must not contain plaintext api key")
	}

	snapshot := stats.Snapshot()
	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if bytes.Contains(payload, []byte(apiKey)) {
		t.Fatalf("usage snapshot must not contain plaintext api key")
	}

	if info, err := os.Stat(statsPath); err == nil {
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("unexpected stats perms: %v", got)
		}
	}

	// Ensure the file is created under ~/.cliproxy.
	wantDir := filepath.Join(tmp, cliproxyDirName)
	if filepath.Dir(statsPath) != wantDir {
		t.Fatalf("unexpected stats dir: %s", filepath.Dir(statsPath))
	}
}

func TestAggregatedSnapshot_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	resetUsageHMACKeyForTest()

	stats1 := NewRequestStatistics()
	stats1.Record(context.Background(), coreusage.Record{
		Provider:    "claude",
		Model:       "claude-3-5-sonnet",
		APIKey:      "sk_test_roundtrip_a",
		RequestedAt: time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC),
		Failed:      false,
		Detail: coreusage.Detail{
			InputTokens:  1,
			OutputTokens: 2,
			TotalTokens:  3,
		},
	})
	stats1.Record(context.Background(), coreusage.Record{
		Provider:    "claude",
		Model:       "claude-3-5-sonnet",
		APIKey:      "sk_test_roundtrip_a",
		RequestedAt: time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC),
		Failed:      true,
		Detail: coreusage.Detail{
			InputTokens:  10,
			OutputTokens: 20,
			TotalTokens:  30,
		},
	})

	snapshot := stats1.SnapshotAggregated()

	stats2 := NewRequestStatistics()
	stats2.ApplyAggregatedSnapshot(snapshot)

	snapshot2 := stats2.SnapshotAggregated()
	if !reflect.DeepEqual(snapshot, snapshot2) {
		t.Fatalf("expected aggregated snapshot round-trip to preserve values")
	}
}

func TestReplaceAggregatedSnapshot_IsIdempotent(t *testing.T) {
	stats := NewRequestStatistics()
	snapshot := AggregatedStatisticsSnapshot{
		TotalRequests: 42,
		SuccessCount:  40,
		FailureCount:  2,
		TotalTokens:   4200,
		APIs: map[string]AggregatedAPISnapshot{
			"api:hmac256:test": {
				TotalRequests: 42,
				TotalTokens:   4200,
				Models: map[string]AggregatedModelSnapshot{
					"gemini-3-pro-preview": {
						TotalRequests: 42,
						TotalTokens:   4200,
					},
				},
			},
		},
		RequestsByDay: map[string]int64{
			"2026-02-20": 42,
		},
		RequestsByHour: map[string]int64{
			"10": 7,
		},
		TokensByDay: map[string]int64{
			"2026-02-20": 4200,
		},
		TokensByHour: map[string]int64{
			"10": 700,
		},
	}

	stats.ReplaceAggregatedSnapshot(snapshot)
	first := stats.SnapshotAggregated()

	// Applying the same snapshot again must not duplicate totals.
	stats.ReplaceAggregatedSnapshot(snapshot)
	second := stats.SnapshotAggregated()

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("expected idempotent replace, got different snapshots")
	}
}
