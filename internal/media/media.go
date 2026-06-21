package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Manager struct {
	dir         string
	ffprobePath string
	client      *http.Client
}

type Metadata struct {
	SizeBytes       int64
	DurationSeconds int
}

type DiskUsage struct {
	TotalBytes     uint64
	AvailableBytes uint64
}

func NewManager(dir, ffprobePath string) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Manager{
		dir:         dir,
		ffprobePath: ffprobePath,
		client: &http.Client{
			Timeout: 30 * time.Minute,
		},
	}, nil
}

func (m *Manager) Dir() string {
	return m.dir
}

func (m *Manager) Download(ctx context.Context, url, originalName string, maxBytes int64) (string, Metadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", Metadata{}, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return "", Metadata{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", Metadata{}, fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return "", Metadata{}, fmt.Errorf("file is too large: %d bytes", resp.ContentLength)
	}

	ext := strings.ToLower(filepath.Ext(originalName))
	if ext == "" {
		ext = ".mp4"
	}
	tmp, err := os.CreateTemp(m.dir, "download-*.tmp")
	if err != nil {
		return "", Metadata{}, err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	hasher := sha256.New()
	limited := io.LimitReader(resp.Body, maxBytes+1)
	written, err := io.Copy(io.MultiWriter(tmp, hasher), limited)
	closeErr := tmp.Close()
	if err != nil {
		return "", Metadata{}, err
	}
	if closeErr != nil {
		return "", Metadata{}, closeErr
	}
	if written > maxBytes {
		return "", Metadata{}, fmt.Errorf("file exceeds max size of %d bytes", maxBytes)
	}

	hash := hex.EncodeToString(hasher.Sum(nil))[:16]
	finalPath := filepath.Join(m.dir, fmt.Sprintf("%d-%s%s", time.Now().Unix(), hash, ext))
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return "", Metadata{}, err
	}

	meta, err := m.Probe(ctx, finalPath)
	if err != nil {
		return finalPath, Metadata{SizeBytes: written}, err
	}
	meta.SizeBytes = written
	return finalPath, meta, nil
}

func (m *Manager) Probe(ctx context.Context, path string) (Metadata, error) {
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
		"-show_entries", "format=duration",
		"-of", "json",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return meta, fmt.Errorf("ffprobe failed: %w", err)
	}
	var result struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return meta, err
	}
	if result.Format.Duration != "" {
		duration, err := strconv.ParseFloat(result.Format.Duration, 64)
		if err == nil {
			meta.DurationSeconds = int(duration + 0.5)
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
	if maxDurationSeconds > 0 && meta.DurationSeconds > maxDurationSeconds {
		return fmt.Errorf("video exceeds max duration of %d seconds", maxDurationSeconds)
	}
	return nil
}

func (m *Manager) DiskUsage() (DiskUsage, error) {
	return DiskUsageForPath(m.dir)
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

func RemoveFile(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
