# Operations

## First-Time Setup

1. Install dependencies:

   ```sh
   brew install go ffmpeg
   ```

2. Configure Telegram Local Bot API Server:

   - create a bot with BotFather;
   - obtain `api_id` and `api_hash` from Telegram;
   - use `./run.sh logout-public` for the manual public API logout step before first switching to the local server.

   The public Telegram Bot API is not supported. The Local Bot API Server must run with `--local` and return absolute local file paths from `getFile`.

3. Configure OBS:

   - enable OBS WebSocket;
   - use port `4455` unless changed in `.env`;
   - create a Media Source named `tg_loop_player`;
   - create a Media Source named `tg_music_player`;
   - add both sources to the Program scene used for playback;
   - leave OBS source looping disabled; the app controls looping over WebSocket.

   The app mutes the loop source, plays Lo-Fi tracks through the music source, and attempts to center the loop source before playback restarts. It does not change scale, bounds, or crop, so oversized videos may extend beyond the canvas while staying centered.

4. Configure Telegram group access:

   - add it to the target group;
   - collect the group chat ID;
   - make sure queue managers are Telegram group admins.

5. Create local config:

   ```sh
   cp .env.example .env
   ```

6. Fill `.env`. `.env` is ignored by git; `.env.example` is the versioned schema and starts with `ENV_SCHEMA_VERSION`.

7. Validate, build, and start:

   ```sh
   ./run.sh doctor
   ./run.sh build
   ./run.sh up
   ```

The Go backend, Telegram Local Bot API Server, and OBS should run on the same machine. If they do not, their media paths must be on shared storage and readable at the same absolute paths by the backend and OBS.

## Media Library

`PLAYER_MODE=library` is the default runtime mode. Store loop videos in `LOOP_MEDIA_DIR` and music files in `MUSIC_MEDIA_DIR`; defaults are `./data/media/loops` and `./data/media/music`.

Loop files must be named `loop_<period>_<theme>_<variant>.<ext>`, where `period` is one of `morning`, `day`, `evening`, or `night`. Example:

```text
loop_morning_xiaozhu-cafe_001.mp4
```

Music files must be named `music_<track>.<ext>`, for example:

```text
music_lofi-chill-001.mp3
```

The app scans these folders non-recursively. Telegram admin uploads that match the schema are copied into the corresponding library folder so Telegram Local Bot API cache cleanup does not remove production assets.

The daily schedule uses local time:

- morning: 06:00-10:59
- day: 11:00-16:59
- evening: 17:00-20:59
- night: 21:00-05:59

Without an override, each period randomly chooses one playable theme and one matching loop file, then keeps that loop until the period ends. `/preview` materializes the next period pick and stores it, so repeated previews and the actual next period stay consistent unless the asset is removed before playback.

## Production Config Upgrades

Before deploying a new build, manually back up the production `.env`.

Before starting services, run `./run.sh migrate-env` to apply the stack helper's lightweight `.env` repair without starting the Go app. The helper checks `.env` against the supported schema version, backs up the current file to `.env.backup.<unix_timestamp>`, updates older schema markers, and appends missing fields needed by the Local Bot API helper.

After migration, confirm the appended Telegram Local Bot API Server defaults are correct for production. If they are wrong, edit `.env` and restart the root `./run.sh up` supervisor so both child services inherit the same configuration.

Numeric config values are strict in production: malformed integers fail startup instead of falling back to defaults. Keep `OBS_PORT` in `1..65535`, set `MAX_VIDEO_SIZE_MB` and `MAX_QUEUE_LENGTH` above `0`, and use non-negative values for `MAX_VIDEO_DURATION_SECONDS`, `RETENTION_DAYS`, and `RETENTION_MAX_FILES`. A value of `0` remains valid for duration and retention limits where it means disabled. `RETENTION_DELETE_LOCAL_FILES` must be boolean and defaults to `false`.

The stack helpers also run this migration before validating Local Bot API Server fields, so `./run.sh up`, `./run.sh doctor`, and `./run.sh env` can handle older `.env` files that are missing supported schema defaults. The Go app itself only reads config at startup; it does not rewrite `.env`.

## Local Runbook

Unified runtime entrypoint:

```sh
./run.sh help
./run.sh doctor
./run.sh up
```

Common commands:

```sh
./run.sh bot-api         # start only Telegram Local Bot API Server
./run.sh app             # start only tg-obs-bot
./run.sh health          # check Local Bot API /getMe
./run.sh env             # print sanitized runtime config
./run.sh migrate-env     # back up .env and append missing schema defaults
./run.sh logout-public   # manually log out from public Bot API
```

Download dependencies:

```sh
./run.sh tidy
```

Run tests:

```sh
./run.sh test
```

Build:

```sh
./run.sh build
```

Run:

```sh
./run.sh build
./run.sh up
```

For unattended shell-based production use, build once before starting the supervisor:

```sh
./run.sh build
./run.sh up
```

The built binary is written to `dist/tg-obs-bot`. `./run.sh up` uses that binary when it exists and is not older than Go source files; otherwise it falls back to `go run` and prints a warning. Set `APP_BIN` to run a different binary path.

`./run.sh up` supervises the Telegram Local Bot API Server and `tg-obs-bot` as separate child services. If one exits, only that child restarts with exponential backoff. `Ctrl-C` or `TERM` stops both children. The supervisor uses `dist/tg-obs-bot` when it is current; if the binary is missing or older than Go source files, it falls back to `go run` and prints a warning. Tune restart delays with `RESTART_MIN_DELAY_SECONDS` and `RESTART_MAX_DELAY_SECONDS`; both must be positive integers no larger than 86400 seconds.

The supervisor loads `.env` once at startup. After changing `.env`, stop and restart the root `./run.sh up` process instead of killing only one child service.

## Telegram Admin Commands

Telegram group admins can use these management commands. Anonymous admins should disable anonymous admin mode before issuing commands, because Telegram does not expose their real user ID to the bot.

The bot registers Telegram's command menu on startup. Most responses also include inline buttons for common actions:

- library/status/now/preview navigation;
- refresh buttons that update the current message;
- admin-only scan, theme, select, and skip actions where applicable.

- `/library`: show loop/music library counts and useful asset IDs.
- `/now`: show current period, loop, theme, period end, and music.
- `/preview`: show the next period's planned theme and loop under the current algorithm.
- `/status`: show OBS status, library status, overrides, disk space, next period preview, and last error.
- `/scan`: rescan the media library.
- `/theme <theme|random>`: set today's theme override or return to random.
- `/select <asset_id|clear>`: force today's loop asset or clear the direct override.
- `/skip loop`: redraw the current period loop and restart it.
- `/skip music`: skip to another Lo-Fi track.
- `/skip`: alias for `/skip loop`.

## Storage And Retention

In library mode, imported media is copied into `LOOP_MEDIA_DIR` or `MUSIC_MEDIA_DIR` and is not deleted by retention cleanup. Remove obsolete library assets manually during a maintenance window, then run `/scan`.

`RETENTION_DAYS` and `RETENTION_MAX_FILES` are legacy queue-mode limits. A played queue row can be removed from SQLite when it is older than the age limit or when the played history exceeds the file count limit. If both are set to `0`, retention cleanup is disabled and skips history scans.

When `FALLBACK_MODE=random_played`, the currently playing random fallback row is protected from retention cleanup. Uploaded video files remain owned by Telegram Local Bot API Server and are not deleted by default. App retention removes SQLite rows only unless `RETENTION_DELETE_LOCAL_FILES=true`; even then, it skips deleting a local file while another queue/history row still references the same path.

`/status` reports library disk and `TELEGRAM_BOT_API_DIR` disk. Monitor `LOOP_MEDIA_DIR`, `MUSIC_MEDIA_DIR`, and `TELEGRAM_BOT_API_DIR`. If paths are relative, inspect them from the repository root, or use the absolute resolved paths shown by `./run.sh doctor`.

Set conservative values on the MacBook first, for example:

```env
PLAYER_MODE=library
LOOP_MEDIA_DIR=./data/media/loops
MUSIC_MEDIA_DIR=./data/media/music
FALLBACK_MODE=random_played
RETENTION_DAYS=7
RETENTION_MAX_FILES=100
RETENTION_DELETE_LOCAL_FILES=false
MAX_VIDEO_SIZE_MB=2000
MAX_QUEUE_LENGTH=50
```

## Troubleshooting

If `/status` says OBS is disconnected:

- confirm OBS is open;
- confirm WebSocket is enabled;
- confirm `OBS_HOST` and `OBS_PORT`;
- if OBS WebSocket authentication is enabled, set `OBS_PASSWORD`; otherwise leave it empty;
- confirm macOS firewall is not blocking local WebSocket access.

If uploads fail after acceptance:

- confirm the Local Bot API Server is running and `TELEGRAM_API_BASE_URL` points to it;
- confirm it was started with `--local`;
- confirm `getFile` returns an absolute path readable by the Go backend;
- confirm returned paths resolve under `TELEGRAM_BOT_API_DIR`;
- check `ffprobe` is installed;
- check free disk space in `/status`;
- check `MAX_VIDEO_SIZE_MB` and `MAX_VIDEO_DURATION_SECONDS`;
- inspect service logs for local path/probe errors.

If videos do not visually change in OBS:

- confirm the Media Source name exactly matches `OBS_LOOP_SOURCE_NAME`;
- confirm the source supports local files;
- confirm the source is visible in the active scene.

If music does not change in OBS:

- confirm the Media Source name exactly matches `OBS_MUSIC_SOURCE_NAME`;
- confirm the source supports the music file type;
- confirm the source is not muted in OBS unless you intentionally mute it outside the app.

## Portable Shell Supervisor

Use the root `run.sh` as the portable production entrypoint when you want to keep the current shell or `start.sh` environment:

```sh
cd /path/to/tg-obs-bot
./run.sh build
./run.sh up
```

Keep fixed Local Bot API data, `.env`, `data/`, and logs on local disk. The root and `deploy/telegram-bot-api` helper scripts load `.env` from the repository root and resolve a relative `TELEGRAM_BOT_API_DIR` against the repository root. If an external `start.sh` calls this project, call `./run.sh up` from the repository root so relative values such as `DATA_DIR`, `TELEGRAM_BOT_API_DIR`, and `DATABASE_PATH` resolve consistently.

The portable supervisor writes app logs to stdout/stderr and does not rotate logs itself. For unattended operation, run it from a process wrapper or terminal multiplexer that captures stdout/stderr, rotates logs, restarts the root process after host reboot, and alerts on repeated child restarts. `./run.sh health` checks only Telegram Local Bot API `/getMe`; use Telegram `/status` and wrapper process state for app, queue, disk, and OBS readiness.
