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

: "${TELEGRAM_BOT_API_BIN:=telegram-bot-api}"
: "${TELEGRAM_BOT_API_HOST:=127.0.0.1}"
: "${TELEGRAM_BOT_API_PORT:=8081}"

if is_placeholder "${TELEGRAM_API_ID:-}"; then
  die "TELEGRAM_API_ID is required"
fi
if is_placeholder "${TELEGRAM_API_HASH:-}"; then
  die "TELEGRAM_API_HASH is required"
fi
if is_placeholder "${TELEGRAM_BOT_API_DIR:-}"; then
  die "TELEGRAM_BOT_API_DIR is required"
fi

case "$TELEGRAM_BOT_API_DIR" in
  /*) BOT_API_DIR="$TELEGRAM_BOT_API_DIR" ;;
  *) BOT_API_DIR="$REPO_ROOT/$TELEGRAM_BOT_API_DIR" ;;
esac

mkdir -p "$BOT_API_DIR"

exec "$TELEGRAM_BOT_API_BIN" \
  --api-id="$TELEGRAM_API_ID" \
  --api-hash="$TELEGRAM_API_HASH" \
  --local \
  --http-ip-address="$TELEGRAM_BOT_API_HOST" \
  --http-port="$TELEGRAM_BOT_API_PORT" \
  --dir="$BOT_API_DIR"
