package management

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

type usageExportPayload struct {
	Version    int                      `json:"version"`
	ExportedAt time.Time                `json:"exported_at"`
	Usage      usage.StatisticsSnapshot `json:"usage"`
}

type usageImportPayload struct {
	Version int                      `json:"version"`
	Usage   usage.StatisticsSnapshot `json:"usage"`
}

// GetUsageStatistics returns the in-memory request statistics snapshot.
func (h *Handler) GetUsageStatistics(c *gin.Context) {
	var snapshot usage.StatisticsSnapshot
	if h != nil && h.usageStats != nil {
		snapshot = h.usageStats.Snapshot()
	}
	c.JSON(http.StatusOK, gin.H{
		"usage":           snapshot,
		"failed_requests": snapshot.FailureCount,
	})
}

// ExportUsageStatistics returns a complete usage snapshot for backup/migration.
func (h *Handler) ExportUsageStatistics(c *gin.Context) {
	var snapshot usage.StatisticsSnapshot
	if h != nil && h.usageStats != nil {
		snapshot = h.usageStats.Snapshot()
	}
	c.JSON(http.StatusOK, usageExportPayload{
		Version:    1,
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

	var payload usageImportPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	if payload.Version != 0 && payload.Version != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported version"})
		return
	}

	result := h.usageStats.MergeSnapshot(payload.Usage)
	snapshot := h.usageStats.Snapshot()
	c.JSON(http.StatusOK, gin.H{
		"added":           result.Added,
		"skipped":         result.Skipped,
		"total_requests":  snapshot.TotalRequests,
		"failed_requests": snapshot.FailureCount,
	})
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
	// Save immediately after compaction
	if err := h.usageStats.Save(); err != nil {
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

	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)

	// Header
	if err := writer.Write([]string{
		"Account/Email",
		"Total Requests",
		"Success Count",
		"Failure Count",
		"Total Tokens",
		"Input Tokens",
		"Output Tokens",
		"Last Used",
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to write csv header"})
		return
	}

	// Data rows
	for _, acc := range accounts {
		if err := writer.Write([]string{
			acc.Source,
			fmt.Sprintf("%d", acc.TotalRequests),
			fmt.Sprintf("%d", acc.SuccessCount),
			fmt.Sprintf("%d", acc.FailureCount),
			fmt.Sprintf("%d", acc.TotalTokens),
			fmt.Sprintf("%d", acc.InputTokens),
			fmt.Sprintf("%d", acc.OutputTokens),
			acc.LastUsed.Format(time.RFC3339),
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to write csv row"})
			return
		}
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to flush csv"})
		return
	}

	c.Header("Content-Type", "text/csv")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=usage_stats_%s.csv", time.Now().UTC().Format("2006-01-02")))
	c.Data(http.StatusOK, "text/csv", buf.Bytes())
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
	now := time.Now().UTC()

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

		// Check if in cooldown
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
