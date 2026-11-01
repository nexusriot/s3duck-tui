package ops

import (
	"context"
	"fmt"
	"strings"

	"github.com/dustin/go-humanize"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

// KindPurge is a permanent, version-by-version delete.
const KindPurge Kind = "purge"

// VersionDeleter removes stored versions by ID. It is model.Model.DeleteVersions.
type VersionDeleter interface {
	DeleteVersions(ctx context.Context, bucket *model.Object, refs []model.VersionRef) (int, []model.VersionFailure, error)
}

// purgeChunk is how many versions one progress step removes. DeleteVersions
// batches 1000 to a request on its own; the smaller step is only so a large
// purge shows movement and answers Cancel between requests.
const purgeChunk = 250

// Purge permanently deletes an already-planned list of versions and delete
// markers. Planning (the listing, the choice between all versions and old
// ones) happens before it, because the confirmation has to say exactly what
// will go — counts and bytes — and nothing else is removed.
type Purge struct {
	Bucket *model.Object
	Refs   []model.VersionRef
	// Mode is what was asked for; it only shapes the wording.
	Mode model.PurgeMode
	// What names the selection for the job label ("3 item(s)", "docs/").
	What    string
	Deleter VersionDeleter
}

func (p *Purge) Describe() Descriptor {
	return Descriptor{
		Kind:         KindPurge,
		Title:        fmt.Sprintf("%s in %s", p.What, bucketLabel(p.Bucket)),
		ConfirmText:  PurgeConfirmText(bucketLabel(p.Bucket), p.What, p.Mode, model.SummarizePurge(p.Refs)),
		TotalItems:   len(p.Refs),
		RefreshAfter: true,
	}
}

// bucketLabel is the bucket's name for display.
func bucketLabel(b *model.Object) string {
	if b == nil || b.Key == nil {
		return ""
	}
	return *b.Key
}

// PurgeConfirmText is the confirmation: what goes, how much, and that it is
// final. A purge is the one delete no trash, undo or version history can
// reverse, so the text says so in as many words, whatever the profile's
// safe-delete setting.
func PurgeConfirmText(bucket, what string, mode model.PurgeMode, s model.PurgeSummary) string {
	var b strings.Builder
	switch mode {
	case model.PurgeNoncurrent:
		fmt.Fprintf(&b, "Delete the OLD versions of %s in %s?\n\n", what, bucket)
		b.WriteString("What each key reads as now is kept; its history is not.\n\n")
	default:
		fmt.Fprintf(&b, "Delete EVERY version of %s in %s?\n\n", what, bucket)
		b.WriteString("The keys are gone afterwards, with no history to restore from.\n\n")
	}
	fmt.Fprintf(&b, "%d key(s): %d version(s), %s, and %d delete marker(s).\n\n",
		s.Keys, s.Versions, humanize.IBytes(uint64(s.Bytes)), s.Markers)
	b.WriteString("This is permanent: no trash, no undo, no version to go back to.")
	return b.String()
}

func (p *Purge) Run(ctx context.Context, rep Reporter) Result {
	led := &ledger{}
	var failed []model.VersionRef
	done := 0
	for i := 0; i < len(p.Refs); i += purgeChunk {
		if ctx.Err() != nil {
			led.cancel()
			break
		}
		end := i + purgeChunk
		if end > len(p.Refs) {
			end = len(p.Refs)
		}
		chunk := p.Refs[i:end]
		rep.Progress(Progress{
			Items: done, TotalItems: len(p.Refs), ByObject: true,
			Active: []string{chunk[0].Key},
		})
		n, failures, err := p.Deleter.DeleteVersions(ctx, p.Bucket, chunk)
		for j := 0; j < n; j++ {
			led.ok(0, false)
		}
		done += n
		for _, f := range failures {
			led.fail(versionLabel(f.Ref), f.Err)
			failed = append(failed, f.Ref)
		}
		if err != nil {
			if ctx.Err() != nil {
				led.cancel()
				break
			}
			// The whole request failed: every version in it not already
			// counted stays, and is retryable.
			for _, r := range chunk[n+len(failures):] {
				led.fail(versionLabel(r), err)
				failed = append(failed, r)
			}
		}
	}
	rep.Progress(Progress{Items: done, TotalItems: len(p.Refs), ByObject: true})
	res := led.result()
	rep.Log("purge %s in %s: %d version(s) deleted, %d failed", p.What, bucketLabel(p.Bucket), res.Done, res.Failed())
	if len(failed) > 0 {
		next := *p
		next.Refs = failed
		next.What = fmt.Sprintf("%d failed version(s)", len(failed))
		res.Retry = &next
	}
	return res
}

// versionLabel names one version in the failure list.
func versionLabel(r model.VersionRef) string {
	kind := "version"
	if r.IsDeleteMark {
		kind = "delete marker"
	}
	return fmt.Sprintf("%s (%s %s)", r.Key, kind, r.VersionID)
}
