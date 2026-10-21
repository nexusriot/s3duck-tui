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
  and sync's new **exclude list** (which also closed the roadmap's old
  sync-globs item).

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
- **Show deleted objects** `[M]` — undelete itself exists: `d` on a delete marker in the
  version browser removes it, and the version underneath becomes current again. What is
  missing is reaching a deleted key: `ListObjectsV2` hides it, and the version browser
  opens only on a listed object. A "show deleted" listing toggle (keys whose latest
  version is a marker) closes the loop the version browser opened.
- **Deep duplicate scan** `[M]` — the finder groups by (size, ETag), which cannot see
  two identical files uploaded with different part sizes. `ObjectChecksum` alone does
  not close that blind spot: a multipart object's stored checksum is a
  checksum-of-parts, which depends on the part size just as the ETag does. It wants
  part-aware checksums (or a streamed hash of the candidates), one request or more per
  candidate, so it should be opt-in the way sync's checksum mode is.

## Soon

- **Batch dedupe** `[S–M]` — the duplicate finder (`D`, v0.6.0) deletes one copy at a
  time (through the trash on a safe-delete profile); a "keep the oldest, delete the
  rest" action per group (and one for all groups, with a totals confirm like the
  delete flow's) is its natural completion.
  `findDuplicates` already orders members keeper-first, so the plan is `Members[1:]`.
- **Duplicate scan across all buckets** `[S]` — same checkbox the recursive search
  already has; stale cross-bucket copies (migrations, abandoned backups) are the
  common real case.
- **Export the duplicate report** `[S]` — the failure ledger's export
  (`writeFailureReport`) is the shape to copy.
- **Presigned PUT** `[S]` — `PresignGetURL` exists; the upload counterpart is ~15 lines
  and enables "send me a file" workflows.
- **Copy `s3://` URI** `[S]` — `CopyToClipboard` reports its errors now, so this is
  only the button.
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

From the 2026-08 functional reviews and the 2026-10 review; the fixed ones are
listed in DESIGN.md's history.

- ~~**Navigation race, dual-pane variant**~~ — **fixed.** Pane state moved into
  `session.Pane`; the controller addresses `panes[active]` instead of mirroring the
  live pane into its own fields, and every navigation captures its `*Pane` before
  spawning and writes the location through `Pane.Enter` under the pane's lock.
- **`RefreshClient` mutates the shared model mid-transfer** — entering a bucket rebuilds
  `m.Client`/`m.Downloader` in place, so a running transfer *on the same profile* can
  see the region swap under it (cross-profile retargeting is fixed — clients are
  captured at entry — this is the same-profile variant). Wants navigation to build a
  new Model instead of mutating the shared one. It also runs on every bucket entry
  whatever the endpoint: it overwrites `m.Cf.Region` (which `CreateBucket` then uses,
  below), a backend without `GetBucketLocation` shows "Failed to resolve bucket region"
  on each entry, and for an `amazonaws.com` URL the rebuilt client uses the SDK's
  regional endpoint instead of `base_url`.
- **Download throttle is bursty** — throttling sleeps after each 5 MiB buffer flush, so
  at low caps the socket sits idle for tens of seconds between full-speed bursts (long
  enough for some proxies to drop the connection; SDK chunk retries then double-count
  progress). Wants throttling in smaller quanta on the read side. (The sleep does
  answer Cancel since the 2026-10-21 review; it used to be waited out.)
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
- ~~**Profile edit can still create a duplicate name**~~ — **fixed** (0.10.2). The
  edit form now refuses a rename onto another profile's name, as creation and copy
  already did.
- **Undo doesn't check its destination** `[S]` — every other remote write now
  confirms before replacing an existing object (0.8.0); undo goes straight
  through on the grounds that it has its own confirmation and restores objects
  to where they were moments ago. If something took that key meanwhile, it is
  overwritten silently. What the move itself left alone is safe now: an undo
  carries the move's skip set, so objects kept by "Skip existing" stay put
  (an undo used to copy the kept destination over the untouched source and
  then delete it), and an undo replays only through the profile it was
  recorded in.
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

Found in the 2026-10 review:

- **Folder undo also moves the destination's other objects** `[M]` — undoing a
  folder move into a folder that already held other, non-conflicting objects moves
  those into the source folder too: the undo is per folder, not per object. Nothing
  is lost (their source-side keys never existed), but it is wrong. Wants `MoveKeys`
  to report the exact keys it wrote, so the undo can replay just those.
- **Server-side copies reset the storage class** `[S]` — a copy writes STANDARD
  unless a class is asked for (S3 copy semantics), so a STANDARD_IA or
  INTELLIGENT_TIERING object comes back STANDARD after a rename or a trash and
  restore. GLACIER / DEEP_ARCHIVE objects cannot be trashed or moved until restored
  (`InvalidObjectState`).
- **Multipart copies carry no additional checksum** — `UploadPartCopy` cannot send a
  part checksum, so a copy above 5 GiB is written without the profile's
  `checksum_algo` (single-request copies do send it). The only fix in reach is
  streaming the bytes through the client, which is what a server-side copy is for
  avoiding.
- **Download verification runs after the rename** `[S]` — on a mismatch with
  Overwrite chosen, the previous file is already replaced and the corrupt one sits
  at the target name. Wants the check on the temp file, before the rename.
- **Download sync and non-canonical keys** `[M]` — a remote `a//b.txt` is planned as
  create `a//b.txt` plus delete `a/b.txt`, which are the same local file, so the
  fresh download is deleted again. (Writes no longer follow symlinked local
  subdirectories: sync leaves them out on both sides.)
- **`CreateBucket` uses the last bucket's region** `[S]` — `RefreshClient` overwrote
  `m.Cf.Region` on the way into the last bucket opened, so a new bucket lands there
  rather than in the profile's region.
- **History in a local pane** `[S]` — back/forward deliberately does nothing there
  (`session.Visit` has no local directory; replaying a remote entry built a hybrid
  pane), and toggling the local pane in single-pane mode after dual mode was used
  can show pane 1's old location.
- **`jumpTo` always takes focus** `[S]` — a bookmark jump, search reveal or session
  restore focuses the list when it finishes, whatever has focus by then.
- **Ctrl+C does not save the location** `[S]` — tview's own Ctrl+C handler stops the
  app without going through `Controller.Stop`; and `rememberLocation` records
  nothing while a local pane is active.
- **Activity-log rotation is not serialized** `[S]` — two writers crossing 2 MiB
  together, or two instances, can rotate twice and lose the previous generation;
  `auditLine` folds newlines and tabs but passes ESC sequences in keys through.
- **Profile names stored untrimmed, looked up trimmed** `[S]` — a profile saved as
  `"prod "` is looked up as `"prod"`, so its details and options resolve to nothing,
  or to a different profile of that name.
- **Usage browser paths for empty segments** `[S]` — a node's `fullPath` is wrong for
  keys like `/etc/x` or `a//b`.
- **config.json edge cases** `[S]` — `WriteConfig` does not fsync before its rename,
  writes through a fixed `config.json.tmp` that two instances share, and replaces a
  symlinked `config.json` (dotfile managers) with a regular file. An explicit save
  also writes the in-memory list, so it drops what another running instance saved
  since this one started, and fields this build does not know (only the automatic
  location save patches the file in place). With `$HOME` unset every path becomes
  relative to the working directory.
- **No conditional reads on large transfers** `[M]` — a multipart copy sends no
  `CopySourceIfMatch` and the downloader no `IfMatch`, so an object replaced
  mid-transfer can produce a mix of two versions.
- **Incomplete-upload cleanup** `[S]` — `ListMultipartUploads` returns only the first
  1 000, and "abort all" also aborts uploads this app is still running.
- **Sync retry drops the skipped deletes** `[S]` — a retry after failed writes does not
  carry the deletes the barrier held back, and "Retry failed (N)" counts the synthetic
  "deletes skipped" entry, which has nothing to retry.
- **Planned folder markers probe as folders** `[S]` — a folder-marker key in a
  copy/move/restore plan is checked as a folder, so `dst/docs/` reports a conflict
  whenever anything at all is under it.
- **Thin failure reporting** `[M]` — cross-profile copy lists the first 8 failures
  with no Retry or Export, undo has neither, and upload stops at its first error
  with no retry. The first two still open-code their pipeline and get the Runner's
  report when they become `ops` (REFACTOR.md §7); upload's is the model aborting
  the whole set on the first error.
- **Uncancellable short flows** `[S–M]` — rename, metadata and storage-class rewrites,
  version actions, edit, and the overwrite and trash-restore pre-scans
  (`PlannedCopyKeys`) still run on `context.Background()`.
- **Links and folder names** `[S]` — the Properties link of a plain AWS profile (empty
  `base_url`) is `/bucket/key`, with keys not URL-escaped; Create folder collapses
  `a//` through `path.Join`.
- **Unlocked controller reads** `[M]` — `c.active`, `c.panes`, the `c.view` pane
  pointers and `c.model` are still read from background goroutines without a lock
  in places. The reads are word-sized and the wrong-pane bugs they caused are fixed
  by capturing the pane first, but they remain races. `findBucketByName` likewise
  writes `c.pane().SetBuckets` from a navigation goroutine.
- **Navigation lists the active pane, not the navigated one** `[S]` — `Down`,
  `jumpTo` and `navigateTo` capture their pane for `Enter`, but the listing that
  follows lists whichever pane is active by then. Results land in the right pane (the
  listing captures it), but after a Tab mid-navigation the navigated pane shows its
  old rows until it is focused again and re-listed.
- **Editor save drops some headers** `[S]` — `PutBytes` carries Content-Type,
  Cache-Control, Content-Disposition, Content-Encoding, Content-Language, metadata,
  class and tags, but not Expires, the website redirect or the source's own KMS key
  (it takes the profile's SSE, else the bucket default).
- **A delegating `aws_profile` that resolves to nothing opens anyway** `[S]` — with the
  pinned SDK (config v1.18.8) `LoadDefaultConfig` falls back to an IMDS-backed
  provider, so `GetConfig`'s no-credentials check never fires and the first request
  fails with the SDK's own credential error.

Found in the 2026-10-21 review (the ones it fixed are in DESIGN.md's history):

- **Version downloads skip the transfer machinery** `[S]` — `DownloadVersion` writes
  straight to the final name (no temp file and rename), ignores the bandwidth cap,
  and on Windows a failed download leaves a partial file under that name.
- **A download can still clobber or collide** `[S]` — `overwrite=false` is checked
  before the transfer and not at the rename, so a file created meanwhile is replaced
  without a prompt, and two keys that land on one local path (`a//b` and `a/b`, or
  names differing only in case on a case-insensitive filesystem) are both reported
  downloaded while one survives. No fsync before the rename either.
- **Rewrites reset object ACLs** `[S]` — metadata saves, storage-class changes,
  version restores, editor saves and moves are copies or PUTs that send no ACL, so on
  a legacy ACL-enabled AWS bucket a public-read object comes back private.
- **`BucketConfig` reads any error as "off"** `[S]` — AccessDenied shows
  "Versioning: off" and "Encryption: none" as facts; only not-implemented and
  `NoSuch…Configuration` answers should mean the default.
- **`~/.aws` import differs from the SDK** `[S]` — a `[profile x]` section in the
  credentials file and a prefix-less section in the config file are taken where the
  SDK ignores them, `[default]` vs `[profile default]` is resolved in map order,
  indented sub-properties (`s3 =` blocks) are not ignored, China regions get the
  `.amazonaws.com` endpoint, and an unreadable credentials file is skipped silently.
- **Blocking work on the UI goroutine** `[S–M]` — bucket and folder creation call S3
  from the form's Save handler, and the upload preview (`PrepareUpload`,
  `LocalTargets`) walks the local tree there: a slow endpoint or a large directory
  freezes the app with no way to cancel.
- **Keymap conflicts the warnings miss** `[S]` — a leader that is also an action's
  chord makes that action unreachable, an action bound to the leader can never fire,
  and Ctrl+Q is hard-wired at the app level (binding it is dead; rebinding `quit`
  leaves it quitting).
- **Progress and display nits** `[S]` — copy/move's object rate divides one item's
  count by the whole run's time; the byte gauge dips while a file is verified; the
  resolving counter shows files approved rather than processed; column truncation
  and padding use different width functions, so emoji presentation sequences spill
  past their column; tview's URL tags (`[:::https://…]`) survive `tview.Escape`.
- **`V` looks in one directory** `[S]` — verify checks the profile's download
  directory, but a download lands in the local pane's directory when one is open, or
  in the picked one when `download_dir` is unset.
- **Version diff hides line-ending changes** `[S]` — `splitLines` normalises CRLF and
  the final newline, so two versions differing only there are "identical".
- **Esc and focus in reports and overlays** `[S]` — the delete and trash-restore
  reports keep the wait modal's Esc capture, so Esc does nothing there; the palette,
  activity log and bucket dashboard force focus to the list when closed, even when
  another overlay is still open above it.
- **Selection after renames** `[S]` — a folder rename that finishes after the user
  navigated away unselects in the new scope, leaving a phantom mark in the old one; a
  cancelled batch rename keeps marks on the items it already renamed. A successful
  retry of a failed cut-paste does not consume the cut.
- **The activity log names the profile open when a job finishes** `[S]` — a transfer
  backgrounded in one profile and finishing after a switch is logged under the other.
- **Packaging** `[S]` — `make x86_64` (and so `make all`) forces CGO on, which fails
  on a non-amd64 host; `Installed-Size` in `DEBIAN/control` is a fixed guess; CI does
  not run the packaging scripts or the static cgo build.
