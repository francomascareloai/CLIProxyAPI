// Package usage provides usage tracking and logging functionality for the CLI Proxy API server.
package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// Save persists the current statistics to disk.
func (s *RequestStatistics) Save() error {
	if s == nil {
		return fmt.Errorf("statistics is nil")
	}

	s.mu.RLock()
	data := persistedData{
		Version:        persistenceVersion,
		SavedAt:        time.Now(),
		TotalRequests:  s.totalRequests,
		SuccessCount:   s.successCount,
		FailureCount:   s.failureCount,
		TotalTokens:    s.totalTokens,
		APIs:           make(map[string]*persistedAPI, len(s.apis)),
		RequestsByDay:  make(map[string]int64, len(s.requestsByDay)),
		RequestsByHour: make(map[int]int64, len(s.requestsByHour)),
		TokensByDay:    make(map[string]int64, len(s.tokensByDay)),
		TokensByHour:   make(map[int]int64, len(s.tokensByHour)),
	}

	for apiName, stats := range s.apis {
		pAPI := &persistedAPI{
			TotalRequests: stats.TotalRequests,
			TotalTokens:   stats.TotalTokens,
			Models:        make(map[string]*persistedModel, len(stats.Models)),
		}
		for modelName, modelStatsValue := range stats.Models {
			details := make([]RequestDetail, len(modelStatsValue.Details))
			copy(details, modelStatsValue.Details)
			pAPI.Models[modelName] = &persistedModel{
				TotalRequests: modelStatsValue.TotalRequests,
				TotalTokens:   modelStatsValue.TotalTokens,
				Details:       details,
			}
		}
		data.APIs[apiName] = pAPI
	}

	for k, v := range s.requestsByDay {
		data.RequestsByDay[k] = v
	}
	for k, v := range s.requestsByHour {
		data.RequestsByHour[k] = v
	}
	for k, v := range s.tokensByDay {
		data.TokensByDay[k] = v
	}
	for k, v := range s.tokensByHour {
		data.TokensByHour[k] = v
	}
	s.mu.RUnlock()

	path := getPersistencePath()

	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create persistence directory: %w", err)
	}

	// Write to temp file first, then rename (atomic)
	tempPath := path + ".tmp"
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal statistics: %w", err)
	}

	if err := os.WriteFile(tempPath, jsonData, 0644); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	if err := os.Rename(tempPath, path); err != nil {
		os.Remove(tempPath) // cleanup temp file on error
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	log.Debugf("usage statistics saved to %s (%d bytes)", path, len(jsonData))
	return nil
}

// Load restores statistics from disk.
func (s *RequestStatistics) Load() error {
	if s == nil {
		return fmt.Errorf("statistics is nil")
	}

	path := getPersistencePath()
	jsonData, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Debugf("no existing usage statistics file at %s", path)
			return nil // Not an error, just no data yet
		}
		return fmt.Errorf("failed to read persistence file: %w", err)
	}

	var data persistedData
	if err := json.Unmarshal(jsonData, &data); err != nil {
		return fmt.Errorf("failed to unmarshal statistics: %w", err)
	}

	if data.Version > persistenceVersion {
		log.Warnf("usage statistics file has newer version (%d > %d), skipping load", data.Version, persistenceVersion)
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.totalRequests = data.TotalRequests
	s.successCount = data.SuccessCount
	s.failureCount = data.FailureCount
	s.totalTokens = data.TotalTokens

	s.apis = make(map[string]*apiStats, len(data.APIs))
	for apiName, pAPI := range data.APIs {
		stats := &apiStats{
			TotalRequests: pAPI.TotalRequests,
			TotalTokens:   pAPI.TotalTokens,
			Models:        make(map[string]*modelStats, len(pAPI.Models)),
		}
		for modelName, pModel := range pAPI.Models {
			details := make([]RequestDetail, len(pModel.Details))
			copy(details, pModel.Details)
			stats.Models[modelName] = &modelStats{
				TotalRequests: pModel.TotalRequests,
				TotalTokens:   pModel.TotalTokens,
				Details:       details,
			}
		}
		s.apis[apiName] = stats
	}

	s.requestsByDay = make(map[string]int64, len(data.RequestsByDay))
	for k, v := range data.RequestsByDay {
		s.requestsByDay[k] = v
	}

	s.requestsByHour = make(map[int]int64, len(data.RequestsByHour))
	for k, v := range data.RequestsByHour {
		s.requestsByHour[k] = v
	}

	s.tokensByDay = make(map[string]int64, len(data.TokensByDay))
	for k, v := range data.TokensByDay {
		s.tokensByDay[k] = v
	}

	s.tokensByHour = make(map[int]int64, len(data.TokensByHour))
	for k, v := range data.TokensByHour {
		s.tokensByHour[k] = v
	}

	log.Infof("usage statistics loaded from %s (saved at %s, %d requests, %d tokens)",
		path, data.SavedAt.Format(time.RFC3339), s.totalRequests, s.totalTokens)
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
			if len(modelStats.Details) == 0 {
				continue
			}

			// Keep only details newer than cutoff
			kept := make([]RequestDetail, 0, len(modelStats.Details))
			for _, detail := range modelStats.Details {
				if detail.Timestamp.After(cutoff) {
					kept = append(kept, detail)
				} else {
					removedCount++
				}
			}
			modelStats.Details = kept
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

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Aggregate by source (email)
	accountMap := make(map[string]*AccountStats)

	for _, apiStats := range s.apis {
		for _, modelStats := range apiStats.Models {
			for _, detail := range modelStats.Details {
				source := detail.Source
				if source == "" {
					source = "unknown"
				}

				stats, ok := accountMap[source]
				if !ok {
					stats = &AccountStats{Source: source}
					accountMap[source] = stats
				}

				stats.TotalRequests++
				stats.TotalTokens += detail.Tokens.TotalTokens
				stats.InputTokens += detail.Tokens.InputTokens
				stats.OutputTokens += detail.Tokens.OutputTokens
				if detail.Failed {
					stats.FailureCount++
				} else {
					stats.SuccessCount++
				}
				if detail.Timestamp.After(stats.LastUsed) {
					stats.LastUsed = detail.Timestamp
				}
			}
		}
	}

	// Convert to slice and sort by total tokens descending
	result := make([]AccountStats, 0, len(accountMap))
	for _, stats := range accountMap {
		result = append(result, *stats)
	}

	// Sort by TotalTokens descending
	for i := 0; i < len(result)-1; i++ {
		for j := i + 1; j < len(result); j++ {
			if result[j].TotalTokens > result[i].TotalTokens {
				result[i], result[j] = result[j], result[i]
			}
		}
	}

	return result
}
