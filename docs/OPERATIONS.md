# Operations

## First-Time Setup

1. Install dependencies:

   ```sh
   brew install go ffmpeg
   ```

2. Configure Telegram Local Bot API Server:

   - create a bot with BotFather;
   - obtain `api_id` and `api_hash` from Telegram;
   - fill the shared root `.env`;
   - run the stack with `./run.sh up`.

   The public Telegram Bot API is not supported. The Local Bot API Server must run with `--local` and return absolute local file paths from `getFile`. Use `./run.sh logout-public` for the manual public API logout step before first switching to the local server.

3. Configure OBS:

   - enable OBS WebSocket;
   - use port `4455` unless changed in `.env`;
   - create a Media Source named `tg_queue_player`;
   - disable looping on that source.

4. Configure Telegram group access:

   - add it to the target group;
   - collect the group chat ID;
   - make sure queue managers are Telegram group admins.

5. Create local config:

   ```sh
   cp .env.example .env
   ```

6. Fill `.env`. `.env` is ignored by git; `.env.example` is the versioned schema and starts with `ENV_SCHEMA_VERSION`.

The Go backend, Telegram Local Bot API Server, and OBS should run on the same machine. If they do not, their media paths must be on shared storage and readable at the same absolute paths by the backend and OBS.

## Production Config Upgrades

Before deploying a new build, manually back up the production `.env`.

Before starting services, run `./run.sh migrate-env` to apply the stack helper's lightweight `.env` repair without starting the Go app. The helper checks `.env` against the supported schema version, backs up the current file to `.env.backup.<unix_timestamp>`, updates older schema markers, and appends missing fields needed by the Local Bot API helper.

After migration, confirm the appended Telegram Local Bot API Server defaults are correct for production. If they are wrong, edit `.env` and restart the relevant process.

Numeric config values are strict in production: malformed integers fail startup instead of falling back to defaults. Keep `OBS_PORT` in `1..65535`, set `MAX_VIDEO_SIZE_MB` and `MAX_QUEUE_LENGTH` above `0`, and use non-negative values for `MAX_VIDEO_DURATION_SECONDS`, `RETENTION_DAYS`, and `RETENTION_MAX_FILES`. A value of `0` remains valid for duration and retention limits where it means disabled.

The stack helpers also run this migration before validating Local Bot API Server fields, so `./run.sh up`, `./run.sh doctor`, and `./run.sh env` can handle older `.env` files that are missing the v2 Local Bot API defaults.

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
./run.sh up
```

The built binary is written to `dist/tg-obs-bot`.

## Telegram Admin Commands

Telegram group admins can use these management commands. Anonymous admins should disable anonymous admin mode before issuing commands, because Telegram does not expose their real user ID to the bot.

The bot registers Telegram's command menu on startup. Most responses also include inline buttons for common actions:

- queue/status/now/history navigation;
- refresh buttons that update the current message;
- admin-only skip, remove, and move buttons where applicable.

- `/queue`: show current and upcoming videos.
- `/now`: show the current video.
- `/status`: show OBS status, queue counts, media size, disk space, and last error.
- `/history`: show recent played, canceled, or failed items.
- `/remove <id>`: cancel a queued item.
- `/move <id> <position>`: move a ready item.
- `/skip`: skip current playback.

## Storage And Retention

`RETENTION_DAYS` and `RETENTION_MAX_FILES` are both active. A played queue row can be removed from SQLite when it is older than the age limit or when the played history exceeds the file count limit.

When `FALLBACK_MODE=random_played`, the currently playing random fallback row is protected from retention cleanup. Uploaded video files remain owned by Telegram Local Bot API Server and are not deleted by this project. App retention removes SQLite rows only; it does not remove media files under `TELEGRAM_BOT_API_DIR`.

Monitor disk usage for `TELEGRAM_BOT_API_DIR` alongside `/status`. If `TELEGRAM_BOT_API_DIR` is relative, run `du -sh "$TELEGRAM_BOT_API_DIR"` from the repository root, or use the absolute resolved path. If manual cleanup is needed, first stop the stack or confirm the files are not referenced by queued, currently playing, or fallback history rows. Never delete paths that may still be queued, current, or used as random fallback candidates.

Set conservative values on the MacBook first, for example:

```env
FALLBACK_MODE=random_played
RETENTION_DAYS=7
RETENTION_MAX_FILES=100
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
- check `ffprobe` is installed;
- check free disk space in `/status`;
- check `MAX_VIDEO_SIZE_MB` and `MAX_VIDEO_DURATION_SECONDS`;
- inspect service logs for local path/probe errors.

If videos do not visually change in OBS:

- confirm the Media Source name exactly matches `OBS_MEDIA_SOURCE_NAME`;
- confirm the source supports local files;
- confirm the source is visible in the active scene.

## Suggested LaunchAgent

For unattended use, build once and create macOS LaunchAgents for the underlying long-running services: `deploy/telegram-bot-api/run.sh` and `dist/tg-obs-bot`. The root and `deploy/telegram-bot-api` helper scripts load `.env` from the repository root and resolve a relative `TELEGRAM_BOT_API_DIR` against the repository root. When running binaries directly or writing custom plists, set `WorkingDirectory` to the repository root, or convert all relative paths in `.env` and plist arguments to absolute paths. Relative values such as `.env`, `DATA_DIR`, `TELEGRAM_BOT_API_DIR`, and log paths then resolve from the process working directory, not from the script or binary file location.

Keep fixed Local Bot API data, `.env`, `data/`, and logs on local disk. The root `run.sh` is the local operator entrypoint, not a replacement for future LaunchAgent plists.
