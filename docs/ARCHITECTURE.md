# Architecture

`tg-obs-bot` is a single-process Go service for running a Telegram-submitted video queue through OBS.

## Components

- `cmd/tg-obs-bot`: process entrypoint, config loading, signal handling.
- `internal/config`: `.env` and environment variable parsing.
- `internal/app`: orchestration between Telegram, queue, media storage, and OBS.
- `internal/telegram`: Telegram update loop, uploads, commands, and live group admin checks.
- `internal/obs`: OBS WebSocket v5 client, auth handshake, media source control, playback-ended events.
- `internal/queue`: SQLite-backed queue state and ordering.
- `internal/media`: Telegram file download, file naming, `ffprobe` metadata, disk usage.

## Runtime Flow

1. A user sends a video in the configured Telegram group.
2. Telegram service validates group membership and upload type.
3. App service checks queue length and file size, records a `downloading` row, then downloads the file.
4. Media service probes duration and validates configured limits.
5. Queue store marks the item `ready` and assigns a queue position.
6. If OBS is connected and idle, app service starts playback immediately.
7. OBS client updates the configured Media Source and triggers restart.
8. OBS playback-ended events cause app service to mark the current item played and advance to the next ready item.

## Management Authorization

The service uses Telegram's live group administrator list for management permissions. Admin-only commands call `getChatAdministrators` through the Telegram Bot API, cache the result briefly, and deny access if lookup fails.

## Telegram Interaction UX

Text commands remain supported, but the bot also registers Telegram command menu entries at startup and attaches inline keyboards to queue, status, now-playing, history, and upload-accepted messages. Callback buttons reuse the same authorization and hook paths as text commands.

## Queue States

- `downloading`: accepted by Telegram and being downloaded.
- `ready`: downloaded, validated, and waiting for OBS.
- `playing`: current OBS item.
- `played`: completed or skipped with no replacement.
- `canceled`: removed before playback.
- `failed`: rejected after initial acceptance due to download/probe/storage error.

## Data Persistence

SQLite persists all queue metadata under `DATABASE_PATH`. Video files live under `MEDIA_DIR`.

On restart:

- queued `ready` rows remain ordered by `queue_position`;
- the reconnect loop tries OBS every 5 seconds;
- if OBS is connected and no row is `playing`, the next `ready` row starts.

## Failure Handling

- OBS connection loss does not delete queue state.
- OBS playback failure leaves the next `ready` item in the queue instead of marking it played.
- A canceled item that finishes downloading cannot become `ready`; the downloaded file is removed by app logic.
- Retention cleanup removes old played files by age and maximum file count.
- Random fallback playback locks the active history file so retention cleanup cannot delete it mid-playback.

## Fallback Playback

When the normal queue is empty, `FALLBACK_MODE=random_played` randomly selects a previously played video whose local file still exists. The bot announces when it enters random fallback mode, then keeps rotating through history until a new ready queue item is available. `FALLBACK_MODE=file` uses `OBS_FALLBACK_FILE`, and `FALLBACK_MODE=off` leaves OBS idle when the queue is empty.

## Intentional MVP Constraints

- No transcoding by default, to keep CPU use low on the MacBook.
- No web dashboard; Telegram commands are the management UI.
- Backend and OBS are expected to run on the same Mac.
