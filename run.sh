#!/bin/sh
set -eu

REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ENV_FILE="$REPO_ROOT/.env"
BOT_API_RUN="$REPO_ROOT/deploy/telegram-bot-api/run.sh"
BOT_API_HEALTH="$REPO_ROOT/deploy/telegram-bot-api/healthcheck.sh"
BOT_API_LOGOUT="$REPO_ROOT/deploy/telegram-bot-api/logout-public.sh"
GO_CACHE_DIR="$REPO_ROOT/.cache/go-build"
GO_MOD_CACHE_DIR="$REPO_ROOT/.cache/go-mod"

die() {
  printf '%s\n' "error: $1" >&2
  exit 1
}

info() {
  printf '%s\n' "$1" >&2
}

usage() {
  cat <<'EOF'
Usage: ./run.sh <command>

Commands:
  up              Start Telegram Local Bot API Server, wait for health, then start tg-obs-bot
  bot-api         Start only Telegram Local Bot API Server
  app             Start only tg-obs-bot
  health          Check Local Bot API /getMe
  doctor          Check local config, tools, data dirs, and common ports
  env             Print sanitized runtime config
  logout-public   Manually log the bot out from the public Telegram Bot API
  test            Run make test
  build           Run make build
  tidy            Run make tidy
  help            Show this help
EOF
}

is_placeholder() {
  case "$1" in
    ""|replace-with-*) return 0 ;;
    *) return 1 ;;
  esac
}

masked_state() {
  if [ "${1+x}" != "x" ] || [ -z "$1" ]; then
    printf '<missing>'
  elif is_placeholder "$1"; then
    printf '<placeholder>'
  else
    printf '<set>'
  fi
}

load_env() {
  [ -f "$ENV_FILE" ] || die ".env is required at repo root"
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  set +a

  : "${TELEGRAM_BOT_API_BIN:=telegram-bot-api}"
  : "${TELEGRAM_BOT_API_HOST:=127.0.0.1}"
  : "${TELEGRAM_BOT_API_PORT:=8081}"
  : "${FFPROBE_PATH:=ffprobe}"
  : "${GO:=go}"
}

require_value() {
  key=$1
  eval "value=\${$key:-}"
  if is_placeholder "$value"; then
    die "$key is required"
  fi
}

has_value() {
  key=$1
  eval "value=\${$key:-}"
  ! is_placeholder "$value"
}

require_stack_env() {
  require_value TELEGRAM_BOT_TOKEN
  require_value TELEGRAM_API_BASE_URL
  require_value TELEGRAM_API_ID
  require_value TELEGRAM_API_HASH
  require_value TELEGRAM_BOT_API_DIR
  require_value ALLOWED_CHAT_ID
}

bot_api_dir_abs() {
  case "${TELEGRAM_BOT_API_DIR:-}" in
    /*) printf '%s' "$TELEGRAM_BOT_API_DIR" ;;
    *) printf '%s/%s' "$REPO_ROOT" "$TELEGRAM_BOT_API_DIR" ;;
  esac
}

ensure_go_cache() {
  mkdir -p "$GO_CACHE_DIR" "$GO_MOD_CACHE_DIR"
}

run_app() {
  load_env
  require_value TELEGRAM_BOT_TOKEN
  require_value TELEGRAM_API_BASE_URL
  require_value ALLOWED_CHAT_ID
  ensure_go_cache
  cd "$REPO_ROOT"
  GOCACHE="$GO_CACHE_DIR" GOMODCACHE="$GO_MOD_CACHE_DIR" "${GO:-go}" run ./cmd/tg-obs-bot
}

run_bot_api() {
  load_env
  require_value TELEGRAM_API_ID
  require_value TELEGRAM_API_HASH
  require_value TELEGRAM_BOT_API_DIR
  exec "$BOT_API_RUN"
}

health() {
  load_env
  require_value TELEGRAM_BOT_TOKEN
  require_value TELEGRAM_API_BASE_URL
  "$BOT_API_HEALTH"
}

wait_for_health() {
  attempts=${1:-30}
  idx=1
  while [ "$idx" -le "$attempts" ]; do
    if "$BOT_API_HEALTH" >/dev/null 2>&1; then
      info "Telegram Local Bot API Server is healthy."
      return 0
    fi
    info "Waiting for Telegram Local Bot API Server ($idx/$attempts)..."
    idx=$((idx + 1))
    sleep 1
  done
  return 1
}

run_up() {
  load_env
  require_stack_env
  ensure_go_cache

  bot_api_pid=""
  app_pid=""

  cleanup() {
    status=$?
    trap - INT TERM EXIT
    if [ -n "$app_pid" ] && kill -0 "$app_pid" 2>/dev/null; then
      kill "$app_pid" 2>/dev/null || true
      wait "$app_pid" 2>/dev/null || true
    fi
    if [ -n "$bot_api_pid" ] && kill -0 "$bot_api_pid" 2>/dev/null; then
      kill "$bot_api_pid" 2>/dev/null || true
      wait "$bot_api_pid" 2>/dev/null || true
    fi
    exit "$status"
  }

  trap cleanup INT TERM EXIT

  info "Starting Telegram Local Bot API Server..."
  "$BOT_API_RUN" &
  bot_api_pid=$!

  if ! wait_for_health 30; then
    die "Telegram Local Bot API Server did not become healthy"
  fi

  info "Starting tg-obs-bot..."
  cd "$REPO_ROOT"
  GOCACHE="$GO_CACHE_DIR" GOMODCACHE="$GO_MOD_CACHE_DIR" "${GO:-go}" run ./cmd/tg-obs-bot &
  app_pid=$!
  wait "$app_pid"
}

check_cmd() {
  label=$1
  command=$2
  if command -v "$command" >/dev/null 2>&1; then
    printf 'ok   %s: %s\n' "$label" "$command"
    return 0
  fi
  printf 'fail %s: %s not found\n' "$label" "$command"
  return 1
}

check_port() {
  host=${TELEGRAM_BOT_API_HOST:-127.0.0.1}
  port=${TELEGRAM_BOT_API_PORT:-8081}
  if command -v lsof >/dev/null 2>&1 && lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    printf 'warn bot-api port: %s:%s is already listening; the local server may already be running\n' "$host" "$port"
    return 0
  fi
  printf 'ok   bot-api port: %s:%s is free or lsof is unavailable\n' "$host" "$port"
}

check_obs_port() {
  host=${OBS_HOST:-127.0.0.1}
  port=${OBS_PORT:-4455}
  if ! command -v lsof >/dev/null 2>&1; then
    printf 'warn OBS WebSocket: cannot check %s:%s because lsof is unavailable\n' "$host" "$port"
    return 0
  fi
  if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    printf 'ok   OBS WebSocket: %s:%s is listening\n' "$host" "$port"
    return 0
  fi
  printf 'warn OBS WebSocket: %s:%s is not listening; open OBS and enable WebSocket before running the bot\n' "$host" "$port"
}

doctor() {
  load_env
  failures=0

  for key in TELEGRAM_BOT_TOKEN TELEGRAM_API_BASE_URL TELEGRAM_API_ID TELEGRAM_API_HASH ALLOWED_CHAT_ID; do
    if has_value "$key"; then
      printf 'ok   %s\n' "$key"
    else
      printf 'fail %s is required\n' "$key"
      failures=$((failures + 1))
    fi
  done

  for item in "telegram-bot-api:$TELEGRAM_BOT_API_BIN" "go:${GO:-go}" "ffprobe:${FFPROBE_PATH:-ffprobe}" "curl:curl"; do
    label=${item%%:*}
    command=${item#*:}
    if ! check_cmd "$label" "$command"; then
      failures=$((failures + 1))
    fi
  done

  if has_value TELEGRAM_BOT_API_DIR; then
    dir=$(bot_api_dir_abs)
    if mkdir -p "$dir" 2>/dev/null; then
      printf 'ok   TELEGRAM_BOT_API_DIR: %s\n' "$dir"
    else
      printf 'fail TELEGRAM_BOT_API_DIR is not writable\n'
      failures=$((failures + 1))
    fi
  else
    printf 'fail TELEGRAM_BOT_API_DIR is required\n'
    failures=$((failures + 1))
  fi

  check_port
  check_obs_port

  if [ "$failures" -gt 0 ]; then
    die "doctor found $failures problem(s)"
  fi
}

print_env() {
  load_env
  printf 'ENV_SCHEMA_VERSION=%s\n' "${ENV_SCHEMA_VERSION:-<missing>}"
  printf 'TELEGRAM_BOT_TOKEN=%s\n' "$(masked_state "${TELEGRAM_BOT_TOKEN:-}")"
  printf 'TELEGRAM_API_BASE_URL=%s\n' "${TELEGRAM_API_BASE_URL:-<missing>}"
  printf 'TELEGRAM_API_ID=%s\n' "$(masked_state "${TELEGRAM_API_ID:-}")"
  printf 'TELEGRAM_API_HASH=%s\n' "$(masked_state "${TELEGRAM_API_HASH:-}")"
  printf 'TELEGRAM_BOT_API_BIN=%s\n' "${TELEGRAM_BOT_API_BIN:-telegram-bot-api}"
  printf 'TELEGRAM_BOT_API_HOST=%s\n' "${TELEGRAM_BOT_API_HOST:-127.0.0.1}"
  printf 'TELEGRAM_BOT_API_PORT=%s\n' "${TELEGRAM_BOT_API_PORT:-8081}"
  printf 'TELEGRAM_BOT_API_DIR=%s\n' "${TELEGRAM_BOT_API_DIR:-<missing>}"
  printf 'ALLOWED_CHAT_ID=%s\n' "${ALLOWED_CHAT_ID:-<missing>}"
  printf 'OBS_HOST=%s\n' "${OBS_HOST:-127.0.0.1}"
  printf 'OBS_PORT=%s\n' "${OBS_PORT:-4455}"
  printf 'OBS_PASSWORD=%s\n' "$(masked_state "${OBS_PASSWORD:-}")"
  printf 'OBS_MEDIA_SOURCE_NAME=%s\n' "${OBS_MEDIA_SOURCE_NAME:-tg_queue_player}"
  printf 'DATA_DIR=%s\n' "${DATA_DIR:-./data}"
  printf 'MEDIA_DIR=%s\n' "${MEDIA_DIR:-./data/media}"
  printf 'DATABASE_PATH=%s\n' "${DATABASE_PATH:-./data/queue.db}"
  printf 'FFPROBE_PATH=%s\n' "${FFPROBE_PATH:-ffprobe}"
  printf 'LOG_LEVEL=%s\n' "${LOG_LEVEL:-info}"
}

run_make_target() {
  target=$1
  cd "$REPO_ROOT"
  make "$target"
}

cmd=${1:-help}
case "$cmd" in
  up) run_up ;;
  bot-api) run_bot_api ;;
  app) run_app ;;
  health) health ;;
  doctor) doctor ;;
  env) print_env ;;
  logout-public)
    load_env
    require_value TELEGRAM_BOT_TOKEN
    exec "$BOT_API_LOGOUT"
    ;;
  test) run_make_target test ;;
  build) run_make_target build ;;
  tidy) run_make_target tidy ;;
  help|-h|--help) usage ;;
  *) usage >&2; die "unknown command: $cmd" ;;
esac
