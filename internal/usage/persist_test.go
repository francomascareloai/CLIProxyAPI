package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	if snapshot2.TotalRequests != snapshot.TotalRequests || snapshot2.SuccessCount != snapshot.SuccessCount || snapshot2.FailureCount != snapshot.FailureCount || snapshot2.TotalTokens != snapshot.TotalTokens {
		t.Fatalf("expected aggregated totals to survive round-trip: before=%+v after=%+v", snapshot, snapshot2)
	}
	if !reflect.DeepEqual(snapshot.RequestsByDay, snapshot2.RequestsByDay) || !reflect.DeepEqual(snapshot.RequestsByHour, snapshot2.RequestsByHour) || !reflect.DeepEqual(snapshot.TokensByDay, snapshot2.TokensByDay) || !reflect.DeepEqual(snapshot.TokensByHour, snapshot2.TokensByHour) {
		t.Fatalf("expected day/hour aggregates to survive round-trip")
	}
}

func TestReplaceAggregatedSnapshot_IsIdempotent(t *testing.T) {
	stats := NewRequestStatistics()
	snapshot := AggregatedStatisticsSnapshot{
		TotalRequests: 42,
		SuccessCount:  40,
		FailureCount:  2,
		TotalTokens:   4200,
		RollingState: RollingStateSnapshot{
			CoverageStart: time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC),
			CoverageEnd:   time.Date(2026, 2, 20, 10, 59, 0, 0, time.UTC),
			MinuteBuckets: map[string]RollingMinuteBucket{
				"2026-02-20T10:00:00Z": {Requests: 1, SuccessCount: 1, TotalTokens: 100},
				"2026-02-20T10:01:00Z": {Requests: 1, FailureCount: 1, TotalTokens: 50, CountOnly: 1},
			},
		},
		APIs: map[string]AggregatedAPISnapshot{
			"api:hmac256:test": {
				TotalRequests:   42,
				SuccessCount:    40,
				FailureCount:    2,
				TotalTokens:     4200,
				InputTokens:     1000,
				OutputTokens:    2000,
				ReasoningTokens: 800,
				CachedTokens:    400,
				LastUsed:        time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC),
				Models: map[string]AggregatedModelSnapshot{
					"gemini-3-pro-preview": {
						TotalRequests:   42,
						SuccessCount:    40,
						FailureCount:    2,
						TotalTokens:     4200,
						InputTokens:     1000,
						OutputTokens:    2000,
						ReasoningTokens: 800,
						CachedTokens:    400,
						LastUsed:        time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC),
					},
				},
			},
		},
		Breakdowns: UsageBreakdownsSnapshot{
			BySource: []usageBreakdownBucket{{
				Source:          "user@example.com",
				TotalRequests:   42,
				SuccessCount:    40,
				FailureCount:    2,
				TotalTokens:     4200,
				InputTokens:     1000,
				OutputTokens:    2000,
				ReasoningTokens: 800,
				CachedTokens:    400,
				LastUsed:        time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC),
			}},
			ByAuthIndex: []usageBreakdownBucket{{
				AuthIndex:       "auth-1",
				TotalRequests:   42,
				SuccessCount:    40,
				FailureCount:    2,
				TotalTokens:     4200,
				InputTokens:     1000,
				OutputTokens:    2000,
				ReasoningTokens: 800,
				CachedTokens:    400,
				LastUsed:        time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC),
			}},
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

func TestLoadIntoStore_MigratesLegacyUsageFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	resetUsageHMACKeyForTest()
	legacyDir := filepath.Join(tmp, ".cli-proxy-api")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("mkdir legacy dir: %v", err)
	}
	legacyPath := filepath.Join(legacyDir, persistenceFilename)
	payload := persistedData{
		Version:       persistenceVersion,
		SavedAt:       time.Now().UTC(),
		TotalRequests: 2,
		SuccessCount:  1,
		FailureCount:  1,
		TotalTokens:   30,
		APIs: map[string]*persistedAPI{
			"claude": {
				TotalRequests: 2,
				TotalTokens:   30,
				Models: map[string]*persistedModel{
					"claude-3-5-sonnet": {
						TotalRequests: 2,
						TotalTokens:   30,
						Details: []RequestDetail{
							{Timestamp: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC), Source: "legacy@example.com", AuthIndex: "legacy-1", Tokens: TokenStats{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}},
							{Timestamp: time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC), Source: "legacy@example.com", AuthIndex: "legacy-1", Failed: true, Tokens: TokenStats{InputTokens: 5, OutputTokens: 10, TotalTokens: 15}},
						},
					},
				},
			},
		},
		RequestsByDay:  map[string]int64{"2026-03-01": 2},
		RequestsByHour: map[int]int64{10: 1, 11: 1},
		TokensByDay:    map[string]int64{"2026-03-01": 30},
		TokensByHour:   map[int]int64{10: 15, 11: 15},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal legacy payload: %v", err)
	}
	if err := os.WriteFile(legacyPath, data, 0o644); err != nil {
		t.Fatalf("write legacy payload: %v", err)
	}
	stats := NewRequestStatistics()
	p := &UsagePersister{stats: stats}
	if err := p.loadIntoStore(); err != nil {
		t.Fatalf("loadIntoStore: %v", err)
	}
	canonicalPath, err := usageStatsPath()
	if err != nil {
		t.Fatalf("usageStatsPath: %v", err)
	}
	if _, err := os.Stat(canonicalPath); err != nil {
		t.Fatalf("expected canonical usage file: %v", err)
	}
	snapshot := stats.SnapshotAggregated()
	if snapshot.TotalRequests != 2 || snapshot.TotalTokens != 30 {
		t.Fatalf("unexpected migrated totals: %+v", snapshot)
	}
	if len(snapshot.Breakdowns.BySource) != 1 || snapshot.Breakdowns.BySource[0].Source != "legacy@example.com" {
		t.Fatalf("expected migrated source breakdown: %+v", snapshot.Breakdowns.BySource)
	}
	if len(snapshot.Breakdowns.ByAuthIndex) != 1 || snapshot.Breakdowns.ByAuthIndex[0].AuthIndex != "legacy-1" {
		t.Fatalf("expected migrated auth breakdown: %+v", snapshot.Breakdowns.ByAuthIndex)
	}
}

func TestCompactOldDetails_PreservesDurableBreakdowns(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{Provider: "claude", Model: "claude-3-5-sonnet", APIKey: "sk-test-preserve", Source: "persist@example.com", AuthIndex: "auth-preserve", RequestedAt: time.Now().AddDate(0, 0, -detailRetentionDays-2), Detail: coreusage.Detail{InputTokens: 10, OutputTokens: 15, TotalTokens: 25}})
	before := stats.SnapshotUsageBreakdowns()
	removed := stats.CompactOldDetails()
	after := stats.SnapshotUsageBreakdowns()
	if removed != 1 {
		t.Fatalf("expected one removed detail, got %d", removed)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("durable breakdowns changed after compaction")
	}
}

func TestSanitiseAggregatedSnapshot_PrunesOldRollingBuckets(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	oldMinute := now.Add(-(rollingRetentionDays + 1) * 24 * time.Hour)
	snapshot := AggregatedStatisticsSnapshot{
		RollingState: RollingStateSnapshot{
			CoverageStart: oldMinute,
			CoverageEnd:   now,
			MinuteBuckets: map[string]RollingMinuteBucket{
				oldMinute.Format(time.RFC3339): {Requests: 1, TotalTokens: 10},
				now.Format(time.RFC3339):       {Requests: 2, TotalTokens: 20},
			},
		},
	}
	sanitised := sanitiseAggregatedSnapshot(snapshot)
	if len(sanitised.RollingState.MinuteBuckets) != 1 {
		t.Fatalf("expected pruned rolling buckets, got %+v", sanitised.RollingState.MinuteBuckets)
	}
	if _, ok := sanitised.RollingState.MinuteBuckets[now.Format(time.RFC3339)]; !ok {
		t.Fatalf("expected recent rolling bucket to remain")
	}
}

func TestFlushUsageStatsNow_WritesUsageJournal(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	resetUsageHMACKeyForTest()
	ApplyUsageJournalConfig(true, 180, 8)
	stats := NewRequestStatistics()
	ts := time.Date(2026, 3, 7, 12, 34, 0, 0, time.UTC)
	stats.Record(context.Background(), coreusage.Record{
		Provider:    "claude",
		Model:       "claude-3-5-sonnet",
		APIKey:      "sk-test-journal",
		RequestedAt: ts,
		Detail:      coreusage.Detail{InputTokens: 10, OutputTokens: 20, TotalTokens: 30},
	})
	if err := FlushUsageStatsNow(stats); err != nil {
		t.Fatalf("flush: %v", err)
	}
	journalPath := filepath.Join(tmp, cliproxyDirName, usageJournalDirName, "2026-03-07.jsonl")
	data, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if !bytes.Contains(data, []byte("2026-03-07T12:34:00Z")) {
		t.Fatalf("expected minute-level journal entry, got %s", string(data))
	}
}

func TestFlushUsageStatsNow_AppendOnlyDoesNotDuplicateMinutes(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	resetUsageHMACKeyForTest()
	ApplyUsageJournalConfig(true, 180, 8)
	stats := NewRequestStatistics()
	ts := time.Date(2026, 3, 7, 12, 34, 0, 0, time.UTC)
	stats.Record(context.Background(), coreusage.Record{
		Provider:    "claude",
		Model:       "claude-3-5-sonnet",
		APIKey:      "sk-test-journal-append",
		RequestedAt: ts,
		Detail:      coreusage.Detail{InputTokens: 10, OutputTokens: 20, TotalTokens: 30},
	})
	if err := FlushUsageStatsNow(stats); err != nil {
		t.Fatalf("first flush: %v", err)
	}
	if err := FlushUsageStatsNow(stats); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	journalPath := filepath.Join(tmp, cliproxyDirName, usageJournalDirName, "2026-03-07.jsonl")
	data, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if got := strings.Count(string(data), "2026-03-07T12:34:00Z"); got != 1 {
		t.Fatalf("expected single journal minute after repeated flushes, got %d\n%s", got, string(data))
	}
}

func TestLoadIntoStore_ReplaysJournalIntoRollingOnly(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	resetUsageHMACKeyForTest()
	ApplyUsageJournalConfig(true, 180, 8)
	now := time.Now().UTC().Truncate(time.Minute)
	journalNow := now.Add(time.Minute)
	start := now.Add(-(7 * 24) * time.Hour)
	minuteBuckets := make(map[string]RollingMinuteBucket)
	for ts := start; !ts.After(now); ts = ts.Add(time.Minute) {
		minuteBuckets[ts.Format(time.RFC3339)] = RollingMinuteBucket{Requests: 1, SuccessCount: 1, TotalTokens: 10}
	}
	seed := AggregatedStatisticsSnapshot{
		TotalRequests: 5,
		SuccessCount:  5,
		TotalTokens:   50,
		RequestsByDay: map[string]int64{"2026-03-01": 5},
		TokensByDay:   map[string]int64{"2026-03-01": 50},
	}
	journalSnapshot := AggregatedStatisticsSnapshot{RollingState: RollingStateSnapshot{CoverageStart: start, CoverageEnd: now, MinuteBuckets: minuteBuckets}}
	if err := writeUsageJournal(journalSnapshot, journalNow); err != nil {
		t.Fatalf("write journal: %v", err)
	}
	payload := persistedUsageStats{Version: usagePersistenceVersion, ExportedAt: now, Usage: seed}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	statsPath, err := usageStatsPath()
	if err != nil {
		t.Fatalf("usageStatsPath: %v", err)
	}
	if err := writeAtomicFile(statsPath, data, 0o600); err != nil {
		t.Fatalf("write canonical snapshot: %v", err)
	}
	loaded := NewRequestStatistics()
	loader := &UsagePersister{stats: loaded}
	if err := loader.loadIntoStore(); err != nil {
		t.Fatalf("loadIntoStore: %v", err)
	}
	snapshot := loaded.Snapshot()
	if snapshot.TotalRequests != 5 || snapshot.TotalTokens != 50 {
		t.Fatalf("expected long aggregates from snapshot only, got %+v", snapshot)
	}
	if !snapshot.Rolling.Windows.Window24H.Available || snapshot.Rolling.Windows.Window24H.Requests != 24*60 {
		t.Fatalf("expected rolling window rebuilt from journal, got %+v", snapshot.Rolling.Windows.Window24H)
	}
	if snapshot.RequestsByDay["2026-03-01"] != 5 || snapshot.TotalRequests != 5 {
		t.Fatalf("expected no long-aggregate double count after replay, got %+v", snapshot)
	}
	if !snapshot.Rolling.Windows.Window7D.Available || snapshot.Rolling.Windows.Window7D.Requests != 7*24*60 {
		t.Fatalf("expected 7d rolling rebuilt from journal, got %+v", snapshot.Rolling.Windows.Window7D)
	}
}

func TestLoadIntoStore_IgnoresTrailingCorruptJournalLine(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	resetUsageHMACKeyForTest()
	ApplyUsageJournalConfig(true, 180, 8)
	now := time.Now().UTC().Truncate(time.Minute)
	minute := now.Add(-2 * time.Minute)
	journalPath := filepath.Join(tmp, cliproxyDirName, usageJournalDirName, minute.Format("2006-01-02")+".jsonl")
	if err := os.MkdirAll(filepath.Dir(journalPath), 0o700); err != nil {
		t.Fatalf("mkdir journal: %v", err)
	}
	validLine := `{"minute":"` + minute.Format(time.RFC3339) + `","requests":1,"success_count":1,"failure_count":0,"total_tokens":10}` + "\n"
	if err := os.WriteFile(journalPath, []byte(validLine+`{"minute":"broken`), 0o600); err != nil {
		t.Fatalf("write journal: %v", err)
	}
	payload := persistedUsageStats{Version: usagePersistenceVersion, ExportedAt: now, Usage: AggregatedStatisticsSnapshot{}}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	statsPath, err := usageStatsPath()
	if err != nil {
		t.Fatalf("usageStatsPath: %v", err)
	}
	if err := writeAtomicFile(statsPath, data, 0o600); err != nil {
		t.Fatalf("write canonical snapshot: %v", err)
	}
	loaded := NewRequestStatistics()
	loader := &UsagePersister{stats: loaded}
	if err := loader.loadIntoStore(); err != nil {
		t.Fatalf("loadIntoStore should ignore trailing corrupt line: %v", err)
	}
	snapshot := loaded.Snapshot()
	if snapshot.Rolling.CoverageStart == nil || !snapshot.Rolling.CoverageStart.Equal(minute) {
		t.Fatalf("expected valid journal minute to survive replay, got %+v", snapshot.Rolling)
	}
}

func TestRollingSnapshot_ComputesWindowCountsFromImportedMinuteState(t *testing.T) {
	stats := NewRequestStatistics()
	now := time.Now().UTC().Truncate(time.Minute)
	start := now.Add(-(7*24)*time.Hour + time.Minute)
	minuteBuckets := make(map[string]RollingMinuteBucket)
	for ts := start; !ts.After(now); ts = ts.Add(time.Minute) {
		minuteBuckets[ts.Format(time.RFC3339)] = RollingMinuteBucket{
			Requests:     1,
			SuccessCount: 1,
			TotalTokens:  10,
		}
	}
	stats.ReplaceAggregatedSnapshot(AggregatedStatisticsSnapshot{
		TotalRequests: int64(len(minuteBuckets)),
		SuccessCount:  int64(len(minuteBuckets)),
		TotalTokens:   int64(len(minuteBuckets) * 10),
		RollingState: RollingStateSnapshot{
			CoverageStart: start,
			CoverageEnd:   now,
			MinuteBuckets: minuteBuckets,
		},
	})
	snapshot := stats.Snapshot()
	if !snapshot.Rolling.Windows.Window7H.Available || snapshot.Rolling.Windows.Window7H.Requests != 7*60 {
		t.Fatalf("unexpected 7h rolling window: %+v", snapshot.Rolling.Windows.Window7H)
	}
	if !snapshot.Rolling.Windows.Window24H.Available || snapshot.Rolling.Windows.Window24H.Requests != 24*60 {
		t.Fatalf("unexpected 24h rolling window: %+v", snapshot.Rolling.Windows.Window24H)
	}
	if !snapshot.Rolling.Windows.Window7D.Available || snapshot.Rolling.Windows.Window7D.Requests != 7*24*60 {
		t.Fatalf("unexpected 7d rolling window: %+v", snapshot.Rolling.Windows.Window7D)
	}
}
