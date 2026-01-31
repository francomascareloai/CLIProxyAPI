// Command import-usage converts a usage statistics backup (from GET /v0/management/usage)
// to the persistence file format used by the CLIProxyAPI.
//
// Usage: go run cmd/import-usage/main.go <backup.json> [output-path]
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Input format (from API)
type apiResponse struct {
	FailedRequests int64 `json:"failed_requests"`
	Usage          struct {
		TotalRequests  int64                    `json:"total_requests"`
		SuccessCount   int64                    `json:"success_count"`
		FailureCount   int64                    `json:"failure_count"`
		TotalTokens    int64                    `json:"total_tokens"`
		APIs           map[string]apiAPIStats   `json:"apis"`
		RequestsByDay  map[string]int64         `json:"requests_by_day"`
		RequestsByHour map[string]int64         `json:"requests_by_hour"`
		TokensByDay    map[string]int64         `json:"tokens_by_day"`
		TokensByHour   map[string]int64         `json:"tokens_by_hour"`
	} `json:"usage"`
}

type apiAPIStats struct {
	TotalRequests int64                    `json:"total_requests"`
	TotalTokens   int64                    `json:"total_tokens"`
	Models        map[string]apiModelStats `json:"models"`
}

type apiModelStats struct {
	TotalRequests int64           `json:"total_requests"`
	TotalTokens   int64           `json:"total_tokens"`
	Details       []requestDetail `json:"details"`
}

type requestDetail struct {
	Timestamp time.Time  `json:"timestamp"`
	Source    string     `json:"source"`
	AuthIndex uint64     `json:"auth_index"`
	Tokens    tokenStats `json:"tokens"`
	Failed    bool       `json:"failed"`
}

type tokenStats struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	CachedTokens    int64 `json:"cached_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
}

// Output format (persistence)
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
	Details       []requestDetail `json:"details"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run cmd/import-usage/main.go <backup.json> [output-path]")
		fmt.Println("")
		fmt.Println("If output-path is not specified, defaults to ~/.cli-proxy-api/usage_statistics.json")
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
		outputPath = filepath.Join(homeDir, ".cli-proxy-api", "usage_statistics.json")
	}

	// Read input
	inputData, err := os.ReadFile(inputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading input file: %v\n", err)
		os.Exit(1)
	}

	var apiResp apiResponse
	if err := json.Unmarshal(inputData, &apiResp); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing input JSON: %v\n", err)
		os.Exit(1)
	}

	// Convert to persistence format
	output := persistedData{
		Version:        1,
		SavedAt:        time.Now(),
		TotalRequests:  apiResp.Usage.TotalRequests,
		SuccessCount:   apiResp.Usage.SuccessCount,
		FailureCount:   apiResp.Usage.FailureCount,
		TotalTokens:    apiResp.Usage.TotalTokens,
		APIs:           make(map[string]*persistedAPI),
		RequestsByDay:  apiResp.Usage.RequestsByDay,
		RequestsByHour: make(map[int]int64),
		TokensByDay:    apiResp.Usage.TokensByDay,
		TokensByHour:   make(map[int]int64),
	}

	// Convert string hour keys to int
	for hourStr, count := range apiResp.Usage.RequestsByHour {
		var hour int
		fmt.Sscanf(hourStr, "%d", &hour)
		output.RequestsByHour[hour] = count
	}
	for hourStr, tokens := range apiResp.Usage.TokensByHour {
		var hour int
		fmt.Sscanf(hourStr, "%d", &hour)
		output.TokensByHour[hour] = tokens
	}

	// Convert APIs
	for apiKey, apiStats := range apiResp.Usage.APIs {
		pAPI := &persistedAPI{
			TotalRequests: apiStats.TotalRequests,
			TotalTokens:   apiStats.TotalTokens,
			Models:        make(map[string]*persistedModel),
		}
		for modelName, modelStats := range apiStats.Models {
			pAPI.Models[modelName] = &persistedModel{
				TotalRequests: modelStats.TotalRequests,
				TotalTokens:   modelStats.TotalTokens,
				Details:       modelStats.Details,
			}
		}
		output.APIs[apiKey] = pAPI
	}

	// Ensure output directory exists
	outputDir := filepath.Dir(outputPath)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating output directory: %v\n", err)
		os.Exit(1)
	}

	// Write output
	outputData, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling output: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(outputPath, outputData, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing output file: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Successfully converted usage statistics\n")
	fmt.Printf("  Input:  %s\n", inputPath)
	fmt.Printf("  Output: %s\n", outputPath)
	fmt.Printf("  Total requests: %d\n", output.TotalRequests)
	fmt.Printf("  Total tokens:   %d\n", output.TotalTokens)
	fmt.Printf("  Success count:  %d\n", output.SuccessCount)
	fmt.Printf("  Failure count:  %d\n", output.FailureCount)
}
