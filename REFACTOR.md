# S3Duck-TUI — Architecture Refactor

Companion to [DESIGN.md](DESIGN.md). DESIGN.md describes how the app works
today; this file describes what is structurally wrong with it, what it should
become, and the order in which to get there.

Written against the tree at `40842e9` (streamed listings, #18), which builds,
vets and tests clean.

---

## 1. Diagnosis

The codebase is carefully written — real concurrency reasoning, honest
limitation tracking, disciplined extraction of pure functions. The problem is
not the code inside the container. It is the container.

### Measurements

| Metric | Value |
|---|---|
| `pkg/controller/controller.go` | 5,648 lines |
| Methods on `*Controller` | 201 |
| Fields on `Controller` | 44 |
| Functions in `pkg/controller` | 540 |
| Longest function (`runDownload`) | 449 lines (`controller.go:1002–1449`) |
| Raw `Pages.*Page` calls | 226, over 21 magic page names (`"progress"` ×72, `"modal"` ×61) |
| `QueueUpdateDraw` call sites | 92 |
| `go func` in `pkg/controller` | 55 |
| Named or inline interfaces in the whole repo | **0** |

### What is already right

The refactor must preserve these, not disturb them:

- `pkg/view` is genuinely business-logic-free. It stays as-is (it only moves).
- `pkg/model` is a sane SDK wrapper, already split across ten focused files.
- The pure-function extraction (`planSync`, `filterSortObjects`,
  `deleteConfirmText`, `transferBadge`, `planCopyParts`, `ParseRestoreStatus`)
  is disciplined and yields 247 real unit tests.
- `listing.go`, `opresult.go`, `notify.go` and `guard.go` are the right size and
  shape. **They are the template for everything below.**
- The integration suite tests properties that genuinely require a live server.
- CI already runs `go test -race`.

### The six structural problems

#### P1 — God object

`Controller` carries 44 fields spanning seven unrelated concerns: pane and
location state, selection, filter/sort, keymap and leader state, modal
bookkeeping, the transfer queue, clipboard/undo, the activity log and the render
cache. 201 methods hang off it.

Every feature touches the same struct, so every feature is coupled to every
other feature. This is why `controller.go` keeps growing even as files are split
off it: the files leave, but they all still reach back into the same 44 fields.

#### P2 — No seam at the S3 boundary

There are zero interfaces in the repository. `Model` is a concrete struct
holding a live `*s3.Client`, and `Controller` depends on it concretely.

DESIGN.md records the consequence for the UI ("the UI layer has no seam and is
not unit-tested"), but the more serious casualty is the **orchestration**: retry
policy, skip handling, overwrite decisions, partial-failure accounting,
cancellation draining, worker-pool phase barriers. That is the logic most likely
to be wrong, and it is covered only by a tag-gated suite that needs a live
MinIO.

This also defangs the `-race` job: CI runs it, but the racy code (P5) lives in
goroutines no test can reach. `-race` only finds what tests execute.

#### P3 — Ad-hoc modal lifecycle

226 raw page calls over 21 magic names. `Pages.AddPage` *replaces* a page of the
same name, a footgun the codebase has already been bitten by (hence `"modal-msg"`
splitting out from `"modal"`). The scar tissue is in the comments:

> `// Removing "progress" here would leave showSummary() focusing a detached modal and soft-lock the UI.`

Twelve independent sites hand-manage a page called `"progress"`, each with
slightly different teardown. `runUpload` removes the page on Cancel
(`controller.go:4262`); `runDownload` deliberately does not
(`controller.go:1038`). That divergence is load-bearing and invisible at the
call site.

#### P4 — The UI-goroutine contract is a convention, not a type

In the pinned tview version, `QueueUpdate` is:

```go
ch := make(chan struct{})
a.updates <- queuedUpdate{f: f, done: ch}
<-ch   // blocks until the event loop runs f
```

So calling `c.error` from the UI goroutine is a **guaranteed hard deadlock**,
not a probabilistic one. The codebase manages this by convention: ~75 call sites
write `go c.error(...)`, 76 write `c.error(...)` bare, and the author must know
which goroutine they are on at every one. DESIGN.md devotes a full paragraph to
the trap. Documentation is the wrong tool for an invariant that freezes the app.

#### P5 — Pane state mirroring causes a real data race

The active pane's state lives in `Controller`'s own fields; the inactive pane
lives in `panes[1]`. `swapPane` copies twelve fields each way under `c.mu`. But
navigation goroutines write those same fields with no lock at all:

```go
// controller.go:1858-1864, inside go func()
c.currentBucket = bucket
if err := c.model.RefreshClient(&name); err != nil { ... }
c.restoreNext = ".."
```

This is the "navigation race" in DESIGN.md's limitations table. The cause is
structural: the code *mirrors* state instead of *addressing* it.

#### P6 — Four copies of one transfer algorithm

`runDownload` (449 lines), `runCrossCopy` (226), `runSync` (203) and `runUpload`
(124) each implement the same pipeline:

> confirm → `addJob` → progress modal with Background/Cancel → goroutine →
> `jobSem` → 100 ms-throttled progress → `finalizeJob` → summary/retry

Four `addJob` sites, 15 `finalizeJob` sites, four copies of the Background/Cancel
modal, four throttle implementations — each subtly different.

---

## 2. The claim this refactor rejects

DESIGN.md justifies the god package like this:

> `pkg/controller` is intentionally large because the modal flows — profile CRUD,
> download with overwrite prompts, upload, copy/move, delete, summary — are
> deeply coupled to tview's callback/page model; splitting them would mean
> passing around opaque page/modal handles.

The premise is correct. The conclusion does not follow.

Handles have to be passed around **because an operation asks the user questions
by calling into tview directly**. Invert that: let the operation *emit* a
question and block on a reply channel. The operation then needs no handle, no
`*tview.Modal` and no `*tview.Application`. One tview-aware runner renders every
question for every operation.

That single inversion is what makes the split possible, and it is the core of
this plan.

---

## 3. Target architecture

```
cmd/s3duck-tui/
internal/
  config/          profiles, keymap persistence         (as-is)
  s3/              ← pkg/model, unchanged shape
  session/         Pane, Location, Selection, History   — pure state, no tview
  ops/             Operation + Download/Upload/Copy/Sync/Rename/Delete ops
                   depends on narrow S3 ports + Reporter. ZERO tview.
  jobs/            Job, Queue, status, badge            (notify.go moves here)
  ui/
    shell/         Shell: layer stack, toasts, Do()     — replaces 226 page calls
    widgets/       ← pkg/view, unchanged
    screens/       profiles, browser
    flows/         thin glue: Runner, dialogs, pickers
```

Rough end state: `ops` ≈ 2,000 lines and ~90 % unit-testable; `ui/flows` ≈ 1,200
lines of genuinely tview-coupled glue; nothing over ~600 lines.

The organising principle is **not** MVC purity. It is the testability seam:

| Layer | Depends on | Testable by |
|---|---|---|
| `session`, `jobs` | nothing | plain unit tests |
| `ops` | narrow S3 ports, `Reporter` | fake store + recording reporter |
| `ui/*` | tview | by hand / pty harness (now `e2e/`: the binary in tmux) |

### A. `ops.Operation` — kills P2 and P6

```go
package ops

type Progress struct {
	Bytes, TotalBytes int64
	Items, TotalItems int
	Active            []string
}

// Question is asked by an operation and answered by whoever drives it. The op
// never holds a widget; the runner renders this.
type Question struct {
	Kind   QKind // QOverwrite, QRetry, QConfirm
	Path   string
	Detail string
}

type Reporter interface {
	Progress(Progress)
	Ask(Question) Answer // blocks; the runner replies
	Log(format string, args ...any)
}

type Result struct {
	Done, Skipped int
	Failures      []Failure
	Canceled      bool
	Retry         Operation // nil when there is nothing to retry
}

type Operation interface {
	Describe() Descriptor // kind, title, confirm text, totals
	Run(ctx context.Context, rep Reporter) Result
}
```

The S3 dependency is **narrow interfaces defined in `ops`** — Go-style,
consumer-side — not one fat `Store`:

```go
type Lister interface {
	ListPage(ctx context.Context, b Bucket, prefix string, onPage func([]Entry) bool) error
}
type Fetcher interface {
	Fetch(ctx context.Context, ref ObjectRef, dst io.WriterAt, prog func(int64)) error
}
type Putter interface {
	Put(ctx context.Context, ref ObjectRef, src io.Reader, m Meta, prog func(int64)) error
}
```

`*s3.Model` satisfies all three with no changes. A `fakeStore` in
`ops/fake_test.go` then makes the whole download pipeline — two-phase resolve,
overwrite decisions, the 4-worker pool, cancel draining, retry-target
collection, verification — testable in milliseconds with no network. **This is
the single biggest win available in this codebase.**

### B. One runner — the 4× duplication collapses

```go
// ui/flows/runner.go
func (r *Runner) Start(op ops.Operation) {
	d := op.Describe()
	r.shell.Confirm(d.ConfirmText, func() {
		ctx, cancel := context.WithCancel(context.Background())
		job := r.jobs.Add(d.Kind, d.Title, d.TotalBytes, d.TotalItems, cancel)
		h := r.shell.Push(progressLayer(d.Title, job, cancel))
		go func() {
			res := op.Run(ctx, &uiReporter{shell: r.shell, job: job, layer: h})
			r.jobs.Finish(job, res)
			r.shell.Do(func() { h.ShowSummary(res, r.Start) }) // Retry re-enters here
		}()
	})
}
```

Written once. `runDownload`/`runUpload`/`runCopyOrMove`/`runSync` become
`DownloadOp`/`UploadOp`/`CopyOp`/`SyncOp` constructors. Background/Cancel
semantics get decided in exactly one place instead of diverging across four.

`uiReporter.Ask` is where the handle-passing objection dissolves:

```go
func (r *uiReporter) Ask(q ops.Question) ops.Answer {
	reply := make(chan ops.Answer, 1)
	r.shell.Do(func() { r.shell.Push(questionLayer(q, reply)) })
	return <-reply
}
```

The op blocks on a channel and never sees tview. This also opens the door to
fixing the "sequential per-file overwrite" limitation: the op can batch
questions, because asking is now a value rather than a call stack.

### C. `ui/shell.Shell` — kills P3 and P4

```go
type Handle interface{ Close() }

func (s *Shell) Push(l Layer) Handle // auto-generated unique name; no magic strings
func (s *Shell) Toast(kind Kind, header, body string)
func (s *Shell) Do(f func())         // safe from ANY goroutine, never blocks
```

Two things must be right:

**`Push` returns a `Handle`.** Nothing can accidentally replace someone else's
page, and `Close` pops exactly its own layer and restores focus to the one
beneath. The whole soft-lock class goes away, including the
`showSummary`-focusing-a-detached-modal hazard.

**`Do` never blocks, from any goroutine.** The Shell owns an unbounded pending
slice and a pump goroutine; `Do` appends and signals, the pump drains a batch
and forwards it to `app.QueueUpdateDraw`:

```go
func (s *Shell) Do(f func()) {
	s.mu.Lock()
	s.pending = append(s.pending, f)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
```

Because the pump is never the UI goroutine, `QueueUpdate`'s `<-ch` can always
complete. Callers stop needing to know which goroutine they are on, and
`go c.error(...)` versus `c.error(...)` stops being a decision — the paragraph in
DESIGN.md goes away along with the hazard. Batching is a free bonus: several
updates coalesce into one draw.

**Every** UI mutation routes through `Do`, so the single FIFO preserves the
ordering that 92 separate `QueueUpdateDraw` calls used to get by blocking.

### D. `session.Pane` — kills P5

```go
type Pane struct {
	mu      sync.Mutex
	loc     Location // bucket + prefix, or localDir
	objs    map[string]*s3.Object
	sel     Selection
	hist    History
	filter  string
	partial bool
}

func (p *Pane) Snapshot() PaneView   // one lock, immutable view for rendering
func (p *Pane) Navigate(to Location) // atomic; what Down/Up/jumpTo call
```

`Controller` holds `panes [2]*session.Pane` plus `active int`. `swapPane`
becomes `c.active ^= 1` — the twelve-field copy disappears, and with it the
possibility of a background goroutine writing a field a swap is concurrently
moving. `Down`'s goroutine captures its `*Pane` before launching, so a Tab
mid-navigation can no longer land the result in the wrong pane.

This also unblocks the "inactive pane keeps its previous column layout"
limitation: with a `*Pane` to address, `renderList` can take one as an argument
instead of always rendering "the active one".

### E. `jobs.Queue`

`transferJob`, `jobStatus`, `jobView`, `addJob`, `finalizeJob`, `jobSnapshot`,
`cancelJobAt`, `clearFinishedJobs`, `jobSem`, `notice`/`noticeUntil` and all of
`notify.go` are already 90 % a cohesive class. Lifting them out removes eight
fields from `Controller` on its own. Rendering (`renderTransfers`,
`ShowTransfers`, `transferRow`) stays behind and moves to `ui/screens` later.

---

## 4. Migration plan

Each phase compiles, passes tests and ships. Nothing here is a big-bang rewrite.

### Phase 1 — `ui/shell`

Build `Shell` with `Push`/`Handle`/`Toast`/`Do`. Port the simplest overlays
first (`modal-about`, `modal-help`, `modal-msg`), then the twelve `"progress"`
sites. Route all 92 `QueueUpdateDraw` calls through `Do` and drop the
`go c.error(...)` convention.

*Payoff:* kills the freeze class (P4) and the page-name collision class (P3).

### Phase 2 — `jobs` package

Pure move: the transfer queue plus `notify.go`.

*Payoff:* −8 fields on `Controller`, −~250 lines from `controller.go`.

### Phase 3 — `session.Pane`

Replace mirroring with addressing; `swapPane` becomes one XOR.

*Payoff:* fixes the navigation race (P5) and unblocks inactive-pane rendering.
−12 fields.

### Phase 4 — `ops` + ports + the Runner

Do **one** operation end to end first. `UploadOp` (124 lines, simplest shape)
proves the pattern; `DownloadOp` (449 lines) is the biggest payoff; then
Copy/Sync/Rename/Delete.

*Payoff:* orchestration becomes unit-testable (P2); four pipelines become one
(P6).

### Phase 5 — package moves

`pkg/view` → `internal/ui/widgets`, everything else under `internal/`, and the
first `ui/screens` extraction. Cosmetic by this point.

**Deviation from the plan above:** `pkg/model` became `internal/model`, not
`internal/s3`. Naming the package `s3` would collide with the AWS SDK's own
`s3` package, which those very files import — every file in the package would
need an alias to avoid shadowing. The valuable half of the move (marking the
whole tree non-importable) is unaffected.

The `screens`/`flows` split is partial and deliberately so: the transfers panel
moved out whole (it owns its widgets and nothing else wanted them), and the
profiles screen moved to its own file. The rest of `controller.go` is screen
wiring that still shares one `tview.List` between the browser and the profiles
screen — splitting that needs a render path that can target a pane, which is
its own change rather than a move.

**Order matters:** do Phase 4 before Phase 5. Do not shuffle packages while the
hard logic is still untested.

---

## 5. Smaller findings

- **`progressReader.Read` is non-atomic** (`model.go:164`,
  `pr.written += int64(n)`) while its sibling `progressWriterAt.WriteAt` uses
  `atomic.AddInt64`. Currently safe — s3manager reads non-seekable bodies
  sequentially — but it is an undocumented dependency on SDK internals. Make it
  atomic; it costs nothing.
- **Read-only enforcement is 18 scattered `readOnlyBlocked` calls** in the
  controller. A new write path that forgets one silently bypasses the guard on a
  production profile. In the target architecture this belongs in `ops`: a
  read-only `Store` wrapper that rejects `Put`/`Delete`/`Copy`, so forgetting is
  impossible rather than merely unlikely.
- **The integration CI job does not run `-race`** (`ci.yml`, the "integration
  tests" step) — and it is the one suite that actually exercises concurrent
  transfer paths.

## 6. What this refactor deliberately does not do

- Touch `pkg/view`'s contents. It is doing its job; it only moves.
- Introduce a DI framework or an event bus. Constructor wiring in a composition
  root is enough at this size.
- Split `pkg/model` further. Its file layout is already good.
- Chase MVC purity. The useful split is by testability seam, not by MVC letter.

---

## 7. Progress

| Phase | Status | Notes |
|---|---|---|
| 1 — `ui/shell` | done | 226 raw page calls → 0; the `go c.error(...)` rule retired |
| 2 — `jobs` | done | queue, badge and notices lifted off the controller |
| 3 — `session.Pane` | done | navigation race fixed |
| 4 — `ops` + Runner | done | `Download`, `Upload`, `Copy`/`Move` and `Sync` converted; batch rename, delete, undo, trash restore and cross-profile copy not yet |
| 5 — package moves | done | `internal/model`, not `internal/s3` (see above); `screens` split is partial |

### Where it landed

| Metric | Before | After |
|---|---|---|
| `controller.go` | 5,648 lines | 4,253 |
| Fields on `Controller` | 44 | 30 |
| Methods on `*Controller` | 201 | 188 |
| Raw `Pages.*Page` calls | 226 | 2 (both inside `ui/shell`, which owns them) |
| `QueueUpdateDraw` outside the shell | 92 | 0 |
| Interfaces in the repo | 0 | 9 |
| Tests | 247 | 425 |

Recounted after the 2026-10-21 review's fixes; the version is still 0.10.2,
whose release read 4,020 lines, 29 fields and 184 methods in the first three
rows. The growth is the 2026-10 reviews' fixes themselves — panes captured on
the UI goroutine, the undo skip sets, `canMark`, `renderIfActive`, the
clipboard generation, the profile-switch guards on the copy/move and sync
dialogs — not new flows; the ninth interface is `ops.QueueReporter`. Tests
count `func Test…` across `internal/`, the tag-gated integration suite
included; `e2e/` adds ten more (and its `TestMain`).

The page-call row read "0" until 0.10.1, because it was only ever counted over
`internal/controller`. Ten raw `Pages.RemovePage("modal")` calls had been left
behind in `internal/ui/widgets`, in the Esc handlers of the dialog
constructors — written when overlays were addressed by magic string, and never
converted. The shell names every overlay `layer-N`, so those handlers removed a
page that does not exist and **Esc silently stopped closing ten dialogs**. The
forms were still usable through their Cancel buttons, so nothing failed and
nothing in the suite noticed. Fixed in 0.10.1: no form or dialog constructor in
the widgets installs key handling (overlay lifetime belongs to whoever pushed
the layer; the hotkey panel, which takes an `onClose`, is the one exception),
the controller wires `escapeCloses` at each call site, and two tests now pin
both halves of that boundary — one per package, so neither side can drift
alone.

Phase 4 left a similar residue, found in 0.10.2. Converting sync to
`ops.Sync` moved its scheduler into `internal/ops`, but the controller's copy —
`splitSyncPhases`, `syncWorkerCount`, `indexedOp`, `syncWorkers` — was never
deleted, and kept its tests. Two implementations of the writes-before-deletes
barrier, one of them dead and both green, is the exact failure mode a "move the
logic out" refactor is supposed to end: a later fix to the barrier would have
gone into whichever copy the author found first. Deleted. Worth a rule for the
remaining conversions: when a flow becomes an `ops.Operation`, its old
scheduling helpers go in the same commit, and so do their tests — a test is
what makes dead code look alive.

The 2026-10 review found a third, and this one lost data. `ops.Upload` was
handed the confirmed file list, but its port was `Pusher.Upload(ctx,
localPath, prefix, …)`: what it actually sent was a *root*, which the model
walked for itself. From the upload dialog the two nearly agreed, because the
root was what the user picked (only a prefix containing `//` or `..`, which
the walk's `path.Join` cleaned, wrote other keys than the preview showed).
From the local pane they did not — the root was the directory the marked
files sat in, so uploading one marked file from `~` uploaded all of `~`, past
an overwrite scan that had only seen the marked file's key. The port is now
`Pusher.UploadTargets(ctx, bucket, targets, progress)`: exact targets, no
walk, and `ops.Upload` sends exactly its `Files`. `model.Upload` became
`PrepareUpload` plus `UploadTargets`, so its one remaining direct caller (the
empty-directory upload) also writes, by construction, the keys a preview
would show. The rule this adds to the one above: a port takes what the user
confirmed, never an input it re-derives that from.

### Where the names differ from §3

§3 sketched the target before any of it existed, and the code chose other
names (or another shape) in a few places; §3 is left as it was written. The S3
ports are `Fetcher`, `Pusher`, `Checker`, `FS` and `Gate` (`internal/ops/ports.go`)
plus `Mover` (`internal/ops/copy.go`) and `Opener` (`internal/ops/archive.go`),
not `Lister`/`Putter`. The operations are `ops.Download`, `ops.Upload`,
`ops.Copy`, `ops.Sync` and `ops.Archive`, not `DownloadOp` and friends. The shell hands back a `*shell.Layer`, not a `Handle`, and
`shell.Toast` takes a primitive rather than (kind, header, body). A pane is
entered with `Pane.Enter(bucket, path, restore)`, not `Navigate`, and
`Pane.Snapshot` returns `[]*model.Object` — there is no `PaneView`. The keymap
lives in `internal/controller/keymap.go`, not under `config/`; and `session`
depends on `internal/model` (for the object type), not on nothing.

### What is still open

- **Five flows are converted; five still open-code the pipeline.**
  `Download`, `Upload`, `Copy`/`Move`, `Sync` and `Archive` are
  `ops.Operation`s — `Archive` was written as one from the start, which is the
  shape working as intended: it got the confirmation, the queue, Background,
  Cancel and the failure ledger without adding a line of UI code. Still
  open-coding what the Runner owns: `runCrossCopy` (221 lines),
  `runBatchRename` (84), `runDelete` (77), `runUndo` (67) and
  `runTrashRestore` (34). The pattern is established — each is a
  `Describe`/`Run` pair plus a ten-line constructor — and `runUndo`,
  `runBatchRename` and `runTrashRestore` are all a loop of `MoveKeys` over
  explicit source→destination pairs: `ops.Copy` with per-item destinations and
  a different label, so they should collapse into one op rather than grow
  three types. `runCrossCopy` is the one that needs its own op: it streams
  bytes between two endpoints rather than asking one server to copy. The
  open-coded flows are also where the failure reporting is thinnest (no Retry
  or Export on cross-profile copy or undo — ROADMAP.md, Hardening).
- **`ui/screens` holds one screen.** The browser and the profiles screen still
  share one `tview.List` (what `c.browsing` distinguishes); separating them
  needs a render path that can target a pane rather than always "the active
  one" — which would also fix the inactive-pane column-layout limitation.
- **The read-only guard is still 18 scattered `readOnlyBlocked` calls** in the
  controller, plus one inline check in `pickCrossDestination`
  (`internal/controller/crossprofile.go`) — the one write whose guard is about
  the *destination* profile rather than the open one. With `ops` in place it
  now has a home: a read-only wrapper around the ports that rejects
  `Put`/`Delete`/`Copy`, so forgetting one becomes impossible rather than
  merely unlikely.
- **`progressReader.Read` is still non-atomic** (`internal/model/model.go`),
  unlike its `progressWriterAt` sibling. Reviewed in 0.10.2 and left: one
  goroutine reads a given reader (the uploader takes a part at a time), so
  this is a consistency wart rather than a race.
- **The integration CI job still does not run `-race`.**

See the commit history on `refactor/architecture` for the per-phase diffs.
