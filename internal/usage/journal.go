package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	usageJournalDirName              = "usage_journal"
	DefaultUsageJournalRetentionDays = 180
	DefaultUsageJournalReplayMaxDays = rollingRetentionDays
)

type UsageJournalStatus struct {
	Enabled             bool       `json:"enabled"`
	AppendOnly          bool       `json:"append_only"`
	RetentionDays       int        `json:"retention_days"`
	ReplayMaxDays       int        `json:"replay_max_days"`
	Files               int        `json:"files"`
	SizeBytes           int64      `json:"size_bytes"`
	CoverageStart       *time.Time `json:"coverage_start,omitempty"`
	CoverageEnd         *time.Time `json:"coverage_end,omitempty"`
	BackfillStatus      string     `json:"backfill_status"`
	LastFinalizedMinute *time.Time `json:"last_finalized_minute,omitempty"`
	LastReplayAt        *time.Time `json:"last_replay_at,omitempty"`
	LastReplaySource    string     `json:"last_replay_source,omitempty"`
}

type usageJournalRecord struct {
	Minute       string `json:"minute"`
	Requests     int64  `json:"requests"`
	SuccessCount int64  `json:"success_count"`
	FailureCount int64  `json:"failure_count"`
	TotalTokens  int64  `json:"total_tokens"`
	CountOnly    int64  `json:"count_only,omitempty"`
	Dropped      int64  `json:"dropped,omitempty"`
}

type usageJournalRuntimeSettings struct {
	enabled       bool
	retentionDays int
	replayMaxDays int
}

type usageJournalReplayMetadata struct {
	lastReplayAt     time.Time
	lastReplaySource string
}

type usageJournalTail struct {
	lastMinute   int64
	hasLast      bool
	validEnd     int64
	needsNewline bool
}

var (
	usageJournalSettingsMu sync.RWMutex
	usageJournalSettings   = sanitiseUsageJournalSettings(false, DefaultUsageJournalRetentionDays, DefaultUsageJournalReplayMaxDays)

	usageJournalReplayMu sync.RWMutex
	usageJournalReplay   usageJournalReplayMetadata
)

func ApplyUsageJournalConfig(enabled bool, retentionDays int, replayMaxDays int) {
	settings := sanitiseUsageJournalSettings(enabled, retentionDays, replayMaxDays)
	usageJournalSettingsMu.Lock()
	usageJournalSettings = settings
	usageJournalSettingsMu.Unlock()
}

func currentUsageJournalSettings() usageJournalRuntimeSettings {
	usageJournalSettingsMu.RLock()
	defer usageJournalSettingsMu.RUnlock()
	return usageJournalSettings
}

func sanitiseUsageJournalSettings(enabled bool, retentionDays int, replayMaxDays int) usageJournalRuntimeSettings {
	if retentionDays < rollingRetentionDays {
		retentionDays = DefaultUsageJournalRetentionDays
	}
	if replayMaxDays < rollingRetentionDays {
		replayMaxDays = DefaultUsageJournalReplayMaxDays
	}
	if replayMaxDays > retentionDays {
		replayMaxDays = retentionDays
	}
	return usageJournalRuntimeSettings{
		enabled:       enabled,
		retentionDays: retentionDays,
		replayMaxDays: replayMaxDays,
	}
}

func recordUsageJournalReplay(now time.Time, source string) {
	usageJournalReplayMu.Lock()
	defer usageJournalReplayMu.Unlock()
	usageJournalReplay.lastReplayAt = now.UTC()
	usageJournalReplay.lastReplaySource = strings.TrimSpace(source)
}

func currentUsageJournalReplayMetadata() usageJournalReplayMetadata {
	usageJournalReplayMu.RLock()
	defer usageJournalReplayMu.RUnlock()
	return usageJournalReplay
}

func usageJournalDir() (string, error) {
	base, err := ensureCliproxyDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, usageJournalDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("usage: create journal dir %s: %w", dir, err)
	}
	return dir, nil
}

func usageJournalFilePath(dayKey string) (string, error) {
	dir, err := usageJournalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, dayKey+".jsonl"), nil
}

func usageJournalRecordFromMinute(minute int64, bucket RollingMinuteBucket) usageJournalRecord {
	return usageJournalRecord{
		Minute:       formatRollingMinuteKey(minute),
		Requests:     bucket.Requests,
		SuccessCount: bucket.SuccessCount,
		FailureCount: bucket.FailureCount,
		TotalTokens:  bucket.TotalTokens,
		CountOnly:    bucket.CountOnly,
		Dropped:      bucket.Dropped,
	}
}

func writeUsageJournal(snapshot AggregatedStatisticsSnapshot, now time.Time) error {
	settings := currentUsageJournalSettings()
	if !settings.enabled {
		return nil
	}
	dir, err := usageJournalDir()
	if err != nil {
		return err
	}
	if err := pruneUsageJournal(dir, now, settings.retentionDays); err != nil {
		return err
	}
	_, _, minuteBuckets := restoreRollingState(snapshot.RollingState)
	if len(minuteBuckets) == 0 {
		return nil
	}
	currentMinute := now.UTC().Truncate(time.Minute).Unix() / 60
	byDay := make(map[string]map[int64]RollingMinuteBucket)
	for minute, bucket := range minuteBuckets {
		if minute >= currentMinute || bucket == (RollingMinuteBucket{}) {
			continue
		}
		dayKey := time.Unix(minute*60, 0).UTC().Format("2006-01-02")
		dayBuckets := byDay[dayKey]
		if dayBuckets == nil {
			dayBuckets = make(map[int64]RollingMinuteBucket)
			byDay[dayKey] = dayBuckets
		}
		dayBuckets[minute] = bucket
	}
	if len(byDay) == 0 {
		return nil
	}
	dayKeys := make([]string, 0, len(byDay))
	for dayKey := range byDay {
		dayKeys = append(dayKeys, dayKey)
	}
	sort.Strings(dayKeys)
	for _, dayKey := range dayKeys {
		path, err := usageJournalFilePath(dayKey)
		if err != nil {
			return err
		}
		if err := appendUsageJournalDay(path, byDay[dayKey]); err != nil {
			return err
		}
	}
	return nil
}

func appendUsageJournalDay(path string, dayBuckets map[int64]RollingMinuteBucket) error {
	if len(dayBuckets) == 0 {
		return nil
	}
	tail, err := readUsageJournalTail(path)
	if err != nil {
		return err
	}
	if tail.validEnd > 0 {
		if err := os.Truncate(path, tail.validEnd); err != nil {
			return fmt.Errorf("usage: truncate corrupt journal tail %s: %w", path, err)
		}
	}
	minutes := make([]int64, 0, len(dayBuckets))
	for minute := range dayBuckets {
		if tail.hasLast && minute <= tail.lastMinute {
			continue
		}
		minutes = append(minutes, minute)
	}
	if len(minutes) == 0 {
		return nil
	}
	sort.Slice(minutes, func(i, j int) bool { return minutes[i] < minutes[j] })
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("usage: open journal file %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	if err := file.Chmod(0o600); err != nil && !errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("usage: chmod journal file %s: %w", path, err)
	}
	if tail.needsNewline {
		if _, err := file.Write([]byte("\n")); err != nil {
			return fmt.Errorf("usage: write journal newline %s: %w", path, err)
		}
	}
	writer := bufio.NewWriter(file)
	for _, minute := range minutes {
		payload, err := json.Marshal(usageJournalRecordFromMinute(minute, dayBuckets[minute]))
		if err != nil {
			return fmt.Errorf("usage: encode journal minute %s %d: %w", path, minute, err)
		}
		if _, err := writer.Write(payload); err != nil {
			return fmt.Errorf("usage: append journal minute %s %d: %w", path, minute, err)
		}
		if err := writer.WriteByte('\n'); err != nil {
			return fmt.Errorf("usage: terminate journal minute %s %d: %w", path, minute, err)
		}
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("usage: flush journal writer %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("usage: sync journal file %s: %w", path, err)
	}
	return nil
}

func readUsageJournalTail(path string) (usageJournalTail, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return usageJournalTail{}, nil
		}
		return usageJournalTail{}, fmt.Errorf("usage: open journal file %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return usageJournalTail{}, fmt.Errorf("usage: stat journal file %s: %w", path, err)
	}
	if info.Size() == 0 {
		return usageJournalTail{}, nil
	}
	reader := bufio.NewReader(file)
	var tail usageJournalTail
	var offset int64
	var lastValidEnd int64
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return usageJournalTail{}, fmt.Errorf("usage: read journal file %s: %w", path, readErr)
		}
		lineEnd := offset + int64(len(line))
		trimmed := strings.TrimSpace(string(line))
		if trimmed != "" {
			minute, _, err := decodeUsageJournalRecordLine(trimmed)
			if err != nil {
				if errors.Is(readErr, io.EOF) {
					tail.validEnd = lastValidEnd
					return tail, nil
				}
				return usageJournalTail{}, fmt.Errorf("usage: decode journal file %s at offset %d: %w", path, offset, err)
			}
			tail.lastMinute = minute
			tail.hasLast = true
			lastValidEnd = lineEnd
			tail.validEnd = lineEnd
			tail.needsNewline = !bytes.HasSuffix(line, []byte{'\n'})
		}
		offset = lineEnd
		if errors.Is(readErr, io.EOF) {
			if len(line) == 0 {
				tail.validEnd = lastValidEnd
				tail.needsNewline = false
			}
			return tail, nil
		}
	}
}

func pruneUsageJournal(dir string, now time.Time, retentionDays int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("usage: read journal dir %s: %w", dir, err)
	}
	cutoff := now.UTC().AddDate(0, 0, -(retentionDays - 1)).Format("2006-01-02")
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		dayKey := strings.TrimSuffix(entry.Name(), ".jsonl")
		if strings.TrimSpace(dayKey) == "" || dayKey >= cutoff {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("usage: prune journal file %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func loadUsageJournalReplay(now time.Time) (RollingStateSnapshot, int, error) {
	settings := currentUsageJournalSettings()
	if !settings.enabled {
		return RollingStateSnapshot{}, 0, nil
	}
	dir, err := usageJournalDir()
	if err != nil {
		return RollingStateSnapshot{}, 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return RollingStateSnapshot{}, 0, nil
		}
		return RollingStateSnapshot{}, 0, fmt.Errorf("usage: read journal dir %s: %w", dir, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	currentMinute := now.UTC().Truncate(time.Minute).Unix() / 60
	minMinute := currentMinute - int64(settings.replayMaxDays*24*60)
	minuteBuckets := make(map[int64]RollingMinuteBucket)
	var loaded int
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		dayKey := strings.TrimSuffix(entry.Name(), ".jsonl")
		if strings.TrimSpace(dayKey) == "" {
			continue
		}
		dayStart, err := time.Parse("2006-01-02", dayKey)
		if err != nil {
			continue
		}
		dayEndMinute := dayStart.UTC().Add(24*time.Hour).Unix()/60 - 1
		if dayEndMinute < minMinute {
			continue
		}
		count, err := replayUsageJournalFile(filepath.Join(dir, entry.Name()), minMinute, currentMinute, minuteBuckets)
		if err != nil {
			return RollingStateSnapshot{}, 0, err
		}
		loaded += count
	}
	if len(minuteBuckets) == 0 {
		return RollingStateSnapshot{}, loaded, nil
	}
	var coverageStart time.Time
	var coverageEnd time.Time
	for minute := range minuteBuckets {
		candidate := time.Unix(minute*60, 0).UTC()
		if coverageStart.IsZero() || candidate.Before(coverageStart) {
			coverageStart = candidate
		}
		if coverageEnd.IsZero() || candidate.After(coverageEnd) {
			coverageEnd = candidate
		}
	}
	return snapshotRollingState(coverageStart, coverageEnd, minuteBuckets, now.UTC()), loaded, nil
}

func replayUsageJournalFile(path string, minMinute int64, currentMinute int64, minuteBuckets map[int64]RollingMinuteBucket) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("usage: open journal replay file %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	reader := bufio.NewReader(file)
	var loaded int
	var pendingErr error
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return 0, fmt.Errorf("usage: read journal replay file %s: %w", path, readErr)
		}
		trimmed := strings.TrimSpace(string(line))
		if trimmed != "" {
			minute, record, err := decodeUsageJournalRecordLine(trimmed)
			if err != nil {
				if pendingErr == nil {
					pendingErr = fmt.Errorf("usage: decode journal replay file %s: %w", path, err)
				}
			} else {
				if pendingErr != nil {
					return 0, pendingErr
				}
				if minute >= minMinute && minute < currentMinute {
					minuteBuckets[minute] = rollingMinuteBucketFromJournalRecord(record)
					loaded++
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			return loaded, nil
		}
	}
}

func decodeUsageJournalRecordLine(line string) (int64, usageJournalRecord, error) {
	var record usageJournalRecord
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		return 0, usageJournalRecord{}, err
	}
	minute, ok := parseRollingMinuteKey(record.Minute)
	if !ok {
		return 0, usageJournalRecord{}, fmt.Errorf("invalid minute %q", record.Minute)
	}
	return minute, record, nil
}

func rollingMinuteBucketFromJournalRecord(record usageJournalRecord) RollingMinuteBucket {
	return RollingMinuteBucket{
		Requests:     record.Requests,
		SuccessCount: record.SuccessCount,
		FailureCount: record.FailureCount,
		TotalTokens:  record.TotalTokens,
		CountOnly:    record.CountOnly,
		Dropped:      record.Dropped,
	}
}

func GetUsageJournalStatus(now time.Time) UsageJournalStatus {
	settings := currentUsageJournalSettings()
	status := UsageJournalStatus{
		Enabled:        settings.enabled,
		AppendOnly:     true,
		RetentionDays:  settings.retentionDays,
		ReplayMaxDays:  settings.replayMaxDays,
		BackfillStatus: "warming",
	}
	if !settings.enabled {
		status.BackfillStatus = "disabled"
	}
	if lastFinalized := usageJournalLastFinalizedMinute(now); !lastFinalized.IsZero() {
		status.LastFinalizedMinute = &lastFinalized
	}
	replayMeta := currentUsageJournalReplayMetadata()
	if !replayMeta.lastReplayAt.IsZero() {
		ts := replayMeta.lastReplayAt.UTC()
		status.LastReplayAt = &ts
	}
	status.LastReplaySource = replayMeta.lastReplaySource
	dir, err := usageJournalDir()
	if err != nil {
		return status
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return status
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		files = append(files, filepath.Join(dir, entry.Name()))
		if info, err := entry.Info(); err == nil {
			status.SizeBytes += info.Size()
		}
	}
	if len(files) == 0 {
		return status
	}
	sort.Strings(files)
	status.Files = len(files)
	if ts, ok := usageJournalCoverageBoundary(files[0], true); ok {
		status.CoverageStart = &ts
	}
	if ts, ok := usageJournalCoverageBoundary(files[len(files)-1], false); ok {
		status.CoverageEnd = &ts
	}
	if status.Enabled && usageJournalHasExactBackfill(status, now.UTC()) {
		status.BackfillStatus = "exact"
	}
	return status
}

func usageJournalHasExactBackfill(status UsageJournalStatus, now time.Time) bool {
	if status.CoverageStart == nil || status.CoverageEnd == nil {
		return false
	}
	lastFinalized := usageJournalLastFinalizedMinute(now)
	if lastFinalized.IsZero() {
		return false
	}
	requiredStart := lastFinalized.Add(-(7 * 24 * time.Hour) + time.Minute)
	if status.CoverageStart.After(requiredStart) {
		return false
	}
	if status.CoverageEnd.Before(lastFinalized) {
		return false
	}
	return true
}

func usageJournalCoverageBoundary(path string, first bool) (time.Time, bool) {
	file, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = file.Close() }()
	reader := bufio.NewReader(file)
	var lastValid time.Time
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return time.Time{}, false
		}
		trimmed := strings.TrimSpace(string(line))
		if trimmed != "" {
			minute, _, err := decodeUsageJournalRecordLine(trimmed)
			if err == nil {
				ts := time.Unix(minute*60, 0).UTC()
				if first {
					return ts, true
				}
				lastValid = ts
			}
		}
		if errors.Is(readErr, io.EOF) {
			if lastValid.IsZero() {
				return time.Time{}, false
			}
			return lastValid, true
		}
	}
}

func usageJournalLastFinalizedMinute(now time.Time) time.Time {
	now = now.UTC().Truncate(time.Minute)
	if now.IsZero() {
		return time.Time{}
	}
	return now.Add(-time.Minute)
}
