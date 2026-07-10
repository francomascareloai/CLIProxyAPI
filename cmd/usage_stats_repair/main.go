package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	internalusage "github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

type persistedUsageStats struct {
	Version    int                                        `json:"version"`
	ExportedAt time.Time                                  `json:"exported_at"`
	Usage      internalusage.AggregatedStatisticsSnapshot `json:"usage"`
}

type repairMode string

const (
	modeConservative repairMode = "conservative"
	modeFull         repairMode = "full"
)

type snapshotCandidate struct {
	path    string
	payload persistedUsageStats
}

func main() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fatalf("falha ao resolver HOME: %v", err)
	}

	defaultPath := filepath.Join(homeDir, ".cliproxy", "usage_stats.json")
	pathFlag := flag.String("path", defaultPath, "caminho do usage_stats.json")
	modeFlag := flag.String("mode", string(modeFull), "modo de reparo: conservative|full")
	applyFlag := flag.Bool("apply", false, "gravar resultado no arquivo (sem isso, roda em dry-run)")
	flag.Parse()

	mode := repairMode(strings.ToLower(strings.TrimSpace(*modeFlag)))
	if mode != modeConservative && mode != modeFull {
		fatalf("modo inválido %q (use conservative|full)", mode)
	}

	current, err := loadSnapshot(*pathFlag)
	if err != nil {
		fatalf("falha ao ler snapshot atual: %v", err)
	}

	temps, err := loadTempSnapshots(*pathFlag)
	if err != nil {
		fatalf("falha ao ler snapshots temporários: %v", err)
	}

	before := cloneAggregated(current.Usage)
	after, stats := repairSnapshot(current.Usage, temps, mode)
	after = rebalanceAggregates(before, after)

	printSummary(*pathFlag, before, after, stats, mode, *applyFlag)

	if !*applyFlag {
		return
	}

	if err := backupFile(*pathFlag); err != nil {
		fatalf("falha ao criar backup: %v", err)
	}

	current.Usage = after
	current.ExportedAt = time.Now().UTC()
	if current.Version == 0 {
		current.Version = 1
	}

	if err := writeSnapshot(*pathFlag, current); err != nil {
		fatalf("falha ao gravar arquivo reparado: %v", err)
	}

	fmt.Printf("APPLY concluído: %s\n", *pathFlag)
}

func loadSnapshot(path string) (persistedUsageStats, error) {
	var payload persistedUsageStats
	data, err := os.ReadFile(path)
	if err != nil {
		return payload, err
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return payload, err
	}
	return payload, nil
}

func loadTempSnapshots(mainPath string) ([]snapshotCandidate, error) {
	pattern := mainPath + ".tmp-*"
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)

	out := make([]snapshotCandidate, 0, len(paths))
	for _, path := range paths {
		payload, err := loadSnapshot(path)
		if err != nil {
			continue
		}
		out = append(out, snapshotCandidate{path: path, payload: payload})
	}
	return out, nil
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

func cloneAggregated(in internalusage.AggregatedStatisticsSnapshot) internalusage.AggregatedStatisticsSnapshot {
	out := internalusage.AggregatedStatisticsSnapshot{
		TotalRequests:  in.TotalRequests,
		SuccessCount:   in.SuccessCount,
		FailureCount:   in.FailureCount,
		TotalTokens:    in.TotalTokens,
		APIs:           make(map[string]internalusage.AggregatedAPISnapshot, len(in.APIs)),
		Breakdowns:     in.Breakdowns,
		RequestsByDay:  make(map[string]int64, len(in.RequestsByDay)),
		RequestsByHour: make(map[string]int64, len(in.RequestsByHour)),
		TokensByDay:    make(map[string]int64, len(in.TokensByDay)),
		TokensByHour:   make(map[string]int64, len(in.TokensByHour)),
		RollingState:   cloneRollingState(in.RollingState),
	}
	for k, v := range in.APIs {
		models := make(map[string]internalusage.AggregatedModelSnapshot, len(v.Models))
		for mk, mv := range v.Models {
			models[mk] = mv
		}
		out.APIs[k] = internalusage.AggregatedAPISnapshot{
			TotalRequests:   v.TotalRequests,
			SuccessCount:    v.SuccessCount,
			FailureCount:    v.FailureCount,
			TotalTokens:     v.TotalTokens,
			InputTokens:     v.InputTokens,
			OutputTokens:    v.OutputTokens,
			ReasoningTokens: v.ReasoningTokens,
			CachedTokens:    v.CachedTokens,
			LastUsed:        v.LastUsed,
			Models:          models,
		}
	}
	for k, v := range in.RequestsByDay {
		out.RequestsByDay[k] = v
	}
	for k, v := range in.RequestsByHour {
		out.RequestsByHour[k] = v
	}
	for k, v := range in.TokensByDay {
		out.TokensByDay[k] = v
	}
	for k, v := range in.TokensByHour {
		out.TokensByHour[k] = v
	}
	return out
}

type repairStats struct {
	closedDayReductions int
	heuristicReductions int
	anchorMultiplier    int64
}

func repairSnapshot(
	current internalusage.AggregatedStatisticsSnapshot,
	temps []snapshotCandidate,
	mode repairMode,
) (internalusage.AggregatedStatisticsSnapshot, repairStats) {
	repaired := cloneAggregated(current)
	stats := repairStats{anchorMultiplier: 1}

	days := sortedKeys(repaired.RequestsByDay)
	stageAChanged := make(map[string]bool, len(days))

	// Stage A: for closed days, pick the smallest value seen in older temp snapshots.
	for _, day := range days {
		req, okReq := repaired.RequestsByDay[day]
		tok, okTok := repaired.TokensByDay[day]
		if !okReq || !okTok || req <= 0 || tok <= 0 {
			continue
		}

		minReq := req
		minTok := tok

		endOfDay, err := parseEndOfDay(day)
		if err != nil {
			continue
		}

		for _, cand := range temps {
			if cand.payload.ExportedAt.IsZero() || cand.payload.ExportedAt.Before(endOfDay) {
				continue
			}
			if v, ok := cand.payload.Usage.RequestsByDay[day]; ok && v > 0 && v < minReq {
				minReq = v
			}
			if v, ok := cand.payload.Usage.TokensByDay[day]; ok && v > 0 && v < minTok {
				minTok = v
			}
		}

		if minReq < req || minTok < tok {
			repaired.RequestsByDay[day] = minReq
			repaired.TokensByDay[day] = minTok
			stageAChanged[day] = true
			stats.closedDayReductions++
		}
	}

	if mode == modeConservative {
		return repaired, stats
	}

	// Stage B: infer power-of-two inflation and deflate newer days monotonically.
	for _, day := range days {
		if !stageAChanged[day] {
			continue
		}
		origReq := current.RequestsByDay[day]
		origTok := current.TokensByDay[day]
		newReq := repaired.RequestsByDay[day]
		newTok := repaired.TokensByDay[day]
		if newReq <= 0 || newTok <= 0 {
			continue
		}
		if origReq%newReq != 0 || origTok%newTok != 0 {
			continue
		}
		rr := origReq / newReq
		rt := origTok / newTok
		if rr != rt || rr < 2 || !isPowerOfTwo(rr) {
			continue
		}
		if rr > stats.anchorMultiplier {
			stats.anchorMultiplier = rr
		}
	}

	if stats.anchorMultiplier < 2 {
		return repaired, stats
	}

	prevMultiplier := stats.anchorMultiplier
	for _, day := range days {
		if stageAChanged[day] {
			origReq := current.RequestsByDay[day]
			origTok := current.TokensByDay[day]
			newReq := repaired.RequestsByDay[day]
			newTok := repaired.TokensByDay[day]
			if newReq > 0 && newTok > 0 && origReq%newReq == 0 && origTok%newTok == 0 {
				rr := origReq / newReq
				rt := origTok / newTok
				if rr == rt && rr >= 2 && isPowerOfTwo(rr) && rr < prevMultiplier {
					prevMultiplier = rr
				}
			}
			continue
		}

		req := repaired.RequestsByDay[day]
		tok := repaired.TokensByDay[day]
		if req <= 0 || tok <= 0 {
			continue
		}
		limit := minInt64(maxPowerOfTwoDivisor(req), maxPowerOfTwoDivisor(tok))
		inferred := minInt64(prevMultiplier, limit)
		if inferred < 2 {
			continue
		}
		repaired.RequestsByDay[day] = req / inferred
		repaired.TokensByDay[day] = tok / inferred
		prevMultiplier = inferred
		stats.heuristicReductions++
	}

	return repaired, stats
}

func rebalanceAggregates(
	before internalusage.AggregatedStatisticsSnapshot,
	after internalusage.AggregatedStatisticsSnapshot,
) internalusage.AggregatedStatisticsSnapshot {
	totalReq := sumMap(after.RequestsByDay)
	totalTok := sumMap(after.TokensByDay)
	after.TotalRequests = totalReq
	after.TotalTokens = totalTok

	oldReq := before.TotalRequests
	oldTok := before.TotalTokens

	if oldReq > 0 {
		failRate := float64(before.FailureCount) / float64(oldReq)
		fail := int64(math.Round(float64(totalReq) * failRate))
		if fail < 0 {
			fail = 0
		}
		if fail > totalReq {
			fail = totalReq
		}
		after.FailureCount = fail
		after.SuccessCount = totalReq - fail
	} else {
		after.FailureCount = 0
		after.SuccessCount = totalReq
	}

	reqScale := scaleFactor(oldReq, totalReq)
	tokScale := scaleFactor(oldTok, totalTok)
	after.RequestsByHour = scaleMap(after.RequestsByHour, reqScale)
	after.TokensByHour = scaleMap(after.TokensByHour, tokScale)

	for api, apiStats := range after.APIs {
		apiStats.TotalRequests = scaleValue(apiStats.TotalRequests, reqScale)
		apiStats.TotalTokens = scaleValue(apiStats.TotalTokens, tokScale)
		if apiStats.Models == nil {
			apiStats.Models = make(map[string]internalusage.AggregatedModelSnapshot)
		}
		for model, modelStats := range apiStats.Models {
			modelStats.TotalRequests = scaleValue(modelStats.TotalRequests, reqScale)
			modelStats.TotalTokens = scaleValue(modelStats.TotalTokens, tokScale)
			apiStats.Models[model] = modelStats
		}
		after.APIs[api] = apiStats
	}

	return after
}

func scaleFactor(oldTotal, newTotal int64) float64 {
	if oldTotal <= 0 || newTotal < 0 {
		return 1.0
	}
	return float64(newTotal) / float64(oldTotal)
}

func scaleMap(in map[string]int64, factor float64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = scaleValue(v, factor)
	}
	return out
}

func scaleValue(v int64, factor float64) int64 {
	if v <= 0 {
		return 0
	}
	scaled := int64(math.Round(float64(v) * factor))
	if scaled < 0 {
		return 0
	}
	return scaled
}

func parseEndOfDay(day string) (time.Time, error) {
	day = strings.TrimSpace(day)
	if day == "" {
		return time.Time{}, errors.New("empty day")
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", day+" 23:59:59", time.Local)
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}

func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sumMap(m map[string]int64) int64 {
	var total int64
	for _, v := range m {
		total += v
	}
	return total
}

func maxPowerOfTwoDivisor(v int64) int64 {
	if v <= 0 {
		return 1
	}
	p := int64(1)
	for v%2 == 0 {
		p *= 2
		v /= 2
	}
	return p
}

func isPowerOfTwo(v int64) bool {
	return v > 0 && (v&(v-1)) == 0
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func backupFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	backupPath := fmt.Sprintf("%s.pre_repair.%s.bak", path, time.Now().Format("20060102_150405"))
	if err := os.WriteFile(backupPath, data, 0o600); err != nil {
		return err
	}
	fmt.Printf("backup criado: %s\n", backupPath)
	return nil
}

func writeSnapshot(path string, payload persistedUsageStats) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmpPath := path + ".tmp-repair"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func printSummary(
	path string,
	before internalusage.AggregatedStatisticsSnapshot,
	after internalusage.AggregatedStatisticsSnapshot,
	stats repairStats,
	mode repairMode,
	apply bool,
) {
	fmt.Printf("arquivo: %s\n", path)
	fmt.Printf("modo: %s | apply: %v\n", mode, apply)
	fmt.Printf("reduções stageA (dias fechados): %d\n", stats.closedDayReductions)
	fmt.Printf("reduções stageB (heurística): %d\n", stats.heuristicReductions)
	fmt.Printf("multiplicador âncora: %dx\n", stats.anchorMultiplier)
	fmt.Printf("requests: %d -> %d\n", before.TotalRequests, after.TotalRequests)
	fmt.Printf("tokens:   %d -> %d\n", before.TotalTokens, after.TotalTokens)
}

func fatalf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
