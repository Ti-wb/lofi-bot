package media

import (
	"testing"
)

func TestValidateRejectsEmptyOversizedAndLongVideos(t *testing.T) {
	manager, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if err := manager.Validate(Metadata{SizeBytes: 0}, 100, 10); err == nil {
		t.Fatal("expected empty file rejection")
	}
	if err := manager.Validate(Metadata{SizeBytes: 101}, 100, 10); err == nil {
		t.Fatal("expected oversized file rejection")
	}
	if err := manager.Validate(Metadata{SizeBytes: 50, DurationSeconds: 11}, 100, 10); err == nil {
		t.Fatal("expected long video rejection")
	}
	if err := manager.Validate(Metadata{SizeBytes: 50, DurationSeconds: 10}, 100, 10); err != nil {
		t.Fatalf("expected valid metadata: %v", err)
	}
}

func TestDiskUsage(t *testing.T) {
	manager, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	usage, err := manager.DiskUsage()
	if err != nil {
		t.Fatalf("disk usage: %v", err)
	}
	if usage.TotalBytes == 0 || usage.AvailableBytes == 0 {
		t.Fatalf("unexpected disk usage: %#v", usage)
	}
}
