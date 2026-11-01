S3Duck-TUI 🦆
======

[![CI](https://github.com/nexusriot/s3duck-tui/actions/workflows/ci.yml/badge.svg)](https://github.com/nexusriot/s3duck-tui/actions/workflows/ci.yml)

Terminal UI client for S3-compatible object storage. A TUI implementation of [S3Duck](https://github.com/nexusriot/s3duck) built in Go on top of [tview](https://github.com/rivo/tview) / [tcell](https://github.com/gdamore/tcell), using the AWS SDK for Go v2.

Works with AWS S3 and any S3-compatible service (MinIO, Ceph RGW, Yandex/VK Cloud, Backblaze B2, etc.) by pointing at a custom endpoint URL.

Features
-------------

1. Multi-profile support (create / edit / delete / clone / verify)
2. Bucket browsing with folder-style navigation (delimiter `/`), a live in-listing name filter (`/`, narrows as you type — plain text, `*.glob` or `re:regex`), and recursive search under the current prefix (Ctrl+F, same query syntax) whose results jump straight to the matching object. Listings are **streamed page by page** with a live count, stop on `Esc`, and cap at 20 000 objects per level, so a prefix with a hundred thousand keys is browsable instead of a hang
3. Bucket creation (private or public-read) and deletion
4. Folder creation (zero-byte `prefix/` markers)
5. Recursive download of files and folders with **parallel downloads** (4 objects at a time, or the profile's `transfer_workers`): overwrite conflicts are resolved sequentially first (Overwrite / Skip / Overwrite All / Skip All / Cancel), then approved objects download concurrently — each into a temp file of its own, renamed over the target only once the download fully succeeded and is the size the object was listed at (a large one interrupted part-way is resumed next time, see 64), so a canceled or failed run never destroys local data; live progress shows the active file names, combined byte progress, and **transfer speed + ETA**. Files land in the local pane when one is open, else in the profile's `download_dir` (a leading `~` is expanded), else in a directory you pick
6. Multi-select for batch download, copy, move, rename and **delete** (Space, Ctrl+S all, Ctrl+X none), plus Midnight Commander's **mark by pattern** (`+`), **unmark by pattern** (`-`) and **invert marks** (`*`). The pattern is the filter's query syntax (text, `*.glob`, `re:regex`) matched against the names shown, it acts only on what the filter lets through, and folders are left alone unless *Include folders* is ticked — a folder mark is a recursive one
7. Upload with a built-in, icon-styled local filesystem browser; preserves directory tree, creates markers for empty folders, and sends exactly the keys the overwrite check was shown. Every uploaded object gets a **derived Content-Type** (extension table first, content sniff for extensionless files), so a bucket serving a site over HTTP does not answer `application/octet-stream` to everything
8. Bucket / folder size summary (Ctrl+G)
9. Object properties (size, ETag, storage class, last modified) in the side panel; presigned (time-limited) share links for private objects (Ctrl+W, or the "Presign Link" button in properties — 15m / 1h / 24h / 7d, capped at the SigV4 7-day max)
10. Profile cloning (Ctrl+Y on the profiles screen) and system-clipboard copy of a presigned link (Ctrl+W) or an operation report. On Linux the clipboard needs `xclip`, `xsel` or `wl-clipboard` (the `.deb` recommends them); without one the copy says it failed and shows the link to select by hand
11. Server-side copy / move / rename, recursive for folders, multi-select aware, **cross-bucket** (pick any destination bucket) with live object rate + ETA (Ctrl+Y copy, Ctrl+T move, Ctrl+R rename)
12. **Dual-pane (Midnight Commander style)** layout (Ctrl+O toggle, Tab to switch panes); copy/move defaults its destination to the other pane
13. **Batch / pattern rename** of multiple marked objects (Ctrl+R with >1 selected): `{name}` / `{ext}` / `{n}` tokens plus an optional find→replace
14. **Bookmarks** of bucket+prefix locations per profile (Ctrl+B) and **back/forward navigation history** (`[` / `]`, or Alt+←/→)
15. **Command palette** (Ctrl+K) — fuzzy launcher over a hand-picked list of most browser actions
16. **Per-profile bandwidth throttle** (`max_bytes_per_sec`) capping combined upload/download throughput
17. **Background transfer queue** — downloads/uploads can run in the background (the progress modal has a **Background** button); a transfers panel (`t`) shows each job's state (running / queued / done / failed / canceled), live progress/speed/ETA, cancel, and clear, with at most 2 byte-transfers running at once — the rest wait as *queued*
18. **Object clipboard** — yank/cut objects (`y`/`x`) and paste (`p`) into any folder or the other pane (cross-bucket aware)
19. **Undo** the last move/rename (`u`), in the profile it happened in; objects a "Skip existing" left alone stay where they are
20. **Search across all buckets** (checkbox in the Ctrl+F prompt); results jump straight to the matching object in its bucket
21. **Abort incomplete multipart uploads** (`, m`) and a **bucket dashboard** (`, b`): region, versioning, default encryption, object lock, public access block, tags, lifecycle rules, replication, CORS, website hosting, event notifications, access logging and the bucket policy, read in parallel. Each section says whether it is configured, unset, **not supported by this endpoint** or **access denied** — never a default it could not confirm. `t` edits the bucket's tags, `v` turns **versioning** on or suspends it (behind a confirmation that says what each means; refused when the state could not be read or MFA delete is on) (refused when it carries `aws:` system tags, which a rewrite would lose), `r` reloads. Both are also in the command palette
22. **Operation activity log** — the current session in-app (`, a` or the palette), and a persistent copy on disk (see 48)
23. **Sync** (Ctrl+E) — mirror in any of three directions: local → remote, remote → local, or **remote → remote** (bucket/prefix to bucket/prefix, server-side copies). Always preceded by a mandatory dry-run plan (create / update / delete, per-file reason, total bytes); optional deletion of extraneous objects at the destination. Applied by a worker pool (4 wide, or the profile's `transfer_workers`), writes before deletes. **Exclude patterns** filter both sides before the diff (`node_modules, *.tmp, re:^\.git$`): plain text is a case-insensitive substring tested against the relative path and each of its segments (plain `.git` would also exclude `.github/`), `*` / `?` make a glob, `re:` a regex. The list is split on every comma, so a regex cannot contain one, and a pattern that does not compile stops the run instead of matching nothing. An optional **checksum comparison** replaces size+mtime with real content comparison. A symbolic link to a file is synced as that file; a link to a directory (or a dangling one) is not followed, and paths under it are left out on both sides, as an exclude would leave them
24. **AWS credentials, delegated or static** — name a `~/.aws` profile and the AWS SDK resolves it on every use, which makes **SSO, assume-role and `credential_process` profiles work and stay refreshed** (a stored session token cannot). Static keys with an optional `session_token` still work; `Ctrl+I` on the profiles screen imports either kind from `~/.aws/credentials` / `~/.aws/config`
25. **Sort** the listing by name / size / date, ascending or descending (`s` cycles the key, `S` reverses); **refresh** with `r` or F5
26. **Object versioning** (`v`) — full history for the selected object including delete markers; restore an old version as current (a copy to the top of the history, so nothing is lost), download any version, or permanently delete one
27. **Object metadata & tags** (`m`) — edit Content-Type / Cache-Control / Content-Disposition / Content-Encoding and `x-amz-meta-*` pairs, plus the object tag set; only the halves you actually changed are written
28. **Storage class & Glacier restore** (`c`) — move an object between storage classes in place (metadata preserved) and request a temporary restore of an archived object, with days and retrieval tier; restore state is shown in the metadata (`m`) and storage-class (`c`) dialogs, and every restore is **tracked** (see 52)
29. **Retried uploads** — the multipart uploader now backs off and retries transient failures instead of failing the whole transfer on one blip, and grows its part size with the file so S3's 10 000-part limit no longer fails an upload above 48.8 GiB at its last part
30. **Listing columns** — size, modified date and storage class alongside the name, so the sort keys order by something visible. The layout follows the pane width: columns drop (class → date → size) rather than squeezing names into uselessness, and re-flow on terminal resize
31. **Edit in `$EDITOR`** (`e`) — open a small text object in your editor (the TUI suspends while it runs) and save straight back to the bucket; Content-Type, Cache-Control and friends, user metadata, storage class and tags are all preserved. If the object changed on the server while you edited — or cannot be confirmed unchanged, because it was deleted or the check failed — the save is refused and your version is kept in a temp file (the error names it). Guarded by a 1 MiB cap and a binary sniff
32. **Cross-profile copy** (`>`) — copy the marked objects or folders to a bucket in a *different profile*, streamed through the client (GET here → PUT there), so the two sides can be entirely different endpoints (MinIO → AWS, provider migration). Content headers, metadata and tags ride along (storage class deliberately not — class names aren't portable across providers); runs as a cancellable background transfer
33. **Duplicate finder** (`D`) — scan the current prefix recursively and browse groups of identical objects (matched by size + ETag), ordered by wasted bytes; reveal any copy in the browser or delete it with a confirmation (into the trash on a safe-delete profile). The oldest copy is marked as the likely original
34. **Pane comparison** (`=`) — read-only diff of the two dual-pane bucket locations by name and size (left-only / differs / right-only), answering "are these two prefixes actually the same?" without transferring anything
35. **Overwrite confirmation for remote writes** — rename, batch rename, copy, move, paste, upload, cross-profile copy and restore-from-trash all check the destination first and name what they would replace, offering **Overwrite**, **Skip existing** (when that still leaves something to do) or **Cancel**; an upload's empty-folder markers replace no content and are not counted. Skipping a destination on a *move* leaves the source in place too, so nothing is ever deleted without having been written somewhere. Sync is exempt: its dry-run plan already lists every update before anything moves
36. **Copies above 5 GiB** — every server-side copy (copy, move, rename, paste, trash and restore, storage-class changes, metadata saves, version restores, remote → remote sync) falls back to a concurrent multipart part-copy past the size where a single-request S3 copy is rejected, carrying content headers (Content-Language, Expires and the website redirect included), metadata and tags across, encrypted by the same rule as a single-request copy (see `sse` under *Configuration*). Such a copy carries no additional checksum: `UploadPartCopy` has no way to send one
37. Custom endpoints and self-signed TLS support (`ignore_ssl`)
38. **Local pane** (`l`; pressed in the local pane, it closes it) — point one dual-pane side at a directory and browse it with the same columns, sort, filter and multi-select as a bucket; `Ctrl+Y` from it uploads exactly the marked files and folders to the other pane's prefix, and `Ctrl+Y` (or `Ctrl+D`) from the remote side downloads into that directory. The local pane never deletes, renames or creates: it is a transfer endpoint, not a file manager. Actions that only mean something against an object — properties, presigned links, versions, metadata, Ctrl+U and the rest — say so instead of acting on the local file, and back/forward history does nothing there
39. **Usage browser** (`G`) — one recursive listing, then drill down through the prefix tree by size: share bars, object counts and a **storage-class breakdown** at every level, `g` to jump to any node in the browser. The flat top-10 summary (Ctrl+G) now discloses what it folded away instead of truncating silently
40. **Preview** (`P`) — read-only look at the head of any object through a **ranged GET**, so previewing a 40 GiB log costs one small request. The head is shown the way it reads best: **gzip and zstd** are decoded on the fly (a `.gz`/`.zst`/`.tgz` gets a 256 KiB window, since a compressor's first block can be larger than 64 KiB), **JSON** is pretty-printed (a truncated head is re-indented token by token, and labelled as not validated), **CSV/TSV** become aligned columns with the header highlighted (up to 64 columns; a wider head is shown as text), and a **tar** (or `.tar.gz`) lists its entries like `tar tv`. Binary content is a hexdump; `r` switches between the formatted and the raw text, and `e` hands the object to the editor
41. **Version diff** (`D` in the version browser) — unified diff of any old version against the current one. Bounded by line count rather than by bytes (the comparison is quadratic in lines): a pair too large to diff in memory is declined with both counts named, rather than taking the app down
42. **Read-only profiles** — a per-profile flag that refuses every mutating action, and a header that always names the open profile, its endpoint and its read-only state, so "which account is this?" is answerable without leaving the browser
43. **Safe delete (trash)** — a per-profile mode where delete, from the browser or the duplicate finder, becomes a server-side move into `.s3duck-trash/<timestamp>/`; `R` restores objects to the keys they came from — through the same overwrite confirmation as any other remote write, since the original key may hold something newer by then. Deleting a whole bucket, a version or something already in the trash, and emptying the trash, are real deletes; a sync leaves the trash prefix out of both sides, so its delete-extraneous pass (whose deletes are real) never reaches it
44. **Checksums** — ask S3 to verify a `CRC32C` / `CRC32` / `SHA256` / `SHA1` checksum on writes (all but copies above 5 GiB), verify a downloaded file against it automatically or on demand (`V`), and get a straight answer where no comparison is possible: a multipart object's checksum-of-parts and the ETag of an SSE-KMS object are reported as not verifiable rather than as a mismatch. Optional per-profile **server-side encryption** (SSE-S3 / SSE-KMS)
45. **Retry failed / export list** — download, upload, copy, move, paste, sync, delete and batch rename keep their failures and can write the full list to a file instead of showing the first eight and forgetting the rest; all of them but upload, which stops at its first error, also offer **Retry failed** for exactly those units. Restore from the trash exports but does not retry; cross-profile copy names the first eight failures and offers neither, and undo lists its failures with neither. A job you sent to the background never shows that report, so its failures are written to the activity log instead
46. **Background transfer notices** — a header badge while transfers run, plus a transient notice, a terminal bell and a **desktop notification** when a backgrounded one finishes, or a tracked Glacier restore becomes readable (none for a job you cancelled); no modal steals focus from what you moved on to. **Quitting** while transfers run asks first, then cancels them and lets each clean up (partial files removed, unfinished uploads aborted) before exiting. Notifications go through `notify-send` / `osascript` on a desktop, or through the terminal itself (OSC 9 / OSC 777) — see *Notifications*
47. **Configurable keymap** — every browser-screen action has a name (see *Action names*) and can be rebound in `~/.config/s3duck-tui/keys.json`, with a **leader key** (`,`) opening a second namespace; the in-app hotkey panel is generated from the live keymap, so it cannot drift from what the keys do
48. **Persistent activity log** — operations are recorded in `~/.local/state/s3duck-tui/activity.log` (rotated at 2 MiB) alongside the in-session view, multi-item deletes, copies, moves and syncs as one line of counts rather than one per key; and each profile reopens where it was last browsing (saved on `Ctrl+P` and on quit)
49. **Diff against the local copy** (`, d`) — line diff of the highlighted object against the file next to you in the local pane (by name), or where a download would have put it; when neither exists, the path is asked for. `n` / `N` jump between changes. Two files that differ only in line endings are said to, rather than shown as identical; binary files are compared by bytes. Both sides are capped at 1 MiB — for anything larger, `V` compares checksums
50. **Download as one archive** (`, z`) — the marked items, or the highlighted one, streamed into a single **.tar.gz** or **.zip** (already-compressed files are stored, not deflated again) with entry names relative to the folder being browsed. No object is staged on disk. An object that cannot be read (access denied, an archived Glacier object) is left out and reported; one that fails mid-transfer abandons the archive rather than leave a corrupt file under the final name, which is only written once every entry is in. Runs as a transfer like any download: background, cancel, failure list
51. **Parallel transfers per profile** (`transfer_workers`, on the options form) — how many objects one download or one sync moves at a time, 1–16 (default 4)
52. **Glacier restore tracker** (`, r`) — every restore requested with `c`, or seen in progress there, is remembered (in `~/.local/state/s3duck-tui/restores.json`, so the wait survives quitting), checked with a HEAD every minute while its profile is open, and announced with a notice and the bell when it becomes readable. The listing's class column shows `restoring` / `restored`. The panel lists every tracked restore with its tier, expiry and last check; Enter reveals the object, `r` checks now, Del forgets one, `c` clears the finished ones
53. **Profile export** (`x` on the profiles screen) — the highlighted profile as an AWS CLI profile (`~/.aws/config` + `credentials`, with `endpoint_url` and path-style addressing for a custom endpoint), an `mc alias set` command, or an rclone remote; `f` switches format, `c` copies, `w` writes it to a file. **Secrets are never exported**: the secret key and session token are placeholders, and a profile delegating to `~/.aws` exports the delegation (rclone's `env_auth` + `profile`) instead of keys
54. **Command line** — `s3duck-tui -p prod s3://logs/2026/10/` opens a profile straight at a bucket and folder (or, for an object key, its folder with the object highlighted); `--config` picks another profile file, `--version` / `--help` print and exit. See *Command line*
55. **Go to** (`g`) — type `s3://bucket/key`, `/key` (from the bucket root) or a path relative to the current folder (`..` works); inside a bucket the box starts with the current location as a URI, so editing its tail is the usual move (on the bucket list it starts empty, and a path's first segment is the bucket). A key without a trailing slash opens as a folder if anything lives under it, otherwise as an object in its folder
56. **Copy `s3://` URI** (`, c`) — of the marked items (one per line), the highlighted one, or on `..` the folder being shown; the form `aws s3`, `s5cmd` and this app's own `g` take
57. **Presigned upload link** (`, p`) — a time-limited PUT URL (15m / 1h / 24h / 7d) that lets someone without credentials deliver one file with `curl -T FILE '<link>'`. The key starts as the highlighted object (replacing it, which the result says) or the current folder; refused on read-only and anonymous profiles. The link is not written to the activity log
58. **Anonymous profiles** — a profile option for public buckets (open datasets): requests are sent unsigned, the bucket list is the names you give it (S3 has no anonymous bucket listing), and `g` reaches any other public bucket. The region comes from `HEAD`, which answers unsigned; `Ctrl+W` copies the object's plain URL; `Ctrl+V` checks each named bucket answers. The header shows `[ANONYMOUS]`
59. **Purge versions** (`, X`, or `v` with several items marked) — on a versioned bucket an ordinary delete only adds a delete marker, so nothing is freed; a purge deletes stored versions by ID. *Every version* removes the keys for good, history included; *Old versions only* keeps what each key reads as now (an object, or "deleted") and removes its older versions and old delete markers. The selection's versions are listed first, and the confirmation states the keys, versions, bytes and markers that go — and that it is permanent: no trash, undo or history can bring them back. It runs as a job (Background, Cancel, Retry failed for exactly the versions refused). **Deleting a bucket** now purges it the same way first, so a versioned bucket with history can be removed at last; its confirmation says so
60. **Deleted objects** (`, D`) — the keys under the current folder whose latest entry is a delete marker: an ordinary listing cannot show them, which is what left undelete unreachable. Each row says when it was deleted and what undelete brings back; Space marks, `u` / Enter undeletes (removes just the marker, so the version beneath is current again), `v` opens the key's version history. Capped at 5 000 keys; open a deeper folder for more
61. **Bulk metadata, tags and storage class** — `m` and `c` with more than one item marked, or a folder chosen, act on every object in the selection (folders expanded, the trash left out): one form, one confirmation, one job. The metadata form says what to change rather than showing values — blank keeps each object's own Content-Type and Cache-Control, metadata and tags are set or removed by key and merged into each object's set — and objects already in the asked-for state are skipped. `c` sets one class for all, or requests a restore of the archived ones (each tracked)
62. **Buckets by name when listing is denied** — a key scoped to some buckets is often refused the bucket list. The bucket screen then shows the buckets the profile knows of (its last location, its bookmarks) instead of an error, and `g s3://bucket/` opens any other
63. **Dual panes on different profiles** (`, o`) — point the other pane at any profile, opening the dual layout if needed; it lands where that profile was last browsing. Each pane is its own account: Tab switches which one every action, guard (read-only, safe delete) and the header use, the pane titles name their profile when the two differ, and each profile remembers its own location. Between panes on different profiles, Ctrl+Y copies and Ctrl+T moves by streaming the objects through this machine (a move deletes each source object only once its copy has landed; there is no undo for it), `=` compares each side through its own account, and a clipboard paste across accounts is refused rather than run against a same-named bucket
64. **Download resume** — a download of 8 MiB or more that is cancelled, fails, or is interrupted by quitting keeps the bytes it has in a hidden partial file beside the target (`.name.<hash>.s3duck-part`, with a small `.s3duck-resume` record of the object and its version). The next download of the same object picks up where it stopped — a ranged GET pinned to that version with `If-Match`, so bytes of two versions are never mixed — and an object that changed meanwhile starts over. Every large download is pinned the same way, so one replaced mid-download fails instead of arriving as a mix. Partial files are left out of the local pane, uploads and sync
65. **Export a listing** (`, e`; `x` in the search results and the usage browser) — the rows on screen, filter and sort applied, written as CSV or JSON into the download directory: bucket, key, type, size, modified time (RFC 3339, UTC), storage class, ETag, and for usage rows the object count. Keys with commas, quotes or newlines stay one CSV record; a CSV cell that a spreadsheet would run as a formula (`=`, `+`, `-`, `@`) is prefixed with `'` — the JSON is verbatim
66. Linux (amd64/arm64/armv7/riscv64), FreeBSD and macOS / Windows builds (statically linkable)

Screenshots
-------------

![Profiles](resources/00-profiles.png)
![Create Profile](resources/01-create_profile.png)
![Bucket list](resources/02-bucket_list.png)
![Create Folder](resources/03-create_folder.png)
![Download](resources/04-download.png)
![Local browse](resources/05-local_browse.png)
![Upload](resources/06-upload.png)

Architecture
-------------

The project is layered by **testability seam**: what a package is allowed to
depend on decides what can be asserted about it. `internal/session`, `internal/jobs`
and `internal/ops` know nothing about the terminal; tview is touched only by
`internal/ui/*` and by the controller's own screen wiring (`internal/controller`
builds its forms and lists directly); `internal/model` is the only place that
touches the network.

```
                        +-----------------------------+
                        |    cmd/s3duck-tui/main.go   |
                        +--------------+--------------+
                                       |
                                       v
       +----------------------------------------------------------------+
       |                      internal/controller                       |
       |        screen wiring: keymap, action table, navigation         |
       |         (controller.go = browser, profiles.go = list)          |
       +--+-------------+------------+-------------+----------------+---+
          |             |            |             |                |
          v             v            v             v                v
   +-------------+ +----------+ +---------+ +--------------+ +-------------+
   |   session   | |    ops   | |   jobs  | |  internal/ui | |   config    |
   |  Pane:      | | Download | | Queue,  | |  shell   -+  | | ~/.config/  |
   |  location,  | | Upload   | | badge,  | |  widgets  |  | | s3duck-tui/ |
   |  listing,   | | Copy     | | notices | |  flows    |  | | config.json |
   |  selection, | | Sync     | |         | |  screens  |  | +-------------+
   |  filter,    | | over     | |         | |           |  |
   |  history    | | narrow   | |         | |           |  |
   |             | | ports    | |         | |           |  |
   +------+------+ +----+-----+ +---------+ +-----------+--+
          |             |                               |
          +------+------+                               v
                 |                             +-----------------+
                 v                             |      tview      |
       +-------------------+                   +-----------------+
       |   internal/model  |
       |  AWS SDK v2 wrap  |
       +---------+---------+
                 |
                 v
       +-------------------+
       |  internal/utils   |
       |  SplitFunc,       |
       |  RandStr,         |
       |  CopyToClipboard  |
       +-------------------+
```

The arrows are the layering, not every import: the controller also calls
`internal/model` and `internal/utils` directly, and `internal/ui/flows` drives
`internal/ops` and `internal/jobs`.

`internal/ops` holds the long-running operations (download, upload, copy/move,
sync) as plain values over narrow interfaces, so their orchestration —
overwrite decisions, the worker pool, cancellation, verification, the retry
set — is unit-tested against a fake store rather than a live endpoint. `internal/ui/flows` holds the
single runner that drives any of them: confirmation, job registration, the
Background/Cancel modal, the progress throttle and the summary, written once.
`internal/ui/shell` owns every overlay (handles, not page names) and the UI
update queue that never blocks. See [REFACTOR.md](REFACTOR.md).

### Packages

| Path | Responsibility |
| --- | --- |
| `cmd/s3duck-tui/main.go` | Thin entrypoint: parses the command line, prints `--help` / `--version`, validates the launch against the profiles (exit status 2 on a mistake), then runs the controller's tview loop. |
| `internal/cli` | The command line and the `s3://` URI form: argument parsing (flags before or after the location), `ParseS3URI` / `FormatS3URI`, notification modes, the usage text. No UI, no network. |
| `internal/controller` | Application state and event handling. Owns the action table, modal flows (create/edit profile, create bucket/folder, download, upload, overwrite prompt, summary, delete confirmation), listing order, selection scoping per `bucket:path`, and goroutine→UI marshalling. Split by feature: `sync.go` (directory sync), `objectmeta.go`, `versions.go`, `duplicates.go`, `crossprofile.go`, `columns.go`, `edit.go`, `overwrite.go`, `previewfmt.go` (the preview's decoders and formatters), `difflocal.go` (object vs local file), `archive.go` (download as one archive), `marks.go` (mark by pattern / invert), `restores.go` (the Glacier restore tracker), `bucketconfig.go` (the bucket dashboard and tag editor), `export.go` (profile export), plus `keymap.go` (bindings as data), `listing.go` (streamed, cancellable listings), `opresult.go` (the failure ledger behind the delete, batch-rename and trash-restore reports; everything the runner drives reports through `ops.Result`), `localpane.go` (the local-filesystem pane), `usage.go` (the du browser), `preview.go` (preview + version diff), `match.go` (the text/glob/regex query used by the filter, the search and sync excludes), `guard.go` (read-only and profile identity), `trash.go`, `verify.go`, `notify.go`, `audit.go` and `options.go`. |
| `internal/session` | One browser pane: location (bucket, prefix, local dir), object map, selection sets per listing scope and filter, all behind one mutex, plus the back/forward history, which only the UI goroutine touches and so has no lock. The controller holds `panes [2]*session.Pane`; navigations, listings and transfers capture what they act on — the pane, or its bucket — on the UI goroutine, so a `Tab` cannot redirect them. No tview. |
| `internal/ops` | Long-running operations (`Download`, `Upload`, `Copy` for copy and move, `Sync`, `Archive` for a download into one .tar.gz/.zip) as values implementing `Operation`. They depend only on narrow ports (`Fetcher`, `Pusher`, `Checker`, `FS` and `Gate` in `ports.go`, `Mover` in `copy.go`, `Opener` in `archive.go`; declared consumer-side; `Fetcher`, `Pusher`, `Checker`, `Mover` and `Opener` are satisfied by `*model.Model`, `FS` by `ops.OSFS` and `Gate` by `*jobs.Queue`; `Pusher` is `UploadTargets`, which sends exactly the list it is given) and on a `Reporter` they emit progress and questions through — so an overwrite prompt is a value, not a call into tview. Unit-tested against a fake store. No tview. |
| `internal/jobs` | The background transfer queue: `Job`/`Queue`/`View`, the concurrency gate, the header's transfer badge and the completion notices. No tview. |
| `internal/ui/shell` | Every overlay and every UI update made off the UI goroutine. `Push` returns a `*Layer` whose page name nobody else holds, so two flows cannot destroy each other's modal; `Do` appends to a pending list and returns, so it never blocks from any goroutine (`QueueUpdateDraw` waits on the event loop, and calling it *from* the event loop deadlocks). |
| `internal/ui/flows` | The single `Runner` that drives any `ops.Operation`: confirmation, job registration, the Background/Cancel progress modal, the redraw throttle, and the summary with its Retry and Export buttons. Plus the pure rate/ETA and report-text helpers. |
| `internal/ui/screens` | Self-contained overlay screens that own their own widgets — currently the transfers panel. |
| `internal/ui/widgets` | Pure tview construction. Builds the main flex layout (object list + details panel), modal helper, profile form, local-file browser, hotkeys / about pop-ups. Contains the version string. |
| `internal/model` | S3 layer (`model.go` plus `sync.go`, `object.go`, `versions.go`, `copy.go`, `content.go`, `conflicts.go`, `write.go` for per-profile write options and MIME derivation, `checksum.go` for integrity, `local.go` for the local pane's listings and upload expansion, `bucketconfig.go` for the dashboard's reads and their error classification). Wraps `s3.Client`, `s3manager.Downloader/Uploader`, custom endpoint resolver, a static-credentials provider (or, with `aws_profile`, the SDK's own resolution through `WithSharedConfigProfile`), and TLS skip-verify. Exposes high-level operations: `List` / `ListStream`, `ListBuckets`, `ListObjects` / `ListObjectsStream`, `DownloadTarget`, `SafeLocalPath`, `Upload` / `PrepareUpload` / `UploadTargets`, `CopyObject` / `CopyObjectSized`, `CopyKeys` / `MoveKeys`, `Conflicts` / `PlannedCopyKeys`, `CrossCopy`, `GetObjectContent` / `PutBytes` / `CurrentETag`, `OpenObject`, `HeadObject`, `PutObjectMeta`, `ObjectTags` / `PutObjectTags`, `SetStorageClass`, `RestoreObject`, `ListVersions`, `RestoreVersion`, `DeleteVersion`, `DownloadVersion`, `GetVersionContent`, `GetObjectHead`, `ObjectChecksum` / `VerifyLocalFile` / `LocalChecksum` / `RemoteChecksums`, `MimeForName` / `MimeForFile`, `WalkDir` / `LocalTargets`, `ResolveDownloadObjects`, `Delete`, `DeleteKey`, `DeleteBucket`, `UploadFile`, `WalkLocal` / `WalkLocalTree`, `ListRemoteEntries`, `EmptyBucket`, `CreateBucket`, `CreateFolder`, `MakeBucketPublic`, `BucketConfig` / `PutBucketTags`, `ListMultipartUploads` / `AbortMultipartUpload`, `PresignGetURL`, `GetBucketLocation`, `RefreshClient`, `NormalizePrefix` / `NormalizeTypedPrefix`. The calls that reach S3 take a `context.Context`, except `CreateBucket`, `DeleteBucket`, `CreateFolder`, `MakeBucketPublic`, `GetBucketLocation`, `RefreshClient`, `PresignGetURL` and the multipart-upload listing/abort. Taking one is not the same as being cancellable: some short controller flows still pass `context.Background()` (a single rename, metadata and storage-class rewrites, version actions, the editor's save, the overwrite pre-scans). Implements `progressReader` / `progressWriterAt` for live byte-count progress. |
| `internal/utils` | Small helpers: path-split rune predicate (`SplitFunc`), random string (`RandStr`), clipboard write (`CopyToClipboard`). |
| `internal/config` | JSON profile storage in `~/.config/s3duck-tui/config.json` — load, write, append, copy, delete, and `SaveLocation`, which patches only one profile's `last_bucket` / `last_prefix` on disk and never writes a config that failed to load; auto-creates the directory (`0700`) and file (`0600`) on first run. An explicit save over an unreadable file first moves it aside to `config.json.bak` (then `.bak.1`, `.bak.2`, …, never overwriting an earlier backup). `awsshared.go` parses `~/.aws/credentials` / `~/.aws/config` for the profile import. |
| `e2e` | The end-to-end suite (build tag `e2e`): builds the real binary, drives it in a tmux session per test against MinIO, and asserts on what the server and the disk hold. `docker-compose.yml`, `Dockerfile` and `run.sh` make it hermetic (`make e2e`). |

### Data flow

1. **Startup** — `main` builds a `Controller`, which builds a `View` and loads `Params` from `internal/config`. The profile list is rendered first.
2. **Open profile** — selecting a profile goes through `modelConfigFor` (the single place a stored profile becomes a connection: credentials, throughput cap, write options) and `model.NewModel`, which builds the AWS config — custom endpoint resolver, then either static credentials or the SDK's own resolution for the profile's `aws_profile`, plus a per-phase-timeout HTTP client with optional `InsecureSkipVerify`. If the profile remembers a location, it is reopened there.
3. **Browse** — selecting a bucket triggers `RefreshClient` (resolves bucket region via `GetBucketLocation`, rebuilds the client; on failure the old client stays). Subsequent navigation uses `ListStream(ctx, prefix, bucket, onPage)` with `Delimiter="/"`, rendering folders + files as pages arrive; `Esc` cancels and whatever arrived stays browsable. A pane pointed at a local directory reads it with `WalkDir` instead.
4. **Transfer** — long-running operations (download / upload / copy / sync, and the delete / scan flows) run in goroutines with a `context.Context` that the cancel button on the progress or wait modal can cancel. Progress is reported through `shell.Do`, which never blocks whichever goroutine calls it; failures accumulate in an `ops.Result` (download, upload, copy/move, sync) or, for delete, batch rename and trash restore, an `opResult`, so the final report can export them and, where a retry makes sense, re-run exactly what failed.
5. **Selection scope** — multi-select state is keyed by `bucket:path`, so selections survive navigation in and out of folders.
6. **Sync** — `Ctrl+E` scans both sides (local tree and/or remote prefixes), drops anything matching the run's exclude patterns, optionally resolves per-file checksums, diffs them with the pure `planSync`, shows the resulting plan, and only then applies it through the same transfer-job machinery as downloads and uploads. `=` runs the same diff read-only across the two panes, by name and size only.

### Command line

```
s3duck-tui [flags] [s3://bucket[/prefix/ | /key]]

  -p, --profile NAME   open this profile straight away
  -c, --config FILE    use FILE instead of ~/.config/s3duck-tui/config.json
      --notify MODE    auto | off | command | osc9 | osc777 (also S3DUCK_NOTIFY)
  -v, --version        print the version and exit
  -h, --help           print this help and exit
```

Flags may come before or after the location, and `--` ends them. A location
opens in place of the profile's remembered one, once; `Ctrl+P` and reopening
the profile behave as usual. It needs `--profile` unless only one profile is
configured. A mistyped profile, a missing `--config` file or an unknown flag
is reported on stderr with exit status 2 before the TUI starts. `--config`
chooses a profile list only: `keys.json`, the activity log and the restore
tracker stay where they always are.

#### Notifications

A finished background job and a Glacier restore becoming readable ring the
bell and post a notification, chosen with `--notify` or `S3DUCK_NOTIFY`:

- `auto` (default) — `notify-send` on Linux/BSD when a display is available
  (`DISPLAY` / `WAYLAND_DISPLAY`), `osascript` on macOS; otherwise nothing
  beyond the bell, which is what an SSH session gets;
- `command` — the same programs, without the display check;
- `osc9` / `osc777` — an escape sequence the terminal turns into a
  notification (iTerm2, WezTerm, kitty, foot, Ghostty, Windows Terminal…),
  which also works over SSH. Inside tmux it is wrapped for passthrough, which
  needs `set -g allow-passthrough on` (tmux 3.3+). Opt-in, because a terminal
  that does not understand it shows nothing at best;
- `off`.

Object names are stripped of control characters before they reach a
notification, so a key cannot smuggle escape sequences to the terminal. The
first failed `notify-send` is noted in the activity log.

### Configuration

Profiles live in `~/.config/s3duck-tui/config.json` as a JSON array of:

```json
{
  "name":         "minio-local",
  "base_url":     "https://s3.example.com",
  "region":       "us-east-1",
  "access_key":   "AKIA...",
  "secret_key":   "...",
  "session_token": "",
  "ignore_ssl":   false,
  "download_dir": "~/Downloads/s3",
  "max_bytes_per_sec": 0,
  "bookmarks": [{"name": "photos/2024/", "bucket": "photos", "prefix": "2024/"}]
}
```

`region` is optional for non-AWS endpoints, and is what requests are signed with until a bucket is entered. Entering a bucket, on any endpoint, asks `GetBucketLocation` for that bucket's region and rebuilds the client for it (for an `amazonaws.com` URL, on the SDK's regional endpoint rather than `base_url`); the client keeps that region afterwards, bucket creation included. If the lookup fails, the client keeps the region it had and an error toast says so — on a backend without `GetBucketLocation`, at every bucket entry. `download_dir` is optional: without it `Ctrl+D` asks with a directory picker each time (an open local pane takes precedence either way), while verify (`V`, which looks for the local copy there), version downloads, Export list and the sync form's default use `~/Downloads`, and a new local pane opens there if it exists. A leading `~` is expanded to the user's home directory. `max_bytes_per_sec` (0 = unlimited) caps combined upload/download throughput. `bookmarks` are managed in-app (Ctrl+B); both fields are omitted from the file when unset. `session_token` is only needed for static temporary credentials and is easiest to obtain via **Ctrl+I → import from `~/.aws`** on the profiles screen; it is omitted when empty. Note that `secret_key` and `session_token` are stored in plaintext (the file is `0600`; the mode is re-applied at every start, so a hand-made file is tightened too). A `null` entry in the list is ignored.

#### Per-profile options (`o` on the profiles screen)

Behaviour settings live on a second form, because the connection form is
already as tall as a small terminal can show. The last two fields below are
not on it: they are recorded automatically.

```json
{
  "aws_profile":       "sso-dev",
  "anonymous":         false,
  "public_buckets":    ["noaa-ghcn-pds", "sentinel-cogs"],
  "read_only":         true,
  "trash":             true,
  "trash_prefix":      ".s3duck-trash/",
  "no_mime_detect":    false,
  "sse":               "aws:kms",
  "sse_kms_key_id":    "arn:aws:kms:...",
  "checksum_algo":     "CRC32C",
  "verify_downloads":  true,
  "transfer_workers":  8,
  "last_bucket":       "reports",
  "last_prefix":       "2026/"
}
```

- **`aws_profile`** delegates credentials to the AWS SDK's own resolution for
  that `~/.aws` profile instead of the static keys. This is what makes SSO,
  assume-role and `credential_process` profiles usable — and what keeps them
  refreshed. Nothing is stored here but the profile's name.
- **`anonymous`** sends every request unsigned, for public buckets: the keys
  and `aws_profile` are ignored (setting both is refused on the form). S3 has
  no anonymous bucket listing, so **`public_buckets`** is the list shown on
  the bucket screen — comma-separated on the form; with none, the bucket
  screen says so and `g` (`s3://bucket/`) reaches any public bucket. The
  profile export writes `--no-sign-request` / an empty-key `mc` alias / a
  keyless rclone remote.
- **`read_only`** refuses every write for this profile (delete, upload, copy,
  move, rename, paste, undo, metadata, storage class, version restore/delete,
  purge, undelete, bulk edits, upload links, bucket versioning and tags, sync to
  remote, trash operations). Browsing, searching and downloading still
  work, and the header shows `[READ-ONLY]` in red.
- **`trash`** turns delete — the browser's and the duplicate finder's — into a
  server-side move under `trash_prefix` (default `.s3duck-trash/`), inside a
  timestamped folder that preserves the original key. `R` restores, asking
  first if the original key is occupied again. Deleting a whole bucket, a
  version or something already in the trash, and emptying the trash, delete
  for real; a sync drops the trash prefix from both sides, so its
  delete-extraneous pass (real deletes too) never reaches it.
- **`no_mime_detect`** switches off Content-Type derivation on upload, for
  buckets whose types are managed elsewhere.
- **`sse`** / **`sse_kms_key_id`** encrypt what this app writes (`AES256` or
  `aws:kms`): uploads, edits, folder markers, cross-profile copies (with the
  destination profile's setting) and every server-side copy, single-request or
  multipart. Without them a copy gets the destination bucket's default, except
  a same-bucket copy whose source headers were read first (a single object, a
  metadata or storage-class rewrite, a version restore, anything above 5 GiB),
  which keeps the source's encryption; copies up to 5 GiB sized from a folder
  listing or a sync plan skip that read and get the bucket default.
- **`checksum_algo`** asks S3 to verify an additional checksum on uploads,
  edits, folder markers, cross-profile copies and single-request copies — not
  on multipart copies above 5 GiB, because `UploadPartCopy` cannot send one.
- **`verify_downloads`** re-reads each downloaded file and compares it against
  the object's whole-object checksum or, failing that, its ETag when that is a
  plain MD5 (single-part, not SSE-KMS). A multipart object's checksum-of-parts
  and an SSE-KMS object's ETag are reported as not verifiable rather than as a
  mismatch.
- **`transfer_workers`** is how many objects one download or one sync moves
  at a time, from 1 to 16; omitted (or blank on the form) means 4. It widens
  each job, not the queue — at most two jobs still move bytes at once — and
  `max_bytes_per_sec` still caps them all together. Past a dozen or so a single
  endpoint tends to answer with throttling rather than throughput.
- **`last_bucket`** / **`last_prefix`** are saved when you leave the profile
  with `Ctrl+P` and when you quit with `Ctrl+Q` — not on every navigation, and
  not on `Ctrl+C`, which exits without saving — so the profile reopens where it
  was left (on the bucket list they are cleared; with the local pane active the
  previous location is kept). The save re-reads `config.json` and rewrites
  only these two fields of that one profile, so it neither clobbers what
  another running instance saved nor writes over a config that failed to load.

#### Keymap (`~/.config/s3duck-tui/keys.json`)

Every browser-screen action has a name (see *Action names* below); the file
rebinds any of them. A missing file means the defaults, a malformed one is
reported at startup and ignored, and a binding that cannot be used leaves that
action's default in place.

```json
{
  "leader": ",",
  "keys": {
    "preview": "z",
    "usage": "Ctrl+J",
    "delete": ""
  },
  "leader_keys": {
    "activity-log": "L"
  }
}
```

`keys` sets an action's direct chord and `leader_keys` its chord behind the
leader; setting either drops that action's default in the same namespace, so a
rebind never leaves the old key working too. `usage` and `verify` ship with one
of each, and any action may be given either.

A chord is one character or one key name, optionally with modifiers. `Alt+`
combines with anything. `Ctrl+` works only with a letter (`Ctrl+J`,
`Ctrl+Alt+J`). `Shift+<letter>` is that letter's upper case (`Shift+s` is `S`)
and is refused on anything else — write the shifted character itself (`>`,
not `Shift+.`). The key names are `Enter`, `Esc`/`Escape`, `Tab`, `Backtab`,
`Backspace`, `Del`/`Delete`, `Insert`, `Home`, `End`, `PgUp`, `PgDn`,
`Up`/`Down`/`Left`/`Right` and `F1`–`F12`, plus `Space` for the space
character; only `Alt+` combines with a key name (`Alt+Left`), and `Ctrl+` or
`Shift+` on one is refused with a warning. Modifiers and key names are
case-insensitive. An empty chord unbinds the action. Binding a chord another
action already holds takes it over, with a startup warning, and the other
action loses that key. The leader key (default `,`, changed with `"leader"`)
prefixes the second namespace, and pressing it shows what it offers in the
header.

##### Action names

The names `keys.json` accepts, with their default chords (the hotkey panel
shows chords and descriptions, not these names). The `-alt` actions are second
keys for the same command, so each can be rebound or unbound on its own.

| Action | Default key | Does |
| --- | --- | --- |
| `up` | `Backspace` | Go up one level (`..`) |
| `toggle-select` | `Space` | Mark / unmark the highlighted item |
| `select-all` | `Ctrl+S` | Mark everything visible |
| `clear-selection` | `Ctrl+X` | Clear the marks |
| `mark-pattern` | `+` | Mark the visible items matching a pattern |
| `unmark-pattern` | `-` | Unmark the visible items matching a pattern |
| `invert-marks` | `*` | Invert the marks on the visible items |
| `filter` | `/` | Filter the listing live |
| `cancel-listing` | `Esc` | Stop a running listing (otherwise Esc passes through) |
| `refresh` | `r` | Re-list from the server |
| `refresh-alt` | `F5` | Re-list from the server |
| `sort-key` | `s` | Cycle the sort key: name / size / date |
| `sort-dir` | `S` | Reverse the sort |
| `download` | `Ctrl+D` | Download |
| `upload` | `Ctrl+U` | Upload through the local file browser |
| `delete` | `Delete` | Delete (into the trash on a safe-delete profile) |
| `create` | `Ctrl+N` | New bucket / folder |
| `rename` | `Ctrl+R` | Rename; pattern rename when several are marked |
| `copy` | `Ctrl+Y` | Copy to a bucket + prefix; uploads from a local pane, downloads into one |
| `move` | `Ctrl+T` | Move to a bucket + prefix |
| `yank` | `y` | Copy to the object clipboard |
| `cut` | `x` | Cut to the object clipboard |
| `paste` | `p` | Paste the object clipboard here |
| `undo` | `u` | Undo the last move / rename |
| `search` | `Ctrl+F` | Recursive search |
| `properties` | `Ctrl+L` | Object properties |
| `presign` | `Ctrl+W` | Presigned share link |
| `goto` | `g` | Go to a path or `s3://` URI |
| `summary` | `Ctrl+G` | Size summary |
| `usage` | `G`, leader `u` | Usage browser |
| `preview` | `P` | Preview (ranged GET) |
| `verify` | `V`, leader `v` | Verify the local copy against the object |
| `versions` | `v` | Version history |
| `metadata` | `m` | Metadata and tags |
| `storage-class` | `c` | Storage class / Glacier restore |
| `edit` | `e` | Edit in `$EDITOR` |
| `duplicates` | `D` | Find duplicates |
| `sync` | `Ctrl+E` | Sync |
| `compare-panes` | `=` | Compare the two panes |
| `cross-profile-copy` | `>` | Copy to another profile |
| `bookmarks` | `Ctrl+B` | Bookmarks |
| `palette` | `Ctrl+K` | Command palette |
| `transfers` | `t` | Transfers panel |
| `history-back` | `[` | History back |
| `history-forward` | `]` | History forward |
| `history-back-alt` | `Alt+Left` | History back |
| `history-forward-alt` | `Alt+Right` | History forward |
| `dual-pane` | `Ctrl+O` | Toggle dual pane |
| `swap-pane` | `Tab` | Switch pane (dual-pane) |
| `local-pane` | `l` | Point the other pane at a local directory, or back |
| `profiles` | `Ctrl+P` | Back to profiles |
| `hotkeys` | `Ctrl+H` | Hotkeys panel |
| `about` | `Ctrl+A` | About |
| `quit` | `Ctrl+Q` | Quit (`Ctrl+Q` itself always quits, see *Hotkeys*) |
| `trash-restore` | `R` | Restore the marked items from the trash |
| `trash-empty` | leader `t` | Empty the trash |
| `activity-log` | leader `a` | Activity log |
| `bucket-config` | leader `b` | Bucket dashboard |
| `abort-uploads` | leader `m` | Abort incomplete multipart uploads |
| `diff-local` | leader `d` | Diff the object against its local copy |
| `download-archive` | leader `z` | Download as one .tar.gz / .zip |
| `restores` | leader `r` | Glacier restore tracker |
| `copy-uri` | leader `c` | Copy the `s3://` URI of the marked / highlighted items |
| `presign-upload` | leader `p` | Presigned upload link (PUT) |
| `purge` | leader `X` | Purge versions of the marked / highlighted items (permanent) |
| `deleted-objects` | leader `D` | Deleted objects under this folder (undelete) |
| `other-pane-profile` | leader `o` | Open a profile in the other pane |
| `export-listing` | leader `e` | Export this listing as CSV / JSON |

#### Activity log

Operations are appended to `$XDG_STATE_HOME/s3duck-tui/activity.log` (default
`~/.local/state/s3duck-tui/activity.log`, mode `0600`, rotated once at 2 MiB)
as `timestamp<TAB>profile<TAB>message`. Multi-item deletes, copies, moves,
batch renames and syncs write one line of counts rather than one per key, and
keymap warnings are recorded here too. The in-app view (`, a` or palette →
*Activity log*) still shows the current session only.

Hotkeys
-------------

`Ctrl+H` opens the in-app list of these. It scrolls (↑/↓, PgUp/PgDn, Home/End,
`j`/`k`) and sizes itself to the terminal, so the tail of the list is reachable
on a short one; `Esc`, `Enter`, `Tab`, `q` or `Ctrl+H` closes it.

**Profiles screen**

| Key | Action |
| --- | --- |
| ↑ / ↓ | Navigate |
| Enter | Open profile |
| Ctrl+N | Create profile |
| Ctrl+I | Import a profile from `~/.aws/credentials` / `~/.aws/config` (terminals send Ctrl+I as Tab, so Tab here does the same) |
| Ctrl+E | Edit profile (connection) |
| o | Per-profile options (credentials source incl. anonymous, read-only, trash, SSE, checksums, parallel transfers) |
| x | Export the profile for the AWS CLI / mc / rclone (secrets left out) |
| Ctrl+Y | Copy / clone profile |
| Ctrl+V | Verify profile (test connection) |
| Del | Delete profile |
| Ctrl+H | Hotkeys help |
| Ctrl+A | About |
| Ctrl+Q | Quit (asks first while transfers run, and cleans them up) |

**Browser screen**

| Key | Action |
| --- | --- |
| ↑ / ↓ | Navigate |
| Enter | Open folder / bucket |
| Backspace | Go up (`..`) |
| / | Filter the current listing live — text, `*.glob` or `re:regex` (Enter keeps it, Esc clears) |
| Esc | Stop a running listing (what has arrived stays browsable) |
| s / S | Sort: cycle name → size → date / reverse the direction |
| r / F5 | Refresh the current listing |
| Ctrl+F | Recursive search under the current prefix; Enter reveals a hit, `x` exports the hits |
| Ctrl+O | Toggle dual-pane (Midnight Commander style) |
| Tab | Switch active pane (dual-pane) |
| Ctrl+B | Bookmarks — go to / add current / remove |
| Ctrl+K | Command palette (incl. abort incomplete uploads, bucket config, activity log) |
| [ / ] | History back / forward (also Alt+← / Alt+→) |
| y / x / p | Clipboard: copy / cut / paste objects |
| u | Undo last move/rename |
| t | Transfers panel (background download/upload jobs) |
| Ctrl+P | Back to profiles |
| Ctrl+N | Create bucket / folder |
| Ctrl+D | Download current item or all selected (parallel; `transfer_workers` wide) |
| Ctrl+U | Open local FS browser to upload |
| Ctrl+E | Sync: local ⇄ this prefix, or this prefix → another bucket/prefix (dry-run plan first) |
| = | Compare the two panes (dual-pane, read-only; each through its own profile) |
| D | Find duplicates under this prefix (size + ETag); Enter reveals, d deletes a copy |
| e | Edit the highlighted object in `$EDITOR` (small text objects) |
| > | Copy marked objects/folders to a bucket in another profile (streams cross-endpoint) |
| Ctrl+G | Bucket / folder size summary |
| Space | Toggle selection on item |
| Ctrl+S | Select all visible |
| Ctrl+X | Unselect all |
| + / - | Mark / unmark the visible items matching a pattern (text, `*.glob`, `re:regex`) |
| * | Invert the marks on the visible items |
| Ctrl+R | Rename highlighted object, or batch/pattern rename when >1 marked |
| Ctrl+Y | Copy selected / marked objects to a destination bucket + prefix (cross-bucket) |
| Ctrl+T | Move selected / marked objects to a destination bucket + prefix (cross-bucket) |
| Ctrl+L | Object properties (name, size, modified, ETag, link) |
| v | Version history — restore / download / permanently delete a version; with several marked, purge their versions |
| m | Edit object metadata and tags (with several marked, or a folder: a bulk edit of all of them) |
| c | Change storage class, or request a Glacier restore (bulk, like `m`) |
| Ctrl+W | Copy presigned (time-limited) share link to clipboard (an anonymous profile copies the plain URL) |
| g | Go to a path or `s3://bucket/key` (starts with the current location) |
| Del / Delete | Delete marked objects, or the highlighted one (recursive for prefixes; into the trash on safe-delete profiles) |
| l | Point the other pane at a local directory (`Ctrl+Y` uploads from it) |
| P | Preview the highlighted object (ranged GET; gzip/zstd decoded, JSON indented, CSV/TSV as columns, tar listed, hexdump for binaries; `r` toggles raw) |
| G | Usage browser — drill down by size, with a storage-class breakdown; `x` exports a level |
| V | Verify the local copy against the object's checksum |
| R | Restore the marked items from the trash (safe-delete profiles) |
| , | Leader key — press it, then one of the keys it lists in the header |
| Ctrl+H | Hotkeys help (generated from your keymap) |
| Ctrl+A | About |
| Ctrl+Q | Quit (asks first while transfers run, and cleans them up) |

**Leader namespace** — press `,` (the leader), then one of:

| Key | Action |
| --- | --- |
| , a | Activity log |
| , b | Bucket dashboard (versioning, encryption, object lock, public access block, tags, lifecycle, replication, CORS, website, notifications, logging, policy; `t` edits tags, `v` enables / suspends versioning) |
| , c | Copy the `s3://` URI of the marked items, the highlighted one, or the current folder |
| , d | Diff the highlighted object against its local copy (`n` / `N` jump between changes) |
| , m | Abort incomplete multipart uploads |
| , p | Presigned upload link (PUT) for a key here |
| , X | Purge versions of the marked / highlighted items: every version, or old versions only (permanent) |
| , D | Deleted objects under this folder: Space marks, `u` undeletes, `v` opens the history |
| , o | Open a profile in the other pane (dual panes on two accounts) |
| , e | Export this listing — what the filter shows, in the order shown — as CSV or JSON |
| , t | Empty the trash (safe-delete profiles) |
| , u | Usage browser (same as `G`) |
| , v | Verify the local copy (same as `V`) |
| , r | Glacier restore tracker (Enter reveals, `r` checks now, Del forgets, `c` clears finished) |
| , z | Download the marked items, or the highlighted one, as one .tar.gz / .zip |

Pressing the leader lists its namespace in the header, so it needs no memorising.

The browser-screen and leader bindings above are defaults: each belongs to a
named action (see *Action names* under *Configuration*) that
`~/.config/s3duck-tui/keys.json` can rebind, and `Ctrl+H` shows the list as it
is actually bound. The rest is fixed: the profiles screen's keys, and the list
widget's own ↑/↓, Enter, PgUp/PgDn and Home/End. `Ctrl+Q` is caught before the
keymap on both screens, so it always quits: rebinding `quit` adds a key rather
than moving it, unbinding it removes nothing, and binding `Ctrl+Q` to another
action has no effect.

Testing
-------------

```
make test          # unit tests
make test-race     # unit tests with the race detector
make check         # gofmt, vet (all three tag sets), build, race tests
make staticcheck   # the remaining lint CI runs (needs staticcheck installed)
```

The unit suite needs no network: it covers the pure functions (planners,
matchers, formatters, parsers) plus a set of guards that assert the code still
matches what is written down — every default key binding appears in the key
column of some hotkey table above, the table keys are really bound (cells with
`/` and a short whitelist are skipped, and neither check pairs a key with its
action), every profile field is documented, the `keys.json` example names real actions, every action name
appears in the *Action names* table (and that table names no others), every
backticked `internal/…` or `cmd/…` path the docs name still exists, the version
the app reports matches the one the Makefile and `build-deb.sh` package, the
`.deb` control file still carries the version and installed-size placeholders
both packaging paths substitute, and no confirmation handler in `internal/controller` compares the dialog's button
label to a hand-typed OK literal. The last three exist because each guards
something stated in more than one place that had already drifted once.

What a request looks like on the wire is unit-tested too, against an
`httptest` server standing in for S3: an anonymous profile's requests carry no
`Authorization` header (and a keyed profile's do), its region comes from an
unsigned `HEAD` rather than `GetBucketLocation`, a presigned upload link signs
the host and nothing else, the versioning switch sends the status it says, and
a finished Glacier restore posts its notification through the real check
round.

Behaviour that can only be observed against a real server lives in a tag-gated
integration suite, in two halves. `internal/model` covers the S3 calls
themselves: that restoring a version *adds* one rather than rewinding, that a
storage-class change preserves metadata, that an uploaded object's stored
Content-Type is the derived one, that a checksum round-trips and a tampered
file is caught, that a multipart object's checksum is not mistaken for a
mismatch, that an upload sends exactly its target list, that a prefix delete
takes the subtree and not the sibling sharing its name stem, that a presigned
link actually fetches, and — for the archive — that an object which disappears
between the listing and its turn is left out and reported while the archive
stays readable.
`internal/controller` covers the flows the app *composes* out of those calls —
a full sync (scan both sides, diff, apply, and confirm the destination mirrors
the source, deletes included) and safe delete's round trip through the trash.
Those are not model calls, so the model suite cannot see them; they are also
where the decisions that can lose data live. It also covers the readers that
only a server can feed honestly: the preview formatter against a real
truncated head, the restore tracker against a real missing-key error, and the
bucket dashboard against real versioning, tags, policy and lifecycle rules.

```
make test-integration     # starts a throwaway MinIO in Docker, runs, tears down
```

To point it at an endpoint you already have:

```
S3DUCK_TEST_ENDPOINT=https://s3.example.com \
S3DUCK_TEST_ACCESS_KEY=... S3DUCK_TEST_SECRET_KEY=... \
make test-integration-run
```

Without `S3DUCK_TEST_ENDPOINT` the integration tests skip, so a tagged run on a
machine with no server is a no-op rather than a failure.

Neither suite presses a key. The end-to-end suite (`e2e/`, build tag `e2e`)
does: it builds the real binary, runs it in a real terminal (a tmux session per
test), types at it, reads the screen back, and then checks what MinIO and the
disk actually hold. Each test gets its own HOME (config, downloads, activity
log), its own tmux server and its own bucket. The scenarios are the ones where
a dialog's answer decides what happens to data:

- browse, download one object, quit — the exit save records the location and
  rewrites nothing else in the profile list
- rename, then undo
- a move answered with *Skip existing*, then undone — file and folder variants;
  the kept source and the kept destination must both survive
- the local pane uploads exactly the highlighted file, or exactly the marked
  ones — never the directory they sit in
- a `config.json` that fails to parse is reported and left byte-for-byte alone
- a folder delete spares the sibling that shares its name stem
- Ctrl+N creates the folder marker
- a local → remote sync with delete-extraneous and an exclude, on a safe-delete
  profile: the mirror matches, the excluded paths survive on both sides, and
  the trash is untouched
- each feature of the 0.11.0 round through its own keys: a gzipped JSON
  preview and its raw toggle, mark-by-pattern then invert then archive (the
  `.tar.gz` is opened and its entries checked), a local diff, the dashboard's
  tag editor (read back from the bucket, not the screen), the restore list,
  and a profile export written to a file
- the inputs that should make a dialog *say* something rather than do it: a
  pattern matching nothing, a regex that does not compile, a file name that is
  really a path, a zero-byte object, a missing local file — and a key or
  profile name that looks like a colour tag, which is what caught the escaping
  bug the round fixed
- each feature of the 0.12.0 round through its own keys or flags: a launch
  argument opening a folder and an object; go-to by URI and by `..`, then copy
  URI; an upload link that a plain unauthenticated `PUT` really writes through
  (and one on an existing key that says it will replace it); a signed `Ctrl+W`
  link that a plain `GET` follows; the versioning switch, read back from the
  server; an anonymous profile made on the options form, checked with `Ctrl+V`
  (which names the private bucket that refuses), browsing a public bucket and
  refused by a private one; a read-only profile refusing the upload link and
  the versioning switch; and a backgrounded download's notification, through
  a stand-in `notify-send` and as raw OSC 777 bytes captured with
  `tmux pipe-pane`
- the versioned-bucket round, against a bucket with versioning on: a deleted
  key found with `, D` and undeleted (and only the marked one); a folder purged
  of every version and marker while the sibling sharing its name stem keeps
  its one; `v` over two marked objects keeping only their current versions; a
  bulk tag edit merged into each object's own tags; and a versioned bucket with
  history deleted from the bucket list, which used to fail with
  BucketNotEmpty — each read back from `ListObjectVersions` or the tag set
- the 0.14.0 round: a filtered listing exported to CSV and parsed back (a key
  with a comma intact); a file moved between panes on two profiles — the
  destination holds it, the source does not, an unmarked neighbour stays; the
  two panes compared through their own profiles; a throttled download
  cancelled part-way, leaving only a hidden partial, then resumed to the
  object byte for byte; and a quit under a running download and upload that
  aborts the upload, keeps a resumable partial, and a restarted app that
  resumes it

```
make e2e                  # everything in Docker: MinIO + a runner with Go and tmux
make e2e E2E_RUN=Undo     # a go test -run pattern
```

`make e2e` is hermetic: MinIO (on a tmpfs) and the runner share a private
compose network with no published ports, the working tree is mounted
read-only, and the containers, network and volumes are removed afterwards, pass
or fail (the runner image is kept as a build cache)
([e2e/docker-compose.yml](e2e/docker-compose.yml), [e2e/run.sh](e2e/run.sh)).
To drive an endpoint you already have from the host instead (Go and tmux
needed), set `S3DUCK_TEST_ENDPOINT` and run
`go test -tags e2e -count=1 -v ./e2e/`; `S3DUCK_E2E_BINARY` runs the suite
against a prebuilt binary rather than the working tree — which is how each data
scenario was checked to fail against the code from before the fix it pins.

To see what all three suites reach together, build the binary with coverage
and give every suite one counter directory (a MinIO at
`S3DUCK_TEST_ENDPOINT`; with `GOCOVERDIR` set, the e2e harness quits each app
with `Ctrl+Q` instead of killing it, since a coverage build writes its counters
only on a normal exit):

```
mkdir -p cov/unit cov/e2e
go test -tags integration -cover -coverpkg=./... ./... -args -test.gocoverdir=$PWD/cov/unit
go build -cover -coverpkg=./... -o cov/s3duck ./cmd/s3duck-tui
GOCOVERDIR=$PWD/cov/e2e S3DUCK_E2E_BINARY=$PWD/cov/s3duck go test -tags e2e -count=1 ./e2e/
go tool covdata textfmt -i=cov/unit,cov/e2e -o cov/all.out && go tool cover -func=cov/all.out
```

[CI](.github/workflows/ci.yml) runs the lint/unit job, the MinIO integration
job, the end-to-end job, and a cross-build matrix covering every platform the
Makefile ships binaries for.

Building
-------------

```
go build ./cmd/s3duck-tui
```

Statically linked (no libc dependency):

```
go build -ldflags "-linkmode external -extldflags -static" ./cmd/s3duck-tui
```

Building a deb package
-------------

What it takes: Go (the version `go.mod` names, 1.23 or newer) and `dpkg-deb`,
which ships with `dpkg` on any Debian or Ubuntu system. `build-deb.sh` links a
native-architecture binary statically through the C toolchain, so it also
wants `build-essential`; cross-architecture builds, and the Makefile's `deb-*`
targets, build with CGO off and need nothing more.

```
sudo apt-get install dpkg build-essential
```

Build (amd64 by default):

```
./build-deb.sh          # amd64
./build-deb-arm64.sh    # arm64
./build-deb.sh riscv64  # riscv64
```

Or via the Makefile, which builds all four:

```
make debs
```

Building a FreeBSD binary
-------------

```
GOOS=freebsd GOARCH=amd64 go build ./cmd/s3duck-tui
```

Building for RISC-V (LicheeRV Nano)
-------------

CGO is off for every cross target, so riscv64 needs no extra toolchain:

```
make licheerv           # static binary into dist/
make deb-riscv64        # riscv64 .deb into build/
./build-licheerv.sh     # same binary, standalone script
```

License
-------------

See [LICENSE](LICENSE).
