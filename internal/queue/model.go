package queue

import "time"

type Status string

const (
	StatusDownloading Status = "downloading"
	StatusReady       Status = "ready"
	StatusPlaying     Status = "playing"
	StatusPlayed      Status = "played"
	StatusCanceled    Status = "canceled"
	StatusFailed      Status = "failed"
)

type Video struct {
	ID               int64
	TelegramFileID   string
	TelegramUniqueID string
	SubmitterID      int64
	SubmitterName    string
	ChatID           int64
	MessageID        int
	FileName         string
	LocalPath        string
	MimeType         string
	SizeBytes        int64
	DurationSeconds  int
	QueuePosition    int
	Status           Status
	Error            string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	StartedAt        *time.Time
	FinishedAt       *time.Time
}

type Stats struct {
	ReadyCount       int
	DownloadingCount int
	PlayedCount      int
	FailedCount      int
	TotalBytes       int64
}
