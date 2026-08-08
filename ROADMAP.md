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

## Hardening (from the 2026-08 functional review)

Verified findings not yet fixed; the fixed ones are listed in DESIGN.md's history.

- **Navigation race, dual-pane variant** — `Down`/`jumpTo`/`navigateTo` goroutines write
  the live pane fields without synchronization; Tab during a slow bucket-open lands the
  navigation in the other pane. Proper fix: navigation state passed through the refresh
  path instead of mutated in place. (Long documented for the single-pane case.)
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
