# Architecture

`tg-obs-bot` is a single-process Go service for running a local time-of-day loop animation library and independent Lo-Fi music through OBS. It requires Telegram Local Bot API Server for admin controls and media imports; the public Telegram Bot API is intentionally unsupported for uploads.

## Components

- `cmd/tg-obs-bot`: process entrypoint, config loading, signal handling.
- `internal/config`: `.env` and environment variable parsing.
- `internal/app`: orchestration between Telegram, library scheduling, media storage, and OBS.
- `internal/telegram`: Telegram update loop, uploads/imports, commands, and live group admin checks.
- `internal/obs`: OBS WebSocket v5 client, auth handshake, multi-source media control, playback-ended events.
- `internal/library`: SQLite-backed period plans, overrides, asset scanning, and selection.
- persistent update journal: Telegram polling checkpoint, attempt fencing, poison-update handling, and bounded cleanup.
- `internal/singleton`: canonical database and hashed Telegram identities with process-lifetime kernel locks.
- `internal/media`: local Telegram file probing/import support, `ffprobe` metadata, disk usage.
- `internal/liveness`: fixed required-worker ownership, progress snapshots, and the local FD3 reporter protocol.
- Telegram Local Bot API Server: local Bot API endpoint configured by `TELEGRAM_API_BASE_URL`.

## Runtime Flow

1. Before SQLite, OBS, Telegram, or worker initialization, the app canonicalizes `DATABASE_PATH` and obtains nonblocking OS-held database and Telegram-identity locks.
2. At startup the app scans `LOOP_MEDIA_DIR` and `MUSIC_MEDIA_DIR`.
3. The scheduler determines the current local period: morning, day, evening, or night.
4. Direct loop override wins first; today's theme override wins next; otherwise the scheduler uses the stored random pick for the period or creates one.
5. OBS loop source is pointed at the chosen loop, muted, set to loop, centered, and restarted.
6. OBS music source is pointed at a random music asset and restarted without looping.
7. Music-ended events choose another music asset while avoiding immediate repeats when possible.
8. Period changes cause the next stored or newly selected loop plan to start.
9. `/preview` materializes the next period's planned loop so the preview matches the later playback unless the asset is removed.
10. Before Local Bot API `getFile`, upload metadata passes application admission: positive declared size, library filename, collision, directory-capacity, cache-filesystem, and destination-headroom checks. Shared cache/library filesystems reserve room for both the cached source and staging copy. The returned local path, source identity, actual size, stream type, and storage constraints are then revalidated; library assets are copied and validated inside an app-owned staging subdirectory before no-overwrite atomic publication and scan/import.

## Management Authorization

The service uses Telegram's live group administrator list for management permissions. Admin-only commands call `getChatAdministrators` through the Telegram Bot API, cache the result briefly, and deny access if lookup fails.

## Telegram Interaction UX

Text commands remain supported, but the bot also registers Telegram command menu entries at startup and attaches inline keyboards to library, status, now-playing, preview, and upload-accepted messages. Callback buttons reuse the same authorization and hook paths as text commands.

## Library Scheduling

Loop filenames use `loop_<period>_<theme>_<variant>.<ext>`. Music filenames use `music_<track>.<ext>`. Library scans are non-recursive. They admit only non-empty regular files, run the same `ffprobe` stream/duration/size checks used by uploads, and retain structured scan errors for `/status`. A loop requires a video stream and music requires an audio stream.

The four periods are based on local process time:

- `morning`: 06:00-10:59
- `day`: 11:00-16:59
- `evening`: 17:00-20:59
- `night`: 21:00-05:59

Without an override, each period randomly chooses one theme with at least one matching loop and one loop asset for that theme. That period plan is persisted in SQLite so restarts and repeated previews do not redraw it. Today's direct loop or theme overrides expire at local midnight.

## Data Persistence

SQLite persists period plans, daily overrides, the last music choice, and Telegram polling-journal state under `DATABASE_PATH`. Imported production media is copied from a securely opened regular file below `TELEGRAM_BOT_API_DIR` into `LOOP_MEDIA_DIR` or `MUSIC_MEDIA_DIR`; persisted playback state references the library copy, not the Telegram cache path.

Schema v7 is library-only. Fresh configurations default to `state.db`.
Migration preserves every explicit database path and pins schema-v6-or-older
configs that omitted it to their historical `queue.db`, so an upgrade cannot
silently open an empty database. Existing databases may still contain
historical `videos` tables from older releases. The service neither reads,
plays, updates, nor deletes those rows; operators may archive them separately
after validating the library migration.

The backend opens SQLite through the same canonical path that defines `<canonical DATABASE_PATH>.tg-obs-bot.lock`. It also hashes the Telegram token with SHA-256 and locks `telegram-bot-<digest>.lock` inside a shared per-effective-UID application lock directory. The directory is rooted at `/private/tmp/tg-obs-bot-<uid>/locks` on macOS or `/tmp/tg-obs-bot-<uid>/locks` on Linux; it does not depend on `HOME`, `XDG_CONFIG_HOME`, `TMPDIR`, the checkout, or the runtime data root. Startup traverses the fixed base without following symlinks, verifies that the UID root and lock directory are real directories owned by the effective UID, and enforces mode `0700`. The raw token is neither retained in the lock object nor written to paths or errors. macOS and Linux hold both mode-`0600` files with exclusive nonblocking kernel locks for the complete service lifetime, always acquiring database identity before bot identity and releasing a partial acquisition on failure. Relative paths, `..`, and symlink aliases resolve to the same database identity; different database roots remain independent only when they also use different bot identities. Descriptors are close-on-exec, clean shutdown releases them last, and a crash or forced exit releases both through OS descriptor teardown. Persistent lock files are not PID files and are never used as evidence of ownership.

The Go backend, Local Bot API Server, and OBS are expected to run on the same host. A multi-host setup must provide shared storage where the Local Bot API absolute file paths and OBS media paths are readable by the relevant processes.

On restart:

- the library is rescanned and the persisted current-period plan is reused while its asset remains healthy;
- after a same-process OBS reconnect, matching active loop/music playback with observable progress keeps its current position;
- inactive, terminal, stalled, or path-mismatched sources are reconciled independently;
- a missing or unhealthy candidate is excluded and another matching current-period asset is selected with bounded retry;
- an explicit OBS media-error state quarantines that exact file until its
  identity changes; a persisted bad plan is cleared rather than pinning the
  period;
- the reconnect loop uses a five-second healthy cadence and a per-attempt timeout; consecutive attempted connect, probe, or playback-recovery failures back off exponentially with bounded 20% jitter to one minute, while disconnected/skipped cycles do not advance or reset those independent sequences;
- the scheduler restores loop and music sources when OBS is connected but either source is inactive.

## Internal Worker Liveness Contract

The in-process registry has exactly five stable worker IDs: `telegram`, `obs-reconnect`, `obs-events`, `maintenance`, and `playback`. The `playback` wire slot is owned only by `library-scheduler`.

Only the bound worker execution path advances its monotonic sequence: when it actually starts, crosses a real operation or successful checkpoint boundary, or remains schedulable in its own event, scheduled, or retry wait. A reporter or coordinator must not pulse on a worker's behalf. Long media copy, probe, durability sync, and library scan operations use fixed, bounded, tokenized scopes. Arbitrary close order leaves the newest active scope effective, the final close returns to the base phase, and an authoritative phase change such as cancellation invalidates older restores.

An opt-in reporter snapshots all five entries every ten seconds and writes one bounded `TGOBS1` line to a fixed, write-only FIFO inherited as descriptor 3. The process enables it only when `TG_OBS_LIVENESS_FD3=1` is present; absence leaves direct execution unchanged, while a malformed marker or invalid descriptor fails startup. The descriptor is validated, made nonblocking and close-on-exec before app initialization, so startup media probes cannot inherit it. Full-pipe backpressure drops a complete frame; partial writes, a lost reader, or other permanent write failures stop the required reporter and fail the service. The reporter starts only after all five worker goroutines have been launched and never advances worker progress itself.

`run.sh up` activates this same-host contract for each app generation. It creates one FIFO inside a fresh mode-`0700`, effective-UID-owned directory under `/private/tmp` on macOS or `/tmp` on Linux, fixes the FIFO at mode `0600`, starts one strict AWK reader, and passes only the write end as FD3 with the exact marker. `run.sh app` unsets the marker and remains unsupervised by this watchdog. There is no network endpoint, daemon, user-selected path, or credential-bearing liveness record.

The reader requires exact field order and canonical unsigned 64-bit values. Frame generations must strictly increase; each worker sequence may only increase or remain equal, and an equal sequence is legal only with the same phase. It atomically publishes one normalized private state file for the existing supervisor loop. The shell evaluates all five leases independently: startup allows 360 seconds for the first valid frame, phases `00`-`07` allow 60 seconds without sequence progress, phase `08` allows 150 seconds, and phases `09`-`10` allow 310 seconds. Frame arrival or a repeated equal sequence does not refresh a lease. A schema, ordering, phase-without-progress, reader, state-write, startup, or stale failure drains that app generation with the existing bounded `TERM`-then-`KILL` path and restart backoff; the Bot API sibling remains running. Shutdown reaps the reader before removing its FIFO state.

## Failure Handling

- Process logging is isolated behind one stdout writer goroutine and a fixed
  256-entry in-memory queue installed before configuration loading. Application
  goroutines clone and enqueue records without waiting for stdout; a full queue
  drops the newest record. The bound is by record count, not serialized bytes,
  so unusually large attribute values can still consume memory and make one
  stdout write slow. Total and pending drops, sink errors, queue depth, and
  high-water depth are tracked; the next successfully emitted record carries
  `log_dropped=N`. A sink failure does not clear that pending count.
- Clean shutdown drains log records in FIFO order after `Service.Close`.
  Explicit fatal exits wait at most about 500 milliseconds for the same drain,
  then abort queued logging and preserve exit status `1`, `70`, or `73`.
  Consequently fatal records are best effort when stdout is blocked or the
  queue is full; the supervisor exit code remains the authoritative signal.
- Telegram updates are handled one at a time. Each handler receives a five-minute processing deadline and then a bounded five-second cancellation grace. A cooperative timeout is logged and reaches a terminal outcome before polling continues; a handler that ignores cancellation makes the process fail fast so the external supervisor can replace the complete process generation. Each stuck generation records one failure. The third failure atomically marks the update `dead` and advances the durable checkpoint, but that potentially compromised generation still exits without another poll; only its clean replacement continues.
- Telegram polling progress is durable in SQLite. `next_offset` advances in the same transaction that marks an update attempt `done` or `dead`; `confirmed_offset` advances only after a later `getUpdates` call using that offset succeeds. Every handler invocation first journals the update ID, coarse kind, inferred action, and Telegram actor/message metadata. Active attempts have an owner token and lease: a recent owner produces a fail-closed `busy` result, an expired running owner consumes one failure when reclaimed, and a cooperative shutdown releases its claim without consuming the poison budget. All polling-journal calls are bounded to four seconds. A failed terminal write releases a still-owned claim so the supervisor can replay immediately instead of waiting for the normal multi-minute processing lease.
- Handler panics and other non-terminal failures do not advance the checkpoint. A deterministic failure is retried across process restarts and becomes a `dead` poison update on the third failed attempt, at which point the durable offset advances so later updates are not blocked. A recovered panic still stops the current process after that atomic transition because unrelated in-process state may be inconsistent.
- Terminal update journal pruning is confirmation-gated: only update IDs below `confirmed_offset` are eligible. Confirmed `done` rows are bounded to the newest 10,000 and 30 days; confirmed `dead` rows are bounded to the newest 1,000 and 90 days. Unconfirmed terminal rows are retained even when those limits are exceeded. Confirmation and periodic maintenance each remove at most 256 rows per terminal status, so a large idle backlog converges over maintenance ticks without making a poll perform an unbounded delete.
- When Telegram confirms a higher offset after a long gap, replayable non-terminal rows strictly below it are reconciled to abandoned `dead` rows in batches of at most 256; rows with an active lease and rows at or above `confirmed_offset` remain untouched.
- Telegram polling, OBS reconnect, OBS events, periodic maintenance, and the library scheduler are mandatory workers. An unexpected worker return is fatal. Maintenance runs outside the coordinator so a stuck cleanup cannot hide another worker's failure. After any fatal result or process cancellation, sibling workers receive cancellation and have ten seconds to drain. This outer budget reserves the Telegram handler's five-second stop grace, up to four seconds for independent journal finalization, and a scheduling cushion; failure to drain is reported as an internal stall instead of blocking shutdown indefinitely.
- Recurring dependency failures use cancellation-aware exponential backoff with bounded 20% jitter. Telegram polling grows from three seconds to a one-minute local cap and honors a stronger Bot API `retry_after` hint up to fifteen minutes. Library reconciliation grows from fifteen seconds to five minutes, and periodic maintenance grows from its configured cadence to at most one hour (or its configured cadence when longer). Maintenance lock contention is not counted as a dependency failure: it retries after the smaller of the normal interval and one minute, logs only the first and every eighth deferral plus one recovery, and does not change the real-error backoff. Successful cycles return to the exact healthy cadence. Database journal failures retain their fail-fast semantics.
- Repeated warnings use fixed-size counters rather than error-keyed maps: the first failure and every eighth consecutive failure are logged with a suppressed count, followed by one recovery message for retrying loops. Event streams are never delayed; decode, overflow, and playback-event errors use the same first/eighth sampling and silently reset only after eight consecutive healthy events. Recurring error text is redacted before being capped at 512 valid UTF-8 bytes.
- The process-lifetime database and Telegram-identity kernel locks prevent two cooperating backend generations from executing domain side effects against either the same canonical database or the same bot. Telegram's wall-clock attempt lease remains crash/replay accounting rather than an independent side-effect fence. External writers, binaries that do not honor these locks, hard-link aliases, runtime-symlink mutation after startup, and containers or mount namespaces that do not share the per-user lock directory remain outside that guarantee.
- OBS connection loss does not delete period plans or overrides.
- OBS heartbeat and per-source reconciliation preserve matching healthy playback and repair an unhealthy loop or music source without advancing the other source.
- A terminal or failed loop is retried from its stable plan only while the asset remains healthy; unhealthy assets are excluded so one corrupt file cannot pin the period.
- Maintenance prunes Telegram journal rows, stale import staging files, and obsolete library plans/overrides in bounded batches. It never deletes production library media.

## Intentional MVP Constraints

- No transcoding by default, to keep CPU use low on the MacBook.
- No web dashboard; Telegram commands are the management UI.
- Backend, Telegram Local Bot API Server, and OBS are expected to run on the same Mac.
