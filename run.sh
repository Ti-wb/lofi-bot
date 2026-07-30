#!/bin/sh
set -eu

REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ENV_FILE="$REPO_ROOT/.env"
BOT_API_RUN="$REPO_ROOT/deploy/telegram-bot-api/run.sh"
BOT_API_HEALTH="$REPO_ROOT/deploy/telegram-bot-api/healthcheck.sh"
BOT_API_LOGOUT="$REPO_ROOT/deploy/telegram-bot-api/logout-public.sh"
LIVENESS_READER_AWK="$REPO_ROOT/scripts/liveness-reader.awk"
GO_CACHE_DIR="$REPO_ROOT/.cache/go-build"
GO_MOD_CACHE_DIR="$REPO_ROOT/.cache/go-mod"
CURRENT_ENV_SCHEMA_VERSION=5
DEFAULT_APP_BIN="$REPO_ROOT/dist/tg-obs-bot"
MAX_RESTART_DELAY_SECONDS=86400
ROOT_SHUTDOWN_BUFFER_SECONDS=2
APP_INSTANCE_CONTENTION_EXIT_CODE=73
LIVENESS_TICK_SECONDS=1
LIVENESS_STARTUP_GRACE_SECONDS=360
LIVENESS_NORMAL_STALE_SECONDS=60
LIVENESS_MEDIA_PROBE_STALE_SECONDS=150
LIVENESS_LONG_OPERATION_STALE_SECONDS=310
LIVENESS_APP_EXIT_RACE_SECONDS=1

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

secure_env_file() {
  chmod 600 "$ENV_FILE" || die "could not secure .env"
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
  secure_env_file

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
  if [ "$version" -lt 5 ]; then
    if ! dotenv_has_key MIN_FREE_DISK_MB; then
      add_env_migration_line "MIN_FREE_DISK_MB=512"
    fi
  fi

  if [ "$update_schema_version" -eq 0 ] && [ -z "$ENV_MIGRATION_ADDITIONS" ]; then
    return 0
  fi

  backup_path="$ENV_FILE.backup.$(date +%s)"
  tmp_path="$ENV_FILE.tmp.$$"
  (umask 077 && cp "$ENV_FILE" "$backup_path")
  chmod 600 "$backup_path"

  if [ "$update_schema_version" -eq 1 ]; then
    (
      umask 077
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
    )
  else
    (umask 077 && cp "$ENV_FILE" "$tmp_path")
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
  : "${MIN_FREE_DISK_MB:=512}"
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

build_default_app_binary() {
  mkdir -p "$REPO_ROOT/dist"
  build_path="$REPO_ROOT/dist/.tg-obs-bot.build.$$"
  build_signal_status=
  build_pid=
  build_launching=true

  stop_build_process_group() {
    trap - HUP INT TERM
    if [ -z "$build_pid" ] && [ "$build_launching" = true ]; then
      capture_last_background_pid
      build_pid=$LAST_BACKGROUND_PID
    fi
    if [ -n "$build_pid" ]; then
      if is_process_group_leader "$build_pid"; then
        stop_child_process_group "$build_pid" "tg-obs-bot build" || true
      elif kill -0 "$build_pid" 2>/dev/null; then
        stop_single_child "$build_pid" "tg-obs-bot build"
      fi
    fi
    exit "$build_signal_status"
  }

  trap 'build_signal_status=129; stop_build_process_group' HUP
  trap 'build_signal_status=130; stop_build_process_group' INT
  trap 'build_signal_status=143; stop_build_process_group' TERM
  set -m
  (
    set +m
    trap 'rm -f "$build_path"' 0
    trap 'exit 1' HUP INT TERM
    cd "$REPO_ROOT"
    info "Building tg-obs-bot..."
    env GOCACHE="$GO_CACHE_DIR" GOMODCACHE="$GO_MOD_CACHE_DIR" "${GO:-go}" build -o "$build_path" ./cmd/tg-obs-bot
    mv "$build_path" "$DEFAULT_APP_BIN"
    trap - 0 HUP INT TERM
  ) &
  build_pid=$!
  set +m
  build_launching=false
  if ! is_process_group_leader "$build_pid"; then
    if process_has_exited "$build_pid"; then
      build_status=0
      wait "$build_pid" 2>/dev/null || build_status=$?
      if ! process_group_alive "$build_pid"; then
        trap - HUP INT TERM
        return "$build_status"
      fi
      stop_child_process_group "$build_pid" "tg-obs-bot build" || true
    fi
    kill -TERM "$build_pid" 2>/dev/null || true
    wait "$build_pid" 2>/dev/null || true
    die "could not isolate tg-obs-bot build process group"
  fi

  build_status=0
  wait "$build_pid" || build_status=$?
  trap - HUP INT TERM
  return "$build_status"
}

prepare_app_binary() {
  if [ -n "${APP_BIN:-}" ]; then
    case "$APP_BIN" in
      /*) app_executable=$APP_BIN ;;
      */*) app_executable="$REPO_ROOT/$APP_BIN" ;;
      *)
        app_executable=$(command -v "$APP_BIN" 2>/dev/null) || die "APP_BIN is not executable or not found: $APP_BIN"
        ;;
    esac
    [ -f "$app_executable" ] && [ -x "$app_executable" ] || die "APP_BIN is not executable or not found: $APP_BIN"
    APP_EXECUTABLE=$app_executable
    return
  fi

  if [ ! -x "$DEFAULT_APP_BIN" ]; then
    info "Built binary not found at $DEFAULT_APP_BIN; building it before startup."
    build_default_app_binary
  elif app_sources_newer_than_bin; then
    info "Built binary at $DEFAULT_APP_BIN is older than Go sources; rebuilding it before startup."
    build_default_app_binary
  fi
  APP_EXECUTABLE=$DEFAULT_APP_BIN
}

run_app_process() {
  cd "$REPO_ROOT"
  exec "$APP_EXECUTABLE"
}

run_app() {
  load_env
  unset TG_OBS_LIVENESS_FD3
  require_value TELEGRAM_BOT_TOKEN
  require_value TELEGRAM_API_BASE_URL
  require_value ALLOWED_CHAT_ID
  ensure_go_cache
  : "${SHUTDOWN_GRACE_SECONDS:=15}"
  if ! is_positive_integer "$SHUTDOWN_GRACE_SECONDS"; then
    die "SHUTDOWN_GRACE_SECONDS must be a positive integer"
  fi
  if [ "$SHUTDOWN_GRACE_SECONDS" -gt "$MAX_RESTART_DELAY_SECONDS" ]; then
    die "SHUTDOWN_GRACE_SECONDS must be no greater than $MAX_RESTART_DELAY_SECONDS"
  fi
  prepare_app_binary
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

signal_process_group() {
  signal=$1
  group_leader=$2
  /bin/kill "-$signal" "-$group_leader" 2>/dev/null || true
}

capture_last_background_pid() {
  set +u
  LAST_BACKGROUND_PID=$!
  set -u
}

stop_single_child() {
  single_pid=$1
  single_name=$2
  kill -TERM "$single_pid" 2>/dev/null || true
  (
    sleep "$SHUTDOWN_GRACE_SECONDS"
    if kill -0 "$single_pid" 2>/dev/null; then
      info "$single_name did not stop within ${SHUTDOWN_GRACE_SECONDS}s; sending KILL to PID $single_pid."
      kill -KILL "$single_pid" 2>/dev/null || true
    fi
  ) &
  single_killer_pid=$!
  wait "$single_pid" 2>/dev/null || true
  kill "$single_killer_pid" 2>/dev/null || true
  wait "$single_killer_pid" 2>/dev/null || true
}

wait_for_process_group_exit() {
  wait_group_leader=$1
  wait_seconds=$2
  wait_elapsed=0
  while [ "$wait_elapsed" -lt "$wait_seconds" ]; do
    if ! process_group_alive "$wait_group_leader"; then
      return 0
    fi
    sleep 1
    wait_elapsed=$((wait_elapsed + 1))
  done
  ! process_group_alive "$wait_group_leader"
}

drain_process_group() {
  drain_group_leader=$1
  drain_name=$2
  if ! process_group_alive "$drain_group_leader"; then
    return 0
  fi
  signal_process_group TERM "$drain_group_leader"
  if ! wait_for_process_group_exit "$drain_group_leader" "$SHUTDOWN_GRACE_SECONDS"; then
    info "$drain_name left processes behind after exit; sending KILL to process group $drain_group_leader."
    signal_process_group KILL "$drain_group_leader"
    wait_for_process_group_exit "$drain_group_leader" 1 || return 1
  fi
}

stop_child_process_group() {
  stop_group_leader=$1
  stop_name=$2
  if ! process_group_alive "$stop_group_leader"; then
    wait "$stop_group_leader" 2>/dev/null || true
    return 0
  fi

  signal_process_group TERM "$stop_group_leader"
  (
    sleep "$SHUTDOWN_GRACE_SECONDS"
    if process_group_alive "$stop_group_leader"; then
      info "$stop_name did not stop within ${SHUTDOWN_GRACE_SECONDS}s; sending KILL to process group $stop_group_leader."
      signal_process_group KILL "$stop_group_leader"
    fi
  ) &
  stop_killer_pid=$!
  wait "$stop_group_leader" 2>/dev/null || true
  if process_group_alive "$stop_group_leader"; then
    wait "$stop_killer_pid" 2>/dev/null || true
  else
    kill "$stop_killer_pid" 2>/dev/null || true
    wait "$stop_killer_pid" 2>/dev/null || true
  fi
  if process_group_alive "$stop_group_leader"; then
    signal_process_group KILL "$stop_group_leader"
    wait_for_process_group_exit "$stop_group_leader" 1 || return 1
  fi
}

process_group_alive() {
  group_leader=$1
  /bin/kill -0 "-$group_leader" 2>/dev/null
}

is_process_group_leader() {
  process_pid=$1
  process_pgid=$(ps -o pgid= -p "$process_pid" 2>/dev/null | tr -d '[:space:]')
  [ "$process_pgid" = "$process_pid" ]
}

read_process_group_state() {
  state_file=$1
  state_group=
  if [ -r "$state_file" ]; then
    IFS= read -r state_group <"$state_file" || state_group=
  fi
  case "$state_group" in
    ""|*[!0-9]*|0) return 0 ;;
  esac
  printf '%s' "$state_group"
}

process_has_exited() {
  watched_pid=$1
  watched_state=$(ps -o stat= -p "$watched_pid" 2>/dev/null | tr -d '[:space:]')
  case "$watched_state" in
    ""|Z*) return 0 ;;
    *) return 1 ;;
  esac
}

cleanup_liveness_dir() {
  rm -f "$1/frames.fifo" "$1/state" "$1/state.tmp" || return 1
  [ ! -d "$1" ] || rmdir "$1"
}

supervise_service() {
  service_name=$1
  service_kind=$2
  supervisor_state_file=$3
  set +m
  child_pid=
  child_launching=false
  child_group_owned=false
  liveness_reader_pid=
  liveness_generation_dir="${supervisor_state_file%/*}/app-liveness"
  LIVENESS_FIFO="$liveness_generation_dir/frames.fifo"
  LIVENESS_STATE="$liveness_generation_dir/state"
  LIVENESS_STATE_TMP="$liveness_generation_dir/state.tmp"

  record_child_group() {
    recorded_group=$1
    child_group_owned=true
    if ! printf '%s\n' "$recorded_group" >"${supervisor_state_file}.tmp" ||
       ! mv "${supervisor_state_file}.tmp" "$supervisor_state_file"; then
      info "error: could not record $service_name child process group $recorded_group"
      stop_child_process_group "$recorded_group" "$service_name" || true
      rm -f "${supervisor_state_file}.tmp"
      child_group_owned=false
      kill -TERM "$root_supervisor_pid" 2>/dev/null || true
      return 1
    fi
  }

  clear_child_group() {
    rm -f "$supervisor_state_file" "${supervisor_state_file}.tmp"
    child_group_owned=false
  }

  prepare_liveness_generation() {
    [ ! -e "$liveness_generation_dir" ] || return 1
    (umask 077 && mkdir "$liveness_generation_dir" &&
      mkfifo "$LIVENESS_FIFO") &&
      chmod 700 "$liveness_generation_dir" &&
      chmod 600 "$LIVENESS_FIFO" &&
      [ -p "$LIVENESS_FIFO" ] && return 0
    cleanup_liveness_dir "$liveness_generation_dir" || true
    return 1
  }

  stop_liveness_reader_process() {
    [ -n "$liveness_reader_pid" ] || return 0
    process_has_exited "$liveness_reader_pid" ||
      kill -TERM "$liveness_reader_pid" 2>/dev/null || true
    wait "$liveness_reader_pid" 2>/dev/null || true
    liveness_reader_pid=
  }

  consume_liveness_state() {
    consume_line=$1
    case "$consume_line" in
      "ERROR malformed-frame"|"ERROR frame-order"|"ERROR worker-sequence-decrease"|"ERROR worker-phase-without-progress"|"ERROR channel-eof")
        LIVENESS_WATCHDOG_FAILURE=${consume_line#ERROR }; return 1 ;;
      ERROR*) LIVENESS_WATCHDOG_FAILURE=reader-exited; return 1 ;;
    esac
    consume_old_ifs=$IFS
    IFS=' '
    # The state is emitted only by the strict reader in the private directory.
    # shellcheck disable=SC2086
    set -- $consume_line
    IFS=$consume_old_ifs
    if [ "$#" -ne 7 ] || [ "$1" != TGOBS1 ] ||
       [ "${3#t=}" = "$3" ] || [ "${4#r=}" = "$4" ] ||
       [ "${5#e=}" = "$5" ] || [ "${6#m=}" = "$6" ] ||
       [ "${7#p=}" = "$7" ]; then
      LIVENESS_WATCHDOG_FAILURE=invalid-state
      return 1
    fi
    consume_frame=$2
    [ "$consume_frame" != "$liveness_frame" ] || return 0
    shift 2
    for consume_worker in t r e m p; do
      consume_value=${1#?=}
      eval "consume_previous=\$liveness_${consume_worker}_value"
      if [ "$consume_value" != "$consume_previous" ]; then
        eval "liveness_${consume_worker}_value=\$consume_value"
        eval "liveness_${consume_worker}_age=-1"
      fi
      shift
    done
    liveness_frame=$consume_frame
  }

  tick_liveness_watchdog() {
    if [ -z "$liveness_frame" ]; then
      liveness_startup_age=$((liveness_startup_age + 1))
      if [ "$liveness_startup_age" -ge "$LIVENESS_STARTUP_GRACE_SECONDS" ]; then
        LIVENESS_WATCHDOG_FAILURE=startup-timeout
        LIVENESS_WATCHDOG_AGE=$liveness_startup_age
        return 1
      fi
      return 0
    fi
    for liveness_worker_spec in \
      t:telegram r:obs-reconnect e:obs-events m:maintenance p:playback
    do
      liveness_worker=${liveness_worker_spec%%:*}
      liveness_worker_name=${liveness_worker_spec#*:}
      eval "liveness_value=\$liveness_${liveness_worker}_value"
      eval "liveness_age=\$liveness_${liveness_worker}_age"
      liveness_age=$((liveness_age + 1))
      eval "liveness_${liveness_worker}_age=\$liveness_age"
      liveness_phase=${liveness_value#*,}
      case "$liveness_phase" in
        08) liveness_limit=$LIVENESS_MEDIA_PROBE_STALE_SECONDS ;;
        09|10) liveness_limit=$LIVENESS_LONG_OPERATION_STALE_SECONDS ;;
        *) liveness_limit=$LIVENESS_NORMAL_STALE_SECONDS ;;
      esac
      if [ "$liveness_age" -ge "$liveness_limit" ]; then
        LIVENESS_WATCHDOG_FAILURE=stale
        LIVENESS_WATCHDOG_WORKER=$liveness_worker_name
        LIVENESS_WATCHDOG_PHASE=$liveness_phase
        LIVENESS_WATCHDOG_AGE=$liveness_age
        return 1
      fi
    done
  }

  stop_supervised_child() {
    trap - HUP INT TERM
    if [ -z "$child_pid" ] && [ "$child_launching" = true ]; then
      capture_last_background_pid
      if [ "$LAST_BACKGROUND_PID" != "$liveness_reader_pid" ]; then
        child_pid=$LAST_BACKGROUND_PID
      fi
    fi
    stop_liveness_reader_process
    if [ -n "$child_pid" ]; then
      if [ "$child_group_owned" = true ]; then
        if stop_child_process_group "$child_pid" "$service_name"; then
          clear_child_group
        fi
      elif is_process_group_leader "$child_pid"; then
        if stop_child_process_group "$child_pid" "$service_name"; then
          clear_child_group
        fi
      elif kill -0 "$child_pid" 2>/dev/null; then
        stop_single_child "$child_pid" "$service_name"
      fi
    fi
    if [ "$service_kind" = app ] &&
       ! cleanup_liveness_dir "$liveness_generation_dir"; then
      info "error: could not remove $service_name effective-UID-owned liveness state"
    fi
    exit 0
  }

  trap stop_supervised_child HUP INT TERM
  delay=$RESTART_MIN_DELAY_SECONDS
  while :; do
    info "Starting $service_name..."
    started_at=$(date +%s)
    child_pid=
    liveness_reader_pid=

    if [ "$service_kind" = app ]; then
      if ! prepare_liveness_generation; then
        info "error: could not create supervisor effective-UID-owned liveness FIFO state"
        kill -TERM "$root_supervisor_pid" 2>/dev/null || true
        exit 1
      fi
      awk \
        -v state_path="$LIVENESS_STATE" \
        -v state_tmp="$LIVENESS_STATE_TMP" \
        -f "$LIVENESS_READER_AWK" \
        "$LIVENESS_FIFO" &
      liveness_reader_pid=$!
      liveness_frame=
      liveness_startup_age=-1
      LIVENESS_WATCHDOG_FAILURE=
      LIVENESS_WATCHDOG_WORKER=-
      LIVENESS_WATCHDOG_PHASE=-
      LIVENESS_WATCHDOG_AGE=0
      liveness_t_value= liveness_r_value= liveness_e_value=
      liveness_m_value= liveness_p_value=
      liveness_t_age=0 liveness_r_age=0 liveness_e_age=0
      liveness_m_age=0 liveness_p_age=0
    fi

    child_launching=true
    set -m
    case "$service_kind" in
      bot-api) "$BOT_API_RUN" & ;;
      app)
        (
          TG_OBS_LIVENESS_FD3=1
          export TG_OBS_LIVENESS_FD3
          run_app_process
        ) 3>"$LIVENESS_FIFO" &
        ;;
      *) die "unknown supervised service kind: $service_kind" ;;
    esac
    child_pid=$!
    set +m
    child_launching=false
    if ! is_process_group_leader "$child_pid"; then
      kill -TERM "$child_pid" 2>/dev/null || true
      wait "$child_pid" 2>/dev/null || true
      info "error: could not isolate $service_name child process group"
      stop_liveness_reader_process
      [ "$service_kind" != app ] ||
        cleanup_liveness_dir "$liveness_generation_dir" || true
      kill -TERM "$root_supervisor_pid" 2>/dev/null || true
      exit 1
    fi
    if ! record_child_group "$child_pid"; then
      stop_liveness_reader_process
      [ "$service_kind" != app ] ||
        cleanup_liveness_dir "$liveness_generation_dir" || true
      exit 1
    fi

    status=0
    watchdog_failed=false
    if [ "$service_kind" = app ]; then
      reader_status=running
      while :; do
        if process_has_exited "$liveness_reader_pid"; then
          reader_status=0
          wait "$liveness_reader_pid" 2>/dev/null || reader_status=$?
          liveness_reader_pid=
          if [ "$reader_status" -eq 25 ]; then
            sleep "$LIVENESS_APP_EXIT_RACE_SECONDS"
            process_has_exited "$child_pid" && break
          fi
          case "$reader_status" in
            24) LIVENESS_WATCHDOG_FAILURE=state-write ;;
            *) LIVENESS_WATCHDOG_FAILURE=reader-exited ;;
          esac
          if [ -r "$LIVENESS_STATE" ]; then
            IFS= read -r liveness_state_line <"$LIVENESS_STATE" ||
              liveness_state_line=
            consume_liveness_state "$liveness_state_line" || true
          fi
          watchdog_failed=true
          break
        fi
        process_has_exited "$child_pid" && break
        if [ -r "$LIVENESS_STATE" ]; then
          IFS= read -r liveness_state_line <"$LIVENESS_STATE" ||
            liveness_state_line=
          if ! consume_liveness_state "$liveness_state_line"; then
            watchdog_failed=true
            break
          fi
        fi
        if ! tick_liveness_watchdog; then
          watchdog_failed=true
          break
        fi
        sleep "$LIVENESS_TICK_SECONDS"
      done
      if [ "$watchdog_failed" = true ]; then
        info "error: $service_name liveness failed reason=$LIVENESS_WATCHDOG_FAILURE worker=$LIVENESS_WATCHDOG_WORKER phase=$LIVENESS_WATCHDOG_PHASE age=${LIVENESS_WATCHDOG_AGE}s reader_status=$reader_status"
        stop_liveness_reader_process
        if ! stop_child_process_group "$child_pid" "$service_name liveness failure"; then
          info "error: could not drain $service_name after liveness watchdog failure"
          cleanup_liveness_dir "$liveness_generation_dir" || true
          kill -TERM "$root_supervisor_pid" 2>/dev/null || true
          exit 1
        fi
        status=1
      else
        wait "$child_pid" 2>/dev/null || status=$?
        stop_liveness_reader_process
      fi
    else
      wait "$child_pid" || status=$?
    fi

    exited_child_pid=$child_pid
    if ! drain_process_group "$exited_child_pid" "$service_name"; then
      info "error: could not drain $service_name process group $exited_child_pid"
      [ "$service_kind" != app ] ||
        cleanup_liveness_dir "$liveness_generation_dir" || true
      kill -TERM "$root_supervisor_pid" 2>/dev/null || true
      exit 1
    fi
    clear_child_group
    if [ "$service_kind" = app ] &&
       ! cleanup_liveness_dir "$liveness_generation_dir"; then
      info "error: could not remove $service_name effective-UID-owned liveness state"
      kill -TERM "$root_supervisor_pid" 2>/dev/null || true
      exit 1
    fi
    child_pid=
    if [ "$service_kind" = app ] && [ "$status" -eq "$APP_INSTANCE_CONTENTION_EXIT_CODE" ]; then
      info "$service_name refused startup because its database or Telegram bot is already owned; not restarting."
      return "$status"
    fi
    if [ "$watchdog_failed" = true ]; then
      info "$service_name generation failed its liveness contract."
    fi
    ended_at=$(date +%s)
    runtime=$((ended_at - started_at))
    if [ "$runtime" -ge "$RESTART_RESET_AFTER_SECONDS" ]; then
      if [ "$delay" -ne "$RESTART_MIN_DELAY_SECONDS" ]; then
        info "$service_name ran for ${runtime}s; resetting restart delay."
      fi
      delay=$RESTART_MIN_DELAY_SECONDS
    fi
    info "$service_name exited with status $status; restarting in ${delay}s..."
    child_launching=true
    set -m
    sleep "$delay" &
    child_pid=$!
    set +m
    child_launching=false
    if ! is_process_group_leader "$child_pid"; then
      kill -TERM "$child_pid" 2>/dev/null || true
      wait "$child_pid" 2>/dev/null || true
      info "error: could not isolate $service_name restart-delay process group"
      kill -TERM "$root_supervisor_pid" 2>/dev/null || true
      exit 1
    fi
    record_child_group "$child_pid"
    wait "$child_pid" 2>/dev/null || true
    if ! drain_process_group "$child_pid" "$service_name restart delay"; then
      info "error: could not drain $service_name restart-delay process group $child_pid"
      kill -TERM "$root_supervisor_pid" 2>/dev/null || true
      exit 1
    fi
    clear_child_group
    child_pid=
    delay=$(delay_next "$delay")
  done
}

run_up() {
  load_env
  unset TG_OBS_LIVENESS_FD3
  require_stack_env
  ensure_go_cache

  : "${RESTART_MIN_DELAY_SECONDS:=2}"
  : "${RESTART_MAX_DELAY_SECONDS:=60}"
  : "${RESTART_RESET_AFTER_SECONDS:=300}"
  : "${SHUTDOWN_GRACE_SECONDS:=15}"
  if ! is_positive_integer "$RESTART_MIN_DELAY_SECONDS"; then
    die "RESTART_MIN_DELAY_SECONDS must be a positive integer"
  fi
  if ! is_positive_integer "$RESTART_MAX_DELAY_SECONDS"; then
    die "RESTART_MAX_DELAY_SECONDS must be a positive integer"
  fi
  if ! is_positive_integer "$RESTART_RESET_AFTER_SECONDS"; then
    die "RESTART_RESET_AFTER_SECONDS must be a positive integer"
  fi
  if ! is_positive_integer "$SHUTDOWN_GRACE_SECONDS"; then
    die "SHUTDOWN_GRACE_SECONDS must be a positive integer"
  fi
  if [ "$RESTART_MIN_DELAY_SECONDS" -gt "$MAX_RESTART_DELAY_SECONDS" ]; then
    die "RESTART_MIN_DELAY_SECONDS must be no greater than $MAX_RESTART_DELAY_SECONDS"
  fi
  if [ "$RESTART_MAX_DELAY_SECONDS" -gt "$MAX_RESTART_DELAY_SECONDS" ]; then
    die "RESTART_MAX_DELAY_SECONDS must be no greater than $MAX_RESTART_DELAY_SECONDS"
  fi
  if [ "$RESTART_RESET_AFTER_SECONDS" -gt "$MAX_RESTART_DELAY_SECONDS" ]; then
    die "RESTART_RESET_AFTER_SECONDS must be no greater than $MAX_RESTART_DELAY_SECONDS"
  fi
  if [ "$SHUTDOWN_GRACE_SECONDS" -gt "$MAX_RESTART_DELAY_SECONDS" ]; then
    die "SHUTDOWN_GRACE_SECONDS must be no greater than $MAX_RESTART_DELAY_SECONDS"
  fi
  if [ "$RESTART_MAX_DELAY_SECONDS" -lt "$RESTART_MIN_DELAY_SECONDS" ]; then
    die "RESTART_MAX_DELAY_SECONDS must be greater than or equal to RESTART_MIN_DELAY_SECONDS"
  fi
  root_supervisor_pid=$$
  prepare_app_binary

  bot_api_supervisor_pid=""
  app_supervisor_pid=""
  shutdown_status=
  supervisor_launching=
  candidate_pid=
  supervisor_state_dir=
  bot_child_state_file=
  app_child_state_file=
  app_liveness_dir=

  cleanup() {
    status=${shutdown_status:-$?}
    trap - 0 HUP INT TERM

    if [ -n "$supervisor_launching" ] && [ -z "$candidate_pid" ]; then
      capture_last_background_pid
      pending_pid=$LAST_BACKGROUND_PID
      if [ "$pending_pid" != "$bot_api_supervisor_pid" ] && [ "$pending_pid" != "$app_supervisor_pid" ]; then
        candidate_pid=$pending_pid
      fi
    fi
    if [ -n "$supervisor_launching" ] && [ -n "$candidate_pid" ]; then
      if is_process_group_leader "$candidate_pid"; then
        case "$supervisor_launching" in
          bot-api) bot_api_supervisor_pid=$candidate_pid ;;
          app) app_supervisor_pid=$candidate_pid ;;
        esac
      elif kill -0 "$candidate_pid" 2>/dev/null; then
        stop_single_child "$candidate_pid" "service supervisor"
      fi
    fi

    bot_child_group=$(read_process_group_state "$bot_child_state_file")
    app_child_group=$(read_process_group_state "$app_child_state_file")

    for supervisor_pid in "$app_supervisor_pid" "$bot_api_supervisor_pid"; do
      if [ -n "$supervisor_pid" ] && process_group_alive "$supervisor_pid"; then
        signal_process_group TERM "$supervisor_pid"
      fi
    done
    for child_group in "$app_child_group" "$bot_child_group"; do
      if [ -n "$child_group" ] && process_group_alive "$child_group"; then
        signal_process_group TERM "$child_group"
      fi
    done

    root_shutdown_limit=$((SHUTDOWN_GRACE_SECONDS + ROOT_SHUTDOWN_BUFFER_SECONDS))
    elapsed=0
    while [ "$elapsed" -lt "$root_shutdown_limit" ]; do
      groups_alive=false
      for supervisor_pid in "$app_supervisor_pid" "$bot_api_supervisor_pid"; do
        if [ -n "$supervisor_pid" ] && process_group_alive "$supervisor_pid"; then
          groups_alive=true
        fi
      done
      for child_group in "$app_child_group" "$bot_child_group"; do
        if [ -n "$child_group" ] && process_group_alive "$child_group"; then
          groups_alive=true
        fi
      done
      [ "$groups_alive" = false ] && break
      sleep 1
      elapsed=$((elapsed + 1))
    done

    for supervisor_pid in "$app_supervisor_pid" "$bot_api_supervisor_pid"; do
      if [ -n "$supervisor_pid" ] && process_group_alive "$supervisor_pid"; then
        info "Service supervisor process group $supervisor_pid did not stop after child shutdown grace; sending KILL."
        signal_process_group KILL "$supervisor_pid"
      fi
    done
    for child_group in "$app_child_group" "$bot_child_group"; do
      if [ -n "$child_group" ] && process_group_alive "$child_group"; then
        info "Service child process group $child_group did not stop after shutdown grace; sending KILL."
        signal_process_group KILL "$child_group"
      fi
    done
    for supervisor_pid in "$app_supervisor_pid" "$bot_api_supervisor_pid"; do
      if [ -n "$supervisor_pid" ]; then
        wait "$supervisor_pid" 2>/dev/null || true
      fi
    done
    if [ -n "$supervisor_state_dir" ]; then
      if [ -n "$app_liveness_dir" ] &&
         ! cleanup_liveness_dir "$app_liveness_dir"; then
        info "error: could not remove supervisor effective-UID-owned liveness state"
      fi
      rm -f \
        "$bot_child_state_file" "${bot_child_state_file}.tmp" \
        "$app_child_state_file" "${app_child_state_file}.tmp"
      rmdir "$supervisor_state_dir" 2>/dev/null || true
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

  trap 'shutdown_status=129; cleanup' HUP
  trap 'shutdown_status=130; cleanup' INT
  trap 'shutdown_status=143; cleanup' TERM
  trap 'shutdown_status=$?; cleanup' 0

  command -v awk >/dev/null 2>&1 ||
    die "awk is required for app liveness supervision"
  [ -r "$LIVENESS_READER_AWK" ] ||
    die "app liveness reader is missing or unreadable: $LIVENESS_READER_AWK"
  case "$(uname -s)" in
    Darwin) liveness_runtime_base=/private/tmp ;;
    Linux) liveness_runtime_base=/tmp ;;
    *) die "liveness supervision is supported only on macOS and Linux" ;;
  esac
  supervisor_state_dir=$(
    umask 077
    mktemp -d "$liveness_runtime_base/tg-obs-supervisor-state.XXXXXX"
  ) ||
    die "could not create supervisor effective-UID-owned state directory"
  chmod 700 "$supervisor_state_dir" ||
    die "could not secure supervisor state directory"
  bot_child_state_file="$supervisor_state_dir/bot-api.group"
  app_child_state_file="$supervisor_state_dir/app.group"
  app_liveness_dir="$supervisor_state_dir/app-liveness"

  supervisor_launching=bot-api
  candidate_pid=
  set -m
  supervise_service "Telegram Local Bot API Server" bot-api "$bot_child_state_file" &
  candidate_pid=$!
  set +m
  if ! is_process_group_leader "$candidate_pid"; then
    kill "$candidate_pid" 2>/dev/null || true
    wait "$candidate_pid" 2>/dev/null || true
    die "could not isolate Telegram Local Bot API Server supervisor process group"
  fi
  bot_api_supervisor_pid=$candidate_pid
  supervisor_launching=
  candidate_pid=

  if ! wait_for_health 30; then
    info "Telegram Local Bot API Server is not healthy yet; tg-obs-bot will still start and retry Telegram polling."
  fi

  supervisor_launching=app
  candidate_pid=
  set -m
  supervise_service "tg-obs-bot" app "$app_child_state_file" &
  candidate_pid=$!
  set +m
  if ! is_process_group_leader "$candidate_pid"; then
    kill "$candidate_pid" 2>/dev/null || true
    wait "$candidate_pid" 2>/dev/null || true
    die "could not isolate tg-obs-bot supervisor process group"
  fi
  app_supervisor_pid=$candidate_pid
  supervisor_launching=
  candidate_pid=

  while :; do
    if process_has_exited "$bot_api_supervisor_pid"; then
      info "Telegram Local Bot API Server supervisor exited unexpectedly; shutting down the stack."
      shutdown_status=1
      cleanup
    fi
    if process_has_exited "$app_supervisor_pid"; then
      app_supervisor_status=0
      wait "$app_supervisor_pid" 2>/dev/null || app_supervisor_status=$?
      if [ "$app_supervisor_status" -eq "$APP_INSTANCE_CONTENTION_EXIT_CODE" ]; then
        info "tg-obs-bot singleton contention is not retryable; shutting down the stack with status $app_supervisor_status."
        shutdown_status=$app_supervisor_status
      else
        info "tg-obs-bot supervisor exited unexpectedly; shutting down the stack."
        shutdown_status=1
      fi
      cleanup
    fi
    sleep 1
  done
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
    "MAX_VIDEO_SIZE_MB:2000:1:8796093022207" \
    "MAX_VIDEO_DURATION_SECONDS:7200:0:2147483647" \
    "MIN_FREE_DISK_MB:512:0:8796093022207" \
    "MAX_QUEUE_LENGTH:50:1:2147483647" \
    "RETENTION_DAYS:7:0:106751" \
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
    if (umask 077 && mkdir -p "$dir" && chmod 700 "$dir") 2>/dev/null; then
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
  printf 'MIN_FREE_DISK_MB=%s\n' "${MIN_FREE_DISK_MB:-512}"
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
