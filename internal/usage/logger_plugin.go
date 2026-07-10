// Package usage provides usage tracking and logging functionality for the CLI Proxy API server.
// It includes plugins for monitoring API usage, token consumption, and other metrics
// to help with observability and billing purposes.
package usage

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

const (
	UsageSchemaVersion                = 2
	UsageAggregatesV2Capability       = "usage_aggregates_v2"
	RollingWindowsV1Capability        = "rolling_windows_v1"
	rollingResolutionMinutes          = 1
	rollingRetentionDays              = 8
	rollingRetentionMinutes     int64 = rollingRetentionDays * 24 * 60
)

var statisticsEnabled atomic.Bool

func init() {
	statisticsEnabled.Store(true)
	coreusage.RegisterPlugin(NewLoggerPlugin())
}

// LoggerPlugin collects in-memory request statistics for usage analysis.
// It implements coreusage.Plugin to receive usage records emitted by the runtime.
type LoggerPlugin struct {
	stats *RequestStatistics
}

// NewLoggerPlugin constructs a new logger plugin instance.
//
// Returns:
//   - *LoggerPlugin: A new logger plugin instance wired to the shared statistics store.
func NewLoggerPlugin() *LoggerPlugin { return &LoggerPlugin{stats: defaultRequestStatistics} }

// HandleUsage implements coreusage.Plugin.
// It updates the in-memory statistics store whenever a usage record is received.
//
// Parameters:
//   - ctx: The context for the usage record
//   - record: The usage record to aggregate
func (p *LoggerPlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if !statisticsEnabled.Load() {
		return
	}
	if p == nil || p.stats == nil {
		return
	}
	p.stats.Record(ctx, record)
}

// SetStatisticsEnabled toggles whether in-memory statistics are recorded.
func SetStatisticsEnabled(enabled bool) { statisticsEnabled.Store(enabled) }

// StatisticsEnabled reports the current recording state.
func StatisticsEnabled() bool { return statisticsEnabled.Load() }

func ConfigureUsageRuntime(enabled bool, retentionDays int, replayMaxDays int) {
	SetStatisticsEnabled(enabled)
	ApplyUsageJournalConfig(enabled, retentionDays, replayMaxDays)
}

// Shutdown gracefully stops the usage statistics auto-save and performs a final save.
// This should be called when the service is shutting down.
func Shutdown() {
	defaultRequestStatistics.StopAutoSave()
	StopUsagePersister(context.Background())
}

// RequestStatistics maintains aggregated request metrics in memory.
type RequestStatistics struct {
	mu    sync.RWMutex
	dirty atomic.Bool

	totalRequests int64
	successCount  int64
	failureCount  int64
	totalTokens   int64

	apis map[string]*apiStats

	usageBreakdowns usageBreakdownBuckets

	requestsByDay  map[string]int64
	requestsByHour map[int]int64
	tokensByDay    map[string]int64
	tokensByHour   map[int]int64

	rollingCoverageStart time.Time
	rollingCoverageEnd   time.Time
	rollingMinuteBuckets map[int64]RollingMinuteBucket
}

type usageBreakdownBucket struct {
	Key             string    `json:"key,omitempty"`
	Source          string    `json:"source,omitempty"`
	AuthIndex       string    `json:"auth_index,omitempty"`
	TotalRequests   int64     `json:"total_requests"`
	SuccessCount    int64     `json:"success_count"`
	FailureCount    int64     `json:"failure_count"`
	TotalTokens     int64     `json:"total_tokens"`
	InputTokens     int64     `json:"input_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	ReasoningTokens int64     `json:"reasoning_tokens"`
	CachedTokens    int64     `json:"cached_tokens"`
	LastUsed        time.Time `json:"last_used,omitempty"`
}

type usageBreakdownBuckets struct {
	BySource    map[string]*usageBreakdownBucket
	ByAuthIndex map[string]*usageBreakdownBucket
}

type UsageBreakdownsSnapshot struct {
	BySource    []usageBreakdownBucket `json:"by_source,omitempty"`
	ByAuthIndex []usageBreakdownBucket `json:"by_auth_index,omitempty"`
}

type RollingMinuteBucket struct {
	Requests     int64 `json:"requests"`
	SuccessCount int64 `json:"success_count"`
	FailureCount int64 `json:"failure_count"`
	TotalTokens  int64 `json:"total_tokens"`
	CountOnly    int64 `json:"count_only,omitempty"`
	Dropped      int64 `json:"dropped,omitempty"`
}

type RollingWindowSummary struct {
	Requests       int64  `json:"requests"`
	SuccessCount   int64  `json:"success_count"`
	FailureCount   int64  `json:"failure_count"`
	TotalTokens    int64  `json:"total_tokens"`
	CountOnly      int64  `json:"count_only"`
	Dropped        int64  `json:"dropped"`
	Available      bool   `json:"available"`
	Integrity      string `json:"integrity"`
	DegradedReason string `json:"degraded_reason,omitempty"`
}

type RollingWindows struct {
	Window7H  RollingWindowSummary `json:"7h"`
	Window24H RollingWindowSummary `json:"24h"`
	Window7D  RollingWindowSummary `json:"7d"`
}

type RollingSnapshot struct {
	Available         bool           `json:"available"`
	ResolutionMinutes int            `json:"resolution_minutes"`
	CoverageStart     *time.Time     `json:"coverage_start,omitempty"`
	CoverageEnd       *time.Time     `json:"coverage_end,omitempty"`
	Integrity         string         `json:"integrity"`
	DegradedReason    string         `json:"degraded_reason,omitempty"`
	Windows           RollingWindows `json:"windows"`
}

type RollingStateSnapshot struct {
	CoverageStart time.Time                      `json:"coverage_start,omitempty"`
	CoverageEnd   time.Time                      `json:"coverage_end,omitempty"`
	MinuteBuckets map[string]RollingMinuteBucket `json:"minute_buckets,omitempty"`
}

// apiStats holds aggregated metrics for a single API key.
type apiStats struct {
	TotalRequests   int64
	SuccessCount    int64
	FailureCount    int64
	TotalTokens     int64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	CachedTokens    int64
	LastUsed        time.Time
	Models          map[string]*modelStats
}

// maxRequestDetailsPerModel bounds in-memory per-request detail retention per model.
//
// Rationale:
// - Prevent accidental / malicious memory growth (DoS) when the usage endpoint is enabled.
// - Keep endpoint payload shape compatible: Snapshot() still returns `details`, but truncated.
// - Keep Record() fast: O(1) per record after the slice reaches the cap.
const maxRequestDetailsPerModel = 256

func MaxRequestDetailsPerModel() int {
	return maxRequestDetailsPerModel
}

// modelStats holds aggregated metrics for a specific model within an API.
type modelStats struct {
	TotalRequests   int64
	SuccessCount    int64
	FailureCount    int64
	TotalTokens     int64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	CachedTokens    int64
	LastUsed        time.Time

	// details is a fixed-size ring buffer (once full) holding the most recent request details.
	// The ring order is tracked via detailsNext.
	details     []RequestDetail
	detailsNext int
}

func newModelStats() *modelStats {
	return &modelStats{}
}

func (s *RequestStatistics) ensureBreakdownMaps() {
	if s.usageBreakdowns.BySource == nil {
		s.usageBreakdowns.BySource = make(map[string]*usageBreakdownBucket)
	}
	if s.usageBreakdowns.ByAuthIndex == nil {
		s.usageBreakdowns.ByAuthIndex = make(map[string]*usageBreakdownBucket)
	}
}

func newUsageBreakdownBucket(key string, detail RequestDetail, source bool) *usageBreakdownBucket {
	bucket := &usageBreakdownBucket{Key: key}
	if source {
		bucket.Source = key
	} else {
		bucket.AuthIndex = key
	}
	bucket.update(detail)
	return bucket
}

func (b *usageBreakdownBucket) update(detail RequestDetail) {
	if b == nil {
		return
	}
	detail.Tokens = normaliseTokenStats(detail.Tokens)
	b.TotalRequests++
	if detail.Failed {
		b.FailureCount++
	} else {
		b.SuccessCount++
	}
	b.TotalTokens += detail.Tokens.TotalTokens
	b.InputTokens += detail.Tokens.InputTokens
	b.OutputTokens += detail.Tokens.OutputTokens
	b.ReasoningTokens += detail.Tokens.ReasoningTokens
	b.CachedTokens += detail.Tokens.CachedTokens
	if detail.Timestamp.After(b.LastUsed) {
		b.LastUsed = detail.Timestamp
	}
}

func (s *RequestStatistics) updateUsageBreakdowns(detail RequestDetail) {
	s.ensureBreakdownMaps()
	source := strings.TrimSpace(detail.Source)
	if source == "" {
		source = "unknown"
	}
	authIndex := strings.TrimSpace(detail.AuthIndex)
	if authIndex == "" {
		authIndex = "unknown"
	}
	bucket, ok := s.usageBreakdowns.BySource[source]
	if !ok || bucket == nil {
		bucket = newUsageBreakdownBucket(source, detail, true)
		s.usageBreakdowns.BySource[source] = bucket
	} else {
		bucket.update(detail)
	}
	bucket, ok = s.usageBreakdowns.ByAuthIndex[authIndex]
	if !ok || bucket == nil {
		bucket = newUsageBreakdownBucket(authIndex, detail, false)
		s.usageBreakdowns.ByAuthIndex[authIndex] = bucket
	} else {
		bucket.update(detail)
	}
}

func cloneUsageBreakdownBuckets(in map[string]*usageBreakdownBucket, source bool) []usageBreakdownBucket {
	if len(in) == 0 {
		return nil
	}
	out := make([]usageBreakdownBucket, 0, len(in))
	for key, bucket := range in {
		if bucket == nil {
			continue
		}
		cloned := *bucket
		cloned.Key = ""
		if source {
			cloned.Source = key
			cloned.AuthIndex = ""
		} else {
			cloned.AuthIndex = key
			cloned.Source = ""
		}
		out = append(out, cloned)
	}
	sortUsageBreakdownSnapshot(out, source)
	return out
}

func sortUsageBreakdownSnapshot(items []usageBreakdownBucket, source bool) {
	if len(items) < 2 {
		return
	}
	sort.Slice(items, func(i, j int) bool {
		left := items[i]
		right := items[j]
		if right.TotalTokens != left.TotalTokens {
			return right.TotalTokens < left.TotalTokens
		}
		return usageBreakdownName(left, source) < usageBreakdownName(right, source)
	})
}

func usageBreakdownName(item usageBreakdownBucket, source bool) string {
	if source {
		return item.Source
	}
	return item.AuthIndex
}

func boolToCount(v bool) int64 {
	if v {
		return 1
	}
	return 0
}

func cloneRollingMinuteBuckets(in map[int64]RollingMinuteBucket) map[int64]RollingMinuteBucket {
	if len(in) == 0 {
		return nil
	}
	out := make(map[int64]RollingMinuteBucket, len(in))
	for minute, bucket := range in {
		out[minute] = bucket
	}
	return out
}

func mergeRollingMinuteBucket(dst RollingMinuteBucket, delta RollingMinuteBucket) RollingMinuteBucket {
	dst.Requests += delta.Requests
	dst.SuccessCount += delta.SuccessCount
	dst.FailureCount += delta.FailureCount
	dst.TotalTokens += delta.TotalTokens
	dst.CountOnly += delta.CountOnly
	dst.Dropped += delta.Dropped
	return dst
}

func (s *RequestStatistics) ensureRollingBucketsLocked() {
	if s.rollingMinuteBuckets == nil {
		s.rollingMinuteBuckets = make(map[int64]RollingMinuteBucket)
	}
}

func (s *RequestStatistics) recordRollingMinuteLocked(minute int64, delta RollingMinuteBucket) {
	if s == nil {
		return
	}
	s.ensureRollingBucketsLocked()
	s.rollingMinuteBuckets[minute] = mergeRollingMinuteBucket(s.rollingMinuteBuckets[minute], delta)
	candidateTime := time.Unix(minute*60, 0).UTC()
	if s.rollingCoverageStart.IsZero() || candidateTime.Before(s.rollingCoverageStart) {
		s.rollingCoverageStart = candidateTime
	}
	if s.rollingCoverageEnd.IsZero() || candidateTime.After(s.rollingCoverageEnd) {
		s.rollingCoverageEnd = candidateTime
	}
}

func pruneRollingMinuteBuckets(minuteBuckets map[int64]RollingMinuteBucket, coverageStart time.Time, now time.Time) (map[int64]RollingMinuteBucket, time.Time) {
	if len(minuteBuckets) == 0 {
		return nil, time.Time{}
	}
	nowMinute := now.UTC().Truncate(time.Minute).Unix() / 60
	minMinute := nowMinute - rollingRetentionMinutes
	pruned := make(map[int64]RollingMinuteBucket, len(minuteBuckets))
	var earliest int64
	for minute, bucket := range minuteBuckets {
		if minute < minMinute {
			continue
		}
		if bucket == (RollingMinuteBucket{}) {
			continue
		}
		pruned[minute] = bucket
		if earliest == 0 || minute < earliest {
			earliest = minute
		}
	}
	if len(pruned) == 0 {
		return nil, time.Time{}
	}
	if coverageStart.IsZero() || coverageStart.UTC().Unix()/60 > earliest {
		coverageStart = time.Unix(earliest*60, 0).UTC()
	}
	return pruned, coverageStart.UTC()
}

func formatRollingMinuteKey(minute int64) string {
	return time.Unix(minute*60, 0).UTC().Format(time.RFC3339)
}

func parseRollingMinuteKey(key string) (int64, bool) {
	timestamp, err := time.Parse(time.RFC3339, strings.TrimSpace(key))
	if err != nil {
		return 0, false
	}
	return timestamp.UTC().Unix() / 60, true
}

func cloneRollingStateSnapshot(snapshot RollingStateSnapshot) RollingStateSnapshot {
	result := RollingStateSnapshot{
		CoverageStart: snapshot.CoverageStart.UTC(),
		CoverageEnd:   snapshot.CoverageEnd.UTC(),
	}
	if len(snapshot.MinuteBuckets) > 0 {
		result.MinuteBuckets = make(map[string]RollingMinuteBucket, len(snapshot.MinuteBuckets))
		for minute, bucket := range snapshot.MinuteBuckets {
			result.MinuteBuckets[minute] = bucket
		}
	}
	return result
}

func snapshotRollingState(coverageStart time.Time, coverageEnd time.Time, minuteBuckets map[int64]RollingMinuteBucket, now time.Time) RollingStateSnapshot {
	prunedBuckets, prunedCoverageStart := pruneRollingMinuteBuckets(minuteBuckets, coverageStart, now)
	result := RollingStateSnapshot{}
	if !prunedCoverageStart.IsZero() {
		result.CoverageStart = prunedCoverageStart.UTC()
	}
	if !coverageEnd.IsZero() {
		result.CoverageEnd = coverageEnd.UTC()
	}
	if len(prunedBuckets) > 0 {
		result.MinuteBuckets = make(map[string]RollingMinuteBucket, len(prunedBuckets))
		for minute, bucket := range prunedBuckets {
			result.MinuteBuckets[formatRollingMinuteKey(minute)] = bucket
		}
	}
	return result
}

func restoreRollingState(snapshot RollingStateSnapshot) (time.Time, time.Time, map[int64]RollingMinuteBucket) {
	if len(snapshot.MinuteBuckets) == 0 {
		return time.Time{}, time.Time{}, nil
	}
	minuteBuckets := make(map[int64]RollingMinuteBucket, len(snapshot.MinuteBuckets))
	var earliest int64
	var latest int64
	for key, bucket := range snapshot.MinuteBuckets {
		minute, ok := parseRollingMinuteKey(key)
		if !ok {
			continue
		}
		minuteBuckets[minute] = bucket
		if earliest == 0 || minute < earliest {
			earliest = minute
		}
		if latest == 0 || minute > latest {
			latest = minute
		}
	}
	if len(minuteBuckets) == 0 {
		return time.Time{}, time.Time{}, nil
	}
	coverageStart := snapshot.CoverageStart.UTC()
	if coverageStart.IsZero() {
		coverageStart = time.Unix(earliest*60, 0).UTC()
	}
	coverageEnd := snapshot.CoverageEnd.UTC()
	if coverageEnd.IsZero() {
		coverageEnd = time.Unix(latest*60, 0).UTC()
	}
	return coverageStart, coverageEnd, minuteBuckets
}

func buildRollingWindowSummary(minuteBuckets map[int64]RollingMinuteBucket, startMinute int64, durationMinutes int64, coverageStart time.Time, coverageEnd time.Time, dropped coreusage.DroppedRecordsSnapshot) RollingWindowSummary {
	summary := RollingWindowSummary{}
	if durationMinutes <= 0 {
		summary.Integrity = "unavailable"
		summary.DegradedReason = "invalid_window"
		return summary
	}
	endMinute := startMinute + durationMinutes - 1
	if len(minuteBuckets) == 0 || coverageStart.IsZero() || coverageEnd.IsZero() {
		summary.Integrity = "unavailable"
		summary.DegradedReason = "minute_buckets_unavailable"
		return summary
	}
	coverageStartMinute := coverageStart.UTC().Unix() / 60
	coverageEndMinute := coverageEnd.UTC().Unix() / 60
	if coverageStartMinute > startMinute || coverageEndMinute < endMinute {
		summary.Integrity = "unavailable"
		if coverageEndMinute < endMinute {
			summary.DegradedReason = "coverage_warming"
		} else {
			summary.DegradedReason = "coverage_gap"
		}
		return summary
	}
	for minute, bucket := range minuteBuckets {
		if minute < startMinute || minute > endMinute {
			continue
		}
		summary.Requests += bucket.Requests
		summary.SuccessCount += bucket.SuccessCount
		summary.FailureCount += bucket.FailureCount
		summary.TotalTokens += bucket.TotalTokens
		summary.CountOnly += bucket.CountOnly
		summary.Dropped += bucket.Dropped
	}
	if len(dropped.ByMinute) > 0 {
		for minute, count := range dropped.ByMinute {
			if minute < startMinute || minute > endMinute {
				continue
			}
			summary.Dropped += count
		}
	}
	summary.Available = true
	summary.Integrity = "exact"
	if summary.Dropped > 0 {
		summary.Available = false
		summary.Integrity = "degraded"
		summary.DegradedReason = "dropped_usage_records"
	}
	return summary
}

func (s *RequestStatistics) buildRollingSnapshotLocked(now time.Time, dropped coreusage.DroppedRecordsSnapshot) RollingSnapshot {
	result := RollingSnapshot{
		ResolutionMinutes: rollingResolutionMinutes,
		Integrity:         "unavailable",
	}
	prunedBuckets, prunedCoverageStart := pruneRollingMinuteBuckets(s.rollingMinuteBuckets, s.rollingCoverageStart, now)
	if len(prunedBuckets) == 0 || prunedCoverageStart.IsZero() {
		result.DegradedReason = "minute_buckets_unavailable"
		return result
	}
	coverageStart := prunedCoverageStart.UTC()
	coverageEnd := s.rollingCoverageEnd.UTC()
	if coverageEnd.IsZero() {
		coverageEnd = now.UTC().Truncate(time.Minute)
	}
	result.CoverageStart = &coverageStart
	result.CoverageEnd = &coverageEnd
	startMinute7H := coverageEnd.Add(-7*time.Hour+time.Minute).Unix() / 60
	startMinute24H := coverageEnd.Add(-24*time.Hour+time.Minute).Unix() / 60
	startMinute7D := coverageEnd.Add(-(7*24)*time.Hour+time.Minute).Unix() / 60
	result.Windows.Window7H = buildRollingWindowSummary(prunedBuckets, startMinute7H, 7*60, coverageStart, coverageEnd, dropped)
	result.Windows.Window24H = buildRollingWindowSummary(prunedBuckets, startMinute24H, 24*60, coverageStart, coverageEnd, dropped)
	result.Windows.Window7D = buildRollingWindowSummary(prunedBuckets, startMinute7D, 7*24*60, coverageStart, coverageEnd, dropped)
	result.Available = result.Windows.Window7H.Available || result.Windows.Window24H.Available || result.Windows.Window7D.Available
	if result.Windows.Window7H.Available && result.Windows.Window24H.Available && result.Windows.Window7D.Available {
		result.Integrity = "exact"
		result.Available = true
		return result
	}
	result.Integrity = "unavailable"
	for _, window := range []RollingWindowSummary{result.Windows.Window7H, result.Windows.Window24H, result.Windows.Window7D} {
		if window.DegradedReason != "" {
			result.DegradedReason = window.DegradedReason
			break
		}
	}
	if result.DegradedReason == "" {
		result.DegradedReason = "rolling_unavailable"
	}
	return result
}

func (s *RequestStatistics) SnapshotUsageBreakdowns() UsageBreakdownsSnapshot {
	if s == nil {
		return UsageBreakdownsSnapshot{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return UsageBreakdownsSnapshot{
		BySource:    cloneUsageBreakdownBuckets(s.usageBreakdowns.BySource, true),
		ByAuthIndex: cloneUsageBreakdownBuckets(s.usageBreakdowns.ByAuthIndex, false),
	}
}

func normaliseUsageBreakdownBucket(bucket usageBreakdownBucket, source bool) (string, usageBreakdownBucket, bool) {
	key := ""
	if source {
		key = strings.TrimSpace(bucket.Source)
	} else {
		key = strings.TrimSpace(bucket.AuthIndex)
	}
	if key == "" {
		key = strings.TrimSpace(bucket.Key)
	}
	if key == "" {
		return "", usageBreakdownBucket{}, false
	}
	bucket.Key = ""
	bucket.TotalTokens = normaliseTokenStats(TokenStats{
		InputTokens:     bucket.InputTokens,
		OutputTokens:    bucket.OutputTokens,
		ReasoningTokens: bucket.ReasoningTokens,
		CachedTokens:    bucket.CachedTokens,
		TotalTokens:     bucket.TotalTokens,
	}).TotalTokens
	if source {
		bucket.Source = key
		bucket.AuthIndex = ""
	} else {
		bucket.AuthIndex = key
		bucket.Source = ""
	}
	return key, bucket, true
}

func mergeUsageBreakdownBucket(target map[string]*usageBreakdownBucket, key string, bucket usageBreakdownBucket) {
	existing, exists := target[key]
	if !exists || existing == nil {
		copyBucket := bucket
		target[key] = &copyBucket
		return
	}
	existing.TotalRequests += bucket.TotalRequests
	existing.SuccessCount += bucket.SuccessCount
	existing.FailureCount += bucket.FailureCount
	existing.TotalTokens += bucket.TotalTokens
	existing.InputTokens += bucket.InputTokens
	existing.OutputTokens += bucket.OutputTokens
	existing.ReasoningTokens += bucket.ReasoningTokens
	existing.CachedTokens += bucket.CachedTokens
	if bucket.LastUsed.After(existing.LastUsed) {
		existing.LastUsed = bucket.LastUsed
	}
}

func (s *RequestStatistics) applyUsageBreakdownsSnapshotLocked(snapshot UsageBreakdownsSnapshot) {
	s.ensureBreakdownMaps()
	for _, item := range snapshot.BySource {
		key, bucket, ok := normaliseUsageBreakdownBucket(item, true)
		if !ok {
			continue
		}
		mergeUsageBreakdownBucket(s.usageBreakdowns.BySource, key, bucket)
	}
	for _, item := range snapshot.ByAuthIndex {
		key, bucket, ok := normaliseUsageBreakdownBucket(item, false)
		if !ok {
			continue
		}
		mergeUsageBreakdownBucket(s.usageBreakdowns.ByAuthIndex, key, bucket)
	}
}

func (s *RequestStatistics) replaceUsageBreakdownsSnapshotLocked(snapshot UsageBreakdownsSnapshot) {
	s.usageBreakdowns = usageBreakdownBuckets{
		BySource:    make(map[string]*usageBreakdownBucket, len(snapshot.BySource)),
		ByAuthIndex: make(map[string]*usageBreakdownBucket, len(snapshot.ByAuthIndex)),
	}
	for _, item := range snapshot.BySource {
		key, bucket, ok := normaliseUsageBreakdownBucket(item, true)
		if !ok {
			continue
		}
		copyBucket := bucket
		s.usageBreakdowns.BySource[key] = &copyBucket
	}
	for _, item := range snapshot.ByAuthIndex {
		key, bucket, ok := normaliseUsageBreakdownBucket(item, false)
		if !ok {
			continue
		}
		copyBucket := bucket
		s.usageBreakdowns.ByAuthIndex[key] = &copyBucket
	}
}

func (m *modelStats) appendDetail(detail RequestDetail) {
	if m == nil || maxRequestDetailsPerModel <= 0 {
		return
	}
	if m.details == nil {
		m.details = make([]RequestDetail, 0, maxRequestDetailsPerModel)
		m.detailsNext = 0
	}

	if len(m.details) < maxRequestDetailsPerModel {
		m.details = append(m.details, detail)
		return
	}
	// Safety clamp: should not happen, but prevents unbounded growth if state is corrupted.
	if len(m.details) > maxRequestDetailsPerModel {
		m.details = append(m.details[:0], m.details[len(m.details)-maxRequestDetailsPerModel:]...)
		m.detailsNext = 0
	}

	m.details[m.detailsNext] = detail
	m.detailsNext++
	if m.detailsNext >= maxRequestDetailsPerModel {
		m.detailsNext = 0
	}
}

// orderedDetailSegments returns two underlying slices that represent the details in
// chronological order (oldest->newest). The caller must not mutate the returned slices.
func (m *modelStats) orderedDetailSegments() ([]RequestDetail, []RequestDetail) {
	if m == nil || len(m.details) == 0 {
		return nil, nil
	}
	if len(m.details) < maxRequestDetailsPerModel || m.detailsNext == 0 {
		return m.details, nil
	}
	return m.details[m.detailsNext:], m.details[:m.detailsNext]
}

// RequestDetail stores the timestamp and token usage for a single request.
type RequestDetail struct {
	Timestamp time.Time  `json:"timestamp"`
	Source    string     `json:"source"`
	AuthIndex string     `json:"auth_index"`
	Tokens    TokenStats `json:"tokens"`
	Failed    bool       `json:"failed"`
}

func sanitiseDetailSource(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return ""
	}
	if strings.HasPrefix(source, "api:hmac256:") {
		return source
	}
	if strings.Contains(source, "@") {
		return source
	}
	if shouldHashAPIKeyCandidate(source) {
		return HashClientKey(source)
	}
	return source
}

// TokenStats captures the token usage breakdown for a request.
type TokenStats struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	CachedTokens    int64 `json:"cached_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
}

// StatisticsSnapshot represents an immutable view of the aggregated metrics.
type StatisticsSnapshot struct {
	TotalRequests int64 `json:"total_requests"`
	SuccessCount  int64 `json:"success_count"`
	FailureCount  int64 `json:"failure_count"`
	TotalTokens   int64 `json:"total_tokens"`

	APIs map[string]APISnapshot `json:"apis"`

	Breakdowns UsageBreakdownsSnapshot `json:"breakdowns,omitempty"`

	RequestsByDay  map[string]int64     `json:"requests_by_day"`
	RequestsByHour map[string]int64     `json:"requests_by_hour"`
	TokensByDay    map[string]int64     `json:"tokens_by_day"`
	TokensByHour   map[string]int64     `json:"tokens_by_hour"`
	Rolling        RollingSnapshot      `json:"rolling,omitempty"`
	RollingState   RollingStateSnapshot `json:"rolling_state,omitempty"`
}

// APISnapshot summarises metrics for a single API key.
type APISnapshot struct {
	TotalRequests   int64                    `json:"total_requests"`
	SuccessCount    int64                    `json:"success_count"`
	FailureCount    int64                    `json:"failure_count"`
	TotalTokens     int64                    `json:"total_tokens"`
	InputTokens     int64                    `json:"input_tokens"`
	OutputTokens    int64                    `json:"output_tokens"`
	ReasoningTokens int64                    `json:"reasoning_tokens"`
	CachedTokens    int64                    `json:"cached_tokens"`
	LastUsed        time.Time                `json:"last_used,omitempty"`
	Models          map[string]ModelSnapshot `json:"models"`
}

// ModelSnapshot summarises metrics for a specific model.
type ModelSnapshot struct {
	TotalRequests   int64           `json:"total_requests"`
	SuccessCount    int64           `json:"success_count"`
	FailureCount    int64           `json:"failure_count"`
	TotalTokens     int64           `json:"total_tokens"`
	InputTokens     int64           `json:"input_tokens"`
	OutputTokens    int64           `json:"output_tokens"`
	ReasoningTokens int64           `json:"reasoning_tokens"`
	CachedTokens    int64           `json:"cached_tokens"`
	LastUsed        time.Time       `json:"last_used,omitempty"`
	Details         []RequestDetail `json:"details"`
}

var defaultRequestStatistics = NewRequestStatistics()

// GetRequestStatistics returns the shared statistics store.
func GetRequestStatistics() *RequestStatistics { return defaultRequestStatistics }

// NewRequestStatistics constructs an empty statistics store.
func NewRequestStatistics() *RequestStatistics {
	return &RequestStatistics{
		apis: make(map[string]*apiStats),
		usageBreakdowns: usageBreakdownBuckets{
			BySource:    make(map[string]*usageBreakdownBucket),
			ByAuthIndex: make(map[string]*usageBreakdownBucket),
		},
		requestsByDay:        make(map[string]int64),
		requestsByHour:       make(map[int]int64),
		tokensByDay:          make(map[string]int64),
		tokensByHour:         make(map[int]int64),
		rollingMinuteBuckets: make(map[int64]RollingMinuteBucket),
	}
}

func (s *RequestStatistics) IsDirty() bool {
	if s == nil {
		return false
	}
	return s.dirty.Load()
}

func (s *RequestStatistics) ClearDirty() {
	if s == nil {
		return
	}
	s.dirty.Store(false)
}

// Record ingests a new usage record and updates the aggregates.
func (s *RequestStatistics) Record(ctx context.Context, record coreusage.Record) {
	if s == nil {
		return
	}
	if !statisticsEnabled.Load() {
		return
	}
	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}
	timestamp = timestamp.UTC()
	detail := normaliseDetail(record.Detail)
	totalTokens := detail.TotalTokens
	statsKey := ""
	if record.APIKey != "" {
		statsKey = HashClientKey(record.APIKey)
	}
	if statsKey == "" {
		statsKey = resolveAPIIdentifier(ctx, record)
	}
	failed := record.Failed
	if !failed {
		failed = !resolveSuccess(ctx)
	}
	success := !failed
	modelName := record.Model
	if modelName == "" {
		modelName = "unknown"
	}
	dayKey := timestamp.Format("2006-01-02")
	hourKey := timestamp.Hour()
	minuteKey := timestamp.Truncate(time.Minute).Unix() / 60

	s.mu.Lock()
	defer s.mu.Unlock()

	s.dirty.Store(true)

	s.totalRequests++
	if success {
		s.successCount++
	} else {
		s.failureCount++
	}
	s.totalTokens += totalTokens

	stats, ok := s.apis[statsKey]
	if !ok {
		stats = &apiStats{Models: make(map[string]*modelStats)}
		s.apis[statsKey] = stats
	}
	s.updateAPIStats(stats, modelName, RequestDetail{
		Timestamp: timestamp,
		Source:    sanitiseDetailSource(record.Source),
		AuthIndex: record.AuthIndex,
		Tokens:    detail,
		Failed:    failed,
	})

	s.requestsByDay[dayKey]++
	s.requestsByHour[hourKey]++
	s.tokensByDay[dayKey] += totalTokens
	s.tokensByHour[hourKey] += totalTokens
	s.recordRollingMinuteLocked(minuteKey, RollingMinuteBucket{
		Requests:     1,
		SuccessCount: boolToCount(success),
		FailureCount: boolToCount(failed),
		TotalTokens:  totalTokens,
		CountOnly:    boolToCount(record.CountOnly),
	})
}

func (s *RequestStatistics) updateAPIStats(stats *apiStats, model string, detail RequestDetail) {
	detail.Tokens = normaliseTokenStats(detail.Tokens)
	stats.TotalRequests++
	if detail.Failed {
		stats.FailureCount++
	} else {
		stats.SuccessCount++
	}
	stats.TotalTokens += detail.Tokens.TotalTokens
	stats.InputTokens += detail.Tokens.InputTokens
	stats.OutputTokens += detail.Tokens.OutputTokens
	stats.ReasoningTokens += detail.Tokens.ReasoningTokens
	stats.CachedTokens += detail.Tokens.CachedTokens
	if detail.Timestamp.After(stats.LastUsed) {
		stats.LastUsed = detail.Timestamp
	}
	modelStatsValue, ok := stats.Models[model]
	if !ok {
		modelStatsValue = newModelStats()
		stats.Models[model] = modelStatsValue
	}
	modelStatsValue.TotalRequests++
	if detail.Failed {
		modelStatsValue.FailureCount++
	} else {
		modelStatsValue.SuccessCount++
	}
	modelStatsValue.TotalTokens += detail.Tokens.TotalTokens
	modelStatsValue.InputTokens += detail.Tokens.InputTokens
	modelStatsValue.OutputTokens += detail.Tokens.OutputTokens
	modelStatsValue.ReasoningTokens += detail.Tokens.ReasoningTokens
	modelStatsValue.CachedTokens += detail.Tokens.CachedTokens
	if detail.Timestamp.After(modelStatsValue.LastUsed) {
		modelStatsValue.LastUsed = detail.Timestamp
	}
	modelStatsValue.appendDetail(detail)
	s.updateUsageBreakdowns(detail)
}

// Snapshot returns a copy of the aggregated metrics for external consumption.
func (s *RequestStatistics) Snapshot() StatisticsSnapshot {
	result := StatisticsSnapshot{}
	if s == nil {
		return result
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	result.TotalRequests = s.totalRequests
	result.SuccessCount = s.successCount
	result.FailureCount = s.failureCount
	result.TotalTokens = s.totalTokens

	result.APIs = make(map[string]APISnapshot, len(s.apis))
	for apiName, stats := range s.apis {
		apiSnapshot := APISnapshot{
			TotalRequests:   stats.TotalRequests,
			SuccessCount:    stats.SuccessCount,
			FailureCount:    stats.FailureCount,
			TotalTokens:     stats.TotalTokens,
			InputTokens:     stats.InputTokens,
			OutputTokens:    stats.OutputTokens,
			ReasoningTokens: stats.ReasoningTokens,
			CachedTokens:    stats.CachedTokens,
			LastUsed:        stats.LastUsed,
			Models:          make(map[string]ModelSnapshot, len(stats.Models)),
		}
		for modelName, modelStatsValue := range stats.Models {
			segA, segB := modelStatsValue.orderedDetailSegments()
			totalDetails := len(segA) + len(segB)
			requestDetails := make([]RequestDetail, totalDetails)
			copy(requestDetails, segA)
			copy(requestDetails[len(segA):], segB)
			apiSnapshot.Models[modelName] = ModelSnapshot{
				TotalRequests:   modelStatsValue.TotalRequests,
				SuccessCount:    modelStatsValue.SuccessCount,
				FailureCount:    modelStatsValue.FailureCount,
				TotalTokens:     modelStatsValue.TotalTokens,
				InputTokens:     modelStatsValue.InputTokens,
				OutputTokens:    modelStatsValue.OutputTokens,
				ReasoningTokens: modelStatsValue.ReasoningTokens,
				CachedTokens:    modelStatsValue.CachedTokens,
				LastUsed:        modelStatsValue.LastUsed,
				Details:         requestDetails,
			}
		}
		result.APIs[apiName] = apiSnapshot
	}
	result.Breakdowns = UsageBreakdownsSnapshot{
		BySource:    cloneUsageBreakdownBuckets(s.usageBreakdowns.BySource, true),
		ByAuthIndex: cloneUsageBreakdownBuckets(s.usageBreakdowns.ByAuthIndex, false),
	}

	result.RequestsByDay = make(map[string]int64, len(s.requestsByDay))
	for k, v := range s.requestsByDay {
		result.RequestsByDay[k] = v
	}

	result.RequestsByHour = make(map[string]int64, len(s.requestsByHour))
	for hour, v := range s.requestsByHour {
		key := formatHour(hour)
		result.RequestsByHour[key] = v
	}

	result.TokensByDay = make(map[string]int64, len(s.tokensByDay))
	for k, v := range s.tokensByDay {
		result.TokensByDay[k] = v
	}

	result.TokensByHour = make(map[string]int64, len(s.tokensByHour))
	for hour, v := range s.tokensByHour {
		key := formatHour(hour)
		result.TokensByHour[key] = v
	}

	droppedSnapshot := coreusage.DefaultManager().DroppedRecordsSnapshot()
	result.Rolling = s.buildRollingSnapshotLocked(time.Now().UTC(), droppedSnapshot)
	result.RollingState = snapshotRollingState(s.rollingCoverageStart, s.rollingCoverageEnd, s.rollingMinuteBuckets, time.Now().UTC())

	return result
}

// SnapshotAggregated returns a snapshot suitable for persistence. It purposely excludes
// per-request details and any plaintext API keys.
func (s *RequestStatistics) SnapshotAggregated() AggregatedStatisticsSnapshot {
	result := AggregatedStatisticsSnapshot{}
	if s == nil {
		return result
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	result.TotalRequests = s.totalRequests
	result.SuccessCount = s.successCount
	result.FailureCount = s.failureCount
	result.TotalTokens = s.totalTokens

	result.APIs = make(map[string]AggregatedAPISnapshot, len(s.apis))
	for apiName, stats := range s.apis {
		apiSnapshot := AggregatedAPISnapshot{
			TotalRequests:   stats.TotalRequests,
			SuccessCount:    stats.SuccessCount,
			FailureCount:    stats.FailureCount,
			TotalTokens:     stats.TotalTokens,
			InputTokens:     stats.InputTokens,
			OutputTokens:    stats.OutputTokens,
			ReasoningTokens: stats.ReasoningTokens,
			CachedTokens:    stats.CachedTokens,
			LastUsed:        stats.LastUsed,
			Models:          make(map[string]AggregatedModelSnapshot, len(stats.Models)),
		}
		for modelName, modelStatsValue := range stats.Models {
			apiSnapshot.Models[modelName] = AggregatedModelSnapshot{
				TotalRequests:   modelStatsValue.TotalRequests,
				SuccessCount:    modelStatsValue.SuccessCount,
				FailureCount:    modelStatsValue.FailureCount,
				TotalTokens:     modelStatsValue.TotalTokens,
				InputTokens:     modelStatsValue.InputTokens,
				OutputTokens:    modelStatsValue.OutputTokens,
				ReasoningTokens: modelStatsValue.ReasoningTokens,
				CachedTokens:    modelStatsValue.CachedTokens,
				LastUsed:        modelStatsValue.LastUsed,
			}
		}
		result.APIs[apiName] = apiSnapshot
	}
	result.Breakdowns = UsageBreakdownsSnapshot{
		BySource:    cloneUsageBreakdownBuckets(s.usageBreakdowns.BySource, true),
		ByAuthIndex: cloneUsageBreakdownBuckets(s.usageBreakdowns.ByAuthIndex, false),
	}

	result.RequestsByDay = make(map[string]int64, len(s.requestsByDay))
	for k, v := range s.requestsByDay {
		result.RequestsByDay[k] = v
	}

	result.RequestsByHour = make(map[string]int64, len(s.requestsByHour))
	for hour, v := range s.requestsByHour {
		result.RequestsByHour[formatHour(hour)] = v
	}

	result.TokensByDay = make(map[string]int64, len(s.tokensByDay))
	for k, v := range s.tokensByDay {
		result.TokensByDay[k] = v
	}

	result.TokensByHour = make(map[string]int64, len(s.tokensByHour))
	for hour, v := range s.tokensByHour {
		result.TokensByHour[formatHour(hour)] = v
	}

	droppedSnapshot := coreusage.DefaultManager().DroppedRecordsSnapshot()
	rollingBuckets := cloneRollingMinuteBuckets(s.rollingMinuteBuckets)
	coverageStart := s.rollingCoverageStart
	coverageEnd := s.rollingCoverageEnd
	if len(droppedSnapshot.ByMinute) > 0 {
		if rollingBuckets == nil {
			rollingBuckets = make(map[int64]RollingMinuteBucket, len(droppedSnapshot.ByMinute))
		}
		for minute, count := range droppedSnapshot.ByMinute {
			rollingBuckets[minute] = mergeRollingMinuteBucket(rollingBuckets[minute], RollingMinuteBucket{Dropped: count})
			candidate := time.Unix(minute*60, 0).UTC()
			if coverageStart.IsZero() || candidate.Before(coverageStart) {
				coverageStart = candidate
			}
			if coverageEnd.IsZero() || candidate.After(coverageEnd) {
				coverageEnd = candidate
			}
		}
	}
	result.RollingState = snapshotRollingState(coverageStart, coverageEnd, rollingBuckets, time.Now().UTC())

	return result
}

// ApplyAggregatedSnapshot merges a persisted aggregated snapshot into the current store.
// It marks the store dirty because applying persisted state changes totals.
func (s *RequestStatistics) ApplyAggregatedSnapshot(snapshot AggregatedStatisticsSnapshot) {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.dirty.Store(true)

	s.totalRequests += snapshot.TotalRequests
	s.successCount += snapshot.SuccessCount
	s.failureCount += snapshot.FailureCount
	s.totalTokens += snapshot.TotalTokens

	if s.apis == nil {
		s.apis = make(map[string]*apiStats)
	}
	for apiName, apiSnapshot := range snapshot.APIs {
		apiName = strings.TrimSpace(apiName)
		if apiName == "" {
			continue
		}
		if !strings.HasPrefix(apiName, "api:hmac256:") && shouldHashAPIKeyCandidate(apiName) {
			apiName = HashClientKey(apiName)
		}
		stats, ok := s.apis[apiName]
		if !ok || stats == nil {
			stats = &apiStats{Models: make(map[string]*modelStats)}
			s.apis[apiName] = stats
		} else if stats.Models == nil {
			stats.Models = make(map[string]*modelStats)
		}
		stats.TotalRequests += apiSnapshot.TotalRequests
		stats.SuccessCount += apiSnapshot.SuccessCount
		stats.FailureCount += apiSnapshot.FailureCount
		stats.TotalTokens += apiSnapshot.TotalTokens
		stats.InputTokens += apiSnapshot.InputTokens
		stats.OutputTokens += apiSnapshot.OutputTokens
		stats.ReasoningTokens += apiSnapshot.ReasoningTokens
		stats.CachedTokens += apiSnapshot.CachedTokens
		if apiSnapshot.LastUsed.After(stats.LastUsed) {
			stats.LastUsed = apiSnapshot.LastUsed
		}
		for modelName, modelSnapshot := range apiSnapshot.Models {
			modelName = strings.TrimSpace(modelName)
			if modelName == "" {
				modelName = "unknown"
			}
			modelStatsValue, ok := stats.Models[modelName]
			if !ok || modelStatsValue == nil {
				modelStatsValue = newModelStats()
				stats.Models[modelName] = modelStatsValue
			}
			modelStatsValue.TotalRequests += modelSnapshot.TotalRequests
			modelStatsValue.SuccessCount += modelSnapshot.SuccessCount
			modelStatsValue.FailureCount += modelSnapshot.FailureCount
			modelStatsValue.TotalTokens += modelSnapshot.TotalTokens
			modelStatsValue.InputTokens += modelSnapshot.InputTokens
			modelStatsValue.OutputTokens += modelSnapshot.OutputTokens
			modelStatsValue.ReasoningTokens += modelSnapshot.ReasoningTokens
			modelStatsValue.CachedTokens += modelSnapshot.CachedTokens
			if modelSnapshot.LastUsed.After(modelStatsValue.LastUsed) {
				modelStatsValue.LastUsed = modelSnapshot.LastUsed
			}
		}
	}
	s.applyUsageBreakdownsSnapshotLocked(snapshot.Breakdowns)

	if s.requestsByDay == nil {
		s.requestsByDay = make(map[string]int64)
	}
	for k, v := range snapshot.RequestsByDay {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s.requestsByDay[k] += v
	}

	rollingCoverageStart, rollingCoverageEnd, rollingMinuteBuckets := restoreRollingState(snapshot.RollingState)
	if len(rollingMinuteBuckets) > 0 {
		if s.rollingCoverageStart.IsZero() || (!rollingCoverageStart.IsZero() && rollingCoverageStart.Before(s.rollingCoverageStart)) {
			s.rollingCoverageStart = rollingCoverageStart
		}
		if s.rollingCoverageEnd.IsZero() || (!rollingCoverageEnd.IsZero() && rollingCoverageEnd.After(s.rollingCoverageEnd)) {
			s.rollingCoverageEnd = rollingCoverageEnd
		}
		s.ensureRollingBucketsLocked()
		for minute, bucket := range rollingMinuteBuckets {
			s.rollingMinuteBuckets[minute] = mergeRollingMinuteBucket(s.rollingMinuteBuckets[minute], bucket)
		}
	}

	if s.requestsByHour == nil {
		s.requestsByHour = make(map[int]int64)
	}
	for hourKey, v := range snapshot.RequestsByHour {
		hourInt, err := strconv.Atoi(strings.TrimSpace(hourKey))
		if err != nil {
			continue
		}
		if hourInt < 0 || hourInt > 23 {
			continue
		}
		s.requestsByHour[hourInt] += v
	}

	if s.tokensByDay == nil {
		s.tokensByDay = make(map[string]int64)
	}
	for k, v := range snapshot.TokensByDay {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s.tokensByDay[k] += v
	}

	if s.tokensByHour == nil {
		s.tokensByHour = make(map[int]int64)
	}
	for hourKey, v := range snapshot.TokensByHour {
		hourInt, err := strconv.Atoi(strings.TrimSpace(hourKey))
		if err != nil {
			continue
		}
		if hourInt < 0 || hourInt > 23 {
			continue
		}
		s.tokensByHour[hourInt] += v
	}
}

// ReplaceAggregatedSnapshot replaces the current aggregated store with snapshot values.
// Unlike ApplyAggregatedSnapshot, this method is idempotent and safe for startup loads.
func (s *RequestStatistics) ReplaceAggregatedSnapshot(snapshot AggregatedStatisticsSnapshot) {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.totalRequests = snapshot.TotalRequests
	s.successCount = snapshot.SuccessCount
	s.failureCount = snapshot.FailureCount
	s.totalTokens = snapshot.TotalTokens

	s.apis = make(map[string]*apiStats, len(snapshot.APIs))
	for apiName, apiSnapshot := range snapshot.APIs {
		apiName = strings.TrimSpace(apiName)
		if apiName == "" {
			continue
		}
		if !strings.HasPrefix(apiName, "api:hmac256:") && shouldHashAPIKeyCandidate(apiName) {
			apiName = HashClientKey(apiName)
		}
		stats := &apiStats{
			TotalRequests:   apiSnapshot.TotalRequests,
			SuccessCount:    apiSnapshot.SuccessCount,
			FailureCount:    apiSnapshot.FailureCount,
			TotalTokens:     apiSnapshot.TotalTokens,
			InputTokens:     apiSnapshot.InputTokens,
			OutputTokens:    apiSnapshot.OutputTokens,
			ReasoningTokens: apiSnapshot.ReasoningTokens,
			CachedTokens:    apiSnapshot.CachedTokens,
			LastUsed:        apiSnapshot.LastUsed,
			Models:          make(map[string]*modelStats, len(apiSnapshot.Models)),
		}
		for modelName, modelSnapshot := range apiSnapshot.Models {
			modelName = strings.TrimSpace(modelName)
			if modelName == "" {
				modelName = "unknown"
			}
			stats.Models[modelName] = &modelStats{
				TotalRequests:   modelSnapshot.TotalRequests,
				SuccessCount:    modelSnapshot.SuccessCount,
				FailureCount:    modelSnapshot.FailureCount,
				TotalTokens:     modelSnapshot.TotalTokens,
				InputTokens:     modelSnapshot.InputTokens,
				OutputTokens:    modelSnapshot.OutputTokens,
				ReasoningTokens: modelSnapshot.ReasoningTokens,
				CachedTokens:    modelSnapshot.CachedTokens,
				LastUsed:        modelSnapshot.LastUsed,
				details:         nil,
				detailsNext:     0,
			}
		}
		s.apis[apiName] = stats
	}
	s.replaceUsageBreakdownsSnapshotLocked(snapshot.Breakdowns)

	s.requestsByDay = make(map[string]int64, len(snapshot.RequestsByDay))
	for k, v := range snapshot.RequestsByDay {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s.requestsByDay[k] = v
	}

	s.requestsByHour = make(map[int]int64, len(snapshot.RequestsByHour))
	for hourKey, v := range snapshot.RequestsByHour {
		hourInt, err := strconv.Atoi(strings.TrimSpace(hourKey))
		if err != nil || hourInt < 0 || hourInt > 23 {
			continue
		}
		s.requestsByHour[hourInt] = v
	}

	s.tokensByDay = make(map[string]int64, len(snapshot.TokensByDay))
	for k, v := range snapshot.TokensByDay {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s.tokensByDay[k] = v
	}

	s.tokensByHour = make(map[int]int64, len(snapshot.TokensByHour))
	for hourKey, v := range snapshot.TokensByHour {
		hourInt, err := strconv.Atoi(strings.TrimSpace(hourKey))
		if err != nil || hourInt < 0 || hourInt > 23 {
			continue
		}
		s.tokensByHour[hourInt] = v
	}

	s.rollingCoverageStart, s.rollingCoverageEnd, s.rollingMinuteBuckets = restoreRollingState(snapshot.RollingState)

	// A restored snapshot is the current persisted baseline, not new dirty state.
	s.dirty.Store(false)
}

func (s *RequestStatistics) ReplaceRollingState(snapshot RollingStateSnapshot) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollingCoverageStart, s.rollingCoverageEnd, s.rollingMinuteBuckets = restoreRollingState(snapshot)
	s.dirty.Store(true)
}

type AggregatedStatisticsSnapshot struct {
	TotalRequests int64 `json:"total_requests"`
	SuccessCount  int64 `json:"success_count"`
	FailureCount  int64 `json:"failure_count"`
	TotalTokens   int64 `json:"total_tokens"`

	APIs map[string]AggregatedAPISnapshot `json:"apis"`

	Breakdowns UsageBreakdownsSnapshot `json:"breakdowns,omitempty"`

	RequestsByDay  map[string]int64     `json:"requests_by_day"`
	RequestsByHour map[string]int64     `json:"requests_by_hour"`
	TokensByDay    map[string]int64     `json:"tokens_by_day"`
	TokensByHour   map[string]int64     `json:"tokens_by_hour"`
	RollingState   RollingStateSnapshot `json:"rolling_state,omitempty"`
}

type AggregatedAPISnapshot struct {
	TotalRequests   int64                              `json:"total_requests"`
	SuccessCount    int64                              `json:"success_count"`
	FailureCount    int64                              `json:"failure_count"`
	TotalTokens     int64                              `json:"total_tokens"`
	InputTokens     int64                              `json:"input_tokens"`
	OutputTokens    int64                              `json:"output_tokens"`
	ReasoningTokens int64                              `json:"reasoning_tokens"`
	CachedTokens    int64                              `json:"cached_tokens"`
	LastUsed        time.Time                          `json:"last_used,omitempty"`
	Models          map[string]AggregatedModelSnapshot `json:"models"`
}

type AggregatedModelSnapshot struct {
	TotalRequests   int64     `json:"total_requests"`
	SuccessCount    int64     `json:"success_count"`
	FailureCount    int64     `json:"failure_count"`
	TotalTokens     int64     `json:"total_tokens"`
	InputTokens     int64     `json:"input_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	ReasoningTokens int64     `json:"reasoning_tokens"`
	CachedTokens    int64     `json:"cached_tokens"`
	LastUsed        time.Time `json:"last_used,omitempty"`
}

type MergeResult struct {
	Added   int64 `json:"added"`
	Skipped int64 `json:"skipped"`
}

func statisticsSnapshotHasDetails(snapshot StatisticsSnapshot) bool {
	for _, apiSnapshot := range snapshot.APIs {
		for _, modelSnapshot := range apiSnapshot.Models {
			if len(modelSnapshot.Details) > 0 {
				return true
			}
		}
	}
	return false
}

func statisticsSnapshotHasAggregateOnlyModels(snapshot StatisticsSnapshot) bool {
	for _, apiSnapshot := range snapshot.APIs {
		for _, modelSnapshot := range apiSnapshot.Models {
			if len(modelSnapshot.Details) == 0 && (modelSnapshot.TotalRequests > 0 || modelSnapshot.TotalTokens > 0) {
				return true
			}
		}
	}
	return false
}

func aggregatedSnapshotFromStatisticsSnapshot(snapshot StatisticsSnapshot) AggregatedStatisticsSnapshot {
	result := AggregatedStatisticsSnapshot{
		TotalRequests:  snapshot.TotalRequests,
		SuccessCount:   snapshot.SuccessCount,
		FailureCount:   snapshot.FailureCount,
		TotalTokens:    snapshot.TotalTokens,
		APIs:           make(map[string]AggregatedAPISnapshot, len(snapshot.APIs)),
		Breakdowns:     snapshot.Breakdowns,
		RequestsByDay:  make(map[string]int64, len(snapshot.RequestsByDay)),
		RequestsByHour: make(map[string]int64, len(snapshot.RequestsByHour)),
		TokensByDay:    make(map[string]int64, len(snapshot.TokensByDay)),
		TokensByHour:   make(map[string]int64, len(snapshot.TokensByHour)),
		RollingState:   cloneRollingStateSnapshot(snapshot.RollingState),
	}
	for apiName, apiSnapshot := range snapshot.APIs {
		models := make(map[string]AggregatedModelSnapshot, len(apiSnapshot.Models))
		for modelName, modelSnapshot := range apiSnapshot.Models {
			models[modelName] = AggregatedModelSnapshot{
				TotalRequests:   modelSnapshot.TotalRequests,
				SuccessCount:    modelSnapshot.SuccessCount,
				FailureCount:    modelSnapshot.FailureCount,
				TotalTokens:     modelSnapshot.TotalTokens,
				InputTokens:     modelSnapshot.InputTokens,
				OutputTokens:    modelSnapshot.OutputTokens,
				ReasoningTokens: modelSnapshot.ReasoningTokens,
				CachedTokens:    modelSnapshot.CachedTokens,
				LastUsed:        modelSnapshot.LastUsed,
			}
		}
		result.APIs[apiName] = AggregatedAPISnapshot{
			TotalRequests:   apiSnapshot.TotalRequests,
			SuccessCount:    apiSnapshot.SuccessCount,
			FailureCount:    apiSnapshot.FailureCount,
			TotalTokens:     apiSnapshot.TotalTokens,
			InputTokens:     apiSnapshot.InputTokens,
			OutputTokens:    apiSnapshot.OutputTokens,
			ReasoningTokens: apiSnapshot.ReasoningTokens,
			CachedTokens:    apiSnapshot.CachedTokens,
			LastUsed:        apiSnapshot.LastUsed,
			Models:          models,
		}
	}
	for key, value := range snapshot.RequestsByDay {
		result.RequestsByDay[key] = value
	}
	for key, value := range snapshot.RequestsByHour {
		result.RequestsByHour[key] = value
	}
	for key, value := range snapshot.TokensByDay {
		result.TokensByDay[key] = value
	}
	for key, value := range snapshot.TokensByHour {
		result.TokensByHour[key] = value
	}
	return result
}

// MergeSnapshot merges an exported statistics snapshot into the current store.
// Existing data is preserved and duplicate request details are skipped.
func (s *RequestStatistics) MergeSnapshot(snapshot StatisticsSnapshot) MergeResult {
	result := MergeResult{}
	if s == nil {
		return result
	}
	if !statisticsSnapshotHasDetails(snapshot) {
		s.ApplyAggregatedSnapshot(aggregatedSnapshotFromStatisticsSnapshot(snapshot))
		result.Added = snapshot.TotalRequests
		return result
	}
	if statisticsSnapshotHasAggregateOnlyModels(snapshot) {
		s.ApplyAggregatedSnapshot(aggregatedSnapshotFromStatisticsSnapshot(snapshot))
		result.Added = snapshot.TotalRequests
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	seen := make(map[string]struct{})
	for apiName, stats := range s.apis {
		if stats == nil {
			continue
		}
		for modelName, modelStatsValue := range stats.Models {
			if modelStatsValue == nil {
				continue
			}
			segA, segB := modelStatsValue.orderedDetailSegments()
			for _, detail := range segA {
				seen[dedupKey(apiName, modelName, detail)] = struct{}{}
			}
			for _, detail := range segB {
				seen[dedupKey(apiName, modelName, detail)] = struct{}{}
			}
		}
	}

	for apiName, apiSnapshot := range snapshot.APIs {
		apiName = strings.TrimSpace(apiName)
		if apiName == "" {
			continue
		}
		if !strings.HasPrefix(apiName, "api:hmac256:") && shouldHashAPIKeyCandidate(apiName) {
			apiName = HashClientKey(apiName)
		}
		stats, ok := s.apis[apiName]
		if !ok || stats == nil {
			stats = &apiStats{Models: make(map[string]*modelStats)}
			s.apis[apiName] = stats
		} else if stats.Models == nil {
			stats.Models = make(map[string]*modelStats)
		}
		for modelName, modelSnapshot := range apiSnapshot.Models {
			modelName = strings.TrimSpace(modelName)
			if modelName == "" {
				modelName = "unknown"
			}
			for _, detail := range modelSnapshot.Details {
				detail.Tokens = normaliseTokenStats(detail.Tokens)
				if detail.Timestamp.IsZero() {
					detail.Timestamp = time.Now()
				}
				key := dedupKey(apiName, modelName, detail)
				if _, exists := seen[key]; exists {
					result.Skipped++
					continue
				}
				seen[key] = struct{}{}
				s.recordImported(apiName, modelName, stats, detail)
				result.Added++
			}
		}
	}

	return result
}

func (s *RequestStatistics) recordImported(apiName, modelName string, stats *apiStats, detail RequestDetail) {
	detail.Source = sanitiseDetailSource(detail.Source)
	totalTokens := detail.Tokens.TotalTokens
	if totalTokens < 0 {
		totalTokens = 0
	}

	s.dirty.Store(true)

	s.totalRequests++
	if detail.Failed {
		s.failureCount++
	} else {
		s.successCount++
	}
	s.totalTokens += totalTokens

	s.updateAPIStats(stats, modelName, detail)

	timestamp := detail.Timestamp.UTC()
	dayKey := timestamp.Format("2006-01-02")
	hourKey := timestamp.Hour()
	minuteKey := timestamp.Truncate(time.Minute).Unix() / 60

	s.requestsByDay[dayKey]++
	s.requestsByHour[hourKey]++
	s.tokensByDay[dayKey] += totalTokens
	s.tokensByHour[hourKey] += totalTokens
	s.recordRollingMinuteLocked(minuteKey, RollingMinuteBucket{
		Requests:     1,
		SuccessCount: boolToCount(!detail.Failed),
		FailureCount: boolToCount(detail.Failed),
		TotalTokens:  totalTokens,
	})
}

func dedupKey(apiName, modelName string, detail RequestDetail) string {
	timestamp := detail.Timestamp.UTC().Format(time.RFC3339Nano)
	tokens := normaliseTokenStats(detail.Tokens)
	return fmt.Sprintf(
		"%s|%s|%s|%s|%s|%t|%d|%d|%d|%d|%d",
		apiName,
		modelName,
		timestamp,
		detail.Source,
		detail.AuthIndex,
		detail.Failed,
		tokens.InputTokens,
		tokens.OutputTokens,
		tokens.ReasoningTokens,
		tokens.CachedTokens,
		tokens.TotalTokens,
	)
}

func resolveAPIIdentifier(ctx context.Context, record coreusage.Record) string {
	if ctx != nil {
		if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil {
			path := ginCtx.FullPath()
			if path == "" && ginCtx.Request != nil {
				path = ginCtx.Request.URL.Path
			}
			method := ""
			if ginCtx.Request != nil {
				method = ginCtx.Request.Method
			}
			if path != "" {
				if method != "" {
					return method + " " + path
				}
				return path
			}
		}
	}
	if record.Provider != "" {
		return record.Provider
	}
	return "unknown"
}

func resolveSuccess(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil {
		return true
	}
	status := ginCtx.Writer.Status()
	if status == 0 {
		return true
	}
	return status < httpStatusBadRequest
}

const httpStatusBadRequest = 400

func normaliseDetail(detail coreusage.Detail) TokenStats {
	tokens := TokenStats{
		InputTokens:     detail.InputTokens,
		OutputTokens:    detail.OutputTokens,
		ReasoningTokens: detail.ReasoningTokens,
		CachedTokens:    detail.CachedTokens,
		TotalTokens:     detail.TotalTokens,
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens + detail.CachedTokens
	}
	return tokens
}

func normaliseTokenStats(tokens TokenStats) TokenStats {
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens + tokens.CachedTokens
	}
	return tokens
}

func formatHour(hour int) string {
	if hour < 0 {
		hour = 0
	}
	hour = hour % 24
	return fmt.Sprintf("%02d", hour)
}
