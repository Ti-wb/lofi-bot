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

Numeric config values are strict in production: malformed integers fail startup instead of falling back to defaults. Keep `OBS_PORT` in `1..65535`, set `MAX_VIDEO_SIZE_MB` and `MAX_QUEUE_LENGTH` above `0`, and use non-negative values for `MAX_VIDEO_DURATION_SECONDS`, `MIN_FREE_DISK_MB`, `RETENTION_DAYS`, and `RETENTION_MAX_FILES`. A value of `0` remains valid where it means disabled. `MIN_FREE_DISK_MB` defaults to `512`; setting it to `0` disables only the additional reserve, not actual-size filesystem admission or `MAX_VIDEO_SIZE_MB`. `RETENTION_DELETE_LOCAL_FILES` must be boolean and defaults to `false`.

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

The built binary is written to `dist/tg-obs-bot`. `./run.sh up` never supervises `go run`. If the default binary is missing or older than the Go sources, startup builds a temporary executable and atomically moves it into place. A failed build leaves the prior binary untouched and aborts startup. Set `APP_BIN` to run a different executable; an invalid or non-executable path also fails before either service starts.

`./run.sh up` supervises the Telegram Local Bot API Server and `tg-obs-bot` independently. Every service generation has a dedicated process group. After a child exits, the supervisor sends `TERM` and then bounded `KILL` if necessary to drain any surviving descendants before it starts the replacement. Supervisor and build process groups are also isolated; if the platform cannot prove that isolation, startup fails closed. The root process keeps private runtime records for active child groups. If either service supervisor exits unexpectedly, the root drains the recorded child group plus the peer service tree and exits non-zero instead of waiting forever. `HUP`, `Ctrl-C`/`INT`, and `TERM` stop the complete process trees with the same bounded cleanup.

On macOS and Linux, `tg-obs-bot` acquires two resource fences before opening SQLite or initializing OBS and Telegram: `<canonical DATABASE_PATH>.tg-obs-bot.lock`, plus a Telegram-identity lock named from the token's SHA-256 digest. The second file lives in a fixed per-effective-UID directory: `/private/tmp/tg-obs-bot-<uid>/locks` on macOS or `/tmp/tg-obs-bot-<uid>/locks` on Linux. `HOME`, `XDG_CONFIG_HOME`, `TMPDIR`, checkout location, and data-root changes cannot redirect this identity. Startup rejects symlink substitution or a UID root/lock directory owned by another user and enforces mode `0700` on both directories. Both lock files are mode `0600`, persist while the runtime directory remains present, and are not PID files; do not delete the directory or files as stale-state recovery while a backend may be running. The token itself never appears in the path or contention error.

Kernel ownership of both locks disappears automatically after clean shutdown, crash, `KILL`, or forced exit. A second backend using either the same canonical database (including relative or symlink aliases) or the same Telegram bot fails immediately with exit code `73` and an actionable resource-lock error. `./run.sh up` treats that code as non-retryable, stops the peer Bot API tree, and propagates `73` instead of entering restart backoff. Stop the existing backend before retrying. Only different canonical database roots using different bot tokens may run independently. Unsupported operating systems fail closed instead of starting without fencing. Containers or mount namespaces must bind/share the same per-user lock directory if they are expected to fence one another; separate OS users are distinct lock domains and require an external singleton supervisor.

Inside `tg-obs-bot`, Telegram polling, OBS reconnect, OBS events, periodic maintenance, and the active playback scheduler/watchdog are required workers. The coordinator does not run maintenance inline, so blocked cleanup cannot conceal another worker's failure. An unexpected worker exit stops the process. Telegram update handlers have a five-minute deadline plus a five-second cancellation grace; a non-cooperative handler causes exit status `70` so it cannot leave polling permanently stuck. Internal worker shutdown is bounded to ten seconds: five seconds for handler cancellation, up to four seconds for independent journal finalization, and a scheduling cushion. Normal `TERM` remains graceful when every worker cooperates.

The binary contains the FD3 liveness reporter needed by a later shell-watchdog phase, but the current `run.sh` does not provision or consume it and therefore does not perform liveness-based restarts. Direct starts leave `TG_OBS_LIVENESS_FD3` unset. A future local supervisor must pass a write-only FIFO as descriptor 3 and set the marker to the exact value `1`; setting the marker without that FIFO is a fail-closed initialization error. There is no descriptor or filesystem-path override, and provider credentials or application payloads never appear in its fixed `TGOBS1` records.

Transient recurring failures are intentionally rate-limited inside the Go backend. Telegram polling retries grow from three seconds to one minute with bounded jitter; a Telegram `retry_after` response can extend that wait up to fifteen minutes. OBS connect/probe/playback-recovery retries grow from five seconds to one minute. Library reconciliation and the queue watchdog grow to five minutes, and ten-minute maintenance grows to one hour. A successful attempt restores the normal cadence. Time spent disconnected or skipped because another OBS recovery owns the operation does not erase or inflate the attempted-failure count.

For one continuous failure burst, expect a warning on the first failure and every eighth failure, with `consecutive_failures` and `suppressed` fields. Retrying loops then emit one recovery entry. OBS event processing never sleeps and does not emit recovery entries; its repeated decode/overflow/advance warnings reset silently only after eight consecutive healthy events so alternating success/failure traffic cannot flood logs. Error fields on these recurring paths are secret-redacted and capped at 512 valid UTF-8 bytes. Retention path validation and reconnect library-scan warnings are aggregated or sampled; consult `/status` for the retained latest error when an individual occurrence was suppressed.

Stdout logging uses one writer goroutine behind a fixed 256-record queue. The
queue is bounded by entries, not bytes: avoid logging large payloads or file
contents because one record may still allocate or block a large write. When
stdout cannot keep up, callers remain responsive and the newest records are
dropped; the next record successfully written includes `log_dropped=N`.
Shutdown drains records in order when stdout is writable. Fatal exits spend
only about 500 milliseconds on a best-effort drain, so their final log line may
be absent when stdout is blocked or the queue is full. Use process exit code
`1`, `70`, or `73` and supervisor state as the authoritative failure signal.

Telegram polling checkpoints and handler attempts live in the same SQLite database as the queue. The app resumes from durable `next_offset` after restart and separately records when Telegram has accepted that offset on a successful poll. Journal operations have a four-second bound; a database error while loading, beginning, confirming, or completing an attempt is fatal and leaves the update unacknowledged. A failed completion releases its still-owned claim so the immediate restart can replay rather than repeatedly returning `busy` for the full processing lease. Cooperative shutdown releases an attempt without counting a failure. Repeated deterministic failures are retried across restarts; the third failure quarantines the update as `dead`. A stuck or panicking third generation persists that transition but still exits, and only the next clean generation polls past it.

There is an intentional at-least-once crash window in this stage: if a command changes application state and the process dies before its terminal journal transaction commits, the same Telegram update is replayed and the command may run twice. Operators should investigate repeated restarts or `dead` update log entries before manually changing polling state. Terminal journal cleanup never crosses the Telegram-confirmed checkpoint; safely confirmed `done` rows retain at most 10,000/30 days and `dead` rows at most 1,000/90 days, while unconfirmed rows remain for recovery. Confirmation and each ten-minute maintenance tick remove at most 256 rows per terminal status, allowing an idle backlog to converge without unbounded poll latency.

After a long outage, a successful higher Telegram offset reconciles non-terminal rows strictly below that confirmed offset to abandoned `dead` rows in batches of at most 256; active leases and rows at or above the confirmed offset are retained.

The update lease remains wall-clock crash/replay accounting; the OS-held database and Telegram-identity locks are the process-level fences for overlapping cooperating backends. Do not bypass them with an older binary, mutate runtime symlinks while the service is running, or point a second backend at a hard-link alias. These cases, unshared container lock directories, and external database writers are outside the fencing guarantee.

Tune exponential restart behavior with `RESTART_MIN_DELAY_SECONDS` (default `2`), `RESTART_MAX_DELAY_SECONDS` (default `60`), and `RESTART_RESET_AFTER_SECONDS` (default `300`). A service that ran for at least the reset interval starts its next backoff at the minimum. `SHUTDOWN_GRACE_SECONDS` (default `15`) controls how long each child process group receives after `TERM` before `KILL`. All four values must be positive integers no larger than 86400 seconds. These optional shell-supervisor settings can be added to `.env` without changing the Go application configuration schema.

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

Storage admission uses the file's actual on-disk size, not Telegram's declared size. Queue mode checks the filesystem containing the Local Bot API file before creating a queue row. The Local Bot API has already cached that file by this point, so a rejection never deletes it. Library mode separately checks the destination filesystem and requires `actual size + MIN_FREE_DISK_MB` to remain available before copying.

Library imports copy into the app-owned `.tg-obs-bot-staging` subdirectory on the destination filesystem, flush and close the staging file, validate loop media there, then publish it with an atomic rename. The staging and destination directories are both synchronized after publication. Cancellation, validation failure, write failure, and `ENOSPC` remove the staging file without exposing an invalid final asset. Startup and ten-minute maintenance inspect at most 256 entries per staging directory and remove only matching regular staging files older than six hours; later passes converge on larger backlogs while unrelated, non-regular, and newer files remain untouched.

When `MAX_VIDEO_DURATION_SECONDS` is greater than `0`, `ffprobe` must return a finite positive duration. Missing, malformed, zero, negative, `NaN`, or infinite durations are rejected. Fractional durations are rounded up before enforcing the limit.

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
MIN_FREE_DISK_MB=512
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
