package journalstore_test

import (
	"github.com/tiwb/tg-obs-bot/internal/journalstore"
	"github.com/tiwb/tg-obs-bot/internal/telegram"
)

var _ telegram.UpdateJournal = (*journalstore.Store)(nil)
