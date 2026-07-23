# S3Duck-TUI — Design Notes

Internal reference covering concurrency model, key design decisions, and known constraints. For user-facing docs see [README.md](README.md).

---

## Architecture

Classic **Model–View–Controller**, where the Controller is the only component aware of both S3 and the terminal UI.

```
cmd/s3duck-tui/main.go
        │
        ▼
pkg/controller          ← app state, keybindings, modal flows, goroutine orchestration
   ├── pkg/view         ← pure tview widget construction, no business logic
   ├── pkg/model        ← S3 SDK wrapper, all network I/O
   ├── internal/config  ← JSON profile persistence
   └── pkg/utils        ← small helpers (path split, rand string, clipboard)
```

The controller is intentionally large (single file) because the modal flows — profile CRUD, download with overwrite prompts, upload, copy/move, summary — are deeply coupled to tview's callback/page model. Splitting would require passing around opaque page/modal handles.

---

## Concurrency model

### Goroutine categories

| Category | What runs there | Notes |
|---|---|---|
| **UI goroutine** | tview event loop, all input handlers, `SetChangedFunc` / `AddItem` callbacks | The only goroutine allowed to mutate tview widgets directly |
| **Network goroutines** | `ListBuckets`, `List`, `GetBucketLocation`, upload, download, delete, copy/move, summary | Spawned by controller; results returned to the UI goroutine via `App.QueueUpdateDraw` |
| **Refresh goroutine** | `updateList()` — fetches the object map, then queues a redraw | Serialised by `refreshMu`; at most one active at a time |

### Mutexes

| Mutex | Protects |
|---|---|
| `Controller.mu` | `objs` (the displayed object map) and `selectedByScope` (multi-select state). Both are read on the UI goroutine (input handlers, list callbacks) and written by background refresh/download/upload goroutines. |
| `Controller.refreshMu` | Serialises `updateList()` so overlapping refreshes can't race on the object map or stack redundant network calls. |
| `downloadSummary` per-download `sumMu` | All per-download mutable state inside the download goroutine: the summary counters, `activeProgress` map, `completedBytes`, throttle timestamp. |

### UI update rule

Any code that modifies a tview widget **must** run on the UI goroutine. Background goroutines use `App.QueueUpdateDraw(func() { ... })`. Direct widget calls from a goroutine (without QueueUpdateDraw) are a data race and can corrupt the display.

---

## Download pipeline

### Two-phase design

Downloads are split into two sequential phases inside a single goroutine:

**Phase 1 — overwrite resolution (sequential)**

Iterates `allObjects` one at a time, handling:
- Directory marker keys (`key/`) — created immediately with `os.MkdirAll`; not queued for network download
- File conflict detection — `os.Stat` → modal prompt (Overwrite / Skip / Overwrite All / Skip All / Cancel)
- Building `toDownload []resolvedItem` — the approved list for Phase 2

Overwrite prompts are inherently sequential because `askOverwrite` blocks the goroutine on user input via a channel; parallel prompts would overlap modals and confuse the user.

**Phase 2 — parallel download (semaphore pool)**

```
sem = make(chan struct{}, 4)   // 4 concurrent workers

for each ri in toDownload:
    select { <-ctx.Done → cancel; sem← → acquire slot }
    if canceled: release orphaned slot if any; break
    wg.Add(1)
    go worker(ri):
        defer wg.Done(); defer <-sem
        track in activeProgress
        DownloadTarget(ctx, ri, ...)
        update sum, completedBytes, completedCount
wg.Wait()
showSummary()
```

Cancellation flow:
1. User clicks Cancel → tview `SetDoneFunc` fires → `cancel()` called → ctx cancelled
2. Next semaphore `select` picks `<-ctx.Done()`, sets `canceled = true`, breaks loop
3. Running workers see `ctx.Err() != nil` after `DownloadTarget` returns; `DownloadTarget` itself cancels the S3 download and removes the partial file
4. `wg.Wait()` drains workers; `showSummary()` shows the final report

### Progress display (parallel)

`showProgress` is called from multiple worker goroutines concurrently. It:
1. Acquires `sumMu` to snapshot `completedBytes`, `completedCount`, and the `activeProgress` map
2. Releases `sumMu` before calling `QueueUpdateDraw` (holding a lock across a channel send would deadlock if the UI goroutine tried to acquire it)
3. Throttled at 100 ms to avoid flooding the event queue

Displayed format:
```
Downloading [4 workers]
12 / 47 done
1.2 GiB / 3.4 GiB (35.2%)

file1.txt  report.zip
```

---

## Listing: rendering vs fetching

`updateList()` and `renderList()` are split so the list can re-render without hitting the network:

- **`updateList()`** — takes `refreshMu`, calls `makeObjectMap()` (network `List` / `ListBuckets`), then `renderList()`. Use it whenever the object set changes (navigation, delete, upload, rename, copy/move).
- **`renderList()`** — pure re-render from the in-memory `objs` map: applies the active filter + folders-first/name sort (`filterSortObjects`), then rebuilds the list widget inside a single `QueueUpdateDraw`. No network. Cheap enough to call on every keystroke.

Selection toggles (`ToggleSelectCurrent`, `SelectAllVisible`, `ClearSelection`) and the live filter call `renderList()` directly, avoiding a redundant `List` round-trip on every Space / `/` press. All cursor/list reads now happen **inside** the `QueueUpdateDraw` closure on the UI goroutine (this also removed the earlier off-goroutine read of list state).

## In-listing filter

A persistent one-line `InputField` sits under the list (`view.Filter`). `/` focuses it; typing sets `c.filter` (guarded by `mu`) through `SetChangedFunc` and re-renders live; Enter keeps the filter and returns focus to the list; Esc clears it. Matching is a case-insensitive substring test on the object's short display name. The filter is reset on every navigation (`Down` / `Up` / `Profiles`) so it never leaks across folders. `filterSuppress` stops the change handler from re-rendering when the field is cleared programmatically (`SetText` fires `SetChangedFunc` inline on the UI goroutine).

## Recursive search

Ctrl+F prompts for a query, then lists the current prefix recursively (`ListObjects`, no delimiter) on a background goroutine behind a "Searching…" modal. `computeHits` filters keys by case-insensitive substring (skipping folder-marker keys) and caps results at `searchMaxResults` (1000), flagging truncation in the results title. Enter on a result calls `revealKey`: it clears any active filter, sets `currentPath` to the hit's `parentPrefix`, sets `restoreNext` to the full key, and calls `updateList()` — so the browser lands in the containing folder with the object highlighted.

## Dual-pane (state-swap)

The two-pane (Midnight Commander) layout is implemented by **state-swap** rather than by making every method pane-aware. The controller's live per-location fields (`currentBucket`, `currentPath`, `objs`, `buckets`, `bucketPos`, `restoreNext`, `filter`, `selectedByScope`, `hist`) *are* the active pane; `panes[inactive]` holds the other pane's snapshot as a `paneState`. `Tab` (`swapPane`) snapshots the active fields into `panes[active]`, loads `panes[other]` into the live fields (under `mu`), and repoints `view.List` / `view.Filter` at the other pane's fixed widgets. Every existing method keeps operating on `c.view.List` / `c.currentBucket`, so none of them needed to change.

- Both panes' list and filter widgets are wired with the *same* input-capture / changed-func (`listInputCapture`, `wireListChanged`, `wireFilter`); only the focused (active) pane receives events, and the handlers act on the active pane via the `c.view.*` fields.
- `Ctrl+O` (`ToggleDualPane`) rebuilds the root flex (`ShowSinglePane` / `ShowDualPane`), seeds pane 1 with the current location on first open, and focuses it. `swapAndFocus` re-fetches the newly active pane so it reflects changes made from the other pane. Copy/move (`copyOrMove`) defaults its destination to the other pane's bucket+prefix.
- `Profiles` and `Duck` call `resetPanes` to collapse to a clean single pane, so panes never leak across profiles.
- **Caveat:** both panes share one `model`/client, so region is per-client. Two panes in different-region AWS buckets would fight over the client region — the same cross-region limitation already documented below. Same-endpoint (MinIO/Ceph, same-region AWS) is unaffected.

## Bandwidth throttle

`model.Config.MaxBytesPerSec` (from the profile) builds a `*rateLimiter` on the `Model`, shared by `progressReader`/`progressWriterAt`, so uploads and the 4-worker download pool honor one global cap. The pure `throttleStep` is a token bucket (tokens may go negative — the debt is paid by the next refill, which is what prevents over-crediting sleep time); `newRateLimiter(0)` returns nil (unlimited), making the hot path free when throttling is off.

## Bookmarks, history, palette, batch rename

- **Bookmarks** live in `Config.Bookmarks` per profile; `jumpTo(bucket, prefix)` navigates like `Down`. `addBookmark` dedups by bucket+prefix.
- **History** is a per-pane `histStack` (back/forward). `recordHistory` runs at the start of each user navigation (`Down`/`Up`/`jumpTo`/`revealKey`); `HistoryBack`/`HistoryForward` drive `navigateTo`, which does *not* record (so it doesn't corrupt the stacks).
- **Command palette** filters a static `paletteActions` registry with a case-insensitive subsequence match (`filterActions`); the chosen action runs on the active pane.
- **Batch rename** resolves marked items through the pure `planBatchRename` (which applies `applyRenamePattern` and rejects `/`, duplicates, and targets that would clobber another selected item's source), then runs `MoveKeys` per item behind a progress modal.

## Background transfer queue

Download/upload progress used to live in a blocking modal. Now each transfer is a `transferJob` (id/kind/desc/status/total/done/counts, its own `cancel`, guarded by a per-job mutex). The progress modal gains a **Background** button: pressing it sets `job.bg` and removes the modal, so the transfer keeps running headless while you browse; `showProgress`/the upload callback update the job always and the modal only when `!job.isBackgrounded()`. The transfers panel (`t`) is a `tview.List` re-rendered by a 300 ms ticker from `jobSnapshot()`; `d`/Del cancels the selected job's context, `c` clears finished. `transferRow` is pure (takes `elapsed`) for testing.

Concurrency: **download overwrite resolution stays foreground** (the interactive `askOverwrite` loop in Download's Phase 1), then a `jobSem` (buffered chan, cap 2) gates the byte-transfer phase so at most two transfers push bytes at once; aggregate bandwidth is still capped by `model.Limiter`. Trade-off: two downloads started while one is mid-Phase-1 could show overlapping overwrite modals (minor, not data loss).

## Clipboard, undo, activity log

- **Clipboard** (`clip`): `y`/`x` fill it (copy/cut) from the marked/highlighted set via pure `clipItems`; `p` pastes into the current location. `runCopyOrMove` was generalized to take an explicit `srcBucket` so paste works from the clipboard's origin bucket (cross-bucket).
- **Undo** (`lastUndo`, guarded by `undoMu`): the move path (`runCopyOrMove` when `isMove`, incl. paste-cut) and rename paths record the successful moves; pure `invertOps` swaps src/dst so re-applying reverses them. One-step; `u` confirms then runs the reverse `MoveKeys`. Copy/delete are not undoable.
- **Activity log**: a capped ring buffer (`appendCapped`, pure) of timestamped entries written by `logActivity` from transfer/copy/move/rename/abort/undo paths; shown newest-first via the palette.

## Bucket config & multipart

`model.BucketConfig` gathers versioning/encryption/object-lock/region, treating endpoints that don't support a feature (MinIO/Ceph return an error) as its default rather than failing. `ListMultipartUploads`/`AbortMultipartUpload` surface and clean orphaned upload parts (single-page list, ample for cleanup). Both are palette actions on the current bucket.

## Selection scoping

Multi-select state is keyed by `bucket:path` so selections survive navigation in and out of subfolders:

```go
selectedByScope["my-bucket:photos/2024/"] = {"vacation.jpg": true, "trip.mp4": true}
```

`mu` guards `selectedByScope` because selections are read by input handlers (UI goroutine) and cleared by post-download/move cleanup (network goroutines).

---

## Object identity

`objKey(o)` produces the unique key used in the `objs` map, selection sets, and list secondary text:
- **Buckets**: `*o.Key` (the bucket name)
- **Files / Folders**: `*o.FullPath` (the full S3 prefix, e.g. `photos/2024/vacation.jpg`)

Using `FullPath` avoids collisions between a file `"x"` and a folder `"x/"` at the same prefix level.

---

## Configuration

Profiles are stored in `~/.config/s3duck-tui/config.json` (mode `0600`), created on first run. Each profile is a `Config` struct serialised as JSON:

```json
{
  "name":         "minio-local",
  "base_url":     "https://s3.example.com",
  "region":       "us-east-1",
  "access_key":   "AKIA...",
  "secret_key":   "plaintext — file is 0600 but not encrypted",
  "ignore_ssl":   false,
  "download_dir": "~/Downloads/s3"
}
```

`secret_key` is stored in plaintext. The file has `0600` permissions (owner-readable only), but there is no OS-keychain integration. Future work: `github.com/zalando/go-keyring`.

---

## S3 compatibility notes

### Region handling

For non-AWS endpoints (MinIO, Ceph, etc.), the region field in the endpoint resolver is set from the profile config and `HostnameImmutable: true` is used so the SDK never rewrites the URL.

For AWS endpoints, `GetBucketLocation` is called on bucket entry to discover the bucket's actual region, then the client is rebuilt with that region. This avoids redirect loops for cross-region bucket access. If `GetBucketLocation` fails (permission denied or network error), the client defaults to `us-east-1` and the error is surfaced only through broken subsequent calls (not a hard crash since v0.1.3).

### Folder markers

S3 has no real directories. The app follows the S3 convention:
- A folder is a common prefix returned by `ListObjectsV2` with `Delimiter=/`
- An empty folder is represented by a zero-byte object whose key ends with `/`
- Download preserves the tree via `os.MkdirAll` for marker keys
- Upload creates marker objects for empty local directories

### Delete

`model.Delete` handles both files and folder prefixes. For folders it calls `ListObjects` (no delimiter, full recursive listing) then batches `DeleteObjects` in pages of 1000, which is the S3 API maximum.

---

## Known limitations

| Area | Description |
|---|---|
| **Navigation race** | `c.currentBucket` and `c.restoreNext` are written from the goroutine spawned by `Down()` without the `mu` mutex. In normal use, navigation is serial (one bucket load at a time), so races are unlikely. A proper fix requires either passing bucket through parameters or extending `mu` to cover all navigation state. |
| **No retries on upload** | `s3manager.Uploader` is configured with `aws.NopRetryer{}`. A transient network error fails the entire upload. |
| **Sequential per-file overwrite** | Overwrite decisions block Phase 1 completion. For a large selection with many conflicts, the user must click through each dialog before any download starts. |
| **Cross-bucket copy/move is same-endpoint only** | `CopyKeys` / `MoveKeys` take separate source/destination buckets and issue a server-side `CopyObject`, so both buckets must be reachable through the one configured endpoint. This covers any single S3-compatible endpoint (MinIO/Ceph) and same-region AWS. Copying between AWS buckets in *different regions* is not handled (the client stays pinned to the source region). |
| **No download resume** | Interrupted downloads restart from byte 0; partial files are deleted on cancel. |
| **Summary top-10 cap** | `buildSummary` silently truncates the groups table to the top 10 by size. Groups ranked 11+ are not shown and not counted in any "overflow" indicator. |
