# tg-obs-bot

Go backend for a 24h Lo-Fi Music channel workflow:

- OBS plays time-of-day loop animations from a local media library.
- Independent Lo-Fi tracks play through a separate OBS source.
- Telegram group admins import media, switch the day theme, force a loop, skip playback, and preview the next period.
- The legacy Telegram queue player remains available through `PLAYER_MODE=queue`.

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
3. Create a Media Source named `tg_loop_player` for loop animation.
4. Create a Media Source named `tg_music_player` for Lo-Fi music.
5. Add both sources to the Program scene used for playback.
6. Keep looping disabled in OBS; the app sets loop/music behavior through OBS WebSocket.
7. Keep the backend running on the same Mac.

The app mutes the loop source, plays music through the music source, and centers the loop source in the current Program scene without changing scale, bounds, or crop. The legacy queue source name is still configured by `OBS_MEDIA_SOURCE_NAME`.

## Telegram Setup

1. Create a bot with BotFather.
2. Run a Telegram Local Bot API Server for that bot. Public Telegram Bot API is not supported.
3. Start the Local Bot API Server with `--local` so `getFile` returns an absolute file path.
4. Keep the Go backend, Local Bot API Server, and OBS on the same machine, or use shared paths readable by all three processes.
5. Add the bot to the target group.
6. Find the group chat ID.
7. Copy `.env.example` to `.env` and fill the shared bot, group, OBS, and Local Bot API Server values.

See [deploy/telegram-bot-api](deploy/telegram-bot-api/README.md) for Local Bot API Server setup scripts and the shared `.env` contract.

## Media Library

Put loop videos under `LOOP_MEDIA_DIR` and music files under `MUSIC_MEDIA_DIR`. Defaults are `./data/media/loops` and `./data/media/music`.

Loop filenames must use:

```text
loop_<period>_<theme>_<variant>.<ext>
```

`period` must be `morning`, `day`, `evening`, or `night`, for example `loop_morning_xiaozhu-cafe_001.mp4`.

Music filenames must use:

```text
music_<track>.<ext>
```

For example `music_lofi-chill-001.mp3`.

Without a Telegram override, each period randomly picks one playable theme and one matching loop asset, then keeps that loop until the period ends. `/preview` materializes and reports the next period's planned pick so the preview matches what will actually play.

## Run

```sh
cp .env.example .env
make tidy
./run.sh doctor
./run.sh build
./run.sh up
```

For unattended use with the portable shell environment, build first and keep `./run.sh up` running from your existing `start.sh`, terminal multiplexer, or process wrapper:

```sh
./run.sh build
./run.sh up
```

`./run.sh up` supervises the Telegram Local Bot API Server and `tg-obs-bot` separately. If either child exits, only that child is restarted with exponential backoff. The supervisor uses `dist/tg-obs-bot` when it is current; if the binary is missing or older than Go source files, it falls back to `go run` and prints a warning. You can set `RESTART_MIN_DELAY_SECONDS`, `RESTART_MAX_DELAY_SECONDS`, or `APP_BIN` to customize restart delays or the app binary path. Restart delays must be positive integers no larger than 86400 seconds.

After changing `.env`, restart the root `./run.sh up` process so both child services inherit the same configuration.

## Config Upgrades

`.env` is local runtime config and is ignored by git. `.env.example` is the versioned schema shared by the Go backend and Telegram Local Bot API Server helpers; keep `ENV_SCHEMA_VERSION` at the top when creating or reviewing config.

Before deploying a new build, back up the production `.env`. You can run `./run.sh migrate-env` to apply the stack helper's lightweight `.env` repair without starting the Go app. It copies the original to `.env.backup.<unix_timestamp>`, updates older schema markers, and appends missing fields needed by the Local Bot API helper. If appended Local Bot API Server defaults are not correct for production, edit `.env` before starting the stack.

Numeric config values must be valid integers; malformed values fail startup instead of silently falling back to defaults. `OBS_PORT` must be `1..65535`; `MAX_VIDEO_SIZE_MB` and `MAX_QUEUE_LENGTH` must be positive; `MAX_VIDEO_DURATION_SECONDS`, `RETENTION_DAYS`, and `RETENTION_MAX_FILES` may be `0` to disable that limit where supported. `RETENTION_DELETE_LOCAL_FILES` defaults to `false`, so retention removes old SQLite rows without deleting Telegram Local Bot API media files unless you explicitly opt in.

The stack helpers run this migration before validating Local Bot API Server fields, so `./run.sh up`, `./run.sh doctor`, and `./run.sh env` can handle older `.env` files that are missing supported schema defaults. The Go app itself only reads config at startup; it does not rewrite `.env`.

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
./run.sh up              # supervise Telegram Local Bot API Server and tg-obs-bot
./run.sh app             # start only tg-obs-bot
./run.sh bot-api         # start only Telegram Local Bot API Server
./run.sh health          # check local Telegram Bot API /getMe, not app or OBS readiness
./run.sh doctor          # check config, tools, data dir, and common ports
./run.sh env             # print sanitized runtime config
./run.sh migrate-env     # back up .env and append missing schema defaults
./run.sh logout-public   # manually log out from public Telegram Bot API
```

## Commands

- Telegram group admins can manage library playback automatically; no separate admin ID list is required.
- The bot registers Telegram command menus and adds inline buttons to common responses.
- `/library` shows available loop/music assets by period and theme.
- `/now` shows the current period, loop, theme, and music.
- `/preview` shows the next period's planned loop under the current algorithm.
- `/theme <theme|random>` sets or clears today's theme override.
- `/select <asset_id|clear>` forces or clears today's direct loop override.
- `/skip loop` redraws the current period loop.
- `/skip music` skips to another music track.
- `/scan` rescans the media library.
- `/status` shows OBS, library, override, disk, and error state.

## Notes

- The MVP avoids transcoding to keep CPU use low on the MacBook.
- Imported library media is copied into `LOOP_MEDIA_DIR` or `MUSIC_MEDIA_DIR`.
- SQLite state is stored under `DATA_DIR` so today's overrides and period picks survive restarts.
- `PLAYER_MODE=queue` enables the legacy queue player; `FALLBACK_MODE=random_played` only applies there.
- `OBS_PASSWORD` can be left empty when OBS WebSocket authentication is disabled.
- `TELEGRAM_API_BASE_URL` must point at the Local Bot API Server, for example `http://127.0.0.1:8081`.

More detail:

- [Architecture](docs/ARCHITECTURE.md)
- [Operations](docs/OPERATIONS.md)
