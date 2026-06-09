# Telegram Local Bot API Server

This folder contains local deployment helpers for the official Telegram Bot API Server used by this project.

The server must run with `--local`. `tg-obs-bot` relies on `getFile.file_path` being an absolute local path, and the public Telegram Bot API does not provide that contract.

## Shared `.env`

Use the repository root `.env` for both `tg-obs-bot` and Telegram Bot API Server. Do not create a second secret file for this folder.

`.env` is ignored by git. `.env.example` is the versioned schema and is safe to commit because it contains only placeholders and local defaults.

Required Telegram Bot API Server fields:

```env
TELEGRAM_BOT_TOKEN=replace-with-telegram-bot-token
TELEGRAM_API_BASE_URL=http://127.0.0.1:8081
TELEGRAM_API_ID=replace-with-telegram-api-id
TELEGRAM_API_HASH=replace-with-telegram-api-hash
TELEGRAM_BOT_API_BIN=telegram-bot-api
TELEGRAM_BOT_API_HOST=127.0.0.1
TELEGRAM_BOT_API_PORT=8081
TELEGRAM_BOT_API_DIR=./data/telegram-bot-api
```

`TELEGRAM_BOT_TOKEN` comes from BotFather. `TELEGRAM_API_ID` and `TELEGRAM_API_HASH` come from Telegram application settings.

## First-Time Local Server Switch

1. Copy and fill the shared config:

   ```sh
   cp .env.example .env
   ```

2. Install or build the official `telegram-bot-api` binary and make sure `TELEGRAM_BOT_API_BIN` points to it.

3. Log the bot out from the public Telegram Bot API. This is a manual operator step and is not run automatically:

   ```sh
   deploy/telegram-bot-api/logout-public.sh
   ```

4. Start the local server:

   ```sh
   deploy/telegram-bot-api/run.sh
   ```

5. In another terminal, verify the local endpoint:

   ```sh
   deploy/telegram-bot-api/healthcheck.sh
   ```

## Runtime Contract

`run.sh` starts the server with:

```sh
telegram-bot-api \
  --api-id="$TELEGRAM_API_ID" \
  --api-hash="$TELEGRAM_API_HASH" \
  --local \
  --http-ip-address="$TELEGRAM_BOT_API_HOST" \
  --http-port="$TELEGRAM_BOT_API_PORT" \
  --dir="$TELEGRAM_BOT_API_DIR"
```

Keep `TELEGRAM_BOT_API_DIR` stable across restarts. Changing or deleting it can invalidate file paths that were already returned by `getFile` and stored in the queue.

Keep `TELEGRAM_BOT_API_HOST=127.0.0.1` unless you have a specific local network reason to expose the server. The server accepts HTTP; do not expose it directly to the public internet.

## Scripts

- `run.sh`: loads the root `.env`, validates required local-server settings, creates `TELEGRAM_BOT_API_DIR`, and execs `telegram-bot-api`.
- `healthcheck.sh`: loads the root `.env` and calls local `/getMe` through `TELEGRAM_API_BASE_URL`.
- `logout-public.sh`: loads the root `.env` and calls public Bot API `logOut`. Run it manually before the first local-server switch or when moving back from another Bot API server.

The scripts do not print token or hash values. Error messages name the missing key only.
