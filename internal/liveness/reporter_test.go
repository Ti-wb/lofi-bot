package liveness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	supervisorFDHelperMode     = "TG_OBS_TEST_FD3_HELPER"
	supervisorFDHelperIdentity = "TG_OBS_TEST_FD3_IDENTITY"
)

func TestEncodeFrameUsesExactStableSchema(t *testing.T) {
	snapshots := fixedSnapshots()
	snapshots[0].Sequence, snapshots[0].Phase = 1, PhaseUnknown
	snapshots[1].Sequence, snapshots[1].Phase = 2, PhaseStarting
	snapshots[2].Sequence, snapshots[2].Phase = 3, PhaseOperation
	snapshots[3].Sequence, snapshots[3].Phase = 4, PhaseEventWait
	snapshots[4].Sequence, snapshots[4].Phase = 5, PhaseLibraryScan

	frame, err := EncodeFrame(42, snapshots)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	const want = "TGOBS1 42 t=1,00 r=2,01 e=3,02 m=4,03 p=5,10\n"
	if got := string(frame); got != want {
		t.Fatalf("frame = %q, want %q", got, want)
	}
	if bytes.Count(frame, []byte{'\n'}) != 1 || frame[len(frame)-1] != '\n' {
		t.Fatalf("frame is not exactly one newline-terminated record: %q", frame)
	}
}

func TestEncodeFramePhaseCodesAreExplicitAndStable(t *testing.T) {
	tests := []struct {
		phase Phase
		code  string
	}{
		{PhaseUnknown, "00"},
		{PhaseStarting, "01"},
		{PhaseOperation, "02"},
		{PhaseEventWait, "03"},
		{PhaseScheduledWait, "04"},
		{PhaseRetryWait, "05"},
		{PhaseCancelWait, "06"},
		{PhaseMediaCopy, "07"},
		{PhaseMediaProbe, "08"},
		{PhaseDurabilitySync, "09"},
		{PhaseLibraryScan, "10"},
	}
	for _, test := range tests {
		t.Run(test.phase.String(), func(t *testing.T) {
			snapshots := fixedSnapshots()
			for index := range snapshots {
				snapshots[index].Phase = test.phase
			}
			frame, err := EncodeFrame(1, snapshots)
			if err != nil {
				t.Fatalf("EncodeFrame: %v", err)
			}
			for _, key := range []string{"t", "r", "e", "m", "p"} {
				if !bytes.Contains(frame, []byte(" "+key+"=0,"+test.code)) {
					t.Fatalf("frame %q lacks %s phase code %s", frame, key, test.code)
				}
			}
		})
	}
}

func TestEncodeFrameMaximumSizeIsBoundedForAtomicPipeWrite(t *testing.T) {
	snapshots := fixedSnapshots()
	for index := range snapshots {
		snapshots[index].Sequence = ^uint64(0)
		snapshots[index].Phase = PhaseLibraryScan
	}
	frame, err := EncodeFrame(^uint64(0), snapshots)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	if got, want := len(frame), 158; got != want {
		t.Fatalf("maximum frame bytes = %d, want %d", got, want)
	}
	if len(frame) > MaxFrameBytes {
		t.Fatalf("maximum frame bytes = %d, limit %d", len(frame), MaxFrameBytes)
	}
	if MaxFrameBytes >= 512 {
		t.Fatalf("frame limit %d must stay below POSIX minimum PIPE_BUF", MaxFrameBytes)
	}
}

func TestEncodeFrameRejectsSchemaDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*[RequiredWorkerCount]Snapshot)
		frame  uint64
	}{
		{
			name: "zero frame",
			mutate: func(*[RequiredWorkerCount]Snapshot) {
			},
			frame: 0,
		},
		{
			name: "wrong order",
			mutate: func(snapshots *[RequiredWorkerCount]Snapshot) {
				snapshots[0], snapshots[1] = snapshots[1], snapshots[0]
			},
			frame: 1,
		},
		{
			name: "wrong owner",
			mutate: func(snapshots *[RequiredWorkerCount]Snapshot) {
				snapshots[0].Owner = OwnerOBSEvents
			},
			frame: 1,
		},
		{
			name: "unknown phase value",
			mutate: func(snapshots *[RequiredWorkerCount]Snapshot) {
				snapshots[0].Phase = Phase(255)
			},
			frame: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshots := fixedSnapshots()
			test.mutate(&snapshots)
			if _, err := EncodeFrame(test.frame, snapshots); !errors.Is(err, ErrInvalidFrame) {
				t.Fatalf("EncodeFrame error = %v, want %v", err, ErrInvalidFrame)
			}
		})
	}
}

func TestFIFOFrameSinkWriteSemantics(t *testing.T) {
	frame := []byte("TGOBS1 1 t=0,00 r=0,00 e=0,00 m=0,00 p=0,00\n")
	tests := []struct {
		name          string
		results       []writeResult
		wantDelivered bool
		wantErr       error
		wantCalls     int
	}{
		{
			name:          "one complete syscall",
			results:       []writeResult{{n: len(frame)}},
			wantDelivered: true,
			wantCalls:     1,
		},
		{
			name:          "bounded EINTR retry",
			results:       []writeResult{{err: unix.EINTR}, {n: len(frame)}},
			wantDelivered: true,
			wantCalls:     2,
		},
		{
			name:      "EAGAIN drops whole frame",
			results:   []writeResult{{err: unix.EAGAIN}},
			wantCalls: 1,
		},
		{
			name:      "ENOBUFS drops whole frame",
			results:   []writeResult{{err: unix.ENOBUFS}},
			wantCalls: 1,
		},
		{
			name:      "zero write is partial",
			results:   []writeResult{{}},
			wantErr:   ErrPartialFrameWrite,
			wantCalls: 1,
		},
		{
			name:      "positive short write",
			results:   []writeResult{{n: 1}},
			wantErr:   ErrPartialFrameWrite,
			wantCalls: 1,
		},
		{
			name:      "positive short write with error",
			results:   []writeResult{{n: 1, err: unix.EPIPE}},
			wantErr:   ErrPartialFrameWrite,
			wantCalls: 1,
		},
		{
			name:      "broken reader",
			results:   []writeResult{{err: unix.EPIPE}},
			wantErr:   ErrFrameWrite,
			wantCalls: 1,
		},
		{
			name:      "bad descriptor",
			results:   []writeResult{{err: unix.EBADF}},
			wantErr:   ErrFrameWrite,
			wantCalls: 1,
		},
		{
			name: "EINTR retry bound",
			results: []writeResult{
				{err: unix.EINTR},
				{err: unix.EINTR},
				{err: unix.EINTR},
				{err: unix.EINTR},
			},
			wantErr:   ErrFrameWrite,
			wantCalls: maxEINTRRetries + 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			call := 0
			sink := &fifoFrameSink{
				fd: 91,
				write: func(fd int, got []byte) (int, error) {
					if fd != 91 {
						t.Fatalf("write fd = %d, want 91", fd)
					}
					if !bytes.Equal(got, frame) {
						t.Fatalf("write frame = %q, want unchanged %q", got, frame)
					}
					if call >= len(test.results) {
						t.Fatalf("unexpected write call %d", call+1)
					}
					result := test.results[call]
					call++
					return result.n, result.err
				},
				close: func(int) error { return nil },
			}
			delivered, err := sink.WriteFrame(frame)
			if delivered != test.wantDelivered {
				t.Fatalf("delivered = %v, want %v", delivered, test.wantDelivered)
			}
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if call != test.wantCalls {
				t.Fatalf("write calls = %d, want %d", call, test.wantCalls)
			}
		})
	}
}

func TestOpenSupervisorFIFOValidatesTypeDirectionAndFlags(t *testing.T) {
	t.Run("write pipe", func(t *testing.T) {
		fds := newUnixPipe(t)
		defer unix.Close(fds[0])
		sink, err := openSupervisorFIFO(fds[1])
		if err != nil {
			unix.Close(fds[1])
			t.Fatalf("openSupervisorFIFO: %v", err)
		}
		defer sink.Close()

		status, err := unix.FcntlInt(uintptr(fds[1]), unix.F_GETFL, 0)
		if err != nil {
			t.Fatalf("F_GETFL: %v", err)
		}
		if status&unix.O_ACCMODE != unix.O_WRONLY || status&unix.O_NONBLOCK == 0 {
			t.Fatalf("status flags = %#x, want O_WRONLY|O_NONBLOCK", status)
		}
		descriptor, err := unix.FcntlInt(uintptr(fds[1]), unix.F_GETFD, 0)
		if err != nil {
			t.Fatalf("F_GETFD: %v", err)
		}
		if descriptor&unix.FD_CLOEXEC == 0 {
			t.Fatalf("descriptor flags = %#x, want FD_CLOEXEC", descriptor)
		}
	})

	t.Run("read pipe", func(t *testing.T) {
		fds := newUnixPipe(t)
		defer unix.Close(fds[1])
		defer unix.Close(fds[0])
		if _, err := openSupervisorFIFO(fds[0]); !errors.Is(err, ErrInvalidSupervisorFIFO) {
			t.Fatalf("read-only pipe error = %v, want %v", err, ErrInvalidSupervisorFIFO)
		}
	})

	t.Run("read-write fifo", func(t *testing.T) {
		path := t.TempDir() + "/supervisor.fifo"
		if err := unix.Mkfifo(path, 0o600); err != nil {
			t.Fatalf("Mkfifo: %v", err)
		}
		fd, err := unix.Open(path, unix.O_RDWR, 0)
		if err != nil {
			t.Fatalf("open read-write FIFO: %v", err)
		}
		defer unix.Close(fd)
		if _, err := openSupervisorFIFO(fd); !errors.Is(err, ErrInvalidSupervisorFIFO) {
			t.Fatalf("read-write pipe error = %v, want %v", err, ErrInvalidSupervisorFIFO)
		}
	})

	t.Run("regular file", func(t *testing.T) {
		file, err := os.CreateTemp(t.TempDir(), "regular")
		if err != nil {
			t.Fatalf("CreateTemp: %v", err)
		}
		defer file.Close()
		if _, err := openSupervisorFIFO(int(file.Fd())); !errors.Is(err, ErrInvalidSupervisorFIFO) {
			t.Fatalf("regular file error = %v, want %v", err, ErrInvalidSupervisorFIFO)
		}
	})

	t.Run("directory", func(t *testing.T) {
		directory, err := os.Open(t.TempDir())
		if err != nil {
			t.Fatalf("open directory: %v", err)
		}
		defer directory.Close()
		if _, err := openSupervisorFIFO(int(directory.Fd())); !errors.Is(err, ErrInvalidSupervisorFIFO) {
			t.Fatalf("directory error = %v, want %v", err, ErrInvalidSupervisorFIFO)
		}
	})

	t.Run("socket", func(t *testing.T) {
		fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err != nil {
			t.Fatalf("Socketpair: %v", err)
		}
		defer unix.Close(fds[0])
		defer unix.Close(fds[1])
		if _, err := openSupervisorFIFO(fds[0]); !errors.Is(err, ErrInvalidSupervisorFIFO) {
			t.Fatalf("socket error = %v, want %v", err, ErrInvalidSupervisorFIFO)
		}
	})

	t.Run("missing", func(t *testing.T) {
		if _, err := openSupervisorFIFO(-1); !errors.Is(err, ErrInvalidSupervisorFIFO) {
			t.Fatalf("missing descriptor error = %v, want %v", err, ErrInvalidSupervisorFIFO)
		}
	})
}

func TestFIFOFrameSinkDropsFullPipeWithoutBlocking(t *testing.T) {
	fds := newUnixPipe(t)
	defer unix.Close(fds[0])
	sink, err := openSupervisorFIFO(fds[1])
	if err != nil {
		unix.Close(fds[1])
		t.Fatalf("openSupervisorFIFO: %v", err)
	}
	defer sink.Close()

	fill := bytes.Repeat([]byte{'x'}, 4096)
	for {
		if _, err := unix.Write(fds[1], fill); err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
				break
			}
			t.Fatalf("fill pipe: %v", err)
		}
	}

	start := time.Now()
	delivered, err := sink.WriteFrame([]byte("TGOBS1 full\n"))
	if err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if delivered {
		t.Fatal("full pipe frame was reported delivered")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("full-pipe write blocked for %s", elapsed)
	}
}

func TestFIFOFrameSinkTreatsLostReaderAsPermanentFailure(t *testing.T) {
	fds := newUnixPipe(t)
	sink, err := openSupervisorFIFO(fds[1])
	if err != nil {
		unix.Close(fds[0])
		unix.Close(fds[1])
		t.Fatalf("openSupervisorFIFO: %v", err)
	}
	defer sink.Close()
	if err := unix.Close(fds[0]); err != nil {
		t.Fatalf("close pipe reader: %v", err)
	}

	delivered, err := sink.WriteFrame([]byte("TGOBS1 lost-reader\n"))
	if delivered {
		t.Fatal("lost-reader frame was reported delivered")
	}
	if !errors.Is(err, ErrFrameWrite) || !errors.Is(err, unix.EPIPE) {
		t.Fatalf("lost-reader error = %v, want %v wrapping EPIPE", err, ErrFrameWrite)
	}
}

func TestOpenSupervisorFD3EnvironmentContract(t *testing.T) {
	t.Run("absent marker does not touch descriptor", func(t *testing.T) {
		runSupervisorFDHelper(t, "absent", pipeExtraFile(t))
	})
	t.Run("malformed marker fails before descriptor access", func(t *testing.T) {
		for _, fixture := range []struct {
			name  string
			value string
		}{
			{name: "empty", value: ""},
			{name: "zero", value: "0"},
			{name: "word", value: "true"},
			{name: "leading zero", value: "01"},
			{name: "whitespace", value: " 1"},
		} {
			t.Run(fixture.name, func(t *testing.T) {
				runSupervisorFDHelper(t, "malformed", pipeExtraFile(t), fixture.value)
			})
		}
	})
	t.Run("missing descriptor is rejected", func(t *testing.T) {
		runSupervisorFDHelper(t, "missing", pipeExtraFile(t))
	})
	t.Run("regular descriptor is rejected", func(t *testing.T) {
		file, err := os.CreateTemp(t.TempDir(), "regular")
		if err != nil {
			t.Fatalf("CreateTemp: %v", err)
		}
		runSupervisorFDHelper(t, "regular", file)
		file.Close()
	})
	t.Run("FIFO is close-on-exec across a direct exec", func(t *testing.T) {
		runSupervisorFDHelper(t, "cloexec-stage1", pipeExtraFile(t))
	})
}

func TestReporterPublishesImmediatelyWithoutMutatingRegistry(t *testing.T) {
	registry, workers := completeRegistry(t, time.Millisecond)
	for _, id := range RequiredWorkerIDs() {
		workers[id].Advance(PhaseOperation)
	}
	before := registry.Snapshots()
	sink := &recordingFrameSink{}
	reporter, err := NewReporter(
		registry,
		sink,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		time.Hour,
	)
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	results, err := reporter.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := sink.frameCount(); got != 1 {
		t.Fatalf("frames on Start return = %d, want synchronous first frame", got)
	}
	if after := registry.Snapshots(); after != before {
		t.Fatalf("reporter mutated snapshots: before=%+v after=%+v", before, after)
	}

	cancel()
	select {
	case err := <-results:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("reporter result = %v, want cancellation", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("reporter did not stop and close after cancellation")
	}
	if !sink.isClosed() {
		t.Fatal("reporter did not close sink on cancellation")
	}
}

func TestReporterAdvancesFrameOnDropAndReturnsPermanentFailure(t *testing.T) {
	registry, _ := completeRegistry(t, time.Millisecond)
	wantErr := errors.New("supervisor vanished")
	sink := &recordingFrameSink{
		results: []sinkResult{
			{delivered: false},
			{err: wantErr},
		},
	}
	reporter, err := NewReporter(
		registry,
		sink,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		time.Millisecond,
	)
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}
	results, err := reporter.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case err := <-results:
		if !errors.Is(err, wantErr) {
			t.Fatalf("reporter result = %v, want %v", err, wantErr)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("reporter did not return permanent sink failure")
	}
	frames := sink.framesCopy()
	if len(frames) != 2 ||
		!bytes.HasPrefix(frames[0], []byte("TGOBS1 1 ")) ||
		!bytes.HasPrefix(frames[1], []byte("TGOBS1 2 ")) {
		t.Fatalf("frame attempts = %q, want consumed drop sequence 1 then 2", frames)
	}
	if !sink.isClosed() {
		t.Fatal("reporter did not close failed sink")
	}
}

func TestReporterInitialFailureIsSynchronousAndClosesSink(t *testing.T) {
	registry, _ := completeRegistry(t, time.Millisecond)
	wantErr := errors.New("initial write failed")
	sink := &recordingFrameSink{results: []sinkResult{{err: wantErr}}}
	reporter, err := NewReporter(registry, sink, nil, time.Second)
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}
	results, err := reporter.Start(context.Background())
	if results != nil {
		t.Fatal("failed Start returned a result channel")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("Start error = %v, want %v", err, wantErr)
	}
	if !sink.isClosed() {
		t.Fatal("initial failure did not close sink")
	}
}

func fixedSnapshots() [RequiredWorkerCount]Snapshot {
	return [RequiredWorkerCount]Snapshot{
		{ID: WorkerTelegram, Owner: OwnerTelegram},
		{ID: WorkerOBSReconnect, Owner: OwnerOBSReconnect},
		{ID: WorkerOBSEvents, Owner: OwnerOBSEvents},
		{ID: WorkerMaintenance, Owner: OwnerMaintenance},
		{ID: WorkerPlayback, Owner: OwnerLibraryScheduler},
	}
}

type writeResult struct {
	n   int
	err error
}

func newUnixPipe(t *testing.T) [2]int {
	t.Helper()
	var fds [2]int
	if err := unix.Pipe(fds[:]); err != nil {
		t.Fatalf("pipe: %v", err)
	}
	return fds
}

func pipeExtraFile(t *testing.T) *os.File {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() {
		reader.Close()
		writer.Close()
	})
	return writer
}

func runSupervisorFDHelper(t *testing.T, mode string, extra *os.File, markerOverride ...string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSupervisorFDHelperProcess$")
	cmd.Env = append(
		removeEnvironment(os.Environ(), supervisorFDHelperMode),
		supervisorFDHelperMode+"="+mode,
	)
	cmd.Env = removeEnvironment(cmd.Env, SupervisorMarkerEnv)
	switch mode {
	case "absent":
	case "malformed":
		value := "true"
		if len(markerOverride) > 0 {
			value = markerOverride[0]
		}
		cmd.Env = append(cmd.Env, SupervisorMarkerEnv+"="+value)
	default:
		cmd.Env = append(cmd.Env, SupervisorMarkerEnv+"="+SupervisorMarkerValue)
	}
	cmd.ExtraFiles = []*os.File{extra}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("FD3 helper mode %q: %v\n%s", mode, err, output)
	}
}

func TestSupervisorFDHelperProcess(t *testing.T) {
	mode := os.Getenv(supervisorFDHelperMode)
	if mode == "" {
		return
	}
	switch mode {
	case "absent":
		beforeStatus, statusErr := unix.FcntlInt(supervisorFD, unix.F_GETFL, 0)
		beforeDescriptor, descriptorErr := unix.FcntlInt(supervisorFD, unix.F_GETFD, 0)
		if statusErr != nil || descriptorErr != nil {
			os.Exit(101)
		}
		sink, err := OpenSupervisorFD3FromEnv()
		if err != nil || sink != nil {
			os.Exit(102)
		}
		afterStatus, statusErr := unix.FcntlInt(supervisorFD, unix.F_GETFL, 0)
		afterDescriptor, descriptorErr := unix.FcntlInt(supervisorFD, unix.F_GETFD, 0)
		if statusErr != nil || descriptorErr != nil ||
			beforeStatus != afterStatus ||
			beforeDescriptor != afterDescriptor {
			os.Exit(103)
		}
	case "malformed":
		beforeStatus, statusErr := unix.FcntlInt(supervisorFD, unix.F_GETFL, 0)
		beforeDescriptor, descriptorErr := unix.FcntlInt(supervisorFD, unix.F_GETFD, 0)
		if statusErr != nil || descriptorErr != nil {
			os.Exit(113)
		}
		sink, err := OpenSupervisorFD3FromEnv()
		if sink != nil || !errors.Is(err, ErrInvalidSupervisorMarker) {
			os.Exit(114)
		}
		afterStatus, statusErr := unix.FcntlInt(supervisorFD, unix.F_GETFL, 0)
		afterDescriptor, descriptorErr := unix.FcntlInt(supervisorFD, unix.F_GETFD, 0)
		if statusErr != nil || descriptorErr != nil ||
			beforeStatus != afterStatus ||
			beforeDescriptor != afterDescriptor {
			os.Exit(115)
		}
	case "missing":
		if err := unix.Close(supervisorFD); err != nil {
			os.Exit(104)
		}
		sink, err := OpenSupervisorFD3FromEnv()
		if sink != nil || !errors.Is(err, ErrInvalidSupervisorFIFO) {
			os.Exit(105)
		}
	case "regular":
		sink, err := OpenSupervisorFD3FromEnv()
		if sink != nil || !errors.Is(err, ErrInvalidSupervisorFIFO) {
			os.Exit(106)
		}
	case "cloexec-stage1":
		sink, err := OpenSupervisorFD3FromEnv()
		if err != nil || sink == nil {
			os.Exit(107)
		}
		var stat unix.Stat_t
		if err := unix.Fstat(supervisorFD, &stat); err != nil {
			os.Exit(108)
		}
		identity := strconv.FormatUint(uint64(stat.Dev), 10) + ":" +
			strconv.FormatUint(uint64(stat.Ino), 10)
		environment := append(
			removeEnvironment(os.Environ(), supervisorFDHelperMode),
			supervisorFDHelperMode+"=cloexec-stage2",
			supervisorFDHelperIdentity+"="+identity,
		)
		if err := unix.Exec(
			os.Args[0],
			[]string{os.Args[0], "-test.run=^TestSupervisorFDHelperProcess$"},
			environment,
		); err != nil {
			os.Exit(109)
		}
	case "cloexec-stage2":
		var stat unix.Stat_t
		err := unix.Fstat(supervisorFD, &stat)
		if errors.Is(err, unix.EBADF) {
			return
		}
		if err != nil {
			os.Exit(110)
		}
		got := strconv.FormatUint(uint64(stat.Dev), 10) + ":" +
			strconv.FormatUint(uint64(stat.Ino), 10)
		if got == os.Getenv(supervisorFDHelperIdentity) {
			os.Exit(111)
		}
	default:
		os.Exit(112)
	}
}

func removeEnvironment(environment []string, key string) []string {
	prefix := key + "="
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

type sinkResult struct {
	delivered bool
	err       error
}

type recordingFrameSink struct {
	mu      sync.Mutex
	frames  [][]byte
	results []sinkResult
	closed  bool
}

func (sink *recordingFrameSink) WriteFrame(frame []byte) (bool, error) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.frames = append(sink.frames, bytes.Clone(frame))
	if len(sink.results) == 0 {
		return true, nil
	}
	result := sink.results[0]
	sink.results = sink.results[1:]
	return result.delivered, result.err
}

func (sink *recordingFrameSink) Close() error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.closed = true
	return nil
}

func (sink *recordingFrameSink) frameCount() int {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return len(sink.frames)
}

func (sink *recordingFrameSink) framesCopy() [][]byte {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	frames := make([][]byte, len(sink.frames))
	for index, frame := range sink.frames {
		frames[index] = bytes.Clone(frame)
	}
	return frames
}

func (sink *recordingFrameSink) isClosed() bool {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.closed
}

func TestReporterDropLoggingIsSampledAndRecoveryIsSingle(t *testing.T) {
	registry, _ := completeRegistry(t, time.Millisecond)
	results := make([]sinkResult, droppedFrameLogEvery)
	results = append(results, sinkResult{delivered: true})
	sink := &recordingFrameSink{results: results}
	var logs bytes.Buffer
	reporter, err := NewReporter(
		registry,
		sink,
		slog.New(slog.NewTextHandler(&logs, nil)),
		time.Microsecond,
	)
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}
	for attempt := 0; attempt < droppedFrameLogEvery+1; attempt++ {
		if err := reporter.report(); err != nil {
			t.Fatalf("report attempt %d: %v", attempt+1, err)
		}
	}

	logged := logs.String()
	if got := strings.Count(logged, "liveness frame dropped"); got != 2 {
		t.Fatalf("drop log count = %d, want first and %dth; logs=%s", got, droppedFrameLogEvery, logged)
	}
	if got := strings.Count(logged, "liveness pipe recovered"); got != 1 {
		t.Fatalf("recovery log count = %d, want 1; logs=%s", got, logged)
	}
	if strings.Contains(logged, "telegram") || strings.Contains(logged, "owner") {
		t.Fatalf("liveness log included worker/free-text data: %s", logged)
	}
}

func ExampleEncodeFrame() {
	frame, _ := EncodeFrame(1, fixedSnapshots())
	fmt.Print(string(frame))
	// Output:
	// TGOBS1 1 t=0,00 r=0,00 e=0,00 m=0,00 p=0,00
}
