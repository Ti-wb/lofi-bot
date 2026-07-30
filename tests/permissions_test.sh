#!/bin/sh
set -eu

REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
TEST_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/tg-obs-permissions.XXXXXX")
SECRET_MARKER=permission-test-secret-do-not-log

cleanup() {
  rm -rf "$TEST_ROOT"
}
trap cleanup 0 HUP INT TERM

fail() {
  printf 'not ok - %s\n' "$1" >&2
  if [ -n "${PERMISSION_TRACE:-}" ] && [ -f "$PERMISSION_TRACE" ]; then
    sed 's/^/trace: /' "$PERMISSION_TRACE" >&2
  fi
  exit 1
}

file_mode() {
  if stat -f '%Lp' "$1" >/dev/null 2>&1; then
    stat -f '%Lp' "$1"
  else
    stat -c '%a' "$1"
  fi
}

assert_mode() {
  path=$1
  want=$2
  got=$(file_mode "$path")
  [ "$got" = "$want" ] || fail "$path mode is $got, want $want"
}

assert_no_secret() {
  output=$1
  if grep -Fq "$SECRET_MARKER" "$output"; then
    fail "$output contains the secret marker"
  fi
}

new_fixture() {
  name=$1
  FIXTURE="$TEST_ROOT/$name"
  mkdir -p "$FIXTURE/bin" "$FIXTURE/deploy/telegram-bot-api"
  cp "$REPO_ROOT/run.sh" "$FIXTURE/run.sh"
  cp "$REPO_ROOT/deploy/telegram-bot-api/run.sh" "$FIXTURE/deploy/telegram-bot-api/run.sh"
  cp "$REPO_ROOT/deploy/telegram-bot-api/healthcheck.sh" "$FIXTURE/deploy/telegram-bot-api/healthcheck.sh"
  cp "$REPO_ROOT/deploy/telegram-bot-api/logout-public.sh" "$FIXTURE/deploy/telegram-bot-api/logout-public.sh"
  chmod +x "$FIXTURE/run.sh" "$FIXTURE/deploy/telegram-bot-api/"*.sh

  cat >"$FIXTURE/.env" <<'EOF'
ENV_SCHEMA_VERSION=5
TELEGRAM_BOT_TOKEN=123456789:permission-test-secret-do-not-log
TELEGRAM_API_BASE_URL=http://127.0.0.1:8081
TELEGRAM_API_ID=1
TELEGRAM_API_HASH=permission-test-secret-do-not-log
TELEGRAM_BOT_API_BIN=./bin/fake-bot-api
TELEGRAM_BOT_API_HOST=127.0.0.1
TELEGRAM_BOT_API_PORT=8081
TELEGRAM_BOT_API_DIR=./data/telegram-bot-api
ALLOWED_CHAT_ID=1
OBS_PASSWORD=permission-test-secret-do-not-log
OBS_MEDIA_SOURCE_NAME=Queue
OBS_LOOP_SOURCE_NAME=Loop
OBS_MUSIC_SOURCE_NAME=Music
FFPROBE_PATH=./bin/fake-ffprobe
EOF

  cat >"$FIXTURE/bin/curl" <<'EOF'
#!/bin/sh
set -eu
cat >/dev/null
printf '{"ok":true}\n'
EOF
  cat >"$FIXTURE/bin/fake-bot-api" <<'EOF'
#!/bin/sh
set -eu
bot_api_dir=
for argument do
  case "$argument" in
    --dir=*) bot_api_dir=${argument#--dir=} ;;
  esac
done
[ -n "$bot_api_dir" ] || exit 1
mkdir -p "$bot_api_dir/daemon-child"
: >"$bot_api_dir/daemon-file"
EOF
  cat >"$FIXTURE/bin/fake-ffprobe" <<'EOF'
#!/bin/sh
exit 0
EOF
  chmod +x \
    "$FIXTURE/bin/curl" \
    "$FIXTURE/bin/fake-bot-api" \
    "$FIXTURE/bin/fake-ffprobe"
}

run_fixture() {
  fixture=$1
  output=$2
  shift 2
  (
    umask 000
    cd "$fixture"
    PATH="$fixture/bin:$PATH" "$@"
  ) >"$output" 2>&1
}

new_fixture current
current_fixture=$FIXTURE
chmod 644 "$current_fixture/.env"
run_fixture "$current_fixture" "$current_fixture/root-env.log" ./run.sh env
assert_mode "$current_fixture/.env" 600
assert_no_secret "$current_fixture/root-env.log"
[ -z "$(find "$current_fixture" -maxdepth 1 -name '.env.backup.*' -print)" ] ||
  fail "current-schema root read created a migration backup"

for helper in healthcheck.sh logout-public.sh run.sh; do
  chmod 644 "$current_fixture/.env"
  output="$current_fixture/$helper.log"
  run_fixture \
    "$current_fixture" \
    "$output" \
    "./deploy/telegram-bot-api/$helper"
  assert_mode "$current_fixture/.env" 600
  assert_no_secret "$output"
done
printf 'ok - every shell .env source secures current-schema config without logging secrets\n'

bot_api_dir="$current_fixture/data/telegram-bot-api"
assert_mode "$bot_api_dir" 700
assert_mode "$bot_api_dir/daemon-child" 700
assert_mode "$bot_api_dir/daemon-file" 600
chmod 777 "$bot_api_dir"
run_fixture \
  "$current_fixture" \
  "$current_fixture/resecure-bot-api.log" \
  ./deploy/telegram-bot-api/run.sh
assert_mode "$bot_api_dir" 700
assert_no_secret "$current_fixture/resecure-bot-api.log"
printf 'ok - bot-api cache and daemon files stay private under umask 000\n'

new_fixture doctor
doctor_fixture=$FIXTURE
doctor_bot_api_dir="$doctor_fixture/data/telegram-bot-api"
run_fixture "$doctor_fixture" "$doctor_fixture/doctor-new.log" ./run.sh doctor
assert_mode "$doctor_bot_api_dir" 700
assert_no_secret "$doctor_fixture/doctor-new.log"
chmod 777 "$doctor_bot_api_dir"
run_fixture "$doctor_fixture" "$doctor_fixture/doctor-existing.log" ./run.sh doctor
assert_mode "$doctor_bot_api_dir" 700
assert_no_secret "$doctor_fixture/doctor-existing.log"
printf 'ok - doctor creates and resecures the bot-api cache under umask 000\n'

new_fixture migration
migration_fixture=$FIXTURE
awk '
  /^ENV_SCHEMA_VERSION=/ { print "ENV_SCHEMA_VERSION=4"; next }
  { print }
' "$migration_fixture/.env" >"$migration_fixture/.env.old"
mv "$migration_fixture/.env.old" "$migration_fixture/.env"
chmod 644 "$migration_fixture/.env"

REAL_CP=$(command -v cp)
REAL_AWK=$(command -v awk)
export REAL_CP REAL_AWK
export PERMISSION_TRACE="$migration_fixture/mode.trace"
export PERMISSION_FIXTURE_ROOT="$migration_fixture"

cat >"$migration_fixture/bin/cp" <<'EOF'
#!/bin/sh
set -eu
"$REAL_CP" "$@"
destination=
for argument do
  destination=$argument
done
case "${destination##*/}" in
  .env.backup.*)
    if stat -f '%Lp' "$destination" >/dev/null 2>&1; then
      mode=$(stat -f '%Lp' "$destination")
    else
      mode=$(stat -c '%a' "$destination")
    fi
    printf 'backup %s\n' "$mode" >>"$PERMISSION_TRACE"
    ;;
esac
EOF

cat >"$migration_fixture/bin/awk" <<'EOF'
#!/bin/sh
set -eu
for path in "$PERMISSION_FIXTURE_ROOT"/.env.tmp.*; do
  [ -f "$path" ] || continue
  if [ ! -e "$PERMISSION_FIXTURE_ROOT/.temp-mode-recorded" ]; then
    if stat -f '%Lp' "$path" >/dev/null 2>&1; then
      mode=$(stat -f '%Lp' "$path")
    else
      mode=$(stat -c '%a' "$path")
    fi
    printf 'temp %s\n' "$mode" >>"$PERMISSION_TRACE"
    : >"$PERMISSION_FIXTURE_ROOT/.temp-mode-recorded"
  fi
done
exec "$REAL_AWK" "$@"
EOF
chmod +x "$migration_fixture/bin/cp" "$migration_fixture/bin/awk"

run_fixture "$migration_fixture" "$migration_fixture/migrate.log" ./run.sh migrate-env
assert_mode "$migration_fixture/.env" 600
assert_no_secret "$migration_fixture/migrate.log"
backup=$(find "$migration_fixture" -maxdepth 1 -name '.env.backup.*' -type f -print)
[ -n "$backup" ] || fail "migration backup is missing"
assert_mode "$backup" 600
grep -qx 'backup 600' "$migration_fixture/mode.trace" ||
  fail "migration backup was not private when cp completed"
grep -qx 'temp 600' "$migration_fixture/mode.trace" ||
  fail "migration temp was not private when its writer started"
[ -z "$(find "$migration_fixture" -maxdepth 1 -name '.env.tmp.*' -print)" ] ||
  fail "migration temp survived successful replacement"
printf 'ok - migration backup and temp are private from creation under umask 000\n'

printf 'all permission tests passed\n'
