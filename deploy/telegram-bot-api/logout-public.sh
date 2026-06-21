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

if is_placeholder "${TELEGRAM_BOT_TOKEN:-}"; then
  die "TELEGRAM_BOT_TOKEN is required"
fi

printf '%s\n' "Logging the bot out from the public Telegram Bot API. Run this manually before switching to the local server."
curl_status=0
printf 'url = "https://api.telegram.org/bot%s/logOut"\n' "$TELEGRAM_BOT_TOKEN" | curl -fsS --config - || curl_status=$?
if [ "$curl_status" -ne 0 ]; then
  die "public Telegram Bot API logOut failed (curl exit $curl_status)"
fi
printf '\n'
