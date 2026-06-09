# Telegram Local Bot API Server

This folder is reserved for the Local Bot API Server deployment assets. Future work may add macOS LaunchAgent plists, wrapper scripts, and log rotation helpers here.

## Requirements

- Telegram `api_id` and `api_hash` from Telegram's application settings.
- Bot token from BotFather.
- A fixed data directory on local disk.
- The Go backend, Local Bot API Server, and OBS running on the same machine, or shared paths readable by the backend and OBS.

## Runtime Contract

Run Telegram Bot API Server with `--local`. This project relies on `getFile` returning an absolute local file path, so the public Bot API is not supported.

Set the backend environment to the server base URL:

```env
TELEGRAM_API_BASE_URL=http://127.0.0.1:8081
```

Keep the Local Bot API data directory stable across restarts. Changing it can make previously returned file paths invalid.

## Placeholder

No managed service files live here yet. When added, keep them minimal and make `api_id`, `api_hash`, bot token, port, and data directory explicit operator inputs.
