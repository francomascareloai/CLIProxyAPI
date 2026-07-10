// Command import-usage converts a usage statistics backup (from GET /v0/management/usage)
// to the canonical persistence file format used by the CLIProxyAPI.
//
// Usage: go run cmd/import-usage/main.go <backup.json> [output-path]
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	internalusage "github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
)

type usageImportPayload struct {
	Version int                              `json:"version"`
	Usage   internalusage.StatisticsSnapshot `json:"usage"`
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
	Timestamp time.Time                `json:"timestamp"`
	Source    string                   `json:"source"`
	AuthIndex json.RawMessage          `json:"auth_index"`
	Tokens    internalusage.TokenStats `json:"tokens"`
	Failed    bool                     `json:"failed"`
}

type persistedUsageStats struct {
	Version    int                                        `json:"version"`
	ExportedAt time.Time                                  `json:"exported_at"`
	Usage      internalusage.AggregatedStatisticsSnapshot `json:"usage"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run cmd/import-usage/main.go <backup.json> [output-path]")
		fmt.Println("")
		fmt.Println("If output-path is not specified, defaults to ~/.cliproxy/usage_stats.json")
		os.Exit(1)
	}

	inputPath := os.Args[1]
	outputPath := ""
	if len(os.Args) >= 3 {
		outputPath = os.Args[2]
	} else {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting home directory: %v\n", err)
			os.Exit(1)
		}
		outputPath = filepath.Join(homeDir, ".cliproxy", "usage_stats.json")
	}

	inputData, err := os.ReadFile(inputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading input file: %v\n", err)
		os.Exit(1)
	}

	payload, err := decodeUsageImportPayload(inputData)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing input JSON: %v\n", err)
		os.Exit(1)
	}

	output := persistedUsageStats{
		Version:    internalusage.UsageSchemaVersion,
		ExportedAt: time.Now().UTC(),
		Usage:      aggregatedSnapshotFromStatistics(payload.Usage),
	}

	outputDir := filepath.Dir(outputPath)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating output directory: %v\n", err)
		os.Exit(1)
	}

	outputData, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling output: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(outputPath, outputData, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing output file: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Successfully converted usage statistics\n")
	fmt.Printf("  Input:  %s\n", inputPath)
	fmt.Printf("  Output: %s\n", outputPath)
	fmt.Printf("  Total requests: %d\n", output.Usage.TotalRequests)
	fmt.Printf("  Total tokens:   %d\n", output.Usage.TotalTokens)
	fmt.Printf("  Success count:  %d\n", output.Usage.SuccessCount)
	fmt.Printf("  Failure count:  %d\n", output.Usage.FailureCount)
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

func convertLegacyStatisticsImport(in legacyStatisticsImport) internalusage.StatisticsSnapshot {
	out := internalusage.StatisticsSnapshot{
		TotalRequests:  in.TotalRequests,
		SuccessCount:   in.SuccessCount,
		FailureCount:   in.FailureCount,
		TotalTokens:    in.TotalTokens,
		APIs:           make(map[string]internalusage.APISnapshot, len(in.APIs)),
		RequestsByDay:  in.RequestsByDay,
		RequestsByHour: in.RequestsByHour,
		TokensByDay:    in.TokensByDay,
		TokensByHour:   in.TokensByHour,
	}
	for apiName, api := range in.APIs {
		models := make(map[string]internalusage.ModelSnapshot, len(api.Models))
		for modelName, model := range api.Models {
			details := make([]internalusage.RequestDetail, 0, len(model.Details))
			for _, detail := range model.Details {
				details = append(details, internalusage.RequestDetail{
					Timestamp: detail.Timestamp,
					Source:    detail.Source,
					AuthIndex: decodeLegacyAuthIndex(detail.AuthIndex),
					Tokens:    detail.Tokens,
					Failed:    detail.Failed,
				})
			}
			models[modelName] = internalusage.ModelSnapshot{
				TotalRequests: model.TotalRequests,
				SuccessCount:  model.SuccessCount,
				FailureCount:  model.FailureCount,
				TotalTokens:   model.TotalTokens,
				InputTokens:   model.InputTokens,
				OutputTokens:  model.OutputTokens,
				Details:       details,
			}
		}
		out.APIs[apiName] = internalusage.APISnapshot{
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

func cloneRollingState(snapshot internalusage.RollingStateSnapshot) internalusage.RollingStateSnapshot {
	result := internalusage.RollingStateSnapshot{
		CoverageStart: snapshot.CoverageStart,
		CoverageEnd:   snapshot.CoverageEnd,
	}
	if len(snapshot.MinuteBuckets) > 0 {
		result.MinuteBuckets = make(map[string]internalusage.RollingMinuteBucket, len(snapshot.MinuteBuckets))
		for minute, bucket := range snapshot.MinuteBuckets {
			result.MinuteBuckets[minute] = bucket
		}
	}
	return result
}

func aggregatedSnapshotFromStatistics(snapshot internalusage.StatisticsSnapshot) internalusage.AggregatedStatisticsSnapshot {
	result := internalusage.AggregatedStatisticsSnapshot{
		TotalRequests:  snapshot.TotalRequests,
		SuccessCount:   snapshot.SuccessCount,
		FailureCount:   snapshot.FailureCount,
		TotalTokens:    snapshot.TotalTokens,
		APIs:           make(map[string]internalusage.AggregatedAPISnapshot, len(snapshot.APIs)),
		Breakdowns:     snapshot.Breakdowns,
		RequestsByDay:  make(map[string]int64, len(snapshot.RequestsByDay)),
		RequestsByHour: make(map[string]int64, len(snapshot.RequestsByHour)),
		TokensByDay:    make(map[string]int64, len(snapshot.TokensByDay)),
		TokensByHour:   make(map[string]int64, len(snapshot.TokensByHour)),
		RollingState:   cloneRollingState(snapshot.RollingState),
	}
	for apiName, api := range snapshot.APIs {
		models := make(map[string]internalusage.AggregatedModelSnapshot, len(api.Models))
		for modelName, model := range api.Models {
			models[modelName] = internalusage.AggregatedModelSnapshot{
				TotalRequests: model.TotalRequests,
				SuccessCount:  model.SuccessCount,
				FailureCount:  model.FailureCount,
				TotalTokens:   model.TotalTokens,
				InputTokens:   model.InputTokens,
				OutputTokens:  model.OutputTokens,
			}
		}
		result.APIs[apiName] = internalusage.AggregatedAPISnapshot{
			TotalRequests: api.TotalRequests,
			SuccessCount:  api.SuccessCount,
			FailureCount:  api.FailureCount,
			TotalTokens:   api.TotalTokens,
			InputTokens:   api.InputTokens,
			OutputTokens:  api.OutputTokens,
			Models:        models,
		}
	}
	for k, v := range snapshot.RequestsByDay {
		result.RequestsByDay[k] = v
	}
	for k, v := range snapshot.RequestsByHour {
		result.RequestsByHour[k] = v
	}
	for k, v := range snapshot.TokensByDay {
		result.TokensByDay[k] = v
	}
	for k, v := range snapshot.TokensByHour {
		result.TokensByHour[k] = v
	}
	return result
}
