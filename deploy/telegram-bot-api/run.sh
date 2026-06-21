#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
ENV_FILE="$REPO_ROOT/.env"

die() {
  printf '%s\n' "error: $1" >&2
  exit 1
}

disable_xtrace() {
  case $- in
    *x*) set +x ;;
  esac
}

reject_env_xtrace() {
  awk '
    {
      line = $0
      sub(/^[[:space:]]+/, "", line)
      sub(/[[:space:]]+$/, "", line)
      if (line == "" || substr(line, 1, 1) == "#") {
        next
      }
      if (line ~ /^set([[:space:]]|$)/ && line ~ /(^|[[:space:]])(-[^;#[:space:]]*x|-o[[:space:]]+xtrace|\+o[[:space:]]+xtrace)([;#[:space:]]|$)/) {
        exit 1
      }
    }
  ' "$ENV_FILE" || die ".env must not enable shell tracing"
}

disable_xtrace

is_placeholder() {
  case "$1" in
    ""|replace-with-*) return 0 ;;
    *) return 1 ;;
  esac
}

[ -f "$ENV_FILE" ] || die ".env is required at repo root"

reject_env_xtrace
disable_xtrace
# shellcheck disable=SC1090
. "$ENV_FILE"
disable_xtrace

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
