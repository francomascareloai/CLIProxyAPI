package usage

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// Record contains the usage statistics captured for a single provider request.
type Record struct {
	Provider    string
	Model       string
	APIKey      string
	AuthID      string
	AuthIndex   string
	Source      string
	RequestedAt time.Time
	Failed      bool
	CountOnly   bool
	Detail      Detail
}

// Detail holds the token usage breakdown.
type Detail struct {
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	CachedTokens    int64
	TotalTokens     int64
}

// Plugin consumes usage records emitted by the proxy runtime.
type Plugin interface {
	HandleUsage(ctx context.Context, record Record)
}

type queueItem struct {
	ctx    context.Context
	record Record
}

type DroppedRecordsSnapshot struct {
	TotalDropped int64
	ByMinute     map[int64]int64
}

// Manager maintains a queue of usage records and delivers them to registered plugins.
type Manager struct {
	once     sync.Once
	stopOnce sync.Once
	cancel   context.CancelFunc

	mu              sync.Mutex
	cond            *sync.Cond
	queue           []queueItem
	maxLen          int
	closed          bool
	done            chan struct{}
	droppedRecords  int64
	droppedByMinute map[int64]int64

	pluginsMu sync.RWMutex
	plugins   []Plugin

	dropLoggedAt int64
}

// NewManager constructs a manager with a buffered queue.
func NewManager(buffer int) *Manager {
	m := &Manager{
		maxLen:          buffer,
		droppedByMinute: make(map[int64]int64),
	}
	m.cond = sync.NewCond(&m.mu)
	return m
}

// Start launches the background dispatcher. Calling Start multiple times is safe.
func (m *Manager) Start(ctx context.Context) {
	if m == nil {
		return
	}
	m.once.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}
		var workerCtx context.Context
		workerCtx, m.cancel = context.WithCancel(ctx)
		m.mu.Lock()
		m.done = make(chan struct{})
		m.mu.Unlock()
		go m.run(workerCtx)

		// Ensure ctx cancellation triggers Stop(), so Wait(ctx) can observe progress.
		go func() {
			<-workerCtx.Done()
			m.Stop()
		}()
	})
}

// Stop stops the dispatcher and drains the queue.
func (m *Manager) Stop() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() {
		if m.cancel != nil {
			m.cancel()
		}
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		m.cond.Broadcast()
	})
}

// Wait blocks until the dispatcher exits, or the context is done.
//
// This is useful when callers must ensure the queue has been fully drained.
func (m *Manager) Wait(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	done := m.done
	m.mu.Unlock()
	if done == nil {
		return nil
	}
	if ctx == nil {
		<-done
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Register appends a plugin to the delivery list.
func (m *Manager) Register(plugin Plugin) {
	if m == nil || plugin == nil {
		return
	}
	m.pluginsMu.Lock()
	m.plugins = append(m.plugins, plugin)
	m.pluginsMu.Unlock()
}

// Publish enqueues a usage record for processing. If no plugin is registered
// the record will be discarded downstream.
func (m *Manager) Publish(ctx context.Context, record Record) {
	if m == nil {
		return
	}
	// ensure worker is running even if Start was not called explicitly
	m.Start(context.Background())

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	// Buffer is a hard cap: when full, drop (do not block requests).
	if m.maxLen > 0 && len(m.queue) >= m.maxLen {
		m.recordDroppedLocked(record)
		m.mu.Unlock()
		m.logDropRateLimited()
		return
	}
	m.queue = append(m.queue, queueItem{ctx: ctx, record: record})
	m.mu.Unlock()
	m.cond.Signal()
}

func (m *Manager) run(ctx context.Context) {
	defer func() {
		m.mu.Lock()
		done := m.done
		m.done = nil
		m.mu.Unlock()
		if done != nil {
			close(done)
		}
	}()

	for {
		m.mu.Lock()
		for !m.closed && len(m.queue) == 0 {
			m.cond.Wait()
		}
		if len(m.queue) == 0 && m.closed {
			m.mu.Unlock()
			return
		}
		item := m.queue[0]
		m.queue = m.queue[1:]
		m.mu.Unlock()
		m.dispatch(item)
	}
}

func (m *Manager) dispatch(item queueItem) {
	m.pluginsMu.RLock()
	plugins := make([]Plugin, len(m.plugins))
	copy(plugins, m.plugins)
	m.pluginsMu.RUnlock()
	if len(plugins) == 0 {
		return
	}
	for _, plugin := range plugins {
		if plugin == nil {
			continue
		}
		safeInvoke(plugin, item.ctx, item.record)
	}
}

func (m *Manager) recordDroppedLocked(record Record) {
	if m == nil {
		return
	}
	m.droppedRecords++
	if m.droppedByMinute == nil {
		m.droppedByMinute = make(map[int64]int64)
	}
	timestamp := record.RequestedAt.UTC()
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}
	minute := timestamp.Truncate(time.Minute).Unix() / 60
	m.droppedByMinute[minute]++
}

func (m *Manager) DroppedRecordsSnapshot() DroppedRecordsSnapshot {
	if m == nil {
		return DroppedRecordsSnapshot{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	result := DroppedRecordsSnapshot{
		TotalDropped: m.droppedRecords,
	}
	if len(m.droppedByMinute) > 0 {
		result.ByMinute = make(map[int64]int64, len(m.droppedByMinute))
		for minute, count := range m.droppedByMinute {
			result.ByMinute[minute] = count
		}
	}
	return result
}

func (m *Manager) ResetDroppedRecords() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.droppedRecords = 0
	m.droppedByMinute = make(map[int64]int64)
}

func (m *Manager) logDropRateLimited() {
	// Rate-limit warnings to avoid log spam on sustained backpressure.
	const minInterval = int64(30) // seconds
	now := time.Now().Unix()
	prev := atomic.LoadInt64(&m.dropLoggedAt)
	if prev != 0 && now-prev < minInterval {
		return
	}
	if !atomic.CompareAndSwapInt64(&m.dropLoggedAt, prev, now) {
		return
	}
	log.Warnf("usage: dropping usage record (queue full)")
}

func safeInvoke(plugin Plugin, ctx context.Context, record Record) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("usage: plugin panic recovered: %v", r)
		}
	}()
	plugin.HandleUsage(ctx, record)
}

var defaultManager = NewManager(512)

// DefaultManager returns the global usage manager instance.
func DefaultManager() *Manager { return defaultManager }

// RegisterPlugin registers a plugin on the default manager.
func RegisterPlugin(plugin Plugin) { DefaultManager().Register(plugin) }

// PublishRecord publishes a record using the default manager.
func PublishRecord(ctx context.Context, record Record) { DefaultManager().Publish(ctx, record) }

// StartDefault starts the default manager's dispatcher.
func StartDefault(ctx context.Context) { DefaultManager().Start(ctx) }

// StopDefault stops the default manager's dispatcher.
func StopDefault() { DefaultManager().Stop() }
