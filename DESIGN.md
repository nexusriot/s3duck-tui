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

`pkg/controller` is intentionally large because the modal flows — profile CRUD, download with overwrite prompts, upload, copy/move, delete, summary — are deeply coupled to tview's callback/page model; splitting them would mean passing around opaque page/modal handles. It is split by feature where a feature is self-contained: `controller.go` (everything above) plus `sync.go` (directory sync), `objectmeta.go` (metadata / tags / storage class / restore), `versions.go` (version history), `keymap.go` (bindings as data), `listing.go` (streamed listings), `opresult.go` (the failure ledger), `localpane.go`, `usage.go`, `preview.go`, `match.go`, `guard.go`, `trash.go`, `verify.go`, `notify.go`, `audit.go` and `options.go`. Each splits cleanly because its logic is pure or reaches shared state only through existing helpers. `pkg/model` mirrors the shape: `model.go` plus `sync.go`, `object.go`, `versions.go`, `copy.go`, `content.go`, `conflicts.go`, `write.go`, `checksum.go` and `local.go`.

The tree is `gofmt`-, `go vet`- and `staticcheck`-clean; keep it that way. Controller actions that report exclusively through modals (`Delete`, `Upload`) return nothing rather than an always-nil `error`, so a caller can't be misled into writing a dead error branch.

## Testing strategy

Two suites, split by what they can actually establish.

**Unit tests** cover the pure functions only — planners, formatters, parsers, comparators — and need no network. This is a deliberate consequence of the architecture: the interesting decisions are pushed into pure helpers (`planSync`, `deleteConfirmText`, `filterSortObjects`, `ParseRestoreStatus`, `ParseAWSProfiles`) precisely so they can be tested without a server or a terminal. The UI layer has no seam and is not unit-tested.

**Integration tests** (`pkg/model/integration_test.go` and `integration_new_test.go`, build tag `integration`) run against a live S3-compatible endpoint. They exist because the most important properties of this package are unobservable without one:

- a version restore *adds* a version rather than rewinding the history
- a `DeleteVersion` leaves no delete marker, while an ordinary delete does
- `ListObjectVersions` is prefix-based, so a sibling `x.bak` must be filtered out of `x`'s history
- a storage-class change preserves metadata (`MetadataDirective` stays at COPY)
- `DeleteKey` refuses a prefix-like key **and removes nothing**
- the session token actually reaches the wire — a bogus one is rejected while the same credentials without one succeed, which is the only way to verify that fix short of real STS
- an uploaded object's stored `Content-Type` is the derived one (and an unrecognisable body keeps the server's default) — the client can only prove what it *sent*
- a requested checksum is really stored, a locally computed one matches it, and a tampered local file is detected — plus the ETag fallback for a single-part object
- a ranged read returns exactly the requested window, and asking for more than the object holds returns the object rather than an error
- `GetVersionContent` reaches a specific old version, which is what the diff needs
- `ListObjectsStream` pages incrementally, stops when the callback says so, and reports a cancelled context instead of running to the end

They skip entirely without `S3DUCK_TEST_ENDPOINT`, so a tagged run on a machine with no server is a no-op rather than a failure. `make test-integration` brings up a throwaway MinIO in Docker and tears it down again. Individual tests skip themselves when the endpoint lacks the feature (SSE-S3 needs a KMS MinIO does not configure by default, versioning may be unsupported) rather than failing against a legitimate backend.

**What neither suite covers** is the terminal: there is no seam between the controller and tview, so the wiring — a keypress reaching the right action, a modal appearing, a title rendering — is only observable by driving the real binary. That is worth doing by hand (or from a pty harness) after touching input or overlays: it is how a pane title reading `[local]` was caught being swallowed as a tview colour tag, which no unit test could see because the string was correct.

Because the tag hides them from `go build ./...`, CI runs `go vet -tags integration ./...` in the fast job as well, so they cannot silently stop compiling between integration runs.

CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)) is three jobs: lint+unit, MinIO integration, and a cross-build matrix over every platform the Makefile ships. `make check` runs the lint/unit half locally.

---

## Concurrency model

### Goroutine categories

| Category | What runs there | Notes |
|---|---|---|
| **UI goroutine** | tview event loop, all input handlers, `SetChangedFunc` / `AddItem` callbacks | The only goroutine allowed to mutate tview widgets directly |
| **Network goroutines** | `ListBuckets`, `List`, `GetBucketLocation`, upload, download, delete, copy/move, summary | Spawned by controller; results returned to the UI goroutine via `App.QueueUpdateDraw` |
| **Refresh goroutine** | `updateList()` — fetches the object map page by page, then queues a redraw | Serialised by `refreshMu`; at most one active at a time, and cancellable through `listingState` |
| **Status ticker** | `runStatusTicker()` — recomputes the header's transfer badge / notice | One for the app's lifetime; queues a redraw *only* when the text changed, so an idle app costs a string compare |

### Mutexes

| Mutex | Protects |
|---|---|
| `Controller.mu` | `objs` (the displayed object map), `selectedByScope` (multi-select state), `filter`, `partial` (the listing-was-incomplete flag) and `sortBy`/`sortDesc`. All are read on the UI goroutine (input handlers, list callbacks) and written by background refresh/download/upload goroutines. |
| `Controller.jobsMu` | The transfer-job list *and* the header notice (`notice`/`noticeUntil`), which the status ticker reads and `announceJob` writes from transfer goroutines. |
| `listingState.mu` | The in-flight listing's cancel function, so Esc can cancel a listing started by another goroutine. |
| `Controller.refreshMu` | Serialises `updateList()` so overlapping refreshes can't race on the object map or stack redundant network calls. |
| `downloadSummary` per-download `sumMu` | All per-download mutable state inside the download goroutine: the summary counters, `activeProgress` map, `completedBytes`, throttle timestamp. |

### UI update rule

Any code that modifies a tview widget **must** run on the UI goroutine. Background goroutines use `App.QueueUpdateDraw(func() { ... })`. Direct widget calls from a goroutine (without QueueUpdateDraw) are a data race and can corrupt the display.

The mirror-image rule bites more often: **`c.error` and `c.success` are themselves built on `QueueUpdateDraw`, so they must NOT be called inline from the UI goroutine** — from a button handler, a form callback, or a list's selected-func. Doing so blocks the event loop waiting on itself and freezes the app (this is exactly the `Upload()`-on-an-empty-directory bug, and — found in the 2026-08-11 review — the filter box's `SetChangedFunc` calling `renderList` inline, which froze the app on the first keystroke ever typed into the filter). Anything a widget event handler invokes synchronously must not block on `QueueUpdateDraw`. From UI-goroutine code call them as `go c.error(...)` / `go c.success(...)`; from a background goroutine call them directly. `c.chooseDir` is the same trap from the other side: it invokes its `onChosen` callback *on* the UI goroutine, so callers must update widgets directly there rather than wrapping them in `QueueUpdateDraw`.

**Page names.** The overlays each own a name: `"modal"` (long-lived forms), `"modal-msg"` (transient toasts), `"modal-dir"` (the directory picker, which must *overlay* the sync form), `"progress"` (wait/progress modals), `"searching"`, `"modal-help"`, `"modal-about"`, `"modal-palette"`, `"modal-transfers"`, `"modal-activity"`, `"modal-versions"`, `"modal-dups"`, `"modal-usage"`, `"modal-preview"` and `"modal-diff"`. `Pages.AddPage` with an existing name *replaces* the old page, so the transient toasts live on their own page (`"modal-msg"`) — when they shared `"modal"` with the long-lived forms, any asynchronous error or success (a failed background transfer, a late refresh, a rename completing) silently destroyed whatever form the user was typing into. The directory picker has its own page too (`"modal-dir"`), which lets it *overlay* the sync form instead of replacing it. Two more captured invariants: transfer flows capture `c.model` at entry alongside bucket/path (a profile switch swaps `c.model`, and a queued transfer reading it lazily would run against the wrong account — `CheckProfile` now verifies with a throwaway client for the same reason), and `Duck` clears the clipboard and the one-step undo (both carry bucket/key names that would otherwise be replayed against a same-named bucket on the new endpoint).

---

## Download pipeline

### Two-phase design

Downloads are split into two sequential phases inside a single goroutine:

Before either phase, folder **resolution** (the recursive `ListObjects` behind a folder selection) runs in its own goroutine behind a "Resolving…" modal — inline on the UI goroutine it froze the whole TUI for the length of the listing and swallowed its errors. Local paths are derived through `model.SafeLocalPath`, which rejects any key whose cleaned path escapes the destination directory: S3 keys may legally contain `..` segments, and an unchecked `filepath.Join` is the zip-slip pattern (a hostile shared bucket could plant `docs/../../../.bashrc`).

**Phase 1 — overwrite resolution (sequential)**

Iterates `allObjects` one at a time, handling:
- Directory marker keys (`key/`) — created immediately with `os.MkdirAll`; not queued for network download
- File conflict detection — `os.Stat` → modal prompt (Overwrite / Skip / Overwrite All / Skip All / Cancel)
- Building `toDownload []resolvedItem` — the approved list for Phase 2

Overwrite prompts are inherently sequential because `askOverwrite` blocks the goroutine on user input via a channel; parallel prompts would overlap modals and confuse the user.

An Overwrite decision only sets a flag on the item — **nothing is deleted at prompt time**. The transfer downloads into a sibling `*.s3duck-part` temp file and renames it onto the target only after the body is fully on disk, so a cancel while the job is still queued (or a failed transfer) leaves every existing file intact; `sum.overwritten` is likewise counted on completion, when the file was actually replaced. (Phase 1 used to `os.Remove` at decision time: an "Overwrite All" over 200 files followed by a cancel deleted all 200 and downloaded none.)

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
        if profile verifies: compare the file against the object's checksum
        update sum, completedBytes, completedCount
wg.Wait()
showSummary()
```

Cancellation flow:
1. User clicks Cancel → tview `SetDoneFunc` fires → `cancel()` called → ctx cancelled
2. Next semaphore `select` picks `<-ctx.Done()`, sets `canceled = true`, breaks loop
3. Running workers see `ctx.Err() != nil` after `DownloadTarget` returns; `DownloadTarget` itself cancels the S3 download and removes its temp file (the target file — old content included — is never touched by a canceled transfer)

A worker's last step, when the profile asks for it, is the **post-transfer
verify**: the file on disk is re-read and compared against the object's
checksum (or its single-part ETag). A mismatch is counted as a *failure*, not
a success — a silently corrupt local file is the worst of the three possible
outcomes — while an object that cannot be compared at all (multipart, no
additional checksum) is counted separately and disclosed as "not verifiable"
rather than being quietly called verified. The setting is read once, on the UI
goroutine, and carried into the transfer: a profile switch mid-download must
not change the rules a running transfer is playing by.
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

- **`updateList()`** — takes `refreshMu`, calls `makeObjectMap()` (network `ListStream` / `ListBuckets`, page by page with a live count and a cancellable context — see *Streamed, cancellable listings*), then `renderList()`. Use it whenever the object set changes (navigation, delete, upload, rename, copy/move). For a local pane it reads one directory instead (`makeLocalObjectMap`), which needs no pagination and no cancellation.
- **`renderList()`** — pure re-render from the in-memory `objs` map: applies the active filter and the active sort (`filterSortObjects`, see *Listing order* below), then rebuilds the list widget inside a single `QueueUpdateDraw`. No network. Cheap enough to call on every keystroke.

Selection toggles (`ToggleSelectCurrent`, `SelectAllVisible`, `ClearSelection`), the live filter and the sort keys call `renderList()` directly, avoiding a redundant `List` round-trip on every Space / `/` / `s` press. `Refresh` (`r`/F5) is the one binding that deliberately goes back to the network via `updateList()`. All cursor/list reads now happen **inside** the `QueueUpdateDraw` closure on the UI goroutine (this also removed the earlier off-goroutine read of list state).

## In-listing filter

A persistent one-line `InputField` sits under the list (`view.Filter`). `/` focuses it; typing sets `c.filter` (guarded by `mu`) through `SetChangedFunc` and re-renders live; Enter keeps the filter and returns focus to the list; Esc clears it. Matching goes through the shared matcher (see *Query syntax* below): plain text is a case-insensitive substring of the object's short display name, `*.log` is a glob, `re:` is a regex, and the mode in force is named in the pane title. The filter is reset on every navigation (`Down` / `Up` / `Profiles`) so it never leaks across folders. `filterSuppress` stops the change handler from re-rendering when the field is cleared programmatically (`SetText` fires `SetChangedFunc` inline on the UI goroutine).

## Recursive search

Ctrl+F prompts for a query, then walks the current prefix recursively (`ListObjectsStream`, no delimiter) on a background goroutine behind a cancellable "Searching…" modal that reports "scanned N keys, M match(es)". Matching is the shared matcher (text / glob / `re:`), applied **page by page** rather than to one accumulated slice: an all-buckets scan can page through millions of keys, and holding them all only to discard nearly all of them cost memory proportional to the account rather than to the result set. A malformed regex is refused up front here (unlike the live filter, a search is a deliberate one-shot request, and an empty result set would read as "no matches"). Results are capped at `searchMaxResults` (1000), and a cancelled scan reports what it found so far, flagged as truncated. `computeHits` remains the pure, testable form of the same filter. Enter on a result calls `revealKey`: it clears any active filter, sets `currentPath` to the hit's `parentPrefix`, sets `restoreNext` to the full key, and calls `updateList()` — so the browser lands in the containing folder with the object highlighted.

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
- **Command palette** filters the `paletteActions` registry with a case-insensitive subsequence match (`filterActions`); the chosen action runs on the active pane. The registry is rebuilt per invocation rather than being a package-level table, so an entry whose wording depends on state can say which way it will go (the local-pane toggle labels itself *open* or *close*).
- **Batch rename** resolves marked items through the pure `planBatchRename` (which applies `applyRenamePattern` and rejects `/`, duplicates, and targets that would clobber another selected item's source), then runs `MoveKeys` per item behind a progress modal.

## Background transfer queue

Download/upload progress used to live in a blocking modal. Now each transfer is a `transferJob` (id/kind/desc/status/total/done/counts, its own `cancel`, guarded by a per-job mutex). The progress modal gains a **Background** button: pressing it sets `job.bg` and removes the modal, so the transfer keeps running headless while you browse; `showProgress`/the upload callback update the job always and the modal only when `!job.isBackgrounded()`. The transfers panel (`t`) is a `tview.List` re-rendered by a 300 ms ticker from `jobSnapshot()`; `d`/Del cancels the selected job's context, `c` clears finished. `transferRow` is pure (takes `elapsed`) for testing.

A finished job is announced rather than left to be discovered — see
*Background transfer notices*.

Concurrency: **download overwrite resolution stays foreground** (the interactive `askOverwrite` loop in Download's Phase 1), then a `jobSem` (buffered chan, cap 2) gates the byte-transfer phase so at most two transfers push bytes at once; aggregate bandwidth is still capped by `model.Limiter`. Trade-off: two downloads started while one is mid-Phase-1 could show overlapping overwrite modals (minor, not data loss).

## Clipboard, undo, activity log

- **Clipboard** (`clip`): `y`/`x` fill it (copy/cut) from the marked/highlighted set via pure `clipItems`; `p` pastes into the current location. `runCopyOrMove` was generalized to take an explicit `srcBucket` so paste works from the clipboard's origin bucket (cross-bucket).
- **Undo** (`lastUndo`, guarded by `undoMu`): the move path (`runCopyOrMove` when `isMove`, incl. paste-cut) and rename paths record the successful moves; pure `invertOps` swaps src/dst so re-applying reverses them. One-step; `u` confirms then runs the reverse `MoveKeys`. Copy/delete are not undoable.
- **Activity log**: a capped ring buffer (`appendCapped`, pure) of timestamped entries written by `logActivity` from transfer/copy/move/rename/abort/undo paths; shown newest-first via the palette, and simultaneously appended to a file (see *Persistent activity log and session restore*).

## Listing columns

The browser shows size, modified date and storage class next to the name. It stays a `tview.List` — whose rows are a single string — so the columns are produced by padding rather than by `tview.Table`: switching widgets would mean rewriting every cursor, selection and reveal call site that currently speaks List, for a purely cosmetic gain.

Two measurement rules make it line up. Widths are **display cells, not bytes**: the icons are double-width emoji (📄 is four bytes and two cells) and the names carry colour tags, so padding goes through `tview.TaggedStringWidth` and truncation through `runewidth`. And the name is truncated *before* it is coloured, since a tag would otherwise be counted as visible text.

`planColumns(width)` decides what fits, taking columns in priority order size → date → class and **stopping at the first that doesn't fit** rather than skipping it. That `break` (not `continue`) is what makes the layout monotonic: with a skip, dropping the date would free enough room for the lower-priority class to reappear, so narrowing the pane could *add* a column. Each column is only taken if a readable name (`colMinName`) still remains, so a narrow dual-pane degrades to names alone.

The header line is a `TextView` above each pane's list. Its indent is derived at render time from the difference between the list's and the header's inner rects, rather than hardcoding "one cell for the border" — so it stays aligned whatever decoration either widget grows later. `watchResize` (an `App.SetAfterDrawFunc` hook) re-renders the active pane when its width changes, which covers terminal resizes and dual-pane toggles; it converges after one extra render because that render records the new width.

## Listing order

`filterSortObjects` takes the sort key (`name` / `size` / `date`) and direction alongside the filter. Folders and buckets are always emitted before files whatever the key is, so the listing keeps its navigable shape; the key only orders within each group. Ties (equal sizes, equal timestamps, and every folder pair — folders carry no `Size`/`LastModified`) fall back to **ascending name** in both directions, via the `tie` return of `lessBySortKey`, so reversing the direction can't scramble an otherwise-equal group. The state (`sortBy`/`sortDesc`, guarded by `mu`) is shared by both panes: unlike a filter, an ordering preference is not location-specific. `s` cycles the key, `S` reverses; both call `renderList` only — no network round-trip. `r`/F5 is the explicit `updateList` refresh.

## Delete

`Delete` resolves its targets the same way copy/move/rename do — the marked set, falling back to the highlighted item — so the destructive operation is no longer the odd one out that ignored multi-select. Buckets are still single-target (they can't be marked; selection scope only exists inside a bucket).

Because a folder target is a prefix delete, the confirmation must state its cost: a scan goroutine runs `ListObjects` per folder/bucket target, filling in `objects`/`bytes`, behind a "Calculating…" modal. `deleteConfirmText` (pure) renders the totals and names up to 6 targets. A target whose scan failed is counted separately and disclosed — the totals are never silently short. `runDelete` then deletes sequentially behind a **cancellable** progress modal (a folder delete lists recursively before it removes anything, so it is long enough to want out of; cancelling stops before the next target and reports what already went), collecting failures into an `opResult` instead of aborting — so the report can offer *Retry failed* — and drops each deleted name from the selection set as it goes. A bucket target is emptied first (`EmptyBucket`) — S3 only deletes empty buckets, and the confirm has already promised the objects go; on a *versioned* bucket old versions survive and the bucket delete still fails (see the limitations table). When the profile enables safe delete, each non-bucket target is diverted through `trashTarget` instead of being removed (see *Trash*).

## Overwrite confirmation for remote writes

Downloads have always prompted before replacing a local file; the remote side
silently destroyed whatever sat at the destination. Rename onto an existing
name, copy into a folder holding same-named objects, paste, upload — all of it
clobbered without a word, and undo only moved the *new* object back, so the
overwritten content was simply gone. Every remote write now goes through
`confirmOverwrites` first.

- **What would be written.** `PlannedCopyKeys` expands each item into the exact
  destination keys, running the same `planFolderCopy`/`remapKey` mapping
  `CopyKeys` uses — so the list in the dialog is not an approximation of what
  the transfer would do, it is the same computation. Upload skips the listing
  entirely: `PrepareUpload` already knows every key.
- **What already exists.** `model.Conflicts` answers per key, and picks the
  cheaper question. Folder candidates get a `MaxKeys=1` listing each — "is
  anything under this prefix" is the whole question, and enumerating the prefix
  would be the same answer at arbitrary cost. Plain keys up to
  `conflictHeadLimit` (32) are probed with concurrent `HeadObject`s, so a
  one-key rename never triggers a recursive listing of a folder that could hold
  a million objects. Only a larger batch falls back to listing the candidates'
  `CommonPrefix` once and intersecting. That order matters: the common prefix
  of keys sitting at the bucket *root* is empty, so a naive
  listing-for-everything would have enumerated an entire bucket to check a
  handful of names. `CommonPrefix` cuts at a "/" so it is always a real prefix —
  the property that every key stays under it is what makes the single listing
  sound.
- **The decision.** Overwrite / Skip existing / Cancel, where "Skip existing"
  appears only when it would leave something to do (skipping every candidate
  is Cancel with extra steps). The chosen skip set is a set of *destination
  keys*, threaded into `CopyKeys`/`MoveKeys`/`Upload`, which is why a partial
  skip inside a folder copy works per object rather than dropping the folder.
- **A skipped move keeps its source.** `copyKeysTracked` reports only what it
  actually copied and `MoveKeys` deletes exactly that, so skipping a
  destination can never delete the source that was not written anywhere.
- **A failed check blocks the write.** A destination that cannot be inspected
  is not one that may be silently overwritten. This is also why `isNotFound`
  is generous: a HEAD response carries no body for the SDK to parse an error
  shape from, so MinIO answers a missing key with an *untyped* API error —
  reading that as a real failure would have made every rename and copy on that
  backend refuse to run (the integration suite caught exactly this).

Reporting changed with it: the copy/move summary counts objects written, not
items processed, and names how many existing objects were kept. "OK: 2" for a
run that wrote one object and skipped another was the kind of lie this whole
feature exists to remove. The same care applies inside `Upload`, which filters
the walked file list *before* the totals are taken — the walk counts every
file, so leaving the skipped ones in made a skipped upload finish at 60% with
an index counting files it never sent. And a rename or batch-rename operation
whose objects were all kept records no undo entry: its destination does not
exist, so undoing it could only fail.

The check is a snapshot, not a lock. An object that appears at the destination
between the scan and the write is still overwritten silently — S3 offers no
transaction to close that window, and the download flow has always had the same
gap.

Restoring from the trash goes through it too, and has to: a restore writes
back to the key the object was deleted from, which is exactly the key most
likely to have been written again since. Declining leaves that item in the
trash — `MoveKeys` deletes only what it copied — and the report counts it as
skipped rather than restored.

Two paths deliberately stay unprompted: **sync**, whose mandatory dry-run plan
already lists every update before anything moves, and **undo**, which has its
own confirmation and restores objects to where they were moments ago.
Cross-profile copy does prompt, but asks from inside its transfer goroutine
(`askOverwriteBlocking`) because only there are its folders expanded — the
same channel-blocking shape the download flow's `askOverwrite` uses. That
callback is called on *every* exit including Esc: a path that returned without
deciding would hang the goroutine forever.

## Large copies (over 5 GiB)

A single-request `CopyObject` is capped at 5 GiB by S3, and every server-side
copy in the app went through one: copy, move, rename, storage-class change,
metadata save, version restore. Past that size they simply failed, and a folder
move aborted midway leaving a half-populated destination. `runCopy` now picks
the path by size, and `copySpec` describes a copy in enough detail to run it
either way.

The two paths are not interchangeable, which is the whole difficulty:
`CopyObject` inherits the source's metadata and tags for free (COPY
directives), while `CreateMultipartUpload` starts a *bare* object. So the
multipart path HEADs the source and re-applies content type, cache control,
disposition/encoding, user metadata and tags explicitly. Storage class is
handled the other way round — set only when explicitly asked — so both paths
agree: a plain copy lands in the bucket default either way, and a class change
still lands where it was told.

- **Sizes come from listings, not extra round trips.** `CopyObjectSized` takes
  the size the caller already has (`ListObjects` for a folder copy, the sync
  plan for a remote→remote run). Only the single-object `CopyObject` pays one
  `HeadObject`, and only because there is genuinely nothing to go on.
- **Part planning is pure.** `planCopyParts` splits the source into contiguous
  ranges; only the last may fall under S3's 5 MiB minimum, which is exactly
  what S3 permits. `copyPartSize` grows the part beyond the configured 512 MiB
  when a source would otherwise need more than 10,000 parts, so a 5 TiB object
  plans instead of failing.
- **Parts copy concurrently** (4 workers): the bytes move inside S3, so this
  costs no local bandwidth and turns a per-512-MiB round-trip series into
  something bounded. The first error cancels the rest — none of them are worth
  finishing — and the upload is aborted so orphaned parts do not accrue
  charges. That abort runs on a fresh context: on a cancelled copy the original
  is already dead and the cleanup would be dropped.
- **Tags are re-applied on every multipart copy**, including a metadata save:
  `CopyObject` carries them along whatever else it does (TaggingDirective
  defaults to COPY even under MetadataDirective=REPLACE), so anything less
  would make a large metadata save silently drop an object's tags. They are
  read from the source *version* being copied, which matters for a version
  restore. Fetching them is best-effort — tagging is optional on S3-compatible
  backends, and failing a 100 GiB copy because `GetObjectTagging` is
  unimplemented is the worse trade, the same degradation the metadata editor
  makes.
- **A failed upload is always aborted**, whether a part failed or the
  completion did. Either way the upload stays open and its parts keep accruing
  storage charges until a lifecycle rule reaps them.

`MultipartCopyThreshold` and `MultipartCopyPartSize` are variables rather than
constants so the integration suite can exercise the path for real on a 6 MiB
object. The proof it ran is the destination's ETag: a multipart object's ETag
carries a `-<partcount>` suffix that a single `CopyObject` could never produce.

## Credentials

`model.Config.SessionToken` feeds `credentials.NewStaticCredentialsProvider`'s third argument, which was previously hard-coded to `""` — that omission made every form of temporary credential (assume-role, SSO, MFA) unusable regardless of what the user pasted into the profile form. Static keys are only one of the two credential paths now; the other, and the better answer for anything temporary, is *Credentials, delegated* below.

`internal/config/awsshared.go` reads `~/.aws/credentials` and `~/.aws/config` (honoring `AWS_SHARED_CREDENTIALS_FILE` / `AWS_CONFIG_FILE`) with a small pure INI parser rather than the SDK's shared-config loader, so the profile list, the merge precedence (credentials file wins) and the `[profile x]` vs `[x]` section naming are all directly testable. Profiles that delegate rather than carry keys (`sso_session`, `role_arn`, `credential_process`) are recognised by `AWSProfile.Delegates` and imported as **delegating** s3duck profiles — the SDK resolves them on every use, which is also what keeps them refreshed (see *Credentials, delegated* below). Only a profile with neither keys nor a delegation mechanism is listed with the reason it cannot be imported. `Ctrl+I` on the profiles screen imports the selected one as `aws-<name>`, de-duplicated by `uniqueProfileName`.

## Sync

`Sync` (Ctrl+E) mirrors a local directory against the current bucket+prefix in either direction. It is the only operation that can both overwrite and delete, so the flow is always **scan → dry-run plan → explicit Apply**; there is no way to run it unreviewed.

- **Spec.** A run is described by one `syncSpec` (direction + local dir + source and destination bucket/prefix). Introducing it replaced a growing parameter list: the local↔remote flows only ever needed one bucket, but a remote↔remote run needs two, and threading both through every function is where mistakes would have lived. `collectSides` is the single place that knows which side comes from where.
- **Collect.** `model.WalkLocal` walks the local root into `SyncEntry{Rel, Size, Mod}` (regular files only — a directory has no counterpart to compare against); `model.ListRemoteEntries` does the same from a paginated `ListObjects`, skipping folder-marker keys. Both sides key on a slash-separated path relative to their root, so they compare directly. `collectSides` then applies the run's **exclude patterns** (`applyExcludes`, matcher syntax) *before* anything else looks at the entries — an excluded path is invisible to the planner, so a delete-extraneous run cannot decide it is extraneous — and, when asked, fills each side's `Sum` for the paths present on both (`fillSums`).
- **Plan.** `planSync(src, dst, del)` is pure. When both sides carry a checksum with the same algorithm, that decides it: equal sums mean no transfer whatever the timestamps say, different sums mean a transfer whatever the sizes say. Otherwise a file transfers when it is missing at the destination, when the sizes differ, or when the sizes match but the source is newer by more than `syncModTolerance` (2s, absorbing clock skew and coarse filesystem/S3 timestamp granularity). A zero timestamp on either side degrades to a size-only comparison rather than forcing a transfer. Deletes are emitted **only** when the flag is set. Output is ordered creates → updates → deletes, each group by path, so the plan is deterministic and reviewable.
- **Apply.** `runSync` reuses the transfer-job machinery (`addJob`/`jobSem`/`finalizeJob`), so a sync is cancellable and backgroundable like any other transfer and honors the bandwidth limiter. Operations run through a **4-worker pool** (`syncWorkerCount`, never more workers than work), in two phases from the pure `splitSyncPhases`: every write completes before any delete starts, so a run that is cancelled partway leaves the destination having *gained* the new files but not yet *lost* the old ones — the safer intermediate state. Within a phase the operations are independent by construction (a path is either present at the source or not), so they interleave freely. Shared counters and the throttled redraw sit behind one mutex, and per-file byte counts are tracked in an `inFlight` map keyed by plan index so the displayed total stays correct with several transfers in progress. Per-op work goes through `model.UploadFile` (upload to an explicit key — `Model.Upload` derives keys from a directory walk and can't target one), `model.DownloadTarget`, `model.DeleteKey`, or `os.Remove`. Failures are collected, not fatal: one unreadable file doesn't strand the rest.
- **Remote → remote.** The planner never knew which side was local — it diffs two `[]SyncEntry` — so the third direction needed only a second `ListRemoteEntries` call and one branch in `applySyncOp`, which issues a server-side `CopyObject` instead of an upload. Two consequences worth knowing: a server-side copy moves no bytes through this process, so there is **no byte progress** for a remote→remote run (the op counter is the only thing that advances); and it inherits the same-endpoint constraint documented below, since both sides go through one client.
- **Safety.** Deletes are skipped entirely when any write failed — a partly-written destination is not the mirror the reviewed plan assumed, so removals are no longer covered by the user's approval. A remote→remote run between overlapping prefixes of the same bucket is rejected up front (`prefixesOverlap`): the source listing would include the destination, so src-inside-dst with delete-extraneous would delete the physical source objects, and dst-inside-src re-nests one level per run (`mirror/mirror/…`). `DeleteKey` refuses any key ending in `/`, so a sync delete can never degrade into `Delete`'s recursive prefix removal. For files the reviewed plan marked as updates, the download op passes `overwrite=true` and `DownloadTarget` swaps the file atomically (temp + rename) once the body is fully on disk — it used to pre-remove the stale copy, which destroyed the local file even when the transfer then failed. `WalkLocal` follows a symlinked root (`walkFollowingRoot`): `filepath.Walk` lstats its root, so a symlinked directory used to produce an *empty listing with a nil error* — precisely the partial-listing-taken-as-truth case the doc comment promises can't happen, and with delete-extraneous set it planned deleting the entire destination.

## Object metadata, tags and storage class

S3 has no metadata-update API, so `PutObjectMeta` rewrites an object by copying it **onto itself** with `MetadataDirective=REPLACE`. The storage class is passed through explicitly: a replace-copy that omits it silently demotes the object to STANDARD. `SetStorageClass` is the mirror image — it leaves `MetadataDirective` at its COPY default so the existing metadata rides along untouched (verified against MinIO: changing the class preserves Content-Type).

Tags live behind a separate API pair, so the editor fetches both and **writes back only the halves that changed** — a metadata save is a full object copy, far too expensive to do for a tags-only edit. Tagging is optional on S3-compatible backends, so a failing `GetObjectTagging` degrades to "no tags" with a note in the form rather than blocking the editor. Tag limits (10 tags, 128/256 char key/value) are checked client-side so the user gets a precise message instead of a generic `InvalidTag`.

`ParseRestoreStatus` reads the `x-amz-restore` header. It cannot split on commas: the value is quoted and an RFC 1123 expiry date contains commas of its own (`expiry-date="Fri, 21 Dec 2012 00:00:00 GMT"`), so each directive is read up to its closing quote. `IsArchived` deliberately excludes `GLACIER_IR` — instant retrieval needs no restore despite the name.

Both forms are read back with `GetFormItemByLabel` and the exported `view.Field*` constants rather than by index. They mix `TextView` rows with inputs, and a positional read returns the wrong widget the moment a row is inserted — the trap the profile form already sprung when the session-token field was added.

## Object versioning

`ListVersions` gives one object's history. `ListObjectVersions` is **prefix**-based, so it also returns every key that merely starts with this one; the exact-key filter is what turns it into a per-object history (verified: a sibling `doc.txt.bak` does not leak into `doc.txt`'s history). Pagination is followed to the end, since a heavily-rewritten object easily exceeds a page. Delete markers are listed alongside real versions rather than filtered out — the marker *is* what makes an object look deleted, and removing it is how you undo that.

`RestoreVersion` copies the chosen version to the top of the history rather than rewinding, which is the only non-destructive way to go back in a versioned bucket; the e2e run confirms the history grows from 3 entries to 4. `DeleteVersion` is the one genuinely destructive action here — it removes the data outright and leaves no delete marker — so it is behind an explicit confirmation that says so. Downloads are saved as `name.<8-char-version>.ext` so several versions can coexist in one directory. `D` diffs the selected version against the current one (`GetVersionContent` for both bodies, then `unifiedDiff`); a delete marker has no body and is refused rather than diffed.

## Duplicate finder

`D` lists the current prefix recursively and groups objects by **(size, ETag)** — the strongest content signal S3 offers without downloading. For single-part uploads the ETag is the body's MD5, so a match means identical content; multipart ETags depend on the part split, so identical files uploaded differently won't group — a missed duplicate, never a false positive (and size is the second factor guarding against multipart-ETag collisions). The help line discloses this. `ObjectChecksum` (see *Integrity*) is what would close that blind spot; doing it costs one request per candidate, so it is on the roadmap as an opt-in deep scan rather than folded into the default grouping. ETag quotes are normalized because some backends omit them.

`findDuplicates` is pure: groups sort by wasted bytes descending (the first group frees the most), members oldest-first — the oldest copy is the likeliest original and is marked `*`, making everything after it a natural deletion candidate. The browser is two `tview.List` views on one page (`modal-dups`): groups → members, with Enter revealing a copy via `revealKey` and `d` deleting it behind a confirm. Deleting updates the group through the pure `dropDupMember`, which also answers the question the view must not get wrong: after a dissolve, the same index denotes the *next* group, so only an explicit "did this group survive" result prevents teleporting the user into an unrelated group's members with the same `d`-to-delete binding. The surgery runs on the UI goroutine (the member list stays live during the network delete, and its handlers read the same slice), only the `DeleteKey` round-trip happens in the goroutine. The scan's bucket, prefix and client are captured at entry, per the transfer-capture rule.

## Edit in $EDITOR

`e` loads a small object into memory (`GetObjectContent`, 1 MiB cap enforced against both the reported length and the actual body — a lying backend must not balloon the process), sniffs for NUL bytes with git's binary heuristic, writes a 0600 temp file, and hands the terminal to `$EDITOR` (→ `$VISUAL` → `vi`) via `Application.Suspend` — which takes the app's own locks and is safe to call from the download goroutine. On a changed file the body is written back with `PutBytes`, which carries **everything replicable**: Content-Type, Cache-Control, Content-Disposition/Encoding/Language, user metadata, the storage class (omitting it would demote to STANDARD — the same trap `PutObjectMeta` guards), and the tag set (fetched only when the GET reported a `TagCount`, so tag-less backends are never asked). An unchanged file uploads nothing.

Saving is guarded against the **lost update**: the ETag captured at download time is compared (HEAD) against the object's current ETag just before the PUT — the editor may sit open for minutes while a backgrounded sync rewrites the key. On a mismatch, and on any upload failure, the save is refused and the temp file is *kept*, with its path in the error, so the user's edit is never destroyed along with the conflict. A HEAD-compare rather than `If-Match`: conditional-PUT support is spotty across S3-compatible backends, and a narrowed race window with universal compatibility beats an atomic guard that only works on AWS.

## Cross-profile copy

`>` copies the marked set to a bucket in a different profile. `model.CrossCopy` streams each object through this process — a GET from the source client feeds a multipart PUT on an independent destination client — which is the only copy that works across *endpoints*; server-side `CopyObject` requires both buckets behind one endpoint. The content headers (Content-Type, Cache-Control, Content-Disposition/Encoding/Language), user metadata and tags ride along from the source; the **storage class deliberately does not** — class names are not portable across providers (STANDARD_IA would fail the whole PUT on a backend that doesn't know it), so the destination's default applies. The source bandwidth limiter throttles the read side (bounding the whole pipe), and the destination uploader carries the standard retryer. The flow is profile → bucket → prefix, then a cancellable, backgroundable transfer job; folders expand to concrete objects with `crossDstKey` keeping the tail relative to the source location, so a copied folder keeps its name and structure. Item sizes are captured at entry on the UI goroutine (the listing they came from may be gone by transfer time), the source client is captured alongside them, and both expansion-error paths tear the progress modal down before reporting. The source is never modified, and the destination's region is resolved fail-safe (`RefreshClient`) before the transfer.

## Pane comparison

`=` diffs the two dual-pane locations and shows the result read-only. It is the same `planSync` the sync preview uses — deliberately, so the two can never disagree about what counts as a difference — run with `del=true` so entries present only on the right are reported as well; a comparison must be symmetric even though the planner is directional. `comparePlanText` re-words the three kinds (`left-only` / `differs` / `right-only`) because a comparison has no notion of creating or deleting, and states plainly that only names and sizes were compared. `showPlan` is shared with the sync preview and simply omits the Apply button when there is nothing to apply.

## Bucket config & multipart

`model.BucketConfig` gathers versioning/encryption/object-lock/region, treating endpoints that don't support a feature (MinIO/Ceph return an error) as its default rather than failing. `ListMultipartUploads`/`AbortMultipartUpload` surface and clean orphaned upload parts (single-page list, ample for cleanup). Both are palette actions on the current bucket.

## Selection scoping

Multi-select state is keyed by `bucket:path` so selections survive navigation in and out of subfolders:

```go
selectedByScope["my-bucket:photos/2024/"] = {"vacation.jpg": true, "trip.mp4": true}
```

A local pane keys its own scope as `local:<directory>` (`localPaneScope`): sharing the empty scope with the buckets screen would leak marks between the two.

`mu` guards `selectedByScope` because selections are read by input handlers (UI goroutine) and cleared by post-download/move cleanup (network goroutines).

---

## Object identity

`objKey(o)` produces the unique key used in the `objs` map, selection sets, and list secondary text:
- **Buckets**: `*o.Key` (the bucket name)
- **Files / Folders**: `*o.FullPath` (the full S3 prefix, e.g. `photos/2024/vacation.jpg`)

Using `FullPath` avoids collisions between a file `"x"` and a folder `"x/"` at the same prefix level. A **local** pane's entries reuse the same struct with `FullPath` set to the absolute filesystem path — directories keeping a trailing separator — so the same rule holds there and every selection, filter and column path works unchanged.

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
  "session_token": "plaintext, optional — static temporary credentials only",
  "ignore_ssl":   false,
  "download_dir": "~/Downloads/s3",
  "max_bytes_per_sec": 0,
  "bookmarks":    [{"name": "photos/2024/", "bucket": "photos", "prefix": "2024/"}],

  "aws_profile":      "sso-dev",
  "read_only":        false,
  "no_mime_detect":   false,
  "sse":              "",
  "sse_kms_key_id":   "",
  "checksum_algo":    "CRC32C",
  "verify_downloads": true,
  "trash":            false,
  "trash_prefix":     "",
  "last_bucket":      "reports",
  "last_prefix":      "2026/"
}
```

The second group is edited on its own form (`o` on the profiles screen) rather
than on the connection form: tview's `Form` does not scroll, and the
connection form is already as tall as a small terminal can show. Splitting by
*kind* — how to reach the storage, versus how this app should behave against
it — also keeps each form readable. Every field is optional and omitted when
unset, so a config file written by an older build loads unchanged; note that
`no_mime_detect` is stored inverted for exactly that reason, so an existing
profile gets content-type derivation *on* rather than off.

`secret_key` and `session_token` are stored in plaintext. The file has `0600` permissions (owner-readable only), but there is no OS-keychain integration. Future work: `github.com/zalando/go-keyring` — less pressing now that `aws_profile` stores no key material at all, though still the answer for a static MinIO key.

Two more files sit beside it, both optional:

| Path | Written by | Purpose |
|---|---|---|
| `~/.config/s3duck-tui/keys.json` | the user | Rebinds any action; see *Keymap*. Never written by the app. |
| `$XDG_STATE_HOME/s3duck-tui/activity.log` | the app | Append-only operation log, mode 0600, rotated once at 2 MiB; see *Persistent activity log*. |

---

## S3 compatibility notes

### Region handling

For non-AWS endpoints (MinIO, Ceph, etc.), the region field in the endpoint resolver is set from the profile config and `HostnameImmutable: true` is used so the SDK never rewrites the URL.

For AWS endpoints, `GetBucketLocation` is called on bucket entry to discover the bucket's actual region, then the client is rebuilt with that region. This avoids redirect loops for cross-region bucket access. If the lookup or the client rebuild fails, `RefreshClient` now leaves the existing client and region **untouched** and the error is surfaced in the UI — the old behaviour silently pinned everything (including `CreateBucket`) to a guessed `us-east-1`, turning one denied `s3:GetBucketLocation` into a stream of baffling redirect errors. Legacy constraint values are normalized (`""` → us-east-1, `"EU"` → eu-west-1).

### Prefix normalization

Turning a browsing path into an S3 prefix (slash-separate it, and append a trailing `/` unless it is the bucket root) was open-coded in five places. It now lives in one exported helper, `model.NormalizePrefix`, used by `localDownloadPath`, `showSummaryModalFor`, the copy/move destination, `PrepareUpload`, `DownloadTarget` and sync. Consolidating also fixed a latent bug on the copy/move destination field: a user typing a bare `/` used to produce keys with a leading slash, where the helper reads it as the bucket root.

### HTTP timeouts

The shared client uses **per-phase** timeouts (dial 10s, TLS 10s, first response byte 30s) rather than `http.Client.Timeout`. The whole-request form spans the body too, so with 5 MiB parts any link slower than ~1.4 Mbit/s would have every part killed mid-transfer and retried into a hard failure — and the bandwidth throttle, which sleeps inside the download's body-read path, could trigger the same thing on a fast link. Hung connections are still bounded; a healthy transfer may take as long as it takes.

### Folder markers

S3 has no real directories. The app follows the S3 convention:
- A folder is a common prefix returned by `ListObjectsV2` with `Delimiter=/`
- An empty folder is represented by a zero-byte object whose key ends with `/`
- Download preserves the tree via `os.MkdirAll` for marker keys
- Upload creates marker objects for empty local directories

### Delete

`model.Delete` handles both files and folder prefixes. For folders it calls `ListObjects` (no delimiter, full recursive listing) then batches `DeleteObjects` in pages of 1000, which is the S3 API maximum.

---

## Overlay sizing (0.9.0)

Every modal is centred by `centerModal(p, width, height)` at a size its caller
picks, which is fine for forms — they know how tall they are — and wrong for
anything whose content grows. The hotkey panel outgrew its hard-coded 44 rows
and the last six bindings became unreachable: the panel was also closing on
*any* key, so the `TextView` underneath it (scrollable by default in tview)
never saw an arrow key.

`View.ModalClamped` shrinks a requested size to what the terminal can actually
show, using `View.ContentSize()` — the terminal size cached by a
`SetBeforeDrawFunc` hook, minus six rows of `Frame` chrome (a blank line plus
the version line at the top, the hotkey line plus a blank line at the bottom,
and one `Frame` header/footer row on each side). tview's `Application` exposes
no size getter in this version, hence the hook rather than a direct query.

The hotkey panel is then a `Flex` of the scrolling `TextView` plus a one-line
footer, sized `lines + 3` and clamped. Its *body* for the browser screen is
generated from the live keymap (`keymap.helpLines()`), so the panel describes
the keys as bound rather than as once written down; the profiles screen keeps
its static list, whose keys are not configurable. Word wrap is **off** inside it: the text
is written to fit the panel width, and wrapping a two-column key/description
list only ever folded descriptions back to column zero. The footer is drawn
straight to the screen with `tview.Print` from a `SetDrawFunc`, because it
reports the scroll offset — which is only final after the list above has drawn,
and drawing it directly means there is no widget state to mutate mid-draw. It
names the scroll keys and, when the list does not fit, which lines are showing;
a clipped list with no footer looked complete, which is how its tail went
unnoticed. Closing is explicit: Esc/Enter/Tab through `SetDoneFunc`, plus `q`
and Ctrl+H, with every other key falling through to the text view.

`pkg/view` got its first test file with this: `scrollHint` and the clamp
arithmetic are pure, and the panel itself is rendered onto a `tcell`
SimulationScreen at 80x24 and driven with real key events, which is what pins
"the tail is reachable" rather than "the constructor was called correctly".

## Content type, encryption and checksums on write (`model/write.go`)

Everything this app creates used to be born `application/octet-stream`:
neither `Upload` nor `UploadFile` set `ContentType`, and the s3manager
uploader does not sniff one. Every *other* path is meticulous about carrying
the header around — `CopyObject` inherits it, the multipart copy re-applies
it, `CrossCopy` forwards it, `PutBytes` preserves it — so the only place a
wrong type could enter was the one place an object is first written. The
symptom is invisible until something serves the bucket over HTTP, and the only
remedy (the metadata editor) costs a full server-side copy per object.

`WriteOptions` now travels on the `Model` (built from the profile by
`modelConfigFor`, the single place a stored profile becomes a connection) and
is applied at every creation point: both upload paths, `PutBytes`, folder
markers, the multipart-copy `CreateMultipartUpload`, and the destination side
of a cross-profile copy. It decides three things — the derived Content-Type,
the server-side encryption, and the additional checksum.

- **The MIME table is explicit before it is systemic.** `mimeTable` is
  consulted before `mime.TypeByExtension`, because that function reads
  `/etc/mime.types`: absent in a scratch container, and different between
  distributions, which would make an object's type depend on the machine that
  uploaded it.
- **An extension beats a sniff**, and a sniff is only used for extensionless
  files (`README`, `Makefile`) — a `.csv` sniffs as `text/plain` and a `.svg`
  as `text/xml`.
- **A sniff that lands on octet-stream reports nothing.** Saying nothing lets
  the server apply its own default, which keeps a re-upload of the same file
  from *changing* an object's type.
- **An input that already carries a type keeps it.** A copy or an edit knows
  the object's real type; `applyPut` only fills a gap.
- **Folder markers are encrypted but not typed.** A profile whose bucket policy
  requires SSE would otherwise fail folder creation.

## Integrity (`model/checksum.go`)

The ETag is only an MD5 for single-part uploads; a multipart ETag is a hash of
part hashes with a `-N` suffix, so it depends on the part size the uploader
happened to use. That single fact limited three separate features, and S3's
additional checksums close all three at once:

- a **post-transfer verify** covers multipart objects, not just small ones;
- the **duplicate finder**'s documented blind spot (identical files uploaded
  with different part sizes never group) becomes closeable;
- **sync** gains a real content comparison.

`ObjectChecksum` reads the stored checksum through `GetObjectAttributes` —
the only API that reports one — and falls back to a `HeadObject` ETag when a
backend does not implement it, so an unsupported endpoint yields "nothing to
compare" rather than an error. `VerifyLocalFile` prefers a whole-object
checksum, falls back to the ETag for single-part objects, and refuses to
compare a *composite* checksum against a whole-file hash: that false alarm
would be worse than no answer. A size difference is reported on its own
because "1.2 GiB vs 900 MiB" says more than a hash mismatch.

`LocalChecksum` encodes as S3 does — base64 for the additional checksums, hex
for MD5, which is only ever compared against an ETag — and CRC32C uses the
Castagnoli polynomial, which is the whole difference from CRC32 and the
easiest thing to get silently wrong (there is a test that pins it).

Sync's checksum mode fills `SyncEntry.Sum` as `"ALGO:VALUE"` for the paths
present on *both* sides only (a path missing at the destination is already a
create; hashing it would be work for no decision). `planSync` compares sums
when both sides carry the same algorithm and falls back to size+mtime
otherwise — which is what closes the two documented holes in the old rule: a
file edited in place to the same length is now seen, and a download-then-upload
no longer re-sends everything because the local mtime is newer.

## Streamed, cancellable listings (`controller/listing.go`)

`Model.List` paginated to exhaustion on `context.TODO()`. Entering a prefix
holding a hundred thousand keys at one level meant a hundred blocking round
trips that no keypress could interrupt, followed by all hundred thousand rows
landing in the list widget at once — the app looked hung with nothing to do
but kill it.

`ListStream`/`ListObjectsStream` hand each page to a callback with the running
total and stop when it returns false; the context is checked *between* pages as
well as passed into the request, so a cancelled scan stops at the next page
boundary at the latest. `updateList` then:

- reports "listing… N" in the pane title, throttled to 250 ms, writing to the
  **list widget captured before the fetch** — a `Tab` mid-listing must not
  redirect the counter into the other pane;
- stops at `listCap` (20 000) per level and says so;
- treats a cancelled listing as a *partial* result rather than an error: what
  arrived is kept and rendered, because a browsable first slice of a huge
  prefix is far more use than an empty pane. `partial` is per-pane, like the
  object map it describes, and the title discloses it.

The same ctx plumbing reached `ListObjects`, `ListBuckets`, `Delete`,
`EmptyBucket`, `ListRemoteEntries`, `ResolveDownloadObjects` and
`PlannedCopyKeys`, which closes the "cancellation doesn't reach listing
phases" hardening item: the delete scan, the summary scan, the duplicate scan,
the searches, the sync scan and the pane comparison all run behind
`cancellableWait` modals now and abandon their listing when cancelled.
Cancelling the delete *scan* abandons the delete rather than confirming
against half-counted totals.

What deliberately still runs on `context.TODO()` is the set of single-request
bucket-level calls — `CreateBucket`, `CreateFolder`, `DeleteBucket`,
`MakeBucketPublic`, `GetBucketLocation`, `BucketConfig`, `PresignGetURL`,
`ListMultipartUploads`, `AbortMultipartUpload`. Each is one round trip
bounded by the transport's per-phase timeouts, with no pagination to escape
from, so threading a context through them would add signature churn for no
behaviour a user could observe. `GetConfig`'s `LoadDefaultConfig` is the same
case.

## The failure ledger (`controller/opresult.go`)

Every multi-item operation correctly collected failures instead of aborting on
the first one — and then threw them away: the report printed the first eight
and "…and N more", and once dismissed there was no way to see the rest, let
alone act on them.

`opResult` keeps every failure with the flow's own descriptor (`retryItem.target`,
opaque here and type-asserted back by the retry callback that owns it). The
report then offers **Retry failed**, which re-enters the same flow with only
the failed units — a 5000-file sync that lost twelve objects to a flaky link
costs twelve operations, not another full plan — and **Export list**, which
writes all of them to a file, since a `tview.Modal` cannot scroll. Wired into
delete, sync, copy/move, batch rename and download; `text()` is pure, so the
wording of a partial failure is testable.

## Profile identity and read-only (`controller/guard.go`)

The browser screen never named the open profile: the list title carried
bucket, path, selection and sort, and nothing else. With dual panes, a
cross-profile copy and a `Delete` that empties buckets, "am I in staging or
production?" was unanswerable without leaving the browser.

`identityLabel` puts profile, endpoint host, region and a read-only marker in
the frame's header — the left field of the *same row* as the version string,
since tview's `Frame` puts differently-aligned header texts on one line, so it
costs no rows. `readOnlyBlocked` is one guard called from each mutating entry
point (rather than a mode every action would have to interpret) and it names
the action it refused, because "nothing happened" is the failure mode it
replaces. The cross-profile copy checks the *destination* profile's flag: that
copy writes there and only reads here.

## Background transfer notices (`controller/notify.go`)

Backgrounding a transfer is a promise — "carry on, I'll tell you when it's
done" — and the app kept the first half only: `finalizeJob` wrote one activity
line and nothing else. The only way to find out was to keep opening the
transfers panel, which defeats the point.

A single long-lived ticker recomputes a header badge twice a second and queues
a redraw **only when the text changed**, so an idle app costs a string compare
and no draws. `finalizeJob` announces a job that was backgrounded (never a
foreground one, whose modal is already reporting) as a transient notice plus
the terminal bell — through `Screen.Beep()`, captured in the view's
before-draw hook, because this tview version exposes no screen getter and a
raw write would land inside the rendered frame. A modal would have been wrong:
it would steal focus from whatever the user moved on to.

## Credentials, delegated (`GetConfig`)

Static keys are fine for a long-lived MinIO key and structurally wrong for
everything temporary: an assume-role or SSO credential was stored as pasted
and simply started failing when it expired. It also left `~/.aws` profiles
that *delegate* (`sso_session`, `role_arn`, `credential_process`) unusable —
there is no key pair in them to copy.

`Config.AWSProfile` switches the credential provider to
`config.WithSharedConfigProfile`, handing the whole problem to the SDK's own
resolution chain, which knows how to run an SSO flow, assume a role, invoke a
`credential_process` and re-resolve on expiry. Nothing is stored in s3duck's
config but the profile's name. The AWS import now offers these profiles as
delegating ones instead of listing the reason they could not be imported; a
delegating profile that resolves to nothing is caught in `GetConfig`, while
the message can still name the profile.

## Local pane (`controller/localpane.go`, `model/local.go`)

Upload was a modal browser that took exactly one item per flow: no
multi-select, no sorting, no size or date columns, and no view of the
destination while picking. Meanwhile the dual-pane layout, the column engine,
the selection scoping and the transfer queue all existed — for remote panes
only.

The trick that keeps this small is that a local entry is described by the same
`model.Object` as a remote one (directories carry a trailing separator in
`FullPath`, so a file and a directory of the same name cannot collide in the
selection set). Filtering, sorting, column layout, selection and `renderList`
needed **no changes at all**; only navigation, the pane title and the transfer
verbs had to learn which side they were looking at:

- `localDir` is part of the swapped pane state, so a local pane survives `Tab`
  and is cleared by `resetPanes` (it must not outlive a profile switch);
- `scopeKey` gives a local pane its own selection scope — sharing the empty one
  with the buckets screen would leak marks between them;
- `Ctrl+Y` from a local pane uploads to the other pane's bucket+prefix
  (`LocalTargets` expands the selection, keeping each selected directory's own
  name at the destination); a download with a local pane open lands in that
  directory rather than the profile's download directory;
- `remoteOnly` refuses the object-only actions. The local pane deliberately
  does **not** delete, rename or create: those keys have S3 meanings people
  have muscle memory for, and a mis-aimed one outside the bucket is not
  recoverable by anything this app offers. A move across the boundary is
  refused for the same reason.

## Usage browser (`controller/usage.go`)

`buildSummary` answered "how big is this?" one level deep, bytes only, and
silently truncated to the top ten — so on a bucket with a hundred prefixes the
question "where did the other 4 TB go?" had no answer, and nothing said the
table was incomplete.

`buildUsageTree` folds the same recursive listing (one pass, no extra
requests) into a prefix tree whose sizes and counts accumulate up the tree, and
the browser walks it: children largest-first — size *is* the ordering here, so
folders are not floated to the top the way the file listing does it — with
share bars, object counts and a storage-class breakdown at every level. `g`
hands the location to the ordinary browser, because every action that follows
from "where are the bytes" already exists one screen over. The flat summary
now folds its overflow into one disclosed row instead of truncating.

## Preview and version diff (`controller/preview.go`)

Inspecting an object's contents meant downloading it or opening `$EDITOR`,
which refuses anything binary or over 1 MiB. The missing piece was the ranged
GET: `GetObjectHead` reads a fixed 64 KiB window, so previewing a 40 GiB log
costs one small request, and the read is capped independently of the range
because some S3-compatible backends ignore `Range` entirely.

Binary content is rendered as a hexdump rather than refused — the question a
preview answers about a blob is usually "what *is* this?", and magic numbers
answer it. Content goes through `sanitizeText` (control bytes to dots, tabs
expanded) and `escapeForTextView` (`[` doubled), or a log line containing
`[red]` would be swallowed as markup. The viewer is a scrollable `TextView`
inside `ModalClamped`, not a `Modal`: a Modal's text cannot scroll, and a
preview that shows only what fits is not a preview.

`unifiedDiff` is a plain LCS walk rather than Myers — the inputs are capped at
1 MiB, where the quadratic table costs nothing worth optimising — and shows
full context, because a version diff is usually a config file where the
surrounding lines are the point. Identical bodies produce empty output, which
is what lets the caller say "identical" instead of opening an empty window.

## Trash (`controller/trash.go`)

Undo covered move and rename; delete was the one irreversible action, and on an
unversioned bucket a mistaken folder delete had no remedy at all. With safe
delete on, `Delete` becomes a server-side move into
`<trash>/<YYYYmmdd-HHMMSS>/<original key>` — no bytes cross the wire, and the
dated folder is what keeps two deletes of the same key from colliding and what
makes a restore unambiguous (`restoreKeyFrom` inverts the mapping). Deleting
something already *inside* the trash removes it for real: otherwise the trash
could never be emptied and each attempt would nest a deeper copy of it inside
itself. Emptying the trash sizes it first and says plainly that the objects are
not recoverable.

## Keymap (`controller/keymap.go`)

The browser's key space was full: `Ctrl+A`–`Ctrl+Y` almost without a gap, and
`y x p u t s S r v m c e D > = / [ ]` besides. Every new feature had to
displace a binding or hide in the palette, and a user who wanted a different
layout had no way to ask.

Bindings are now data: `defaultBindings()` pairs each named action with a chord
and a description, `~/.config/s3duck-tui/keys.json` overrides any of them, and
a leader key (default `,`) opens a second namespace for what no longer fits.
`actionTable()` is the app's vocabulary; the palette and the hotkey panel are
both generated from the pair, so the panel can no longer drift from what the
keys do. Details worth keeping:

- **The defaults are exactly what the keys did before**, pinned by a test that
  lists them, because changing one is a user-visible break.
- **A rebound action loses its default chord** — leaving both working is the
  confusing half-state.
- **An unparseable override leaves the default in place** (only a *usable*
  override counts as one), so one typo cannot silently remove a key with
  nothing to replace it. Warnings are shown once at startup and logged.
- **Ctrl+letter is matched by key constant, not by rune**: tcell reports it as
  `KeyCtrlD` with the rune cleared, so `chordKey` canonicalises both a parsed
  spec and a live event the same way. Shift is not part of the canonical form —
  it is already expressed in the rune, and terminals do not report it
  consistently for anything else.
- **An unknown key after the leader does nothing** rather than falling
  through: `,` then a typo must not delete anything.

## Persistent activity log and session restore (`controller/audit.go`)

The activity log was a 200-entry ring that died with the process. For a tool
that empties buckets, moves objects between accounts and deletes versions
permanently, "what did I do yesterday?" is a reasonable question with nowhere
to look.

`logActivity` now also appends `timestamp<TAB>profile<TAB>message` to
`$XDG_STATE_HOME/s3duck-tui/activity.log` (mode 0600, rotated once at 2 MiB —
two files is enough to answer "what happened recently", and unbounded rotation
is a disk-space surprise). Message newlines and tabs are folded to spaces, so
a key or an error text cannot forge an entry or shift the columns. Write errors
are deliberately dropped: an unwritable log must never interrupt the operation
it is describing.

The same file's neighbour question — "where was I?" — is answered by
`LastBucket`/`LastPrefix` on the profile, recorded in memory on navigation and
written out when the profile is closed or the app exits (one config rewrite per
navigation would mean rewriting a file full of credentials dozens of times a
minute). `restoreLocation` must run on the UI goroutine, because `jumpTo`
clears the filter box and the details pane synchronously before spawning its
own goroutine.

## Query syntax (`controller/match.go`)

The filter and the search were case-insensitive substring tests. One matcher
now serves the in-listing filter, the recursive search and sync's exclude list,
with the mode picked from the query itself so there is no toggle to discover:
plain text is a substring, anything containing `*` or `?` is an anchored glob,
and an `re:` prefix is a regular expression (case-insensitive unless the
pattern says otherwise).

Two decisions worth recording. The glob dialect is two metacharacters wide and
`*` **crosses `/`**, unlike `path.Match`: the recursive search matches full
keys, and `*.log` there has to find `logs/2024/app.log` or the feature is
useless. And `globMatch` is an iterative two-cursor scan with one backtrack
point rather than recursion — a pattern like `*a*b*c` over a long key would
otherwise branch exponentially, and keys here arrive from listings that can
hold a hundred thousand of them. A query that fails to compile degrades to
*matching nothing*, with the reason in the pane title: silently ignoring it
would look like the filter had been applied and found everything.

## Known limitations

| Area | Description |
|---|---|
| **Navigation race** | `c.currentBucket` and `c.restoreNext` are written from the goroutines spawned by `Down()`/`jumpTo`/`navigateTo` without the `mu` mutex. In single-pane use navigation is serial, so races are unlikely — but in dual-pane, a Tab during a slow bucket-open swaps the live fields mid-write and the navigation lands in the other pane. A proper fix passes navigation state through the refresh path instead of mutating it in place. Transfers are no longer affected: every transfer flow captures its bucket/path *and its client* at entry. |
| ~~No retries on upload~~ | **Fixed.** Both upload paths share `newUploader`, which installs the SDK's standard retryer (`uploadMaxAttempts` = 3). Retries are safe because every part is re-read from the file rather than from a consumed buffer. |
| **Sequential per-file overwrite** | Overwrite decisions block Phase 1 completion. For a large selection with many conflicts, the user must click through each dialog before any download starts. |
| **Remote→remote sync has no byte progress** | The transfer happens inside S3, so the client sees only completed operations. The op counter advances; the byte gauge does not. |
| **Cross-bucket copy/move is same-endpoint only** | `CopyKeys` / `MoveKeys` take separate source/destination buckets and issue a server-side copy, so both buckets must be reachable through the one configured endpoint. This covers any single S3-compatible endpoint (MinIO/Ceph) and same-region AWS. Copying between AWS buckets in *different regions* is not handled (the client stays pinned to the source region); across *profiles*, `>` streams through the client instead. |
| ~~Copies fail above 5 GiB~~ | **Fixed.** Sources over `MultipartCopyThreshold` are copied part by part with `UploadPartCopy` (see *Large copies*). |
| **No download resume** | Interrupted downloads restart from byte 0. Transfers write to a sibling `*.s3duck-part` temp file that is removed on cancel/failure; the target file is only ever replaced by a completed download. |
| ~~Sync compares size + mtime, not content~~ | **Fixed (optional).** The sync form's *Compare content by checksum* fills `SyncEntry.Sum` from the object's stored checksum (or its single-part ETag) and a locally computed one, and `planSync` compares those when both sides agree on the algorithm. It costs one request per remote object and a full read per local one, so it is a choice rather than the default; paths where either side cannot produce a comparable sum fall back to size+mtime. |
| **An upload sync straight after a download sync re-uploads** | A downloaded file's local mtime is its download time, which is newer than the object's `LastModified`. Reversing the direction therefore sees "source is newer" for every file and re-sends them once (sizes are equal, so nothing is corrupted, and the second reversal is a no-op). This matches `aws s3 sync` semantics; the dry-run plan shows it before anything moves — and *Compare content by checksum* avoids it entirely. |
| ~~Sync applies one operation at a time~~ | **Fixed.** `runSync` now uses a 4-worker pool with a writes-then-deletes barrier (see *Sync* above). |
| **Sync direction is one-way** | Each run treats one side as the source of truth. There is no bidirectional merge and no conflict resolution — the newer-wins rule only ever applies in the chosen direction. |
| **Static session tokens expire, silently** | A *stored* temporary credential is used as-is: when it expires, calls start failing with an auth error and s3duck neither refreshes it nor warns beforehand. The fix is not to store one — set the profile's `aws_profile` instead and the SDK re-resolves (and re-runs SSO / assume-role) on expiry. |
| **Versioned buckets can't be emptied from the TUI** | `model.Delete` sends no `VersionId`, so folder deletes write delete markers only; `EmptyBucket` clears current objects but old versions survive, and `DeleteBucket` then fails with BucketNotEmpty. A version-aware purge is on the roadmap. |
| **Whitespace keys** | Every secondary-text reader trims the key, so `"dir/report "` resolves to `"dir/report"` in lookups (wrong object if both exist, silent no-op if only the padded one does). |
| **Versioning needs a versioned bucket** | On an unversioned bucket S3 reports a single `null` version; the browser shows exactly that rather than hiding the feature. Enabling versioning is a bucket-level operation s3duck does not perform. |
| **Restore is asynchronous** | `RestoreObject` only *requests* a restore; completion takes minutes to hours and s3duck does not poll. Re-open the object (`c` or Ctrl+L) to see the current state. |
| **Metadata edits rewrite the object** | A metadata save is a server-side copy onto the same key. On a versioned bucket that creates a new version; the ETag may also change for multipart objects (and always does when the object is large enough to take the multipart-copy path, which re-chunks it). |
| **Undo and sync do not prompt before overwriting** | Every other remote write confirms first (see *Overwrite confirmation*). Sync is exempt because its dry-run plan already lists the updates; undo because it has its own confirmation and restores objects to where they just were. |
| **The inactive pane keeps its previous column layout** | `renderList` renders the active pane, so right after `Ctrl+O` the other pane still shows columns sized for the previous width. It self-heals the moment you `Tab` to it (`swapAndFocus` re-fetches and re-renders). Fixing it properly needs a render path that can target a pane other than the active one. |
| ~~Summary top-10 cap~~ | **Fixed.** The graph folds everything past the top ten into one disclosed row, and the usage browser (`G`) walks the whole tree without a cap. |
| ~~Cancellation doesn't reach listing phases~~ | **Fixed.** Context is plumbed through `List`/`ListObjects`/`ListBuckets`/`Delete`/`EmptyBucket`/`ListRemoteEntries`/`ResolveDownloadObjects`/`PlannedCopyKeys`, and every scan (delete sizing, summary, duplicates, search, sync, compare) runs behind a cancellable wait modal. |
| **Listings cap at 20 000 objects per level** | A prefix with more keys at one level is listed to `listCap` and marked *partial* in the title; the rest is reachable through the recursive search (Ctrl+F) or the usage browser. The cap exists because a `tview.List` holding a hundred thousand rows is slower to render than the listing was to fetch. |
| **The local pane never writes locally** | It browses, marks and uploads; it does not delete, rename or create. Deliberate: those keys have S3 meanings, and a mis-aimed one outside the bucket is beyond anything this app can undo. A move across the pane boundary is refused for the same reason. |
| **A trashed object still costs storage** | Safe delete is a move, not a removal: the objects stay in the bucket (and keep their storage class) until the trash is emptied. On a versioned bucket the move also leaves a delete marker at the original key. |
| **Verification cannot cover every object** | A multipart object with no additional checksum can only be compared by size, and `VerifyLocalFile` says so rather than comparing a hash that could never match. Set the profile's `checksum_algo` and objects written from then on are fully verifiable. |
| **`GetObjectAttributes` support varies** | S3-compatible backends may not implement it; `ObjectChecksum` then falls back to the HEAD ETag, which limits verification to single-part objects. Reported, not silently degraded. |
| **The keymap is app-wide, not per-screen** | `keys.json` rebinds the browser's actions; the profiles screen and the modal overlays keep their built-in keys. Rebinding a chord the modals also use (Esc, Enter) affects only the browser. |
| **The activity log is append-only and unencrypted** | It records bucket and object names (never credentials) at mode 0600, rotated once at 2 MiB. Delete the file to clear it; there is no in-app purge. |

---

## Fixed in the 0.8.0 follow-up review

A second review pass, this one over the 0.8.0 work itself — the deep-review
fixes below plus the two features. All found by reading the new code and by the
integration suite:

- **A large metadata save dropped the object's tags.** `CopyObject` carries
  tags along even under `MetadataDirective=REPLACE`, but the multipart path
  only re-applied them for plain copies — so a metadata edit on an object over
  the threshold silently lost them. Tags are now re-applied on every multipart
  copy, and read from the source *version* (a version restore was reading the
  current object's tags).
- **A failed completion leaked the upload.** Only a failed *part* aborted the
  multipart upload; a failed `CompleteMultipartUpload` left it open with its
  parts accruing storage charges.
- **A skipped upload reported the wrong progress.** `Upload` filtered skipped
  files inside its send loop but had already totalled every walked file, so the
  gauge counted bytes that were never sent and the per-file index counted files
  it skipped. The filter now runs before the totals, through a shared
  `uploadKey` helper so it cannot drift from `PrepareUpload`'s keys.
- **A conflict scan could enumerate a whole bucket.** Keys at the bucket root
  share no common prefix, so a paste of more than a handful of loose files
  listed everything. Folder candidates now use a `MaxKeys=1` probe, plain keys
  are probed concurrently up to a higher limit, and only genuinely large
  batches fall back to a (scoped) listing.
- **A fully-skipped batch rename recorded an undo entry** pointing at a
  destination that was never written.

## Fixed in the 2026-08-11 deep review (0.8.0)

Four parallel review passes over the whole tree; every finding was verified against the code before fixing. The criticals, compressed:

- **Filter deadlock** — the filter's `SetChangedFunc` called `renderList` (→ `QueueUpdateDraw`) inline from the event loop; the first character ever typed into `/` froze the app. Now dispatched on a goroutine.
- **Download path traversal (zip-slip)** — keys with `..` segments escaped the download directory in both the controller's path derivation and `DownloadTarget`. All local paths now go through `SafeLocalPath`, which rejects escapes loudly.
- **Overwrite pre-delete** — "Overwrite" removed local files at prompt time, before the transfer (possibly queued for minutes) moved a byte; a cancel lost them all. Downloads now land in a temp file renamed over the target on success only.
- **Wrong-profile transfers** — queued/backgrounded transfers read `c.model` lazily; opening (or merely Ctrl+V-verifying) another profile swapped the client under them, sending writes to a same-named bucket in the wrong account. Every transfer flow captures the client at entry; `CheckProfile` uses a throwaway client.
- **Cross-profile clipboard/undo** — `Duck` now clears both; a paste/undo after a profile switch executed against the new endpoint with the old names (a cut-paste even *deleted* from it).
- **Move deleted uncopied objects** — `MoveKeys`' delete phase re-listed the source prefix after the copy, sweeping up objects written in between (e.g. by a backgrounded upload during a rename). It now deletes exactly the keys it copied.
- **Symlinked roots walked empty** — `filepath.Walk` lstats its root, so uploads/syncs on a symlinked directory saw zero files with a nil error; with delete-extraneous a sync then planned deleting the whole destination. All walks go through `walkFollowingRoot`.
- **Empty-list panic** — `getSelectedObjectName` indexed an empty buckets list (Ctrl+L/W/G with no buckets crashed the app).
- **Config wipe** — `WriteConfig` truncated in place (a crash mid-write corrupted every profile) and, after a failed load, the next save overwrote the corrupt original with the in-memory (empty) list. Now write-temp-then-rename, with the unreadable original preserved as `.bak`.
- **Edit lost-update** — saving from `$EDITOR` blindly PUT over whatever the object had become; now guarded by an ETag compare, and a refused/failed save keeps the edited temp file.

Also fixed: attribute stripping on edit/cross-copy (Cache-Control & friends, storage class, tags); the dup-finder presenting the *next* group's members after a dissolve (plus its cross-goroutine slice race); async `c.error`/`c.success` destroying open forms (own page name); the profiles screen hijacking the Delete key inside forms; cross-profile copy totals computed from a wrong-keyed lookup and its stranded progress modals; the sync form losing everything when the Browse picker was canceled; downloads resolving folders on the UI goroutine (freeze) and swallowing resolution errors; the summary reporting against the pre-skip total; `Up()` skipping a level on empty path segments; folder create accepting `/`, `.` and `..`; paste with destination == source; phantom marks left in the source scope after a cut-paste; a cut consumed even when the paste failed; tview-tag-like object names recolouring rows; duplicate profile names accepted; `"amazon"` substring matching custom endpoints as AWS; the 5-second `ListBuckets` deadline; and `NewModel` swallowing config errors.
