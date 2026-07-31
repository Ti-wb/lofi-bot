// Package liveness tracks progress made by the service's fixed set of
// mandatory workers.
//
// Progress is advanced through the capability bound to one concrete worker
// path. A separate reporter may observe snapshots, but it must never
// manufacture progress for a blocked worker.
package liveness

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// RequiredWorkerCount is deliberately fixed. Supervised deployments use
	// the same five-worker library-only schema.
	RequiredWorkerCount = 5

	// DefaultProgressInterval is short relative to the supervisor's stale
	// threshold, while keeping idle-worker activity bounded.
	DefaultProgressInterval = 10 * time.Second

	phaseBits    = 8
	sequenceBits = 64 - phaseBits
	sequenceMask = uint64(1<<sequenceBits) - 1

	// Production scopes are shallow, but a fixed bound makes misuse fail
	// explicitly instead of growing per-worker tracking without limit.
	maxActiveScopes = 16
)

var (
	ErrInvalidBinding    = errors.New("invalid liveness worker binding")
	ErrDuplicateBinding  = errors.New("duplicate liveness worker binding")
	ErrIncompleteBinding = errors.New("incomplete liveness worker bindings")
	ErrRegistrySealed    = errors.New("liveness registry is sealed")
)

// WorkerID is the fixed wire identity of one mandatory service worker.
type WorkerID uint8

const (
	WorkerUnknown WorkerID = iota
	WorkerTelegram
	WorkerOBSReconnect
	WorkerOBSEvents
	WorkerMaintenance
	WorkerPlayback
)

var requiredWorkerIDs = [...]WorkerID{
	WorkerTelegram,
	WorkerOBSReconnect,
	WorkerOBSEvents,
	WorkerMaintenance,
	WorkerPlayback,
}

func (id WorkerID) String() string {
	switch id {
	case WorkerTelegram:
		return "telegram"
	case WorkerOBSReconnect:
		return "obs-reconnect"
	case WorkerOBSEvents:
		return "obs-events"
	case WorkerMaintenance:
		return "maintenance"
	case WorkerPlayback:
		return "playback"
	default:
		return "unknown"
	}
}

// RequiredWorkerIDs returns the stable supervisor order. Callers receive a
// copy, so the package-level schema cannot be mutated.
func RequiredWorkerIDs() [RequiredWorkerCount]WorkerID {
	return requiredWorkerIDs
}

// Owner identifies the single concrete loop allowed to advance a WorkerID.
// It is startup validation metadata, not a variable heartbeat label.
type Owner uint8

const (
	OwnerUnknown Owner = iota
	OwnerTelegram
	OwnerOBSReconnect
	OwnerOBSEvents
	OwnerMaintenance
	OwnerLibraryScheduler
)

func (owner Owner) String() string {
	switch owner {
	case OwnerTelegram:
		return "telegram"
	case OwnerOBSReconnect:
		return "obs-reconnect"
	case OwnerOBSEvents:
		return "obs-events"
	case OwnerMaintenance:
		return "maintenance"
	case OwnerLibraryScheduler:
		return "library-scheduler"
	default:
		return "unknown"
	}
}

// Phase is a finite, low-cardinality description of what a worker loop is
// currently doing. An operation phase marks a boundary, not external success.
type Phase uint8

const (
	PhaseUnknown Phase = iota
	PhaseStarting
	PhaseOperation
	PhaseEventWait
	PhaseScheduledWait
	PhaseRetryWait
	PhaseCancelWait
	PhaseMediaCopy
	PhaseMediaProbe
	PhaseDurabilitySync
	PhaseLibraryScan
)

func (phase Phase) String() string {
	switch phase {
	case PhaseStarting:
		return "starting"
	case PhaseOperation:
		return "operation"
	case PhaseEventWait:
		return "event-wait"
	case PhaseScheduledWait:
		return "scheduled-wait"
	case PhaseRetryWait:
		return "retry-wait"
	case PhaseCancelWait:
		return "cancel-wait"
	case PhaseMediaCopy:
		return "media-copy"
	case PhaseMediaProbe:
		return "media-probe"
	case PhaseDurabilitySync:
		return "durability-sync"
	case PhaseLibraryScan:
		return "library-scan"
	default:
		return "unknown"
	}
}

func (phase Phase) valid() bool {
	return phase >= PhaseStarting && phase <= PhaseLibraryScan
}

func (phase Phase) special() bool {
	switch phase {
	case PhaseMediaCopy, PhaseMediaProbe, PhaseDurabilitySync, PhaseLibraryScan:
		return true
	default:
		return false
	}
}

// Options controls local tracking cadence. The production default should be
// used unless a deterministic short interval is needed by a test.
type Options struct {
	ProgressInterval time.Duration
}

// Snapshot is one atomic observation of a mandatory worker.
type Snapshot struct {
	ID       WorkerID
	Owner    Owner
	Phase    Phase
	Sequence uint64
}

type workerState struct {
	id    WorkerID
	owner Owner
	value atomic.Uint64

	scopeMu        sync.Mutex
	scopeBase      Phase
	nextScopeToken uint64
	activeScopes   [maxActiveScopes]activeScope
	activeCount    int
}

type activeScope struct {
	token uint64
	phase Phase
}

// Worker is the capability held by exactly one concrete required loop.
type Worker struct {
	state            *workerState
	progressInterval time.Duration
}

// ScopeHandle is a token-bound capability for one special-operation scope.
// Copies remain safe: the first Close removes the token and later calls become
// no-ops.
type ScopeHandle struct {
	state *workerState
	token uint64
	phase Phase
}

// Registry owns the five fixed worker slots and rejects missing, duplicate, or
// mode-inconsistent ownership before any worker goroutine starts.
type Registry struct {
	mu               sync.RWMutex
	workers          [WorkerPlayback + 1]*workerState
	progressInterval time.Duration
	sealed           bool
}

func NewRegistry(opts Options) *Registry {
	interval := opts.ProgressInterval
	if interval <= 0 {
		interval = DefaultProgressInterval
	}
	return &Registry{progressInterval: interval}
}

// Bind claims a fixed worker slot for its concrete loop owner.
func (r *Registry) Bind(id WorkerID, owner Owner) (*Worker, error) {
	if r == nil || !validBinding(id, owner) {
		return nil, fmt.Errorf("%w: worker=%s owner=%s", ErrInvalidBinding, id, owner)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return nil, ErrRegistrySealed
	}
	if r.workers[id] != nil {
		return nil, fmt.Errorf("%w: worker=%s", ErrDuplicateBinding, id)
	}
	state := &workerState{id: id, owner: owner}
	r.workers[id] = state
	return &Worker{state: state, progressInterval: r.progressInterval}, nil
}

// Seal verifies the complete five-worker library-only schema.
func (r *Registry) Seal() error {
	if r == nil {
		return fmt.Errorf("%w: nil registry", ErrInvalidBinding)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return ErrRegistrySealed
	}
	for _, id := range requiredWorkerIDs {
		if r.workers[id] == nil {
			return fmt.Errorf("%w: missing worker=%s", ErrIncompleteBinding, id)
		}
	}
	if got := r.workers[WorkerPlayback].owner; got != OwnerLibraryScheduler {
		return fmt.Errorf(
			"%w: playback owner=%s want=%s",
			ErrInvalidBinding,
			got,
			OwnerLibraryScheduler,
		)
	}
	r.sealed = true
	return nil
}

func validBinding(id WorkerID, owner Owner) bool {
	switch id {
	case WorkerTelegram:
		return owner == OwnerTelegram
	case WorkerOBSReconnect:
		return owner == OwnerOBSReconnect
	case WorkerOBSEvents:
		return owner == OwnerOBSEvents
	case WorkerMaintenance:
		return owner == OwnerMaintenance
	case WorkerPlayback:
		return owner == OwnerLibraryScheduler
	default:
		return false
	}
}

// Snapshots always returns exactly five entries in stable wire order.
func (r *Registry) Snapshots() [RequiredWorkerCount]Snapshot {
	var snapshots [RequiredWorkerCount]Snapshot
	if r == nil {
		for index, id := range requiredWorkerIDs {
			snapshots[index].ID = id
		}
		return snapshots
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	for index, id := range requiredWorkerIDs {
		snapshots[index].ID = id
		if state := r.workers[id]; state != nil {
			snapshots[index] = snapshotState(state)
		}
	}
	return snapshots
}

func snapshotState(state *workerState) Snapshot {
	value := state.value.Load()
	return Snapshot{
		ID:       state.id,
		Owner:    state.owner,
		Phase:    Phase(value >> sequenceBits),
		Sequence: value & sequenceMask,
	}
}

// Advance records an authoritative phase boundary. It invalidates any active
// special scopes so their deferred restore functions cannot overwrite the new
// phase. The sequence saturates rather than wrapping and making a healthy
// worker appear to move backwards.
func (w *Worker) Advance(phase Phase) uint64 {
	if w == nil || w.state == nil {
		return 0
	}
	if !phase.valid() {
		panic(fmt.Sprintf("invalid liveness phase %d", phase))
	}
	w.state.scopeMu.Lock()
	defer w.state.scopeMu.Unlock()
	w.state.clearScopesLocked()
	return w.state.publishLocked(phase)
}

func (w *Worker) Snapshot() Snapshot {
	if w == nil || w.state == nil {
		return Snapshot{}
	}
	return snapshotState(w.state)
}

// Scope enters one of the fixed long-operation phases and returns an
// idempotent token-bound handle. Handles may close in any order, the newest
// still-active scope remains effective, and the last close returns to the base
// phase. An authoritative Advance invalidates all outstanding tokens, making
// their later Close and Checkpoint calls no-ops.
func (w *Worker) Scope(phase Phase) ScopeHandle {
	if !phase.special() {
		panic(fmt.Sprintf("liveness scope requires special phase, got %s", phase))
	}
	if w == nil || w.state == nil {
		return ScopeHandle{}
	}

	state := w.state
	state.scopeMu.Lock()
	if state.activeCount == len(state.activeScopes) {
		state.scopeMu.Unlock()
		panic(fmt.Sprintf("liveness scope limit exceeded: %d", len(state.activeScopes)))
	}
	if state.activeCount == 0 {
		base := Phase(state.value.Load() >> sequenceBits)
		if !base.valid() || base.special() {
			base = PhaseOperation
		}
		state.scopeBase = base
	}
	token := state.nextScopeToken + 1
	if token == 0 {
		state.scopeMu.Unlock()
		panic("liveness scope token exhausted")
	}
	state.nextScopeToken = token
	state.activeScopes[state.activeCount] = activeScope{token: token, phase: phase}
	state.activeCount++
	state.publishLocked(phase)
	state.scopeMu.Unlock()

	return ScopeHandle{state: state, token: token, phase: phase}
}

// Close exits this handle's scope. It is idempotent and safe after an
// authoritative phase override.
func (handle ScopeHandle) Close() {
	if handle.state == nil || handle.token == 0 {
		return
	}
	handle.state.restoreScope(handle.token)
}

// Checkpoint records completed work only while this exact token is the
// effective active scope. A closed or invalidated handle, an outer handle
// hidden by a newer scope, and an old handle whose phase matches a newer token
// all become no-ops.
func (handle ScopeHandle) Checkpoint() uint64 {
	if handle.state == nil || handle.token == 0 {
		return 0
	}

	state := handle.state
	state.scopeMu.Lock()
	defer state.scopeMu.Unlock()
	current := state.value.Load()
	if state.activeCount == 0 {
		return current & sequenceMask
	}
	effective := state.activeScopes[state.activeCount-1]
	if effective.token != handle.token ||
		effective.phase != handle.phase {
		return current & sequenceMask
	}
	return state.publishLocked(handle.phase)
}

func (state *workerState) publishLocked(phase Phase) uint64 {
	current := state.value.Load()
	sequence := current & sequenceMask
	if sequence < sequenceMask {
		sequence++
	}
	state.value.Store(uint64(phase)<<sequenceBits | sequence)
	return sequence
}

func (state *workerState) clearScopesLocked() {
	for index := 0; index < state.activeCount; index++ {
		state.activeScopes[index] = activeScope{}
	}
	state.activeCount = 0
	state.scopeBase = PhaseUnknown
}

func (state *workerState) restoreScope(token uint64) {
	state.scopeMu.Lock()
	defer state.scopeMu.Unlock()

	index := -1
	for candidate := 0; candidate < state.activeCount; candidate++ {
		if state.activeScopes[candidate].token == token {
			index = candidate
			break
		}
	}
	if index < 0 {
		return
	}

	copy(state.activeScopes[index:], state.activeScopes[index+1:state.activeCount])
	state.activeCount--
	state.activeScopes[state.activeCount] = activeScope{}
	effective := state.scopeBase
	if state.activeCount > 0 {
		effective = state.activeScopes[state.activeCount-1].phase
	} else {
		state.scopeBase = PhaseUnknown
	}
	state.publishLocked(effective)
}

func (w *Worker) ProgressInterval() time.Duration {
	if w == nil || w.progressInterval <= 0 {
		return DefaultProgressInterval
	}
	return w.progressInterval
}

// Wait advances through the bound worker path throughout a scheduled or retry
// delay. This proves the loop is schedulable during a long capped backoff
// without claiming that an external dependency recovered.
func (w *Worker) Wait(ctx context.Context, phase Phase, delay time.Duration) error {
	if phase != PhaseScheduledWait && phase != PhaseRetryWait {
		panic(fmt.Sprintf("liveness wait cannot pulse phase %s", phase))
	}
	w.Advance(phase)
	if err := ctx.Err(); err != nil {
		w.Advance(PhaseCancelWait)
		return err
	}
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()
	pulse := time.NewTicker(w.ProgressInterval())
	defer pulse.Stop()
	for {
		select {
		case <-ctx.Done():
			w.Advance(PhaseCancelWait)
			return ctx.Err()
		case <-timer.C:
			return nil
		case <-pulse.C:
			w.Advance(phase)
		}
	}
}

type workerContextKey struct{}

// WithWorker associates a bound capability with the required worker's context.
func WithWorker(ctx context.Context, worker *Worker) context.Context {
	if worker == nil || worker.state == nil {
		panic("liveness worker is required")
	}
	return context.WithValue(ctx, workerContextKey{}, worker)
}

// WorkerFromContext returns a no-op capability for unsupervised unit-level
// calls that do not run through the app worker coordinator.
func WorkerFromContext(ctx context.Context) *Worker {
	if ctx != nil {
		if worker, ok := ctx.Value(workerContextKey{}).(*Worker); ok && worker != nil {
			return worker
		}
	}
	return &Worker{}
}
