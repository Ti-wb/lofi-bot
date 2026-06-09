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
7. Copy `.env.example` to `.env` and fill the shared bot, group, OBS, and Local Bot API Server values.

See [deploy/telegram-bot-api](deploy/telegram-bot-api/README.md) for Local Bot API Server setup scripts and the shared `.env` contract.

## Run

```sh
cp .env.example .env
make tidy
./run.sh doctor
./run.sh up
```

## Config Upgrades

`.env` is local runtime config and is ignored by git. `.env.example` is the versioned schema shared by the Go backend and Telegram Local Bot API Server helpers; keep `ENV_SCHEMA_VERSION` at the top when creating or reviewing config.

Before deploying a new build, back up the production `.env`. On startup, the app migrates older `.env` files by copying the original to `.env.backup.<unix_timestamp>` and appending missing fields required by the supported schema version. If appended Local Bot API Server defaults are not correct for production, edit `.env` and restart the relevant process.

Build a local binary:

```sh
make build
```

Run tests:

```sh
make test
```

Common runtime commands:

```sh
./run.sh up              # start Telegram Local Bot API Server and tg-obs-bot
./run.sh app             # start only tg-obs-bot
./run.sh bot-api         # start only Telegram Local Bot API Server
./run.sh health          # check local Telegram Bot API /getMe
./run.sh doctor          # check config, tools, data dir, and common ports
./run.sh env             # print sanitized runtime config
./run.sh logout-public   # manually log out from public Telegram Bot API
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
