// Package usage provides usage tracking and logging functionality for the CLI Proxy API server.
package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

// persistedData represents the JSON structure for saving/loading statistics.
type persistedData struct {
	Version        int                       `json:"version"`
	SavedAt        time.Time                 `json:"saved_at"`
	TotalRequests  int64                     `json:"total_requests"`
	SuccessCount   int64                     `json:"success_count"`
	FailureCount   int64                     `json:"failure_count"`
	TotalTokens    int64                     `json:"total_tokens"`
	APIs           map[string]*persistedAPI  `json:"apis"`
	RequestsByDay  map[string]int64          `json:"requests_by_day"`
	RequestsByHour map[int]int64             `json:"requests_by_hour"`
	TokensByDay    map[string]int64          `json:"tokens_by_day"`
	TokensByHour   map[int]int64             `json:"tokens_by_hour"`
}

type persistedAPI struct {
	TotalRequests int64                      `json:"total_requests"`
	TotalTokens   int64                      `json:"total_tokens"`
	Models        map[string]*persistedModel `json:"models"`
}

type persistedModel struct {
	TotalRequests int64           `json:"total_requests"`
	TotalTokens   int64           `json:"total_tokens"`
	Details       []RequestDetail `json:"details"`
}

func buildLegacyBreakdowns(data persistedData) UsageBreakdownsSnapshot {
	bySource := make(map[string]*usageBreakdownBucket)
	byAuthIndex := make(map[string]*usageBreakdownBucket)
	for _, api := range data.APIs {
		if api == nil {
			continue
		}
		for _, model := range api.Models {
			if model == nil {
				continue
			}
			for _, detail := range model.Details {
				detail.Source = sanitiseDetailSource(detail.Source)
				detail.Tokens = normaliseTokenStats(detail.Tokens)
				source := strings.TrimSpace(detail.Source)
				if source == "" {
					source = "unknown"
				}
				authIndex := strings.TrimSpace(detail.AuthIndex)
				if authIndex == "" {
					authIndex = "unknown"
				}
				bucket, ok := bySource[source]
				if !ok || bucket == nil {
					bucket = &usageBreakdownBucket{Source: source}
					bySource[source] = bucket
				}
				bucket.update(detail)
				bucket, ok = byAuthIndex[authIndex]
				if !ok || bucket == nil {
					bucket = &usageBreakdownBucket{AuthIndex: authIndex}
					byAuthIndex[authIndex] = bucket
				}
				bucket.update(detail)
			}
		}
	}
	return UsageBreakdownsSnapshot{
		BySource:    cloneUsageBreakdownBuckets(bySource, true),
		ByAuthIndex: cloneUsageBreakdownBuckets(byAuthIndex, false),
	}
}

func orderedModelDetails(m *modelStats) []RequestDetail {
	if m == nil {
		return nil
	}
	seg1, seg2 := m.orderedDetailSegments()
	if len(seg1) == 0 && len(seg2) == 0 {
		return nil
	}
	out := make([]RequestDetail, 0, len(seg1)+len(seg2))
	out = append(out, seg1...)
	out = append(out, seg2...)
	return out
}

func setModelDetails(m *modelStats, details []RequestDetail) {
	if m == nil {
		return
	}
	if maxRequestDetailsPerModel > 0 && len(details) > maxRequestDetailsPerModel {
		// Keep the newest N entries.
		details = details[len(details)-maxRequestDetailsPerModel:]
	}
	if len(details) == 0 {
		m.details = nil
		m.detailsNext = 0
		return
	}
	if cap(m.details) < len(details) {
		m.details = make([]RequestDetail, len(details))
	} else {
		m.details = m.details[:len(details)]
	}
	copy(m.details, details)
	m.detailsNext = 0
}

const (
	persistenceVersion  = 1
	persistenceFilename = "usage_statistics.json"
	autoSaveInterval    = 5 * time.Minute
	// detailRetentionDays controls how many days of detailed request logs to keep.
	// Older details are compacted into daily summaries.
	detailRetentionDays = 30
)

var (
	persistencePath     string
	persistencePathOnce sync.Once
	autoSaveStop        chan struct{}
	autoSaveMu          sync.Mutex
)

// getPersistencePath returns the path to the persistence file.
func getPersistencePath() string {
	persistencePathOnce.Do(func() {
		baseDir := util.WritablePath()
		if baseDir == "" {
			homeDir, err := os.UserHomeDir()
			if err != nil {
				baseDir = "."
			} else {
				baseDir = filepath.Join(homeDir, ".cli-proxy-api")
			}
		}
		persistencePath = filepath.Join(baseDir, persistenceFilename)
	})
	return persistencePath
}

// SetPersistencePath allows overriding the default persistence path.
func SetPersistencePath(path string) {
	persistencePathOnce.Do(func() {}) // ensure Once is done
	persistencePath = path
}

func loadLegacyAggregatedSnapshot(path string) (AggregatedStatisticsSnapshot, bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return AggregatedStatisticsSnapshot{}, false, nil
	}
	jsonData, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return AggregatedStatisticsSnapshot{}, false, nil
		}
		return AggregatedStatisticsSnapshot{}, false, fmt.Errorf("failed to read legacy persistence file: %w", err)
	}
	if len(jsonData) == 0 {
		return AggregatedStatisticsSnapshot{}, false, nil
	}
	var data persistedData
	if err := json.Unmarshal(jsonData, &data); err != nil {
		return AggregatedStatisticsSnapshot{}, false, fmt.Errorf("failed to unmarshal legacy statistics: %w", err)
	}
	if data.Version > persistenceVersion {
		return AggregatedStatisticsSnapshot{}, false, fmt.Errorf("legacy usage statistics file has newer version (%d > %d)", data.Version, persistenceVersion)
	}
	result := AggregatedStatisticsSnapshot{
		TotalRequests:  data.TotalRequests,
		SuccessCount:   data.SuccessCount,
		FailureCount:   data.FailureCount,
		TotalTokens:    data.TotalTokens,
		APIs:           make(map[string]AggregatedAPISnapshot, len(data.APIs)),
		Breakdowns:     buildLegacyBreakdowns(data),
		RequestsByDay:  make(map[string]int64, len(data.RequestsByDay)),
		RequestsByHour: make(map[string]int64, len(data.RequestsByHour)),
		TokensByDay:    make(map[string]int64, len(data.TokensByDay)),
		TokensByHour:   make(map[string]int64, len(data.TokensByHour)),
	}
	for apiName, api := range data.APIs {
		if api == nil {
			continue
		}
		models := make(map[string]AggregatedModelSnapshot, len(api.Models))
		for modelName, model := range api.Models {
			if model == nil {
				continue
			}
			models[modelName] = AggregatedModelSnapshot{
				TotalRequests: model.TotalRequests,
				TotalTokens:   model.TotalTokens,
			}
		}
		result.APIs[apiName] = AggregatedAPISnapshot{
			TotalRequests: api.TotalRequests,
			TotalTokens:   api.TotalTokens,
			Models:        models,
		}
	}
	for key, value := range data.RequestsByDay {
		result.RequestsByDay[key] = value
	}
	for hour, value := range data.RequestsByHour {
		result.RequestsByHour[formatHour(hour)] = value
	}
	for key, value := range data.TokensByDay {
		result.TokensByDay[key] = value
	}
	for hour, value := range data.TokensByHour {
		result.TokensByHour[formatHour(hour)] = value
	}
	return result, true, nil
}

// Save persists the current statistics to the canonical storage.
func (s *RequestStatistics) Save() error {
	if s == nil {
		return fmt.Errorf("statistics is nil")
	}
	return FlushUsageStatsNow(s)
}

// Load restores statistics from the legacy file into the canonical in-memory schema.
func (s *RequestStatistics) Load() error {
	if s == nil {
		return fmt.Errorf("statistics is nil")
	}
	path := getPersistencePath()
	snapshot, ok, err := loadLegacyAggregatedSnapshot(path)
	if err != nil {
		return err
	}
	if !ok {
		log.Debugf("no existing legacy usage statistics file at %s", path)
		return nil
	}
	s.ReplaceAggregatedSnapshot(snapshot)
	log.Infof("legacy usage statistics loaded from %s (%d requests, %d tokens)", path, snapshot.TotalRequests, snapshot.TotalTokens)
	return nil
}

// StartAutoSave begins a background goroutine that saves statistics periodically.
func (s *RequestStatistics) StartAutoSave() {
	autoSaveMu.Lock()
	defer autoSaveMu.Unlock()

	if autoSaveStop != nil {
		return // already running
	}

	autoSaveStop = make(chan struct{})
	go func() {
		ticker := time.NewTicker(autoSaveInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if err := s.Save(); err != nil {
					log.Errorf("auto-save usage statistics failed: %v", err)
				}
			case <-autoSaveStop:
				// Final save on shutdown
				if err := s.Save(); err != nil {
					log.Errorf("shutdown save usage statistics failed: %v", err)
				} else {
					log.Info("usage statistics saved on shutdown")
				}
				return
			}
		}
	}()

	log.Infof("usage statistics auto-save started (interval: %v)", autoSaveInterval)
}

// StopAutoSave stops the background save goroutine and performs a final save.
func (s *RequestStatistics) StopAutoSave() {
	autoSaveMu.Lock()
	defer autoSaveMu.Unlock()

	if autoSaveStop == nil {
		return // not running
	}

	close(autoSaveStop)
	autoSaveStop = nil
}

// LoadAndStartAutoSave is a convenience function that loads existing data and starts auto-save.
func (s *RequestStatistics) LoadAndStartAutoSave() error {
	if err := s.Load(); err != nil {
		return err
	}
	s.StartAutoSave()
	return nil
}

// CompactOldDetails removes detailed request logs older than detailRetentionDays,
// preserving only aggregated counts and daily summaries.
// This reduces file size while keeping historical data.
func (s *RequestStatistics) CompactOldDetails() (removedCount int) {
	if s == nil {
		return 0
	}

	cutoff := time.Now().AddDate(0, 0, -detailRetentionDays)

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, apiStats := range s.apis {
		for _, modelStats := range apiStats.Models {
			seg1, seg2 := modelStats.orderedDetailSegments()
			if len(seg1) == 0 && len(seg2) == 0 {
				continue
			}

			// Keep only details newer than cutoff
			kept := make([]RequestDetail, 0, len(seg1)+len(seg2))
			for _, detail := range seg1 {
				if detail.Timestamp.After(cutoff) {
					kept = append(kept, detail)
				} else {
					removedCount++
				}
			}
			for _, detail := range seg2 {
				if detail.Timestamp.After(cutoff) {
					kept = append(kept, detail)
				} else {
					removedCount++
				}
			}
			setModelDetails(modelStats, kept)
		}
	}

	if removedCount > 0 {
		log.Infof("compacted %d old request details (older than %d days)", removedCount, detailRetentionDays)
	}
	return removedCount
}

// AccountStats contains aggregated statistics for a single account/source.
type AccountStats struct {
	Source        string    `json:"source"`
	TotalRequests int64     `json:"total_requests"`
	TotalTokens   int64     `json:"total_tokens"`
	SuccessCount  int64     `json:"success_count"`
	FailureCount  int64     `json:"failure_count"`
	LastUsed      time.Time `json:"last_used"`
	InputTokens   int64     `json:"input_tokens"`
	OutputTokens  int64     `json:"output_tokens"`
}

// GetAccountStats returns aggregated statistics per account/email source.
func (s *RequestStatistics) GetAccountStats() []AccountStats {
	if s == nil {
		return nil
	}
	breakdowns := s.SnapshotUsageBreakdowns()
	result := make([]AccountStats, 0, len(breakdowns.BySource))
	for _, bucket := range breakdowns.BySource {
		result = append(result, AccountStats{
			Source:        bucket.Source,
			TotalRequests: bucket.TotalRequests,
			TotalTokens:   bucket.TotalTokens,
			SuccessCount:  bucket.SuccessCount,
			FailureCount:  bucket.FailureCount,
			LastUsed:      bucket.LastUsed,
			InputTokens:   bucket.InputTokens,
			OutputTokens:  bucket.OutputTokens,
		})
	}
	for i := 0; i < len(result)-1; i++ {
		for j := i + 1; j < len(result); j++ {
			if result[j].TotalTokens > result[i].TotalTokens || (result[j].TotalTokens == result[i].TotalTokens && result[j].Source < result[i].Source) {
				result[i], result[j] = result[j], result[i]
			}
		}
	}
	return result
}
