# S3Duck-TUI — Design Notes

Internal reference covering concurrency model, key design decisions, and known constraints. For user-facing docs see [README.md](README.md).

---

## Architecture

Layered by **testability seam** rather than by MVC letter: what a package depends on decides what can be asserted about it.

```
cmd/s3duck-tui/main.go
        │
        ▼
internal/controller      ← app state, keybindings, screen wiring (the remaining glue)
   ├── internal/session   ← Pane: location, listing, selection, history. No tview.
   ├── internal/ops       ← Download/Upload/Copy/Sync as values over narrow S3 ports. No tview.
   ├── internal/jobs      ← transfer queue, status badge, completion notices. No tview.
   ├── internal/model     ← S3 SDK wrapper, all network I/O
   ├── internal/config    ← JSON profile persistence
   ├── internal/utils     ← small helpers (path split, rand string, clipboard)
   └── internal/ui/
        ├── shell        ← overlay stack + the non-blocking UI update queue
        ├── widgets      ← pure tview widget construction, no business logic
        ├── flows        ← the one Runner that drives any ops.Operation
        └── screens      ← self-contained overlay screens (transfers)
```

| Layer | Depends on | Testable by |
|---|---|---|
| `session` | `model`'s plain types only (`*model.Object`) | plain unit tests |
| `jobs` | nothing | plain unit tests |
| `ops` | narrow S3 ports, a `Reporter` (and `model`'s types) | fake store + recording reporter |
| `ui/*` | tview | by hand, plus unit tests for the pure render helpers |

DESIGN.md used to argue that `internal/controller` had to be large "because the modal flows are deeply coupled to tview's callback/page model; splitting them would mean passing around opaque page/modal handles". The premise was right and the conclusion was not — see [REFACTOR.md](REFACTOR.md). Handles had to be passed because an operation asked the user questions by *calling into* tview; once an operation emits an `ops.Question` and blocks on the answer instead, it needs no handle at all, and one runner renders every question for every operation.

What is left in `internal/controller` is wiring, and it is still split by feature: `controller.go` (browser screen, navigation, listing, copy/move/rename/delete, search) plus `profiles.go` (the profiles screen), `sync.go`, `objectmeta.go`, `versions.go`, `keymap.go`, `listing.go`, `opresult.go`, `localpane.go`, `usage.go`, `preview.go`, `edit.go`, `match.go`, `guard.go`, `trash.go`, `verify.go`, `notify.go`, `audit.go`, `options.go`, `crossprofile.go`, `duplicates.go`, `overwrite.go` and `columns.go`. `internal/model` mirrors the shape: `model.go` plus `sync.go`, `object.go`, `versions.go`, `copy.go`, `content.go`, `conflicts.go`, `write.go`, `checksum.go` and `local.go`.

The tree is `gofmt`-, `go vet`- and `staticcheck`-clean; keep it that way. Controller actions that report exclusively through modals (`Delete`, `Upload`) return nothing rather than an always-nil `error`, so a caller can't be misled into writing a dead error branch.

## Testing strategy

Two suites, split by what they can actually establish.

**Unit tests** cover the pure functions — planners, formatters, parsers, comparators (`planSync`, `deleteConfirmText`, `filterSortObjects`, `ParseRestoreStatus`, `ParseAWSProfiles`) — and, since the `ops` refactor, the *orchestration* as well.

That second half is new and is the point of `internal/ops`. An operation depends only on narrow interfaces declared by the consumer — `Fetcher`, `Pusher` (whose one method is `UploadTargets`), `Checker`, `FS` and `Gate` in `internal/ops/ports.go`, `Mover` beside its only user in `internal/ops/copy.go` — and `*model.Model` satisfies them directly. A fake therefore implements a handful of methods rather than sixty: the shared doubles — fetcher, checker, in-memory filesystem, gate and scripted reporter — are one ~240-line file (`internal/ops/fake_test.go`), with the mover and pusher fakes beside the tests that use them, and the decisions that matter run against it in milliseconds: a download's two-phase resolve, its overwrite answers, the four-worker pool, cancellation draining, verification accounting and the retry set; a copy's item-versus-object accounting, that a skipped file is neither done nor recorded as moved, and the skip set it hands the undo; an upload sending exactly its target list; that a cancel mid-run is reported as a cancel rather than as failures; and — the one with the sharpest failure mode — sync's writes-then-deletes barrier, including that a failed write cancels the delete phase and says why. Those are the branches that used to be reachable only by driving the real TUI against a live endpoint, which is to say the ones most likely to be wrong were the least tested.

**Integration tests** (build tag `integration`) run against a live S3-compatible endpoint, in four files: `internal/model/integration_test.go`, `internal/model/integration_new_test.go` and `internal/model/integration_browse_test.go` for the calls the model makes, and `internal/controller/integration_test.go` for the flows the controller composes from them (sync in both directions, the exclude guard, safe delete's trash round trip). They exist because the most important properties of this code are unobservable without a server:

- a version restore *adds* a version rather than rewinding the history
- a `DeleteVersion` leaves no delete marker, while an ordinary delete does
- `ListObjectVersions` is prefix-based, so a sibling `x.bak` must be filtered out of `x`'s history
- a storage-class change preserves metadata (`MetadataDirective` stays at COPY)
- `DeleteKey` refuses a prefix-like key **and removes nothing**
- the session token actually reaches the wire — a bogus one is rejected while the same credentials without one succeed, which is the only way to verify that fix short of real STS
- an uploaded object's stored `Content-Type` is the derived one (and an unrecognisable body keeps the server's default) — the client can only prove what it *sent*
- a requested checksum is really stored, a locally computed one matches it, and a tampered local file is detected — plus the ETag fallback for a single-part object
- a multipart upload made with a checksum algorithm is *not* reported as a mismatch (its stored checksum is a checksum-of-parts, recognisable only from the ETag)
- a multipart copy succeeds under a profile that sets `checksum_algo`, and so does a single-request one
- `UploadTargets` writes exactly the target list it is given — one file and one empty-folder marker, and nothing else from the directory they sit in
- a ranged read returns exactly the requested window, and asking for more than the object holds returns the object rather than an error
- `GetVersionContent` reaches a specific old version, which is what the diff needs
- `ListObjectsStream` reports a cancelled context instead of running to the end. The same test also asserts paging and the early stop, but with 12 keys in one 1000-key page it cannot fail on either; only the cancellation half really checks anything

They skip entirely without `S3DUCK_TEST_ENDPOINT`, so a tagged run on a machine with no server is a no-op rather than a failure. `make test-integration` brings up a throwaway MinIO in Docker — the same pinned `alpine/minio` image, on a tmpfs `/data`, that CI uses — and tears it down again. Individual tests skip themselves when the endpoint lacks the feature (SSE-S3 needs a KMS MinIO does not configure by default, versioning may be unsupported) rather than failing against a legitimate backend.

**Neither suite covers** the wiring of the terminal: a keypress reaching the right action, a modal appearing over the right layer, a dialog's answer reaching the operation behind it. The pieces are unit-tested — the layer stack (`ui/shell`), the report/progress text (`ui/flows`), the transfers panel's row text (`ui/screens`), and in `ui/widgets` the clamp arithmetic plus the hotkey panel itself, rendered onto a tcell simulation screen and driven with real key events (see *Overlay sizing*) — but only the real binary shows them composed.

**The end-to-end suite** (`e2e/`, build tag `e2e`) drives exactly that. `TestMain` builds the binary from the working tree; each test starts it in its own tmux server with its own HOME (config, downloads, `XDG_STATE_HOME`) and its own bucket, types at it with `send-keys`, reads the screen with `capture-pane`, and asserts on what MinIO and the disk hold — never on what the app reported. tmux is the terminal because it is one: capture-pane returns the rendered grid as text, so a test waits for a dialog's own wording rather than for a sleep, and `settle` (two identical captures 60 ms apart) keeps the next key from racing the redraw. A failing test logs its last screen, which is usually the whole story; `remain-on-exit` keeps a crashed binary's panic on that screen. The cursor is located through the details panel's `FullPath` line (single pane) or by narrowing the listing with the filter (dual pane, which has no details panel).

The scenarios are chosen by where a dialog's answer decides what happens to data — the seams the 2026-10 review found its worst bugs in: undo after *Skip existing* (file and folder), the local pane's upload of a highlighted or marked file, a config that fails to parse, a folder delete beside a same-stem sibling, and a delete-extraneous sync on a safe-delete profile with an exclude. Each data scenario was run against a binary built from the commit before its fix (`S3DUCK_E2E_BINARY`) and fails there with a statement about the bucket ("`src/a.txt` holds DST-A, want SRC-A"), not a timeout.

`make e2e` runs it hermetically (`e2e/run.sh`): MinIO on a tmpfs and a runner image (Go pinned by digest, tmux, the module cache) on a private compose network with no published ports, the tree mounted read-only, everything removed afterwards pass or fail. Run from the host against an endpoint you already have, it needs Go and tmux; without `S3DUCK_TEST_ENDPOINT` it skips.

Because the tags hide them from `go build ./...`, CI runs `go vet -tags integration ./...` and `go vet -tags e2e ./...` in the fast job as well, so neither suite can silently stop compiling between the slower runs.

CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)) is four jobs: lint+unit, MinIO integration, end-to-end (`make e2e`), and a cross-build matrix over every platform the Makefile ships. `make check` runs the lint/unit half locally — gofmt, the three `go vet` passes, the build and the race-enabled tests — but not staticcheck, which has its own target (`make staticcheck`) and needs the `staticcheck` binary installed.

---

## Concurrency model

### Goroutine categories

| Category | What runs there | Notes |
|---|---|---|
| **UI goroutine** | tview event loop, all input handlers, `InputField` / `List` changed and selected callbacks (`SetChangedFunc`, `AddItem`) | The only goroutine allowed to mutate tview widgets directly. A `TextView`'s changed handler is the exception: tview runs it with `go t.changed()`, on a goroutine of its own, which is why the two the app installs (`ui/widgets`) only call `App.Draw`, which tview's documentation for that handler permits |
| **Network goroutines** | `ListBuckets`, `List`, `GetBucketLocation`, upload, download, delete, copy/move, summary | Spawned by the controller; results returned to the UI goroutine through `shell.Do` (see *UI update rule*) |
| **Shell pump** | `shell.pump()` — forwards batched UI closures to tview | One for the app's lifetime, and never the UI goroutine, which is what lets `QueueUpdateDraw`'s wait complete |
| **Refresh goroutine** | `updateList()` — fetches the object map page by page into the pane it started for, then queues a redraw if that pane is still active | Serialised by `refreshMu`; at most one active at a time, and cancellable through `listingState` |
| **Status ticker** | `runStatusTicker()` — recomputes the header's transfer badge / notice | One for the app's lifetime; queues a redraw *only* when the text changed, so an idle app costs a string compare |

### Mutexes

Most of the state that used to sit behind one controller-wide lock now belongs
to the object that owns it, so the table reads as an inventory of owners rather
than of fields.

It is not a proof that everything else is confined to one goroutine, because it
is not. Background goroutines still read a few controller fields without a
lock: `c.active` and `c.panes` (through `c.pane()`), the `c.view.*` pane-widget
pointers that `swapPane` repoints, and `c.model`, which `Duck` replaces. All of
them are written only on the UI goroutine and read a pointer (or an index) at a
time, so a reader sees the old value or the new one — but that is still a race
in the Go memory model's sense, and it used to be a correctness bug whenever
the value read was the stale one: a listing, a move's bookkeeping or a copy's
source landing in the pane the user had just Tabbed to. Those were fixed by
capturing the pane (and the client) on the UI goroutine before spawning, not by
locking (see *Dual-pane*). The model has the mirror-image gap: `RefreshClient`
replaces its client and region from a navigation goroutine while a transfer
may be using them (on the roadmap). Three short-lived worker pools also lock
locally and are left out of the table: `copyParts` in
`internal/model/copy.go`, `conflictsByHead` in `internal/model/conflicts.go`,
and the cross-profile copy's progress counters in
`internal/controller/crossprofile.go`.

| Mutex | Protects |
|---|---|
| `session.Pane.mu` | One pane's whole world except its history: location (bucket, prefix, local dir), the bucket list, the object map, the per-scope selection sets, filter, `partial`, the pending cursor target and the bucket-row position. Read on the UI goroutine, written by navigation and refresh goroutines. `Enter` writes bucket, prefix and cursor target under one acquisition, so no reader sees the new bucket paired with the old prefix. The history (`session.History`) has no lock: it is touched only on the UI goroutine. |
| `Controller.mu` | `sortBy`/`sortDesc` — the only listing state left on the controller, since ordering is a preference rather than a property of a location. |
| `Controller.refreshMu` | Serialises `updateList()`, so overlapping refreshes can't stack network calls or race on a pane's object map. |
| `Controller.activityMu` | The in-session activity ring, written from every transfer goroutine. |
| `Controller.undoMu` | The one-step undo, written from transfer goroutines and read by `Undo` on the UI goroutine. |
| `listingState.mu` | The in-flight listing's cancel function and its generation, so `Esc` can cancel a listing started by another goroutine — and so a listing that finishes late cannot unregister the one that replaced it. |
| `shell.Shell.mu` | The overlay stack, the pending-closure list and the current toast. |
| `jobs.Queue.mu` / `jobs.Job.mu` | The job list and the header notice; each job's own counters and status, which progress goroutines write while the panel ticker reads. |
| `ops.ledger.mu` | One operation's result as parallel workers report into it. `syncProgress.mu` and the download's `live`-map lock do the same for the in-flight byte counts. |
| `flows.uiReporter.mu` | The progress redraw throttle. |
| `model.rateLimiter.mu` | The token bucket shared by every transfer worker of one `*model.Model` — the open profile's; a cross-profile copy's destination model has its own. |

### UI update rule

Any code that modifies a tview widget **must** run on the UI goroutine. Direct widget calls from a background goroutine are a data race and can corrupt the display. Every off-UI update goes through `shell.Do`; nothing outside `ui/shell` calls `App.QueueUpdateDraw` any more, for the reason below.

~~The mirror-image rule bites more often~~ — **retired in the `ui/shell` refactor.** It used to read: `c.error` and `c.success` are built on `QueueUpdateDraw`, which blocks until the event loop runs the closure, so calling them inline from the UI goroutine froze the app; callers had to write `go c.error(...)` from a button handler and a bare `c.error(...)` from a worker. That cost two shipped freezes (`Upload()` on an empty directory, and the filter box's `SetChangedFunc` calling `renderList` inline).

Nothing outside `ui/shell`'s pump calls `QueueUpdateDraw` any more (the only other route into tview from a background goroutine is `App.Draw`, from the two `TextView` changed handlers above). Every off-UI update goes through **`shell.Do`**, which appends to a pending list and returns — never blocking, from any goroutine including the UI one — while a pump goroutine (never the UI goroutine, so `QueueUpdate`'s wait can always complete) forwards batches to tview. One FIFO for everything preserves the ordering the blocking calls used to get for free, and batching coalesces a burst of progress updates into one draw. `c.error` / `c.success` are now plain calls everywhere.

`c.chooseDir` is still the trap from the other side: it invokes its `onChosen` callback *on* the UI goroutine, so callers update widgets directly there rather than through `shell.Do`.

**Overlays.** There are no hand-written page names any more. `shell.Push` returns a `*shell.Layer` whose page name (`layer-N`) is allocated by the shell and never reused, and `Layer.Close` pops exactly that layer and refocuses whatever is underneath. This replaced 226 hand-written `Pages.AddPage`/`RemovePage` calls over 21 magic strings — `"progress"` alone appeared 72 times and `"modal"` 61, and because `AddPage` *replaces* a page of the same name, any two flows sharing one could destroy each other's overlay (which is what put transient toasts on their own `"modal-msg"` page, and what the comment about `showSummary` focusing a detached modal was guarding against). A flow that drills down — the usage browser, the duplicate finder, the summary graph — passes its layer to the next level, which closes it after pushing its own, so "replace" is now something the code says rather than something a shared string does.

Toasts keep replace-the-previous semantics through `shell.Toast`, which closes the outgoing toast and leaves every other layer alone; `Layer.Close` is idempotent, so a flow that can finish down two paths (a Done button and an Esc capture) needs no coordination. Two more captured invariants: transfer flows capture `c.model` at entry alongside bucket/path (a profile switch swaps `c.model`, and a queued transfer reading it lazily would run against the wrong account — `CheckProfile` now verifies with a throwaway client for the same reason), and `Duck` clears the clipboard and the one-step undo (both carry bucket/key names that would otherwise be replayed against a same-named bucket on the new endpoint). Clearing is not enough on its own for the undo — a backgrounded move can finish after the switch and record an entry anyway — so each entry also records the client its move ran through, and `Undo` refuses it in any other profile ("Nothing to undo in this profile").

---

## Download pipeline

The pipeline itself is `ops.Download` (`internal/ops/download.go`): it knows
nothing about a terminal, depends only on the `Fetcher` / `Checker` / `FS` /
`Gate` ports, and is unit-tested against a fake store. The controller only
resolves the object set (`Download`) and hands it over (`runDownload`);
`flows.Runner` renders the confirmation, the progress modal and the summary.

### Two-phase design

`Download.Run` splits the work into two sequential phases:

Before either phase, and before `ops.Download` exists at all, the controller's `Download` runs folder **resolution** (the recursive `ListObjects` behind a folder selection) in its own goroutine behind a cancellable "Resolving objects..." modal; inline on the UI goroutine it froze the whole TUI for the length of the listing and swallowed its errors. Local paths are derived through `model.SafeLocalPath`, which rejects any key whose cleaned path escapes the destination directory: S3 keys may legally contain `..` segments, and an unchecked `filepath.Join` is the zip-slip pattern (a hostile shared bucket could plant `docs/../../../.bashrc`). Containment is decided with `filepath.Rel` rather than a string-prefix test, which rejected every key when the destination was the working directory (`.`) or a filesystem root.

**Phase 1 — `plan`: overwrite resolution (sequential)**

Walks `Targets` one at a time, handling:
- Directory marker keys (`key/`) — created immediately through the `FS` port; not queued for transfer
- File conflict detection — `FS.Exists` → `rep.Ask(ops.Question{Kind: QOverwrite})` → Overwrite / Skip / Overwrite All / Skip All / Cancel
- Building `[]resolved` — the approved list, each carrying the decision made about it

Overwrite questions are inherently sequential because the reporter blocks the operation's goroutine until the user answers; asking from four workers at once would stack modals. Note what the question *is* here: a value the operation emits, not a call into tview. That inversion is why this package needs no widget handle and can be driven by a scripted reporter in a test.

Phase 1 runs **outside** the transfer gate. It is user-interaction time, not bandwidth, and holding one of the two transfer slots through a pile of overwrite dialogs would stall every other transfer in the app. When nothing survives it — everything skipped, or only directory markers — `Run` returns without queuing for a slot at all, so a cancel while queued cannot report a download that had nothing left to do as cancelled.

An Overwrite decision only sets a flag on the item — **nothing is deleted at prompt time**. The transfer downloads into a temp file beside the target, `<name>.<random>.s3duck-part` (`os.CreateTemp`, then mode 0644), and renames it onto the target only after the body is fully on disk. The name is unique per transfer because two keys can land on one local path (`a//b` and `a/b`, or two names differing only in case on a case-insensitive filesystem); with one fixed temp name per target, two workers wrote into the same file and the interleaving was reported as a success. Renaming last means a cancel while the job is still queued (or a failed transfer) leaves every existing file intact; the ledger counts an overwrite on completion, when the file was actually replaced. (Phase 1 used to `os.Remove` at decision time: an "Overwrite All" over 200 files followed by a cancel deleted all 200 and downloaded none.)

**Phase 2 — `transfer`: parallel download (semaphore pool)**

```
sem = make(chan struct{}, DownloadWorkers)   // 4 concurrent workers

for each r in todo:
    if ctx.Err() != nil: led.cancel(); break   // checked BEFORE the select
    select { <-ctx.Done → cancel; sem← → acquire slot }
    wg.Add(1)
    go one(r):
        defer wg.Done(); defer <-sem
        live[key] = 0                       // under mu, for the gauge
        Fetcher.DownloadTarget(ctx, ...)
        if Verify: compare the file against the object's checksum
        led.ok(n, overwrote); report()
wg.Wait()
```

The context check before the `select` is not redundant: with a free slot and a dead context both ready, `select` picks at random, so a cancelled run could keep handing out work for several more objects.

Cancellation flow:
1. User clicks Cancel → the Runner's `SetDoneFunc` fires → `cancel()` → ctx cancelled
2. Dispatch stops at the check above and the loop breaks; the modal says "Canceling, please wait..."
3. Running workers see `ctx.Err() != nil` after `DownloadTarget` returns; `DownloadTarget` itself cancels the S3 download and removes its temp file (the target file — old content included — is never touched by a canceled transfer)
4. `wg.Wait()` drains the workers, so the ledger the Runner reads is complete rather than racing the stragglers

A worker's last step, when the profile asks for it, is the **post-transfer
verify**: the file on disk is re-read and compared against the object's
checksum (or its single-part ETag). A mismatch is counted as a *failure*, not
a success — a silently corrupt local file is the worst of the three possible
outcomes — while an object that cannot be compared at all (a multipart object
whose only checksum is a checksum-of-parts, or none; an SSE-KMS object whose
ETag is not an MD5) is counted separately and disclosed as "not verifiable"
rather than being quietly called verified. The verify reads the file at its
target name, after the rename: a mismatch is reported, but with Overwrite
chosen the previous file has already been replaced by the corrupt one (see
*Known limitations*). The setting is read once, on the UI
goroutine, and carried into the transfer: a profile switch mid-download must
not change the rules a running transfer is playing by.

Whatever failed is kept with its size, so `finish` can stamp the result with a
retry operation covering exactly those objects — a 5 000-object download that
lost twelve to a flaky link costs twelve transfers to finish, not another full
run.

### Progress display (parallel)

`transfer`'s `report` closure is called from every worker concurrently. It:
1. Takes the pool's `mu` to sum `doneBytes` plus the `live` map (bytes moved by transfers that have not finished yet), so the gauge stays honest while four run at once
2. **Releases it before** `rep.Progress(p)` — the reporter forwards to the UI, and holding a lock across that would invite a deadlock with anything the UI goroutine wants
3. Is throttled by the reporter (`drawEvery`, 100 ms), not by the operation: `ops.Reporter` documents throttling as the implementation's job, so no operation has to re-invent it

Displayed format:
```
Download
12 / 47 done
1.2 GiB / 3.4 GiB (35.2%)
4.1 MiB/s · ETA 8:21

file1.txt  report.zip
```

---

## Listing: rendering vs fetching

`updateList()` and `renderList()` are split so the list can re-render without hitting the network:

- **`updateList()`** — takes `refreshMu`, captures the active pane, calls `makeObjectMap(ctx, pane, …)` (network `ListStream` / `ListBuckets`, page by page with a live count and a cancellable context — see *Streamed, cancellable listings*), stores the result in *that* pane, and renders through `renderIfActive`, which draws only if the pane is still the one on screen. Use it whenever the object set changes (navigation, delete, upload, rename, copy/move). For a local pane it reads one directory instead (`makeLocalObjectMap(pane)`), which needs no pagination and no cancellation. Storing into whatever pane was active when the fetch *returned* is what used to put one pane's objects in the other after a Tab mid-listing, where a delete then resolved them against the wrong bucket; a pane that lost focus that way is re-fetched by `swapAndFocus` when it regains it.
- **`renderList()`** — pure re-render from the in-memory `objs` map: applies the active filter and the active sort (`filterSortObjects`, see *Listing order* below), then rebuilds the list widget inside a single `shell.Do` closure. No network. Cheap enough to call on every keystroke.

Selection toggles (`ToggleSelectCurrent`, `SelectAllVisible`, `ClearSelection`), the live filter and the sort keys call `renderList()` directly, avoiding a redundant `List` round-trip on every Space / `/` / `s` press. `Refresh` (`r`/F5) is the one binding that deliberately goes back to the network via `updateList()`. All cursor/list reads now happen **inside** that closure, on the UI goroutine (this also removed the earlier off-goroutine read of list state).

## In-listing filter

A persistent one-line `InputField` sits under the list (`view.Filter`). `/` focuses it; typing stores the query on the active pane (`Pane.SetFilter`, under the pane's lock) through `SetChangedFunc` and re-renders live; Enter keeps the filter and returns focus to the list; Esc clears it and re-renders (clearing alone suppresses the change handler's render, so for a while Esc emptied the box and left the list filtered). Matching goes through the shared matcher (see *Query syntax* below): plain text is a case-insensitive substring of the object's short display name, `*.log` is a glob, `re:` is a regex, and the mode in force is named in the pane title. The filter is reset on every navigation (`Down` / `Up` / `Profiles`) so it never leaks across folders. `filterSuppress` stops the change handler from re-rendering when the field is cleared programmatically (`SetText` fires `SetChangedFunc` inline on the UI goroutine).

## Recursive search

Ctrl+F prompts for a query, then walks the current prefix recursively (`ListObjectsStream`, no delimiter) on a background goroutine behind a cancellable search modal that reports "scanned N keys, M match(es)". Matching is the shared matcher (text / glob / `re:`), applied **page by page** rather than to one accumulated slice: an all-buckets scan can page through millions of keys, and holding them all only to discard nearly all of them cost memory proportional to the account rather than to the result set. A malformed regex is refused up front here (unlike the live filter, a search is a deliberate one-shot request, and an empty result set would read as "no matches"). Results are capped at `searchMaxResults` (1000), and a cancelled scan reports what it found so far, flagged as truncated. `computeHits` remains the pure, testable form of the same filter. Enter on a result calls `revealKey` (or `jumpTo` when the hit is in another bucket): it clears any active filter, records history, points the active pane at the hit's `parentPrefix` (`c.pane().SetPath`), sets its pending cursor target to the full key (`SetRestoreNext`), and calls `updateList()` — so the browser lands in the containing folder with the object highlighted.

## Dual-pane (addressed panes)

The two-pane (Midnight Commander) layout used to be implemented by **state-swap**: the controller's own per-location fields *were* the active pane, and `panes[inactive]` held the other as a `paneState` snapshot, with `Tab` copying twelve fields each way. That is gone.

A pane is now a `*session.Pane` — one object owning its location, object map, selection sets, filter, partial flag and pending cursor target behind one mutex, plus its history (`session.History`), which has no lock because only the UI goroutine touches it. The controller holds `panes [2]*session.Pane` plus an `active` index, so `swapPane` is `c.active ^= 1` plus repointing `view.List` / `view.Filter` / `view.Header` at the other pane's fixed widgets. Per-location access goes through `c.pane()` (active) and `c.other()` (inactive).

Addressing rather than mirroring is what retired the navigation race — but only where a flow actually holds on to the address. `Down`, `Up`, `jumpTo` and `navigateTo` capture their `*Pane` on the UI goroutine *before* spawning, so the goroutine writes the new location into the pane the user started from no matter what Tab does meanwhile, and they set the whole location in one `Pane.Enter` call under the lock, so no reader can observe a new bucket paired with an old prefix. Every flow that acts on a pane later does the same: `updateList` stores its listing in the pane it started for (see *Listing: rendering vs fetching*), copy/move captures the source pane, bucket and scope before its bucket-list round trip, a cut records its pane in the clipboard so the paste clears marks there, and rename and batch rename capture theirs. Each of those used to read `c.pane()` from its goroutine, and after a Tab the answer was the other pane — a listing stored in the wrong pane, a move copying and deleting same-named keys in the other pane's bucket, marks cleared on the wrong side. `renderList` likewise takes the pending cursor target with `TakeRestoreNext` — one read-and-clear, where two separate calls could straddle a navigation and let one listing consume another's target. What remains unlocked is the reading of `c.active` itself (see *Mutexes*); note too that the navigation flows capture the pane for `Enter` but then call `updateList`, which lists whichever pane is active by the time it starts — after a Tab that is the other one, and the navigated pane is listed when it is next Tabbed back to.

- Both panes' list and filter widgets are wired with the *same* input-capture / changed-func (`listInputCapture`, `wireListChanged`, `wireFilter`); only the focused (active) pane receives events, and the handlers act on the active pane via the `c.view.*` fields.
- `Ctrl+O` (`ToggleDualPane`) rebuilds the root flex (`ShowSinglePane` / `ShowDualPane`), seeds pane 1 with the current location on first open, and focuses it. `swapAndFocus` re-fetches the newly active pane so it reflects changes made from the other pane. Copy/move (`copyOrMove`) defaults its destination to the other pane's bucket+prefix.
- `Profiles` and `Duck` call `resetPanes` to collapse to a clean single pane, so panes never leak across profiles.
- **Caveat:** both panes share one `model`/client, so region is per-client. Two panes in different-region AWS buckets would fight over the client region — the same cross-region limitation already documented below. Same-endpoint (MinIO/Ceph, same-region AWS) is unaffected.

## Bandwidth throttle

`model.Config.MaxBytesPerSec` (from the profile) builds a `*rateLimiter` on the `Model`, shared by `progressReader`/`progressWriterAt`, so uploads, sync transfers and the 4-worker download pool of one profile honor one cap between them. It is one limiter per `*model.Model`, not one for the process: each profile open builds a fresh model (and limiter), and a cross-profile copy builds a second model for the destination, whose limiter is separate — the copy is throttled by the source's, on the read side. The pure `throttleStep` is a token bucket (tokens may go negative — the debt is paid by the next refill, which is what prevents over-crediting sleep time); `newRateLimiter(0)` returns nil (unlimited), making the hot path free when throttling is off.

## Bookmarks, history, palette, batch rename

- **Bookmarks** live in `Config.Bookmarks` per profile; `jumpTo(bucketName, prefix, selectKey)` navigates like `Down`. `addBookmark` dedups by bucket+prefix.
- **History** is a per-pane `session.History` (back/forward stacks of `session.Visit`, capped at 100). `recordHistory` runs at the start of each user navigation (`Down`/`Up`/`jumpTo`/`revealKey`, and the local pane's own in/out); `HistoryBack`/`HistoryForward` drive `navigateTo`, which does *not* record (so it doesn't corrupt the stacks). A `Visit` is a bucket and a prefix only, with no local directory, so in a local pane back/forward just re-lists the directory it is in.
- **Command palette** filters the `paletteActions` registry with a case-insensitive subsequence match (`filterActions`); the chosen action runs on the active pane. The registry is rebuilt per invocation rather than being a package-level table, so an entry whose wording depends on state can say which way it will go (the local-pane toggle labels itself *open* or *close*).
- **Batch rename** resolves marked items through the pure `planBatchRename` (which applies `applyRenamePattern` and rejects `/`, duplicates, and targets that would clobber another selected item's source), then runs `MoveKeys` per item behind a progress modal.

## Background transfer queue

Download/upload progress used to live in a blocking modal. Now each transfer is a `jobs.Job` (kind/desc/status/totals/done counts, its own `cancel`, guarded by a per-job mutex), registered by `flows.Runner` for every operation it drives. The progress modal gains a **Background** button: pressing it calls `job.SetBackgrounded()` and closes the modal's layer, so the transfer keeps running headless while you browse; the runner's reporter (`flows.uiReporter.Progress`) updates the job always and the modal only when the job is not `IsBackgrounded()` (and not cancelling), and a backgrounded job gets no summary modal. The transfers panel (`t`, in `ui/screens`) is a `tview.List` re-rendered by a 300 ms ticker from `jobs.Queue.Snapshot()`; `d`/Del cancels the selected job's context, `c` clears finished. `screens.TransferRow` is pure (takes `elapsed`) for testing.

A finished job is announced rather than left to be discovered — see
*Background transfer notices*.

Concurrency: **download overwrite resolution stays foreground** (`ops.Download`'s phase 1, `plan`, asks through `rep.Ask`, which the controller's `askOverwrite` renders and blocks on), then `jobs.Queue` — a buffered channel of capacity 2, taken through `ops.Reserve` — gates the byte-transfer phase so at most two transfers push bytes at once; bandwidth is still capped per profile by the model's limiter (see *Bandwidth throttle*). Trade-off: two downloads started while one is mid-Phase-1 could show overlapping overwrite modals (minor, not data loss).

## Clipboard, undo, activity log

- **Clipboard** (`clip`): `y`/`x` fill it (copy/cut) from the marked/highlighted set via pure `clipItems`; `p` pastes into the current location. `ops.Copy` takes an explicit source bucket, so paste works from the clipboard's origin bucket (cross-bucket). A fill also records the pane it was made in (whose marks a cut-paste clears) and a generation number, so a backgrounded cut-paste that finishes late consumes its own cut and not one yanked while it ran.
- **Undo** (`lastUndo`, guarded by `undoMu`): the move path and the rename paths record the successful moves; pure `invertOps` swaps src/dst so re-applying reverses them. One-step; `u` confirms then runs the reverse `MoveKeys`. Copy/delete are not undoable. The move is `ops.Copy` with `Move` set, and it hands back only what *actually* moved through its `OnMoved` callback — so a failed item can never reach an undo entry pointing at a destination that was never written, a move where nothing succeeded records no entry at all, and a file item whose destination the user chose to keep ("Skip existing") is not recorded either. A folder item can be partly kept, so each entry carries the move's skip set, and `undoSkip` maps every kept destination key back onto its source key: the undo then leaves both alone instead of copying the kept destination over the untouched source and deleting it. The entry also records the client the move ran through (see *UI update rule*), and finishing an undo clears only its own entry (`consumeUndo`), not one a move recorded while it ran. What a folder undo still cannot tell apart is an object that was already in the destination folder before the move: the reverse move takes the whole folder, so those come back to the source too (see *Known limitations*).
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

Because a folder target is a prefix delete, the confirmation must state its cost: a scan goroutine runs `ListObjects` per folder/bucket target, filling in `objects`/`bytes`, behind a cancellable "Calculating delete size..." modal. `deleteConfirmText` (pure) renders the totals and names up to 6 targets. A target whose scan failed is counted separately and disclosed — the totals are never silently short. `runDelete` then deletes sequentially behind a **cancellable** progress modal (a folder delete lists recursively before it removes anything, so it is long enough to want out of; cancelling stops before the next target and reports what already went), collecting failures into an `opResult` instead of aborting — so the report can offer *Retry failed* — and drops each deleted name from the selection set as it goes. A bucket target is emptied first (`EmptyBucket`) — S3 only deletes empty buckets, and the confirm has already promised the objects go; on a *versioned* bucket old versions survive and the bucket delete still fails (see the limitations table). When the profile enables safe delete, each non-bucket target is diverted through `trashTarget` instead of being removed (see *Trash*).

## Overwrite confirmation for remote writes

Downloads have always prompted before replacing a local file; the remote side
silently destroyed whatever sat at the destination. Rename onto an existing
name, copy into a folder holding same-named objects, paste, upload — all of it
clobbered without a word, and undo only moved the *new* object back, so the
overwritten content was simply gone. Every copy, move, rename, batch rename,
paste, upload and trash restore now goes through `confirmOverwrites` first
(cross-profile copy asks the same question from its own goroutine, below);
the writes that deliberately do not are listed at the end of this section.

- **What would be written.** `PlannedCopyKeys` expands each item into the exact
  destination keys, running the same `planFolderCopy`/`remapKey` mapping
  `CopyKeys` uses — so the list in the dialog is not an approximation of what
  the transfer would do, it is the same computation. It also refuses a folder
  copy whose destination keys would land on the source's own objects
  (`checkSelfOverlap`), so that error arrives before the prompt rather than
  after it. Upload skips the listing entirely: `PrepareUpload` (or, from a
  local pane, `LocalTargets`) already knows every key, and the upload sends
  exactly that list. Folder markers are left out of an upload's scan
  (`overwriteCandidates`): a zero-byte marker replaces no content, and probing
  a `/` key reports everything already under that folder as a conflict.
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
  keys*, threaded into `CopyKeys`/`MoveKeys` and the upload's target filter,
  which is why a partial skip inside a folder copy works per object rather
  than dropping the folder — and it is kept with any undo entry the move
  records, so the undo leaves the kept objects alone too.
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
feature exists to remove. The same care applies to an upload, which drops the
skipped targets *before* the totals are taken — leaving them in made a skipped
upload finish at 60% with an index counting files it never sent. And a rename
or batch-rename operation whose objects were all kept records no undo entry:
its destination does not exist, so undoing it could only fail.

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
own confirmation and restores objects to where they were moments ago — while
leaving alone whatever the original move skipped. Nor do the **in-place
rewrites** — a metadata save, a storage-class change, a version restore, an
editor save — which write back the key they just read; the editor's save has
its own lost-update guard instead (see *Edit in $EDITOR*). Creating a folder,
an empty-directory upload and a move into the trash write markers or
time-stamped keys and are not scanned either.

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
disposition/encoding/language, Expires, the website redirect, user metadata
and tags explicitly. (A metadata save is a replace-copy on either path, and it
sends the same set: Content-Language, Expires and the redirect are not
editable, but a replace discards every header it does not send, so it carries
them through.) Storage class is handled the other way round — set only when
explicitly asked — so both paths agree: a plain copy lands in STANDARD either
way, which is what S3 gives a copy that names no class, and a class change
still lands where it was told. That includes every move, rename, trash and
restore: a `STANDARD_IA` object comes back from the trash as STANDARD, and an
archived (GLACIER / DEEP_ARCHIVE) one cannot be copied at all until it is
restored (see *Known limitations*).

- **Encryption is chosen, not inherited.** A copy that sends no SSE headers
  gets the *destination bucket's* default, not the source's encryption — so
  every copy up to 5 GiB used to drop the profile's SSE (failing outright
  under a bucket policy that requires the header) and re-encrypt an SSE-KMS
  object under the bucket default. `copyEncryption` decides it for both paths:
  the profile's SSE whenever it names one; otherwise, for a copy within one
  bucket, the source's SSE and KMS key when a HEAD of the source is at hand;
  otherwise nothing, i.e. the destination bucket's default. A cross-bucket
  copy deliberately takes that default (S3's own rule, and it does not depend
  on the source's KMS key being usable over there); so does a sized copy from a
  listing or a plan, which skipped the HEAD.
- **No additional checksum on the multipart path.** An upload that declares a
  checksum algorithm must send a checksum with every part, and `UploadPartCopy`
  has no way to send one: MinIO refuses each part and AWS the completion, so
  with `checksum_algo` set every copy above the threshold used to fail.
  `CreateMultipartUpload` is therefore sent without one, and a copy above
  5 GiB carries no additional checksum — verification then reports it as not
  verifiable rather than pretending. Single-request copies do send the
  profile's algorithm.
- **Sizes come from listings, not extra round trips.** `CopyObjectSized` takes
  the size the caller already has (`ListObjects` for a folder copy, the sync
  plan for a remote→remote run). Only a copy whose size nobody knew — a
  single-object `CopyObject`, a storage-class change, a version restore — pays
  a `HeadObject` up front, because there is genuinely nothing to go on; the
  multipart path HEADs anyway for the attributes it must re-apply, unless a
  metadata save has supplied them.
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

`internal/config/awsshared.go` reads `~/.aws/credentials` and `~/.aws/config` (honoring `AWS_SHARED_CREDENTIALS_FILE` / `AWS_CONFIG_FILE`) with a small pure INI parser rather than the SDK's shared-config loader, so the profile list, the merge precedence (credentials file wins) and the `[profile x]` vs `[x]` section naming are all directly testable. Being hand-written, it has to accept what the SDK's reader accepts: a section header ends at the first `]`, so `[prod] # careful` is still prod (requiring `]` to be the last character left that header unrecognised and merged prod's keys into the section above — importing *that* profile then authenticated as prod), and a `#` or `;` preceded by whitespace starts an inline comment that is stripped from the value (`stripInlineComment`). Profiles that delegate rather than carry keys (`sso_session`, `role_arn`, `credential_process`) are recognised by `AWSProfile.Delegates` and imported as **delegating** s3duck profiles — the SDK resolves them on every use, which is also what keeps them refreshed (see *Credentials, delegated* below). Only a profile with neither keys nor a delegation mechanism is listed with the reason it cannot be imported. `Ctrl+I` on the profiles screen imports the selected one as `aws-<name>`, de-duplicated by `uniqueProfileName`.

## Sync

`Sync` (Ctrl+E) mirrors in one of three directions: a local directory up to the current bucket+prefix, that prefix down to a local directory, or the current prefix across to another bucket+prefix on the same endpoint. (A `~` in the local directory is expanded as everywhere else a directory is typed; left alone it named a directory literally called `~`, which a download sync then created.) It is the only operation that can both overwrite and delete, so the flow is always **scan → dry-run plan → explicit Apply**; there is no way to run it unreviewed.

- **Spec.** A run is described by one `syncSpec` (direction + local dir + source and destination bucket/prefix). Introducing it replaced a growing parameter list: the local↔remote flows only ever needed one bucket, but a remote↔remote run needs two, and threading both through every function is where mistakes would have lived. `collectSides` is the single place that knows which side comes from where.
- **Collect.** `model.WalkLocal` walks the local root into `SyncEntry{Rel, Size, Mod}` (regular files only — a directory has no counterpart to compare against); `model.ListRemoteEntries` does the same from a paginated `ListObjects`, skipping folder-marker keys. Both sides key on a slash-separated path relative to their root, so they compare directly. `collectSides` then applies the run's **exclude patterns** (`applyExcludes`, matcher syntax) *before* anything else looks at the entries — an excluded path is invisible to the planner, so a delete-extraneous run cannot decide it is extraneous. That protection is why every pattern is compiled when the form is read and one that does not compile stops the run: an invalid `re:` used to match nothing, which put the very path it was meant to protect into the delete plan. The profile's trash prefix is dropped from the remote side(s) the same way (`dropTrash`): it is the app's own bookkeeping, and a delete-extraneous sync of the bucket root used to plan — and run, as real deletes — the removal of everything in the trash. Finally, when asked, `collectSides` fills each side's `Sum` for the paths present on both (`fillSums`).
- **Plan.** `planSync(src, dst, del)` is pure. When both sides carry a checksum with the same algorithm, that decides it: equal sums mean no transfer whatever the timestamps say, different sums mean a transfer whatever the sizes say. Otherwise a file transfers when it is missing at the destination, when the sizes differ, or when the sizes match but the source is newer by more than `syncModTolerance` (2s, absorbing clock skew and coarse filesystem/S3 timestamp granularity). A zero timestamp on either side degrades to a size-only comparison rather than forcing a transfer. Deletes are emitted **only** when the flag is set. Output is ordered creates → updates → deletes, each group by path, so the plan is deterministic and reviewable.
- **Apply.** The plan is handed to `ops.Sync`, which runs through the shared `flows.Runner` like every other transfer — so a sync is cancellable and backgroundable and honours the bandwidth limiter without `runSync` knowing how any of that works. `ops.Sync` sees only `SyncUnit{Label, Bytes, Delete}` plus an `Apply(ctx, i, onProgress)` closure: it has no idea there are three directions, which is what keeps the barrier and the pool testable without a bucket or a filesystem. Operations run through a **4-worker pool** (`ops.SyncWorkerCount`, never more workers than work), in two phases from the pure `ops.SplitSyncPhases`: every write completes before any delete starts, so a run cancelled partway leaves the destination having *gained* the new files but not yet *lost* the old ones — the safer intermediate state. Within a phase the operations are independent by construction (a path is either present at the source or not), so they interleave freely. Per-unit byte counts live in an `inFlight` map keyed by plan index so the displayed total stays correct with several transfers in progress. The controller keeps exactly the part that needs to know all three directions: `applySyncOp`, which maps plan entry *i* onto `model.UploadFile` (upload to an explicit key — `Model.Upload` derives keys from a local path of its own), `model.DownloadTarget`, `model.CopyObjectSized`, `model.DeleteKey`, or `os.Remove`. Failures are collected, not fatal: one unreadable file doesn't strand the rest, and the retry plan re-runs exactly those units (its `Apply` remaps the new indices back onto the original plan). A *cancel* is not a failure, though: the units in flight when it lands are not ledgered as failed, and the run reports itself cancelled. Ledgering them used to offer a "Retry failed" covering only those few, silently dropping every unit never dispatched and every delete.
- **Remote → remote.** The planner never knew which side was local — it diffs two `[]SyncEntry` — so the third direction needed only a second `ListRemoteEntries` call and one branch in `applySyncOp`, which issues a server-side copy instead of an upload — `CopyObjectSized`, with the size the plan already carries, so no object costs a `HeadObject` to choose between the single-request and multipart paths. Two consequences worth knowing: a server-side copy moves no bytes through this process, so there is **no byte progress** for a remote→remote run (the op counter is the only thing that advances); and it inherits the same-endpoint constraint documented below, since both sides go through one client.
- **Safety.** Deletes are skipped entirely when any write failed — a partly-written destination is not the mirror the reviewed plan assumed, so removals are no longer covered by the user's approval. A remote→remote run between overlapping prefixes of the same bucket is rejected up front (`prefixesOverlap`): the source listing would include the destination, so src-inside-dst with delete-extraneous would delete the physical source objects, and dst-inside-src re-nests one level per run (`mirror/mirror/…`). `DeleteKey` refuses any key ending in `/`, so a sync delete can never degrade into `Delete`'s recursive prefix removal. For files the reviewed plan marked as updates, the download op passes `overwrite=true` and `DownloadTarget` swaps the file atomically (temp + rename) once the body is fully on disk — it used to pre-remove the stale copy, which destroyed the local file even when the transfer then failed. Nor does the download op create directories itself any more: `DownloadTarget` makes the parents only after `SafeLocalPath` has checked the key stays inside the local directory, where an `os.MkdirAll` up front created directories outside it for a hostile `../` key before the write was refused. Two download-direction gaps remain (see *Known limitations*): a non-canonical key such as `a//b.txt` plans as a create of `a//b.txt` *and* a delete of the local `a/b.txt` — the same file, so delete-extraneous removes what the run just downloaded — and writes go through a symlinked local subdirectory while the plan says "create". `WalkLocal` follows a symlinked root (`walkFollowingRoot`): `filepath.Walk` lstats its root, so a symlinked directory used to produce an *empty listing with a nil error* — precisely the partial-listing-taken-as-truth case the doc comment promises can't happen, and with delete-extraneous set it planned deleting the entire destination.

## Object metadata, tags and storage class

S3 has no metadata-update API, so `PutObjectMeta` rewrites an object by copying it **onto itself** with `MetadataDirective=REPLACE`. A replace keeps only what it sends, so everything the HEAD returned rides along explicitly: the storage class (a replace-copy that omits it silently demotes the object to STANDARD), the headers the form does not edit (Content-Language, Expires, the website redirect — all three were dropped before the 2026-10-20 review), and the encryption (see *Large copies*). `SetStorageClass` is the mirror image — it leaves `MetadataDirective` at its COPY default so the existing metadata rides along untouched (verified against MinIO: changing the class preserves Content-Type).

Tags live behind a separate API pair, so the editor fetches both and **writes back only the halves that changed** — a metadata save is a full object copy, far too expensive to do for a tags-only edit. Tagging is optional on S3-compatible backends, so a failing `GetObjectTagging` degrades to "no tags" with a note in the form rather than blocking the editor. Tag limits (10 tags, 128/256 char key/value) are checked client-side so the user gets a precise message instead of a generic `InvalidTag`.

`ParseRestoreStatus` reads the `x-amz-restore` header. It cannot split on commas: the value is quoted and an RFC 1123 expiry date contains commas of its own (`expiry-date="Fri, 21 Dec 2012 00:00:00 GMT"`), so each directive is read up to its closing quote. `IsArchived` deliberately excludes `GLACIER_IR` — instant retrieval needs no restore despite the name.

Both forms are read back with `GetFormItemByLabel` and the exported `widgets.Field*` constants rather than by index. They mix `TextView` rows with inputs, and a positional read returns the wrong widget the moment a row is inserted — the trap the profile form already sprung when the session-token field was added.

## Object versioning

`ListVersions` gives one object's history. `ListObjectVersions` is **prefix**-based, so it also returns every key that merely starts with this one; the exact-key filter is what turns it into a per-object history (verified: a sibling `doc.txt.bak` does not leak into `doc.txt`'s history). Pagination is followed to the end, since a heavily-rewritten object easily exceeds a page. Delete markers are listed alongside real versions rather than filtered out — the marker *is* what makes an object look deleted, and removing it is how you undo that.

`RestoreVersion` copies the chosen version to the top of the history rather than rewinding, which is the only non-destructive way to go back in a versioned bucket; the e2e run confirms the history grows from 3 entries to 4. `DeleteVersion` is the one genuinely destructive action here — it removes the data outright and leaves no delete marker — so it is behind an explicit confirmation that says so. Downloads are saved as `name.<8-char-version>.ext` so several versions can coexist in one directory. `D` diffs the selected version against the current one (`GetVersionContent` for both bodies, then `unifiedDiff`); a delete marker has no body and is refused rather than diffed.

## Duplicate finder

`D` lists the current prefix recursively and groups objects by **(size, ETag)** — the strongest content signal S3 offers without downloading. For single-part uploads the ETag is the body's MD5, so a match means identical content; multipart ETags depend on the part split, so identical files uploaded differently won't group — a missed duplicate, never a false positive (and size is the second factor guarding against multipart-ETag collisions). The help line discloses this. `ObjectChecksum` (see *Integrity*) was expected to close that blind spot, and it does so only partly: a multipart object's stored checksum is a checksum-of-parts, as dependent on the split as its ETag, so a deep scan over stored checksums would only help where both copies carry a whole-object checksum of the same algorithm (SSE-KMS objects written in one request, say, whose ETags differ although their content does not). Closing it fully means hashing content, which is a download per candidate. ETag quotes are normalized because some backends omit them.

`findDuplicates` is pure: groups sort by wasted bytes descending (the first group frees the most), members oldest-first — the oldest copy is the likeliest original and is marked `*`, making everything after it a natural deletion candidate. The browser is two `tview.List` views, each its own pushed layer that replaces the previous level: groups → members, with Enter revealing a copy via `revealKey` and `d` deleting it behind a confirm. The delete is a delete like any other: on a safe-delete profile it goes through the trash (`trashTarget`), and the confirm says which of the two it will be ("moved to the trash" / "deleted permanently"). Deleting updates the group through the pure `dropDupMember`, which also answers the question the view must not get wrong: after a dissolve, the same index denotes the *next* group, so only an explicit "did this group survive" result prevents teleporting the user into an unrelated group's members with the same `d`-to-delete binding. The surgery runs on the UI goroutine (the list's handlers read the same slice), and only the delete round trip happens in the goroutine — behind a wait modal that holds focus until it returns. The list used to stay live during it, and a second `d` on the last other copy was confirmed against a count, "1 other copy remains", that the first delete was about to make false. The scan's bucket, prefix and client are captured at entry, per the transfer-capture rule.

## Edit in $EDITOR

`e` loads a small object into memory (`GetObjectContent`, 1 MiB cap enforced against both the reported length and the actual body — a lying backend must not balloon the process), sniffs for NUL bytes with git's binary heuristic, writes a 0600 temp file, and hands the terminal to `$EDITOR` (→ `$VISUAL` → `vi`) via `Application.Suspend` — which takes the app's own locks and is safe to call from the download goroutine. On a changed file the body is written back with `PutBytes`, which carries what the GET reported: Content-Type, Cache-Control, Content-Disposition/Encoding/Language, user metadata, the storage class (omitting it would demote to STANDARD — the same trap `PutObjectMeta` guards), and the tag set (fetched only when the GET reported a `TagCount`, so tag-less backends are never asked). It is a fresh PUT rather than a copy, so it is not quite everything: Expires and the website redirect are not carried, and the encryption is the profile's choice (or the bucket default), not the source object's own SSE-KMS key. An unchanged file uploads nothing.

Saving is guarded against the **lost update**: the ETag captured at download time is compared (HEAD) against the object's current ETag just before the PUT — the editor may sit open for minutes while a backgrounded sync rewrites the key. The guard fails closed: a HEAD that fails (the object was deleted meanwhile, or the request errored) is a version the user never saw just as much as a changed ETag is, and saving anyway used to recreate or overwrite the key. On a mismatch, a failed HEAD, and any upload failure, the save is refused and the temp file is *kept*, with its path in the error, so the user's edit is never destroyed along with the conflict. The one case that still saves without a comparison is an object whose GET returned no ETag at all — there is nothing to compare against. A HEAD-compare rather than `If-Match`: conditional-PUT support is spotty across S3-compatible backends, and a narrowed race window with universal compatibility beats an atomic guard that only works on AWS.

## Cross-profile copy

`>` copies the marked set to a bucket in a different profile. `model.CrossCopy` streams each object through this process — a GET from the source client feeds a multipart PUT on an independent destination client — which is the only copy that works across *endpoints*; server-side `CopyObject` requires both buckets behind one endpoint. The content headers (Content-Type, Cache-Control, Content-Disposition/Encoding/Language), user metadata and tags ride along from the source; the **storage class deliberately does not** — class names are not portable across providers (STANDARD_IA would fail the whole PUT on a backend that doesn't know it), so the destination's default applies. The source bandwidth limiter throttles the read side (bounding the whole pipe), and the destination uploader carries the standard retryer. The flow is profile → bucket → prefix, then a cancellable, backgroundable transfer job; folders expand to concrete objects with `crossDstKey` keeping the tail relative to the source location, so a copied folder keeps its name and structure. Item sizes are captured at entry on the UI goroutine (the listing they came from may be gone by transfer time), the source client is captured alongside them, and both expansion-error paths tear the progress modal down before reporting. The source is never modified, and the destination's region is resolved fail-safe (`RefreshClient`) before the transfer.

## Pane comparison

`=` diffs the two dual-pane locations and shows the result read-only. It is the same `planSync` the sync preview uses — deliberately, so the two can never disagree about what counts as a difference — run with `del=true` so entries present only on the right are reported as well, and with both sides' timestamps cleared (`withoutMtimes`): a comparison must be symmetric even though the planner is directional, and the planner flags a same-size file only when the *source* is newer, so a newer copy on the right used to compare as identical. `comparePlanText` re-words the three kinds (`left-only` / `differs` / `right-only`) because a comparison has no notion of creating or deleting, and states plainly that only names and sizes were compared. `showPlan` is shared with the sync preview and simply omits the Apply button when there is nothing to apply.

## Bucket config & multipart

`model.BucketConfig` gathers versioning/encryption/object-lock/region, treating endpoints that don't support a feature (MinIO/Ceph return an error) as its default rather than failing. `ListMultipartUploads`/`AbortMultipartUpload` surface and clean orphaned upload parts (one page — the first 1 000 uploads — which is usually ample for cleanup; note that "abort all" cannot tell an orphan from an upload this app is still running). Both are palette actions on the current bucket.

## Selection scoping

Multi-select state is keyed by `bucket:path` so selections survive navigation in and out of subfolders:

```go
selectedByScope["my-bucket:photos/2024/"] = {"vacation.jpg": true, "trip.mp4": true}
```

The map is per pane, and the scope key comes from the pane's location (`session.Pane.ScopeKey`). A local pane keys its own scope as `local:<directory>` (`session.LocalScopeKey`): sharing the empty scope with the buckets screen would leak marks between the two.

`session.Pane.mu` guards `selectedByScope` because selections are read by input handlers (UI goroutine) and cleared by post-download/move cleanup (network goroutines) — which is also why a move's cleanup clears the marks of the pane captured when it started rather than of whichever pane is active when it ends.

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

Two kinds of save write the file, and they are deliberately different. An
explicit edit (create, edit, copy or delete a profile, its options, its
bookmarks) rewrites the whole list through `WriteConfig`: a temp file renamed
over the original, so a crash mid-write cannot leave a partial file. If the
last load failed, the unreadable original is moved aside first — to
`config.json.bak`, or the first free `.bak.1`, `.bak.2`, … when that exists, so
a second corruption can never overwrite the first backup — and the load error
is cleared only once that rename succeeded. The save the app makes on its own,
the last location, goes through `SaveLocation` instead and never rewrites the
list (see *Persistent activity log and session restore*).

`secret_key` and `session_token` are stored in plaintext. The file has `0600` permissions (owner-readable only), but there is no OS-keychain integration. Future work: `github.com/zalando/go-keyring` — less pressing now that `aws_profile` stores no key material at all, though still the answer for a static MinIO key.

Two more files sit beside it, both optional:

| Path | Written by | Purpose |
|---|---|---|
| `~/.config/s3duck-tui/keys.json` | the user | Rebinds any action; see *Keymap*. Never written by the app. |
| `$XDG_STATE_HOME/s3duck-tui/activity.log` | the app | Append-only operation log, mode 0600, rotated once at 2 MiB; see *Persistent activity log*. |

---

## S3 compatibility notes

### Region handling

A profile opens (`NewModel` → `GetConfig`) with a custom endpoint resolver over its `base_url`, `HostnameImmutable: true` so the SDK never rewrites the URL, and the profile's region as the signing region.

On every bucket entry — `Down` from the bucket list, `jumpTo`, `navigateTo` — and for every endpoint, not only AWS, `RefreshClient` asks `GetBucketLocation` for the bucket's actual region, normalizes legacy constraint values (`""` → us-east-1, `"EU"` → eu-west-1), overwrites the model's region (`m.Cf.Region`) with the answer and rebuilds the client. For a `base_url` containing `amazonaws.com` the rebuild passes only the region, so the new client uses the SDK's own regional endpoint rather than `base_url`; that is what avoids redirect loops for cross-region bucket access. For any other endpoint it keeps the custom resolver and only the signing region changes.

If the lookup or the rebuild fails, `RefreshClient` leaves the existing client and region **untouched** and returns the error, which the navigation shows as a "Failed to resolve bucket region" toast before carrying on — the old behaviour silently pinned everything (including `CreateBucket`) to a guessed `us-east-1`, turning one denied `s3:GetBucketLocation` into a stream of baffling redirect errors. Two consequences of running it everywhere: a backend that does not implement `GetBucketLocation` shows that toast on every bucket entry, and because a *successful* lookup overwrites the model's region, `CreateBucket` — which sends that region as its location constraint — creates a new bucket in the region of the last bucket entered rather than the profile's (see *Known limitations*).

### Prefix normalization

Turning a browsing path into an S3 prefix (slash-separate it, and append a trailing `/` unless it is the bucket root) was open-coded in five places. It now lives in one exported helper, `model.NormalizePrefix`, used by `SafeLocalPath` (and through it `DownloadTarget` and the download verify), `PrepareUpload`, `ListRemoteEntries` and the sync form, the copy/move destination and the undo's skip mapping, the local-pane upload, `showSummaryModalFor`, the usage browser (`usage.go`), the duplicate finder (`duplicates.go`) and the cross-profile copy (`crossprofile.go`). Consolidating also fixed a latent bug on the copy/move destination field: a user typing a bare `/` used to produce keys with a leading slash, where the helper reads it as the bucket root.

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

`internal/ui/widgets` got its first test file with this: `scrollHint` and the clamp
arithmetic are pure, and the panel itself is rendered onto a `tcell`
SimulationScreen at 80x20 and driven with real key events, which is what pins
"the tail is reachable" rather than "the constructor was called correctly".

## Content type, encryption and checksums on write (`internal/model/write.go`)

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
is applied at every creation point: the upload path (`UploadFile`, which
`UploadTargets` and sync go through), `PutBytes`, folder markers, every
server-side copy, and the destination side of a cross-profile copy. It decides
three things — the derived Content-Type, the server-side encryption, and the
additional checksum — though not all three everywhere. A copy keeps the
source's Content-Type rather than deriving one. Its encryption follows
`copyEncryption` (the profile's, else on a same-bucket copy the source's,
else the destination bucket's default; see *Large copies*), which since the
2026-10-20 review covers single-request copies too — they used to send no SSE
at all. And the checksum algorithm goes on a single-request copy but never on
a multipart one, because `UploadPartCopy` cannot send the per-part checksums
such an upload would then require.

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

## Integrity (`internal/model/checksum.go`)

The ETag is only an MD5 for single-part uploads that are not SSE-KMS
encrypted; a multipart ETag is a hash of part hashes with a `-N` suffix, so it
depends on the part size the uploader happened to use, and an SSE-KMS object's
ETag is not a hash of its content at all. That limited three separate
features — the **post-transfer verify**, the **duplicate finder** (identical
files uploaded with different part sizes never group) and **sync**, which had
no content comparison — and S3's additional checksums were adopted to close
all three.

They close less than this section used to claim. A checksum on an object
written in one request is a whole-object hash: that gives sync a real content
comparison and makes an SSE-KMS object verifiable. But the checksum S3 stores
for a *multipart* upload is a checksum of the parts' checksums — like the ETag
it depends on the part split, and no whole-file hash can match it. Every upload
larger than the uploader's part size (5 MiB at the smallest) is multipart, so
for exactly the large files that motivated this, `checksum_algo` buys S3-side
checking of each part on the way in, not a verifiable download; and the
duplicate finder's blind spot stays open for the same reason (composite
checksums agree only when the parts split alike). Multipart *copies* carry no
additional checksum at all (see *Large copies*).

`ObjectChecksum` reads the stored checksum through `GetObjectAttributes` —
the only API that reports one — and falls back to a `HeadObject` ETag when a
backend does not implement it, so an unsupported endpoint yields "nothing to
compare" rather than an error. `VerifyLocalFile` prefers a whole-object
checksum, falls back to the ETag for single-part objects, and refuses to
compare a *composite* checksum against a whole-file hash: that false alarm
would be worse than no answer. Recognising one is the subtle part:
`GetObjectAttributes` reports a multipart object's checksum-of-parts *without*
the `-N` suffix HeadObject shows, so the value alone cannot tell, and looking
for a `-` in a base64 value never matched — every multipart download made with
a checksum verified as a MISMATCH. The multipart ETag decides it instead
(`isMultipartETag`). That rule is conservative: a multipart object carrying a
genuine full-object checksum, which newer AWS clients can write, is reported
unverifiable too. An SSE-KMS ETag is ruled out the same way (`ruleOutKMS`):
when the ETag is the only thing left to compare, one extra HEAD reads the
object's encryption, and an `aws:kms` object is reported as not verifiable,
with that reason (`ChecksumInfo.KMSETag`), instead of as corrupt. A size
difference is reported on its own because "1.2 GiB vs 900 MiB" says more than
a hash mismatch.

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
no longer re-sends everything because the local mtime is newer. Both only for
objects with a comparable sum (a whole-object checksum, or a plain MD5 ETag):
`RemoteChecksums` leaves out a composite checksum and an SSE-KMS ETag, so
those paths fall back to size+mtime.

## Streamed, cancellable listings (`internal/controller/listing.go`)

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
`PlannedCopyKeys`. The delete scan, the download resolution, the summary
scan, the usage and duplicate scans, the searches, the sync scan and the pane
comparison all run behind `cancellableWait` modals now and abandon their
listing when cancelled; cancelling the delete *scan* abandons the delete
rather than confirming against half-counted totals. That closes most of the
"cancellation doesn't reach listing phases" hardening item, not all of it:
`PlannedCopyKeys` accepts a context, but its callers — the overwrite pre-scan
behind every copy, move, rename, batch rename and paste, and the trash
restore's — still pass `context.Background()` (as does the conflict probe that
follows), and `confirmOverwrites` shows a plain, uncancellable wait modal while
that listing runs. A handful of short flows (rename, the
metadata and storage-class rewrites, the version actions, the editor) pass
`context.Background()` too; together they are what is left of that item.

What deliberately still runs on `context.TODO()` is the set of bucket-level
calls — `CreateBucket`, `CreateFolder`, `DeleteBucket`, `MakeBucketPublic`,
`GetBucketLocation`, `BucketConfig`, `PresignGetURL`, `ListMultipartUploads`,
`AbortMultipartUpload`. None of them paginates, and each request is bounded by
the transport's per-phase timeouts, so threading a context through them would
add signature churn for no behaviour a user could observe. They are not all
one round trip, though: `BucketConfig` makes four (location, versioning,
encryption, object lock), a public `CreateBucket` two (the bucket, then its
policy), and `PresignGetURL` none — it signs locally. `GetConfig`'s
`LoadDefaultConfig` is the same case.

## The failure ledger (`internal/controller/opresult.go`)

Every multi-item operation correctly collected failures instead of aborting on
the first one — and then threw them away: the report printed the first eight
and "…and N more", and once dismissed there was no way to see the rest, let
alone act on them.

`opResult` keeps every failure with the flow's own descriptor (`retryItem.target`,
opaque here and type-asserted back by the retry callback that owns it). The
report then offers **Retry failed**, which re-enters the same flow with only
the failed units — a 5000-file sync that lost twelve objects to a flaky link
costs twelve operations, not another full plan — and **Export list**, which
writes all of them to a file, since a `tview.Modal` cannot scroll. `text()` is
pure, so the wording of a partial failure is testable.

`opResult` itself now backs only the flows still written in the controller:
delete and batch rename (both with Retry and Export), and the trash restore
(Export only — no Retry). The operations that moved to `internal/ops` —
download, upload, copy/move/paste and sync — carry the same idea in
`ops.Result`: the operation attaches a `Retry` operation covering exactly the
failed units, and `flows.Runner` renders both buttons, exporting through
`opResult`'s writer. An upload stops at its first error, so it has one failure
and no retry. Two flows have neither button: a cross-profile copy lists its
first eight failures in the report and "...and N more", and an undo lists
every failure in its report with nothing to act on.

## Profile identity and read-only (`internal/controller/guard.go`)

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

## Background transfer notices (`internal/controller/notify.go`)

Backgrounding a transfer is a promise — "carry on, I'll tell you when it's
done" — and the app kept the first half only: finishing a job wrote one
activity line and nothing else. The only way to find out was to keep opening the
transfers panel, which defeats the point.

A single long-lived ticker recomputes a header badge twice a second and queues
a redraw **only when the text changed**, so an idle app costs a string compare
and no draws. `jobs.Queue.Finish` (through `announce`) announces a job that
was backgrounded (never a foreground one, whose modal is already reporting) as
a transient notice plus the terminal bell (the bell is skipped for a job the
user cancelled themselves), through `Screen.Beep()`, captured in the view's
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
delegating ones instead of listing the reason they could not be imported.

What is *not* caught at open is a delegating profile that resolves to nothing.
`GetConfig` checks for `cfg.Credentials == nil` so that it could name the
profile, but with the pinned SDK (`config` v1.18.8) the check never fires: a
profile name missing from `~/.aws` is ignored rather than refused, and a
profile with no credential source falls through the chain to the EC2
instance-metadata provider, so `LoadDefaultConfig` always hands back *some*
provider. The profile opens, and the first request fails with the SDK's own
credential error — typically the opaque "no EC2 IMDS role found" the check
was written to prevent.

## Local pane (`internal/controller/localpane.go`, `model/local.go`)

Upload was a modal browser that took exactly one item per flow: no
multi-select, no sorting, no size or date columns, and no view of the
destination while picking. Meanwhile the dual-pane layout, the column engine,
the selection scoping and the transfer queue all existed — for remote panes
only.

The trick that keeps this small is that a local entry is described by the same
`model.Object` as a remote one (directories carry a trailing separator in
`FullPath`, so a file and a directory of the same name cannot collide in the
selection set). Filtering, sorting, column layout and `renderList` needed **no
changes at all**; only navigation, marking, the pane title and the transfer
verbs had to learn which side they were looking at:

- the directory is part of the pane's location (`session.Location.LocalDir`),
  so a local pane survives `Tab`, and `resetPanes` builds fresh panes (it must
  not outlive a profile switch);
- the pane's scope key gives a local pane its own selection scope — sharing the
  empty one with the buckets screen would leak marks between them;
- marking asks `canMark()` — inside a bucket, *or* in a local directory. It used
  to ask only for a bucket, which a local pane never has, so Space and Ctrl+S
  did nothing there and the multi-file upload below was impossible;
- `Ctrl+Y` from a local pane uploads to the other pane's bucket+prefix
  (`LocalTargets` expands the selection, keeping each selected directory's own
  name at the destination, empty directories as folder markers), and it sends
  *exactly* that list: the overwrite scan and the transfer see the same
  targets. It used to hand the upload the directory the selection sat in,
  which the model then walked — so one marked file in `~` uploaded all of `~`,
  past a scan that had never seen those keys. A download with a local pane
  open lands in that directory rather than the profile's download directory;
- `remoteOnly` refuses the object-only actions. The local pane deliberately
  does **not** delete, rename or create: those keys have S3 meanings people
  have muscle memory for, and a mis-aimed one outside the bucket is not
  recoverable by anything this app offers. A move across the boundary is
  refused for the same reason.

## Usage browser (`internal/controller/usage.go`)

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

## Preview and version diff (`internal/controller/preview.go`)

Inspecting an object's contents meant downloading it or opening `$EDITOR`,
which refuses anything binary or over 1 MiB. The missing piece was the ranged
GET: `GetObjectHead` reads a fixed 64 KiB window, so previewing a 40 GiB log
costs one small request, and the read is capped independently of the range
because some S3-compatible backends ignore `Range` entirely.

Binary content is rendered as a hexdump rather than refused — the question a
preview answers about a blob is usually "what *is* this?", and magic numbers
answer it. Content goes through `sanitizeText` (control bytes to dots, tabs
expanded) and `escapeForTextView`, or a log line containing `[red]` would be
swallowed as markup. The escape is `tview.Escape`, which is what this tview
understands (a tag is neutralised by an `[` before its closing `]`); it
used to double every `[` instead, which is not an escape at all —
`[default]` rendered as a lone `[` and `[red]` still coloured. It covers the
hexdump's printable-bytes gutter and the version diff as well as plain text.
The viewer is a scrollable `TextView` inside `ModalClamped`, not a `Modal`: a
Modal's text cannot scroll, and a preview that shows only what fits is not a
preview.

`unifiedDiff` is a plain LCS walk rather than Myers, and shows full context,
because a version diff is usually a config file where the surrounding lines
are the point. Identical bodies produce empty output, which is what lets the
caller say "identical" instead of opening an empty window.

The walk is quadratic in **lines**, which is why the 1 MiB cap on the bodies
bounds nothing useful on its own: a 1 MiB CSV of 40-byte rows is ~26 000 lines
a side, and the table for it is ~5 GiB of `int`. `maxDiffCells` (4 Mi cells,
~32 MiB, two 2 000-line files) is the real bound; past it the diff declines
with a message naming both line counts and suggests downloading the versions.
That covers every config file a version diff is for, and declining beats
taking the process down.

## Trash (`internal/controller/trash.go`)

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

The original key is kept *exactly*, a leading `/` included: trimming it made
`/abs/file` restore to `abs/file`, a different key. Every delete honours the
setting — the duplicate finder's `d` included, which went straight to a
permanent delete until the 2026-10-20 review — except emptying the trash, the
one delete that is never diverted. And the trash is the app's own bookkeeping,
so sync leaves it alone: its prefix is dropped from both remote sides before
planning, where a delete-extraneous sync of the bucket root used to delete
everything in it for real.

Two things follow from the trash being a move. A move is a server-side copy
that names no storage class, so a trashed object lands in STANDARD and comes
back from a restore as STANDARD, whatever class it had; and an archived
(GLACIER / DEEP_ARCHIVE) object cannot be copied until it is restored, so it
cannot be trashed either — S3 refuses the copy (`InvalidObjectState`) and the
delete reports that object as failed.

## Keymap (`internal/controller/keymap.go`)

The browser's key space was full: `Ctrl+A`–`Ctrl+Y` almost without a gap, and
`y x p u t s S r v m c e D > = / [ ]` besides. Every new feature had to
displace a binding or hide in the palette, and a user who wanted a different
layout had no way to ask.

Bindings are now data: `defaultBindings()` pairs each named action with a chord
and a description, `~/.config/s3duck-tui/keys.json` overrides any of them, and
a leader key (default `,`) opens a second namespace for what no longer fits.
`actionTable()` is the app's vocabulary, and the hotkey panel is generated from
the live keymap (`keymap.helpLines()`), so the panel can no longer drift from
what the keys do. The command palette is *not* generated: `paletteActions` is
a hand-written list of labels and calls, so a new action reaches it only when
someone adds it there. Details worth keeping:

- **The defaults are exactly what the keys did before**, pinned by a test that
  lists them, because changing one is a user-visible break.
- **A rebound action loses its default chord** — leaving both working is the
  confusing half-state.
- **An unparseable override leaves the default in place** (only a *usable*
  override counts as one), so one typo cannot silently remove a key with
  nothing to replace it. Warnings are always written to the activity log; the
  startup modal listing them appears only when `config.json` loaded cleanly,
  since a config error takes that slot (`Run`).
- **Taking a chord over is allowed, never silent.** Binding a chord another
  action holds gives it to the new action with a warning naming the loser, and
  the loser's line is dropped from the hotkey panel rather than advertising a
  key that no longer does that.
- **Ctrl+letter is matched by key constant, not by rune**: tcell reports it as
  `KeyCtrlD` with the rune cleared, so `chordKey` canonicalises both a parsed
  spec and a live event the same way. Shift is not part of the canonical form —
  it is already expressed in the rune, and terminals do not report it
  consistently for anything else.
- **The modifier rules follow from that canonical form**, and anything it
  cannot represent is refused with a warning rather than bound to something
  else. `Ctrl+` needs a letter (`Ctrl+Shift+d` is just `Ctrl+D`), and
  `Ctrl+Alt+<letter>` keeps its Alt — dropping it used to bind plain Ctrl+D,
  stealing the download key. `Shift+<letter>` means the upper-case letter
  (`Shift+s` is `S`); `Shift+` on a character with no upper case is refused —
  write the shifted character itself. Only `Alt+` combines with a named key
  (`Alt+Left`): `Ctrl+Down` used to bind plain Down, taking the cursor key
  away.
- **An unknown key after the leader does nothing** rather than falling
  through: `,` then a typo must not delete anything.

## Persistent activity log and session restore (`internal/controller/audit.go`)

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
`LastBucket`/`LastPrefix` on the profile, recorded in memory
(`rememberLocation`) and written out when the profile is closed (Ctrl+P) or
the app quits through Ctrl+Q — one config rewrite per navigation would mean
rewriting a file full of credentials dozens of times a minute. With a local
pane active nothing new is recorded (it has no bucket; the previous location
stands), and Ctrl+C, which tview handles by stopping the application
directly, bypasses the save altogether.

The write itself is `config.Params.SaveLocation(name, bucket, prefix)`, and it
is careful in a way an explicit save does not need to be, because the user
never asked for it. It writes nothing when the config failed to load: the
automatic save used to go through `WriteConfig`, which on startup moved the
unreadable file aside, replaced it with an empty list and cleared the error
before the "Config error" modal could show it, so one typo in `config.json`
silently cost every profile. It re-reads the file and patches only that
profile's `last_bucket`/`last_prefix`, so quitting one instance no longer
erases profiles or bookmarks another instance saved since this one started.
And it writes nothing when the location has not changed.

`restoreLocation` must run on the UI goroutine, because `jumpTo` clears the
filter box and the details pane synchronously before spawning its own
goroutine. A remembered bucket that has since gone leaves the user on the
bucket list: the failed jump still lists the pane, which used to be left
showing the profile rows — where Enter opened a profile.

## Query syntax (`internal/controller/match.go`)

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
hold a hundred thousand of them. (A `*` in the pattern is tried as the wildcard
before it is tried as a character, or a key holding a literal `*` at that
position consumed the wildcard and failed to match.)

Only `re:` can fail to compile, and each consumer treats that according to what
an empty answer would mean. The live filter degrades to *matching nothing*, and
the pane title marks it `(bad pattern)` — the mode, not the compiler's reason:
silently ignoring the query would look like the filter had been applied and
found everything. The recursive search refuses to start and shows the error,
since an empty result would read as "no matches". And a sync exclude that does
not compile stops the run, because an exclude matching nothing puts the path it
was meant to protect into the delete plan.

## Known limitations

| Area | Description |
|---|---|
| ~~Navigation race~~ | **Fixed.** The live fields it described (`c.currentBucket`, `c.restoreNext`) no longer exist: a pane is now a `session.Pane` with one mutex over its whole location, and `Down()`/`jumpTo`/`navigateTo` capture the `*session.Pane` on the UI goroutine before spawning. A Tab during a slow bucket-open therefore cannot redirect a navigation into the other pane, and `Pane.Enter` writes bucket, prefix and cursor target under one lock so no reader sees the new bucket paired with the old prefix. Listings, copy/move, cut-paste and rename capture their pane the same way (see *Dual-pane*). |
| ~~No retries on upload~~ | **Fixed.** Every upload path (`UploadFile`, and through it `UploadTargets` and sync; `CrossCopy`) shares `newUploader`, which installs the SDK's standard retryer (`uploadMaxAttempts` = 3) and sizes parts to fit S3's 10 000-part limit. Retries are safe because the uploader buffers each part of a non-seekable body before sending it, so a retry resends the buffered part rather than re-reading a consumed stream. An upload still stops at its first failed file, with no Retry button. |
| **Sequential per-file overwrite** | Overwrite decisions block Phase 1 completion. For a large selection with many conflicts the user answers before any download starts — one dialog per conflict unless *Overwrite All* or *Skip All* settles the rest at once. |
| **Remote→remote sync has no byte progress** | The transfer happens inside S3, so the client sees only completed operations. The op counter advances; the byte gauge does not. |
| **Cross-bucket copy/move is same-endpoint only** | `CopyKeys` / `MoveKeys` take separate source/destination buckets and issue a server-side copy, so both buckets must be reachable through the one configured endpoint. This covers any single S3-compatible endpoint (MinIO/Ceph) and same-region AWS. Copying between AWS buckets in *different regions* is not handled (the client stays pinned to the source region); across *profiles*, `>` streams through the client instead. |
| **Copies land in STANDARD; archived objects cannot move** | A server-side copy that names no class is written as STANDARD — so a `STANDARD_IA` or `INTELLIGENT_TIERING` object is STANDARD after a move or a rename, and comes back from a trash-and-restore as STANDARD. GLACIER / DEEP_ARCHIVE objects cannot be copied, moved, renamed or trashed until restored: S3 answers `InvalidObjectState`. Only the in-place rewrites (storage-class change, metadata save, version restore) name a class, to keep or set it. |
| **`CreateBucket` uses the last bucket's region** | `RefreshClient` overwrites the model's region on every bucket entry, and `CreateBucket` sends that region as its location constraint, so a new bucket is created where the last bucket entered lives, not in the profile's region. |
| ~~Copies fail above 5 GiB~~ | **Fixed.** Sources over `MultipartCopyThreshold` are copied part by part with `UploadPartCopy` (see *Large copies*). |
| **Multipart copies carry no additional checksum** | `UploadPartCopy` cannot send part checksums, so a copy over 5 GiB is created without `checksum_algo` (declaring it made every such copy fail). The copy is reported as not verifiable afterwards. |
| **No download resume** | Interrupted downloads restart from byte 0. Transfers write to a temp file beside the target, `<name>.<random>.s3duck-part` (unique per transfer), that is removed on cancel/failure; the target file is only ever replaced by a completed download. |
| ~~Sync compares size + mtime, not content~~ | **Fixed (optional).** The sync form's *Compare content by checksum* fills `SyncEntry.Sum` from the object's stored whole-object checksum (or its plain-MD5 ETag) and a locally computed one, and `planSync` compares those when both sides agree on the algorithm. It costs one request per remote object and a full read per local one, so it is a choice rather than the default; paths where either side cannot produce a comparable sum fall back to size+mtime. |
| **An upload sync straight after a download sync re-uploads** | A downloaded file's local mtime is its download time, which is newer than the object's `LastModified`. Reversing the direction therefore sees "source is newer" for every file and re-sends them once (sizes are equal, so nothing is corrupted, and the second reversal is a no-op). This matches `aws s3 sync` semantics; the dry-run plan shows it before anything moves — and *Compare content by checksum* avoids it entirely. |
| ~~Sync applies one operation at a time~~ | **Fixed.** `ops.Sync` uses a 4-worker pool with a writes-then-deletes barrier (see *Sync* above). |
| **Sync direction is one-way** | Each run treats one side as the source of truth. There is no bidirectional merge and no conflict resolution — the newer-wins rule only ever applies in the chosen direction. |
| **Download sync and unusual keys** | A non-canonical key such as `a//b.txt` is planned as a create of `a//b.txt` *and* a delete of the local `a/b.txt` — the same file — so delete-extraneous removes what the run just downloaded. Writes also follow a symlinked local subdirectory while the plan says "create". |
| **Static session tokens expire, silently** | A *stored* temporary credential is used as-is: when it expires, calls start failing with an auth error and s3duck neither refreshes it nor warns beforehand. The fix is not to store one — set the profile's `aws_profile` instead and the SDK re-resolves (and re-runs SSO / assume-role) on expiry. |
| **Versioned buckets can't be emptied from the TUI** | `model.Delete` sends no `VersionId`, so folder deletes write delete markers only; `EmptyBucket` clears current objects but old versions survive, and `DeleteBucket` then fails with BucketNotEmpty. A version-aware purge is on the roadmap. |
| **Whitespace keys** | Every secondary-text reader trims the key, so `"dir/report "` resolves to `"dir/report"` in lookups (wrong object if both exist, silent no-op if only the padded one does). |
| **Versioning needs a versioned bucket** | On an unversioned bucket S3 reports a single `null` version; the browser shows exactly that rather than hiding the feature. Enabling versioning is a bucket-level operation s3duck does not perform. |
| **Restore is asynchronous** | `RestoreObject` only *requests* a restore; completion takes minutes to hours and s3duck does not poll. Re-open the storage-class form (`c`) or the metadata editor (`m`) to see the current state. |
| **Metadata edits rewrite the object** | A metadata save is a server-side copy onto the same key. On a versioned bucket that creates a new version; the ETag may also change for multipart objects (and always does when the object is large enough to take the multipart-copy path, which re-chunks it). |
| **Undo and sync do not prompt before overwriting** | Copy, move, rename, paste, upload and trash restore confirm first (see *Overwrite confirmation*). Sync is exempt because its dry-run plan already lists the updates; undo because it has its own confirmation and restores objects to where they just were — leaving alone whatever the original move skipped. An undo belongs to the profile its move ran in; anywhere else `u` says "Nothing to undo in this profile". In-place rewrites (metadata, storage class, version restore, editor save) do not prompt either. |
| **A folder undo also moves pre-existing objects** | Undoing a folder move reverses the whole destination folder, so objects that were already in it — ones the move did not conflict with — are moved back into the source folder too. Nothing is lost (their keys did not exist at the source), but they end up where they never were. A per-object undo would need `MoveKeys` to report the exact keys it wrote. |
| **The inactive pane keeps its previous column layout** | `renderList` renders the active pane, so right after `Ctrl+O` the other pane still shows columns sized for the previous width. It self-heals the moment you `Tab` to it (`swapAndFocus` re-fetches and re-renders). Fixing it properly needs a render path that can target a pane other than the active one. |
| ~~Summary top-10 cap~~ | **Fixed.** The graph folds everything past the top ten into one disclosed row, and the usage browser (`G`) walks the whole tree without a cap. |
| ~~Cancellation doesn't reach listing phases~~ | **Mostly fixed.** Context is plumbed through `List`/`ListObjects`/`ListBuckets`/`Delete`/`EmptyBucket`/`ListRemoteEntries`/`ResolveDownloadObjects`/`PlannedCopyKeys`, and every scan (delete sizing, download resolution, summary, usage, duplicates, search, sync, compare) runs behind a cancellable wait modal. The exception is the overwrite pre-scan: its `PlannedCopyKeys` listing runs on `context.Background()` behind an uncancellable wait (see *Streamed, cancellable listings*). |
| **Listings cap at 20 000 objects per level** | A prefix with more keys at one level is listed to `listCap` and marked *partial* in the title; the rest is reachable through the recursive search (Ctrl+F) or the usage browser. The cap exists because a `tview.List` holding a hundred thousand rows is slower to render than the listing was to fetch. |
| **The local pane never writes locally** | It browses, marks and uploads; it does not delete, rename or create. Deliberate: those keys have S3 meanings, and a mis-aimed one outside the bucket is beyond anything this app can undo. A move across the pane boundary is refused for the same reason. |
| **Local-pane navigation gaps** | A history entry is a bucket and a prefix, so back/forward in a local pane only re-lists the directory it is in. And opening the local pane (`l`) from single-pane mode after dual mode was used before reopens pane 1 at its old location rather than at the current one. |
| **A trashed object still costs storage** | Safe delete is a move, not a removal: the objects stay in the bucket — in STANDARD, whatever class they had, since the move is a copy that names none — until the trash is emptied. On a versioned bucket the move also leaves a delete marker at the original key. |
| **Verification cannot cover every object** | Only a whole-object checksum or a plain-MD5 ETag can be compared. A multipart object — whose stored checksum, if any, is a checksum-of-parts — and an SSE-KMS object without an additional checksum can only be compared by size, and `VerifyLocalFile` says which rather than comparing a hash that could never match. `checksum_algo` makes objects written in one request verifiable (SSE-KMS ones included); it cannot do that for multipart uploads, which is every upload over the part size. |
| **Download verification runs after the rename** | The file is checked at its target name, so on a mismatch with Overwrite chosen the previous file has already been replaced and the corrupt one stays in its place — reported as a failure, but not rolled back. |
| **`GetObjectAttributes` support varies** | S3-compatible backends may not implement it; `ObjectChecksum` then falls back to the HEAD ETag, which limits verification to single-part objects. Reported, not silently degraded. |
| **The keymap covers the browser only** | `keys.json` rebinds the browser's actions; the profiles screen and the modal overlays keep their built-in keys. Rebinding a chord the modals also use (Esc, Enter) affects only the browser. |
| **The activity log is append-only and unencrypted** | It records profile and bucket names, the object name for single-object actions and each downloaded key, and counts for bulk ones (a folder delete or copy, a sync) — never credentials — at mode 0600, rotated once at 2 MiB. Delete the file to clear it; there is no in-app purge. |
| **Ctrl+C does not save the location** | tview handles Ctrl+C by stopping the application directly, past `Stop`, so the last location is not written; quit with Ctrl+Q to keep it. |
| **The version diff is bounded by line count** | `unifiedDiff`'s LCS table is quadratic in lines, so `maxDiffCells` (4 Mi cells, two ~2 000-line files) bounds it rather than the 1 MiB body cap, which would permit a multi-gigabyte table. A larger pair is declined with both line counts named; download the two versions and diff them locally. |

---

## Fixed in the 2026-10-20 review

Another read-through of the whole tree, aimed this time at what the overwrite
guard, the undo, the local pane and the config file promise. The first four
lost data or wrote where the user had not asked; the next group failed
outright, or acted on the wrong pane or profile; the rest are smaller. The
version stays 0.10.2.

- **Undo after "Skip existing" destroyed what the user had kept.** `ops.Copy`
  recorded a skipped *file* item as moved, so undoing the move "moved it back":
  it copied the kept destination — the very object the user had chosen not to
  overwrite — over the untouched source, then deleted it. The source's content
  was gone, and so was the destination the user had kept: the overwrite they
  had declined, performed in reverse. A folder move, rename or batch rename
  with a partial skip was undone wholesale the same way. A skipped file item is
  no longer recorded (nor counted as done — it is counted once, in Skipped),
  and every undo entry now carries the move's skip set (`ops.Transfer.Skip`,
  `transferPair.skip`), which `invertOps`/`undoSkip` map onto the reverse
  direction so the kept objects stay where they are. One gap remains, in
  *Known limitations*: a folder undo still moves back objects that were already
  in the destination folder.
- **A local-pane upload sent the pane's whole directory.** Ctrl+Y over one
  marked file in `~` uploaded all of `~`. The upload port took a directory to
  walk, the controller handed it the directory the selection sat in, and the
  overwrite scan before it had seen only the marked keys. The port is now
  `Pusher.UploadTargets` (`model.Model.UploadTargets`) and `ops.Upload` sends
  exactly its `Files`. `Model.Upload` became `PrepareUpload`, the skip filter
  and `UploadTargets`, so the keys written are by construction the ones
  previewed — which also fixed a mismatch under prefixes holding `//` or `..`,
  where the old walk's `path.Join` cleaned segments `PrepareUpload` kept, so
  the preview promised one key and the upload wrote another. `PrepareUpload`
  now lists empty directories as `/` marker targets (the convention
  `LocalTargets` already used), `uploadKey` is gone, folder markers are left
  out of the upload's overwrite scan (`overwriteCandidates`), and the report
  counts every file ("Done: N of N"; it said 1 of N).
- **A config.json that failed to parse was replaced at startup.** `Run` →
  `Profiles` → `persistProfiles` → `WriteConfig` moved the unreadable file to
  `.bak`, wrote an empty list and cleared `LoadErr` before the "Config error"
  modal could say anything; a second corruption then overwrote the `.bak`. The
  automatic save is now `config.Params.SaveLocation(name, bucket, prefix)`,
  which never writes while `LoadErr` is set, re-reads the file and patches only
  that profile's `last_bucket`/`last_prefix` (so quitting one instance no
  longer erases profiles or bookmarks another one saved), and writes nothing
  when nothing changed. Explicit edits still go through `WriteConfig`, whose
  backup never overwrites an existing one (`config.json.bak`, then `.bak.1`,
  `.bak.2`, …) and which clears `LoadErr` only after the rename succeeded.
- **A folder copy into its own ancestor destroyed the source.** A same-bucket
  copy or move of `a/a/` to `a/` lists `a/a/a/f` and writes it onto `a/a/f` —
  a source object not yet read — and a move then deleted both, so the original
  content existed nowhere. `planFolderCopy` refused a destination *inside* the
  source, not this. `checkSelfOverlap` refuses exactly this case — only when
  some remapped key lands on a source key — in `copyKeysTracked` and in
  `PlannedCopyKeys`, so the user hears it before the overwrite prompt.

The ones that failed outright, or acted on the wrong target:

- **Uploads over 48.8 GiB failed at the last part.** Every upload body is a
  `progressReader`, which is not seekable, so the SDK never grew the fixed
  5 MiB part size, and part 10 001 failed after the rest had been sent.
  `newUploader(client, size)` picks the part size through `uploadPartSize` to
  fit 10 000 parts (`UploadFile`, and through it `UploadTargets`; `CrossCopy`).
- **Every multipart copy failed under `checksum_algo`.** `UploadPartCopy`
  cannot send a part checksum, so MinIO refused each part and AWS the
  completion. Multipart copies no longer declare an algorithm, so a copy above
  the threshold carries no additional checksum (see *Large copies*);
  single-request copies now do send the profile's.
- **Verify called every multipart object with a checksum a MISMATCH.**
  `GetObjectAttributes` returns the checksum-of-parts without the `-N` suffix,
  and the code looked for `-` in a base64 value. Composites are now recognised
  from the multipart ETag (`isMultipartETag`). SSE-KMS ETags, which are not an
  MD5 of the content, are no longer compared either (`ruleOutKMS`,
  `ChecksumInfo.KMSETag` — one extra HEAD, only when the ETag is all that is
  left), and `VerifyLocalFile` says why.
- **Single-request server-side copies sent no encryption.** Copy, move,
  rename, paste, trash and restore, metadata save, storage-class change,
  version restore and remote→remote sync all took the destination bucket's
  default, so a bucket policy requiring SSE refused them, and an SSE-KMS object
  rewritten in place was re-encrypted under the bucket default.
  `copyEncryption` now decides it on both paths: the profile's SSE always; without one, a same-bucket copy keeps the
  source's SSE and KMS key when the source was HEADed (`ObjectMeta` gained
  `SSE`, `SSEKMSKeyID`); a cross-bucket copy, or one sized from a listing or a
  plan without a HEAD, gets the destination bucket's default.
- **Replace-copies dropped three headers.** A metadata save and every
  multipart copy lost Content-Language, Expires and WebsiteRedirectLocation.
  `ObjectMeta` carries them and both paths send them.
- **A move cleared marks in the wrong pane.** Its bookkeeping (`OnMoved`) read
  `c.pane()` on the operation's goroutine — a data race, and after a Tab or a
  cross-pane paste it cleared the marks of whichever pane was active. The
  source pane is captured on the UI goroutine (`clipboard.pane`,
  `runCopyOrMove`'s `srcPane`); rename and batch rename capture theirs too.
- **A Tab before the copy/move dialog appeared changed its source.** The form
  appears after an asynchronous `ListBuckets`, and its handler read the source
  bucket from whatever pane was active by then — a move then copied and
  deleted same-named keys in the other pane's bucket. The source pane, bucket
  and scope are captured before the goroutine.
- **Listings landed in whichever pane was active when they returned.** A Tab
  mid-listing stored pane A's objects in pane B, where a delete resolved them
  against B's bucket. `updateList` captures the pane, `makeObjectMap` and
  `makeLocalObjectMap` take it, and `renderIfActive` draws only if it is still
  on screen (`swapAndFocus` re-fetches it on the way back).
- **Nothing could be marked in a local pane**: Space and Ctrl+S were gated on
  a bucket, so the documented multi-file upload was impossible. `canMark()`.
- **The keyboard froze after cancelling an undo or a batch rename during its
  last item.** The report focused a modal that Cancel had already closed, and
  tview delivered no key to anything after that. The report closures check
  `lay.IsOpen()`.
- **Retry callbacks built UI off the UI goroutine** (`go retry(items)`, for
  delete and batch rename), racing tview and `c.active`. Called inline.
- **Undo could replay against another profile.** A backgrounded move that
  finished after Ctrl+P recorded an entry that the next profile's `u` replayed
  against same-named keys. `undoOp` records its model, `Undo` refuses ("Nothing
  to undo in this profile") when it differs, `runUndo` uses it, and finishing
  an undo clears only its own entry (`consumeUndo`), not one a move recorded
  meanwhile.
- **The `~/.aws` importer misread two common shapes.** `[prod] # comment` was
  not recognised as a header, so prod's keys merged into the section above —
  importing *that* profile then authenticated as prod — and inline `#`/`;`
  comments stayed in values. A header now ends at the first `]`, and
  `stripInlineComment` drops a whitespace-preceded `#` or `;`.

Smaller ones:

- A cancelled sync or copy is reported as cancelled, not as failures whose
  "Retry failed" covered only the units in flight and silently dropped the
  undispatched ones and the deletes.
- A download where nothing survives phase 1 no longer queues for a transfer
  slot.
- Sync validates its exclude patterns (an invalid `re:` matched nothing, so a
  delete-extraneous run could delete the path it was meant to protect),
  expands `~` in the local directory, drops the profile's trash prefix from
  both remote sides (a delete-extraneous sync of the bucket root permanently
  deleted the trash), copies remote→remote with `CopyObjectSized` and the
  planned size (no HEAD per object), and no longer creates directories before
  `SafeLocalPath` has vetted a key (a `../` key made directories outside the
  local root).
- Pane comparison (`=`) compares names and sizes only, so it is symmetric: a
  same-size file used to differ only when the *left* copy was newer.
- The duplicate finder's `d` goes through the trash on safe-delete profiles
  (its confirm says which), and blocks the list behind a wait modal while it
  runs — a second `d` could delete the last copy while the dialog said "1
  other copy remains".
- Trash keys keep a leading `/` (`/abs/file` used to restore to `abs/file`).
- The editor's save guard fails closed when the HEAD before saving fails.
- A copy/move retry is keyed by the item, not its display name, which a file
  `docs` and a folder `docs/` share.
- Restoring a profile whose remembered bucket is gone lists the bucket list
  instead of leaving the profile rows on screen.
- Preview, the hexdump gutter and the version diff escape with `tview.Escape`;
  doubling every `[` was not an escape.
- Saving profile options keeps the cursor on the edited profile.
- Esc in the filter re-renders, so the list no longer stays filtered.
- The keymap parser keeps Alt in `Ctrl+Alt+<letter>`, refuses `Ctrl+`/`Shift+`
  on a named key and `Shift+` on a character with no upper case, reads
  `Shift+s` as `S`, and warns when a binding takes another action's chord
  (see *Keymap*).
- A glob `*` matches a key that contains a literal `*`.
- The leader key restores the transfer badge instead of blanking the status
  line.
- Rename with exactly one marked item renames that item, not the highlighted
  one, and clears the old mark.
- A backgrounded cut-paste clears only its own clipboard fill
  (`clipboard.gen`), not one yanked while it ran.
- `SafeLocalPath` decides containment with `filepath.Rel`, so downloading into
  `.` or `/` works (every key used to be rejected).
- Download temp files are unique per transfer, so two keys that land on one
  local path no longer interleave into one corrupted file reported as a
  success.
- `VersionFileName` treats `\` as a separator, so a key like `..\..\x.bat`
  cannot name a file outside the download directory on Windows.

Each has a unit test. The integration suite gained three — `UploadTargets`
sends exactly its list, a multipart upload with a checksum is not a mismatch,
and a multipart copy works under `checksum_algo` — and passes against MinIO
(`alpine/minio:RELEASE.2025-10-15T17-29-55Z`), where the SSE test skips for
want of a KMS.

### Documentation that had stopped describing the code

Some of the drift was renamed symbols again; most of it was claims about
behaviour — of tview, of the SDK, of S3 — that read plausibly and had never
been checked against the code or the dependency underneath:

- **Concurrency.** The goroutine table put every `SetChangedFunc` callback on
  the UI goroutine; tview runs a `TextView`'s on a goroutine of its own. The
  mutex inventory ended "anything not listed here is confined to one
  goroutine", which was false (`c.active`, `c.panes`, the `c.view.*` pointers
  and `c.model` are read from background goroutines, and three worker-local
  locks were unlisted). The throttle was "one global cap"; it is one per
  `*model.Model`. And the UI-update rule said nothing calls `QueueUpdateDraw`,
  which the shell's own pump does.
- **Integrity.** It promised that `checksum_algo` makes multipart objects
  verifiable. A multipart upload's checksum is a checksum-of-parts, which no
  whole-file hash matches — the reason the MISMATCH bug above existed — so the
  section, the duplicate finder's "closeable blind spot" and the verification
  row in *Known limitations* now say what a checksum does and does not buy.
- **Delegated credentials** "caught in `GetConfig`": with the pinned SDK that
  check cannot fire. **Region handling** described `RefreshClient` as
  AWS-only; it runs on every bucket entry for every endpoint, and its region
  is what `CreateBucket` uses.
- **Overwrite confirmation** said every remote write is checked; in-place
  rewrites and undo are not. **Streamed listings** said the context plumbing
  closed the cancellation item and that the remaining bucket-level calls are
  one round trip each; the overwrite pre-scan is still uncancellable, and
  `BucketConfig` makes four. **The failure ledger** listed five flows using
  `opResult`; it backs three.
- **The trash** "keeps its storage class"; it lands in STANDARD. **The
  palette** was said to be generated from the keymap; it is a hand-written
  list. **`make check`** does not run staticcheck. **Restore status** is shown
  by `m` and `c`, not Ctrl+L. The upload-retry row explained its safety by a
  re-read from the file that the uploader does not do (it buffers each part).
- **Stale names**, found by the same grep as last time: `transferJob`,
  `jobSnapshot`, `transferRow`, `jobSem`, `finalizeJob`, `histStack`,
  `localPaneScope`, `localDownloadPath`, `showProgress`, `c.filter` and
  `view.Field*`, plus the `modal-dups` page name and the 80x24 test screen
  (it is 80x20). The `ListObjectsStream` integration test was described as
  pinning paging and the early stop; with 12 keys in one page it can only fail
  on cancellation.

## Fixed in the 0.10.2 deep review

A second read-through of the whole tree, after 0.10.1's. The first four are
failures a user can hit — one of them fatal to the process; the rest are
places where the code and this document had stopped agreeing about what the
code does.

- **Properties over a local file crashed the app.** `ShowFileProperties` read
  `*c.pane().Bucket().Key` — and then the object's `Size` and `Etag` — without
  a `remoteOnly` guard. A local pane holds `model.File` objects but no bucket,
  no ETag and no S3 link, so `Ctrl+L` (or *Properties* in the palette) over any
  file in a local pane dereferenced nil and took the whole TUI down, losing
  every in-flight background transfer with it. Every other object action had
  the guard; this one and `PresignLink` were the two that did not. Both now
  have it, the optional fields are no longer dereferenced blind, and a
  regression test drives the local-pane path (it panics against the old code).
- **The batch-rename progress counter was a data race.** The loop wrote
  `n, label := i+1, op.label`, queued a closure reading `n`, and then wrote
  `n, err := mdl.MoveKeys(...)` — which *assigns* to the same `n`, since only
  `err` is new in that scope. The UI goroutine therefore read a variable the
  worker was still writing, and displayed the object count the move returned
  instead of the item index. Renamed apart, which is the whole fix; the
  compiler caught a second shadowing of the outer `moved` ledger on the way.
- **A version diff could exhaust memory.** See *Preview and version diff*: the
  LCS table is quadratic in lines, not bytes, so the `editMaxSize` cap on the
  bodies did not bound it. `maxDiffCells` does.
- **Folder markers written by an upload skipped the profile's write options.**
  This document said `WriteOptions` is "applied at every creation point …
  folder markers" and that "folder markers are encrypted but not typed".
  `CreateFolder` did that; the marker `PutObject` inside `Model.Upload` — the
  one that represents an empty local directory — did not. On a bucket policy
  requiring SSE, uploading a tree containing an empty directory failed with a
  policy error naming a key the user never typed. The doc described the
  intent; the code now matches it.
- **A listing that finished late could unregister the one that replaced it.**
  `listingState.end` took a `context.Context` and cleared `l.cancel`
  unconditionally, despite a comment claiming the argument was what kept it
  from retiring a newer listing. It never checked identity, so the newer
  listing's cancel func was dropped and `Esc` had nothing to cancel. Only
  `refreshMu`'s serialisation kept this unreachable. `begin` now returns a
  generation token that `end` must quote.
- **Renaming a profile could duplicate a name.** Creating and copying both
  refuse a name already in use, because the details pane and the options form
  look profiles up *by name*; the edit form did not check, so renaming one
  profile onto another's name made every name-keyed lookup resolve to the
  first of the two.
- **The controller still carried the sync scheduler the refactor had moved.**
  `splitSyncPhases`, `syncWorkerCount`, `indexedOp` and `syncWorkers` lived on
  in `internal/controller/sync.go` with tests of their own, duplicating
  `ops.SplitSyncPhases` / `ops.SyncWorkerCount` — which is what actually runs.
  Two copies of a writes-before-deletes barrier, one of them dead and both
  tested, is exactly the drift the refactor existed to remove. Deleted, along
  with `getPosition` (a lookup that reported "not found" as index 0).

### Documentation that had stopped describing the code

The guard tests cover the contracts a *user* types — key bindings, action
names, profile fields, the version string — and all of those were in step.
What had drifted is the part only a reader checks: three sections of this
document still described the pre-`ops`, pre-`session` controller.

- **The mutex table listed four locks, two of which no longer exist.**
  `Controller.jobsMu` and the per-download `sumMu` (with `activeProgress` and
  `completedBytes`) went in the `jobs` and `ops` moves; `Controller.mu` was
  described as guarding the object map, the selection sets, the filter and the
  partial flag, all of which belong to `session.Pane` now — it guards
  `sortBy`/`sortDesc` and nothing else. Rewritten as an inventory of owners,
  which is what the refactor actually produced, and extended to the locks that
  were never listed (`activityMu`, `undoMu`, the shell, the ledger).
- **The goroutine table and the UI-update rule contradicted each other** two
  paragraphs apart: the table said results come back via `QueueUpdateDraw`,
  the rule said nothing calls it any more. The rule was right.
- **The download-pipeline section documented symbols that are gone** —
  `toDownload []resolvedItem`, `sum.overwritten`, `showProgress`,
  `showSummary`. The *reasoning* had survived the move to `ops.Download`
  intact (two phases, sequential questions, four workers, temp-file-then-
  rename, verify-counts-as-failure), so it was re-anchored to the names that
  run, and gained the two invariants the move added: phase 1 runs outside the
  transfer gate, and the context is checked before the dispatch `select`
  because a ready channel and a dead context make `select` a coin flip.

Worth noting what this says about the guards: a test can pin a string a user
types, but not a paragraph's claim about structure. The cheap check that found
all three was mechanical — grep every identifier the docs name and see whether
it still exists in the tree — and the half of it that generalises is now
`TestDocsOnlyNameFilesThatExist`: every backticked `internal/…` path in the
four documents has to resolve. It cannot see a stale *claim*, only a stale
*pointer*, but a renamed file is how most stale claims begin.

### Reviewed and deliberately left alone

- **`progressReader.Read` is still non-atomic**, unlike `progressWriterAt`. It
  is only ever read by one goroutine per transfer (the uploader reads a part
  at a time), so there is no race to fix — it stays on REFACTOR.md's open list
  as a consistency item, not a defect.
- **The transfers panel's ticker outlives a layer closed by anything but
  `Esc`.** Nothing closes it another way today; worth remembering if a second
  close path appears.
- **`searchMatch` is exercised only by tests.** Unlike `getPosition` it states
  the search predicate in one place and is the thing the test is about, so it
  stays.

## Fixed in the 0.10.1 deep review

A read-through of the whole tree. Every finding below was reproduced by a
failing test before it was fixed, and the test was re-run against the old code
to confirm it actually caught the defect.

- **Empty Trash was a silent no-op.** Its confirmation handler compared the
  pressed button against `"ok"` while `NewConfirm` offers `"OK"`, so the
  comparison never matched and the handler always took the early return. The
  dialog sized the trash, promised to remove N objects, closed on OK, and
  removed nothing — the objects kept costing storage while the user believed
  they were gone. The label was a magic string retyped at nine call sites; it
  is now `widgets.ConfirmOK`, and a source-scanning test rejects any new
  literal comparison. Note this *activated* a destructive path that had never
  once run.
- **Deleting a profile could panic the TUI.** `Params.DeleteConfig` sliced with
  an unchecked index, and it dropped the entry from the in-memory list *before*
  saving — so a failed write (read-only home, full disk) left the slice one
  shorter than the list on screen. Every profile action addresses a profile by
  row, so the next edit or delete indexed past the end. The index is now
  checked and the new list is only adopted once the save succeeded; the
  remaining row-addressed actions go through a `configAt` guard.
- **Duplicating a profile shared its bookmarks with the original.** The copy is
  made by value, so both `Bookmarks` slices pointed at one backing array. Each
  profile appended into the same spare capacity and the second write silently
  overwrote the first profile's new bookmark — both slices kept the right
  length, so nothing looked wrong. `CopyConfig` now clones the slice and the
  `Region` pointer.
- **The bookmark manager never closed when a bookmark was picked.** Its rows
  are wired before the overlay is pushed, and `rebuildBookmarkList` took the
  layer handle *by value* — capturing the nil it still held. `Layer.Close` is
  nil-safe, so the miss was silent: the jump happened behind a modal still
  covering the listing. It now takes the handle by address, the same idiom
  `escapeCloses` already used.
- **Retrying a partly-failed move replayed the original run's bookkeeping.**
  `ops.Copy.withRetry` copied the operation wholesale, so the retry inherited
  `OnMoved` — which is not only the undo ledger but also consumes the cut
  clipboard and clears the marks in the source scope. A user who retried a
  backgrounded paste after yanking something else had that new clipboard wiped.
  The callback is now cleared, which is what its own comment had always
  claimed. The cost, stated there: a retry's moves are not added to the
  one-step undo.
- **Profile-list mutations raced the UI.** Copy and delete rewrote
  `c.params.Config` from a background goroutine while every profiles-screen
  handler read it. Both now run inline on the UI goroutine the done handler is
  already on — which is what every other `WriteConfig` call site does, a save
  being one small local file.
- **Esc stopped closing ten dialogs.** Found while auditing REFACTOR.md's
  "226 raw page calls → 0" claim, which had only ever been counted over
  `internal/controller`. Every dialog constructor in `internal/ui/widgets`
  still carried an Esc handler calling `Pages.RemovePage("modal")` — a page
  name from before overlays were handles. The shell names every overlay
  `layer-N`, so the call removed nothing and Esc did nothing in the profile
  forms, rename, search, batch rename, copy/move, sync, metadata, storage
  class and create dialogs. Each stayed usable through its Cancel button, so
  nothing failed; only a user pressing Esc would notice. The widgets now
  install no key handling at all — overlay lifetime belongs to whoever pushed
  the layer — and the controller wires `escapeCloses` at each call site. The
  copy/move dialog turned out never to have had Esc wired at all, which the
  new guard caught rather than the audit.

Test coverage added alongside: the integration suite gained a controller half
(`internal/controller/integration_test.go`) covering the flows the app
*composes* rather than the calls it makes — a full sync in both directions
including delete-extraneous and the exclude guard, and safe delete's round trip
through the trash. The model half gained the browsing and housekeeping calls
that had no server-backed test at all: the recursive prefix delete, the
delimited listing navigation is built on, folder markers, download resolution,
presigned links, incomplete-upload cleanup, bucket config, public-bucket
policy, and `RefreshClient`'s fail-safe contract. Both halves were verified
re-runnable against a persistent server.

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
  `uploadKey` helper so it cannot drift from `PrepareUpload`'s keys. (It did
  drift anyway, under prefixes holding `//` or `..`; the 2026-10-20 review
  removed the second derivation instead — `Upload` now sends `PrepareUpload`'s
  own targets — and `uploadKey` is gone.)
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
