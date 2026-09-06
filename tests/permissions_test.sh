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
ENV_SCHEMA_VERSION=7
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
grep -Fqx 'DATABASE_PATH=./data/state.db' "$current_fixture/root-env.log" ||
  fail "fresh schema-v7 config did not use the state.db default"
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

new_fixture current_obsolete
current_obsolete_fixture=$FIXTURE
cat >>"$current_obsolete_fixture/.env" <<'EOF'
PLAYER_MODE=library
OBS_MEDIA_SOURCE_NAME=LegacyQueueSource
OBS_FALLBACK_FILE=/legacy/fallback.mp4
FALLBACK_MODE=file
MAX_QUEUE_LENGTH=99
RETENTION_DAYS=30
RETENTION_MAX_FILES=100
RETENTION_DELETE_LOCAL_FILES=true
EOF
run_fixture \
  "$current_obsolete_fixture" \
  "$current_obsolete_fixture/migrate.log" \
  ./run.sh migrate-env
if grep -Eq '^(OBS_MEDIA_SOURCE_NAME|OBS_FALLBACK_FILE|FALLBACK_MODE|PLAYER_MODE|MAX_QUEUE_LENGTH|RETENTION_[^=]*)=' "$current_obsolete_fixture/.env"; then
  fail "current-schema migration retained an obsolete queue setting"
fi
obsolete_backup_count=$(find "$current_obsolete_fixture" -maxdepth 1 -name '.env.backup.*' -type f -print | wc -l | tr -d ' ')
[ "$obsolete_backup_count" = 1 ] ||
  fail "obsolete-key cleanup created $obsolete_backup_count backups, want 1"
run_fixture \
  "$current_obsolete_fixture" \
  "$current_obsolete_fixture/migrate-again.log" \
  ./run.sh migrate-env
idempotent_backup_count=$(find "$current_obsolete_fixture" -maxdepth 1 -name '.env.backup.*' -type f -print | wc -l | tr -d ' ')
[ "$idempotent_backup_count" = "$obsolete_backup_count" ] ||
  fail "idempotent obsolete-key cleanup created another backup"
printf 'ok - current-schema migration removes queue-only settings idempotently\n'

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

new_fixture doctor_explicit_library_dirs
doctor_explicit_fixture=$FIXTURE
cat >>"$doctor_explicit_fixture/.env" <<'EOF'
DATA_DIR=/dev/null/unused-data-base
MEDIA_DIR=/dev/null/unused-library-base
DATABASE_PATH=./explicit-runtime/library.db
LOOP_MEDIA_DIR=./explicit-loop-library
MUSIC_MEDIA_DIR=./explicit-music-library
EOF
run_fixture \
  "$doctor_explicit_fixture" \
  "$doctor_explicit_fixture/doctor.log" \
  ./run.sh doctor
[ -d "$doctor_explicit_fixture/explicit-loop-library" ] ||
  fail "doctor did not create the explicit loop library"
[ -d "$doctor_explicit_fixture/explicit-music-library" ] ||
  fail "doctor did not create the explicit music library"
[ ! -e /dev/null/unused-data-base ] ||
  fail "doctor touched the unused DATA_DIR base"
[ ! -e /dev/null/unused-library-base ] ||
  fail "doctor touched the unused MEDIA_DIR base"
printf 'ok - explicit runtime paths do not depend on unused DATA_DIR or MEDIA_DIR writability\n'

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
grep -Fqx 'ENV_SCHEMA_VERSION=7' "$migration_fixture/.env" ||
  fail "legacy config did not migrate to schema v7"
grep -Fqx "DATABASE_PATH='./data/queue.db'" "$migration_fixture/.env" ||
  fail "legacy config without DATABASE_PATH was not pinned to queue.db"
backup=$(find "$migration_fixture" -maxdepth 1 -name '.env.backup.*' -type f -print)
[ -n "$backup" ] || fail "migration backup is missing"
assert_mode "$backup" 600
grep -qx 'backup 600' "$migration_fixture/mode.trace" ||
  fail "migration backup was not private when cp completed"
grep -qx 'temp 600' "$migration_fixture/mode.trace" ||
  fail "migration temp was not private when its writer started"
[ -z "$(find "$migration_fixture" -maxdepth 1 -name '.env.tmp.*' -print)" ] ||
  fail "migration temp survived successful replacement"
run_fixture \
  "$migration_fixture" \
  "$migration_fixture/migrate-again.log" \
  ./run.sh migrate-env
migration_backup_count=$(find "$migration_fixture" -maxdepth 1 -name '.env.backup.*' -type f -print | wc -l | tr -d ' ')
[ "$migration_backup_count" = 1 ] ||
  fail "idempotent schema-v7 migration created another backup"
printf 'ok - migration backup and temp are private from creation under umask 000\n'

new_fixture legacy_queue
legacy_queue_fixture=$FIXTURE
awk '
  /^ENV_SCHEMA_VERSION=/ { print "ENV_SCHEMA_VERSION=5"; next }
  { print }
' "$legacy_queue_fixture/.env" >"$legacy_queue_fixture/.env.old"
mv "$legacy_queue_fixture/.env.old" "$legacy_queue_fixture/.env"
printf '%s\n' 'PLAYER_MODE=queue' >>"$legacy_queue_fixture/.env"
chmod 644 "$legacy_queue_fixture/.env"
cp "$legacy_queue_fixture/.env" "$legacy_queue_fixture/.env.before"
if run_fixture "$legacy_queue_fixture" "$legacy_queue_fixture/migrate.log" ./run.sh migrate-env; then
  fail "legacy queue config migration unexpectedly succeeded"
fi
cmp "$legacy_queue_fixture/.env.before" "$legacy_queue_fixture/.env" >/dev/null ||
  fail "rejected legacy queue migration changed .env"
assert_mode "$legacy_queue_fixture/.env" 600
[ -z "$(find "$legacy_queue_fixture" -maxdepth 1 -name '.env.backup.*' -print)" ] ||
  fail "rejected legacy queue migration created a backup"
grep -Fq 'PLAYER_MODE=queue is no longer supported' "$legacy_queue_fixture/migrate.log" ||
  fail "legacy queue migration did not provide an actionable error"
if grep -Eq '^DATABASE_PATH=' "$legacy_queue_fixture/.env"; then
  fail "rejected legacy queue migration added a database path"
fi
printf 'ok - legacy queue mode fails before migration changes .env\n'

sed '/^PLAYER_MODE=/d' "$legacy_queue_fixture/.env.before" >"$legacy_queue_fixture/.env"
cp "$legacy_queue_fixture/.env" "$legacy_queue_fixture/.env.before"
if run_fixture "$legacy_queue_fixture" "$legacy_queue_fixture/inherited-queue.log" env PLAYER_MODE=queue ./run.sh env; then
  fail "inherited queue mode unexpectedly succeeded"
fi
cmp "$legacy_queue_fixture/.env.before" "$legacy_queue_fixture/.env" >/dev/null ||
  fail "rejected inherited queue mode changed .env"
[ -z "$(find "$legacy_queue_fixture" -maxdepth 1 -name '.env.backup.*' -print)" ] ||
  fail "rejected inherited queue mode created a backup"
grep -Fq 'PLAYER_MODE=queue is no longer supported' "$legacy_queue_fixture/inherited-queue.log" ||
  fail "inherited queue mode did not provide an actionable error"
printf 'ok - inherited queue mode fails before migration changes .env\n'

new_fixture spaced_paths
spaced_paths_fixture=$FIXTURE
awk '
  /^ENV_SCHEMA_VERSION=/ { print "ENV_SCHEMA_VERSION=3"; next }
  { print }
' "$spaced_paths_fixture/.env" >"$spaced_paths_fixture/.env.old"
mv "$spaced_paths_fixture/.env.old" "$spaced_paths_fixture/.env"
cat >>"$spaced_paths_fixture/.env" <<'EOF'
MEDIA_DIR='./data/Lo Fi'
PLAYER_MODE=library
OBS_MEDIA_SOURCE_NAME=LegacyQueueSource
OBS_FALLBACK_FILE=/legacy/fallback.mp4
FALLBACK_MODE=file
MAX_QUEUE_LENGTH=99
RETENTION_DAYS=30
RETENTION_MAX_FILES=100
RETENTION_DELETE_LOCAL_FILES=true
EOF
run_fixture "$spaced_paths_fixture" "$spaced_paths_fixture/migrate.log" ./run.sh migrate-env
grep -Fqx "DATABASE_PATH='./data/queue.db'" "$spaced_paths_fixture/.env" ||
  fail "MEDIA_DIR incorrectly changed the legacy database default"
grep -Fqx "LOOP_MEDIA_DIR='./data/Lo Fi/loops'" "$spaced_paths_fixture/.env" ||
  fail "derived loop path with spaces was not shell-quoted"
grep -Fqx "MUSIC_MEDIA_DIR='./data/Lo Fi/music'" "$spaced_paths_fixture/.env" ||
  fail "derived music path with spaces was not shell-quoted"
if grep -Eq '^(OBS_MEDIA_SOURCE_NAME|OBS_FALLBACK_FILE|FALLBACK_MODE|PLAYER_MODE|MAX_QUEUE_LENGTH|RETENTION_[^=]*)=' "$spaced_paths_fixture/.env"; then
  fail "library-only migration added an obsolete queue setting"
fi
run_fixture "$spaced_paths_fixture" "$spaced_paths_fixture/env.log" ./run.sh env
grep -Fqx 'LOOP_MEDIA_DIR=./data/Lo Fi/loops' "$spaced_paths_fixture/env.log" ||
  fail "quoted loop path did not survive .env sourcing"
grep -Fqx 'MUSIC_MEDIA_DIR=./data/Lo Fi/music' "$spaced_paths_fixture/env.log" ||
  fail "quoted music path did not survive .env sourcing"
printf 'ok - migration quotes derived library paths and adds no queue settings\n'

new_fixture legacy_database_custom_data
legacy_database_custom_data_fixture=$FIXTURE
awk '
  /^ENV_SCHEMA_VERSION=/ { print "ENV_SCHEMA_VERSION=6"; next }
  { print }
' "$legacy_database_custom_data_fixture/.env" >"$legacy_database_custom_data_fixture/.env.old"
mv "$legacy_database_custom_data_fixture/.env.old" "$legacy_database_custom_data_fixture/.env"
cat >>"$legacy_database_custom_data_fixture/.env" <<'EOF'
DATA_DIR='./Legacy O'\''Brien'
EOF
run_fixture \
  "$legacy_database_custom_data_fixture" \
  "$legacy_database_custom_data_fixture/migrate.log" \
  ./run.sh migrate-env
[ "$(grep -Ec '^DATABASE_PATH=' "$legacy_database_custom_data_fixture/.env")" -eq 1 ] ||
  fail "legacy custom DATA_DIR migration did not produce exactly one DATABASE_PATH"
run_fixture \
  "$legacy_database_custom_data_fixture" \
  "$legacy_database_custom_data_fixture/env.log" \
  ./run.sh env
grep -Fqx "DATABASE_PATH=./Legacy O'Brien/queue.db" "$legacy_database_custom_data_fixture/env.log" ||
  fail "legacy custom DATA_DIR was not pinned to its queue.db path"
legacy_custom_backup_count=$(find "$legacy_database_custom_data_fixture" -maxdepth 1 -name '.env.backup.*' -type f -print | wc -l | tr -d ' ')
[ "$legacy_custom_backup_count" = 1 ] ||
  fail "legacy custom DATA_DIR migration created $legacy_custom_backup_count backups, want 1"
printf 'ok - legacy custom DATA_DIR is safely quoted and pinned to queue.db\n'

new_fixture legacy_database_empty
legacy_database_empty_fixture=$FIXTURE
awk '
  /^ENV_SCHEMA_VERSION=/ { print "ENV_SCHEMA_VERSION=6"; next }
  { print }
' "$legacy_database_empty_fixture/.env" >"$legacy_database_empty_fixture/.env.old"
mv "$legacy_database_empty_fixture/.env.old" "$legacy_database_empty_fixture/.env"
printf '%s\n' "DATABASE_PATH=''" >>"$legacy_database_empty_fixture/.env"
run_fixture \
  "$legacy_database_empty_fixture" \
  "$legacy_database_empty_fixture/migrate.log" \
  ./run.sh migrate-env
[ "$(grep -Ec '^DATABASE_PATH=' "$legacy_database_empty_fixture/.env")" -eq 1 ] ||
  fail "empty legacy DATABASE_PATH was not canonicalized"
grep -Fqx "DATABASE_PATH='./data/queue.db'" "$legacy_database_empty_fixture/.env" ||
  fail "empty legacy DATABASE_PATH was not pinned to queue.db"
run_fixture \
  "$legacy_database_empty_fixture" \
  "$legacy_database_empty_fixture/env.log" \
  ./run.sh env
grep -Fqx 'DATABASE_PATH=./data/queue.db' "$legacy_database_empty_fixture/env.log" ||
  fail "canonical legacy DATABASE_PATH did not survive sourcing"
printf 'ok - empty legacy DATABASE_PATH is canonicalized to queue.db\n'

new_fixture legacy_database_explicit
legacy_database_explicit_fixture=$FIXTURE
awk '
  /^ENV_SCHEMA_VERSION=/ { print "ENV_SCHEMA_VERSION=6"; next }
  { print }
' "$legacy_database_explicit_fixture/.env" >"$legacy_database_explicit_fixture/.env.old"
mv "$legacy_database_explicit_fixture/.env.old" "$legacy_database_explicit_fixture/.env"
cat >>"$legacy_database_explicit_fixture/.env" <<'EOF'
DATA_DIR='./ignored legacy data'
DATABASE_PATH='./runtime/Existing State.db'
EOF
run_fixture \
  "$legacy_database_explicit_fixture" \
  "$legacy_database_explicit_fixture/migrate.log" \
  ./run.sh migrate-env
[ "$(grep -Ec '^DATABASE_PATH=' "$legacy_database_explicit_fixture/.env")" -eq 1 ] ||
  fail "explicit legacy DATABASE_PATH was duplicated"
grep -Fqx "DATABASE_PATH='./runtime/Existing State.db'" "$legacy_database_explicit_fixture/.env" ||
  fail "explicit legacy DATABASE_PATH changed during migration"
run_fixture \
  "$legacy_database_explicit_fixture" \
  "$legacy_database_explicit_fixture/env.log" \
  ./run.sh env
grep -Fqx 'DATABASE_PATH=./runtime/Existing State.db' "$legacy_database_explicit_fixture/env.log" ||
  fail "explicit legacy DATABASE_PATH did not survive sourcing"
printf 'ok - explicit legacy DATABASE_PATH is preserved exactly\n'

new_fixture legacy_database_inherited_data
legacy_database_inherited_data_fixture=$FIXTURE
awk '
  /^ENV_SCHEMA_VERSION=/ { print "ENV_SCHEMA_VERSION=6"; next }
  { print }
' "$legacy_database_inherited_data_fixture/.env" >"$legacy_database_inherited_data_fixture/.env.old"
mv "$legacy_database_inherited_data_fixture/.env.old" "$legacy_database_inherited_data_fixture/.env"
run_fixture \
  "$legacy_database_inherited_data_fixture" \
  "$legacy_database_inherited_data_fixture/migrate.log" \
  env DATA_DIR='./Inherited State' ./run.sh migrate-env
grep -Fqx "DATABASE_PATH='./Inherited State/queue.db'" "$legacy_database_inherited_data_fixture/.env" ||
  fail "inherited legacy DATA_DIR was not pinned to queue.db"
run_fixture \
  "$legacy_database_inherited_data_fixture" \
  "$legacy_database_inherited_data_fixture/env.log" \
  ./run.sh env
grep -Fqx 'DATABASE_PATH=./Inherited State/queue.db' "$legacy_database_inherited_data_fixture/env.log" ||
  fail "pinned inherited DATA_DIR did not survive without the inherited variable"
printf 'ok - inherited legacy DATA_DIR is materialized before schema v7\n'

new_fixture legacy_database_missing_schema
legacy_database_missing_schema_fixture=$FIXTURE
awk '
  $0 !~ /^ENV_SCHEMA_VERSION=/ { print }
' "$legacy_database_missing_schema_fixture/.env" >"$legacy_database_missing_schema_fixture/.env.old"
mv "$legacy_database_missing_schema_fixture/.env.old" "$legacy_database_missing_schema_fixture/.env"
run_fixture \
  "$legacy_database_missing_schema_fixture" \
  "$legacy_database_missing_schema_fixture/migrate.log" \
  ./run.sh migrate-env
grep -Fqx 'ENV_SCHEMA_VERSION=7' "$legacy_database_missing_schema_fixture/.env" ||
  fail "schema-less legacy config did not migrate to schema v7"
grep -Fqx "DATABASE_PATH='./data/queue.db'" "$legacy_database_missing_schema_fixture/.env" ||
  fail "schema-less legacy config was not pinned to queue.db"
printf 'ok - schema-less legacy config keeps its queue.db default\n'

new_fixture fresh_database_custom_data
fresh_database_custom_data_fixture=$FIXTURE
printf '%s\n' "DATA_DIR='./Fresh State'" >>"$fresh_database_custom_data_fixture/.env"
run_fixture \
  "$fresh_database_custom_data_fixture" \
  "$fresh_database_custom_data_fixture/env.log" \
  ./run.sh env
grep -Fqx 'DATABASE_PATH=./Fresh State/state.db' "$fresh_database_custom_data_fixture/env.log" ||
  fail "fresh schema-v7 DATA_DIR did not derive state.db"
[ -z "$(find "$fresh_database_custom_data_fixture" -maxdepth 1 -name '.env.backup.*' -print)" ] ||
  fail "fresh schema-v7 default created a migration backup"
if grep -Eq '^DATABASE_PATH=' "$fresh_database_custom_data_fixture/.env"; then
  fail "fresh schema-v7 fallback unexpectedly rewrote .env"
fi
printf 'ok - fresh schema-v7 config derives state.db without rewriting .env\n'

new_fixture fresh_database_empty
fresh_database_empty_fixture=$FIXTURE
cat >>"$fresh_database_empty_fixture/.env" <<'EOF'
DATA_DIR='./Fresh Empty'
DATABASE_PATH=''
EOF
run_fixture \
  "$fresh_database_empty_fixture" \
  "$fresh_database_empty_fixture/env.log" \
  ./run.sh env
grep -Fqx 'DATABASE_PATH=./Fresh Empty/state.db' "$fresh_database_empty_fixture/env.log" ||
  fail "empty schema-v7 DATABASE_PATH did not derive state.db"
[ -z "$(find "$fresh_database_empty_fixture" -maxdepth 1 -name '.env.backup.*' -print)" ] ||
  fail "empty schema-v7 DATABASE_PATH created a migration backup"
printf 'ok - empty schema-v7 DATABASE_PATH uses the fresh state.db default\n'

new_fixture malformed_schema
malformed_schema_fixture=$FIXTURE
awk '
  /^ENV_SCHEMA_VERSION=/ { print "ENV_SCHEMA_VERSION=surprise"; next }
  { print }
' "$malformed_schema_fixture/.env" >"$malformed_schema_fixture/.env.old"
mv "$malformed_schema_fixture/.env.old" "$malformed_schema_fixture/.env"
chmod 644 "$malformed_schema_fixture/.env"
cp "$malformed_schema_fixture/.env" "$malformed_schema_fixture/.env.before"
if run_fixture "$malformed_schema_fixture" "$malformed_schema_fixture/migrate.log" ./run.sh migrate-env; then
  fail "malformed schema migration unexpectedly succeeded"
fi
cmp "$malformed_schema_fixture/.env.before" "$malformed_schema_fixture/.env" >/dev/null ||
  fail "malformed schema migration changed .env"
assert_mode "$malformed_schema_fixture/.env" 600
[ -z "$(find "$malformed_schema_fixture" -maxdepth 1 -name '.env.backup.*' -print)" ] ||
  fail "malformed schema migration created a backup"
printf 'ok - malformed schema fails closed without migration writes\n'

new_fixture future_schema
future_schema_fixture=$FIXTURE
awk '
  /^ENV_SCHEMA_VERSION=/ { print "ENV_SCHEMA_VERSION=8"; next }
  { print }
' "$future_schema_fixture/.env" >"$future_schema_fixture/.env.old"
mv "$future_schema_fixture/.env.old" "$future_schema_fixture/.env"
chmod 644 "$future_schema_fixture/.env"
cp "$future_schema_fixture/.env" "$future_schema_fixture/.env.before"
if run_fixture "$future_schema_fixture" "$future_schema_fixture/migrate.log" ./run.sh migrate-env; then
  fail "future schema migration unexpectedly succeeded"
fi
cmp "$future_schema_fixture/.env.before" "$future_schema_fixture/.env" >/dev/null ||
  fail "future schema migration changed .env"
assert_mode "$future_schema_fixture/.env" 600
[ -z "$(find "$future_schema_fixture" -maxdepth 1 -name '.env.backup.*' -print)" ] ||
  fail "future schema migration created a backup"
printf 'ok - future schema fails closed without migration writes\n'

printf 'all permission tests passed\n'
