# Security

This project is intended to be safe to publish as source code. Runtime secrets must stay outside git.

## Do Not Commit

- `.env` or `.env.*` files
- Telegram bot tokens
- OBS WebSocket passwords
- private keys, certificates, or credential files
- downloaded media under `data/`
- local build output under `dist/`
- Go build/module caches under `.cache/`

## Before Publishing

Run:

```sh
git status --short --ignored
rg --hidden -g '!.git/**' -g '!.cache/**' -g '!dist/**' -g '!data/**' -i '(token|secret|password|api[_-]?key|private[_-]?key|credential|authorization|bearer)'
make test
make build
```

Expected ignored local directories:

- `.cache/`
- `dist/`
- `data/` if you have run the service locally
