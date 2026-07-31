# tg-obs-bot

Go backend for a 24h Lo-Fi Music channel workflow:

- OBS plays time-of-day loop animations from a local media library.
- Independent Lo-Fi tracks play through a separate OBS source.
- Telegram group admins import media, switch the day theme, force a loop, skip playback, and preview the next period.

## Requirements

- macOS or Linux with OBS Studio
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
7. Keep the backend running on the same host.

The app mutes and loops the animation source, plays music through the music source without source looping, and centers the loop source in the current Program scene without changing scale, bounds, or crop. The two configured source names must be different.

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

`./run.sh up` supervises the Telegram Local Bot API Server and `tg-obs-bot` separately. It never supervises `go run`: when `dist/tg-obs-bot` is missing or older than the Go sources, startup first builds a temporary binary and atomically installs it; a build or process-group isolation failure aborts startup. Each service generation runs in its own process group, and the supervisor drains that complete group before starting a replacement, so descendants cannot accumulate across crashes. The root process also tracks each active generation in private runtime state; if either service supervisor dies unexpectedly, it drains both trees and exits non-zero. Singleton contention is a non-retryable startup failure: the stack stops and propagates exit code `73` instead of looping. `HUP`, `Ctrl-C`/`INT`, and `TERM` use a bounded `TERM`-then-`KILL` shutdown.

For supervised app generations, `./run.sh up` also creates a private local FIFO on descriptor 3 and enables the built-in worker reporter with the exact marker `TG_OBS_LIVENESS_FD3=1`. A strict reader rejects malformed, replayed, reversed, or inconsistent frames; the supervisor restarts only the failed app process group when no initial frame arrives within 360 seconds or a worker makes no progress for 60 seconds in phases `00`-`07`, 150 seconds in phase `08`, or 310 seconds in phases `09`-`10`. A newer frame with an unchanged worker sequence does not renew that worker's lease. `./run.sh app` explicitly leaves this local watchdog disabled.

Set `APP_BIN` to use a different executable. The optional supervisor controls are `RESTART_MIN_DELAY_SECONDS` (default `2`), `RESTART_MAX_DELAY_SECONDS` (default `60`), `RESTART_RESET_AFTER_SECONDS` (default `300`, resets exponential backoff after a stable run), and `SHUTDOWN_GRACE_SECONDS` (default `15`). These values must be positive integers no larger than 86400 seconds.

After changing `.env`, restart the root `./run.sh up` process so both child services inherit the same configuration.

## Config Upgrades

`.env` is local runtime config and is ignored by git. `.env.example` is the versioned schema shared by the Go backend and Telegram Local Bot API Server helpers; keep `ENV_SCHEMA_VERSION` at the top when creating or reviewing config.

Runtime entrypoints set `.env` to owner-only mode `0600` before reading it, and env migration creates its backup and replacement temp files private from their first write. The backend likewise creates or repairs `DATA_DIR` as owner-only mode `0700` before opening persistent state, and keeps the SQLite database plus its WAL/SHM sidecars at mode `0600` even when `DATABASE_PATH` points elsewhere.

Before deploying a new build, back up the production `.env`. You can run `./run.sh migrate-env` to apply the stack helper's lightweight `.env` repair without starting the Go app. It copies the original to `.env.backup.<unix_timestamp>`, updates older schema markers, removes queue-only fields, and appends missing fields needed by the Local Bot API helper. If appended Local Bot API Server defaults are not correct for production, edit `.env` before starting the stack.

Numeric config values must be valid integers; malformed values fail startup instead of silently falling back to defaults. `OBS_PORT` must be `1..65535`, `MAX_VIDEO_SIZE_MB` must be positive, and `MAX_VIDEO_DURATION_SECONDS` plus `MIN_FREE_DISK_MB` may be `0` to disable that limit where supported. `MIN_FREE_DISK_MB` defaults to `512`; uploads require a positive declared size and enough configured cache/destination headroom before Local Bot API `getFile`, followed by authoritative checks against the actual local file. Setting it to `0` disables only the additional reserve—the actual-size filesystem admission and `MAX_VIDEO_SIZE_MB` limit remain enforced.

Each loop/music directory has a fixed, non-configurable ceiling of 10,000 top-level entries. Uploads that would exceed the destination ceiling are rejected before Local Bot API `getFile` and rechecked at the authoritative write boundary; the app never auto-deletes library assets or Telegram-owned cache files to make room.

Version 7 is library-only and gives fresh configurations the neutral
`DATA_DIR/state.db` default. Upgrades from schema v6 or older preserve an
explicit `DATABASE_PATH`; when an older config omitted that value, migration
materializes its historical `DATA_DIR/queue.db` path before advancing the
schema so existing state is never stranded. The loader temporarily accepts a
deprecated `PLAYER_MODE=library` value so an unmigrated deployment can fail
safely into the migration path; `./run.sh migrate-env` removes it together
with all other queue-only keys. `PLAYER_MODE=queue` fails closed before any
migration write with upgrade instructions, so the app never silently turns an
old queue deployment into live library playback.

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
./run.sh migrate-env     # back up .env, remove queue-only keys, add required defaults
./run.sh logout-public   # manually log out from public Telegram Bot API
```

## Commands

- Telegram group admins can manage library playback automatically; no separate admin ID list is required.
- The bot registers Telegram command menus and adds inline buttons to common responses.
- `/library [page]` shows available loop/music assets by period and theme.
- `/now` shows the current period, loop, theme, and music.
- `/preview` shows the next period's planned loop under the current algorithm.
- `/theme <theme|random>` sets or clears today's theme override. A theme must
  fit both the 240-rune and 240-byte UTF-8 bounds and may not contain `_`, `/`,
  or `\`.
- `/select <asset_id|clear>` forces or clears today's direct loop override.
- `/skip loop` redraws the current period loop.
- `/skip music` skips to another music track.
- `/scan` rescans the media library.
- `/status` shows OBS, playable/rejected assets, overrides, last errors, and
  separate loop/music/Telegram-cache disk readings.

## Notes

- The MVP avoids transcoding to keep CPU use low on the MacBook.
- Imported library media is copied into `LOOP_MEDIA_DIR` or `MUSIC_MEDIA_DIR`.
- Library imports are validated in a dedicated staging directory on the destination filesystem before no-overwrite atomic publication; startup and periodic maintenance remove interrupted staging files older than six hours in bounded batches.
- Every scan applies the same stream/duration/size validation to files added
  directly by an operator. Invalid or OBS-failed assets are excluded and
  surfaced through `/status`; a changed file is eligible for validation again.
- When `MAX_VIDEO_DURATION_SECONDS` is enabled, a missing or invalid `ffprobe` duration rejects the video instead of treating it as zero.
- SQLite state is stored under `DATA_DIR` so today's overrides and period picks survive restarts.
- `OBS_PASSWORD` can be left empty when OBS WebSocket authentication is disabled.
- `TELEGRAM_API_BASE_URL` must point at the Local Bot API Server, for example `http://127.0.0.1:8081`.

More detail:

- [Architecture](docs/ARCHITECTURE.md)
- [Operations](docs/OPERATIONS.md)
