BEGIN {
  uint64_max = "18446744073709551615"
  exit_status = 0
}

function decimal_compare(left, right,    i, left_digit, right_digit) {
  if (length(left) != length(right)) {
    return length(left) < length(right) ? -1 : 1
  }
  for (i = 1; i <= length(left); i++) {
    left_digit = substr(left, i, 1) + 0
    right_digit = substr(right, i, 1) + 0
    if (left_digit != right_digit) return left_digit < right_digit ? -1 : 1
  }
  return 0
}

function is_uint64(value) {
  if (value == "0") return 1
  if (value !~ /^[1-9][0-9]*$/ || length(value) > 20) return 0
  return length(value) < 20 || decimal_compare(value, uint64_max) <= 0
}

function publish(line,    command) {
  print line > state_tmp
  close(state_tmp)
  # Both paths come from the fixed-base, mktemp-created supervisor directory.
  command = "chmod 600 " state_tmp " && mv " state_tmp " " state_path
  return system(command) == 0
}

function fail(reason, status) {
  failed = 1
  exit_status = publish("ERROR " reason) ? status : 24
  exit exit_status
}

function parse_worker(token, label, position,    parts) {
  if (substr(token, 1, 2) != label "=" ||
      split(substr(token, 3), parts, ",") != 2 ||
      !is_uint64(parts[1]) || parts[2] !~ /^(0[0-9]|10)$/) {
    fail("malformed-frame", 21)
  }
  next_sequence[position] = parts[1]
  next_phase[position] = parts[2]
}

{
  if (length($0) > 191 || split($0, field, " ") != 7 ||
      field[1] != "TGOBS1" || !is_uint64(field[2])) {
    fail("malformed-frame", 21)
  }
  parse_worker(field[3], "t", 1)
  parse_worker(field[4], "r", 2)
  parse_worker(field[5], "e", 3)
  parse_worker(field[6], "m", 4)
  parse_worker(field[7], "p", 5)
  normalized = "TGOBS1 " field[2] \
    " t=" next_sequence[1] "," next_phase[1] \
    " r=" next_sequence[2] "," next_phase[2] \
    " e=" next_sequence[3] "," next_phase[3] \
    " m=" next_sequence[4] "," next_phase[4] \
    " p=" next_sequence[5] "," next_phase[5]
  if ($0 != normalized) fail("malformed-frame", 21)
  if (have_frame && decimal_compare(field[2], frame) <= 0) {
    fail("frame-order", 22)
  }
  for (i = 1; i <= 5; i++) {
    if (have_frame) {
      ordering = decimal_compare(next_sequence[i], sequence[i])
      if (ordering < 0) fail("worker-sequence-decrease", 23)
      if (ordering == 0 && next_phase[i] != phase[i]) {
        fail("worker-phase-without-progress", 23)
      }
    }
    sequence[i] = next_sequence[i]
    phase[i] = next_phase[i]
  }
  frame = field[2]
  have_frame = 1
  if (!publish(normalized)) {
    failed = 1
    exit_status = 24
    exit exit_status
  }
}

END {
  if (!failed) exit_status = publish("ERROR channel-eof") ? 25 : 24
  exit exit_status
}
