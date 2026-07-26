package liveness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// SupervisorMarkerEnv is deliberately only a presence marker. The
	// supervised process protocol always uses descriptor 3 and has no
	// configurable descriptor or path.
	SupervisorMarkerEnv = "TG_OBS_LIVENESS_FD3"

	SupervisorMarkerValue = "1"

	DefaultReportInterval = 10 * time.Second
	MaxFrameBytes         = 192

	supervisorFD         = 3
	maxEINTRRetries      = 3
	droppedFrameLogEvery = 64
)

var (
	ErrInvalidSupervisorMarker = errors.New("invalid liveness supervisor marker")
	ErrInvalidSupervisorFIFO   = errors.New("invalid liveness supervisor FIFO")
	ErrInvalidFrame            = errors.New("invalid liveness frame")
	ErrFrameTooLarge           = errors.New("liveness frame exceeds atomic size limit")
	ErrPartialFrameWrite       = errors.New("partial liveness frame write")
	ErrFrameWrite              = errors.New("liveness frame write failed")
	ErrReporterStarted         = errors.New("liveness reporter already started")
)

// FrameSink accepts one complete, pre-encoded supervisor frame. A false
// delivered result with a nil error means the entire frame was dropped under
// nonblocking backpressure.
type FrameSink interface {
	WriteFrame(frame []byte) (delivered bool, err error)
	Close() error
}

// OpenSupervisorFD3FromEnv returns nil without inspecting descriptor 3 when
// the supervision marker is absent. When present, only the exact value "1" is
// accepted and descriptor 3 must be a write-only FIFO.
func OpenSupervisorFD3FromEnv() (FrameSink, error) {
	value, present := os.LookupEnv(SupervisorMarkerEnv)
	if !present {
		return nil, nil
	}
	if value != SupervisorMarkerValue {
		return nil, fmt.Errorf(
			"%w: %s must equal %q",
			ErrInvalidSupervisorMarker,
			SupervisorMarkerEnv,
			SupervisorMarkerValue,
		)
	}

	sink, err := openSupervisorFIFO(supervisorFD)
	if err != nil {
		// The marker transfers ownership of descriptor 3 to this process. Close
		// a present-but-invalid descriptor on every failed initialization path.
		_ = unix.Close(supervisorFD)
		return nil, err
	}
	return sink, nil
}

type fifoFrameSink struct {
	mu       sync.Mutex
	fd       int
	write    func(int, []byte) (int, error)
	close    func(int) error
	closed   bool
	closeErr error
}

func openSupervisorFIFO(fd int) (*fifoFrameSink, error) {
	descriptorFlags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: descriptor flags: %w", ErrInvalidSupervisorFIFO, err)
	}
	if _, err := unix.FcntlInt(
		uintptr(fd),
		unix.F_SETFD,
		descriptorFlags|unix.FD_CLOEXEC,
	); err != nil {
		return nil, fmt.Errorf("%w: set close-on-exec: %w", ErrInvalidSupervisorFIFO, err)
	}

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, fmt.Errorf("%w: inspect descriptor: %w", ErrInvalidSupervisorFIFO, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFIFO {
		return nil, fmt.Errorf("%w: descriptor 3 is not a FIFO", ErrInvalidSupervisorFIFO)
	}

	statusFlags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: status flags: %w", ErrInvalidSupervisorFIFO, err)
	}
	if statusFlags&unix.O_ACCMODE != unix.O_WRONLY {
		return nil, fmt.Errorf("%w: descriptor 3 is not write-only", ErrInvalidSupervisorFIFO)
	}
	if _, err := unix.FcntlInt(
		uintptr(fd),
		unix.F_SETFL,
		statusFlags|unix.O_NONBLOCK,
	); err != nil {
		return nil, fmt.Errorf("%w: set nonblocking: %w", ErrInvalidSupervisorFIFO, err)
	}

	return &fifoFrameSink{
		fd:    fd,
		write: unix.Write,
		close: unix.Close,
	}, nil
}

func (sink *fifoFrameSink) WriteFrame(frame []byte) (bool, error) {
	if len(frame) == 0 {
		return false, fmt.Errorf("%w: empty", ErrInvalidFrame)
	}
	if len(frame) > MaxFrameBytes {
		return false, fmt.Errorf("%w: bytes=%d limit=%d", ErrFrameTooLarge, len(frame), MaxFrameBytes)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.closed {
		return false, fmt.Errorf("%w: descriptor closed", ErrFrameWrite)
	}

	for retry := 0; ; retry++ {
		written, err := sink.write(sink.fd, frame)
		if written > 0 && written != len(frame) {
			return false, fmt.Errorf(
				"%w: wrote=%d want=%d: %v",
				ErrPartialFrameWrite,
				written,
				len(frame),
				err,
			)
		}
		if written == len(frame) {
			if err != nil {
				return false, fmt.Errorf("%w after complete syscall: %w", ErrFrameWrite, err)
			}
			return true, nil
		}
		if err == nil {
			return false, fmt.Errorf(
				"%w: wrote=%d want=%d",
				ErrPartialFrameWrite,
				written,
				len(frame),
			)
		}
		if written <= 0 && errors.Is(err, unix.EINTR) && retry < maxEINTRRetries {
			continue
		}
		if written <= 0 &&
			(errors.Is(err, unix.EAGAIN) ||
				errors.Is(err, unix.EWOULDBLOCK) ||
				errors.Is(err, unix.ENOBUFS)) {
			return false, nil
		}
		return false, fmt.Errorf("%w: %w", ErrFrameWrite, err)
	}
}

func (sink *fifoFrameSink) Close() error {
	if sink == nil {
		return nil
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.closed {
		return sink.closeErr
	}
	sink.closed = true
	sink.closeErr = sink.close(sink.fd)
	return sink.closeErr
}

// EncodeFrame constructs one complete TGOBS1 frame in the stable supervisor
// order. It rejects schema drift before any bytes reach the pipe.
func EncodeFrame(
	frame uint64,
	snapshots [RequiredWorkerCount]Snapshot,
) ([]byte, error) {
	if frame == 0 {
		return nil, fmt.Errorf("%w: frame sequence must be positive", ErrInvalidFrame)
	}

	keys := [...]byte{'t', 'r', 'e', 'm', 'p'}
	encoded := make([]byte, 0, MaxFrameBytes)
	encoded = append(encoded, "TGOBS1 "...)
	encoded = strconv.AppendUint(encoded, frame, 10)
	for index, expectedID := range requiredWorkerIDs {
		snapshot := snapshots[index]
		if snapshot.ID != expectedID || !validBinding(snapshot.ID, snapshot.Owner) {
			return nil, fmt.Errorf(
				"%w: position=%d worker=%s owner=%s",
				ErrInvalidFrame,
				index,
				snapshot.ID,
				snapshot.Owner,
			)
		}
		code, ok := phaseWireCode(snapshot.Phase)
		if !ok {
			return nil, fmt.Errorf(
				"%w: worker=%s phase=%d",
				ErrInvalidFrame,
				snapshot.ID,
				snapshot.Phase,
			)
		}
		encoded = append(encoded, ' ', keys[index], '=')
		encoded = strconv.AppendUint(encoded, snapshot.Sequence, 10)
		encoded = append(encoded, ',', code[0], code[1])
	}
	encoded = append(encoded, '\n')
	if len(encoded) > MaxFrameBytes {
		return nil, fmt.Errorf(
			"%w: bytes=%d limit=%d",
			ErrFrameTooLarge,
			len(encoded),
			MaxFrameBytes,
		)
	}
	return encoded, nil
}

func phaseWireCode(phase Phase) ([2]byte, bool) {
	switch phase {
	case PhaseUnknown:
		return [2]byte{'0', '0'}, true
	case PhaseStarting:
		return [2]byte{'0', '1'}, true
	case PhaseOperation:
		return [2]byte{'0', '2'}, true
	case PhaseEventWait:
		return [2]byte{'0', '3'}, true
	case PhaseScheduledWait:
		return [2]byte{'0', '4'}, true
	case PhaseRetryWait:
		return [2]byte{'0', '5'}, true
	case PhaseCancelWait:
		return [2]byte{'0', '6'}, true
	case PhaseMediaCopy:
		return [2]byte{'0', '7'}, true
	case PhaseMediaProbe:
		return [2]byte{'0', '8'}, true
	case PhaseDurabilitySync:
		return [2]byte{'0', '9'}, true
	case PhaseLibraryScan:
		return [2]byte{'1', '0'}, true
	default:
		return [2]byte{}, false
	}
}

// Reporter observes Registry snapshots without owning any worker capability.
// Start publishes the first frame synchronously, then runs one ticker goroutine.
type Reporter struct {
	registry *Registry
	sink     FrameSink
	logger   *slog.Logger
	interval time.Duration

	startMu sync.Mutex
	started bool
	frame   uint64
	drops   uint64
}

func NewReporter(
	registry *Registry,
	sink FrameSink,
	logger *slog.Logger,
	interval time.Duration,
) (*Reporter, error) {
	if registry == nil || sink == nil {
		return nil, fmt.Errorf("%w: reporter requires registry and sink", ErrInvalidFrame)
	}
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = DefaultReportInterval
	}
	return &Reporter{
		registry: registry,
		sink:     sink,
		logger:   logger,
		interval: interval,
	}, nil
}

// Start emits frame 1 before returning. A nil start error guarantees that the
// returned channel will receive exactly one terminal result.
func (reporter *Reporter) Start(ctx context.Context) (<-chan error, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	reporter.startMu.Lock()
	if reporter.started {
		reporter.startMu.Unlock()
		return nil, ErrReporterStarted
	}
	reporter.started = true
	reporter.startMu.Unlock()

	if err := reporter.report(); err != nil {
		return nil, errors.Join(err, reporter.sink.Close())
	}

	result := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(reporter.interval)
		defer ticker.Stop()
		var runErr error
		for {
			select {
			case <-ctx.Done():
				runErr = ctx.Err()
			case <-ticker.C:
				if err := reporter.report(); err != nil {
					runErr = err
				}
			}
			if runErr != nil {
				result <- errors.Join(runErr, reporter.sink.Close())
				return
			}
		}
	}()
	return result, nil
}

func (reporter *Reporter) report() error {
	if reporter.frame == ^uint64(0) {
		return fmt.Errorf("%w: frame sequence exhausted", ErrInvalidFrame)
	}
	reporter.frame++
	frame, err := EncodeFrame(reporter.frame, reporter.registry.Snapshots())
	if err != nil {
		return err
	}
	delivered, err := reporter.sink.WriteFrame(frame)
	if err != nil {
		return err
	}
	if delivered {
		if reporter.drops != 0 {
			reporter.logger.Info(
				"liveness pipe recovered",
				"dropped_frames",
				reporter.drops,
				"frame",
				reporter.frame,
			)
			reporter.drops = 0
		}
		return nil
	}

	reporter.drops++
	if reporter.drops == 1 || reporter.drops%droppedFrameLogEvery == 0 {
		reporter.logger.Warn(
			"liveness frame dropped",
			"dropped_frames",
			reporter.drops,
			"frame",
			reporter.frame,
		)
	}
	return nil
}

func (reporter *Reporter) Close() error {
	if reporter == nil || reporter.sink == nil {
		return nil
	}
	return reporter.sink.Close()
}
