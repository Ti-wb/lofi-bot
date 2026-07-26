// Package asynclog isolates application goroutines from blocking log sinks.
package asynclog

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
)

// QueueCapacity is the fixed number of records that can wait behind the
// record currently being emitted.
const QueueCapacity = 256

// State describes the handler lifecycle.
type State uint32

const (
	StateOpen State = iota
	StateDraining
	StateStopped
)

func (s State) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StateDraining:
		return "draining"
	case StateStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// Stats is an instantaneous snapshot of handler health. Depth counts queued
// records and excludes a record currently blocked in the sink.
type Stats struct {
	State        State
	TotalDrops   uint64
	PendingDrops uint64
	SinkErrors   uint64
	Depth        int
	HighWater    int
}

// Handler is a nonblocking slog.Handler. Derived handlers share one queue,
// lifecycle, metric set, and sink writer goroutine.
type Handler struct {
	core *core
	ops  []operation
	view *sinkView
}

type core struct {
	sink  slog.Handler
	level slog.Leveler
	queue chan queuedRecord

	drain chan struct{}
	abort chan struct{}
	done  chan struct{}

	lifecycleMu sync.Mutex
	doneOnce    sync.Once
	state       atomic.Uint32

	totalDrops   atomic.Uint64
	pendingDrops atomic.Uint64
	sinkErrors   atomic.Uint64
	depth        atomic.Int64
	highWater    atomic.Int64
}

type queuedRecord struct {
	ctx    context.Context
	record slog.Record
	view   *sinkView
}

type operation struct {
	attrs []slog.Attr
	group string
}

// sinkView is initialized and read only by the single writer goroutine.
// Keeping resolution state off the caller-visible Handler path lets a sink
// take ownership of WithAttrs slices without racing callers or later records.
type sinkView struct {
	ops      []operation
	resolved slog.Handler
}

// New constructs a handler over sink. level is checked synchronously without
// consulting sink.Enabled; nil uses slog.LevelInfo.
func New(sink slog.Handler, level slog.Leveler) *Handler {
	if sink == nil {
		sink = slog.NewTextHandler(io.Discard, nil)
	}
	if level == nil {
		level = slog.LevelInfo
	}
	c := &core{
		sink:  sink,
		level: level,
		queue: make(chan queuedRecord, QueueCapacity),
		drain: make(chan struct{}),
		abort: make(chan struct{}),
		done:  make(chan struct{}),
	}
	c.state.Store(uint32(StateOpen))
	go c.run()
	return &Handler{
		core: c,
		view: &sinkView{resolved: sink},
	}
}

// NewHandler is an alias for New.
func NewHandler(sink slog.Handler, level slog.Leveler) *Handler {
	return New(sink, level)
}

// Enabled applies only the configured Leveler. It deliberately never calls
// the sink because a custom sink's Enabled method is allowed to block.
func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.core.level.Level()
}

// Handle clones record before attempting a nonblocking enqueue. Drops are
// intentionally reported only through Stats and a later log_dropped summary;
// logging backpressure is never returned to application code.
func (h *Handler) Handle(ctx context.Context, record slog.Record) error {
	if ctx == nil {
		ctx = context.Background()
	}
	item := queuedRecord{
		ctx:    ctx,
		record: record.Clone(),
		view:   h.view,
	}

	c := h.core
	c.lifecycleMu.Lock()
	if State(c.state.Load()) != StateOpen {
		c.recordDrop()
		c.lifecycleMu.Unlock()
		return nil
	}
	select {
	case c.queue <- item:
		depth := c.depth.Add(1)
		c.updateHighWater(depth)
	default:
		c.recordDrop()
	}
	c.lifecycleMu.Unlock()
	return nil
}

// WithAttrs returns a derived handler without touching the sink on the caller
// goroutine. The sink operation is applied in order by the writer goroutine.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	copied := append([]slog.Attr(nil), attrs...)
	ops := make([]operation, len(h.ops)+1)
	copy(ops, h.ops)
	ops[len(h.ops)] = operation{attrs: copied}
	return &Handler{
		core: h.core,
		ops:  ops,
		view: &sinkView{ops: ops},
	}
}

// WithGroup returns a derived handler without touching the sink on the caller
// goroutine. Empty group names preserve the current handler.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	ops := make([]operation, len(h.ops)+1)
	copy(ops, h.ops)
	ops[len(h.ops)] = operation{group: name}
	return &Handler{
		core: h.core,
		ops:  ops,
		view: &sinkView{ops: ops},
	}
}

// Shutdown starts a FIFO drain and waits for it to complete. The first caller
// starts the transition; concurrent and later callers observe the same drain.
// A context timeout does not wait for a sink that is stuck in Handle.
func (h *Handler) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c := h.core
	c.beginDrain()
	if State(c.state.Load()) == StateStopped {
		return nil
	}
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Abort stops accepting records and releases Shutdown waiters immediately.
// An io.Writer already blocked inside the sole writer goroutine cannot be
// forcibly canceled; if it later returns, the worker discards queued records.
func (h *Handler) Abort() {
	c := h.core
	c.lifecycleMu.Lock()
	if State(c.state.Load()) != StateStopped {
		c.state.Store(uint32(StateStopped))
		close(c.abort)
		c.doneOnce.Do(func() {
			close(c.done)
		})
	}
	c.lifecycleMu.Unlock()
}

// Stats returns an approximate concurrent snapshot.
func (h *Handler) Stats() Stats {
	c := h.core
	return Stats{
		State:        State(c.state.Load()),
		TotalDrops:   c.totalDrops.Load(),
		PendingDrops: c.pendingDrops.Load(),
		SinkErrors:   c.sinkErrors.Load(),
		Depth:        int(c.depth.Load()),
		HighWater:    int(c.highWater.Load()),
	}
}

func (c *core) beginDrain() {
	c.lifecycleMu.Lock()
	if State(c.state.Load()) == StateOpen {
		c.state.Store(uint32(StateDraining))
		close(c.drain)
	}
	c.lifecycleMu.Unlock()
}

func (c *core) run() {
	defer c.finish()
	for {
		select {
		case <-c.abort:
			c.discardQueued()
			return
		default:
		}

		select {
		case <-c.abort:
			c.discardQueued()
			return
		case item := <-c.queue:
			c.recordDequeue()
			if State(c.state.Load()) == StateStopped {
				c.discardQueued()
				return
			}
			c.emit(item)
		case <-c.drain:
			c.drainQueue()
			return
		}
	}
}

func (c *core) drainQueue() {
	for {
		if State(c.state.Load()) == StateStopped {
			c.discardQueued()
			return
		}
		select {
		case <-c.abort:
			c.discardQueued()
			return
		case item := <-c.queue:
			c.recordDequeue()
			c.emit(item)
		default:
			return
		}
	}
}

func (c *core) discardQueued() {
	for {
		select {
		case <-c.queue:
			c.recordDequeue()
		default:
			return
		}
	}
}

func (c *core) finish() {
	c.state.Store(uint32(StateStopped))
	c.doneOnce.Do(func() {
		close(c.done)
	})
}

func (c *core) emit(item queuedRecord) {
	pending := c.pendingDrops.Load()
	record := item.record
	if pending > 0 {
		record.AddAttrs(slog.Uint64("log_dropped", pending))
	}

	if c.handleSink(item.ctx, record, item.view) {
		c.sinkErrors.Add(1)
		return
	}
	if pending > 0 {
		c.clearReportedDrops(pending)
	}
}

// handleSink reports whether the sink returned an error or panicked. There is
// intentionally no recursive logging on either path.
func (c *core) handleSink(ctx context.Context, record slog.Record, view *sinkView) (failed bool) {
	defer func() {
		if recover() != nil {
			failed = true
		}
	}()

	handler := view.resolved
	if handler == nil {
		handler = c.sink
		for _, op := range view.ops {
			if op.group != "" {
				handler = handler.WithGroup(op.group)
			} else {
				// Handler.WithAttrs transfers ownership of its slice. Never
				// pass the immutable operation storage itself: a valid sink
				// may retain or modify the transferred slice.
				attrs := append([]slog.Attr(nil), op.attrs...)
				handler = handler.WithAttrs(attrs)
			}
			if handler == nil {
				panic("slog Handler returned nil derived handler")
			}
		}
		// Publish only after the complete chain resolves. A panic leaves the
		// original operations and retry state untouched for the next record.
		view.resolved = handler
	}
	return handler.Handle(ctx, record) != nil
}

func (c *core) recordDrop() {
	c.totalDrops.Add(1)
	c.pendingDrops.Add(1)
}

func (c *core) recordDequeue() {
	// Enqueue and its depth increment happen while lifecycleMu is held. Taking
	// the same mutex after receive prevents a fast worker from exposing a
	// transient negative depth before the sender records that enqueue.
	c.lifecycleMu.Lock()
	c.depth.Add(-1)
	c.lifecycleMu.Unlock()
}

func (c *core) clearReportedDrops(reported uint64) {
	for {
		current := c.pendingDrops.Load()
		if current < reported {
			return
		}
		if c.pendingDrops.CompareAndSwap(current, current-reported) {
			return
		}
	}
}

func (c *core) updateHighWater(depth int64) {
	for {
		current := c.highWater.Load()
		if depth <= current || c.highWater.CompareAndSwap(current, depth) {
			return
		}
	}
}
