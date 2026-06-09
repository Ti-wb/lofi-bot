#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
ENV_FILE="$REPO_ROOT/.env"

die() {
  printf '%s\n' "error: $1" >&2
  exit 1
}

is_placeholder() {
  case "$1" in
    ""|replace-with-*) return 0 ;;
    *) return 1 ;;
  esac
}

[ -f "$ENV_FILE" ] || die ".env is required at repo root"

set -a
# shellcheck disable=SC1090
. "$ENV_FILE"
set +a

if is_placeholder "${TELEGRAM_BOT_TOKEN:-}"; then
  die "TELEGRAM_BOT_TOKEN is required"
fi

printf '%s\n' "Logging the bot out from the public Telegram Bot API. Run this manually before switching to the local server."
printf 'url = "https://api.telegram.org/bot%s/logOut"\n' "$TELEGRAM_BOT_TOKEN" | curl -fsS --config -
printf '\n'
