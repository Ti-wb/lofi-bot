package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/tiwb/tg-obs-bot/internal/liveness"
)

type Manager struct {
	ffprobePath string
}

type Metadata struct {
	SizeBytes       int64
	DurationSeconds int
	HasVideoStream  bool
	HasAudioStream  bool
}

type DiskUsage struct {
	TotalBytes     uint64
	AvailableBytes uint64
}

func NewManager(ffprobePath string) *Manager {
	return &Manager{ffprobePath: ffprobePath}
}

func (m *Manager) Probe(ctx context.Context, path string) (Metadata, error) {
	tracker := liveness.WorkerFromContext(ctx)
	probeScope := tracker.Scope(liveness.PhaseMediaProbe)
	defer probeScope.Close()

	stat, err := os.Stat(path)
	if err != nil {
		return Metadata{}, err
	}
	meta := Metadata{SizeBytes: stat.Size()}

	if m.ffprobePath == "" {
		return meta, nil
	}
	cmd := exec.CommandContext(ctx, m.ffprobePath,
		"-v", "error",
		"-show_entries", "format=duration:stream=codec_type",
		"-of", "json",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return meta, ctxErr
		}
		return meta, fmt.Errorf("ffprobe failed: %w", err)
	}
	var result struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			CodecType string `json:"codec_type"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return meta, err
	}
	if result.Format.Duration != "" {
		duration, err := strconv.ParseFloat(result.Format.Duration, 64)
		if err == nil && duration > 0 && !math.IsNaN(duration) && !math.IsInf(duration, 0) {
			rounded := math.Ceil(duration)
			if rounded <= float64(1<<31-1) {
				meta.DurationSeconds = int(rounded)
			}
		}
	}
	for _, stream := range result.Streams {
		switch strings.ToLower(strings.TrimSpace(stream.CodecType)) {
		case "video":
			meta.HasVideoStream = true
		case "audio":
			meta.HasAudioStream = true
		}
	}
	return meta, nil
}

func (m *Manager) Validate(meta Metadata, maxBytes int64, maxDurationSeconds int) error {
	if meta.SizeBytes <= 0 {
		return errors.New("file is empty")
	}
	if meta.SizeBytes > maxBytes {
		return fmt.Errorf("file exceeds max size of %d bytes", maxBytes)
	}
	if maxDurationSeconds > 0 && meta.DurationSeconds <= 0 {
		return errors.New("video duration is unavailable or invalid")
	}
	if maxDurationSeconds > 0 && meta.DurationSeconds > maxDurationSeconds {
		return fmt.Errorf("video exceeds max duration of %d seconds", maxDurationSeconds)
	}
	return nil
}

func (m *Manager) ValidateVideo(meta Metadata, maxBytes int64, maxDurationSeconds int) error {
	if err := m.Validate(meta, maxBytes, maxDurationSeconds); err != nil {
		return err
	}
	if !meta.HasVideoStream {
		return errors.New("media does not contain a video stream")
	}
	return nil
}

func (m *Manager) ValidateAudio(meta Metadata, maxBytes int64, maxDurationSeconds int) error {
	if err := m.Validate(meta, maxBytes, maxDurationSeconds); err != nil {
		return err
	}
	if !meta.HasAudioStream {
		return errors.New("media does not contain an audio stream")
	}
	return nil
}

func DiskUsageForPath(path string) (DiskUsage, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return DiskUsage{}, err
	}
	blockSize := uint64(stat.Bsize)
	return DiskUsage{
		TotalBytes:     stat.Blocks * blockSize,
		AvailableBytes: stat.Bavail * blockSize,
	}, nil
}
