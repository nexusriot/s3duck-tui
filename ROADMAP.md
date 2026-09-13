# S3Duck-TUI — Roadmap

Feature candidates, ordered by value against effort. Sizes: `[S]`mall / `[M]`edium / `[L]`arge.
Known constraints live in [DESIGN.md](DESIGN.md); user-facing docs in [README.md](README.md).

## Delivered in this round

Three tiers of work landed together; the design notes for each are in
DESIGN.md, and the new limitations they carry are in its table.

**Gaps in shipped features**

- **Content-Type on upload** — every object this app created was
  `application/octet-stream`; now derived from the name (with a sniff for
  extensionless files), applied at every creation point. Per-upload
  **server-side encryption** rode along on the same plumbing.
- **Streamed, cancellable listings** — a prefix with a hundred thousand keys is
  browsable (live count, `Esc` to stop, a disclosed cap) instead of a hang, and
  context now reaches every listing phase, closing that hardening item.
- **Profile identity + read-only profiles** — the header names the account and
  its endpoint; a read-only profile refuses every mutating action.
- **Background transfer notices** — a header badge while transfers run, a
  transient notice and the terminal bell when a backgrounded one finishes.
- **Retry failed / Export list** — failures are kept with enough context to
  re-run exactly those units, across download, sync, copy/move, rename and
  delete.

**Features built on what already existed**

- **Delegated credentials** — a profile can name a `~/.aws` profile and let the
  SDK resolve it, which makes SSO / assume-role / `credential_process` work and
  keeps them refreshed. Delegating profiles are now importable rather than
  listed with the reason they were not.
- **Checksums** — asked for on write, verified after download (automatically or
  via `V`), and used by sync's optional content comparison. This is also the
  honest answer for multipart objects, whose ETag cannot be compared.
- **Local pane** (`l`) — one dual-pane side browses a directory with the same
  columns, sort, filter and multi-select as a bucket; `Ctrl+Y` uploads from it.
- **Usage browser** (`G`) — drill down through the prefix tree by size, with
  object counts and a storage-class breakdown; the flat summary's silent
  top-10 truncation is now disclosed.
- **Preview** (`P`) via a ranged GET, with a hexdump for binaries, and a
  **version diff** (`D` in the version browser).

**Structural**

- **Configurable keymap** — bindings are data (`~/.config/s3duck-tui/keys.json`)
  with a leader key (`,`) for a second namespace, and the hotkey panel is
  generated from the live keymap.
- **Persistent activity log** + per-profile **session restore**.
- **Trash / safe delete** — delete becomes a move under `.s3duck-trash/<ts>/`,
  with restore (`R`) and an explicit empty.
- **Glob / regex queries** — one matcher for the filter, the recursive search
  and sync's new **exclude list** (which also closes the sync-globs item below).

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
  versioned buckets can actually be removed. (Safe delete does not help here: a
  trashed object leaves a delete marker at its old key.)
- **Enable bucket versioning from the dashboard** `[S]` — the bucket dashboard reports
  versioning read-only and the version browser requires it; the app should be able to
  turn it on (`PutBucketVersioning`).
- **Show deleted objects + undelete** `[M]` — `ListObjectsV2` hides delete-marked keys,
  so nothing can be undeleted unless you already know the key. A "show deleted" listing
  toggle plus a remove-the-marker action closes the loop the version browser opened.
- **Deep duplicate scan** `[M]` — the finder groups by (size, ETag), which cannot see
  two identical files uploaded with different part sizes. With `ObjectChecksum` in
  place, a second pass over same-size groups that differ only by ETag would close
  that documented blind spot; the cost is one request per candidate, so it wants to
  be opt-in the way sync's checksum mode is.

## Soon

- **Batch dedupe** `[S–M]` — the duplicate finder (`D`, v0.6.0) deletes one copy at a
  time; a "keep the oldest, delete the rest" action per group (and one for all groups,
  with a totals confirm like the delete flow's) is its natural completion.
  `findDuplicates` already orders members keeper-first, so the plan is `Members[1:]`.
- **Duplicate scan across all buckets** `[S]` — same checkbox the recursive search
  already has; stale cross-bucket copies (migrations, abandoned backups) are the
  common real case.
- **Export the duplicate report** `[S]` — the failure ledger's export
  (`writeFailureReport`) is the shape to copy.
- **Presigned PUT** `[S]` — `PresignGetURL` exists; the upload counterpart is ~15 lines
  and enables "send me a file" workflows.
- **Copy `s3://` URI** `[S]` — and fix `CopyToClipboard` swallowing errors while there.
- **Go-to-path jump** `[S]` — `jumpTo()` exists; needs only an input modal.
- **CLI flags** `[S]` — `--profile` / `--config` / `--version`; nothing parses args today.
- **Mouse support** `[S]` — `EnableMouse(true)` plus click-to-select. Now that the
  keymap is data, this is the other half of "input is configurable".
- **Export listing to CSV/JSON** `[S]` — makes recursive search and the usage browser
  usable outside the TUI.
- **Watch mode** `[S]` — a toggle that re-lists the current prefix every few
  seconds and highlights new/changed rows; for watching a pipeline drop files
  into a bucket. The transfers panel's ticker pattern already exists.
- **Anonymous profiles** `[S]` — a "public bucket (no credentials)" checkbox
  using `aws.AnonymousCredentials`, for browsing public datasets. Today a
  profile always signs requests, so public-only access is impossible.
- **Search filters** `[M]` — extend recursive search beyond the name query with
  size/date predicates (`>100M`, `<2025-01-01`). The query syntax (text / glob /
  `re:`) is in place; this is the predicate half.
- **Saved sync jobs** `[S]` — persist local dir + direction + destination + the
  exclude list per profile (the `Bookmarks` pattern), re-run from the palette.
- **Local pane: sort by the pane's own key** `[S]` — the local pane honours the
  shared sort key, which is right for name and size but means a local and a
  remote pane cannot be ordered differently.

## Later

- **Bucket policy viewer** `[S–M]` — `MakeBucketPublic` writes a policy nothing can read
  back; a read-only viewer alone is worthwhile.
- **Lifecycle rules viewer** `[M]` — explains why objects change class or vanish, and
  would pair with the usage browser's storage-class breakdown.
- **Object Lock retention / legal hold** `[M]` — per-object, complementing the dashboard.
- **OS keyring for secrets** `[M]` — `secret_key` / `session_token` are plaintext (0600).
  Less pressing now that `aws_profile` stores no key material at all, but it is still
  the answer for a static MinIO key.
- **Download resume** `[L]`.
- **Dual panes on different profiles** `[L]` — the cross-profile copy (`>`,
  v0.7.0) built the two-client substrate, and the local pane showed that a pane
  can hold something other than "the one model": the remaining work is a
  per-pane client.
- **Trash retention** `[S–M]` — an age-based "empty anything older than N days",
  so safe delete does not need a manual sweep.

## Hardening (verified, not yet fixed)

From the 2026-08 functional reviews; the fixed ones are listed in DESIGN.md's history.

- **Navigation race, dual-pane variant** — `Down`/`jumpTo`/`navigateTo` goroutines write
  the live pane fields without synchronization; Tab during a slow bucket-open lands the
  navigation in the other pane. Proper fix: navigation state passed through the refresh
  path instead of mutated in place. (The streamed listing narrowed the window — the
  progress counter and the render target are captured before the fetch — but the
  underlying mutation is unchanged.)
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
  The trimming starts earlier than the readers: `Model.List` itself trims each key
  while deriving the display name. Removing the trims needs care around the `[..]`
  row and profile names.
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
