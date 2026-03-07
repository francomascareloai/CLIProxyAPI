package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestGetUsageStatistics_ExposesUsageAggregatesV2(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stats := usage.NewRequestStatistics()
	stats.Record(nil, coreusage.Record{
		Provider:    "claude",
		Model:       "claude-3-5-sonnet",
		APIKey:      "sk-test-usage-v2",
		Source:      "user@example.com",
		AuthIndex:   "auth-1",
		RequestedAt: time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC),
		Detail: coreusage.Detail{
			InputTokens:     30,
			OutputTokens:    50,
			ReasoningTokens: 5,
			CachedTokens:    5,
			TotalTokens:     90,
		},
	})

	h := &Handler{usageStats: stats}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	h.GetUsageStatistics(c)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", w.Code)
	}

	var payload struct {
		SchemaVersion int `json:"schema_version"`
		Capabilities  struct {
			UsageAggregatesV2 bool `json:"usage_aggregates_v2"`
			RollingWindowsV1  bool `json:"rolling_windows_v1"`
		} `json:"capabilities"`
		Breakdowns struct {
			BySource []map[string]any `json:"by_source"`
		} `json:"breakdowns"`
		Retention struct {
			DetailsEphemeral          bool `json:"details_ephemeral"`
			MaxRequestDetailsPerModel int  `json:"max_request_details_per_model"`
		} `json:"retention"`
		Usage struct {
			TotalRequests int `json:"total_requests"`
			Rolling       struct {
				Available         bool   `json:"available"`
				ResolutionMinutes int    `json:"resolution_minutes"`
				Integrity         string `json:"integrity"`
				Windows           map[string]struct {
					Requests       int64  `json:"requests"`
					Available      bool   `json:"available"`
					Integrity      string `json:"integrity"`
					DegradedReason string `json:"degraded_reason"`
				} `json:"windows"`
			} `json:"rolling"`
			Breakdowns struct {
				ByAuthIndex []map[string]any `json:"by_auth_index"`
			} `json:"breakdowns"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.SchemaVersion != usage.UsageSchemaVersion {
		t.Fatalf("unexpected schema version: %d", payload.SchemaVersion)
	}
	if !payload.Capabilities.UsageAggregatesV2 {
		t.Fatalf("expected usage_aggregates_v2 capability")
	}
	if !payload.Capabilities.RollingWindowsV1 {
		t.Fatalf("expected rolling_windows_v1 capability")
	}
	if !payload.Retention.DetailsEphemeral {
		t.Fatalf("expected details_ephemeral=true")
	}
	if payload.Retention.MaxRequestDetailsPerModel != usage.MaxRequestDetailsPerModel() {
		t.Fatalf("unexpected max_request_details_per_model: %d", payload.Retention.MaxRequestDetailsPerModel)
	}
	if payload.Usage.TotalRequests != 1 {
		t.Fatalf("unexpected total requests: %d", payload.Usage.TotalRequests)
	}
	if payload.Usage.Rolling.ResolutionMinutes != 1 {
		t.Fatalf("unexpected rolling resolution: %d", payload.Usage.Rolling.ResolutionMinutes)
	}
	if payload.Usage.Rolling.Windows["7h"].Available {
		t.Fatalf("expected 7h rolling unavailable while warming")
	}
	if payload.Usage.Rolling.Integrity == "" {
		t.Fatalf("expected top-level rolling integrity marker")
	}
	if len(payload.Breakdowns.BySource) != 1 {
		t.Fatalf("expected by_source breakdown")
	}
	if len(payload.Usage.Breakdowns.ByAuthIndex) != 1 {
		t.Fatalf("expected nested by_auth_index breakdown")
	}
}

func TestGetAccountStats_UsesDurableBreakdownsWithoutDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	seed := usage.NewRequestStatistics()
	seed.Record(nil, coreusage.Record{
		Provider:    "claude",
		Model:       "claude-3-5-sonnet",
		APIKey:      "sk-test-durable",
		Source:      "durable@example.com",
		AuthIndex:   "auth-durable",
		RequestedAt: time.Date(2026, 3, 6, 13, 0, 0, 0, time.UTC),
		Detail: coreusage.Detail{
			InputTokens:  20,
			OutputTokens: 22,
			TotalTokens:  42,
		},
	})
	stats := usage.NewRequestStatistics()
	stats.ReplaceAggregatedSnapshot(seed.SnapshotAggregated())

	h := &Handler{usageStats: stats}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	h.GetAccountStats(c)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", w.Code)
	}

	var payload struct {
		Accounts []usage.AccountStats `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(payload.Accounts) != 1 {
		t.Fatalf("expected one account, got %d", len(payload.Accounts))
	}
	if payload.Accounts[0].Source != "durable@example.com" {
		t.Fatalf("unexpected source: %s", payload.Accounts[0].Source)
	}
	if payload.Accounts[0].TotalTokens != 42 {
		t.Fatalf("unexpected total tokens: %d", payload.Accounts[0].TotalTokens)
	}
}

func TestGetUsageStatistics_ExposesHourAndDayAggregates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stats := usage.NewRequestStatistics()
	stats.Record(nil, coreusage.Record{
		Provider:    "claude",
		Model:       "claude-3-5-sonnet",
		APIKey:      "sk-test-aggregate-shape",
		RequestedAt: time.Date(2026, 3, 7, 15, 0, 0, 0, time.UTC),
		Detail: coreusage.Detail{
			InputTokens:  40,
			OutputTokens: 2,
			TotalTokens:  42,
		},
	})

	h := &Handler{usageStats: stats}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	h.GetUsageStatistics(c)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", w.Code)
	}

	var payload struct {
		Usage struct {
			RequestsByDay  map[string]int64 `json:"requests_by_day"`
			RequestsByHour map[string]int64 `json:"requests_by_hour"`
			TokensByDay    map[string]int64 `json:"tokens_by_day"`
			TokensByHour   map[string]int64 `json:"tokens_by_hour"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Usage.RequestsByDay["2026-03-07"] != 1 {
		t.Fatalf("unexpected requests_by_day: %+v", payload.Usage.RequestsByDay)
	}
	if payload.Usage.RequestsByHour["15"] != 1 {
		t.Fatalf("unexpected requests_by_hour: %+v", payload.Usage.RequestsByHour)
	}
	if payload.Usage.TokensByDay["2026-03-07"] != 42 {
		t.Fatalf("unexpected tokens_by_day: %+v", payload.Usage.TokensByDay)
	}
	if payload.Usage.TokensByHour["15"] != 42 {
		t.Fatalf("unexpected tokens_by_hour: %+v", payload.Usage.TokensByHour)
	}
}
