package asynclog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiwb/tg-obs-bot/internal/secret"
)

type contextKey string

type capturedRecord struct {
	Message string
	Level   slog.Level
	Time    time.Time
	PC      uintptr
	Context any
	Attrs   []slog.Attr
	Ops     []capturedOperation
}

type capturedOperation struct {
	Group string
	Attrs []slog.Attr
}

type probeSink struct {
	mu      sync.Mutex
	records []capturedRecord

	enabledCalls atomic.Int64
	handleCalls  atomic.Int64
	active       atomic.Int64
	maxActive    atomic.Int64

	blockFirst bool
	started    chan struct{}
	release    chan struct{}
	startOnce  sync.Once
	failCall   int64
}

type probeHandler struct {
	sink *probeSink
	ops  []capturedOperation
}

type retainingMutatingSink struct {
	mu sync.Mutex

	retained       [][]slog.Attr
	records        []capturedRecord
	withAttrsCalls int
	withGroupCalls int
	panicAttrsCall int

	mutatorStop chan struct{}
	mutatorOnce sync.Once
	mutatorWG   sync.WaitGroup
}

type retainingMutatingHandler struct {
	sink *retainingMutatingSink
	ops  []capturedOperation
}

func (h *probeHandler) Enabled(context.Context, slog.Level) bool {
	h.sink.enabledCalls.Add(1)
	return true
}

func (h *probeHandler) Handle(ctx context.Context, record slog.Record) error {
	active := h.sink.active.Add(1)
	for {
		current := h.sink.maxActive.Load()
		if active <= current || h.sink.maxActive.CompareAndSwap(current, active) {
			break
		}
	}
	defer h.sink.active.Add(-1)

	call := h.sink.handleCalls.Add(1)
	if h.sink.blockFirst && call == 1 {
		h.sink.startOnce.Do(func() {
			close(h.sink.started)
		})
		<-h.sink.release
	}

	attrs := make([]slog.Attr, 0, record.NumAttrs())
	record.Attrs(func(attr slog.Attr) bool {
		attrs = append(attrs, attr)
		return true
	})
	ops := make([]capturedOperation, len(h.ops))
	for i, op := range h.ops {
		ops[i] = capturedOperation{
			Group: op.Group,
			Attrs: append([]slog.Attr(nil), op.Attrs...),
		}
	}
	var contextValue any
	if ctx != nil {
		contextValue = ctx.Value(contextKey("request"))
	}

	h.sink.mu.Lock()
	h.sink.records = append(h.sink.records, capturedRecord{
		Message: record.Message,
		Level:   record.Level,
		Time:    record.Time,
		PC:      record.PC,
		Context: contextValue,
		Attrs:   attrs,
		Ops:     ops,
	})
	h.sink.mu.Unlock()

	if h.sink.failCall == call {
		return errors.New("scripted sink failure")
	}
	return nil
}

func (h *probeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	ops := cloneCapturedOperations(h.ops)
	ops = append(ops, capturedOperation{Attrs: append([]slog.Attr(nil), attrs...)})
	return &probeHandler{sink: h.sink, ops: ops}
}

func (h *probeHandler) WithGroup(name string) slog.Handler {
	ops := cloneCapturedOperations(h.ops)
	ops = append(ops, capturedOperation{Group: name})
	return &probeHandler{sink: h.sink, ops: ops}
}

func cloneCapturedOperations(ops []capturedOperation) []capturedOperation {
	cloned := make([]capturedOperation, len(ops))
	for i, op := range ops {
		cloned[i] = capturedOperation{
			Group: op.Group,
			Attrs: append([]slog.Attr(nil), op.Attrs...),
		}
	}
	return cloned
}

func (s *probeSink) snapshot() []capturedRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]capturedRecord(nil), s.records...)
}

func (h *retainingMutatingHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *retainingMutatingHandler) Handle(ctx context.Context, record slog.Record) error {
	h.sink.mu.Lock()
	attrs := make([]slog.Attr, 0, record.NumAttrs())
	record.Attrs(func(attr slog.Attr) bool {
		attrs = append(attrs, attr)
		return true
	})
	h.sink.records = append(h.sink.records, capturedRecord{
		Message: record.Message,
		Level:   record.Level,
		Time:    record.Time,
		PC:      record.PC,
		Attrs:   attrs,
		Ops:     cloneCapturedOperations(h.ops),
	})
	h.sink.mu.Unlock()
	return nil
}

func (h *retainingMutatingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	semanticAttrs := append([]slog.Attr(nil), attrs...)
	h.sink.mu.Lock()
	h.sink.withAttrsCalls++
	call := h.sink.withAttrsCalls
	h.sink.retained = append(h.sink.retained, attrs)
	if h.sink.mutatorStop == nil {
		h.sink.mutatorStop = make(chan struct{})
	}
	mutatorStop := h.sink.mutatorStop
	panicCall := h.sink.panicAttrsCall
	h.sink.mu.Unlock()

	// A Handler owns the slice passed to WithAttrs and is allowed to retain and
	// modify it. This intentionally poisons any async wrapper that reuses the
	// transferred slice for a later record or sibling derived handler.
	mutateTransferredAttrs(attrs)
	h.sink.mutatorWG.Add(1)
	go func(owned []slog.Attr) {
		defer h.sink.mutatorWG.Done()
		for {
			select {
			case <-mutatorStop:
				return
			default:
				mutateTransferredAttrs(owned)
				runtime.Gosched()
			}
		}
	}(attrs)
	if panicCall == call {
		panic("scripted WithAttrs panic after taking ownership")
	}

	ops := cloneCapturedOperations(h.ops)
	ops = append(ops, capturedOperation{Attrs: semanticAttrs})
	return &retainingMutatingHandler{sink: h.sink, ops: ops}
}

func (h *retainingMutatingHandler) WithGroup(group string) slog.Handler {
	h.sink.mu.Lock()
	h.sink.withGroupCalls++
	h.sink.mu.Unlock()
	ops := cloneCapturedOperations(h.ops)
	ops = append(ops, capturedOperation{Group: group})
	return &retainingMutatingHandler{sink: h.sink, ops: ops}
}

func (s *retainingMutatingSink) snapshot() ([]capturedRecord, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]capturedRecord(nil), s.records...), s.withAttrsCalls, s.withGroupCalls
}

func (s *retainingMutatingSink) stopMutators() {
	s.mu.Lock()
	stop := s.mutatorStop
	s.mu.Unlock()
	if stop == nil {
		return
	}
	s.mutatorOnce.Do(func() {
		close(stop)
	})
	s.mutatorWG.Wait()
}

func mutateTransferredAttrs(attrs []slog.Attr) {
	if len(attrs) > 0 {
		attrs[0] = slog.String("sink_owned_mutation", "poison")
	}
}

func TestPermanentBlockingSinkKeepsCallersBoundedAndDropsNewest(t *testing.T) {
	sink := &probeSink{
		blockFirst: true,
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	handler := New(&probeHandler{sink: sink}, slog.LevelInfo)
	logger := slog.New(handler)
	logger.Info("block writer")
	select {
	case <-sink.started:
	case <-time.After(time.Second):
		t.Fatal("writer did not reach blocking sink")
	}

	const calls = 100_000
	start := time.Now()
	for i := 0; i < calls; i++ {
		logger.Info("queued", "sequence", i)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("%d caller-side log operations took %v", calls, elapsed)
	}

	stats := handler.Stats()
	if stats.Depth != QueueCapacity {
		t.Fatalf("queue depth = %d, want %d", stats.Depth, QueueCapacity)
	}
	if stats.HighWater != QueueCapacity {
		t.Fatalf("queue high-water = %d, want %d", stats.HighWater, QueueCapacity)
	}
	if want := uint64(calls - QueueCapacity); stats.TotalDrops != want {
		t.Fatalf("total drops = %d, want %d", stats.TotalDrops, want)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	shutdownStart := time.Now()
	err := handler.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(shutdownStart); elapsed > 500*time.Millisecond {
		t.Fatalf("blocked Shutdown returned after %v", elapsed)
	}

	handler.Abort()
	handler.Abort()
	if err := handler.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after Abort: %v", err)
	}
	if got := handler.Stats().State; got != StateStopped {
		t.Fatalf("state after Abort = %s, want stopped", got)
	}

	afterStop := time.Now()
	for i := 0; i < calls; i++ {
		logger.Error("after stop", "sequence", i)
	}
	if elapsed := time.Since(afterStop); elapsed > 5*time.Second {
		t.Fatalf("%d post-stop log operations took %v", calls, elapsed)
	}

	close(sink.release)
	if got := sink.maxActive.Load(); got != 1 {
		t.Fatalf("maximum concurrent sink calls = %d, want 1", got)
	}
}

func TestShutdownDrainsFIFOAndIsConcurrentAndIdempotent(t *testing.T) {
	sink := &probeSink{}
	handler := New(&probeHandler{sink: sink}, slog.LevelDebug)
	logger := slog.New(handler)

	const records = 128
	for i := 0; i < records; i++ {
		logger.Info(fmt.Sprintf("record-%03d", i))
	}

	const shutdowns = 24
	errs := make(chan error, shutdowns)
	var wg sync.WaitGroup
	for i := 0; i < shutdowns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			errs <- handler.Shutdown(ctx)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Shutdown: %v", err)
		}
	}
	if err := handler.Shutdown(context.Background()); err != nil {
		t.Fatalf("idempotent Shutdown: %v", err)
	}

	got := sink.snapshot()
	if len(got) != records {
		t.Fatalf("emitted records = %d, want %d", len(got), records)
	}
	for i, record := range got {
		want := fmt.Sprintf("record-%03d", i)
		if record.Message != want {
			t.Fatalf("record %d message = %q, want %q", i, record.Message, want)
		}
	}

	before := handler.Stats().TotalDrops
	logger.Error("after shutdown")
	if after := handler.Stats().TotalDrops; after != before+1 {
		t.Fatalf("post-shutdown drop total = %d, want %d", after, before+1)
	}
}

func TestHandlerPreservesRecordContextOperationsAndDynamicLevel(t *testing.T) {
	sink := &probeSink{}
	var level slog.LevelVar
	level.Set(slog.LevelWarn)
	handler := New(&probeHandler{sink: sink}, &level)
	logger := slog.New(handler)

	if handler.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("info unexpectedly enabled at warn level")
	}
	if !handler.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("error unexpectedly disabled at warn level")
	}
	logger.Info("filtered")
	logger.Warn("warn")
	level.Set(slog.LevelDebug)
	logger.Debug("debug")
	if calls := sink.enabledCalls.Load(); calls != 0 {
		t.Fatalf("sink Enabled calls = %d, want 0", calls)
	}

	withAttrsInput := []slog.Attr{slog.String("component", "test")}
	derived := handler.WithAttrs(withAttrsInput).WithGroup("request").WithAttrs([]slog.Attr{
		slog.Int("attempt", 3),
	})
	withAttrsInput[0] = slog.String("component", "mutated")

	fixedTime := time.Date(2026, time.July, 26, 12, 34, 56, 789, time.UTC)
	var callers [1]uintptr
	runtime.Callers(1, callers[:])
	record := slog.NewRecord(fixedTime, slog.LevelError, "fixed", callers[0])
	record.AddAttrs(
		slog.String("key", "value"),
		slog.Group("nested", slog.Int("count", 2)),
	)
	ctx := context.WithValue(context.Background(), contextKey("request"), "ctx-42")
	if err := derived.Handle(ctx, record); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	record.Message = "mutated after Handle"
	record.Time = time.Time{}
	record.PC = 0
	record.AddAttrs(slog.String("late", "mutation"))

	shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := handler.Shutdown(shutdownContext); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	got := sink.snapshot()
	if len(got) != 3 {
		t.Fatalf("emitted records = %d, want warn, debug, and fixed", len(got))
	}
	if got[0].Message != "warn" || got[1].Message != "debug" {
		t.Fatalf("dynamic level messages = %q, %q", got[0].Message, got[1].Message)
	}
	fixed := got[2]
	if fixed.Message != "fixed" || fixed.Level != slog.LevelError ||
		!fixed.Time.Equal(fixedTime) || fixed.PC != callers[0] {
		t.Fatalf("fixed record metadata changed: %+v", fixed)
	}
	if fixed.Context != "ctx-42" {
		t.Fatalf("context value = %v, want ctx-42", fixed.Context)
	}
	if len(fixed.Ops) != 3 ||
		len(fixed.Ops[0].Attrs) != 1 ||
		fixed.Ops[0].Attrs[0].Value.String() != "test" ||
		fixed.Ops[1].Group != "request" ||
		len(fixed.Ops[2].Attrs) != 1 ||
		fixed.Ops[2].Attrs[0].Key != "attempt" {
		t.Fatalf("derived handler operations changed: %+v", fixed.Ops)
	}
	if len(fixed.Attrs) != 2 || fixed.Attrs[0].Key != "key" || fixed.Attrs[1].Key != "nested" {
		t.Fatalf("record attrs changed: %+v", fixed.Attrs)
	}
	if calls := sink.enabledCalls.Load(); calls != 0 {
		t.Fatalf("sink Enabled calls after drain = %d, want 0", calls)
	}
}

func TestLazySinkViewsTransferFreshAttrsOnceAndKeepSiblingsIndependent(t *testing.T) {
	sink := &retainingMutatingSink{}
	defer sink.stopMutators()
	root := New(&retainingMutatingHandler{sink: sink}, slog.LevelInfo)

	commonAttrs := []slog.Attr{slog.String("component", "stable")}
	base := root.WithAttrs(commonAttrs)
	left := slog.New(base.WithGroup("left"))
	right := slog.New(base.WithGroup("right"))
	commonAttrs[0] = slog.String("component", "caller mutation")

	left.Info("left-1")
	left.Info("left-2")
	right.Info("right-1")
	right.Info("right-2")
	if err := root.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	sink.stopMutators()

	records, attrsCalls, groupCalls := sink.snapshot()
	if attrsCalls != 2 || groupCalls != 2 {
		t.Fatalf(
			"derived sink calls WithAttrs=%d WithGroup=%d, want once for each of two views",
			attrsCalls,
			groupCalls,
		)
	}
	if len(records) != 4 {
		t.Fatalf("emitted records = %d, want 4", len(records))
	}
	for i, record := range records {
		wantGroup := "left"
		if i >= 2 {
			wantGroup = "right"
		}
		if len(record.Ops) != 2 ||
			len(record.Ops[0].Attrs) != 1 ||
			record.Ops[0].Attrs[0].Key != "component" ||
			record.Ops[0].Attrs[0].Value.String() != "stable" ||
			record.Ops[1].Group != wantGroup {
			t.Fatalf("record %d derived operations corrupted: %+v", i, record.Ops)
		}
	}
}

func TestLazySinkViewRetriesCleanlyAfterWithAttrsPanic(t *testing.T) {
	sink := &retainingMutatingSink{panicAttrsCall: 1}
	defer sink.stopMutators()
	root := New(&retainingMutatingHandler{sink: sink}, slog.LevelInfo)
	logger := slog.New(
		root.
			WithAttrs([]slog.Attr{slog.String("component", "stable")}).
			WithGroup("request"),
	)

	logger.Info("lost during lazy resolve panic")
	logger.Info("retry resolves")
	logger.Info("resolved view reused")
	if err := root.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	sink.stopMutators()

	records, attrsCalls, groupCalls := sink.snapshot()
	if attrsCalls != 2 || groupCalls != 1 {
		t.Fatalf(
			"resolution calls WithAttrs=%d WithGroup=%d, want retry counts 2 and 1",
			attrsCalls,
			groupCalls,
		)
	}
	if len(records) != 2 ||
		records[0].Message != "retry resolves" ||
		records[1].Message != "resolved view reused" {
		t.Fatalf("records after lazy resolution panic = %+v", records)
	}
	for i, record := range records {
		if len(record.Ops) != 2 ||
			len(record.Ops[0].Attrs) != 1 ||
			record.Ops[0].Attrs[0].Key != "component" ||
			record.Ops[0].Attrs[0].Value.String() != "stable" ||
			record.Ops[1].Group != "request" {
			t.Fatalf("record %d retry operations corrupted: %+v", i, record.Ops)
		}
	}
	stats := root.Stats()
	if stats.SinkErrors != 1 {
		t.Fatalf("sink errors after lazy resolution panic = %d, want 1", stats.SinkErrors)
	}
}

func TestTextHandlerWithAttrsGroupTimeAndPCMatchesDirectHandler(t *testing.T) {
	var direct bytes.Buffer
	var asynchronous bytes.Buffer
	options := &slog.HandlerOptions{AddSource: true, Level: slog.LevelDebug}

	directHandler := slog.NewTextHandler(&direct, options).
		WithAttrs([]slog.Attr{slog.String("component", "equivalence")}).
		WithGroup("request").
		WithAttrs([]slog.Attr{slog.Int("attempt", 4)})

	asyncRoot := New(slog.NewTextHandler(&asynchronous, options), slog.LevelDebug)
	asyncHandler := asyncRoot.
		WithAttrs([]slog.Attr{slog.String("component", "equivalence")}).
		WithGroup("request").
		WithAttrs([]slog.Attr{slog.Int("attempt", 4)})

	fixedTime := time.Date(2026, time.July, 26, 1, 2, 3, 4, time.UTC)
	var callers [1]uintptr
	runtime.Callers(1, callers[:])
	record := slog.NewRecord(fixedTime, slog.LevelInfo, "same", callers[0])
	record.AddAttrs(
		slog.String("message_id", "abc"),
		slog.Group("nested", slog.Bool("ok", true)),
	)
	if err := directHandler.Handle(context.Background(), record.Clone()); err != nil {
		t.Fatalf("direct Handle: %v", err)
	}
	if err := asyncHandler.Handle(context.Background(), record); err != nil {
		t.Fatalf("async Handle: %v", err)
	}
	if err := asyncRoot.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if got, want := asynchronous.String(), direct.String(); got != want {
		t.Fatalf("async text output differs\n got: %s\nwant: %s", got, want)
	}
}

func TestHandleClonesRecordBeforeDelayedDropDecoration(t *testing.T) {
	sink := &probeSink{
		blockFirst: true,
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	handler := New(&probeHandler{sink: sink}, slog.LevelInfo)
	logger := slog.New(handler)
	logger.Info("in flight")
	select {
	case <-sink.started:
	case <-time.After(time.Second):
		t.Fatal("writer did not reach blocking sink")
	}

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "clone target", 0)
	attrs := make([]slog.Attr, 100)
	for i := range attrs {
		attrs[i] = slog.Int(fmt.Sprintf("attr_%03d", i), i)
	}
	record.AddAttrs(attrs...)
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle clone target: %v", err)
	}

	// This deliberately extends the caller's Record after Handle returns. A
	// shallow asynchronous copy can share spare backing capacity and slog will
	// later emit !BUG when log_dropped is appended to that copy.
	record.AddAttrs(slog.String("caller_after_handle", "preserved"))
	for i := 0; i < QueueCapacity-1; i++ {
		logger.Info("filler", "sequence", i)
	}
	logger.Info("drop to require delayed decoration")
	close(sink.release)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := handler.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	records := sink.snapshot()
	if len(records) != QueueCapacity+1 {
		t.Fatalf("sink calls = %d, want %d", len(records), QueueCapacity+1)
	}
	cloneTarget := records[1]
	if got := attrUint64(cloneTarget.Attrs, "log_dropped"); got != 1 {
		t.Fatalf("clone target drop summary = %d, want 1", got)
	}
	for _, attr := range cloneTarget.Attrs {
		if attr.Key == "!BUG" {
			t.Fatalf("delayed record was not cloned: %+v", cloneTarget.Attrs)
		}
		if attr.Key == "caller_after_handle" {
			t.Fatalf("post-Handle caller attribute leaked into queued clone")
		}
	}

	callerAttrs := make([]slog.Attr, 0, record.NumAttrs())
	record.Attrs(func(attr slog.Attr) bool {
		callerAttrs = append(callerAttrs, attr)
		return true
	})
	foundCallerAttr := false
	for _, attr := range callerAttrs {
		if attr.Key == "caller_after_handle" && attr.Value.String() == "preserved" {
			foundCallerAttr = true
		}
	}
	if !foundCallerAttr {
		t.Fatal("caller Record was corrupted by delayed sink decoration")
	}
}

func TestDropSummarySurvivesSinkErrorUntilSuccessfulEmission(t *testing.T) {
	sink := &probeSink{
		blockFirst: true,
		started:    make(chan struct{}),
		release:    make(chan struct{}),
		failCall:   2,
	}
	handler := New(&probeHandler{sink: sink}, slog.LevelInfo)
	logger := slog.New(handler)
	logger.Info("in flight")
	select {
	case <-sink.started:
	case <-time.After(time.Second):
		t.Fatal("writer did not reach blocking sink")
	}

	for i := 0; i < QueueCapacity; i++ {
		logger.Info("queued", "sequence", i)
	}
	const dropped = 7
	for i := 0; i < dropped; i++ {
		logger.Info("drop newest", "sequence", i)
	}
	close(sink.release)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := handler.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	records := sink.snapshot()
	if len(records) != QueueCapacity+1 {
		t.Fatalf("sink calls = %d, want %d", len(records), QueueCapacity+1)
	}
	if got := attrUint64(records[0].Attrs, "log_dropped"); got != 0 {
		t.Fatalf("in-flight record drop summary = %d, want 0", got)
	}
	if got := attrUint64(records[1].Attrs, "log_dropped"); got != dropped {
		t.Fatalf("failed sink record drop summary = %d, want %d", got, dropped)
	}
	if got := attrUint64(records[2].Attrs, "log_dropped"); got != dropped {
		t.Fatalf("next successful record drop summary = %d, want %d", got, dropped)
	}
	for i := 3; i < len(records); i++ {
		if got := attrUint64(records[i].Attrs, "log_dropped"); got != 0 {
			t.Fatalf("record %d repeated cleared drop summary %d", i, got)
		}
	}

	stats := handler.Stats()
	if stats.TotalDrops != dropped || stats.PendingDrops != 0 || stats.SinkErrors != 1 {
		t.Fatalf("stats after sink recovery = %+v", stats)
	}
}

func TestAsyncOutputPreservesSecretRedaction(t *testing.T) {
	const (
		token    = "123456789:async-token-secret"
		password = "obs-password-secret"
	)
	var output bytes.Buffer
	handler := New(slog.NewTextHandler(&output, nil), slog.LevelInfo)
	logger := slog.New(handler)
	logger.Error(
		"startup failed",
		"error",
		secret.RedactError(
			fmt.Errorf("telegram %s and OBS %s", token, password),
			token,
			password,
		),
	)
	if err := handler.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	logged := output.String()
	if strings.Contains(logged, token) ||
		strings.Contains(logged, "async-token-secret") ||
		strings.Contains(logged, password) {
		t.Fatalf("async output leaked secret: %q", logged)
	}
	if !strings.Contains(strings.ToLower(logged), "redacted") {
		t.Fatalf("async output lacks redaction marker: %q", logged)
	}
}

func attrUint64(attrs []slog.Attr, key string) uint64 {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value.Uint64()
		}
	}
	return 0
}
