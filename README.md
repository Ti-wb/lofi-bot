# tg-obs-bot

Go backend for a 24h Lo-Fi Music channel workflow:

- Telegram group members submit videos.
- The bot validates and queues videos.
- OBS plays queued videos through one Media Source controlled by OBS WebSocket.
- Telegram group admins manage queue order from Telegram.

## Requirements

- macOS with OBS Studio
- OBS WebSocket enabled, usually port `4455`
- Go 1.22+
- `ffmpeg` / `ffprobe`

On macOS:

```sh
brew install go ffmpeg
```

## OBS Setup

1. Open OBS.
2. Enable WebSocket server in OBS settings.
3. Create a Media Source named `tg_queue_player`.
4. Disable looping on that source.
5. Keep the backend running on the same Mac.

## Telegram Setup

1. Create a bot with BotFather.
2. Add the bot to the target group.
3. Find the group chat ID.
4. Copy `.env.example` to `.env` and fill values.

## Run

```sh
cp .env.example .env
make tidy
make run
```

Build a local binary:

```sh
make build
```

Run tests:

```sh
make test
```

## Commands

- Telegram group admins can manage queue playback automatically; no separate admin ID list is required.
- The bot registers Telegram command menus and adds inline buttons to common responses.
- `/queue` shows now playing and upcoming videos.
- `/now` shows the current video.
- `/remove <id>` cancels a queued video.
- `/move <id> <position>` reorders a queued video.
- `/skip` skips current playback.
- `/history` shows recently completed/canceled/failed items.
- `/status` shows queue, OBS, disk, and error state.

## Notes

- The MVP avoids transcoding to keep CPU use low on the MacBook.
- Files are retained by `RETENTION_DAYS` and `RETENTION_MAX_FILES`.
- SQLite state is stored under `DATA_DIR` so the queue survives restarts.
- `FALLBACK_MODE=random_played` keeps the channel alive by replaying completed history when the queue is empty.

More detail:

- [Architecture](docs/ARCHITECTURE.md)
- [Operations](docs/OPERATIONS.md)
