# Architecture

`tg-obs-bot` is a single-process Go service for running a local time-of-day loop animation library and independent Lo-Fi music through OBS. It requires Telegram Local Bot API Server for admin controls and media imports; the public Telegram Bot API is intentionally unsupported for uploads.

## Components

- `cmd/tg-obs-bot`: process entrypoint, config loading, signal handling.
- `internal/config`: `.env` and environment variable parsing.
- `internal/app`: orchestration between Telegram, library scheduling, media storage, and OBS.
- `internal/telegram`: Telegram update loop, uploads/imports, commands, and live group admin checks.
- `internal/obs`: OBS WebSocket v5 client, auth handshake, multi-source media control, playback-ended events.
- `internal/queue`: SQLite-backed legacy queue state and ordering.
- `internal/media`: local Telegram file probing/import support, `ffprobe` metadata, disk usage.
- `internal/library`: media filename parsing, library scanning, period helpers, and selection primitives.
- Telegram Local Bot API Server: local Bot API endpoint configured by `TELEGRAM_API_BASE_URL`.

## Runtime Flow

1. At startup the app scans `LOOP_MEDIA_DIR` and `MUSIC_MEDIA_DIR`.
2. The scheduler determines the current local period: morning, day, evening, or night.
3. Direct loop override wins first; today's theme override wins next; otherwise the scheduler uses the stored random pick for the period or creates one.
4. OBS loop source is pointed at the chosen loop, muted, set to loop, centered, and restarted.
5. OBS music source is pointed at a random music asset and restarted without looping.
6. Music-ended events choose another music asset while avoiding immediate repeats when possible.
7. Period changes cause the next stored or newly selected loop plan to start.
8. `/preview` materializes the next period's planned loop so the preview matches the later playback unless the asset is removed.
9. Telegram admin uploads that match the library filename schema are copied into the library folder and become available after scan/import.

## Management Authorization

The service uses Telegram's live group administrator list for management permissions. Admin-only commands call `getChatAdministrators` through the Telegram Bot API, cache the result briefly, and deny access if lookup fails.

## Telegram Interaction UX

Text commands remain supported, but the bot also registers Telegram command menu entries at startup and attaches inline keyboards to library, status, now-playing, preview, and upload-accepted messages. Callback buttons reuse the same authorization and hook paths as text commands.

## Library Scheduling

Loop filenames use `loop_<period>_<theme>_<variant>.<ext>`. Music filenames use `music_<track>.<ext>`. Library scans are non-recursive and ignore unsupported files with structured scan errors for `/status`.

The four periods are based on local process time:

- `morning`: 06:00-10:59
- `day`: 11:00-16:59
- `evening`: 17:00-20:59
- `night`: 21:00-05:59

Without an override, each period randomly chooses one theme with at least one matching loop and one loop asset for that theme. That period plan is persisted in SQLite so restarts and repeated previews do not redraw it. Today's direct loop or theme overrides expire at local midnight.

## Queue States

Queue states apply to `PLAYER_MODE=queue`, the legacy Telegram-submitted video mode.

- `downloading`: accepted by Telegram and being probed from the Local Bot API file path.
- `ready`: local file path validated and waiting for OBS.
- `playing`: current OBS item.
- `played`: completed or skipped with no replacement.
- `canceled`: removed before playback.
- `failed`: rejected after initial acceptance due to local path/probe/storage error.

## Data Persistence

SQLite persists all queue metadata under `DATABASE_PATH`, including the absolute local file path returned by Telegram Local Bot API Server. New uploads are not copied into `MEDIA_DIR`; their file lifecycle is owned by Telegram Local Bot API Server. The app resolves upload paths and requires them to be regular files under `TELEGRAM_BOT_API_DIR` before probing or handing them to OBS.

The Go backend, Local Bot API Server, and OBS are expected to run on the same host. A multi-host setup must provide shared storage where the Local Bot API absolute file paths and OBS media paths are readable by the relevant processes.

On restart:

- queued `ready` rows remain ordered by `queue_position`;
- stale `downloading` rows older than 6 hours are marked `failed`;
- an existing `playing` row is restarted from its local file path after OBS reconnects, so a planned restart may replay the current video from the beginning;
- if the current file is missing, that row is marked `failed` and playback advances;
- the reconnect loop tries OBS every 5 seconds, with a per-attempt timeout;
- if OBS is connected and no row is `playing`, the next `ready` row starts.

## Failure Handling

- OBS connection loss does not delete queue state.
- OBS reconnect replays the current `playing` row instead of waiting for a playback-ended event that may never arrive after a process or OBS restart.
- OBS playback failure leaves the next `ready` item in the queue instead of marking it played.
- A canceled item cannot become `ready` after cancellation.
- Retention cleanup removes old played queue rows by age and maximum file count. By default it keeps local Telegram Bot API media files; `RETENTION_DELETE_LOCAL_FILES=true` opts into deleting unreferenced local files with removed rows.
- Random fallback playback locks the active history row so retention cleanup cannot remove it mid-playback.

## Fallback Playback

When the legacy normal queue is empty, `FALLBACK_MODE=random_played` randomly selects a previously played video whose local file still exists. The bot announces when it enters random fallback mode, then keeps rotating through history until a new ready queue item is available. `FALLBACK_MODE=file` uses `OBS_FALLBACK_FILE`, and `FALLBACK_MODE=off` leaves OBS idle when the queue is empty.

## Intentional MVP Constraints

- No transcoding by default, to keep CPU use low on the MacBook.
- No web dashboard; Telegram commands are the management UI.
- Backend, Telegram Local Bot API Server, and OBS are expected to run on the same Mac.
