#!/bin/sh
set -eu

REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ENV_FILE="$REPO_ROOT/.env"
BOT_API_RUN="$REPO_ROOT/deploy/telegram-bot-api/run.sh"
BOT_API_HEALTH="$REPO_ROOT/deploy/telegram-bot-api/healthcheck.sh"
BOT_API_LOGOUT="$REPO_ROOT/deploy/telegram-bot-api/logout-public.sh"
GO_CACHE_DIR="$REPO_ROOT/.cache/go-build"
GO_MOD_CACHE_DIR="$REPO_ROOT/.cache/go-mod"
CURRENT_ENV_SCHEMA_VERSION=4
DEFAULT_APP_BIN="$REPO_ROOT/dist/tg-obs-bot"
MAX_RESTART_DELAY_SECONDS=86400

die() {
  printf '%s\n' "error: $1" >&2
  exit 1
}

info() {
  printf '%s\n' "$1" >&2
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

usage() {
  cat <<'EOF'
Usage: ./run.sh <command>

Commands:
  up              Supervise Telegram Local Bot API Server and tg-obs-bot
  bot-api         Start only Telegram Local Bot API Server
  app             Start only tg-obs-bot
  health          Check Local Bot API /getMe
  doctor          Check local config, tools, data dirs, and common ports
  env             Print sanitized runtime config
  migrate-env     Back up and append missing .env fields for the supported schema
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

dotenv_value() {
  key=$1
  awk -v want="$key" '
    {
      line = $0
      sub(/^[[:space:]]+/, "", line)
      sub(/[[:space:]]+$/, "", line)
      if (line == "" || substr(line, 1, 1) == "#") {
        next
      }
      pos = index(line, "=")
      if (pos == 0) {
        next
      }
      rawKey = substr(line, 1, pos - 1)
      value = substr(line, pos + 1)
      sub(/^[[:space:]]+/, "", rawKey)
      sub(/[[:space:]]+$/, "", rawKey)
      sub(/^[[:space:]]+/, "", value)
      sub(/[[:space:]]+$/, "", value)
      if ((substr(value, 1, 1) == "\"" && substr(value, length(value), 1) == "\"") ||
          (substr(value, 1, 1) == "'"'"'" && substr(value, length(value), 1) == "'"'"'")) {
        value = substr(value, 2, length(value) - 2)
      }
      if (rawKey == want) {
        print value
        found = 1
      }
    }
    END {
      exit found ? 0 : 0
    }
  ' "$ENV_FILE" | tail -n 1
}

dotenv_has_key() {
  key=$1
  awk -v want="$key" '
    {
      line = $0
      sub(/^[[:space:]]+/, "", line)
      sub(/[[:space:]]+$/, "", line)
      if (line == "" || substr(line, 1, 1) == "#") {
        next
      }
      pos = index(line, "=")
      if (pos == 0) {
        next
      }
      rawKey = substr(line, 1, pos - 1)
      sub(/^[[:space:]]+/, "", rawKey)
      sub(/[[:space:]]+$/, "", rawKey)
      if (rawKey == want) {
        found = 1
      }
    }
    END {
      exit found ? 0 : 1
    }
  ' "$ENV_FILE"
}

add_env_migration_line() {
  ENV_MIGRATION_ADDITIONS="${ENV_MIGRATION_ADDITIONS}${1}
"
}

join_env_path() {
  base=$(printf '%s' "$1" | sed 's#[/\\]*$##')
  if [ -z "$base" ]; then
    printf '/%s' "$2"
  else
    printf '%s/%s' "$base" "$2"
  fi
}

library_media_dir_default_from_env_file() {
  media_dir=$(dotenv_value MEDIA_DIR)
  if [ -n "$media_dir" ]; then
    printf '%s' "$media_dir"
    return
  fi
  data_dir=$(dotenv_value DATA_DIR)
  if [ -n "$data_dir" ]; then
    join_env_path "$data_dir" media
    return
  fi
  printf './data/media'
}

migrate_env() {
  [ -f "$ENV_FILE" ] || die ".env is required at repo root"

  raw_version=$(dotenv_value ENV_SCHEMA_VERSION)
  if [ -z "$raw_version" ]; then
    version=0
  elif printf '%s\n' "$raw_version" | awk '/^-?[0-9]+$/ { exit 0 } { exit 1 }'; then
    version=$raw_version
  else
    die "ENV_SCHEMA_VERSION must be an integer"
  fi

  if [ "$version" -gt "$CURRENT_ENV_SCHEMA_VERSION" ]; then
    die "ENV_SCHEMA_VERSION $version is newer than this helper supports ($CURRENT_ENV_SCHEMA_VERSION)"
  fi

  ENV_MIGRATION_ADDITIONS=
  update_schema_version=0

  if [ "$version" -lt "$CURRENT_ENV_SCHEMA_VERSION" ] && dotenv_has_key ENV_SCHEMA_VERSION; then
    update_schema_version=1
  fi

  if [ "$version" -lt 1 ]; then
    if ! dotenv_has_key ENV_SCHEMA_VERSION; then
      add_env_migration_line "ENV_SCHEMA_VERSION=$CURRENT_ENV_SCHEMA_VERSION"
    fi
    if ! dotenv_has_key TELEGRAM_API_BASE_URL; then
      add_env_migration_line "TELEGRAM_API_BASE_URL=http://127.0.0.1:8081"
    fi
    if ! dotenv_has_key MAX_VIDEO_SIZE_MB; then
      add_env_migration_line "MAX_VIDEO_SIZE_MB=2000"
    fi
  fi

  for line in \
    "TELEGRAM_API_ID=replace-with-telegram-api-id" \
    "TELEGRAM_API_HASH=replace-with-telegram-api-hash" \
    "TELEGRAM_BOT_API_BIN=telegram-bot-api" \
    "TELEGRAM_BOT_API_HOST=127.0.0.1" \
    "TELEGRAM_BOT_API_PORT=8081" \
    "TELEGRAM_BOT_API_DIR=./data/telegram-bot-api"
  do
    key=${line%%=*}
    if ! dotenv_has_key "$key"; then
      add_env_migration_line "$line"
    fi
  done
  if [ "$version" -lt 3 ]; then
    if ! dotenv_has_key RETENTION_DELETE_LOCAL_FILES; then
      add_env_migration_line "RETENTION_DELETE_LOCAL_FILES=false"
    fi
  fi
  if [ "$version" -lt 4 ]; then
    for line in \
      "PLAYER_MODE=library" \
      "OBS_LOOP_SOURCE_NAME=tg_loop_player" \
      "OBS_MUSIC_SOURCE_NAME=tg_music_player"
    do
      key=${line%%=*}
      if ! dotenv_has_key "$key"; then
        add_env_migration_line "$line"
      fi
    done
    library_media_dir=$(library_media_dir_default_from_env_file)
    if ! dotenv_has_key LOOP_MEDIA_DIR; then
      add_env_migration_line "LOOP_MEDIA_DIR=$(join_env_path "$library_media_dir" loops)"
    fi
    if ! dotenv_has_key MUSIC_MEDIA_DIR; then
      add_env_migration_line "MUSIC_MEDIA_DIR=$(join_env_path "$library_media_dir" music)"
    fi
  fi

  if [ "$update_schema_version" -eq 0 ] && [ -z "$ENV_MIGRATION_ADDITIONS" ]; then
    return 0
  fi

  backup_path="$ENV_FILE.backup.$(date +%s)"
  tmp_path="$ENV_FILE.tmp.$$"
  cp "$ENV_FILE" "$backup_path"
  chmod 600 "$backup_path"

  if [ "$update_schema_version" -eq 1 ]; then
    awk -v want="ENV_SCHEMA_VERSION" -v replacement="ENV_SCHEMA_VERSION=$CURRENT_ENV_SCHEMA_VERSION" '
      {
        line = $0
        trimmed = line
        sub(/^[[:space:]]+/, "", trimmed)
        sub(/[[:space:]]+$/, "", trimmed)
        pos = index(trimmed, "=")
        if (!done && trimmed != "" && substr(trimmed, 1, 1) != "#" && pos > 0) {
          rawKey = substr(trimmed, 1, pos - 1)
          sub(/^[[:space:]]+/, "", rawKey)
          sub(/[[:space:]]+$/, "", rawKey)
          if (rawKey == want) {
            print replacement
            done = 1
            next
          }
        }
        print line
      }
    ' "$ENV_FILE" > "$tmp_path"
  else
    cp "$ENV_FILE" "$tmp_path"
  fi

  if [ -n "$ENV_MIGRATION_ADDITIONS" ]; then
    printf '\n# Added automatically by tg-obs-bot env migration.\n' >> "$tmp_path"
    printf '%s' "$ENV_MIGRATION_ADDITIONS" >> "$tmp_path"
  fi

  chmod 600 "$tmp_path"
  mv "$tmp_path" "$ENV_FILE"
  info "Migrated .env for schema v$CURRENT_ENV_SCHEMA_VERSION; backup: $backup_path"
}

load_env() {
  migrate_env
  reject_env_xtrace
  disable_xtrace
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  disable_xtrace

  : "${TELEGRAM_BOT_API_BIN:=telegram-bot-api}"
  : "${TELEGRAM_BOT_API_HOST:=127.0.0.1}"
  : "${TELEGRAM_BOT_API_PORT:=8081}"
  : "${FFPROBE_PATH:=ffprobe}"
  : "${GO:=go}"
  : "${PLAYER_MODE:=library}"
  : "${OBS_LOOP_SOURCE_NAME:=tg_loop_player}"
  : "${OBS_MUSIC_SOURCE_NAME:=tg_music_player}"
  : "${DATA_DIR:=./data}"
  : "${MEDIA_DIR:=$(join_env_path "$DATA_DIR" media)}"
  : "${LOOP_MEDIA_DIR:=$(join_env_path "$MEDIA_DIR" loops)}"
  : "${MUSIC_MEDIA_DIR:=$(join_env_path "$MEDIA_DIR" music)}"
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

is_positive_integer() {
  value=$1
  case "$value" in
    ""|*[!0-9]*|0|0*) return 1 ;;
    ??????*) return 1 ;;
    *) return 0 ;;
  esac
}

is_integer_value() {
  printf '%s\n' "$1" | awk '/^-?[0-9]+$/ { exit 0 } { exit 1 }'
}

check_integer_range_env() {
  key=$1
  default_value=$2
  min_value=$3
  max_value=$4
  eval "value=\${$key:-}"
  if [ -z "$value" ]; then
    value=$default_value
  fi
  if is_placeholder "$value"; then
    return 0
  fi
  if ! is_integer_value "$value"; then
    printf 'fail %s must be an integer\n' "$key"
    return 1
  fi
  if ! awk -v value="$value" -v min="$min_value" -v max="$max_value" 'BEGIN { exit (value + 0 >= min && value + 0 <= max) ? 0 : 1 }'; then
    printf 'fail %s must be between %s and %s\n' "$key" "$min_value" "$max_value"
    return 1
  fi
  printf 'ok   %s numeric range\n' "$key"
  return 0
}

check_nonzero_integer_env() {
  key=$1
  eval "value=\${$key:-}"
  if is_placeholder "$value"; then
    return 0
  fi
  if ! is_integer_value "$value"; then
    printf 'fail %s must be an integer\n' "$key"
    return 1
  fi
  case "$value" in
    0|-0)
      printf 'fail %s must be non-zero\n' "$key"
      return 1
      ;;
  esac
  printf 'ok   %s numeric value\n' "$key"
  return 0
}

check_bool_env() {
  key=$1
  default_value=$2
  eval "value=\${$key:-}"
  if [ -z "$value" ]; then
    value=$default_value
  fi
  case "$value" in
    true|false|TRUE|FALSE|True|False|1|0)
      printf 'ok   %s boolean value\n' "$key"
      return 0
      ;;
  esac
  printf 'fail %s must be true or false\n' "$key"
  return 1
}

check_http_url_env() {
  key=$1
  eval "value=\${$key:-}"
  if is_placeholder "$value"; then
    return 0
  fi
  case "$value" in
    http://*|https://*)
      rest=${value#*://}
      host=${rest%%/*}
      if [ -n "$host" ]; then
        printf 'ok   %s URL\n' "$key"
        return 0
      fi
      ;;
  esac
  printf 'fail %s must be an http or https URL with a host\n' "$key"
  return 1
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

media_dir_default() {
  default_media_dir=${MEDIA_DIR:-}
  if [ -z "$default_media_dir" ]; then
    default_media_dir=$(join_env_path "${DATA_DIR:-./data}" media)
  fi
  case "$1" in
    MEDIA_DIR) printf '%s' "$default_media_dir" ;;
    LOOP_MEDIA_DIR) join_env_path "$default_media_dir" loops ;;
    MUSIC_MEDIA_DIR) join_env_path "$default_media_dir" music ;;
    *) printf '' ;;
  esac
}

ensure_go_cache() {
  mkdir -p "$GO_CACHE_DIR" "$GO_MOD_CACHE_DIR"
}

app_sources_newer_than_bin() {
  [ -x "$DEFAULT_APP_BIN" ] || return 1
  newer_files=$(find "$REPO_ROOT/cmd" "$REPO_ROOT/internal" "$REPO_ROOT/go.mod" "$REPO_ROOT/go.sum" -type f -newer "$DEFAULT_APP_BIN" -print 2>/dev/null) || return 0
  [ -n "$newer_files" ] && return 0
  return 1
}

run_app_process() {
  cd "$REPO_ROOT"
  if [ -n "${APP_BIN:-}" ]; then
    exec "$APP_BIN"
  fi
  if [ -x "$DEFAULT_APP_BIN" ]; then
    if app_sources_newer_than_bin; then
      info "Built binary at $DEFAULT_APP_BIN is older than Go sources; falling back to go run. Run ./run.sh build before unattended production use."
    else
      exec "$DEFAULT_APP_BIN"
    fi
  else
    info "Built binary not found at $DEFAULT_APP_BIN; falling back to go run. Run ./run.sh build for production."
  fi
  exec env GOCACHE="$GO_CACHE_DIR" GOMODCACHE="$GO_MOD_CACHE_DIR" "${GO:-go}" run ./cmd/tg-obs-bot
}

run_app() {
  load_env
  require_value TELEGRAM_BOT_TOKEN
  require_value TELEGRAM_API_BASE_URL
  require_value ALLOWED_CHAT_ID
  ensure_go_cache
  run_app_process
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

  : "${RESTART_MIN_DELAY_SECONDS:=2}"
  : "${RESTART_MAX_DELAY_SECONDS:=60}"
  if ! is_positive_integer "$RESTART_MIN_DELAY_SECONDS"; then
    die "RESTART_MIN_DELAY_SECONDS must be a positive integer"
  fi
  if ! is_positive_integer "$RESTART_MAX_DELAY_SECONDS"; then
    die "RESTART_MAX_DELAY_SECONDS must be a positive integer"
  fi
  if [ "$RESTART_MIN_DELAY_SECONDS" -gt "$MAX_RESTART_DELAY_SECONDS" ]; then
    die "RESTART_MIN_DELAY_SECONDS must be no greater than $MAX_RESTART_DELAY_SECONDS"
  fi
  if [ "$RESTART_MAX_DELAY_SECONDS" -gt "$MAX_RESTART_DELAY_SECONDS" ]; then
    die "RESTART_MAX_DELAY_SECONDS must be no greater than $MAX_RESTART_DELAY_SECONDS"
  fi
  if [ "$RESTART_MAX_DELAY_SECONDS" -lt "$RESTART_MIN_DELAY_SECONDS" ]; then
    die "RESTART_MAX_DELAY_SECONDS must be greater than or equal to RESTART_MIN_DELAY_SECONDS"
  fi

  bot_api_supervisor_pid=""
  app_supervisor_pid=""

  cleanup() {
    status=$?
    trap - INT TERM EXIT
    if [ -n "$app_supervisor_pid" ] && kill -0 "$app_supervisor_pid" 2>/dev/null; then
      kill "$app_supervisor_pid" 2>/dev/null || true
      wait "$app_supervisor_pid" 2>/dev/null || true
    fi
    if [ -n "$bot_api_supervisor_pid" ] && kill -0 "$bot_api_supervisor_pid" 2>/dev/null; then
      kill "$bot_api_supervisor_pid" 2>/dev/null || true
      wait "$bot_api_supervisor_pid" 2>/dev/null || true
    fi
    exit "$status"
  }

  delay_next() {
    current=$1
    if [ "$current" -lt "$RESTART_MIN_DELAY_SECONDS" ]; then
      current=$RESTART_MIN_DELAY_SECONDS
    fi
    next=$((current * 2))
    if [ "$next" -gt "$RESTART_MAX_DELAY_SECONDS" ]; then
      next=$RESTART_MAX_DELAY_SECONDS
    fi
    printf '%s' "$next"
  }

  trap cleanup INT TERM EXIT

  (
    trap 'if [ -n "${child_pid:-}" ] && kill -0 "$child_pid" 2>/dev/null; then kill "$child_pid" 2>/dev/null || true; wait "$child_pid" 2>/dev/null || true; fi; exit 0' INT TERM
    delay=$RESTART_MIN_DELAY_SECONDS
    while :; do
      info "Starting Telegram Local Bot API Server..."
      "$BOT_API_RUN" &
      child_pid=$!
      status=0
      wait "$child_pid" || status=$?
      info "Telegram Local Bot API Server exited with status $status; restarting in ${delay}s..."
      sleep "$delay"
      delay=$(delay_next "$delay")
    done
  ) &
  bot_api_supervisor_pid=$!

  if ! wait_for_health 30; then
    info "Telegram Local Bot API Server is not healthy yet; tg-obs-bot will still start and retry Telegram polling."
  fi

  (
    trap 'if [ -n "${child_pid:-}" ] && kill -0 "$child_pid" 2>/dev/null; then kill "$child_pid" 2>/dev/null || true; wait "$child_pid" 2>/dev/null || true; fi; exit 0' INT TERM
    delay=$RESTART_MIN_DELAY_SECONDS
    while :; do
      info "Starting tg-obs-bot..."
      run_app_process &
      child_pid=$!
      status=0
      wait "$child_pid" || status=$?
      info "tg-obs-bot exited with status $status; restarting in ${delay}s..."
      sleep "$delay"
      delay=$(delay_next "$delay")
    done
  ) &
  app_supervisor_pid=$!

  wait "$bot_api_supervisor_pid" "$app_supervisor_pid" || true
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

  if ! check_nonzero_integer_env ALLOWED_CHAT_ID; then
    failures=$((failures + 1))
  fi
  if ! check_http_url_env TELEGRAM_API_BASE_URL; then
    failures=$((failures + 1))
  fi
  if ! check_bool_env RETENTION_DELETE_LOCAL_FILES false; then
    failures=$((failures + 1))
  fi
  case "${PLAYER_MODE:-library}" in
    library|queue)
      printf 'ok   PLAYER_MODE: %s\n' "${PLAYER_MODE:-library}"
      ;;
    *)
      printf 'fail PLAYER_MODE must be library or queue\n'
      failures=$((failures + 1))
      ;;
  esac
  for key in OBS_MEDIA_SOURCE_NAME OBS_LOOP_SOURCE_NAME OBS_MUSIC_SOURCE_NAME; do
    eval "value=\${$key:-}"
    if [ -n "$value" ]; then
      printf 'ok   %s\n' "$key"
    else
      printf 'fail %s is required\n' "$key"
      failures=$((failures + 1))
    fi
  done
  for item in \
    "OBS_PORT:4455:1:65535" \
    "TELEGRAM_BOT_API_PORT:8081:1:65535" \
    "MAX_VIDEO_SIZE_MB:2000:1:2147483647" \
    "MAX_VIDEO_DURATION_SECONDS:7200:0:2147483647" \
    "MAX_QUEUE_LENGTH:50:1:2147483647" \
    "RETENTION_DAYS:7:0:2147483647" \
    "RETENTION_MAX_FILES:100:0:2147483647"
  do
    key=${item%%:*}
    rest=${item#*:}
    default_value=${rest%%:*}
    rest=${rest#*:}
    min_value=${rest%%:*}
    max_value=${rest#*:}
    if ! check_integer_range_env "$key" "$default_value" "$min_value" "$max_value"; then
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

  for key in MEDIA_DIR LOOP_MEDIA_DIR MUSIC_MEDIA_DIR; do
    eval "raw_dir=\${$key:-}"
    [ -n "$raw_dir" ] || raw_dir=$(media_dir_default "$key")
    case "$raw_dir" in
      /*) dir=$raw_dir ;;
      *) dir="$REPO_ROOT/$raw_dir" ;;
    esac
    if mkdir -p "$dir" 2>/dev/null; then
      printf 'ok   %s: %s\n' "$key" "$dir"
    else
      printf 'fail %s is not writable\n' "$key"
      failures=$((failures + 1))
    fi
  done

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
  printf 'OBS_LOOP_SOURCE_NAME=%s\n' "${OBS_LOOP_SOURCE_NAME:-tg_loop_player}"
  printf 'OBS_MUSIC_SOURCE_NAME=%s\n' "${OBS_MUSIC_SOURCE_NAME:-tg_music_player}"
  printf 'PLAYER_MODE=%s\n' "${PLAYER_MODE:-library}"
  printf 'DATA_DIR=%s\n' "${DATA_DIR:-./data}"
  printf 'MEDIA_DIR=%s\n' "${MEDIA_DIR:-./data/media}"
  printf 'LOOP_MEDIA_DIR=%s\n' "${LOOP_MEDIA_DIR:-./data/media/loops}"
  printf 'MUSIC_MEDIA_DIR=%s\n' "${MUSIC_MEDIA_DIR:-./data/media/music}"
  printf 'DATABASE_PATH=%s\n' "${DATABASE_PATH:-./data/queue.db}"
  printf 'RETENTION_DELETE_LOCAL_FILES=%s\n' "${RETENTION_DELETE_LOCAL_FILES:-false}"
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
  migrate-env) migrate_env ;;
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
