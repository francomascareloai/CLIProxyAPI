package management

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
)

type usageQueueRecord []byte

type usageExportPayload struct {
	Version    int                      `json:"version"`
	ExportedAt time.Time                `json:"exported_at"`
	Usage      usage.StatisticsSnapshot `json:"usage"`
}

type usageCapabilities struct {
	UsageAggregatesV2 bool `json:"usage_aggregates_v2"`
	RollingWindowsV1  bool `json:"rolling_windows_v1"`
}

type usageDetailsRetention struct {
	DetailsEphemeral          bool                     `json:"details_ephemeral"`
	MaxRequestDetailsPerModel int                      `json:"max_request_details_per_model"`
	UsageJournal              usage.UsageJournalStatus `json:"usage_journal"`
}

type usageImportPayload struct {
	Version int                      `json:"version"`
	Usage   usage.StatisticsSnapshot `json:"usage"`
}

type legacyUsageImportPayload struct {
	Version int                    `json:"version"`
	Usage   legacyStatisticsImport `json:"usage"`
}

type legacyStatisticsImport struct {
	TotalRequests  int64                      `json:"total_requests"`
	SuccessCount   int64                      `json:"success_count"`
	FailureCount   int64                      `json:"failure_count"`
	TotalTokens    int64                      `json:"total_tokens"`
	APIs           map[string]legacyAPIImport `json:"apis"`
	RequestsByDay  map[string]int64           `json:"requests_by_day"`
	RequestsByHour map[string]int64           `json:"requests_by_hour"`
	TokensByDay    map[string]int64           `json:"tokens_by_day"`
	TokensByHour   map[string]int64           `json:"tokens_by_hour"`
}

type legacyAPIImport struct {
	TotalRequests int64                        `json:"total_requests"`
	SuccessCount  int64                        `json:"success_count"`
	FailureCount  int64                        `json:"failure_count"`
	TotalTokens   int64                        `json:"total_tokens"`
	InputTokens   int64                        `json:"input_tokens"`
	OutputTokens  int64                        `json:"output_tokens"`
	Models        map[string]legacyModelImport `json:"models"`
}

type legacyModelImport struct {
	TotalRequests int64                `json:"total_requests"`
	SuccessCount  int64                `json:"success_count"`
	FailureCount  int64                `json:"failure_count"`
	TotalTokens   int64                `json:"total_tokens"`
	InputTokens   int64                `json:"input_tokens"`
	OutputTokens  int64                `json:"output_tokens"`
	Details       []legacyDetailImport `json:"details"`
}

type legacyDetailImport struct {
	Timestamp time.Time        `json:"timestamp"`
	Source    string           `json:"source"`
	AuthIndex json.RawMessage  `json:"auth_index"`
	Tokens    usage.TokenStats `json:"tokens"`
	Failed    bool             `json:"failed"`
}

func decodeUsageImportPayload(data []byte) (usageImportPayload, error) {
	var payload usageImportPayload
	if err := json.Unmarshal(data, &payload); err == nil {
		return payload, nil
	}
	var legacy legacyUsageImportPayload
	if err := json.Unmarshal(data, &legacy); err != nil {
		return usageImportPayload{}, err
	}
	return usageImportPayload{Version: legacy.Version, Usage: convertLegacyStatisticsImport(legacy.Usage)}, nil
}

func convertLegacyStatisticsImport(in legacyStatisticsImport) usage.StatisticsSnapshot {
	out := usage.StatisticsSnapshot{
		TotalRequests:  in.TotalRequests,
		SuccessCount:   in.SuccessCount,
		FailureCount:   in.FailureCount,
		TotalTokens:    in.TotalTokens,
		APIs:           make(map[string]usage.APISnapshot, len(in.APIs)),
		RequestsByDay:  in.RequestsByDay,
		RequestsByHour: in.RequestsByHour,
		TokensByDay:    in.TokensByDay,
		TokensByHour:   in.TokensByHour,
	}
	for apiName, api := range in.APIs {
		models := make(map[string]usage.ModelSnapshot, len(api.Models))
		for modelName, model := range api.Models {
			details := make([]usage.RequestDetail, 0, len(model.Details))
			for _, detail := range model.Details {
				details = append(details, usage.RequestDetail{
					Timestamp: detail.Timestamp,
					Source:    detail.Source,
					AuthIndex: decodeLegacyAuthIndex(detail.AuthIndex),
					Tokens:    detail.Tokens,
					Failed:    detail.Failed,
				})
			}
			models[modelName] = usage.ModelSnapshot{
				TotalRequests: model.TotalRequests,
				SuccessCount:  model.SuccessCount,
				FailureCount:  model.FailureCount,
				TotalTokens:   model.TotalTokens,
				InputTokens:   model.InputTokens,
				OutputTokens:  model.OutputTokens,
				Details:       details,
			}
		}
		out.APIs[apiName] = usage.APISnapshot{
			TotalRequests: api.TotalRequests,
			SuccessCount:  api.SuccessCount,
			FailureCount:  api.FailureCount,
			TotalTokens:   api.TotalTokens,
			InputTokens:   api.InputTokens,
			OutputTokens:  api.OutputTokens,
			Models:        models,
		}
	}
	return out
}

func decodeLegacyAuthIndex(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return fmt.Sprintf("%d", n)
	}
	var u uint64
	if err := json.Unmarshal(raw, &u); err == nil {
		return fmt.Sprintf("%d", u)
	}
	return ""
}

// GetUsageStatistics returns the in-memory request statistics snapshot.
func (h *Handler) GetUsageStatistics(c *gin.Context) {
	now := time.Now().UTC()
	var snapshot usage.StatisticsSnapshot
	if h != nil && h.usageStats != nil {
		snapshot = h.usageStats.Snapshot()
	}
	c.JSON(http.StatusOK, gin.H{
		"schema_version": usage.UsageSchemaVersion,
		"capabilities": usageCapabilities{
			UsageAggregatesV2: true,
			RollingWindowsV1:  true,
		},
		"breakdowns": snapshot.Breakdowns,
		"retention": usageDetailsRetention{
			DetailsEphemeral:          true,
			MaxRequestDetailsPerModel: usage.MaxRequestDetailsPerModel(),
			UsageJournal:              usage.GetUsageJournalStatus(now),
		},
		"usage":           snapshot,
		"failed_requests": snapshot.FailureCount,
	})
}

func (r usageQueueRecord) MarshalJSON() ([]byte, error) {
	if json.Valid(r) {
		return append([]byte(nil), r...), nil
	}
	return json.Marshal(string(r))
}

// ExportUsageStatistics returns a complete usage snapshot for backup/migration.
func (h *Handler) ExportUsageStatistics(c *gin.Context) {
	var snapshot usage.StatisticsSnapshot
	if h != nil && h.usageStats != nil {
		snapshot = h.usageStats.Snapshot()
	}
	c.JSON(http.StatusOK, usageExportPayload{
		Version:    usage.UsageSchemaVersion,
		ExportedAt: time.Now().UTC(),
		Usage:      snapshot,
	})
}

// ImportUsageStatistics merges a previously exported usage snapshot into memory.
func (h *Handler) ImportUsageStatistics(c *gin.Context) {
	if h == nil || h.usageStats == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage statistics unavailable"})
		return
	}

	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	payload, err := decodeUsageImportPayload(data)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	if payload.Version != 0 && payload.Version != 1 && payload.Version != 2 && payload.Version != usage.UsageSchemaVersion {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported version"})
		return
	}

	result := h.usageStats.MergeSnapshot(payload.Usage)
	if err := usage.FlushUsageStatsNow(h.usageStats); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "import succeeded but save failed"})
		return
	}
	snapshot := h.usageStats.Snapshot()
	c.JSON(http.StatusOK, gin.H{
		"added":           result.Added,
		"skipped":         result.Skipped,
		"total_requests":  snapshot.TotalRequests,
		"failed_requests": snapshot.FailureCount,
	})
}

// GetUsageQueue pops queued usage records from the usage queue.
func (h *Handler) GetUsageQueue(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	count, errCount := parseUsageQueueCount(c.Query("count"))
	if errCount != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errCount.Error()})
		return
	}

	items := redisqueue.PopOldest(count)
	records := make([]usageQueueRecord, 0, len(items))
	for _, item := range items {
		records = append(records, usageQueueRecord(append([]byte(nil), item...)))
	}
	c.JSON(http.StatusOK, records)
}

func parseUsageQueueCount(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 1, nil
	}
	count, errCount := strconv.Atoi(value)
	if errCount != nil || count <= 0 {
		return 0, errors.New("count must be a positive integer")
	}
	return count, nil
}

// GetAccountStats returns aggregated statistics per account/email.
func (h *Handler) GetAccountStats(c *gin.Context) {
	if h == nil || h.usageStats == nil {
		c.JSON(http.StatusOK, gin.H{"accounts": []usage.AccountStats{}})
		return
	}
	accounts := h.usageStats.GetAccountStats()
	c.JSON(http.StatusOK, gin.H{
		"accounts":       accounts,
		"total_accounts": len(accounts),
	})
}

// CompactUsageData triggers compaction of old request details.
func (h *Handler) CompactUsageData(c *gin.Context) {
	if h == nil || h.usageStats == nil {
		c.JSON(http.StatusOK, gin.H{"removed": 0, "message": "no statistics available"})
		return
	}
	removed := h.usageStats.CompactOldDetails()
	// Flush immediately after compaction to the canonical storage.
	if err := usage.FlushUsageStatsNow(h.usageStats); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "compaction succeeded but save failed",
			"removed": removed,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"removed": removed,
		"message": fmt.Sprintf("compacted %d old request details", removed),
	})
}

// ExportUsageCSV exports usage statistics as CSV.
func (h *Handler) ExportUsageCSV(c *gin.Context) {
	if h == nil || h.usageStats == nil {
		c.String(http.StatusOK, "No data available")
		return
	}

	accounts := h.usageStats.GetAccountStats()

	c.Header("Content-Type", "text/csv")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=usage_stats_%s.csv", time.Now().Format("2006-01-02")))

	writer := csv.NewWriter(c.Writer)
	defer writer.Flush()

	_ = writer.Write([]string{
		"Account/Email",
		"Total Requests",
		"Success Count",
		"Failure Count",
		"Total Tokens",
		"Input Tokens",
		"Output Tokens",
		"Last Used",
	})

	for _, acc := range accounts {
		_ = writer.Write([]string{
			acc.Source,
			fmt.Sprintf("%d", acc.TotalRequests),
			fmt.Sprintf("%d", acc.SuccessCount),
			fmt.Sprintf("%d", acc.FailureCount),
			fmt.Sprintf("%d", acc.TotalTokens),
			fmt.Sprintf("%d", acc.InputTokens),
			fmt.Sprintf("%d", acc.OutputTokens),
			acc.LastUsed.Format(time.RFC3339),
		})
	}
}

// GetCooldownStatus returns the rate limiting status for all auth credentials.
// This shows which accounts are in cooldown and when they'll be available again.
func (h *Handler) GetCooldownStatus(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusOK, gin.H{
			"accounts":       []gin.H{},
			"total_accounts": 0,
			"in_cooldown":    0,
			"available":      0,
		})
		return
	}

	auths := h.authManager.List()
	now := time.Now()

	type accountCooldown struct {
		ID              string    `json:"id"`
		Label           string    `json:"label,omitempty"`
		Provider        string    `json:"provider"`
		Status          string    `json:"status"`
		Disabled        bool      `json:"disabled"`
		Unavailable     bool      `json:"unavailable"`
		QuotaExceeded   bool      `json:"quota_exceeded"`
		QuotaReason     string    `json:"quota_reason,omitempty"`
		NextRecoverAt   time.Time `json:"next_recover_at,omitempty"`
		CooldownSeconds int64     `json:"cooldown_seconds,omitempty"`
		LastError       string    `json:"last_error,omitempty"`
	}

	accounts := make([]accountCooldown, 0, len(auths))
	inCooldown := 0
	available := 0

	for _, auth := range auths {
		acc := accountCooldown{
			ID:            auth.ID,
			Label:         auth.Label,
			Provider:      auth.Provider,
			Status:        string(auth.Status),
			Disabled:      auth.Disabled,
			Unavailable:   auth.Unavailable,
			QuotaExceeded: auth.Quota.Exceeded,
			QuotaReason:   auth.Quota.Reason,
		}

		if auth.LastError != nil {
			acc.LastError = auth.LastError.Message
		}

		if !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) {
			acc.NextRecoverAt = auth.Quota.NextRecoverAt
			acc.CooldownSeconds = int64(auth.Quota.NextRecoverAt.Sub(now).Seconds())
			inCooldown++
		} else if !auth.Disabled && !auth.Unavailable && !auth.Quota.Exceeded {
			available++
		}

		accounts = append(accounts, acc)
	}

	c.JSON(http.StatusOK, gin.H{
		"accounts":       accounts,
		"total_accounts": len(accounts),
		"in_cooldown":    inCooldown,
		"available":      available,
		"checked_at":     now,
	})
}
