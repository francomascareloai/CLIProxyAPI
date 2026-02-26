package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const usagePersistenceVersion = 1

type persistedUsageStats struct {
	Version    int                          `json:"version"`
	ExportedAt time.Time                    `json:"exported_at"`
	Usage      AggregatedStatisticsSnapshot `json:"usage"`
}

// UsagePersister periodically flushes aggregated usage statistics to disk.
//
// It never writes request details, prompts/responses, or plaintext API keys.
type UsagePersister struct {
	stats    *RequestStatistics
	interval time.Duration

	mu           sync.Mutex
	running      bool
	writesActive bool
	cancel       context.CancelFunc
	done         chan struct{}
}

var defaultUsagePersister = &UsagePersister{
	stats:    defaultRequestStatistics,
	interval: 30 * time.Second,
}

// StartUsagePersister starts the default usage persister.
func StartUsagePersister(ctx context.Context) {
	defaultUsagePersister.Start(ctx)
}

// StopUsagePersister stops the default usage persister and attempts a final flush.
func StopUsagePersister(ctx context.Context) {
	defaultUsagePersister.Stop(ctx)
}

func (p *UsagePersister) Start(ctx context.Context) {
	if p == nil || p.stats == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	p.running = true
	p.writesActive = false
	p.done = make(chan struct{})
	workerCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	done := p.done
	p.mu.Unlock()

	if err := p.loadIntoStore(); err != nil {
		log.Warnf("usage: failed to load persisted stats: %v", err)
	}

	if _, err := ensureUsageHMACKey(); err != nil {
		// Fail closed: without a stable HMAC secret we must not persist client identifiers.
		log.Warnf("usage: disabling stats persistence (hmac key unavailable): %v", err)
		p.mu.Lock()
		p.running = false
		p.writesActive = false
		p.cancel = nil
		p.done = nil
		p.mu.Unlock()
		cancel()
		if done != nil {
			close(done)
		}
		return
	}

	select {
	case <-workerCtx.Done():
		// Start was cancelled (likely via Stop()) before the worker was launched.
		p.mu.Lock()
		p.running = false
		p.writesActive = false
		p.cancel = nil
		p.done = nil
		p.mu.Unlock()
		if done != nil {
			close(done)
		}
		return
	default:
	}

	p.mu.Lock()
	// Start the worker only after the HMAC key is available, so writesActive cannot
	// be observed as true before the safety precondition is satisfied.
	p.writesActive = true
	go p.run(workerCtx)
	p.mu.Unlock()
}

func (p *UsagePersister) Stop(ctx context.Context) {
	if p == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return
	}
	cancel := p.cancel
	done := p.done
	p.running = false
	p.cancel = nil
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	stopped := true
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			stopped = false
			log.Warnf("usage: persister stop timed out: %v", ctx.Err())
		}
	}

	p.mu.Lock()
	writesActive := p.writesActive
	p.mu.Unlock()

	// Avoid concurrent writes on shutdown: only perform the final flush if the
	// background worker has fully stopped.
	if !stopped {
		return
	}
	if writesActive {
		if err := p.flush(true); err != nil {
			log.Warnf("usage: final stats flush failed: %v", err)
		}
	}
}

func (p *UsagePersister) run(ctx context.Context) {
	defer func() {
		p.mu.Lock()
		done := p.done
		p.done = nil
		p.mu.Unlock()
		if done != nil {
			close(done)
		}
	}()

	p.mu.Lock()
	writesActive := p.writesActive
	interval := p.interval
	p.mu.Unlock()
	if !writesActive {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.flush(false); err != nil {
				log.Warnf("usage: periodic stats flush failed: %v", err)
			}
		}
	}
}

func (p *UsagePersister) loadIntoStore() error {
	path, err := usageStatsPath()
	if err != nil {
		return err
	}

	// Hardening: prevent accidental / malicious local DoS via extremely large files.
	// This file should only contain aggregated counters.
	const maxUsageStatsBytes = 25 * 1024 * 1024 // 25MiB

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Size() <= 0 {
		return nil
	}
	if info.Size() > maxUsageStatsBytes {
		return fmt.Errorf("refusing to load %s: size %d exceeds max %d", path, info.Size(), maxUsageStatsBytes)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil
	}

	var payload persistedUsageStats
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	if payload.Version != 0 && payload.Version != usagePersistenceVersion {
		return fmt.Errorf("unsupported version %d", payload.Version)
	}
	p.stats.ReplaceAggregatedSnapshot(payload.Usage)
	return nil
}

func (p *UsagePersister) flush(force bool) error {
	if p == nil || p.stats == nil {
		return nil
	}

	if !force {
		// Prevent missing updates: claim dirty by CAS before taking the snapshot.
		if !p.stats.dirty.CompareAndSwap(true, false) {
			return nil
		}
	}

	snapshot := p.stats.SnapshotAggregated()
	snapshot = sanitiseAggregatedSnapshot(snapshot)

	payload := persistedUsageStats{
		Version:    usagePersistenceVersion,
		ExportedAt: time.Now().UTC(),
		Usage:      snapshot,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		if !force {
			p.stats.dirty.Store(true)
		}
		return fmt.Errorf("encode payload: %w", err)
	}

	statsPath, err := usageStatsPath()
	if err != nil {
		if !force {
			p.stats.dirty.Store(true)
		}
		return err
	}
	if err := writeAtomicFile(statsPath, data, 0o600); err != nil {
		if !force {
			p.stats.dirty.Store(true)
		}
		return err
	}

	return nil
}

func sanitiseAggregatedSnapshot(snapshot AggregatedStatisticsSnapshot) AggregatedStatisticsSnapshot {
	out := snapshot
	if len(snapshot.APIs) == 0 {
		return out
	}

	out.APIs = make(map[string]AggregatedAPISnapshot, len(snapshot.APIs))
	for apiName, apiSnapshot := range snapshot.APIs {
		key := strings.TrimSpace(apiName)
		if key == "" {
			continue
		}
		if !strings.HasPrefix(key, "api:hmac256:") && shouldHashAPIKeyCandidate(key) {
			key = HashClientKey(key)
		}
		out.APIs[key] = apiSnapshot
	}
	return out
}

func shouldHashAPIKeyCandidate(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.Contains(value, " ") {
		return false
	}
	if strings.Contains(value, "/") {
		return false
	}
	if strings.Contains(value, ":") {
		return false
	}
	// Heuristic: long, opaque identifiers are likely API keys.
	return len(value) >= 24
}

func writeAtomicFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if strings.TrimSpace(dir) == "" {
		return errors.New("usage: invalid path")
	}

	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create tmp in %s: %w", dir, err)
	}
	tmp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}

	_ = os.Chmod(tmp, perm)
	n, err := f.Write(data)
	if err != nil {
		cleanup()
		return fmt.Errorf("write tmp %s: %w", tmp, err)
	}
	if n != len(data) {
		cleanup()
		return fmt.Errorf("write tmp %s: short write (%d/%d)", tmp, n, len(data))
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("fsync tmp %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		if _, statErr := os.Stat(path); statErr == nil {
			// Best-effort compatibility fallback for platforms/filesystems that do not
			// support atomic replace semantics.
			_ = os.Remove(path)
			if err2 := os.Rename(tmp, path); err2 == nil {
				goto renameOK
			} else {
				err = err2
			}
		}
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
renameOK:
	_ = os.Chmod(path, perm)

	// Best-effort fsync the directory to ensure the rename is durable.
	d, err := os.Open(dir)
	if err != nil {
		return nil
	}
	_ = d.Sync()
	_ = d.Close()
	return nil
}
