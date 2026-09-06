#!/bin/sh
set -eu

die() {
  printf '%s\n' "error: $1" >&2
  exit 1
}

usage() {
  printf '%s\n' \
    "Usage: ./scripts/generate-acceptance-fixtures.sh OUTPUT_DIRECTORY"
}

[ "$#" -eq 1 ] || {
  usage >&2
  exit 2
}

FFMPEG_BIN=${FFMPEG_BIN:-ffmpeg}
FFPROBE_BIN=${FFPROBE_BIN:-ffprobe}
command -v "$FFMPEG_BIN" >/dev/null 2>&1 || die "ffmpeg is required"
command -v "$FFPROBE_BIN" >/dev/null 2>&1 || die "ffprobe is required"

case "$1" in
  /*) fixture_target=$1 ;;
  *) fixture_target=$(pwd -P)/$1 ;;
esac

[ ! -e "$fixture_target" ] ||
  die "output directory already exists; choose a new dedicated path"
fixture_parent=$(dirname -- "$fixture_target")
[ -d "$fixture_parent" ] ||
  die "output parent directory does not exist"

fixture_work=$(
  umask 077
  mktemp -d "$fixture_parent/.acceptance-fixtures.XXXXXX"
)
cleanup() {
  if [ -n "${fixture_work:-}" ] && [ -d "$fixture_work" ]; then
    rm -rf -- "$fixture_work"
  fi
}
trap cleanup EXIT HUP INT TERM

mkdir -p \
  "$fixture_work/library/loops" \
  "$fixture_work/library/music" \
  "$fixture_work/outside/positive" \
  "$fixture_work/outside/rejections" \
  "$fixture_work/evidence/fixture-probes"

make_video() {
  output=$1
  color=$2
  "$FFMPEG_BIN" -nostdin -hide_banner -loglevel error \
    -f lavfi -i "color=c=$color:s=160x90:r=10:d=8" \
    -an -c:v mpeg4 -q:v 5 -pix_fmt yuv420p -movflags +faststart \
    "$output"
}

make_music() {
  output=$1
  frequency=$2
  duration=$3
  "$FFMPEG_BIN" -nostdin -hide_banner -loglevel error \
    -f lavfi \
    -i "sine=frequency=$frequency:sample_rate=48000:duration=$duration" \
    -vn -c:a aac -b:a 64k -movflags +faststart "$output"
}

make_video "$fixture_work/library/loops/loop_morning_accept_a.mp4" blue
make_video "$fixture_work/library/loops/loop_morning_accept_b.mp4" cyan
make_video "$fixture_work/library/loops/loop_morning_focus_c.mp4" teal
make_video "$fixture_work/library/loops/loop_day_accept_a.mp4" yellow
make_video "$fixture_work/library/loops/loop_day_accept_b.mp4" orange
make_video "$fixture_work/library/loops/loop_day_focus_c.mp4" gold
make_video "$fixture_work/library/loops/loop_evening_accept_a.mp4" magenta
make_video "$fixture_work/library/loops/loop_evening_accept_b.mp4" purple
make_video "$fixture_work/library/loops/loop_evening_focus_c.mp4" violet
make_video "$fixture_work/library/loops/loop_night_accept_a.mp4" navy
make_video "$fixture_work/library/loops/loop_night_accept_b.mp4" black
make_video "$fixture_work/library/loops/loop_night_focus_c.mp4" gray

# Short A is long enough to create a deliberate skip-near-end window. Long B
# exceeds the bounded stale-event capture duration documented by the runbook.
make_music "$fixture_work/library/music/music_stale-short-a.m4a" 440 8
make_music "$fixture_work/library/music/music_stale-long-b.m4a" 220 600

make_video "$fixture_work/outside/positive/loop_day_upload_001.mp4" green
make_music "$fixture_work/outside/positive/music_upload-c.m4a" 660 20

cp "$fixture_work/library/loops/loop_morning_accept_a.mp4" \
  "$fixture_work/outside/rejections/invalid-playable.mp4"
cp "$fixture_work/library/music/music_stale-short-a.m4a" \
  "$fixture_work/outside/rejections/loop_day_bad-audio_001.mp4"
cp "$fixture_work/library/loops/loop_day_accept_b.mp4" \
  "$fixture_work/outside/rejections/music_bad-video.m4a"

require_stream() {
  expected=$1
  path=$2
  case "$expected" in
    video) selector=v ;;
    audio) selector=a ;;
    *) die "unknown generated stream requirement" ;;
  esac
  stream=$(
    "$FFPROBE_BIN" -v error -select_streams "$selector:0" \
      -show_entries stream=codec_type -of default=noprint_wrappers=1:nokey=1 \
      "$path"
  )
  [ "$stream" = "$expected" ] ||
    die "generated fixture is missing its required $expected stream"
}

for loop_fixture in "$fixture_work/library/loops/"*.mp4 \
  "$fixture_work/outside/positive/loop_day_upload_001.mp4"
do
  require_stream video "$loop_fixture"
done
for music_fixture in "$fixture_work/library/music/"*.m4a \
  "$fixture_work/outside/positive/music_upload-c.m4a"
do
  require_stream audio "$music_fixture"
done
require_stream audio \
  "$fixture_work/outside/rejections/loop_day_bad-audio_001.mp4"
require_stream video \
  "$fixture_work/outside/rejections/music_bad-video.m4a"

short_duration=$(
  "$FFPROBE_BIN" -v error -show_entries format=duration \
    -of default=noprint_wrappers=1:nokey=1 \
    "$fixture_work/library/music/music_stale-short-a.m4a"
)
long_duration=$(
  "$FFPROBE_BIN" -v error -show_entries format=duration \
    -of default=noprint_wrappers=1:nokey=1 \
    "$fixture_work/library/music/music_stale-long-b.m4a"
)
awk -v duration="$short_duration" \
  'BEGIN { exit !(duration > 5 && duration < 30) }' ||
  die "short A duration is outside the required window"
awk -v duration="$long_duration" \
  'BEGIN { exit !(duration >= 600) }' ||
  die "long B duration is shorter than the capture bound"

fixture_count=$(
  find "$fixture_work/library" "$fixture_work/outside" -type f -print |
    wc -l | tr -d '[:space:]'
)
[ "$fixture_count" = 19 ] ||
  die "fixture generator produced an unexpected file count"

{
  for period in morning day evening night; do
    period_count=$(
      find "$fixture_work/library/loops" -type f \
        -name "loop_${period}_*.mp4" -print |
        wc -l | tr -d '[:space:]'
    )
    printf 'loops_%s=%s\n' "$period" "$period_count"
  done
  printf 'loops_total=12\n'
  printf 'music_total=2\n'
  printf 'fixture_total=19\n'
  printf 'short_a_duration_seconds=%s\n' "$short_duration"
  printf 'long_b_duration_seconds=%s\n' "$long_duration"
} >"$fixture_work/evidence/fixture-counts.txt"

fixture_sha256() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{ print $1 }'
  elif command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{ print $1 }'
  else
    die "shasum or sha256sum is required"
  fi
}

find "$fixture_work/library" "$fixture_work/outside" -type f -print |
  LC_ALL=C sort |
  while IFS= read -r fixture_path; do
    relative_path=${fixture_path#"$fixture_work"/}
    fixture_size=$(wc -c <"$fixture_path" | tr -d '[:space:]')
    fixture_sha=$(fixture_sha256 "$fixture_path")
    printf '%s\t%s\t%s\n' \
      "$relative_path" "$fixture_size" "$fixture_sha"

    probe_name=$(printf '%s' "$relative_path" | tr '/ ' '__')
    "$FFPROBE_BIN" -v error -show_format -show_streams -of json \
      "$fixture_path" \
      >"$fixture_work/evidence/fixture-probes/$probe_name.json"
  done >"$fixture_work/evidence/fixture-files.tsv"

chmod -R u=rwX,go=rX "$fixture_work"
mv -- "$fixture_work" "$fixture_target"
fixture_work=
trap - EXIT HUP INT TERM
printf '%s\n' "$fixture_target"
