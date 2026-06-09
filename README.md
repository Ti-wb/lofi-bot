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
- Telegram Local Bot API Server running with `--local`

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
2. Run a Telegram Local Bot API Server for that bot. Public Telegram Bot API is not supported.
3. Start the Local Bot API Server with `--local` so `getFile` returns an absolute file path.
4. Keep the Go backend, Local Bot API Server, and OBS on the same machine, or use shared paths readable by all three processes.
5. Add the bot to the target group.
6. Find the group chat ID.
7. Copy `.env.example` to `.env` and fill values, including `TELEGRAM_API_BASE_URL`.

See [deploy/telegram-bot-api](deploy/telegram-bot-api/README.md) for the reserved Local Bot API Server deployment notes.

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
- Played queue history is retained by `RETENTION_DAYS` and `RETENTION_MAX_FILES`; uploaded files are owned by Telegram Local Bot API Server.
- SQLite state is stored under `DATA_DIR` so the queue survives restarts.
- `FALLBACK_MODE=random_played` keeps the channel alive by replaying completed history when the queue is empty.
- `OBS_PASSWORD` can be left empty when OBS WebSocket authentication is disabled.
- `TELEGRAM_API_BASE_URL` must point at the Local Bot API Server, for example `http://127.0.0.1:8081`.

More detail:

- [Architecture](docs/ARCHITECTURE.md)
- [Operations](docs/OPERATIONS.md)
