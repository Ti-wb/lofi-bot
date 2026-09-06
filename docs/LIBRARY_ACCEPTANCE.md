# Library-only real-system acceptance checklist

This runbook verifies one library-only release build against a real OBS Studio
instance and a real Telegram Local Bot API Server running with `--local`. It
complements, but does not replace, the automated test suite. A release passes
only when the automated suite and every required real-system item below pass
against the same clean commit.

## Pass rules, scope, and safety

- Use a dedicated bot, Telegram group, OBS profile and scene collection,
  database, media directories, and Telegram Bot API data directory. Never run
  destructive tests against production paths.
- The operator sending admin commands and uploads must be a visible Telegram
  group administrator. Anonymous admin mode is not sufficient.
- Use the recorded production binary for every test except an optional
  operator-supplied clock harness. Fakes for OBS or Telegram do not count.
- Section 1 is configuration qualification. After its final configuration hash
  is recorded, do not edit `.env`, rebuild the binary, change the commit, or
  change the OBS source names/endpoints. Media and database state are expected
  to change during the tests.
- Mark an item `FAIL`, not `N/A`, when required evidence is missing. Start a
  new run after a binary or final-config change.
- Never put an unredacted `.env`, bot token, API hash, OBS password, raw
  Telegram Local Bot API command line, Telegram cache payload, full chat
  transcript/export, or unrelated/private conversation in the evidence
  bundle. Checklist evidence may include only tightly cropped screenshots
  from the dedicated acceptance group, with unrelated messages and
  nonessential identities obscured and every secret/private cache path
  redacted.
- Treat any bot token previously serialized into an OBS Media Source path,
  scene collection, or log as compromised: revoke/rotate it before acceptance
  and remove the secret-bearing legacy path. Never copy that source into the
  dedicated profile or retain the old token as evidence.
- Do not enable shell tracing. Run `set +x` before any stack launch.
- The real malformed-upload smoke test can prove responses and filesystem
  effects, but it cannot reliably prove whether the app called Local Bot API
  `getFile`: Telegram may already have cached the file, and this repository
  does not ship a safe request-audit proxy. The automated upload-preflight test
  is authoritative for the no-`getFile` requirement. Do not collect
  token-bearing HTTP request logs merely to strengthen this run.
- The stale-event test uses the repository's independent,
  credential-redacting `obs-stale-monitor` as a second OBS WebSocket client.
  It does not change or proxy the frozen app endpoint. The event payload still
  does not identify a media generation, so only the permitted two-second
  correlation and deterministic two-track fixture described in section 13
  may be claimed.

## Evidence bootstrap and immutable build provenance

Run these commands from the repository root. They create evidence outside the
checkout, reject a dirty tree, run the automated suite, build once, and record
the exact binary. Keep the resulting shell variables for the whole run.

```sh
set -eu
set +x
repo_root=$(pwd -P)
acceptance_evidence_parent=$(
  CDPATH= cd -- "${TMPDIR:-/tmp}"
  pwd -P
)
case "$acceptance_evidence_parent" in
  "$repo_root"|"$repo_root"/*)
    printf '%s\n' "evidence parent must be outside the checkout" >&2
    exit 1
    ;;
esac
acceptance_run_id=$(date -u +%Y%m%dT%H%M%SZ)
acceptance_evidence_dir=$(
  umask 077
  mktemp -d "$acceptance_evidence_parent/library-acceptance.$acceptance_run_id.XXXXXX"
)
chmod 700 "$acceptance_evidence_dir"
for evidence_section in \
  upgrade startup status obs uploads rejections pagination removed-queue \
  preview periods restart reconnect stale quarantine transition cleanup
do
  mkdir -p "$acceptance_evidence_dir/$evidence_section"
done

git rev-parse HEAD >"$acceptance_evidence_dir/commit.txt"
git status --porcelain=v1 >"$acceptance_evidence_dir/git-status.txt"
if [ -s "$acceptance_evidence_dir/git-status.txt" ]; then
  printf '%s\n' "acceptance requires a clean checkout" >&2
  exit 1
fi
date -u >"$acceptance_evidence_dir/start-utc.txt"
uname -a >"$acceptance_evidence_dir/platform.txt"
go version >"$acceptance_evidence_dir/go-version.txt"
ffmpeg -version >"$acceptance_evidence_dir/ffmpeg-version.txt"
ffprobe -version >"$acceptance_evidence_dir/ffprobe-version.txt"
sqlite3 -version >"$acceptance_evidence_dir/sqlite-version.txt"

go test ./... -count=1 \
  >"$acceptance_evidence_dir/go-test.log" 2>&1
go test -race ./... -count=1 -timeout=2m \
  >"$acceptance_evidence_dir/go-test-race.log" 2>&1
go test -race -p=1 ./... -count=1 -timeout=3m \
  >"$acceptance_evidence_dir/go-test-race-serial.log" 2>&1
go vet ./... >"$acceptance_evidence_dir/go-vet.log" 2>&1
./run.sh test >"$acceptance_evidence_dir/shell-and-default-tests.log" 2>&1
./run.sh build >"$acceptance_evidence_dir/build.log" 2>&1
git status --porcelain=v1 \
  >"$acceptance_evidence_dir/git-status-after-build.txt"
if [ -s "$acceptance_evidence_dir/git-status-after-build.txt" ]; then
  printf '%s\n' "tests/build changed the clean checkout" >&2
  exit 1
fi
binary_path="$repo_root/dist/tg-obs-bot"
test -x "$binary_path"
shasum -a 256 "$binary_path" >"$acceptance_evidence_dir/binary.sha256"
wc -c "$binary_path" >"$acceptance_evidence_dir/binary-size.txt"
if stat -f '%N size=%z mtime=%m mode=%Sp' "$binary_path" \
  >"$acceptance_evidence_dir/binary-stat.txt" 2>/dev/null; then
  :
else
  stat -c '%n size=%s mtime=%Y mode=%A' "$binary_path" \
    >"$acceptance_evidence_dir/binary-stat.txt"
fi
monitor_path="$repo_root/dist/obs-stale-monitor"
test -x "$monitor_path"
shasum -a 256 "$monitor_path" >"$acceptance_evidence_dir/monitor.sha256"
wc -c "$monitor_path" >"$acceptance_evidence_dir/monitor-size.txt"
if stat -f '%N size=%z mtime=%m mode=%Sp' "$monitor_path" \
  >"$acceptance_evidence_dir/monitor-stat.txt" 2>/dev/null; then
  :
else
  stat -c '%n size=%s mtime=%Y mode=%A' "$monitor_path" \
    >"$acceptance_evidence_dir/monitor-stat.txt"
fi
```

The bootstrap fails if `sqlite3`, the tests, or the build is unavailable. Do
not substitute a writable SQLite connection for the read-only evidence
queries below. Record the evidence directory's final approved destination in
`environment.md`; a temporary directory is only a staging location.

Also record in `environment.md`:

- OBS version, profile, scene collection, Program scene, and WebSocket version;
- Telegram Local Bot API Server version and confirmation that `--local` is
  active;
- local timezone and UTC offset;
- absolute `DATA_DIR`, `DATABASE_PATH`, `LOOP_MEDIA_DIR`,
  `MUSIC_MEDIA_DIR`, and `TELEGRAM_BOT_API_DIR`;
- `TELEGRAM_API_BASE_URL` with the token omitted, its configured port, and the
  fact that the authority is loopback;
- source names used for `OBS_LOOP_SOURCE_NAME` and
  `OBS_MUSIC_SOURCE_NAME`;
- whether period transitions use wall-clock waiting or an operator-supplied
  acceptance clock harness;
- the next period's `date_key` and period name used by the read-only SQL
  queries.

Screenshots and recordings must show a clock or have a matching UTC timestamp
in `timeline.tsv`:

```text
utc_time	local_time	step	action	expected	observed	evidence_file	result
```

## 0. Isolated environment, OBS setup, and fixtures

- [ ] Generate the credential-free fixture pack into a new dedicated path.
      The generator refuses an existing destination, creates the exact 19
      positive/rejection files below, validates required streams and stale
      A/B durations, and writes probe/count/hash evidence:

```sh
set -eu
acceptance_fixture_root=/absolute/new/dedicated/path/acceptance-fixtures
./scripts/generate-acceptance-fixtures.sh "$acceptance_fixture_root" \
  >"$acceptance_evidence_dir/fixture-generator-output.txt"
cp "$acceptance_fixture_root/evidence/fixture-counts.txt" \
  "$acceptance_evidence_dir/fixture-counts.txt"
cp "$acceptance_fixture_root/evidence/fixture-files.tsv" \
  "$acceptance_evidence_dir/fixture-files.tsv"
cp -R "$acceptance_fixture_root/evidence/fixture-probes" \
  "$acceptance_evidence_dir/fixture-probes"
```

- [ ] Create dedicated acceptance paths. At least two of the loop, music, and
      Telegram Bot API directories must be on filesystems whose free/total
      values are visibly different at the precision used by `/status`.
      Merely using different mount points with identically displayed values
      does not pass the disk test.
- [ ] Create a dedicated OBS Program scene containing exactly the two
      configured Media Sources. Confirm both source names are distinct and no
      legacy queue source is required.
- [ ] Create both Media Sources from scratch rather than duplicating a legacy
      queue source. Confirm the dedicated scene collection and its new logs
      contain no bot token or Telegram cache path; record only the redacted
      result, never a matching secret.
- [ ] Before the first launch, clear/stop both source files. Set the loop
      source muted and the music source unmuted. Route the music source to the
      intended output/monitor device, set an audible mixer level, and confirm
      no other scene/source is supplying the test audio.
- [ ] Disable source looping in OBS before startup so the test proves the app
      enables it only for the loop source. Leave transform/crop at the desired
      baseline; the app centers the loop but does not resize or clear crop.
- [ ] Preload the generator's exactly 12 valid loop videos, three per period.
      Make every loop
      short enough to observe at least two rewinds during a 35-second window.
      Keep one additional valid loop outside the library; its accepted upload
      creates the 13-item pagination boundary.
- [ ] Give the current period two specifically named loop fixtures, A and B,
      for the runtime OBS-error test. Both must have the same current-period
      classification and be independently playable.
- [ ] Use the generator's exactly two music fixtures for the stale-event test: short A
      and long B. A's probed duration must be strictly longer than five
      seconds and it must end naturally within a practical observation
      window; B must be longer than the entire stale-event attempt. Other
      music may be used elsewhere but will be held outside the library during
      section 13.
- [ ] Keep the generated additional valid music filename outside the library for the
      Telegram upload test.
- [ ] Keep the generated rejection fixtures outside the library: a playable file with an
      invalid library filename, an audio-only file named like
      `loop_day_bad-audio_001.mp4`, and a video-only file renamed to a
      supported music extension such as `music_bad-video.m4a`.
- [ ] Validate the recorded `dist/obs-stale-monitor` build needed by section
      13. It connects independently to the same loopback OBS WebSocket server,
      so the app's endpoint remains unchanged. Save its commit/checksum,
      redaction policy, and exact safe invocation, but no password or
      authentication payload.

Loop names must follow:

```text
loop_<morning|day|evening|night>_<theme>_<variant>.<supported-video-extension>
```

Music names must follow:

```text
music_<track>.<supported-audio-extension>
```

Evidence to save:

- `paths.txt`: `pwd`, all five absolute runtime paths, and `df -k` output for
  the loop, music, and Telegram Bot API directories;
- `obs-scene-before.png`: Program scene, both source names, stopped/cleared
  inputs, mixer, mute state, and audio routing;
- `fixture-files.tsv`: sorted names, sizes, and SHA-256 hashes;
- `fixture-probes/`: `ffprobe -show_format -show_streams -of json` output for
  every positive and negative fixture;
- `fixture-counts.txt`: loop counts by period and total loop/music counts;
- monitor source revision, version, safe invocation, and redaction policy.

## 1. Schema v7 configuration qualification

All `.env` edits and migration checks happen in this section. The application
must not be running yet.

### 1.1 Reject a legacy queue deployment

- [ ] Start from a representative schema v6 acceptance `.env` with the
      dedicated paths, the two source names, and `PLAYER_MODE=queue`.
- [ ] Record its SHA-256 and the existing `.env.backup.*` filenames.
- [ ] Run `./run.sh migrate-env`. It must exit non-zero with instructions to
      configure library sources/media; it must not silently start library
      playback.
- [ ] Confirm the `.env` content hash is unchanged and no new backup exists.
      A permission change to mode `0600` is allowed.
- [ ] Confirm `./run.sh env`, `./run.sh doctor`, and `./run.sh up` also refuse
      this file. OBS must remain unchanged.

Save sanitized output, exit codes, before/after hashes, backup lists, and an
OBS screenshot under `upgrade/queue-*`.

### 1.2 Upgrade an accepted library deployment

- [ ] Change only the deprecated mode to `PLAYER_MODE=library`, then run
      `./run.sh migrate-env`.
- [ ] Confirm one private backup is created, schema becomes
      `ENV_SCHEMA_VERSION=7`, all configured paths remain unchanged, and an
      explicit `DATABASE_PATH` is preserved. If the old key was missing or
      empty, confirm migration writes the historical `DATA_DIR/queue.db`
      value before advancing the schema rather than selecting a new empty
      database. A brand-new schema-v7 config instead defaults to
      `DATA_DIR/state.db`.
- [ ] Confirm migration removes
      `OBS_MEDIA_SOURCE_NAME`, `OBS_FALLBACK_FILE`, `FALLBACK_MODE`,
      `PLAYER_MODE`, `MAX_QUEUE_LENGTH`, `RETENTION_DAYS`,
      `RETENTION_MAX_FILES`, and `RETENTION_DELETE_LOCAL_FILES`.
- [ ] Run migration again. It must be idempotent and create no second backup.
- [ ] Run `./run.sh env` and save its sanitized output. It must show schema
      v7, distinct library source names, dedicated paths, and no playback-queue
      configuration. Redact the chat ID before sharing the evidence bundle.
- [ ] Run `./run.sh doctor`. Explicit whitespace-only or equal loop/music
      source names must fail. Omitted or exactly empty values resolve to the
      documented defaults; the two effective names must still be distinct.
- [ ] Confirm `TELEGRAM_API_BASE_URL` points to the dedicated loopback Local
      Bot API endpoint, not the public Telegram endpoint.
- [ ] Set acceptance `APP_BIN` to the recorded binary's absolute path. This
      prevents `./run.sh up` from selecting or rebuilding a different
      executable. Record this non-secret path in `environment.md`.

### 1.3 Preserve historical `videos` rows

Use a synthetic or sanitized copy of a representative legacy database containing
at least one non-private sentinel `videos` row. Never copy production chat/user
data into this run. Record the full historical table before the first app
launch:

```sh
set -eu
acceptance_database_path=/absolute/path/from-environment-md/state-or-legacy.db
case "$acceptance_database_path" in
  *'?'*|*'#'*|*'%'*)
    printf '%s\n' \
      "static evidence database path must not contain URI characters %, ?, or #" >&2
    exit 1
    ;;
esac
if [ -s "$acceptance_database_path-wal" ]; then
  printf '%s\n' \
    "static evidence requires a stopped, fully checkpointed database" >&2
  exit 1
fi
acceptance_database_immutable_uri="file:$acceptance_database_path?mode=ro&immutable=1"
sqlite3 "$acceptance_database_immutable_uri" \
  ".schema videos" >"$acceptance_evidence_dir/upgrade/videos-before.schema"
sqlite3 -header -csv "$acceptance_database_immutable_uri" \
  "SELECT * FROM videos ORDER BY id;" \
  >"$acceptance_evidence_dir/upgrade/videos-before.csv"
shasum -a 256 \
  "$acceptance_evidence_dir/upgrade/videos-before.schema" \
  "$acceptance_evidence_dir/upgrade/videos-before.csv" \
  >"$acceptance_evidence_dir/upgrade/videos-before.sha256"
```

The `videos` table is historical input only. The release may create library
and Telegram journal tables in the same database, but must not read for
playback, update, or delete historical `videos` rows.

The immutable URI is limited to this stopped, pre-start snapshot. It avoids a
read-only SQLite client trying to create WAL shared-memory state. Never use
`immutable=1` for later queries while the app can write; those sections must
keep their documented live `-readonly` connections. A non-empty `-wal`
sidecar fails this precondition because ignoring uncheckpointed state would
make the evidence incomplete.

### 1.4 Freeze the accepted runtime

Once all accepted values are final:

```sh
set -eu
./run.sh doctor >"$acceptance_evidence_dir/upgrade/doctor-final.txt" 2>&1
./run.sh env >"$acceptance_evidence_dir/upgrade/env-final.private.txt"
shasum -a 256 .env >"$acceptance_evidence_dir/runtime-config.sha256"
shasum -a 256 -c "$acceptance_evidence_dir/binary.sha256"
shasum -a 256 -c "$acceptance_evidence_dir/monitor.sha256"
```

From here onward, the binary and `.env` are immutable. Record any failed
configuration experiments before this hash, not after it.

## 2. Start the real stack and prove fail-closed startup

Use this exact launch pattern for every launch. It captures one complete
stdout/stderr stream without a pipeline and records the root PID separately;
it never captures command arguments.

```sh
set -eu
set +x
printf '=== stack start %s ===\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  >>"$acceptance_evidence_dir/runtime.log"
shasum -a 256 -c "$acceptance_evidence_dir/binary.sha256"
shasum -a 256 -c "$acceptance_evidence_dir/runtime-config.sha256"
./run.sh up >>"$acceptance_evidence_dir/runtime.log" 2>&1 &
acceptance_root_pid=$!
printf '%s\n' "$acceptance_root_pid" \
  >"$acceptance_evidence_dir/current-root.pid"
```

Capture process identity with executable names, never raw arguments:

```sh
set -eu
ps -axo pid=,ppid=,pgid=,comm= \
  >"$acceptance_evidence_dir/processes.safe.txt"
```

From that safe table, set `app_pid` to the single live `tg-obs-bot` process
PID (not the root or service-supervisor PID), then prove the kernel's
canonical executable mapping matches the frozen build. This records only PID
and paths; neither branch reads or emits argv:

```sh
set -eu
app_pid=REPLACE_WITH_TG_OBS_BOT_PID
case "$app_pid" in
  ''|*[!0-9]*)
    printf '%s\n' "app_pid must be one numeric tg-obs-bot PID" >&2
    exit 1
  ;;
esac
kill -0 "$app_pid"
test ! -L "$binary_path"
case "$(uname -s)" in
  Darwin)
    expected_app_dir=$(
      CDPATH= cd -- "${binary_path%/*}" && pwd -P
    )
    expected_app_path=$expected_app_dir/${binary_path##*/}
    app_executable_path=$(
      lsof -a -p "$app_pid" -d txt -Fn |
        awk '
          substr($0, 1, 1) == "n" {
            print substr($0, 2)
            found = 1
            exit
          }
          END {
            if (!found) {
              exit 1
            }
          }
        '
    )
    ;;
  Linux)
    expected_app_path=$(readlink -f -- "$binary_path")
    app_executable_path=$(readlink -f -- "/proc/$app_pid/exe")
    ;;
  *)
    printf '%s\n' "canonical executable proof supports macOS and Linux" >&2
    exit 1
    ;;
esac
test "$app_executable_path" = "$expected_app_path"
{
  printf 'pid=%s\n' "$app_pid"
  printf 'executable=%s\n' "$app_executable_path"
  printf 'expected=%s\n' "$expected_app_path"
} >"$acceptance_evidence_dir/startup/app-executable.safe.txt"
```

Stop a run with:

```sh
set -eu
kill -TERM "$acceptance_root_pid"
if wait "$acceptance_root_pid"; then
  acceptance_exit=0
else
  acceptance_exit=$?
fi
printf '%s\n' "$acceptance_exit" \
  >>"$acceptance_evidence_dir/root-exit-codes.txt"
```

### 2.1 No-current-period startup must stop a stale source

- [ ] Hold every loop for the current local-time period outside
      `LOOP_MEDIA_DIR`; leave other-period loops present.
- [ ] In OBS, set the configured loop source to a known stale or other-period
      file and start it manually.
- [ ] Launch the frozen stack. Do not send `/status` or `/preview` yet.
- [ ] Within startup recovery plus two scheduler cycles, confirm the app stops
      the loop source. It must not continue the stale file and must not choose
      an asset from the wrong period.
- [ ] Send `/now`. A clear no-playable-current-period error is acceptable and
      must not restart the app. Music may continue independently.
- [ ] Gracefully stop the stack, restore the current-period fixtures, and
      leave the OBS loop source stopped for the healthy launch.

Save before/after OBS source state and path, `/now`, safe process tables, PID,
and the timestamped runtime excerpt.

### 2.2 Healthy launch and configured Local Bot API endpoint

- [ ] Launch again with the restored fixtures and the same frozen binary and
      config.
- [ ] Run `./run.sh health`. It proves that `/getMe` succeeds at the configured
      `TELEGRAM_API_BASE_URL`; by itself it does not prove which process owns
      the listener.
- [ ] Confirm the configured URL is loopback. Use `lsof -nP` for its configured
      port and match the listening PID/executable to the dedicated Local Bot
      API process in the safe process table.
- [ ] After the app is healthy, verify the binary and config checksums again.
      Confirm the startup log contains no build/rebuild message and the app
      executable is the recorded `APP_BIN`, using the canonical OS-specific
      executable proof above rather than only the process basename.
- [ ] Send `/now`, but do not send `/status` or `/preview` before section 2.3.
      The bot must respond in the dedicated group and report current-period
      loop playback.
- [ ] Confirm the active OBS loop path is under `LOOP_MEDIA_DIR`, not any
      historical `videos.local_path`.
- [ ] Re-run the read-only historical-table captures and compare them exactly:

```sh
set -eu
sqlite3 -readonly "$acceptance_database_path" \
  ".schema videos" >"$acceptance_evidence_dir/upgrade/videos-after.schema"
sqlite3 -readonly -header -csv "$acceptance_database_path" \
  "SELECT * FROM videos ORDER BY id;" \
  >"$acceptance_evidence_dir/upgrade/videos-after.csv"
cmp "$acceptance_evidence_dir/upgrade/videos-before.schema" \
  "$acceptance_evidence_dir/upgrade/videos-after.schema"
cmp "$acceptance_evidence_dir/upgrade/videos-before.csv" \
  "$acceptance_evidence_dir/upgrade/videos-after.csv"
```

Save `startup/health.txt`, the listener evidence, `/now`, OBS path, comparison
exit codes, and the startup portion of `runtime.log`. The first accepted upload
in section 4 supplies additional evidence that Local Bot API `--local`
materializes an absolute local file.

### 2.3 First `/status` materializes the next-period plan

This must be the run's first `/status` or `/preview`. Leave enough time before
the next boundary to complete section 9, and execute section 9 immediately
after this subsection before continuing with the intervening functional tests.

- [ ] Set the recorded next date and period, then confirm no matching row
      exists:

```sh
set -eu
acceptance_next_date=YYYY-MM-DD
acceptance_next_period=morning
sqlite3 -readonly "$acceptance_database_path" \
  "SELECT COUNT(*)
   FROM library_period_plans
   WHERE date_key='$acceptance_next_date'
     AND period='$acceptance_next_period';" \
  >"$acceptance_evidence_dir/status/next-plan-before.count"
test "$(tr -d '\r\n' \
  <"$acceptance_evidence_dir/status/next-plan-before.count")" = 0
sqlite3 -readonly -header -csv "$acceptance_database_path" \
  "SELECT date_key,period,theme,loop_id,updated_at
   FROM library_period_plans
   WHERE date_key='$acceptance_next_date'
     AND period='$acceptance_next_period';" \
  >"$acceptance_evidence_dir/status/next-plan-before.csv"
```

- [ ] Send `/status`. It must show a concrete `Next` preview and thereby
      persist exactly one next-period plan.
- [ ] Run the same read-only query into `next-plan-after.csv`. It must contain
      exactly one data row matching the preview.
- [ ] Send `/status` again. The `Next` asset and row must remain unchanged;
      no duplicate row may appear.

The explicit count check avoids treating a header-only CSV as a data row.
Record both the count and CSV. This section makes explicit that `/status` is a
materializing read for the next-period plan.

## 3. Two-source OBS behavior

- [ ] Confirm the loop source is under `LOOP_MEDIA_DIR`, playing with looping
      enabled by the app, muted, and centered in the Program scene without an
      unexpected scale/crop change.
- [ ] Confirm the music source is under `MUSIC_MEDIA_DIR`, playing without
      source looping, unmuted, routed to the intended output, and audible.
- [ ] Confirm source names and paths are distinct and no queue-era source is
      controlled.
- [ ] Compare OBS paths with `/now`. The loop ID/filename and music filename
      must match. `/now` does not expose a music asset ID, so do not claim that
      it does.

Save the Program scene, both source properties, mixer/audio routing, `/now`,
and source paths from OBS properties or a credential-redacting WebSocket
inspector.

## 4. Admin loop and music uploads

Send media as Telegram documents/files so exact names are retained.

- [ ] As a current group administrator, upload one valid not-yet-present loop
      and one valid not-yet-present music file.
- [ ] Confirm both responses say the asset was imported and offer library
      actions.
- [ ] Confirm each destination is under the correct library directory, has the
      uploaded SHA-256, and is not a reference to the Telegram cache path.
- [ ] Run `/scan`, `/library 1`, and `/status`. Counts must increase. The loop
      must appear with an addressable loop ID in `/library`; music is verified
      by filename/count and by `/skip music`, not by a nonexistent music-ID
      listing.
- [ ] Remove the operator's admin role and immediately attempt another valid
      upload. It must be rejected before import despite a previous positive
      admin result. Restore the role afterward.
- [ ] Confirm `.tg-obs-bot-staging` directories are empty after each attempt.

Save Telegram screenshots, before/after filenames/counts/hashes, destination
paths, scan/library/status output, and staging listings. Do not save a raw
Local Bot API request log.

## 5. Upload and operator-scan rejection

### 5.1 Telegram upload rejection

- [ ] Upload a playable file with an invalid library filename. It must be
      rejected during preflight and create no library destination or staging
      file.
- [ ] Upload the audio-only valid-loop-name fixture. It must be rejected for
      lacking a video stream.
- [ ] Upload the video-only supported-music-name fixture. It must be rejected
      for lacking an audio stream.
- [ ] Confirm the app does not delete Telegram-owned cache files associated
      with content-validation failures.
- [ ] Send `/status`; the failure must be observable without exposing a token
      or private cache path.

The bootstrap automated-test log, including
`TestUploadPreflightRejectionAvoidsGetFile`, is the authoritative evidence
that malformed names do not invoke `getFile`. The real smoke evidence is
limited to safe responses, destination/staging manifests, and cache ownership
effects; absence of a cache change is not treated as proof of no `getFile`.

### 5.2 Invalid files placed directly by an operator

- [ ] Copy the invalid-name, audio-only loop, and video-only music fixtures
      directly into their isolated library directories.
- [ ] Record healthy counts and current OBS paths, then send `/scan`.
- [ ] The scan must report a partial/structured warning, exclude every invalid
      file from playable counts, keep valid assets available, and keep the app
      alive. `/status` must show rejected/scan-warning state without leaking
      absolute private cache paths.
- [ ] Remove only those named fixtures, run `/scan` again, and confirm the
      warning/rejected count clears while healthy playback continues.

Save before/after directory manifests, `/scan`, `/status`, `/now`, process
identity, and empty staging listings.

## 6. Pagination and callbacks

The page size is 12. Use exactly 13 visible loop assets; music count does not
determine loop page count.

- [ ] `/library 1` shows `1/2`, exactly 12 loop rows, refresh and next, but no
      previous action.
- [ ] `/library 2` shows `2/2`, exactly one loop row, refresh and previous, but
      no next action.
- [ ] Inline next, previous, and refresh edit the existing message and preserve
      bounds.
- [ ] `/library 0`, `/library -1`, and `/library 3` return friendly bounds
      errors without a restart.
- [ ] While page 2 remains visible, hold one fixture outside the library and
      `/scan` down to 12 loops. Refresh stale page 2; it must return a clear
      out-of-range result without a stale asset action or crash.
- [ ] `/library 1` now shows `1/1`. Restore the fixture and `/scan`.

Save screenshots, manifests, scan responses, and safe process tables before
and after.

## 7. Removed playback-queue surface

- [ ] Record `/now`, OBS paths, and a read-only `videos` table capture.
- [ ] Send `/queue`, `/list`, `/history`, `/remove 1`, and `/move 1 2`. Each
      must return the current unknown-command behavior.
- [ ] Send bare `/skip`. It must return library-only usage guidance requiring
      `loop` or `music`, not perform the removed queue-style skip.
- [ ] None of those commands may mutate playback or historical `videos` rows.
- [ ] Confirm the Telegram command menu contains library-only commands and
      omits queue/list/history/remove/move.
- [ ] If a dedicated old test message containing a stale queue callback is
      available, tap it and confirm it has no side effect. Do not require or
      manufacture such a message in a production chat.
- [ ] Confirm `/skip loop` and `/skip music` remain valid, distinct library
      actions.
- [ ] Re-capture `/now`, OBS paths, and `videos`; compare the historical table
      exactly.

Save command responses, command menu, before/after playback, table comparisons,
and PID evidence.

## 8. Status and filesystem reporting

- [ ] Send healthy `/status`. It must report OBS, validated-snapshot and
      currently-playable library counts, current loop/music filenames,
      materialized next preview, override/error state, rejected/quarantined
      counts, and separate loop, music, and Telegram Bot API disk values.
- [ ] Immediately bracket the message with `df -k` readings for the exact
      paths. Differences are allowed only for documented display rounding and
      intervening writes.
- [ ] Confirm at least two values remain visibly distinct at `/status`
      precision; otherwise this test has not proved independent probing.
- [ ] Temporarily make OBS unavailable and send `/status`. It must report
      disconnected while retaining library, preview, and disk information.
      Restore OBS before continuing.

Save both statuses, `df` before/after, and a comparison table mapping every
displayed value to its path, filesystem, raw value, display conversion, and
rounding difference.

## 9. Preview stability and restart persistence

- [ ] At least three minutes before a boundary, compare `/preview` with the
      plan already materialized by the first `/status`. Next period, theme,
      filename, and loop ID must match.
- [ ] Send `/preview` again; it must be identical.
- [ ] Query the database read-only and confirm exactly one matching
      `library_period_plans` row:

```sh
set -eu
sqlite3 -readonly -header -csv "$acceptance_database_path" \
  "SELECT date_key,period,theme,loop_id,updated_at
   FROM library_period_plans
   WHERE date_key='$acceptance_next_date'
     AND period='$acceptance_next_period';" \
  >"$acceptance_evidence_dir/preview/period-plan-before-restart.csv"
```

- [ ] Gracefully stop with the section 2 command, verify the binary and config
      checksums, then relaunch with the exact section 2 launch pattern.
- [ ] Send `/preview` a third time. It and the row must remain unchanged.
- [ ] At the boundary, allow scheduler reconciliation and send `/now`. The
      actual loop ID/path must match the persisted preview. Removing that
      asset invalidates the item and requires a rerun.

Save all previews, SQL results, shutdown/restart timestamps and PIDs, checksum
verification, post-boundary `/now`, and OBS paths.

## 10. All four local-time periods and short-loop rewinds

The production binary has no runtime clock override:

| Period | Local interval | Boundary into period |
| --- | --- | --- |
| morning | 06:00-10:59 | 06:00 |
| day | 11:00-16:59 | 11:00 |
| evening | 17:00-20:59 | 17:00 |
| night | 21:00-05:59 | 21:00 |

For every row:

- [ ] Capture `/preview` immediately before the boundary.
- [ ] After the boundary, allow at least 30 seconds, then capture `/now`,
      `/status`, and the OBS loop path.
- [ ] Confirm period, exclusive end time, filename period, loop ID, persisted
      preview, and OBS path agree.
- [ ] Observe the loop input's media cursor at a cadence shorter than the
      fixture duration for at least 35 seconds. Save timestamps, cursor
      position, duration, state, and path. The same path must remain selected
      while the cursor decreases on at least two completed rewinds.
- [ ] Confirm music continues independently and those loop rewinds do not
      redraw the period plan.

An operator-supplied clock harness may replace wall-clock waiting only if it
injects the existing application clock dependency while retaining real OBS
and Telegram connections. Record its source revision and invocation. This
repository does not include such a harness. Changing the host OS clock is not
recommended. A fake-OBS or fake-Telegram unit test does not satisfy this
section.

Save per-period preview/now/status/OBS screenshots, cursor samples, a short
recording containing at least two rewinds, runtime excerpts, and any external
clock-harness provenance.

## 11. Graceful restart behavior

Run away from a period boundary.

- [ ] Record `/now`, `/status`, current plan row, `last_music_id` row, and safe
      process identities:

```sh
set -eu
sqlite3 -readonly -header -csv "$acceptance_database_path" \
  "SELECT key,value,updated_at
   FROM library_kv WHERE key='last_music_id';" \
  >"$acceptance_evidence_dir/restart/last-music-before.csv"
```

- [ ] Stop the root process with `TERM` using section 2's command. Confirm app
      and Local Bot API child process groups drain.
- [ ] Verify the binary/config checksums, then relaunch with the exact section
      2 pattern.
- [ ] Confirm the current-period loop ID is unchanged and both OBS sources
      recover. With multiple music tracks, startup may choose a different
      music filename to avoid immediately repeating persisted last music; it
      must not lose playback. Do not claim `/now` exposes a music ID.
- [ ] Confirm new Telegram updates are processed once and `/status` is
      healthy.

Save before/after database rows, OBS paths, `/now`, `/status`, safe process
tables, shutdown/restart logs, and one unique post-restart command.

## 12. OBS disconnect and reconnect

- [ ] Make long music B active, then record loop ID, music filename, both OBS
      paths, and app PID. B must not end naturally during this section.
- [ ] Disable OBS WebSocket or close OBS without stopping the app.
- [ ] Confirm `/status` reports disconnected and the app PID remains alive.
- [ ] Restore OBS with the same scene collection.
- [ ] Confirm `/status` reconnects without an app restart and the persisted
      loop is replayed if needed. Both sources must return healthy; a source
      that stayed healthy must not advance unnecessarily.

Save statuses, paths, PIDs, timestamped reconnect logs, and a short recording.

## 13. Stale OBS ended-event correlation

The event payload identifies only the input name, not file path or playback
generation. This section therefore tests a bounded correlation, not direct
generation identity.

- [ ] Hold all music except short A and long B outside `MUSIC_MEDIA_DIR`, run
      `/scan`, and prove exactly A/B are visible.
- [ ] Outside the evidence bundle, prepare a mode-`0600` file containing the
      exact OBS WebSocket password bytes with no trailing newline. Use an
      empty file when OBS authentication is disabled. Do not pass the password
      in argv or an environment variable.
- [ ] Start the credential-redacting monitor with the commands below. The
      8-minute capture bound is shorter than the generated 600-second B
      fixture. The trace is a new mode-`0600` file and records only the safe
      input name, exact A/B basenames or `other`, fixed event/action labels,
      and numeric timing:

```sh
set -eu
set +x
acceptance_obs_password_file=/private/path/outside/evidence/obs-password.stdin
test -f "$acceptance_obs_password_file"
test ! -L "$acceptance_obs_password_file"
if acceptance_password_mode=$(stat -f '%Lp' \
  "$acceptance_obs_password_file" 2>/dev/null); then
  :
else
  acceptance_password_mode=$(stat -c '%a' "$acceptance_obs_password_file")
fi
test "$acceptance_password_mode" = 600
test ! -e "$acceptance_evidence_dir/stale/obs-stale.jsonl"

"$monitor_path" capture \
  --password-stdin \
  --host 127.0.0.1 \
  --port 4455 \
  --input tg_music_player \
  --a-basename music_stale-short-a.m4a \
  --b-basename music_stale-long-b.m4a \
  --output "$acceptance_evidence_dir/stale/obs-stale.jsonl" \
  --max-bytes 1048576 \
  --max-duration 8m \
  <"$acceptance_obs_password_file" \
  >"$acceptance_evidence_dir/stale/monitor.stdout" \
  2>"$acceptance_evidence_dir/stale/monitor.stderr" &
acceptance_monitor_pid=$!

acceptance_monitor_wait=0
until grep -Fqx 'READY' \
  "$acceptance_evidence_dir/stale/monitor.stdout" 2>/dev/null
do
  if ! kill -0 "$acceptance_monitor_pid" 2>/dev/null; then
    wait "$acceptance_monitor_pid" || true
    printf '%s\n' "OBS evidence monitor stopped before READY" >&2
    exit 1
  fi
  acceptance_monitor_wait=$((acceptance_monitor_wait + 1))
  if [ "$acceptance_monitor_wait" -ge 10 ]; then
    kill -TERM "$acceptance_monitor_pid" 2>/dev/null || true
    wait "$acceptance_monitor_pid" || true
    printf '%s\n' "OBS evidence monitor did not become ready" >&2
    exit 1
  fi
  sleep 1
done
```

Use the actual frozen loopback host, port, and music source name when they
differ; record those non-secret substitutions. The monitor requires
obs-websocket 5.4 or newer and refuses non-loopback hosts.
- [ ] Make A active. With exactly two tracks, one `/skip music` from B selects
      A deterministically.
- [ ] Near A's natural end, send `/skip music`. With exactly A/B, B must become
      active.
- [ ] Repeat until the trace shows: A active; source changes to B; an ended
      event for the same input arrives less than two seconds later; and B
      remains current. Because B is longer than the complete attempt, the
      event is correlated to A rather than B's natural end.
- [ ] At the event timestamp, capture/query the OBS music path. Confirm `/now`
      and OBS remain on B, no third advance occurs, and app PID is unchanged.
- [ ] After the immediate and two-second settled snapshots have been recorded,
      stop and verify the monitor. Verification must emit the exact bounded
      conclusion and no raw path or credential:

```sh
set -eu
kill -TERM "$acceptance_monitor_pid"
wait "$acceptance_monitor_pid"
test ! -s "$acceptance_evidence_dir/stale/monitor.stderr"
"$monitor_path" verify \
  --trace "$acceptance_evidence_dir/stale/obs-stale.jsonl" \
  >"$acceptance_evidence_dir/stale/verdict.json"
grep -Fq \
  '"conclusion":"bounded correlation consistent with superseded A"' \
  "$acceptance_evidence_dir/stale/verdict.json"
```

- [ ] After the two-second settle window, send `/skip music` so A becomes
      current, let A end naturally without another skip, and confirm normal
      end handling advances to B.
- [ ] Restore any held music and `/scan`.

If the event arrives at or beyond two seconds, B is not provably longer than
the attempt, the source-change/event ordering is missing, or credentials were
captured, mark the item `FAIL`. The monitor cannot honestly label the event's
generation from the payload; the deterministic fixtures and timing supply the
correlation.

Save the redacted JSONL and verdict, monitor checksum/invocation, A/B
durations, skip and `/now` screenshots, OBS path snapshots, PIDs, runtime
excerpts, and a recording. The verdict proves only a bounded correlation
consistent with superseded A; it must not be described as direct playback
generation identity.

## 14. Runtime OBS error, quarantine, and recovery

Use current-period loop fixtures A and B with no direct/theme override.

- [ ] Hold B outside the loop directory, keep A present, run `/scan`, clear
      overrides with `/select clear` and `/theme random`, then `/skip loop`.
      Prove A is the only current-period choice and is active.
- [ ] Restore valid B and `/scan`; A must remain active while B becomes an
      alternate.
- [ ] Back up A outside the library, atomically replace A at the same filename
      with non-empty invalid bytes, and use the OBS media-source restart
      control to make the real input enter its error state. Do not run `/scan`
      while inducing this runtime failure.
- [ ] Capture the real OBS error state. Within reconciliation/retry bounds, A
      must be quarantined, B must become active, `/status` must show the
      quarantine/error, and the app PID must remain unchanged.
- [ ] Atomically restore valid A with a changed file identity, run `/scan`, and
      confirm the quarantine clears and playable counts recover.
- [ ] Use A's loop ID with `/select` to prove restored A is playable, then
      `/select clear`.
- [ ] Restore the normal current-period fixture set.

Save hashes/stamps of A before corruption, corrupt state, and restoration;
OBS error and recovery evidence; statuses; paths; scan/select responses; and
PIDs.

## 15. Fail closed at a period transition with no candidate

This explicitly verifies that an expired loop does not leak into the next
period.

- [ ] Before one boundary, hold every loop for the upcoming period outside the
      library and run `/scan`. Keep the current period's loop active.
- [ ] At the boundary, wait two scheduler cycles. The configured loop source
      must stop; it must not continue the expired loop or select another
      period's asset. Music may continue.
- [ ] `/now` or `/status` must expose the no-playable-upcoming-period error
      without restarting the app.
- [ ] Restore the upcoming-period fixtures and `/scan`. The correct-period
      loop must begin and status must recover.
- [ ] Rerun that period's normal section 10 evidence after restoration.

Wall-clock waiting or the same fully recorded operator-supplied real-system
clock harness from section 10 is required. A unit test alone does not pass
this item.

## 16. Cleanup and finalization

- [ ] Send final `/status`, `/now`, and `/library 1`; save responses.
- [ ] Gracefully stop the root process and confirm no acceptance app or Local
      Bot API child remains. OBS must receive no further source changes.
- [ ] Record final read-only database table counts, historical `videos`
      comparison, media/cache manifests, file modes, and empty staging
      listings.
- [ ] Copy the OBS log and a redacted runtime log. Keep the private original
      only in the approved restricted evidence store.
- [ ] Restore the original OBS profile/scene collection and remove only the
      dedicated acceptance sources.
- [ ] Restore the pre-test `.env` only from an explicitly named backup and
      only after all final checksum comparisons.
- [ ] Remove the private OBS-password stdin file with the approved secret
      cleanup workflow after confirming its exact path is outside the evidence
      bundle. Never archive it with the run.
- [ ] Archive or move dedicated runtime paths. Delete them only after proving
      each resolved path is inside the dedicated acceptance root. Never
      recursively delete a repository root, production path, home directory,
      or unresolved variable.
- [ ] Review all evidence for secrets and private cache/chat payloads.
- [ ] Complete `RESULTS.md` with one `PASS` or `FAIL` for every numbered
      section and explain every failure.
- [ ] Verify the frozen artifacts:

```sh
set -eu
shasum -a 256 -c "$acceptance_evidence_dir/binary.sha256"
shasum -a 256 -c "$acceptance_evidence_dir/monitor.sha256"
shasum -a 256 .env >"$acceptance_evidence_dir/runtime-config-final.sha256"
cmp "$acceptance_evidence_dir/runtime-config.sha256" \
  "$acceptance_evidence_dir/runtime-config-final.sha256"
git status --porcelain=v1 \
  >"$acceptance_evidence_dir/git-status-final.txt"
test ! -s "$acceptance_evidence_dir/git-status-final.txt"
```

- [ ] Generate an evidence manifest, make the bundle read-only or upload it to
      the approved restricted store, and record that final location.

## Release sign-off

Record:

```text
Commit:
Binary path/size/mtime/checksum:
Runtime config checksum:
Run ID:
Operator:
Reviewer:
OBS version:
Telegram Local Bot API version:
Timezone:
Start/end UTC:
Period method: wall-clock | operator-supplied clock harness
Stale-event monitor checksum:
Result: PASS | FAIL
Known deviations:
Evidence location:
```

The release is accepted only when every required item is traceable to saved
evidence, the automated suite passed before the recorded build, the binary and
final configuration stayed unchanged, all four periods and both fail-closed
cases were exercised, the stale event met the bounded correlation rule, and
cleanup left no acceptance process or asset in production.
