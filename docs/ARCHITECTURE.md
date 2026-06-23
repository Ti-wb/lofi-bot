# Architecture

`tg-obs-bot` is a single-process Go service for running a Telegram-submitted video queue through OBS. It requires Telegram Local Bot API Server; the public Telegram Bot API is intentionally unsupported for uploads.

## Components

- `cmd/tg-obs-bot`: process entrypoint, config loading, signal handling.
- `internal/config`: `.env` and environment variable parsing.
- `internal/app`: orchestration between Telegram, queue, media storage, and OBS.
- `internal/telegram`: Telegram update loop, uploads, commands, and live group admin checks.
- `internal/obs`: OBS WebSocket v5 client, auth handshake, media source control, playback-ended events.
- `internal/queue`: SQLite-backed queue state and ordering.
- `internal/media`: local Telegram file probing, `ffprobe` metadata, disk usage.
- Telegram Local Bot API Server: local Bot API endpoint configured by `TELEGRAM_API_BASE_URL`.

## Runtime Flow

1. A user sends a video in the configured Telegram group.
2. Telegram service validates group membership and upload type.
3. Telegram service resolves the file through Local Bot API `getFile`; the server must be running with `--local` so the response includes an absolute local file path.
4. App service checks queue length and file size, records a `downloading` row, then probes the local file path.
5. Media service probes duration and validates configured limits.
6. Queue store marks the item `ready` and assigns a queue position.
7. If OBS is connected and idle, app service starts playback immediately.
8. OBS client updates the configured Media Source and triggers restart.
9. OBS playback-ended events cause app service to mark the current item played and advance to the next ready item.

## Management Authorization

The service uses Telegram's live group administrator list for management permissions. Admin-only commands call `getChatAdministrators` through the Telegram Bot API, cache the result briefly, and deny access if lookup fails.

## Telegram Interaction UX

Text commands remain supported, but the bot also registers Telegram command menu entries at startup and attaches inline keyboards to queue, status, now-playing, history, and upload-accepted messages. Callback buttons reuse the same authorization and hook paths as text commands.

## Queue States

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

When the normal queue is empty, `FALLBACK_MODE=random_played` randomly selects a previously played video whose local file still exists. The bot announces when it enters random fallback mode, then keeps rotating through history until a new ready queue item is available. `FALLBACK_MODE=file` uses `OBS_FALLBACK_FILE`, and `FALLBACK_MODE=off` leaves OBS idle when the queue is empty.

## Intentional MVP Constraints

- No transcoding by default, to keep CPU use low on the MacBook.
- No web dashboard; Telegram commands are the management UI.
- Backend, Telegram Local Bot API Server, and OBS are expected to run on the same Mac.
