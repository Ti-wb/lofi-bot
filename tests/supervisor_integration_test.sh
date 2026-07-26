#!/bin/sh
set -eu

REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
TEST_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/tg-obs-supervisor.XXXXXX")
ACTIVE_ROOT_PID=

fail() {
  printf 'not ok - %s\n' "$1" >&2
  find "$TEST_ROOT" -name run.log -type f -exec sh -c '
    for log_path do
      printf "%s\n" "--- $log_path" >&2
      tail -n 80 "$log_path" >&2
    done
  ' sh {} +
  exit 1
}

fixture_processes() {
  find "$TEST_ROOT" -type f \( -name '*.pid' -o -name '*.pids' \) -print 2>/dev/null |
    while IFS= read -r pid_file; do
      while IFS= read -r pid; do
        case "$pid" in
          ""|*[!0-9]*) continue ;;
        esac
        command_line=$(ps -o command= -p "$pid" 2>/dev/null || true)
        case "$command_line" in
          *"$TEST_ROOT"*) printf '%s\n' "$pid" ;;
        esac
      done <"$pid_file"
    done
}

fixture_command_processes() {
  fixture_path=$1
  ps -axo pid=,command= 2>/dev/null |
    while read -r process_pid command_line; do
      case "$command_line" in
        *"$fixture_path"*) printf '%s\n' "$process_pid" ;;
      esac
    done
}

fixture_process_groups_alive() {
  fixture_path=$1
  for pid_file in \
    "$fixture_path/app-supervisor.pids" \
    "$fixture_path/app.pids" \
    "$fixture_path/bot-supervisor.pid" \
    "$fixture_path/bot.pid" \
    "$fixture_path/build-group.pid"
  do
    [ -f "$pid_file" ] || continue
    while IFS= read -r group_pid; do
      case "$group_pid" in
        ""|*[!0-9]*) continue ;;
      esac
      if /bin/kill -0 "-$group_pid" 2>/dev/null; then
        printf '%s\n' "$group_pid"
      fi
    done <"$pid_file"
  done
}

cleanup() {
  trap - 0 HUP INT TERM
  if [ -n "$ACTIVE_ROOT_PID" ] && kill -0 "$ACTIVE_ROOT_PID" 2>/dev/null; then
    kill -TERM "$ACTIVE_ROOT_PID" 2>/dev/null || true
    sleep 2
    kill -KILL "$ACTIVE_ROOT_PID" 2>/dev/null || true
    wait "$ACTIVE_ROOT_PID" 2>/dev/null || true
  fi
  fixture_processes | while IFS= read -r pid; do
    kill -KILL "$pid" 2>/dev/null || true
  done
  fixture_command_processes "$TEST_ROOT" | while IFS= read -r pid; do
    kill -KILL "$pid" 2>/dev/null || true
  done
  rm -rf "$TEST_ROOT"
}
trap cleanup 0 HUP INT TERM

wait_for_file() {
  path=$1
  attempts=${2:-100}
  count=0
  while [ "$count" -lt "$attempts" ]; do
    [ -s "$path" ] && return 0
    sleep 0.1
    count=$((count + 1))
  done
  fail "timed out waiting for $path"
}

wait_for_line_count() {
  path=$1
  expected=$2
  attempts=${3:-100}
  count=0
  while [ "$count" -lt "$attempts" ]; do
    if [ -f "$path" ] && [ "$(wc -l <"$path" | tr -d '[:space:]')" -ge "$expected" ]; then
      return 0
    fi
    sleep 0.1
    count=$((count + 1))
  done
  fail "timed out waiting for $expected lines in $path"
}

assert_process_gone() {
  pid=$1
  label=$2
  count=0
  while [ "$count" -lt 20 ]; do
    if ! kill -0 "$pid" 2>/dev/null; then
      return 0
    fi
    sleep 0.1
    count=$((count + 1))
  done
  fail "$label process $pid is still alive"
}

wait_for_root() {
  pid=$1
  timeout=$2
  (
    sleep "$timeout"
    kill -KILL "$pid" 2>/dev/null || true
  ) &
  watchdog_pid=$!
  ROOT_STATUS=0
  wait "$pid" 2>/dev/null || ROOT_STATUS=$?
  kill "$watchdog_pid" 2>/dev/null || true
  wait "$watchdog_pid" 2>/dev/null || true
  ACTIVE_ROOT_PID=
}

new_fixture() {
  name=$1
  FIXTURE="$TEST_ROOT/$name"
  mkdir -p "$FIXTURE/bin" "$FIXTURE/cmd/tg-obs-bot" "$FIXTURE/internal" "$FIXTURE/deploy/telegram-bot-api"
  cp "$REPO_ROOT/run.sh" "$FIXTURE/run.sh"
  cp "$REPO_ROOT/deploy/telegram-bot-api/healthcheck.sh" "$FIXTURE/deploy/telegram-bot-api/healthcheck.sh"
  chmod +x "$FIXTURE/run.sh" "$FIXTURE/deploy/telegram-bot-api/healthcheck.sh"
  : >"$FIXTURE/cmd/tg-obs-bot/main.go"
  : >"$FIXTURE/internal/source.go"
  : >"$FIXTURE/go.mod"
  : >"$FIXTURE/go.sum"

  cat >"$FIXTURE/.env" <<'EOF'
ENV_SCHEMA_VERSION=5
TELEGRAM_BOT_TOKEN=test-token
TELEGRAM_API_BASE_URL=http://127.0.0.1:8081
TELEGRAM_API_ID=1
TELEGRAM_API_HASH=test-hash
TELEGRAM_BOT_API_DIR=./data/telegram-bot-api
ALLOWED_CHAT_ID=1
GO=./bin/fake-go
RESTART_MIN_DELAY_SECONDS=1
RESTART_MAX_DELAY_SECONDS=4
RESTART_RESET_AFTER_SECONDS=1
SHUTDOWN_GRACE_SECONDS=1
EOF

  cat >"$FIXTURE/bin/fake-go" <<'EOF'
#!/bin/sh
set -eu
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ -f "$script_dir/.env" ]; then
  root=$script_dir
else
  root=$(CDPATH= cd -- "$script_dir/.." && pwd)
fi
printf '%s\n' "$*" >>"$root/go.commands"
[ "${1:-}" = build ] || {
  printf 'fake-go rejected non-build command: %s\n' "$*" >&2
  exit 91
}
if [ -f "$root/slow-build" ]; then
  printf '%s\n' "$$" >"$root/build-child.pid"
  ps -o ppid= -p "$$" | tr -d '[:space:]' >"$root/build-group.pid"
  "$root/grandchild.sh" build "$root" &
  trap '' HUP INT TERM
  wait
fi
if [ -f "$root/graceful-build" ]; then
  printf '%s\n' "$$" >"$root/build-child.pid"
  ps -o ppid= -p "$$" | tr -d '[:space:]' >"$root/build-group.pid"
  "$root/grandchild.sh" build-graceful "$root" &
  trap 'exit 0' HUP INT TERM
  wait
fi
shift
output=
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then
    output=$2
    shift 2
  else
    shift
  fi
done
[ -n "$output" ] || exit 92
cp "$root/app-template.sh" "$output"
chmod +x "$output"
EOF

  cat >"$FIXTURE/bin/curl" <<'EOF'
#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
printf '%s\n' "$*" >>"$root/curl.commands"
cat >/dev/null
printf '{"ok":true}\n'
EOF

  cat >"$FIXTURE/bin/mv" <<'EOF'
#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
destination=
for argument do
  destination=$argument
done
case "$destination" in
  *.group)
    if [ -f "$root/fail-state-write" ]; then
      sleep 0.1
      exit 75
    fi
    ;;
esac
exec /bin/mv "$@"
EOF

  cat >"$FIXTURE/grandchild.sh" <<'EOF'
#!/bin/sh
set -eu
role=$1
root=$2
printf '%s\n' "$$" >>"$root/$role-grandchild.pids"
if [ "$role" = build-graceful ] || [ -f "$root/fast-exit" ]; then
  trap 'exit 0' HUP INT TERM
else
  trap '' HUP INT TERM
fi
while :; do
  sleep 1
done
EOF

  cat >"$FIXTURE/app-template.sh" <<'EOF'
#!/bin/sh
set -eu
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ -f "$script_dir/.env" ]; then
  root=$script_dir
else
  root=$(CDPATH= cd -- "$script_dir/.." && pwd)
fi
if [ -s "$root/app-grandchild.pids" ]; then
  previous_grandchild=$(tail -n 1 "$root/app-grandchild.pids")
  if kill -0 "$previous_grandchild" 2>/dev/null; then
    printf '%s\n' "$previous_grandchild" >"$root/replacement-started-before-drain"
  fi
fi
printf '%s\n' "$$" >>"$root/app.pids"
supervisor_pid=$(ps -o ppid= -p "$$" | tr -d '[:space:]')
printf '%s' "$supervisor_pid" >>"$root/app-supervisor.pids"
printf '\n' >>"$root/app-supervisor.pids"
ps -o pgid= -p "$$" | tr -d '[:space:]' >>"$root/app-child.pgids"
printf '\n' >>"$root/app-child.pgids"
if [ -f "$root/post-exit-pause" ]; then
  "$root/post-exit-grandchild.sh" "$$" "$supervisor_pid" "$root" &
  sleep 0.05
  kill -KILL "$$"
fi
"$root/grandchild.sh" app "$root" &
grandchild_pid=$!
stop() {
  trap - HUP INT TERM
  kill -KILL "$grandchild_pid" 2>/dev/null || true
  wait "$grandchild_pid" 2>/dev/null || true
  exit 0
}
trap stop HUP INT TERM
while :; do
  sleep 1
done
EOF

  cat >"$FIXTURE/post-exit-grandchild.sh" <<'EOF'
#!/bin/sh
set -eu
leader_pid=$1
supervisor_pid=$2
root=$3
printf '%s\n' "$$" >"$root/post-exit-grandchild.pid"
trap '' HUP INT TERM
while kill -0 "$leader_pid" 2>/dev/null; do
  sleep 0.01
done
kill -STOP "$supervisor_pid"
printf '%s\n' "$supervisor_pid" >"$root/post-exit-supervisor-stopped"
while :; do
  sleep 1
done
EOF

  cat >"$FIXTURE/deploy/telegram-bot-api/run.sh" <<'EOF'
#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
printf '%s\n' "$$" >"$root/bot.pid"
ps -o ppid= -p "$$" | tr -d '[:space:]' >"$root/bot-supervisor.pid"
ps -o pgid= -p "$$" | tr -d '[:space:]' >"$root/bot-child.pgid"
"$root/grandchild.sh" bot "$root" &
stop() {
  trap - HUP INT TERM
  exit 0
}
trap stop HUP INT TERM
while :; do
  sleep 1
done
EOF

  chmod +x \
    "$FIXTURE/bin/fake-go" \
    "$FIXTURE/bin/curl" \
    "$FIXTURE/bin/mv" \
    "$FIXTURE/grandchild.sh" \
    "$FIXTURE/app-template.sh" \
    "$FIXTURE/post-exit-grandchild.sh" \
    "$FIXTURE/deploy/telegram-bot-api/run.sh"
}

start_fixture_command() {
  fixture=$1
  command_name=$2
  env PATH="$fixture/bin:$PATH" /bin/sh -c '
    cd "$1"
    exec ./run.sh "$2"
  ' sh "$fixture" "$command_name" >"$fixture/run.log" 2>&1 &
  ACTIVE_ROOT_PID=$!
}

start_fixture_command_with_job_control() {
  fixture=$1
  command_name=$2
  set -m
  env PATH="$fixture/bin:$PATH" /bin/sh -c '
    cd "$1"
    exec ./run.sh "$2"
  ' sh "$fixture" "$command_name" >"$fixture/run.log" 2>&1 &
  ACTIVE_ROOT_PID=$!
  set +m
}

start_fixture() {
  start_fixture_command "$1" up
}

new_fixture process_tree
process_fixture=$FIXTURE
start_fixture "$process_fixture"
process_root_pid=$ACTIVE_ROOT_PID
wait_for_line_count "$process_fixture/app.pids" 1
wait_for_line_count "$process_fixture/app-grandchild.pids" 1
wait_for_file "$process_fixture/bot.pid"
wait_for_line_count "$process_fixture/bot-grandchild.pids" 1
wait_for_file "$process_fixture/curl.commands"

[ -x "$process_fixture/dist/tg-obs-bot" ] || fail "atomic build did not install dist/tg-obs-bot"
grep -q '^build ' "$process_fixture/go.commands" || fail "supervised startup did not build the app"
if grep -Eq '(^|[[:space:]])run([[:space:]]|$)' "$process_fixture/go.commands"; then
  fail "supervised startup invoked go run"
fi
grep -Fq -- '--connect-timeout 2 --max-time 5' "$process_fixture/curl.commands" ||
  fail "healthcheck did not pass bounded curl timeouts"

old_app_pid=$(sed -n '1p' "$process_fixture/app.pids")
old_app_grandchild_pid=$(sed -n '1p' "$process_fixture/app-grandchild.pids")
app_supervisor_pid=$(sed -n '1p' "$process_fixture/app-supervisor.pids")
bot_pid=$(cat "$process_fixture/bot.pid")
bot_supervisor_pid=$(cat "$process_fixture/bot-supervisor.pid")
bot_grandchild_pid=$(sed -n '1p' "$process_fixture/bot-grandchild.pids")

[ "$(ps -o pgid= -p "$app_supervisor_pid" | tr -d '[:space:]')" = "$app_supervisor_pid" ] ||
  fail "app supervisor is not its process-group leader"
[ "$(ps -o pgid= -p "$bot_supervisor_pid" | tr -d '[:space:]')" = "$bot_supervisor_pid" ] ||
  fail "bot supervisor is not its process-group leader"
app_child_pgid=$(sed -n '1p' "$process_fixture/app-child.pgids")
bot_child_pgid=$(cat "$process_fixture/bot-child.pgid")
[ "$app_child_pgid" = "$old_app_pid" ] || fail "app child generation is not its process-group leader"
[ "$bot_child_pgid" = "$bot_pid" ] || fail "bot child generation is not its process-group leader"
[ "$app_child_pgid" != "$app_supervisor_pid" ] || fail "app child shares its supervisor process group"
[ "$bot_child_pgid" != "$bot_supervisor_pid" ] || fail "bot child shares its supervisor process group"

kill -KILL "$old_app_pid"
wait_for_line_count "$process_fixture/app.pids" 2
wait_for_line_count "$process_fixture/app-grandchild.pids" 2
sleep 1
[ "$(wc -l <"$process_fixture/app.pids" | tr -d '[:space:]')" -eq 2 ] ||
  fail "killing one app child caused more than one replacement"
[ "$(cat "$process_fixture/bot.pid")" = "$bot_pid" ] || fail "app restart also restarted bot-api"
[ "$(sed -n '2p' "$process_fixture/app-supervisor.pids")" = "$app_supervisor_pid" ] ||
  fail "app child replacement did not stay under the same supervisor"
[ ! -e "$process_fixture/replacement-started-before-drain" ] ||
  fail "replacement started before the prior child generation was drained"
assert_process_gone "$old_app_pid" "old app"
assert_process_gone "$old_app_grandchild_pid" "old app grandchild"

new_app_pid=$(sed -n '2p' "$process_fixture/app.pids")
new_app_grandchild_pid=$(sed -n '2p' "$process_fixture/app-grandchild.pids")
new_app_pgid=$(sed -n '2p' "$process_fixture/app-child.pgids")
[ "$new_app_pgid" = "$new_app_pid" ] || fail "replacement app is not its process-group leader"
[ "$new_app_pgid" != "$app_child_pgid" ] || fail "replacement reused the prior child process group"
started_at=$(date +%s)
kill -TERM "$process_root_pid"
wait_for_root "$process_root_pid" 4
elapsed=$(( $(date +%s) - started_at ))
[ "$ROOT_STATUS" -eq 143 ] || fail "root supervisor exited with $ROOT_STATUS instead of 143"
[ "$elapsed" -le 3 ] || fail "root shutdown exceeded SHUTDOWN_GRACE_SECONDS + 2"
for process_entry in \
  "$app_supervisor_pid:app supervisor" \
  "$new_app_pid:app child" \
  "$new_app_grandchild_pid:app grandchild" \
  "$bot_supervisor_pid:bot supervisor" \
  "$bot_pid:bot child" \
  "$bot_grandchild_pid:bot grandchild"
do
  process_pid=${process_entry%%:*}
  process_label=${process_entry#*:}
  assert_process_gone "$process_pid" "$process_label"
done
grep -q 'sending KILL' "$process_fixture/run.log" ||
  fail "stubborn descendant did not exercise TERM-to-KILL escalation"
printf 'ok - process-tree supervision, direct app restart, atomic build, and bounded healthcheck\n'

new_fixture build_cancel
build_fixture=$FIXTURE
: >"$build_fixture/slow-build"
start_fixture "$build_fixture"
build_root_pid=$ACTIVE_ROOT_PID
wait_for_file "$build_fixture/build-child.pid"
wait_for_file "$build_fixture/build-group.pid"
wait_for_line_count "$build_fixture/build-grandchild.pids" 1
build_child_pid=$(cat "$build_fixture/build-child.pid")
build_group_pid=$(cat "$build_fixture/build-group.pid")
build_grandchild_pid=$(sed -n '1p' "$build_fixture/build-grandchild.pids")
[ "$build_group_pid" != "$build_root_pid" ] || fail "build shares the root process group"
[ "$(ps -o pgid= -p "$build_child_pid" | tr -d '[:space:]')" = "$build_group_pid" ] ||
  fail "build child is outside the owned build process group"
kill -HUP "$build_root_pid"
wait_for_root "$build_root_pid" 4
[ "$ROOT_STATUS" -eq 129 ] || fail "build-phase HUP exited with $ROOT_STATUS instead of 129"
assert_process_gone "$build_group_pid" "build group leader"
assert_process_gone "$build_child_pid" "build child"
assert_process_gone "$build_grandchild_pid" "build grandchild"
[ ! -e "$build_fixture/dist/tg-obs-bot" ] || fail "interrupted build installed a partial binary"
printf 'ok - build-phase HUP drains the isolated compiler process tree\n'

new_fixture app_build_cancel
app_build_fixture=$FIXTURE
: >"$app_build_fixture/graceful-build"
awk '$0 !~ /^SHUTDOWN_GRACE_SECONDS=/' "$app_build_fixture/.env" >"$app_build_fixture/.env.without-grace"
mv "$app_build_fixture/.env.without-grace" "$app_build_fixture/.env"
start_fixture_command "$app_build_fixture" app
app_build_root_pid=$ACTIVE_ROOT_PID
wait_for_file "$app_build_fixture/build-child.pid"
wait_for_file "$app_build_fixture/build-group.pid"
wait_for_line_count "$app_build_fixture/build-graceful-grandchild.pids" 1
app_build_group_pid=$(cat "$app_build_fixture/build-group.pid")
app_build_child_pid=$(cat "$app_build_fixture/build-child.pid")
app_build_grandchild_pid=$(sed -n '1p' "$app_build_fixture/build-graceful-grandchild.pids")
kill -TERM "$app_build_root_pid"
wait_for_root "$app_build_root_pid" 3
[ "$ROOT_STATUS" -eq 143 ] || fail "run.sh app build TERM exited with $ROOT_STATUS instead of 143"
assert_process_gone "$app_build_group_pid" "run.sh app build group leader"
assert_process_gone "$app_build_child_pid" "run.sh app build child"
assert_process_gone "$app_build_grandchild_pid" "run.sh app build grandchild"
[ ! -e "$app_build_fixture/dist/tg-obs-bot" ] || fail "run.sh app interrupted build installed a partial binary"
printf 'ok - run.sh app supplies shutdown grace before cancellable build\n'

new_fixture dead_leader_owned_group
dead_leader_fixture=$FIXTURE
: >"$dead_leader_fixture/post-exit-pause"
printf '%s\n' 'APP_BIN=./app-template.sh' >>"$dead_leader_fixture/.env"
start_fixture "$dead_leader_fixture"
dead_leader_root_pid=$ACTIVE_ROOT_PID
wait_for_file "$dead_leader_fixture/post-exit-supervisor-stopped"
wait_for_file "$dead_leader_fixture/post-exit-grandchild.pid"
wait_for_file "$dead_leader_fixture/bot.pid"
dead_app_pid=$(sed -n '1p' "$dead_leader_fixture/app.pids")
dead_app_supervisor_pid=$(cat "$dead_leader_fixture/post-exit-supervisor-stopped")
dead_app_grandchild_pid=$(cat "$dead_leader_fixture/post-exit-grandchild.pid")
dead_bot_pid=$(cat "$dead_leader_fixture/bot.pid")
dead_bot_supervisor_pid=$(cat "$dead_leader_fixture/bot-supervisor.pid")
dead_bot_grandchild_pid=$(sed -n '1p' "$dead_leader_fixture/bot-grandchild.pids")
assert_process_gone "$dead_app_pid" "dead app leader"
kill -STOP "$dead_leader_root_pid"
kill -TERM "$dead_app_supervisor_pid"
kill -CONT "$dead_app_supervisor_pid"
assert_process_gone "$dead_app_grandchild_pid" "owned dead-leader grandchild"
kill -CONT "$dead_leader_root_pid"
wait_for_root "$dead_leader_root_pid" 6
[ "$ROOT_STATUS" -eq 1 ] || fail "dead-leader supervisor exit produced $ROOT_STATUS instead of 1"
for process_entry in \
  "$dead_app_supervisor_pid:dead-leader app supervisor" \
  "$dead_bot_supervisor_pid:dead-leader bot supervisor" \
  "$dead_bot_pid:dead-leader bot child" \
  "$dead_bot_grandchild_pid:dead-leader bot grandchild"
do
  process_pid=${process_entry%%:*}
  process_label=${process_entry#*:}
  assert_process_gone "$process_pid" "$process_label"
done
printf 'ok - TERM after leader exit drains the previously owned generation PGID\n'

new_fixture supervisor_sigkill
supervisor_kill_fixture=$FIXTURE
printf '%s\n' 'APP_BIN=./app-template.sh' >>"$supervisor_kill_fixture/.env"
start_fixture "$supervisor_kill_fixture"
supervisor_kill_root_pid=$ACTIVE_ROOT_PID
wait_for_line_count "$supervisor_kill_fixture/app.pids" 1
wait_for_line_count "$supervisor_kill_fixture/app-grandchild.pids" 1
wait_for_file "$supervisor_kill_fixture/bot.pid"
wait_for_line_count "$supervisor_kill_fixture/bot-grandchild.pids" 1
killed_app_supervisor_pid=$(sed -n '1p' "$supervisor_kill_fixture/app-supervisor.pids")
orphaned_app_pid=$(sed -n '1p' "$supervisor_kill_fixture/app.pids")
orphaned_app_grandchild_pid=$(sed -n '1p' "$supervisor_kill_fixture/app-grandchild.pids")
surviving_bot_supervisor_pid=$(cat "$supervisor_kill_fixture/bot-supervisor.pid")
surviving_bot_pid=$(cat "$supervisor_kill_fixture/bot.pid")
surviving_bot_grandchild_pid=$(sed -n '1p' "$supervisor_kill_fixture/bot-grandchild.pids")
kill -KILL "$killed_app_supervisor_pid"
wait_for_root "$supervisor_kill_root_pid" 6
[ "$ROOT_STATUS" -eq 1 ] || fail "SIGKILLed supervisor produced root status $ROOT_STATUS instead of 1"
for process_entry in \
  "$killed_app_supervisor_pid:killed app supervisor" \
  "$orphaned_app_pid:orphaned app child" \
  "$orphaned_app_grandchild_pid:orphaned app grandchild" \
  "$surviving_bot_supervisor_pid:peer bot supervisor" \
  "$surviving_bot_pid:peer bot child" \
  "$surviving_bot_grandchild_pid:peer bot grandchild"
do
  process_pid=${process_entry%%:*}
  process_label=${process_entry#*:}
  assert_process_gone "$process_pid" "$process_label"
done
grep -q 'supervisor exited unexpectedly; shutting down the stack' "$supervisor_kill_fixture/run.log" ||
  fail "root did not report unexpected supervisor death"
printf 'ok - root detects supervisor SIGKILL and drains both recorded child PGIDs\n'

new_fixture state_write_failure
state_failure_fixture=$FIXTURE
: >"$state_failure_fixture/fail-state-write"
printf '%s\n' 'APP_BIN=./app-template.sh' >>"$state_failure_fixture/.env"
start_fixture "$state_failure_fixture"
state_failure_root_pid=$ACTIVE_ROOT_PID
wait_for_root "$state_failure_root_pid" 6
[ "$ROOT_STATUS" -ne 0 ] || fail "state-write failure exited successfully"
sleep 0.1
state_failure_processes=$(fixture_command_processes "$state_failure_fixture")
[ -z "$state_failure_processes" ] ||
  fail "state-write failure leaked fixture process(es): $state_failure_processes"
state_failure_groups=$(fixture_process_groups_alive "$state_failure_fixture")
[ -z "$state_failure_groups" ] ||
  fail "state-write failure leaked process group(s): $state_failure_groups"
grep -q 'could not record Telegram Local Bot API Server child process group' "$state_failure_fixture/run.log" ||
  fail "state-write failure was not reported"
printf 'ok - state-write failure drains the verified child group and fails closed\n'

new_fixture singleton_contention
singleton_fixture=$FIXTURE
cat >"$singleton_fixture/singleton-app" <<'EOF'
#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
printf '%s\n' "$$" >>"$root/singleton-app.pids"
exit 73
EOF
chmod +x "$singleton_fixture/singleton-app"
printf '%s\n' 'APP_BIN=./singleton-app' >>"$singleton_fixture/.env"
start_fixture "$singleton_fixture"
singleton_root_pid=$ACTIVE_ROOT_PID
wait_for_root "$singleton_root_pid" 6
[ "$ROOT_STATUS" -eq 73 ] ||
  fail "singleton contention produced root status $ROOT_STATUS instead of 73"
[ "$(wc -l <"$singleton_fixture/singleton-app.pids" | tr -d '[:space:]')" -eq 1 ] ||
  fail "singleton contention restarted the app"
grep -q 'database or Telegram bot is already owned; not restarting' "$singleton_fixture/run.log" ||
  fail "singleton contention did not report the non-retryable ownership failure"
if grep -q 'tg-obs-bot exited with status 73; restarting' "$singleton_fixture/run.log"; then
  fail "singleton contention entered restart backoff"
fi
sleep 0.1
singleton_processes=$(fixture_command_processes "$singleton_fixture")
[ -z "$singleton_processes" ] ||
  fail "singleton contention leaked fixture process(es): $singleton_processes"
singleton_groups=$(fixture_process_groups_alive "$singleton_fixture")
[ -z "$singleton_groups" ] ||
  fail "singleton contention leaked process group(s): $singleton_groups"
printf 'ok - singleton contention stops the stack without restart churn\n'

new_fixture launch_race
race_fixture=$FIXTURE
: >"$race_fixture/fast-exit"
printf '%s\n' 'APP_BIN=./app-template.sh' >>"$race_fixture/.env"
race_iteration=1
while [ "$race_iteration" -le 100 ]; do
  start_fixture "$race_fixture"
  race_root_pid=$ACTIVE_ROOT_PID
  delay_ms=$(( (race_iteration * 17 + 7) % 21 ))
  delay_seconds=$(awk -v delay_ms="$delay_ms" 'BEGIN { printf "%.3f", delay_ms / 1000 }')
  sleep "$delay_seconds"
  kill -TERM "$race_root_pid" 2>/dev/null || true
  wait_for_root "$race_root_pid" 3
  [ "$ROOT_STATUS" -eq 143 ] ||
    fail "launch-race iteration $race_iteration exited with $ROOT_STATUS instead of 143"
  sleep 0.02
  leaked_processes=$(fixture_command_processes "$race_fixture")
  [ -z "$leaked_processes" ] ||
    fail "launch-race iteration $race_iteration leaked fixture process(es): $leaked_processes"
  leaked_groups=$(fixture_process_groups_alive "$race_fixture")
  [ -z "$leaked_groups" ] ||
    fail "launch-race iteration $race_iteration leaked process group(s): $leaked_groups"
  race_iteration=$((race_iteration + 1))
done
printf 'ok - 100 jittered startup TERM races leave no untracked descendants\n'

new_fixture backoff_reset
backoff_fixture=$FIXTURE
cat >"$backoff_fixture/backoff-app" <<'EOF'
#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
count=0
[ ! -f "$root/backoff.count" ] || count=$(cat "$root/backoff.count")
count=$((count + 1))
printf '%s\n' "$count" >"$root/backoff.count"
case "$count" in
  1|2) exit 1 ;;
  3)
    sleep 2
    exit 1
    ;;
esac
printf '%s\n' "$$" >"$root/backoff-ready.pid"
trap 'exit 0' HUP INT TERM
while :; do
  sleep 1
done
EOF
chmod +x "$backoff_fixture/backoff-app"
printf '%s\n' 'APP_BIN=./backoff-app' >>"$backoff_fixture/.env"
start_fixture_command_with_job_control "$backoff_fixture" up
backoff_root_pid=$ACTIVE_ROOT_PID
wait_for_file "$backoff_fixture/backoff-ready.pid" 150
grep -q 'tg-obs-bot exited with status 1; restarting in 2s' "$backoff_fixture/run.log" ||
  fail "restart delay did not increase after repeated short failures"
grep -q 'tg-obs-bot ran for .*; resetting restart delay' "$backoff_fixture/run.log" ||
  fail "stable uptime did not reset restart delay"
[ "$(grep -c 'tg-obs-bot exited with status 1; restarting in 1s' "$backoff_fixture/run.log")" -ge 2 ] ||
  fail "restart delay did not return to minimum after stable uptime"
kill -INT "$backoff_root_pid"
wait_for_root "$backoff_root_pid" 3
[ "$ROOT_STATUS" -eq 130 ] || fail "backoff fixture INT exited with $ROOT_STATUS instead of 130"
printf 'ok - stable uptime resets exponential restart backoff; INT drains descendants\n'

printf 'all supervisor integration tests passed\n'
