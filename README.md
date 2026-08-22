S3Duck-TUI 🦆
======

[![CI](https://github.com/nexusriot/s3duck-tui/actions/workflows/ci.yml/badge.svg)](https://github.com/nexusriot/s3duck-tui/actions/workflows/ci.yml)

Terminal UI client for S3-compatible object storage. A TUI implementation of [S3Duck](https://github.com/nexusriot/s3duck) built in Go on top of [tview](https://github.com/rivo/tview) / [tcell](https://github.com/gdamore/tcell), using the AWS SDK for Go v2.

Works with AWS S3 and any S3-compatible service (MinIO, Ceph RGW, Yandex/VK Cloud, Backblaze B2, etc.) by pointing at a custom endpoint URL.

Features
-------------

1. Multi-profile support (create / edit / delete / clone / verify)
2. Bucket browsing with folder-style navigation (delimiter `/`), a live in-listing name filter (`/`, narrows as you type), and recursive search under the current prefix (Ctrl+F) whose results jump straight to the matching object
3. Bucket creation (private or public-read) and deletion
4. Folder creation (zero-byte `prefix/` markers)
5. Recursive download of files and folders with **4-worker parallel downloads**: overwrite conflicts are resolved sequentially first (Overwrite / Skip / Overwrite All / Skip All / Cancel), then approved objects download concurrently — an existing file is replaced only after its download fully succeeded, so a canceled or failed run never destroys local data; live progress shows worker count, per-file active names, combined byte progress, and **transfer speed + ETA**; configurable per-profile destination directory (defaults to `~/Downloads`, supports a leading `~`)
6. Multi-select for batch download, copy, move, rename and **delete** (Space, Ctrl+S all, Ctrl+X none)
7. Upload with a built-in, icon-styled local filesystem browser; preserves directory tree, creates markers for empty folders
8. Bucket / folder size summary (Ctrl+G)
9. Object properties (size, ETag, storage class, last modified) in the side panel; presigned (time-limited) share links for private objects (Ctrl+W, or the "Presign Link" button in properties — 15m / 1h / 24h / 7d, capped at the SigV4 7-day max)
10. Clipboard yank of profile data (Ctrl+Y)
11. Server-side copy / move / rename, recursive for folders, multi-select aware, **cross-bucket** (pick any destination bucket) with live object rate + ETA (Ctrl+Y copy, Ctrl+T move, Ctrl+R rename)
12. **Dual-pane (Midnight Commander style)** layout (Ctrl+O toggle, Tab to switch panes); copy/move defaults its destination to the other pane
13. **Batch / pattern rename** of multiple marked objects (Ctrl+R with >1 selected): `{name}` / `{ext}` / `{n}` tokens plus an optional find→replace
14. **Bookmarks** of bucket+prefix locations per profile (Ctrl+B) and **back/forward navigation history** (`[` / `]`, or Alt+←/→)
15. **Command palette** (Ctrl+K) — fuzzy launcher for every action
16. **Per-profile bandwidth throttle** (`max_bytes_per_sec`) capping combined upload/download throughput
17. **Background transfer queue** — downloads/uploads can run in the background (the progress modal has a **Background** button); a transfers panel (`t`) shows live per-job progress/speed/ETA, cancel, and clear, with at most 2 byte-transfers running at once
18. **Object clipboard** — yank/cut objects (`y`/`x`) and paste (`p`) into any folder or the other pane (cross-bucket aware)
19. **Undo** the last move/rename (`u`)
20. **Search across all buckets** (checkbox in the Ctrl+F prompt); results jump straight to the matching object in its bucket
21. **Abort incomplete multipart uploads** and a **read-only bucket-config dashboard** (versioning / encryption / object-lock / region), via the command palette
22. **In-session operation activity log** (command palette)
23. **Sync** (Ctrl+E) — mirror in any of three directions: local → remote, remote → local, or **remote → remote** (bucket/prefix to bucket/prefix, server-side copies). Always preceded by a mandatory dry-run plan (create / update / delete, per-file reason, total bytes); optional deletion of extraneous objects at the destination. Applied by a 4-worker pool, writes before deletes
24. **Temporary AWS credentials** — `session_token` support (assume-role / SSO / MFA) plus one-key import of profiles from `~/.aws/credentials` and `~/.aws/config` (Ctrl+I on the profiles screen)
25. **Sort** the listing by name / size / date, ascending or descending (`s` cycles the key, `S` reverses); **refresh** with `r` or F5
26. **Object versioning** (`v`) — full history for the selected object including delete markers; restore an old version as current (a copy to the top of the history, so nothing is lost), download any version, or permanently delete one
27. **Object metadata & tags** (`m`) — edit Content-Type / Cache-Control / Content-Disposition / Content-Encoding and `x-amz-meta-*` pairs, plus the object tag set; only the halves you actually changed are written
28. **Storage class & Glacier restore** (`c`) — move an object between storage classes in place (metadata preserved) and request a temporary restore of an archived object, with days and retrieval tier; restore state is shown wherever the object is inspected
29. **Retried uploads** — the multipart uploader now backs off and retries transient failures instead of failing the whole transfer on one blip
30. **Listing columns** — size, modified date and storage class alongside the name, so the sort keys order by something visible. The layout follows the pane width: columns drop (class → date → size) rather than squeezing names into uselessness, and re-flow on terminal resize
31. **Edit in `$EDITOR`** (`e`) — open a small text object in your editor (the TUI suspends while it runs) and save straight back to the bucket; Content-Type, Cache-Control and friends, user metadata, storage class and tags are all preserved. If the object changed on the server while you edited, the save is refused and your version is kept in a temp file (the error names it). Guarded by a 1 MiB cap and a binary sniff
32. **Cross-profile copy** (`>`) — copy the marked objects or folders to a bucket in a *different profile*, streamed through the client (GET here → PUT there), so the two sides can be entirely different endpoints (MinIO → AWS, provider migration). Content headers, metadata and tags ride along (storage class deliberately not — class names aren't portable across providers); runs as a cancellable background transfer
33. **Duplicate finder** (`D`) — scan the current prefix recursively and browse groups of identical objects (matched by size + ETag), ordered by wasted bytes; reveal any copy in the browser or delete it with a confirmation. The oldest copy is marked as the likely original
34. **Pane comparison** (`=`) — read-only diff of the two dual-pane locations (left-only / differs / right-only), answering "are these two prefixes actually the same?" without transferring anything
35. **Overwrite confirmation for remote writes** — rename, batch rename, copy, move, paste, upload and cross-profile copy all check the destination first and name exactly what they would replace, offering **Overwrite**, **Skip existing** (when that still leaves something to do) or **Cancel**. Skipping a destination on a *move* leaves the source in place too, so nothing is ever deleted without having been written somewhere. Sync is exempt: its dry-run plan already lists every update before anything moves
36. **Copies above 5 GiB** — copy, move, rename, storage-class changes, metadata saves and version restores fall back to a concurrent multipart part-copy past the size where a single-request S3 copy is rejected, carrying content headers, metadata and tags across
37. Custom endpoints and self-signed TLS support (`ignore_ssl`)
38. Linux (amd64/arm64/armv7/riscv64), FreeBSD and macOS / Windows builds (statically linkable)

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

The project follows a classic **Model–View–Controller** layout. The controller is the only component aware of both the AWS SDK layer and the tview UI; the model is pure storage I/O, and the view is pure UI primitives.

```
                +-----------------------------+
                |   cmd/s3duck-tui/main.go    |
                |  controller.NewController() |
                |          .Run()             |
                +--------------+--------------+
                               |
                               v
+------------------------------------------------------------+
|                    pkg/controller                          |
|  - app state: current bucket, path, selection, sort        |
|  - keybindings, modals, forms, progress UI                 |
|  - orchestrates Model calls from goroutines, marshals      |
|    UI updates back via App.QueueUpdateDraw                 |
+----------+----------------------------+--------------------+
           |                            |
           v                            v
+----------------------+    +----------------------------+
|   pkg/view           |    |   pkg/model                |
|  tview widgets:      |    |  AWS SDK v2 wrapper:       |
|   - List / Frame     |    |   - ListBuckets / List     |
|   - Details TextView |    |   - CreateBucket/Folder    |
|   - Modals & Forms   |    |   - Delete (paged 1000)    |
|   - Hotkeys / About  |    |   - Upload  (s3manager)    |
|   - Local FS browser |    |   - DownloadTarget         |
+----------------------+    |   - GetBucketLocation      |
                            |   - PrepareUpload /        |
                            |     ResolveDownloadObjects |
                            |   - UploadFile / DeleteKey |
                            |   - WalkLocal /            |
                            |     ListRemoteEntries      |
                            |   - progressReader /       |
                            |     progressWriterAt       |
                            +-------------+--------------+
                                          |
                                          v
                              +-------------------------+
                              |    internal/config      |
                              |  ~/.config/s3duck-tui/  |
                              |       config.json       |
                              |  Params.NewConfiguration|
                              |  Copy / Delete / Write  |
                              +-------------------------+
                                          |
                                          v
                              +-------------------------+
                              |    pkg/utils            |
                              |  SplitFunc, RandStr,    |
                              |  CopyToClipboard        |
                              +-------------------------+
```

### Packages

| Path | Responsibility |
| --- | --- |
| `cmd/s3duck-tui/main.go` | Thin entrypoint: instantiates the controller and runs the tview app loop. |
| `pkg/controller` | Application state and event handling. Owns key bindings, modal flows (create/edit profile, create bucket/folder, download, upload, overwrite prompt, summary, delete confirmation), listing order, selection scoping per `bucket:path`, and goroutine→UI marshalling. `sync.go` holds the directory-sync planner and its dialog/apply flow. |
| `pkg/view` | Pure tview construction. Builds the main flex layout (object list + details panel), modal helper, profile form, local-file browser, hotkeys / about pop-ups. Contains the version string. |
| `pkg/model` | S3 layer. Wraps `s3.Client`, `s3manager.Downloader/Uploader`, custom endpoint resolver, static-credentials provider, and TLS skip-verify. Exposes high-level operations: `List`, `ListBuckets`, `ListObjects`, `DownloadTarget`, `Upload`, `PrepareUpload`, `HeadObject`, `PutObjectMeta`, `ObjectTags` / `PutObjectTags`, `SetStorageClass`, `RestoreObject`, `ListVersions`, `RestoreVersion`, `DeleteVersion`, `DownloadVersion`, `ResolveDownloadObjects`, `Delete`, `DeleteKey`, `DeleteBucket`, `UploadFile`, `WalkLocal`, `ListRemoteEntries`, `CreateBucket`, `CreateFolder`, `MakeBucketPublic`, `GetBucketLocation`, `RefreshClient`. Implements `progressReader` / `progressWriterAt` for live byte-count progress. |
| `pkg/utils` | Small helpers: path-split rune predicate, random string, clipboard write. |
| `internal/config` | JSON profile storage in `~/.config/s3duck-tui/config.json` — load, write, append, copy, delete; auto-creates the file/dir on first run with `0700` permissions. `awsshared.go` parses `~/.aws/credentials` / `~/.aws/config` for the profile import. |

### Data flow

1. **Startup** — `main` builds a `Controller`, which builds a `View` and loads `Params` from `internal/config`. The profile list is rendered first.
2. **Open profile** — selecting a profile constructs a `model.Config` and calls `model.NewModel`, which builds the AWS config (custom endpoint resolver + static credentials, including the optional session token, + 30s HTTP client with optional `InsecureSkipVerify`).
3. **Browse** — selecting a bucket triggers `RefreshClient` (resolves bucket region via `GetBucketLocation`, rebuilds the client). Subsequent navigation uses `List(prefix, bucket)` with `Delimiter="/"` to render folders + files.
4. **Transfer** — long-running operations (download / upload / delete / summary) run in goroutines with a `context.Context` that the cancel button on the progress modal can cancel. Progress callbacks are funneled back to the UI through `App.QueueUpdateDraw`.
5. **Selection scope** — multi-select state is keyed by `bucket:path`, so selections survive navigation in and out of folders.
6. **Sync** — `Ctrl+E` scans both sides (local tree and/or remote prefixes), diffs them with the pure `planSync`, shows the resulting plan, and only then applies it through the same transfer-job machinery as downloads and uploads. `=` runs the same diff read-only across the two panes.

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

`region` is optional for non-AWS endpoints; for AWS it is auto-detected from `GetBucketLocation` on bucket entry. `download_dir` is optional; omitting it shows a directory-picker dialog on each download. A leading `~` is expanded to the user's home directory. `max_bytes_per_sec` (0 = unlimited) caps combined upload/download throughput. `bookmarks` are managed in-app (Ctrl+B); both fields are omitted from the file when unset. `session_token` is only needed for temporary credentials (assume-role / SSO / MFA) and is easiest to obtain via **Ctrl+I → import from `~/.aws`** on the profiles screen; it is omitted when empty. Note that `secret_key` and `session_token` are stored in plaintext (the file is `0600`).

Hotkeys
-------------

`Ctrl+H` opens the in-app list of these. It scrolls (↑/↓, PgUp/PgDn, Home/End,
`j`/`k`) and sizes itself to the terminal, so the tail of the list is reachable
on a short one; `Esc`, `q` or `Ctrl+H` closes it.

**Profiles screen**

| Key | Action |
| --- | --- |
| ↑ / ↓ | Navigate |
| Enter | Open profile |
| Ctrl+N | Create profile |
| Ctrl+I | Import a profile from `~/.aws/credentials` / `~/.aws/config` |
| Ctrl+E | Edit profile |
| Ctrl+Y | Copy / clone profile |
| Ctrl+V | Verify profile (test connection) |
| Del | Delete profile |
| Ctrl+H | Hotkeys help |
| Ctrl+A | About |
| Ctrl+Q | Quit |

**Browser screen**

| Key | Action |
| --- | --- |
| ↑ / ↓ | Navigate |
| Enter | Open folder / bucket |
| Backspace | Go up (`..`) |
| / | Filter the current listing live (Enter keeps it, Esc clears) |
| s / S | Sort: cycle name → size → date / reverse the direction |
| r / F5 | Refresh the current listing |
| Ctrl+F | Recursive search under the current prefix; Enter reveals a hit |
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
| Ctrl+D | Download current item or all selected (4-worker parallel) |
| Ctrl+U | Open local FS browser to upload |
| Ctrl+E | Sync: local ⇄ this prefix, or this prefix → another bucket/prefix (dry-run plan first) |
| = | Compare the two panes (dual-pane, read-only) |
| D | Find duplicates under this prefix (size + ETag); Enter reveals, d deletes a copy |
| e | Edit the highlighted object in `$EDITOR` (small text objects) |
| > | Copy marked objects/folders to a bucket in another profile (streams cross-endpoint) |
| Ctrl+G | Bucket / folder size summary |
| Space | Toggle selection on item |
| Ctrl+S | Select all visible |
| Ctrl+X | Unselect all |
| Ctrl+R | Rename highlighted object, or batch/pattern rename when >1 marked |
| Ctrl+Y | Copy selected / marked objects to a destination bucket + prefix (cross-bucket) |
| Ctrl+T | Move selected / marked objects to a destination bucket + prefix (cross-bucket) |
| Ctrl+L | Object properties (size, ETag, storage class, link) |
| v | Version history — restore / download / permanently delete a version |
| m | Edit object metadata and tags |
| c | Change storage class, or request a Glacier restore |
| Ctrl+W | Copy presigned (time-limited) share link to clipboard |
| Del | Delete marked objects, or the highlighted one (recursive for prefixes) |
| Ctrl+H | Hotkeys help |
| Ctrl+A | About |
| Ctrl+Q | Quit |

Testing
-------------

```
make test          # unit tests
make test-race     # unit tests with the race detector
make check         # everything CI enforces: gofmt, vet, build, race tests
```

The unit suite is pure-function only and needs no network. Behaviour that can
only be observed against a real server — that restoring a version *adds* one
rather than rewinding, that a storage-class change preserves metadata, that a
prefix-like key is refused — lives in a tag-gated integration suite:

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

[CI](.github/workflows/ci.yml) runs the lint/unit job, the MinIO integration
job, and a cross-build matrix covering every platform the Makefile ships
binaries for.

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

Install required tooling:

```
sudo apt-get install git devscripts build-essential lintian upx-ucl
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
