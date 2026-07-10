package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneUsageJournal_RemovesFilesOutsideRetention(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	dir, err := usageJournalDir()
	if err != nil {
		t.Fatalf("usageJournalDir: %v", err)
	}
	oldPath := filepath.Join(dir, "2026-01-01.jsonl")
	keepPath := filepath.Join(dir, "2026-03-07.jsonl")
	if err := os.WriteFile(oldPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write old: %v", err)
	}
	if err := os.WriteFile(keepPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write keep: %v", err)
	}
	now := time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)
	if err := pruneUsageJournal(dir, now, 8); err != nil {
		t.Fatalf("pruneUsageJournal: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expected old journal file pruned, stat err=%v", err)
	}
	if _, err := os.Stat(keepPath); err != nil {
		t.Fatalf("expected recent journal file kept: %v", err)
	}
}

func TestGetUsageJournalStatus_ExposesTelemetry(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	ApplyUsageJournalConfig(true, 180, 8)
	now := time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC)
	dir, err := usageJournalDir()
	if err != nil {
		t.Fatalf("usageJournalDir: %v", err)
	}
	files := map[string]string{
		"2026-02-28.jsonl": "{\"minute\":\"2026-02-28T00:00:00Z\",\"requests\":1,\"success_count\":1,\"failure_count\":0,\"total_tokens\":10}\n",
		"2026-03-06.jsonl": "{\"minute\":\"2026-03-06T23:59:00Z\",\"requests\":1,\"success_count\":1,\"failure_count\":0,\"total_tokens\":10}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write journal %s: %v", name, err)
		}
	}
	recordUsageJournalReplay(now, "usage_journal")
	status := GetUsageJournalStatus(now)
	if !status.Enabled || !status.AppendOnly {
		t.Fatalf("expected enabled append-only journal status: %+v", status)
	}
	if status.RetentionDays != 180 || status.ReplayMaxDays != 8 {
		t.Fatalf("unexpected retention/replay status: %+v", status)
	}
	if status.Files != 2 || status.SizeBytes <= 0 {
		t.Fatalf("expected file telemetry, got %+v", status)
	}
	if status.CoverageStart == nil || status.CoverageEnd == nil {
		t.Fatalf("expected coverage telemetry, got %+v", status)
	}
	if status.BackfillStatus != "exact" {
		t.Fatalf("expected exact backfill status, got %+v", status)
	}
	if status.LastFinalizedMinute == nil || status.LastReplayAt == nil || status.LastReplaySource != "usage_journal" {
		t.Fatalf("expected replay telemetry, got %+v", status)
	}
}
