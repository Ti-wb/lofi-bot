#!/bin/sh
set -eu

REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
READER="$REPO_ROOT/scripts/liveness-reader.awk"
TEST_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/tg-obs-liveness-reader.XXXXXX")
ACTIVE_READER=

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

cleanup() {
  trap - 0 HUP INT TERM
  exec 3>&- 2>/dev/null || true
  if [ -n "$ACTIVE_READER" ]; then
    kill -TERM "$ACTIVE_READER" 2>/dev/null || true
    wait "$ACTIVE_READER" 2>/dev/null || true
  fi
  rm -rf "$TEST_ROOT"
}
trap cleanup 0 HUP INT TERM

start_reader() {
  case_name=$1
  CASE_DIR="$TEST_ROOT/$case_name"
  STATE="$CASE_DIR/state"
  STATE_TMP="$CASE_DIR/state.tmp"
  FIFO="$CASE_DIR/frames.fifo"
  mkdir "$CASE_DIR"
  mkfifo "$FIFO"
  awk -v state_path="$STATE" -v state_tmp="$STATE_TMP" \
    -f "$READER" "$FIFO" &
  ACTIVE_READER=$!
  exec 3>"$FIFO"
}

write_frame() {
  printf '%s\n' "$1" >&3 || true
}

wait_for_state() {
  expected=$1
  attempts=0
  while [ "$attempts" -lt 100 ]; do
    if [ -r "$STATE" ] && [ "$(sed -n '1p' "$STATE")" = "$expected" ]; then
      return 0
    fi
    sleep 0.02
    attempts=$((attempts + 1))
  done
  fail "reader did not publish expected state: $expected"
}

finish_reader() {
  expected_status=$1
  expected_state=$2
  exec 3>&-
  reader_status=0
  wait "$ACTIVE_READER" 2>/dev/null || reader_status=$?
  ACTIVE_READER=
  [ "$reader_status" -eq "$expected_status" ] ||
    fail "reader exited $reader_status instead of $expected_status"
  if [ -n "$expected_state" ]; then
    [ -r "$STATE" ] || fail "reader did not publish terminal state"
    [ "$(sed -n '1p' "$STATE")" = "$expected_state" ] ||
      fail "reader published unexpected terminal state"
  fi
}

assert_rejected() {
  case_name=$1
  expected_status=$2
  expected_reason=$3
  shift 3
  start_reader "$case_name"
  for frame do
    write_frame "$frame"
  done
  finish_reader "$expected_status" "ERROR $expected_reason"
}

FRAME1='TGOBS1 1 t=1,00 r=1,00 e=1,00 m=1,00 p=1,00'
FRAME2_SAME='TGOBS1 2 t=1,00 r=1,00 e=1,00 m=1,00 p=1,00'
FRAME2_PHASE='TGOBS1 2 t=1,08 r=1,00 e=1,00 m=1,00 p=1,00'

start_reader max_values
MAX_FRAME='TGOBS1 18446744073709551615 t=18446744073709551615,10 r=18446744073709551615,10 e=18446744073709551615,10 m=18446744073709551615,10 p=18446744073709551615,10'
write_frame "$MAX_FRAME"
wait_for_state "$MAX_FRAME"
finish_reader 25 'ERROR channel-eof'

start_reader equal_sequence_same_phase
write_frame "$FRAME1"
write_frame "$FRAME2_SAME"
wait_for_state "$FRAME2_SAME"
finish_reader 25 'ERROR channel-eof'

assert_rejected overflow 21 malformed-frame \
  'TGOBS1 18446744073709551616 t=1,00 r=1,00 e=1,00 m=1,00 p=1,00'
assert_rejected leading_zero 21 malformed-frame \
  'TGOBS1 01 t=1,00 r=1,00 e=1,00 m=1,00 p=1,00'
assert_rejected wrong_order 21 malformed-frame \
  'TGOBS1 1 r=1,00 t=1,00 e=1,00 m=1,00 p=1,00'
assert_rejected invalid_phase 21 malformed-frame \
  'TGOBS1 1 t=1,11 r=1,00 e=1,00 m=1,00 p=1,00'
assert_rejected double_space 21 malformed-frame \
  'TGOBS1  1 t=1,00 r=1,00 e=1,00 m=1,00 p=1,00'
assert_rejected extra_field 21 malformed-frame \
  'TGOBS1 1 t=1,00 r=1,00 e=1,00 m=1,00 p=1,00 extra'
assert_rejected replay 22 frame-order "$FRAME1" "$FRAME1"
assert_rejected reverse 22 frame-order \
  'TGOBS1 2 t=1,00 r=1,00 e=1,00 m=1,00 p=1,00' "$FRAME1"
assert_rejected sequence_decrease 23 worker-sequence-decrease \
  'TGOBS1 1 t=2,00 r=2,00 e=2,00 m=2,00 p=2,00' \
  'TGOBS1 2 t=1,00 r=2,00 e=2,00 m=2,00 p=2,00'
assert_rejected mixed_progress 23 worker-sequence-decrease \
  'TGOBS1 1 t=2,00 r=2,00 e=2,00 m=2,00 p=2,00' \
  'TGOBS1 2 t=3,00 r=1,00 e=3,00 m=3,00 p=3,00'
assert_rejected phase_without_progress 23 worker-phase-without-progress \
  "$FRAME1" "$FRAME2_PHASE"

start_reader channel_eof
finish_reader 25 'ERROR channel-eof'

mkdir "$TEST_ROOT/fail-bin"
cat >"$TEST_ROOT/fail-bin/mv" <<'EOF'
#!/bin/sh
exit 75
EOF
chmod +x "$TEST_ROOT/fail-bin/mv"
CASE_DIR="$TEST_ROOT/state_write"
STATE="$CASE_DIR/state"
STATE_TMP="$CASE_DIR/state.tmp"
FIFO="$CASE_DIR/frames.fifo"
mkdir "$CASE_DIR"
mkfifo "$FIFO"
PATH="$TEST_ROOT/fail-bin:$PATH" \
  awk -v state_path="$STATE" -v state_tmp="$STATE_TMP" \
    -f "$READER" "$FIFO" &
ACTIVE_READER=$!
exec 3>"$FIFO"
write_frame "$FRAME1"
finish_reader 24 ''
[ ! -e "$STATE" ] || fail "failed atomic state write installed a state file"

printf 'ok - liveness reader accepts only strict monotonic TGOBS1 frames\n'
