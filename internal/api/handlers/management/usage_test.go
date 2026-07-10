package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestGetUsageStatistics_ExposesUsageAggregatesV2(t *testing.T) {
	gin.SetMode(gin.TestMode)
	usage.ConfigureUsageRuntime(true, 180, 8)
	t.Cleanup(func() { usage.ConfigureUsageRuntime(false, 180, 8) })
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
			UsageJournal              struct {
				Enabled             bool   `json:"enabled"`
				AppendOnly          bool   `json:"append_only"`
				RetentionDays       int    `json:"retention_days"`
				ReplayMaxDays       int    `json:"replay_max_days"`
				BackfillStatus      string `json:"backfill_status"`
				LastReplaySource    string `json:"last_replay_source"`
				CoverageStart       string `json:"coverage_start"`
				CoverageEnd         string `json:"coverage_end"`
				LastFinalizedMinute string `json:"last_finalized_minute"`
			} `json:"usage_journal"`
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
	if !payload.Retention.UsageJournal.Enabled || payload.Retention.UsageJournal.RetentionDays <= 0 {
		t.Fatalf("expected usage journal status in retention payload")
	}
	if !payload.Retention.UsageJournal.AppendOnly || payload.Retention.UsageJournal.ReplayMaxDays < 8 {
		t.Fatalf("expected append-only replay telemetry in retention payload: %+v", payload.Retention.UsageJournal)
	}
	if payload.Retention.UsageJournal.BackfillStatus == "" || payload.Retention.UsageJournal.LastFinalizedMinute == "" {
		t.Fatalf("expected backfill telemetry in retention payload: %+v", payload.Retention.UsageJournal)
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
	usage.ConfigureUsageRuntime(true, 180, 8)
	t.Cleanup(func() { usage.ConfigureUsageRuntime(false, 180, 8) })
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
	stats.Record(nil, coreusage.Record{
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
	usage.ConfigureUsageRuntime(true, 180, 8)
	t.Cleanup(func() { usage.ConfigureUsageRuntime(false, 180, 8) })
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

func TestGetUsageQueuePopsRequestedRecords(t *testing.T) {
	withManagementUsageQueue(t, func() {
		redisqueue.Enqueue([]byte(`{"id":1}`))
		redisqueue.Enqueue([]byte(`{"id":2}`))
		redisqueue.Enqueue([]byte(`{"id":3}`))

		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-queue?count=2", nil)

		h := &Handler{}
		h.GetUsageQueue(ginCtx)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
		}

		var payload []json.RawMessage
		if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
			t.Fatalf("unmarshal response: %v", errUnmarshal)
		}
		if len(payload) != 2 {
			t.Fatalf("response records = %d, want 2", len(payload))
		}
		requireRecordID(t, payload[0], 1)
		requireRecordID(t, payload[1], 2)

		remaining := redisqueue.PopOldest(10)
		if len(remaining) != 1 || string(remaining[0]) != `{"id":3}` {
			t.Fatalf("remaining queue = %q, want third item only", remaining)
		}
	})
}

func TestGetUsageQueueInvalidCountDoesNotPop(t *testing.T) {
	withManagementUsageQueue(t, func() {
		redisqueue.Enqueue([]byte(`{"id":1}`))

		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-queue?count=0", nil)

		h := &Handler{}
		h.GetUsageQueue(ginCtx)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}

		remaining := redisqueue.PopOldest(10)
		if len(remaining) != 1 || string(remaining[0]) != `{"id":1}` {
			t.Fatalf("remaining queue = %q, want original item", remaining)
		}
	})
}

func withManagementUsageQueue(t *testing.T, fn func()) {
	t.Helper()

	prevQueueEnabled := redisqueue.Enabled()
	redisqueue.SetEnabled(false)
	redisqueue.SetEnabled(true)

	defer func() {
		redisqueue.SetEnabled(false)
		redisqueue.SetEnabled(prevQueueEnabled)
	}()

	fn()
}

func requireRecordID(t *testing.T, raw json.RawMessage, want int) {
	t.Helper()

	var payload struct {
		ID int `json:"id"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal record: %v", errUnmarshal)
	}
	if payload.ID != want {
		t.Fatalf("record id = %d, want %d", payload.ID, want)
	}
}
