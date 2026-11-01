package controller

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rivo/tview"

	"github.com/nexusriot/s3duck-tui/internal/model"
	"github.com/nexusriot/s3duck-tui/internal/ui/shell"
)

// Exporting a listing: the rows on screen — the browser's (filter and sort
// applied), the recursive search's hits, a usage-browser level — written as
// CSV or JSON for a spreadsheet, jq or a ticket. One row type for all three,
// so a column means the same thing whichever view it came from.

// exportRow is one exported object, folder or bucket.
type exportRow struct {
	Bucket       string     `json:"bucket"`
	Key          string     `json:"key"`
	Type         string     `json:"type"`
	Size         *int64     `json:"size,omitempty"`
	LastModified *time.Time `json:"last_modified,omitempty"`
	StorageClass string     `json:"storage_class,omitempty"`
	ETag         string     `json:"etag,omitempty"`
	// Objects is set for usage rows: how many objects the size adds up.
	Objects *int `json:"objects,omitempty"`
}

// exportFormat names a file format.
type listingFormat string

const (
	formatCSVExport  listingFormat = "csv"
	formatJSONExport listingFormat = "json"
)

// exportRowFor turns a listed object into a row. bucket is the pane's bucket
// ("" on the bucket list, where the row is a bucket itself).
func exportRowFor(bucket string, o *model.Object) exportRow {
	r := exportRow{Bucket: bucket, LastModified: o.LastModified}
	switch o.Ot {
	case model.Bucket:
		r.Type, r.Bucket = "bucket", deref(o.Key)
	case model.Folder:
		r.Type, r.Key = "folder", deref(o.FullPath)
	default:
		r.Type, r.Key = "object", deref(o.FullPath)
		r.Size = o.Size
		if o.StorageClass != nil {
			r.StorageClass = *o.StorageClass
		}
		r.ETag = strings.Trim(deref(o.Etag), `"`)
	}
	return r
}

// csvFormulaStart are the first characters spreadsheets read as a formula.
// A cell starting with one is prefixed with an apostrophe in CSV — an object
// key is attacker-controlled on a shared bucket, and "=HYPERLINK(...)" opened
// in a spreadsheet runs. JSON is written verbatim.
const csvFormulaStart = "=+-@\t\r"

func csvCellSafe(s string) string {
	if s != "" && strings.ContainsRune(csvFormulaStart, rune(s[0])) {
		return "'" + s
	}
	return s
}

// encodeRows writes rows in format. CSV has a header row and RFC 3339 UTC
// times; keys holding commas, quotes or newlines are quoted by encoding/csv,
// so one key stays one record.
func encodeRows(rows []exportRow, format listingFormat) ([]byte, error) {
	var buf bytes.Buffer
	switch format {
	case formatJSONExport:
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if rows == nil {
			rows = []exportRow{}
		}
		if err := enc.Encode(rows); err != nil {
			return nil, err
		}
	case formatCSVExport:
		w := csv.NewWriter(&buf)
		_ = w.Write([]string{"bucket", "key", "type", "size", "last_modified", "storage_class", "etag", "objects"})
		for _, r := range rows {
			size, objects, when := "", "", ""
			if r.Size != nil {
				size = strconv.FormatInt(*r.Size, 10)
			}
			if r.Objects != nil {
				objects = strconv.Itoa(*r.Objects)
			}
			if r.LastModified != nil {
				when = r.LastModified.UTC().Format(time.RFC3339)
			}
			if err := w.Write([]string{csvCellSafe(r.Bucket), csvCellSafe(r.Key), r.Type, size, when, csvCellSafe(r.StorageClass), r.ETag, objects}); err != nil {
				return nil, err
			}
		}
		w.Flush()
		if err := w.Error(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown export format %q", format)
	}
	return buf.Bytes(), nil
}

var exportNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// listingFileName names an export after where it came from, and when.
func listingFileName(what string, format listingFormat, now time.Time) string {
	base := strings.Trim(exportNameUnsafe.ReplaceAllString(what, "-"), "-.")
	if len(base) > 80 {
		base = base[:80]
	}
	if base == "" {
		base = "listing"
	}
	return fmt.Sprintf("%s-%s.%s", base, now.Format("20060102-150405"), format)
}

// writeExport writes data to dir/name through a temp file renamed into place,
// so a failed write never leaves a truncated export under the final name.
func writeExport(dir, name string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	fp, err := os.CreateTemp(dir, "."+name+".*.tmp")
	if err != nil {
		return "", err
	}
	tmp := fp.Name()
	_, werr := fp.Write(data)
	if cerr := fp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0644)
	}
	target := filepath.Join(dir, name)
	if werr == nil {
		werr = os.Rename(tmp, target)
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return "", werr
	}
	return target, nil
}

// askExport offers the two formats for rows and writes the chosen one to the
// download directory. UI goroutine.
func (c *Controller) askExport(what string, rows []exportRow) {
	if len(rows) == 0 {
		c.error("Export", fmt.Errorf("nothing to export"))
		return
	}
	dir := c.resolveDownloadDir()
	var lay *shell.Layer
	modal := tview.NewModal().
		SetText(tview.Escape(fmt.Sprintf("Export %d row(s) of %s\n\nto %s", len(rows), what, dir))).
		AddButtons([]string{"CSV", "JSON", "Cancel"}).
		SetDoneFunc(func(_ int, label string) {
			lay.Close()
			var format listingFormat
			switch label {
			case "CSV":
				format = formatCSVExport
			case "JSON":
				format = formatJSONExport
			default:
				return
			}
			data, err := encodeRows(rows, format)
			if err == nil {
				var path string
				path, err = writeExport(dir, listingFileName(what, format, time.Now()), data)
				if err == nil {
					c.logActivity("exported %d row(s) of %s to %s", len(rows), what, path)
					c.success(fmt.Sprintf("Exported %d row(s) to\n%s", len(rows), path))
					return
				}
			}
			c.error("Export failed", err)
		})
	lay = c.pushRaw(modal)
}

// ExportListing exports the listing on screen: what the filter lets through,
// in the order shown. Bound to leader e.
func (c *Controller) ExportListing() {
	loc := c.pane().Loc()
	sk, sd := c.getSort()
	objs := filterSortObjects(c.pane().Snapshot(), c.pane().Filter(), sk, sd)
	bucket, what := "", "buckets"
	switch {
	case loc.LocalDir != "":
		what = loc.LocalDir
	case loc.Bucket != nil && loc.Bucket.Key != nil:
		bucket = *loc.Bucket.Key
		what = bucket + "/" + loc.Path
	}
	rows := make([]exportRow, 0, len(objs))
	for _, o := range objs {
		if o == nil || o.Key == nil {
			continue
		}
		rows = append(rows, exportRowFor(bucket, o))
	}
	c.askExport(what, rows)
}
