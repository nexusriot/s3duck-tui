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

`pkg/controller` is intentionally large because the modal flows — profile CRUD, download with overwrite prompts, upload, copy/move, delete, summary — are deeply coupled to tview's callback/page model; splitting them would mean passing around opaque page/modal handles. It is split by feature where a feature is self-contained: `controller.go` (everything above) plus `sync.go` (directory sync), `objectmeta.go` (metadata / tags / storage class / restore) and `versions.go` (version history). Each splits cleanly because its logic is pure or reaches shared state only through existing helpers. `pkg/model` mirrors the shape: `model.go` plus `sync.go`, `object.go` and `versions.go`.

The tree is `gofmt`-, `go vet`- and `staticcheck`-clean; keep it that way. Controller actions that report exclusively through modals (`Delete`, `Upload`) return nothing rather than an always-nil `error`, so a caller can't be misled into writing a dead error branch.

## Testing strategy

Two suites, split by what they can actually establish.

**Unit tests** cover the pure functions only — planners, formatters, parsers, comparators — and need no network. This is a deliberate consequence of the architecture: the interesting decisions are pushed into pure helpers (`planSync`, `deleteConfirmText`, `filterSortObjects`, `ParseRestoreStatus`, `ParseAWSProfiles`) precisely so they can be tested without a server or a terminal. The UI layer has no seam and is not unit-tested.

**Integration tests** (`pkg/model/integration_test.go`, build tag `integration`) run against a live S3-compatible endpoint. They exist because the most important properties of this package are unobservable without one:

- a version restore *adds* a version rather than rewinding the history
- a `DeleteVersion` leaves no delete marker, while an ordinary delete does
- `ListObjectVersions` is prefix-based, so a sibling `x.bak` must be filtered out of `x`'s history
- a storage-class change preserves metadata (`MetadataDirective` stays at COPY)
- `DeleteKey` refuses a prefix-like key **and removes nothing**
- the session token actually reaches the wire — a bogus one is rejected while the same credentials without one succeed, which is the only way to verify that fix short of real STS

They skip entirely without `S3DUCK_TEST_ENDPOINT`, so a tagged run on a machine with no server is a no-op rather than a failure. `make test-integration` brings up a throwaway MinIO in Docker and tears it down again.

Because the tag hides them from `go build ./...`, CI runs `go vet -tags integration ./...` in the fast job as well, so they cannot silently stop compiling between integration runs.

CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)) is three jobs: lint+unit, MinIO integration, and a cross-build matrix over every platform the Makefile ships. `make check` runs the lint/unit half locally.

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
| `Controller.mu` | `objs` (the displayed object map), `selectedByScope` (multi-select state), `filter`, and `sortBy`/`sortDesc`. All are read on the UI goroutine (input handlers, list callbacks) and written by background refresh/download/upload goroutines. |
| `Controller.refreshMu` | Serialises `updateList()` so overlapping refreshes can't race on the object map or stack redundant network calls. |
| `downloadSummary` per-download `sumMu` | All per-download mutable state inside the download goroutine: the summary counters, `activeProgress` map, `completedBytes`, throttle timestamp. |

### UI update rule

Any code that modifies a tview widget **must** run on the UI goroutine. Background goroutines use `App.QueueUpdateDraw(func() { ... })`. Direct widget calls from a goroutine (without QueueUpdateDraw) are a data race and can corrupt the display.

The mirror-image rule bites more often: **`c.error` and `c.success` are themselves built on `QueueUpdateDraw`, so they must NOT be called inline from the UI goroutine** — from a button handler, a form callback, or a list's selected-func. Doing so blocks the event loop waiting on itself and freezes the app (this is exactly the `Upload()`-on-an-empty-directory bug). From UI-goroutine code call them as `go c.error(...)` / `go c.success(...)`; from a background goroutine call them directly. `c.chooseDir` is the same trap from the other side: it invokes its `onChosen` callback *on* the UI goroutine, so callers must update widgets directly there rather than wrapping them in `QueueUpdateDraw`.

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
- **`renderList()`** — pure re-render from the in-memory `objs` map: applies the active filter and the active sort (`filterSortObjects`, see *Listing order* below), then rebuilds the list widget inside a single `QueueUpdateDraw`. No network. Cheap enough to call on every keystroke.

Selection toggles (`ToggleSelectCurrent`, `SelectAllVisible`, `ClearSelection`), the live filter and the sort keys call `renderList()` directly, avoiding a redundant `List` round-trip on every Space / `/` / `s` press. `Refresh` (`r`/F5) is the one binding that deliberately goes back to the network via `updateList()`. All cursor/list reads now happen **inside** the `QueueUpdateDraw` closure on the UI goroutine (this also removed the earlier off-goroutine read of list state).

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

## Listing columns

The browser shows size, modified date and storage class next to the name. It stays a `tview.List` — whose rows are a single string — so the columns are produced by padding rather than by `tview.Table`: switching widgets would mean rewriting every cursor, selection and reveal call site that currently speaks List, for a purely cosmetic gain.

Two measurement rules make it line up. Widths are **display cells, not bytes**: the icons are double-width emoji (📄 is four bytes and two cells) and the names carry colour tags, so padding goes through `tview.TaggedStringWidth` and truncation through `runewidth`. And the name is truncated *before* it is coloured, since a tag would otherwise be counted as visible text.

`planColumns(width)` decides what fits, taking columns in priority order size → date → class and **stopping at the first that doesn't fit** rather than skipping it. That `break` (not `continue`) is what makes the layout monotonic: with a skip, dropping the date would free enough room for the lower-priority class to reappear, so narrowing the pane could *add* a column. Each column is only taken if a readable name (`colMinName`) still remains, so a narrow dual-pane degrades to names alone.

The header line is a `TextView` above each pane's list. Its indent is derived at render time from the difference between the list's and the header's inner rects, rather than hardcoding "one cell for the border" — so it stays aligned whatever decoration either widget grows later. `watchResize` (an `App.SetAfterDrawFunc` hook) re-renders the active pane when its width changes, which covers terminal resizes and dual-pane toggles; it converges after one extra render because that render records the new width.

## Listing order

`filterSortObjects` takes the sort key (`name` / `size` / `date`) and direction alongside the filter. Folders and buckets are always emitted before files whatever the key is, so the listing keeps its navigable shape; the key only orders within each group. Ties (equal sizes, equal timestamps, and every folder pair — folders carry no `Size`/`LastModified`) fall back to **ascending name** in both directions, via the `tie` return of `lessBySortKey`, so reversing the direction can't scramble an otherwise-equal group. The state (`sortBy`/`sortDesc`, guarded by `mu`) is shared by both panes: unlike a filter, an ordering preference is not location-specific. `s` cycles the key, `S` reverses; both call `renderList` only — no network round-trip. `r`/F5 is the explicit `updateList` refresh.

## Delete

`Delete` resolves its targets the same way copy/move/rename do — the marked set, falling back to the highlighted item — so the destructive operation is no longer the odd one out that ignored multi-select. Buckets are still single-target (they can't be marked; selection scope only exists inside a bucket).

Because a folder target is a prefix delete, the confirmation must state its cost: a scan goroutine runs `ListObjects` per folder/bucket target, filling in `objects`/`bytes`, behind a "Calculating…" modal. `deleteConfirmText` (pure) renders the totals and names up to 6 targets. A target whose scan failed is counted separately and disclosed — the totals are never silently short. `runDelete` then deletes sequentially behind a progress modal, collecting failures instead of aborting, and drops each deleted name from the selection set as it goes. A bucket target is emptied first (`EmptyBucket`) — S3 only deletes empty buckets, and the confirm has already promised the objects go; on a *versioned* bucket old versions survive and the bucket delete still fails (see the limitations table).

## Credentials

`model.Config.SessionToken` feeds `credentials.NewStaticCredentialsProvider`'s third argument, which was previously hard-coded to `""` — that omission made every form of temporary credential (assume-role, SSO, MFA) unusable regardless of what the user pasted into the profile form.

`internal/config/awsshared.go` reads `~/.aws/credentials` and `~/.aws/config` (honoring `AWS_SHARED_CREDENTIALS_FILE` / `AWS_CONFIG_FILE`) with a small pure INI parser rather than the SDK's shared-config loader, so the profile list, the merge precedence (credentials file wins) and the `[profile x]` vs `[x]` section naming are all directly testable. Profiles that delegate rather than carry keys (`sso_session`, `role_arn`, `credential_process`) are **listed with the reason they can't be imported** instead of being dropped, so the import dialog explains itself. `Ctrl+I` on the profiles screen imports the selected one as `aws-<name>`, de-duplicated by `uniqueProfileName`.

## Sync

`Sync` (Ctrl+E) mirrors a local directory against the current bucket+prefix in either direction. It is the only operation that can both overwrite and delete, so the flow is always **scan → dry-run plan → explicit Apply**; there is no way to run it unreviewed.

- **Spec.** A run is described by one `syncSpec` (direction + local dir + source and destination bucket/prefix). Introducing it replaced a growing parameter list: the local↔remote flows only ever needed one bucket, but a remote↔remote run needs two, and threading both through every function is where mistakes would have lived. `collectSides` is the single place that knows which side comes from where.
- **Collect.** `model.WalkLocal` walks the local root into `SyncEntry{Rel, Size, Mod}` (regular files only — a directory has no counterpart to compare against); `model.ListRemoteEntries` does the same from a paginated `ListObjects`, skipping folder-marker keys. Both sides key on a slash-separated path relative to their root, so they compare directly.
- **Plan.** `planSync(src, dst, del)` is pure. A file transfers when it is missing at the destination, when the sizes differ, or when the sizes match but the source is newer by more than `syncModTolerance` (2s, absorbing clock skew and coarse filesystem/S3 timestamp granularity). A zero timestamp on either side degrades to a size-only comparison rather than forcing a transfer. Deletes are emitted **only** when the flag is set. Output is ordered creates → updates → deletes, each group by path, so the plan is deterministic and reviewable.
- **Apply.** `runSync` reuses the transfer-job machinery (`addJob`/`jobSem`/`finalizeJob`), so a sync is cancellable and backgroundable like any other transfer and honors the bandwidth limiter. Operations run through a **4-worker pool** (`syncWorkerCount`, never more workers than work), in two phases from the pure `splitSyncPhases`: every write completes before any delete starts, so a run that is cancelled partway leaves the destination having *gained* the new files but not yet *lost* the old ones — the safer intermediate state. Within a phase the operations are independent by construction (a path is either present at the source or not), so they interleave freely. Shared counters and the throttled redraw sit behind one mutex, and per-file byte counts are tracked in an `inFlight` map keyed by plan index so the displayed total stays correct with several transfers in progress. Per-op work goes through `model.UploadFile` (upload to an explicit key — `Model.Upload` derives keys from a directory walk and can't target one), `model.DownloadTarget`, `model.DeleteKey`, or `os.Remove`. Failures are collected, not fatal: one unreadable file doesn't strand the rest.
- **Remote → remote.** The planner never knew which side was local — it diffs two `[]SyncEntry` — so the third direction needed only a second `ListRemoteEntries` call and one branch in `applySyncOp`, which issues a server-side `CopyObject` instead of an upload. Two consequences worth knowing: a server-side copy moves no bytes through this process, so there is **no byte progress** for a remote→remote run (the op counter is the only thing that advances); and it inherits the same-endpoint constraint documented below, since both sides go through one client.
- **Safety.** Deletes are skipped entirely when any write failed — a partly-written destination is not the mirror the reviewed plan assumed, so removals are no longer covered by the user's approval. A remote→remote run between overlapping prefixes of the same bucket is rejected up front (`prefixesOverlap`): the source listing would include the destination, so src-inside-dst with delete-extraneous would delete the physical source objects, and dst-inside-src re-nests one level per run (`mirror/mirror/…`). `DeleteKey` refuses any key ending in `/`, so a sync delete can never degrade into `Delete`'s recursive prefix removal. And `DownloadTarget` refuses to clobber an existing file (correct for the browser's download flow), so the download path removes the stale copy first — only for files the reviewed plan already marked as updates.

## Object metadata, tags and storage class

S3 has no metadata-update API, so `PutObjectMeta` rewrites an object by copying it **onto itself** with `MetadataDirective=REPLACE`. The storage class is passed through explicitly: a replace-copy that omits it silently demotes the object to STANDARD. `SetStorageClass` is the mirror image — it leaves `MetadataDirective` at its COPY default so the existing metadata rides along untouched (verified against MinIO: changing the class preserves Content-Type).

Tags live behind a separate API pair, so the editor fetches both and **writes back only the halves that changed** — a metadata save is a full object copy, far too expensive to do for a tags-only edit. Tagging is optional on S3-compatible backends, so a failing `GetObjectTagging` degrades to "no tags" with a note in the form rather than blocking the editor. Tag limits (10 tags, 128/256 char key/value) are checked client-side so the user gets a precise message instead of a generic `InvalidTag`.

`ParseRestoreStatus` reads the `x-amz-restore` header. It cannot split on commas: the value is quoted and an RFC 1123 expiry date contains commas of its own (`expiry-date="Fri, 21 Dec 2012 00:00:00 GMT"`), so each directive is read up to its closing quote. `IsArchived` deliberately excludes `GLACIER_IR` — instant retrieval needs no restore despite the name.

Both forms are read back with `GetFormItemByLabel` and the exported `view.Field*` constants rather than by index. They mix `TextView` rows with inputs, and a positional read returns the wrong widget the moment a row is inserted — the trap the profile form already sprung when the session-token field was added.

## Object versioning

`ListVersions` gives one object's history. `ListObjectVersions` is **prefix**-based, so it also returns every key that merely starts with this one; the exact-key filter is what turns it into a per-object history (verified: a sibling `doc.txt.bak` does not leak into `doc.txt`'s history). Pagination is followed to the end, since a heavily-rewritten object easily exceeds a page. Delete markers are listed alongside real versions rather than filtered out — the marker *is* what makes an object look deleted, and removing it is how you undo that.

`RestoreVersion` copies the chosen version to the top of the history rather than rewinding, which is the only non-destructive way to go back in a versioned bucket; the e2e run confirms the history grows from 3 entries to 4. `DeleteVersion` is the one genuinely destructive action here — it removes the data outright and leaves no delete marker — so it is behind an explicit confirmation that says so. Downloads are saved as `name.<8-char-version>.ext` so several versions can coexist in one directory.

## Pane comparison

`=` diffs the two dual-pane locations and shows the result read-only. It is the same `planSync` the sync preview uses — deliberately, so the two can never disagree about what counts as a difference — run with `del=true` so entries present only on the right are reported as well; a comparison must be symmetric even though the planner is directional. `comparePlanText` re-words the three kinds (`left-only` / `differs` / `right-only`) because a comparison has no notion of creating or deleting, and states plainly that only names and sizes were compared. `showPlan` is shared with the sync preview and simply omits the Apply button when there is nothing to apply.

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
  "session_token": "plaintext, optional — temporary credentials only",
  "ignore_ssl":   false,
  "download_dir": "~/Downloads/s3"
}
```

`secret_key` and `session_token` are stored in plaintext. The file has `0600` permissions (owner-readable only), but there is no OS-keychain integration. Future work: `github.com/zalando/go-keyring`.

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

## Known limitations

| Area | Description |
|---|---|
| **Navigation race** | `c.currentBucket` and `c.restoreNext` are written from the goroutines spawned by `Down()`/`jumpTo`/`navigateTo` without the `mu` mutex. In single-pane use navigation is serial, so races are unlikely — but in dual-pane, a Tab during a slow bucket-open swaps the live fields mid-write and the navigation lands in the other pane. A proper fix passes navigation state through the refresh path instead of mutating it in place. Transfers are no longer affected: Download and Upload capture their bucket/path at entry. |
| ~~No retries on upload~~ | **Fixed.** Both upload paths share `newUploader`, which installs the SDK's standard retryer (`uploadMaxAttempts` = 3). Retries are safe because every part is re-read from the file rather than from a consumed buffer. |
| **Sequential per-file overwrite** | Overwrite decisions block Phase 1 completion. For a large selection with many conflicts, the user must click through each dialog before any download starts. |
| **Remote→remote sync has no byte progress** | The transfer happens inside S3, so the client sees only completed operations. The op counter advances; the byte gauge does not. |
| **Cross-bucket copy/move is same-endpoint only** | `CopyKeys` / `MoveKeys` take separate source/destination buckets and issue a server-side `CopyObject`, so both buckets must be reachable through the one configured endpoint. This covers any single S3-compatible endpoint (MinIO/Ceph) and same-region AWS. Copying between AWS buckets in *different regions* is not handled (the client stays pinned to the source region). |
| **No download resume** | Interrupted downloads restart from byte 0; partial files are deleted on cancel. |
| **Sync compares size + mtime, not content** | `planSync` never hashes. A file edited in place to exactly the same size, with its mtime preserved, is not detected as changed. Comparing ETags would only help for single-part uploads (a multipart ETag is not the MD5 of the object) and would need a matching local chunking scheme. |
| **An upload sync straight after a download sync re-uploads** | A downloaded file's local mtime is its download time, which is newer than the object's `LastModified`. Reversing the direction therefore sees "source is newer" for every file and re-sends them once (sizes are equal, so nothing is corrupted, and the second reversal is a no-op). This matches `aws s3 sync` semantics; the dry-run plan shows it before anything moves. |
| ~~Sync applies one operation at a time~~ | **Fixed.** `runSync` now uses a 4-worker pool with a writes-then-deletes barrier (see *Sync* above). |
| **Sync direction is one-way** | Each run treats one side as the source of truth. There is no bidirectional merge and no conflict resolution — the newer-wins rule only ever applies in the chosen direction. |
| **Session tokens expire, silently** | An imported temporary credential is stored as-is. When it expires, calls start failing with an auth error; s3duck neither refreshes it nor warns beforehand. Re-import after `aws sso login` / a fresh assume-role. |
| **Versioned buckets can't be emptied from the TUI** | `model.Delete` sends no `VersionId`, so folder deletes write delete markers only; `EmptyBucket` clears current objects but old versions survive, and `DeleteBucket` then fails with BucketNotEmpty. A version-aware purge is on the roadmap. |
| **Whitespace keys** | Every secondary-text reader trims the key, so `"dir/report "` resolves to `"dir/report"` in lookups (wrong object if both exist, silent no-op if only the padded one does). |
| **Versioning needs a versioned bucket** | On an unversioned bucket S3 reports a single `null` version; the browser shows exactly that rather than hiding the feature. Enabling versioning is a bucket-level operation s3duck does not perform. |
| **Restore is asynchronous** | `RestoreObject` only *requests* a restore; completion takes minutes to hours and s3duck does not poll. Re-open the object (`c` or Ctrl+L) to see the current state. |
| **Metadata edits rewrite the object** | A metadata save is a server-side copy onto the same key. On a versioned bucket that creates a new version; the ETag may also change for multipart objects. |
| **The inactive pane keeps its previous column layout** | `renderList` renders the active pane, so right after `Ctrl+O` the other pane still shows columns sized for the previous width. It self-heals the moment you `Tab` to it (`swapAndFocus` re-fetches and re-renders). Fixing it properly needs a render path that can target a pane other than the active one. |
| **Summary top-10 cap** | `buildSummary` silently truncates the groups table to the top 10 by size. Groups ranked 11+ are not shown and not counted in any "overflow" indicator. |
