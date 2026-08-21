# S3Duck-TUI — Roadmap

Feature candidates, ordered by value against effort. Sizes: `[S]`mall / `[M]`edium / `[L]`arge.
Known constraints live in [DESIGN.md](DESIGN.md); user-facing docs in [README.md](README.md).

## Next

- **Bulk operations for metadata / storage class / versions** `[M]` — `m`, `c` and `v`
  act on the highlighted object only, while download / copy / move / rename / delete
  all honour the marked set. Tagging forty objects `env=staging` or archiving a whole
  folder to GLACIER should be one action. The per-object machinery exists; this is the
  marked-set loop plus a progress modal, same shape as `runDelete`.
- **Version-aware delete / purge** `[M]` — `model.Delete` sends no `VersionId`, so on a
  versioned bucket a folder delete only writes delete markers and a bucket can never be
  fully emptied from the TUI (`EmptyBucket` clears current objects; versions survive).
  Wants: "delete all versions of this object", and a version-aware recursive purge so
  versioned buckets can actually be removed.
- **Sync include/exclude globs** `[S–M]` — `planSync` has no filtering at all, so sync
  can't be pointed at any real working tree (`.git/`, `node_modules/`, `*.tmp`). A glob
  list on the sync form, applied to both entry slices before the diff. The planner is
  pure, so cheap to test.
- **Enable bucket versioning from the dashboard** `[S]` — the bucket dashboard reports
  versioning read-only and the version browser requires it; the app should be able to
  turn it on (`PutBucketVersioning`).
- **Show deleted objects + undelete** `[M]` — `ListObjectsV2` hides delete-marked keys,
  so nothing can be undeleted unless you already know the key. A "show deleted" listing
  toggle plus a remove-the-marker action closes the loop the version browser opened.

## Soon

- **Batch dedupe** `[S–M]` — the duplicate finder (`D`, v0.6.0) deletes one copy at a
  time; a "keep the oldest, delete the rest" action per group (and one for all groups,
  with a totals confirm like the delete flow's) is its natural completion.
  `findDuplicates` already orders members keeper-first, so the plan is `Members[1:]`.
- **Duplicate scan across all buckets** `[S]` — same checkbox the recursive search
  already has; stale cross-bucket copies (migrations, abandoned backups) are the
  common real case.
- **Export the duplicate report** `[S]` — pairs with the export-listing item below.
- **Presigned PUT** `[S]` — `PresignGetURL` exists; the upload counterpart is ~15 lines
  and enables "send me a file" workflows.
- **Copy `s3://` URI** `[S]` — and fix `CopyToClipboard` swallowing errors while there.
- **Go-to-path jump** `[S]` — `jumpTo()` exists; needs only an input modal.
- **CLI flags** `[S]` — `--profile` / `--config` / `--version`; nothing parses args today.
- **Mouse support** `[S]` — `EnableMouse(true)` plus click-to-select.
- **Read-only profile flag** `[S]` — blocks delete/move/upload for profiles pointing at
  production.
- **Export listing to CSV/JSON** `[S]` — makes recursive search and the size summary
  usable outside the TUI.
- **Summary by storage class** `[S]` — `buildSummary` groups by category/prefix; a class
  breakdown is the closest thing to a cost view the app can offer.
- **Post-transfer verify** `[M]` — droid parity: after a download, MD5 the local
  file and compare against the ETag (single-part only — multipart tags carry a
  `-N` suffix and are skipped honestly). Also usable as a standalone "verify
  local file against object" action, upgrading compare/sync from size-match to
  content-match for single-part objects.
- **Watch mode** `[S]` — a toggle that re-lists the current prefix every few
  seconds and highlights new/changed rows; for watching a pipeline drop files
  into a bucket. The transfers panel's ticker pattern already exists.
- **Anonymous profiles** `[S]` — a "public bucket (no credentials)" checkbox
  using `aws.AnonymousCredentials`, for browsing public datasets. Today a
  profile always signs requests, so public-only access is impossible.
- **Search filters** `[M]` — extend recursive search beyond name-substring with
  size/date predicates (`>100M`, `<2025-01-01`).
- **Saved sync jobs** `[S]` — persist local dir + direction + destination per profile
  (the `Bookmarks` pattern), re-run from the palette.
- **Per-upload server-side encryption** `[S–M]` — the dashboard reads bucket encryption
  but no upload sets `ServerSideEncryption`; per-profile default (SSE-S3 / SSE-KMS).

## Later

- **Bucket policy viewer** `[S–M]` — `MakeBucketPublic` writes a policy nothing can read
  back; a read-only viewer alone is worthwhile.
- **Lifecycle rules viewer** `[M]` — explains why objects change class or vanish.
- **Object Lock retention / legal hold** `[M]` — per-object, complementing the dashboard.
- **OS keyring for secrets** `[M]` — `secret_key` / `session_token` are plaintext (0600).
- **Text preview via ranged GET** `[M]`.
- **Download resume** `[L]`.
- **Dual panes on different profiles** `[L]` — the cross-profile copy (`>`,
  v0.7.0) built the two-client substrate; pointing the second pane at another
  profile is the natural next step.

## Hardening (verified, not yet fixed)

From the 2026-08 functional reviews; the fixed ones are listed in DESIGN.md's history
(latest round: 2026-08-11, released as 0.8.0).

- **Navigation race, dual-pane variant** — `Down`/`jumpTo`/`navigateTo` goroutines write
  the live pane fields without synchronization; Tab during a slow bucket-open lands the
  navigation in the other pane. Proper fix: navigation state passed through the refresh
  path instead of mutated in place. (Long documented for the single-pane case.)
- **`RefreshClient` mutates the shared model mid-transfer** — entering a bucket rebuilds
  `m.Client`/`m.Downloader` in place, so a running transfer *on the same profile* can
  see the region swap under it (cross-profile retargeting is fixed — clients are
  captured at entry — this is the same-profile variant). Wants navigation to build a
  new Model instead of mutating the shared one.
- **Download throttle is bursty** — throttling sleeps after each 5 MiB buffer flush, so
  at low caps the socket sits idle for tens of seconds between full-speed bursts (long
  enough for some proxies to drop the connection; SDK chunk retries then double-count
  progress). Wants throttling in smaller quanta on the read side.
- **Whitespace keys resolve to the wrong object** — every secondary-text reader trims
  the key (`strings.TrimSpace`), so `"dir/report "` is looked up as `"dir/report"`.
  Removing the trims needs care around the `[..]` row and profile names.
- **Cancellation doesn't reach listing phases** — `ListObjects` hardcodes
  `context.TODO()`, so cancelling a copy/move/sync mid-listing keeps paginating until
  the listing completes. Wants ctx plumbed through `ListObjects`/`Delete`.
- **`NormalizePrefix` trims legitimate spaces** — a folder genuinely named with
  leading/trailing spaces mis-derives sync/download relative paths.
- **Upload direction ignores the destination fields** — in the sync form the dest
  bucket/prefix apply only to remote → remote (labels say so; the mandatory preview
  shows the real destination). Consider greying the rows out per direction instead.
- **Profile edit can still create a duplicate name** `[S]` — creation and copy now
  reject duplicates; renaming an existing profile onto another's name does not.
- **Undo doesn't check its destination** `[S]` — every other remote write now
  confirms before replacing an existing object (0.8.0); undo goes straight
  through on the grounds that it has its own confirmation and restores objects
  to where they were moments ago. If something took that key meanwhile, it is
  overwritten silently.
- **Same-named items from different folders collide on copy** `[S]` — copying
  `a/report.txt` and `b/report.txt` into one destination writes both to the
  same key, so only the last survives, and the overwrite scan cannot see it
  (neither key exists at the destination yet). `planBatchRename` already
  rejects this class for renames; copy/move wants the same intra-batch
  duplicate-target check.
- **Sync still writes without an overwrite prompt** `[S]` — by design: its
  dry-run plan already lists every update before anything moves, so a second
  confirmation would be redundant. Revisit only if the plan stops being
  mandatory.
