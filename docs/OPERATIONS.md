# Operations

## First-Time Setup

1. Install dependencies:

   ```sh
   brew install go ffmpeg
   ```

2. Configure OBS:

   - enable OBS WebSocket;
   - use port `4455` unless changed in `.env`;
   - create a Media Source named `tg_queue_player`;
   - disable looping on that source.

3. Configure Telegram:

   - create a bot with BotFather;
   - add it to the target group;
   - collect the group chat ID;
   - make sure queue managers are Telegram group admins.

4. Create local config:

   ```sh
   cp .env.example .env
   ```

5. Fill `.env`.

## Local Runbook

Download dependencies:

```sh
make tidy
```

Run tests:

```sh
make test
```

Build:

```sh
make build
```

Run:

```sh
make run
```

The built binary is written to `dist/tg-obs-bot`.

## Telegram Admin Commands

Telegram group admins can use these management commands. Anonymous admins should disable anonymous admin mode before issuing commands, because Telegram does not expose their real user ID to the bot.

- `/queue`: show current and upcoming videos.
- `/now`: show the current video.
- `/status`: show OBS status, queue counts, media size, disk space, and last error.
- `/history`: show recent played, canceled, or failed items.
- `/remove <id>`: cancel a queued item.
- `/move <id> <position>`: move a ready item.
- `/skip`: skip current playback.

## Storage And Retention

`RETENTION_DAYS` and `RETENTION_MAX_FILES` are both active. A played item can be deleted when it is older than the age limit or when the played history exceeds the file count limit.

Set conservative values on the MacBook first, for example:

```env
RETENTION_DAYS=7
RETENTION_MAX_FILES=100
MAX_VIDEO_SIZE_MB=500
MAX_QUEUE_LENGTH=50
```

## Troubleshooting

If `/status` says OBS is disconnected:

- confirm OBS is open;
- confirm WebSocket is enabled;
- confirm `OBS_HOST`, `OBS_PORT`, and `OBS_PASSWORD`;
- confirm macOS firewall is not blocking local WebSocket access.

If uploads fail after acceptance:

- check `ffprobe` is installed;
- check free disk space in `/status`;
- check `MAX_VIDEO_SIZE_MB` and `MAX_VIDEO_DURATION_SECONDS`;
- inspect service logs for download/probe errors.

If videos do not visually change in OBS:

- confirm the Media Source name exactly matches `OBS_MEDIA_SOURCE_NAME`;
- confirm the source supports local files;
- confirm the source is visible in the active scene.

## Suggested LaunchAgent

For unattended use, build once and create a macOS LaunchAgent that runs `dist/tg-obs-bot` from this project directory. Keep `.env`, `data/`, and logs on local disk.
