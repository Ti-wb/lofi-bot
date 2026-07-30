package liveness

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestRequiredWorkerSchemaIsFixedAcrossPlaybackOwners(t *testing.T) {
	wantIDs := []string{
		"telegram",
		"obs-reconnect",
		"obs-events",
		"maintenance",
		"playback",
	}
	for _, playbackOwner := range []Owner{OwnerLibraryScheduler, OwnerPlaybackWatchdog} {
		registry, workers := completeRegistry(t, playbackOwner, time.Millisecond)
		snapshots := registry.Snapshots()
		if len(snapshots) != RequiredWorkerCount {
			t.Fatalf("snapshot count = %d, want %d", len(snapshots), RequiredWorkerCount)
		}
		for index, snapshot := range snapshots {
			if got := snapshot.ID.String(); got != wantIDs[index] {
				t.Fatalf("snapshot %d ID = %q, want %q", index, got, wantIDs[index])
			}
			if snapshot.ID == WorkerPlayback && snapshot.Owner != playbackOwner {
				t.Fatalf("playback owner = %s, want %s", snapshot.Owner, playbackOwner)
			}
			workers[snapshot.ID].Advance(PhaseStarting)
		}
	}
}

func TestPhaseSchemaIsFiniteAndStable(t *testing.T) {
	want := []struct {
		phase Phase
		name  string
	}{
		{phase: PhaseStarting, name: "starting"},
		{phase: PhaseOperation, name: "operation"},
		{phase: PhaseEventWait, name: "event-wait"},
		{phase: PhaseScheduledWait, name: "scheduled-wait"},
		{phase: PhaseRetryWait, name: "retry-wait"},
		{phase: PhaseCancelWait, name: "cancel-wait"},
		{phase: PhaseMediaCopy, name: "media-copy"},
		{phase: PhaseMediaProbe, name: "media-probe"},
		{phase: PhaseDurabilitySync, name: "durability-sync"},
		{phase: PhaseLibraryScan, name: "library-scan"},
	}
	for _, fixture := range want {
		if !fixture.phase.valid() {
			t.Fatalf("phase %d (%s) is not valid", fixture.phase, fixture.name)
		}
		if got := fixture.phase.String(); got != fixture.name {
			t.Fatalf("phase %d name = %q, want %q", fixture.phase, got, fixture.name)
		}
	}
	if Phase(len(want) + 1).valid() {
		t.Fatalf("unexpected phase %d is valid", len(want)+1)
	}
}

func TestPhaseScopeRestoresPreviousAndNestedPhases(t *testing.T) {
	_, workers := completeRegistry(t, OwnerPlaybackWatchdog, time.Millisecond)
	worker := workers[WorkerTelegram]
	worker.Advance(PhaseOperation)

	scanScope := worker.Scope(PhaseLibraryScan)
	if phase := worker.Snapshot().Phase; phase != PhaseLibraryScan {
		t.Fatalf("outer phase = %s, want %s", phase, PhaseLibraryScan)
	}
	probeScope := worker.Scope(PhaseMediaProbe)
	if phase := worker.Snapshot().Phase; phase != PhaseMediaProbe {
		t.Fatalf("nested phase = %s, want %s", phase, PhaseMediaProbe)
	}
	probeScope.Close()
	if phase := worker.Snapshot().Phase; phase != PhaseLibraryScan {
		t.Fatalf("nested restore phase = %s, want %s", phase, PhaseLibraryScan)
	}
	scanScope.Close()
	if phase := worker.Snapshot().Phase; phase != PhaseOperation {
		t.Fatalf("outer restore phase = %s, want %s", phase, PhaseOperation)
	}
}

func TestPhaseScopeDistinctScopesRestoreOutOfOrder(t *testing.T) {
	_, workers := completeRegistry(t, OwnerPlaybackWatchdog, time.Millisecond)
	worker := workers[WorkerTelegram]
	worker.Advance(PhaseOperation)

	copyScope := worker.Scope(PhaseMediaCopy)
	probeScope := worker.Scope(PhaseMediaProbe)
	copyScope.Close()
	if phase := worker.Snapshot().Phase; phase != PhaseMediaProbe {
		t.Fatalf("phase after outer restore = %s, want active inner %s", phase, PhaseMediaProbe)
	}
	probeScope.Close()
	if phase := worker.Snapshot().Phase; phase != PhaseOperation {
		t.Fatalf("phase after all restores = %s, want base %s", phase, PhaseOperation)
	}
}

func TestPhaseScopeSamePhaseOverlapRestoresToBase(t *testing.T) {
	_, workers := completeRegistry(t, OwnerPlaybackWatchdog, time.Millisecond)
	worker := workers[WorkerTelegram]
	worker.Advance(PhaseRetryWait)

	firstScope := worker.Scope(PhaseMediaCopy)
	secondScope := worker.Scope(PhaseMediaCopy)
	firstScope.Close()
	if phase := worker.Snapshot().Phase; phase != PhaseMediaCopy {
		t.Fatalf("phase after first restore = %s, want overlapping %s", phase, PhaseMediaCopy)
	}
	secondScope.Close()
	if phase := worker.Snapshot().Phase; phase != PhaseRetryWait {
		t.Fatalf("phase after overlapping restores = %s, want base %s", phase, PhaseRetryWait)
	}
}

func TestPhaseScopeCheckpointRejectsABASamePhaseToken(t *testing.T) {
	_, workers := completeRegistry(t, OwnerPlaybackWatchdog, time.Millisecond)
	worker := workers[WorkerTelegram]
	worker.Advance(PhaseOperation)

	oldScope := worker.Scope(PhaseMediaCopy)
	oldScope.Checkpoint()
	worker.Advance(PhaseCancelWait)
	newScope := worker.Scope(PhaseMediaCopy)
	beforeOldCheckpoint := worker.Snapshot()

	oldScope.Checkpoint()
	oldScope.Close()
	if got := worker.Snapshot(); got != beforeOldCheckpoint {
		t.Fatalf("old same-phase token advanced new scope: got=%+v want=%+v", got, beforeOldCheckpoint)
	}

	beforeNewCheckpoint := worker.Snapshot()
	newScope.Checkpoint()
	afterNewCheckpoint := worker.Snapshot()
	if afterNewCheckpoint.Sequence != beforeNewCheckpoint.Sequence+1 ||
		afterNewCheckpoint.Phase != PhaseMediaCopy {
		t.Fatalf(
			"new scope checkpoint = %+v, want media-copy sequence after %+v",
			afterNewCheckpoint,
			beforeNewCheckpoint,
		)
	}
	newScope.Close()
	if phase := worker.Snapshot().Phase; phase != PhaseCancelWait {
		t.Fatalf("new scope close phase = %s, want authoritative base %s", phase, PhaseCancelWait)
	}
}

func TestPhaseScopeCheckpointRequiresEffectiveToken(t *testing.T) {
	_, workers := completeRegistry(t, OwnerPlaybackWatchdog, time.Millisecond)
	worker := workers[WorkerTelegram]
	worker.Advance(PhaseOperation)

	outerScope := worker.Scope(PhaseMediaCopy)
	innerScope := worker.Scope(PhaseMediaCopy)
	beforeOuterCheckpoint := worker.Snapshot()
	outerScope.Checkpoint()
	if got := worker.Snapshot(); got != beforeOuterCheckpoint {
		t.Fatalf("hidden outer checkpoint advanced inner scope: got=%+v want=%+v", got, beforeOuterCheckpoint)
	}
	innerScope.Checkpoint()
	afterInnerCheckpoint := worker.Snapshot()
	if afterInnerCheckpoint.Sequence != beforeOuterCheckpoint.Sequence+1 {
		t.Fatalf("inner checkpoint sequence = %d, want %d", afterInnerCheckpoint.Sequence, beforeOuterCheckpoint.Sequence+1)
	}

	innerScope.Close()
	beforeOuterResumed := worker.Snapshot()
	outerScope.Checkpoint()
	afterOuterResumed := worker.Snapshot()
	if afterOuterResumed.Sequence != beforeOuterResumed.Sequence+1 ||
		afterOuterResumed.Phase != PhaseMediaCopy {
		t.Fatalf("resumed outer checkpoint = %+v, want advance after %+v", afterOuterResumed, beforeOuterResumed)
	}
	outerScope.Close()
	if phase := worker.Snapshot().Phase; phase != PhaseOperation {
		t.Fatalf("final outer close phase = %s, want %s", phase, PhaseOperation)
	}
}

func TestPhaseScopeRestoresOnErrorAndPanic(t *testing.T) {
	_, workers := completeRegistry(t, OwnerLibraryScheduler, time.Millisecond)
	worker := workers[WorkerTelegram]
	worker.Advance(PhaseRetryWait)

	wantErr := errors.New("probe failed")
	err := func() error {
		scope := worker.Scope(PhaseMediaProbe)
		defer scope.Close()
		return wantErr
	}()
	if !errors.Is(err, wantErr) {
		t.Fatalf("scoped error = %v, want %v", err, wantErr)
	}
	if phase := worker.Snapshot().Phase; phase != PhaseRetryWait {
		t.Fatalf("error restore phase = %s, want %s", phase, PhaseRetryWait)
	}

	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("scoped panic was not observed")
			}
		}()
		scope := worker.Scope(PhaseDurabilitySync)
		defer scope.Close()
		panic("sync panic")
	}()
	if phase := worker.Snapshot().Phase; phase != PhaseRetryWait {
		t.Fatalf("panic restore phase = %s, want %s", phase, PhaseRetryWait)
	}
}

func TestDistinctPhaseScopeRestoresAreConcurrentAndBounded(t *testing.T) {
	_, workers := completeRegistry(t, OwnerLibraryScheduler, time.Millisecond)
	worker := workers[WorkerTelegram]
	worker.Advance(PhaseOperation)

	phases := [...]Phase{
		PhaseMediaCopy,
		PhaseMediaProbe,
		PhaseDurabilitySync,
		PhaseLibraryScan,
	}
	scopes := make([]ScopeHandle, maxActiveScopes)
	for index := range scopes {
		scopes[index] = worker.Scope(phases[index%len(phases)])
	}
	entered := worker.Snapshot().Sequence

	var callers sync.WaitGroup
	for _, scope := range scopes {
		callers.Add(1)
		go func() {
			defer callers.Done()
			scope.Close()
		}()
	}
	callers.Wait()

	snapshot := worker.Snapshot()
	if snapshot.Phase != PhaseOperation {
		t.Fatalf("final concurrent restore phase = %s, want %s", snapshot.Phase, PhaseOperation)
	}
	if snapshot.Sequence != entered+uint64(len(scopes)) {
		t.Fatalf(
			"concurrent restore sequence = %d, want %d successful token closes after %d",
			snapshot.Sequence,
			len(scopes),
			entered,
		)
	}
}

func TestPhaseScopeConcurrentCloseCheckpointRace(t *testing.T) {
	_, workers := completeRegistry(t, OwnerPlaybackWatchdog, time.Millisecond)
	worker := workers[WorkerTelegram]

	for iteration := 0; iteration < 100; iteration++ {
		worker.Advance(PhaseOperation)
		scope := worker.Scope(PhaseMediaCopy)
		entered := worker.Snapshot().Sequence
		start := make(chan struct{})
		var callers sync.WaitGroup
		callers.Add(2)
		go func() {
			defer callers.Done()
			<-start
			scope.Checkpoint()
		}()
		go func() {
			defer callers.Done()
			<-start
			scope.Close()
		}()
		close(start)
		callers.Wait()

		snapshot := worker.Snapshot()
		if snapshot.Phase != PhaseOperation {
			t.Fatalf("iteration %d final phase = %s, want %s", iteration, snapshot.Phase, PhaseOperation)
		}
		if snapshot.Sequence < entered+1 || snapshot.Sequence > entered+2 {
			t.Fatalf(
				"iteration %d sequence = %d, want close plus optional pre-close checkpoint after %d",
				iteration,
				snapshot.Sequence,
				entered,
			)
		}
		beforeLateCheckpoint := snapshot
		scope.Checkpoint()
		if got := worker.Snapshot(); got != beforeLateCheckpoint {
			t.Fatalf("iteration %d late checkpoint changed closed scope: got=%+v want=%+v", iteration, got, beforeLateCheckpoint)
		}
	}
}

func TestPhaseScopeRestoreIsRaceSafeAndIdempotent(t *testing.T) {
	registry := NewRegistry(Options{})
	worker, err := registry.Bind(WorkerTelegram, OwnerTelegram)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	scope := worker.Scope(PhaseMediaCopy)
	entered := worker.Snapshot().Sequence

	var callers sync.WaitGroup
	for range 16 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			scope.Close()
		}()
	}
	callers.Wait()
	snapshot := worker.Snapshot()
	if snapshot.Phase != PhaseOperation {
		t.Fatalf("fallback restore phase = %s, want %s", snapshot.Phase, PhaseOperation)
	}
	if snapshot.Sequence != entered+1 {
		t.Fatalf("restore sequence = %d, want one advance after %d", snapshot.Sequence, entered)
	}
}

func TestPhaseScopeRejectsNormalPhasesAndBoundOverflow(t *testing.T) {
	_, workers := completeRegistry(t, OwnerPlaybackWatchdog, time.Millisecond)
	worker := workers[WorkerTelegram]

	assertPanics(t, "normal scope phase", func() {
		worker.Scope(PhaseOperation)
	})

	scopes := make([]ScopeHandle, maxActiveScopes)
	for index := range scopes {
		scopes[index] = worker.Scope(PhaseMediaCopy)
	}
	assertPanics(t, "scope overflow", func() {
		worker.Scope(PhaseMediaProbe)
	})
	for _, scope := range scopes {
		scope.Close()
	}
}

func TestPhaseScopeNoOpContextHandle(t *testing.T) {
	worker := WorkerFromContext(context.Background())
	scope := worker.Scope(PhaseMediaCopy)
	if sequence := scope.Checkpoint(); sequence != 0 {
		t.Fatalf("no-op checkpoint sequence = %d, want 0", sequence)
	}
	scope.Close()
	scope.Close()
	if snapshot := worker.Snapshot(); snapshot != (Snapshot{}) {
		t.Fatalf("no-op worker snapshot = %+v, want zero", snapshot)
	}
}

func TestSpecialScopeDoesNotLeakLeaseIntoSubsequentStalledOperation(t *testing.T) {
	_, workers := completeRegistry(t, OwnerPlaybackWatchdog, time.Millisecond)
	worker := workers[WorkerTelegram]
	worker.Advance(PhaseOperation)
	func() {
		scope := worker.Scope(PhaseLibraryScan)
		defer scope.Close()
	}()
	before := worker.Snapshot()
	if before.Phase != PhaseOperation {
		t.Fatalf("post-scope phase = %s, want normal operation lease", before.Phase)
	}
	time.Sleep(10 * time.Millisecond)
	after := worker.Snapshot()
	if after != before {
		t.Fatalf("stalled normal operation manufactured progress: before=%+v after=%+v", before, after)
	}
}

func TestRegistryRejectsMissingDuplicateAndModeMismatchedOwners(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		registry := NewRegistry(Options{})
		bindNonPlaybackWorkers(t, registry)
		if err := registry.Seal(OwnerLibraryScheduler); !errors.Is(err, ErrIncompleteBinding) {
			t.Fatalf("Seal error = %v, want %v", err, ErrIncompleteBinding)
		}
	})

	t.Run("duplicate", func(t *testing.T) {
		registry := NewRegistry(Options{})
		if _, err := registry.Bind(WorkerTelegram, OwnerTelegram); err != nil {
			t.Fatalf("first Bind: %v", err)
		}
		if _, err := registry.Bind(WorkerTelegram, OwnerTelegram); !errors.Is(err, ErrDuplicateBinding) {
			t.Fatalf("second Bind error = %v, want %v", err, ErrDuplicateBinding)
		}
	})

	t.Run("inactive playback alternative", func(t *testing.T) {
		registry := NewRegistry(Options{})
		bindNonPlaybackWorkers(t, registry)
		if _, err := registry.Bind(WorkerPlayback, OwnerPlaybackWatchdog); err != nil {
			t.Fatalf("Bind playback: %v", err)
		}
		if err := registry.Seal(OwnerLibraryScheduler); !errors.Is(err, ErrInvalidBinding) {
			t.Fatalf("Seal error = %v, want %v", err, ErrInvalidBinding)
		}
	})
}

func TestOneStuckWorkerCannotBeHiddenByOtherWorkerProgress(t *testing.T) {
	registry, workers := completeRegistry(t, OwnerLibraryScheduler, time.Millisecond)
	for _, worker := range workers {
		worker.Advance(PhaseOperation)
	}
	before := snapshotsByID(registry.Snapshots())

	stuckID := WorkerOBSReconnect
	for round := 0; round < 4; round++ {
		for id, worker := range workers {
			if id != stuckID {
				worker.Advance(PhaseOperation)
			}
		}
	}
	after := snapshotsByID(registry.Snapshots())

	for _, id := range RequiredWorkerIDs() {
		if id == stuckID {
			if after[id].Sequence != before[id].Sequence {
				t.Fatalf("stuck %s sequence advanced from %d to %d", id, before[id].Sequence, after[id].Sequence)
			}
			continue
		}
		if after[id].Sequence <= before[id].Sequence {
			t.Fatalf("active %s sequence did not advance: before=%d after=%d", id, before[id].Sequence, after[id].Sequence)
		}
	}
}

func TestRetryWaitAdvancesWithoutExternalSuccess(t *testing.T) {
	_, workers := completeRegistry(t, OwnerPlaybackWatchdog, 2*time.Millisecond)
	worker := workers[WorkerTelegram]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- worker.Wait(ctx, PhaseRetryWait, time.Second)
	}()

	deadline := time.After(250 * time.Millisecond)
	for worker.Snapshot().Sequence < 3 {
		select {
		case <-deadline:
			t.Fatalf("retry wait sequence = %d, want periodic progress", worker.Snapshot().Sequence)
		case <-time.After(time.Millisecond):
		}
	}
	snapshot := worker.Snapshot()
	if snapshot.Phase != PhaseRetryWait {
		t.Fatalf("phase = %s, want %s", snapshot.Phase, PhaseRetryWait)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v, want cancellation", err)
	}
	if phase := worker.Snapshot().Phase; phase != PhaseCancelWait {
		t.Fatalf("phase after cancellation = %s, want %s", phase, PhaseCancelWait)
	}
}

func TestWaitRejectsSyntheticProgressForOperationScopes(t *testing.T) {
	_, workers := completeRegistry(t, OwnerPlaybackWatchdog, time.Millisecond)
	worker := workers[WorkerTelegram]
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("media probe wait did not panic")
		}
	}()
	_ = worker.Wait(context.Background(), PhaseMediaProbe, time.Second)
}

func completeRegistry(
	t *testing.T,
	playbackOwner Owner,
	progressInterval time.Duration,
) (*Registry, map[WorkerID]*Worker) {
	t.Helper()
	registry := NewRegistry(Options{ProgressInterval: progressInterval})
	workers := bindNonPlaybackWorkers(t, registry)
	playback, err := registry.Bind(WorkerPlayback, playbackOwner)
	if err != nil {
		t.Fatalf("Bind playback: %v", err)
	}
	workers[WorkerPlayback] = playback
	if err := registry.Seal(playbackOwner); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return registry, workers
}

func bindNonPlaybackWorkers(t *testing.T, registry *Registry) map[WorkerID]*Worker {
	t.Helper()
	workers := make(map[WorkerID]*Worker)
	for _, binding := range []struct {
		id    WorkerID
		owner Owner
	}{
		{id: WorkerTelegram, owner: OwnerTelegram},
		{id: WorkerOBSReconnect, owner: OwnerOBSReconnect},
		{id: WorkerOBSEvents, owner: OwnerOBSEvents},
		{id: WorkerMaintenance, owner: OwnerMaintenance},
	} {
		worker, err := registry.Bind(binding.id, binding.owner)
		if err != nil {
			t.Fatalf("Bind(%s, %s): %v", binding.id, binding.owner, err)
		}
		workers[binding.id] = worker
	}
	return workers
}

func snapshotsByID(snapshots [RequiredWorkerCount]Snapshot) map[WorkerID]Snapshot {
	result := make(map[WorkerID]Snapshot, len(snapshots))
	for _, snapshot := range snapshots {
		if _, exists := result[snapshot.ID]; exists {
			panic(fmt.Sprintf("duplicate snapshot ID %s", snapshot.ID))
		}
		result[snapshot.ID] = snapshot
	}
	return result
}

func assertPanics(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatalf("%s did not panic", name)
		}
	}()
	fn()
}
