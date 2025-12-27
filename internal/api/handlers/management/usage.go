package management

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

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

// GetAccountStats returns aggregated statistics per account/email.
func (h *Handler) GetAccountStats(c *gin.Context) {
	if h == nil || h.usageStats == nil {
		c.JSON(http.StatusOK, gin.H{"accounts": []usage.AccountStats{}})
		return
	}
	accounts := h.usageStats.GetAccountStats()
	c.JSON(http.StatusOK, gin.H{
		"accounts":      accounts,
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

	c.Header("Content-Type", "text/csv")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=usage_stats_%s.csv", time.Now().Format("2006-01-02")))

	writer := csv.NewWriter(c.Writer)
	defer writer.Flush()

	// Header
	writer.Write([]string{
		"Account/Email",
		"Total Requests",
		"Success Count",
		"Failure Count",
		"Total Tokens",
		"Input Tokens",
		"Output Tokens",
		"Last Used",
	})

	// Data rows
	for _, acc := range accounts {
		writer.Write([]string{
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
