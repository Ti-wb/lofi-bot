# Library-only migration issues

This file is the source-of-truth checklist for removing the legacy Telegram
playback queue. GitHub issues should mirror these IDs and acceptance criteria.
Historical `videos` rows and Telegram-owned cache files are migration input,
not data to delete automatically.

Implementation status was re-audited on 2026-07-31 after merging the latest
`main`. LOFI-LIB-001 through LOFI-LIB-007 are complete. The audit found four
additional repository-side gaps, tracked as LOFI-LIB-009 through
LOFI-LIB-012; their implementations and full automated verification are now
complete. The real-system checklist in `docs/LIBRARY_ACCEPTANCE.md` has not
been executed, so LOFI-LIB-008 and production release acceptance remain
pending regardless of repository test results.

GitHub mirror status: pending. A read-only search of `Ti-wb/lofi-bot` on
2026-07-31 found no issue containing `LOFI-LIB`. Creating the external issues
still requires explicit authorization; this file remains authoritative until
the mirror exists.

## LOFI-LIB-001 — Remove legacy queue playback runtime and persistence model

Status: complete

- [x] Application startup always initializes the media library and its state.
- [x] The only playback worker is the library scheduler; the stable liveness
      wire ID remains unchanged.
- [x] OBS reconnects and media events always use library reconciliation.
- [x] Queue enqueue/advance/skip/remove/move/list/history, fallback playback,
      queue watchdog, and queue retention paths are unreachable and removed.
- [x] Telegram update journaling is separated from the playback queue model.
- [x] Existing SQLite files open without playing, changing, or deleting legacy
      `videos` rows.
- [x] Queue-domain tests are removed without reducing journal/liveness coverage.

## LOFI-LIB-002 — Simplify Telegram commands and uploads

Status: complete

- [x] Help, command scopes, callbacks, and keyboards expose only library
      operations.
- [x] `/queue`, `/list`, `/history`, `/remove`, `/move`, legacy queue `/skip`,
      queue parsing, and queue hooks are removed; stale callbacks have no side
      effects.
- [x] `/skip loop` and `/skip music` retain fresh-admin authorization.
- [x] Every video/audio/document upload uses library naming, preflight, and
      import rules after a fresh-admin lookup.
- [x] Pagination command/callback bounds and authorization are tested.

## LOFI-LIB-003 — Align config, env migration, deployment, and docs

Status: complete

- [x] `PLAYER_MODE` is no longer a runtime selector.
- [x] Queue-only configuration is removed: `OBS_MEDIA_SOURCE_NAME`,
      `OBS_FALLBACK_FILE`, `FALLBACK_MODE`, `MAX_QUEUE_LENGTH`,
      `RETENTION_DAYS`, `RETENTION_MAX_FILES`, and
      `RETENTION_DELETE_LOCAL_FILES`.
- [x] Effective `OBS_LOOP_SOURCE_NAME` and `OBS_MUSIC_SOURCE_NAME` values are
      always nonblank and distinct; omitted/exact-empty values use documented
      defaults, while whitespace-only values are rejected.
- [x] Env schema v7 accepts deprecated `PLAYER_MODE=library` for one migration
      window but fails closed with an actionable error for
      `PLAYER_MODE=queue`.
- [x] Migration is idempotent, preserves explicit paths, pins pre-v7 omitted
      paths to the compatible `queue.db`, gives fresh configs the neutral
      `state.db` default, and safely quotes derived paths containing spaces.
- [x] `doctor`, `env`, `.env.example`, README, architecture, operations, and
      deployment docs describe one library-only contract.

## LOFI-LIB-004 — Validate playable media and fail over unhealthy assets

Status: complete

- [x] Scanner admits only non-empty regular files; symlinks, FIFOs, devices,
      empty files, and malformed assets remain visible as scan issues.
- [x] Loop validation requires a video stream; music validation requires an
      audio stream.
- [x] Imported and operator-added files use equivalent validation.
- [x] Runtime failures quarantine/exclude a bad asset and select another
      matching candidate with bounded retries.
- [x] A healthy persisted plan remains stable, while a bad plan cannot pin a
      period indefinitely.
- [x] Degraded/no-candidate state is visible through `/status`.

## LOFI-LIB-005 — Publish imports without overwriting concurrent destinations

Status: complete

- [x] Final publication is atomic and fails if the destination exists.
- [x] A destination created by an external writer during copy/probe remains
      unchanged.
- [x] Collision/cancellation/validation failures remove staging residue.
- [x] Same-filesystem staging, source-identity binding, file/directory sync,
      capacity, and disk-headroom guarantees remain intact.
- [x] Private staging files are made `0644` through their open descriptor
      before publication, so an OBS process running under another OS user or
      container UID can read imported media without exposing the staging
      directory.
- [x] A deterministic race regression test covers no-replace publication.

## LOFI-LIB-006 — Complete status, pagination, and observability

Status: complete

- [x] `/status` reports the distinct loop, music, and Telegram Bot API
      filesystems accurately.
- [x] Pagination metadata is structured and is not parsed from localized text.
- [x] Tests cover 12/13-item boundaries, first/last/invalid pages, callback
      refresh, and page-count shrink.
- [x] Library responses stay within Telegram message limits with maximum-length
      names.
- [x] Public status and scan diagnostics expose bounded counts and recovery
      guidance without leaking absolute, relative, spaced, or joined
      filesystem paths and raw operating-system errors.
- [x] README and operations docs match `/library [page]` and the actual inline
      actions.

## LOFI-LIB-007 — Harden scheduler and persistent state lifecycle

Status: complete

- [x] Filesystem enumeration and ffprobe validation run outside the playback
      lock; scans are serialized and cancellable with real progress
      checkpoints, and only the newest complete scan publishes atomically.
      Canceled, unreadable, or over-capacity scans preserve the
      last-known-good snapshot.
- [x] Short looping media sampled at the same cursor phase receives a bounded
      non-harmonic confirmation sample, so healthy rewinds remain healthy and
      a phase-locked frozen cursor still fails closed.
- [x] Stalled and asset-specific OBS failures quarantine the rejected asset
      and try a bounded same-period/track alternate; a source-wide RPC failure
      rolls back tentative quarantine instead of poisoning the whole library.
- [x] Forward and backward wall-clock changes update the active period and end
      time correctly, while media stall timing retains monotonic-clock
      semantics.
- [x] IANA spring-forward/fall-back boundaries, repeated/skipped local hours,
      night plan dates, expiry, and restart persistence are covered.
- [x] Scheduler retries are capped at the next period boundary, and a removed
      or newly unhealthy active loop fails closed even within the same period.
- [x] Each OBS activation/reconciliation has one overall operation deadline;
      possible partial source mutations share one bounded fail-closed budget,
      and canceled playback-lock waiters cannot start new cleanup work during
      shutdown.
- [x] Failed bounded candidate selection restores its exact durable
      override/plan snapshot, while serialized operator commands cannot be
      overwritten by an older scheduler rollback or use a stale midnight date.
- [x] Last-music persistence failures are observable and retryable, do not
      mask OBS reconciliation, and cannot immediately repeat the unpersisted
      current track.
- [x] Midnight override expiry clears the prior override and its night plan in
      one transaction, including rollback on the second delete failing.
- [x] A reconnect with no healthy music stops any OBS music left by a previous
      process instead of reporting an empty active track while it still plays.
- [x] Old period plans and overrides are pruned in bounded maintenance batches
      without touching current/next state.
- [x] Missing current-period media has one explicit fail-closed behavior; the
      scheduler never silently selects another period's loop.

## LOFI-LIB-008 — Add full regression and end-to-end acceptance coverage

Status: pending real-system validation

- [x] Automated integration covers startup, library upload/control, OBS source
      configuration, period transition, restart persistence, reconnect, stale
      events, and recovery.
- [x] Upgrade tests cover representative legacy env/database states without
      destructive data loss.
- [x] Queue-only fixtures are removed while journal poison/replay/cancellation
      coverage remains intact.
- [x] A repeatable real OBS + Telegram Local Bot API smoke checklist records
      expected evidence.
- [x] Full Go tests, shell tests, race tests, vet, and production build pass.
- [ ] Freeze these changes in a clean commit and record the one production
      binary/config pair used throughout acceptance. The current worktree is
      intentionally still uncommitted.
- [ ] Publish the clean branch and update open PR #3 before review. Its
      2026-07-31 GitHub metadata still points to the older `6091b81` head and
      incorrectly says `PLAYER_MODE=queue` remains available; the current
      local branch is library-only and has the latest `main` merged.
- [ ] Provision the dedicated acceptance environment: Local Bot API binary and
      private `.env`, dedicated bot/group, OBS profile/scene collection, media
      fixtures, database, and isolated runtime paths. The 2026-07-31 local
      prerequisite audit found OBS, ffprobe, and sqlite3. It also built the
      official ignored `dist/telegram-bot-api` release from upstream commit
      `adfd7f6a8e990272851777eeb3ae0def4216f161` (Bot API 10.2, SHA-256
      `8937553349c723b2684dc22c27275ede7009d24899def76007e13a856a4a7956`).
      OBS Studio 32.1.2 is installed and its WebSocket plugin is configured,
      but OBS is not running and the current scene collection does not contain
      the required `tg_loop_player` or `tg_music_player` sources. Four isolated
      launch diagnostics started the WebSocket server successfully but the
      frontend stopped immediately after clearing scene data, exposed no OBS
      window, and kept returning WebSocket status 207 (`OBS is not ready`);
      explicit profile/collection selection and removing the bundled-plugin
      restriction did not change the result. A one-line configuration
      hypothesis was tested and fully restored to its pre-test SHA-256, and
      every diagnostic OBS process was stopped. The dedicated acceptance
      profile/collection therefore still requires a normal interactive OBS
      launch before automation can configure it. A tracked, credential-free
      generator now recreates exactly 12 short loops (three per period),
      short/long stale-event music, separate valid upload media, and the three
      required rejection cases, with stream/duration validation plus ffprobe,
      size, and SHA-256 evidence for all 19 files. A preliminary ignored pack
      remains under `data/acceptance-fixtures`, but the final run must
      regenerate it from the frozen commit. The pack is installed into an
      ignored isolated runtime with a synthetic legacy `videos` sentinel
      database; music uses a separate 512 MiB sparse APFS volume so `/status`
      reports visibly distinct loop/music capacity. A
      production-binary/loopback-stub smoke returned
      Loop 12, Music 2, Playable 12/2, materialized one next-period plan,
      exited cleanly on `TERM`, and left the historical `videos` schema/row
      hashes unchanged. A credential-scanned preliminary evidence bundle was
      generated outside the checkout. An ignored credential-free pending env
      template exists, but a populated private `.env`, dedicated bot/group,
      and OBS acceptance profile/scene collection are still absent.
- [ ] Revoke/rotate the Telegram bot token that was serialized in the legacy
      OBS queue Media Source path, remove that secret-bearing path from the
      old scene collection, and create the dedicated sources from scratch.
      The exposed credential must not be reused for acceptance or copied into
      evidence.
- [x] Ship a bounded, credential-redacting, second-client OBS monitor and
      strict verifier for the permitted stale ended-event correlation.
- [ ] Record and execute that monitor against the dedicated real OBS instance;
      automated fake-OBS tests do not satisfy release acceptance.
- [ ] Execute the complete real-system checklist against a dedicated OBS Studio
      instance and Telegram Local Bot API Server, then retain its redacted
      evidence bundle.

## LOFI-LIB-009 — Remove legacy queue naming from fresh persistent state

Status: complete

- [x] Fresh schema-v7 configurations default to `DATA_DIR/state.db`.
- [x] Schema-v6-or-older configs with an explicit `DATABASE_PATH` preserve it
      exactly.
- [x] Schema-v6-or-older configs with a missing or empty `DATABASE_PATH`
      materialize their historical `DATA_DIR/queue.db` path before the schema
      is advanced, without filesystem guessing.
- [x] Direct Go config loading applies the same schema-aware defaults and
      rejects malformed or future schema versions.
- [x] Migration remains idempotent and `PLAYER_MODE=queue` still fails before
      any config write.
- [x] Docs, `.env.example`, shell tests, and supervisor fixtures describe the
      same schema-v7 contract.

## LOFI-LIB-010 — Bind validation and quarantine to actual file identity

Status: complete

- [x] Validation-cache and quarantine stamps distinguish an atomically
      replaced file even when its size and mtime are unchanged.
- [x] A valid same-size/same-mtime replacement is re-probed rather than
      inheriting the prior validation result.
- [x] A changed file identity clears a stale quarantine.
- [x] A file that changes during probing fails closed and is not admitted
      under an obsolete validation result.
- [x] Regression tests cover same-size/same-mtime atomic replacement.

## LOFI-LIB-011 — Make real-system acceptance evidence self-contained

Status: complete

- [x] The repository builds a second-client OBS stale-event monitor without
      changing the application's frozen OBS endpoint.
- [x] OBS credentials enter the monitor only through stdin; evidence contains
      only the configured safe input name, exact A/B basenames or `other`,
      fixed event/action labels, and numeric timing.
- [x] Capture is bounded by duration, frame size, and evidence bytes; output
      is a new mode-`0600` file and fails closed on integrity loss.
- [x] Verification proves only the permitted bounded correlation: A changed to
      B, restart was observed, an ended event arrived within two seconds, both
      immediate and settled snapshots remained B, and no third change
      intervened.
- [x] Acceptance bootstrap runs every claimed Go, race, vet, shell, and build
      check and records both production binary and monitor provenance.
- [x] The empty-plan precondition uses a row count instead of incorrectly
      treating a header-only CSV as empty.
- [x] Pre-start legacy-table evidence uses a stopped, fully checkpointed,
      immutable read-only snapshot; live queries never misuse
      `immutable=1`.
- [x] A tracked, credential-free generator recreates and validates all 19
      acceptance media fixtures plus their stream, duration, count, size, and
      checksum evidence from a clean checkout.

## LOFI-LIB-012 — Bound operator-supplied theme state and responses

Status: complete

- [x] `/theme` rejects overlong values before persistence or OBS mutation.
- [x] Text commands and callbacks apply the same length contract.
- [x] The maximum accepted theme remains safe in its command response,
      `/now`, `/preview`, and `/status`.
- [x] Boundary and over-limit regression tests cover both mutation and
      rendered Telegram message size.

## Verification

The following checks passed from this worktree on 2026-07-31:

```text
go test ./... -count=1
go test -race ./... -count=1 -timeout=2m
go test -race -p=1 ./... -count=1 -timeout=3m
go vet ./...
./run.sh test
./run.sh build
sh -n run.sh tests/permissions_test.sh tests/liveness_reader_test.sh tests/supervisor_integration_test.sh
./tests/permissions_test.sh
./tests/liveness_reader_test.sh
./tests/supervisor_integration_test.sh
git diff --check
```

The outstanding release-level task is to execute every required item in
`docs/LIBRARY_ACCEPTANCE.md` against a dedicated real OBS Studio instance and
Telegram Local Bot API Server, then retain the redacted evidence bundle.
