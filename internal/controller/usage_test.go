package controller

import (
	"strings"
	"testing"

	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func usageObj(key string, size int64, class string) s3t.Object {
	k := key
	return s3t.Object{Key: &k, Size: size, StorageClass: s3t.ObjectStorageClass(class)}
}

// S3 happily holds both "foo" and "foo/bar". Whichever order they are listed
// in, the node has to end up a directory: the browser only opens directories,
// so a file node with children hides everything beneath it.
func TestBuildUsageTreeKeyThatIsAlsoAPrefix(t *testing.T) {
	for _, tc := range []struct {
		name string
		objs []s3t.Object
	}{
		{"file first", []s3t.Object{usageObj("foo", 10, ""), usageObj("foo/bar", 30, "")}},
		{"prefix first", []s3t.Object{usageObj("foo/bar", 30, ""), usageObj("foo", 10, "")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := buildUsageTree(tc.objs, "")
			foo := root.children["foo"]
			if foo == nil {
				t.Fatal("foo is missing from the tree")
			}
			if !foo.isDir {
				t.Error("foo has children, so it must be openable as a directory")
			}
			if foo.fullPath != "foo/" {
				t.Errorf("foo.fullPath = %q, want foo/ so drilling in lands on the prefix", foo.fullPath)
			}
			if foo.bytes != 40 || foo.objects != 2 {
				t.Errorf("foo = %d bytes / %d objects, want 40/2", foo.bytes, foo.objects)
			}
			bar := foo.children["bar"]
			if bar == nil || bar.bytes != 30 {
				t.Fatalf("foo/bar = %+v, want a 30-byte leaf", bar)
			}
		})
	}
}

// A streamed scan folds page by page; the tree must come out the same as a
// single-shot build over the whole listing.
func TestAddUsageObjectsMatchesPagedBuild(t *testing.T) {
	objs := []s3t.Object{
		usageObj("logs/a.txt", 5, "STANDARD"),
		usageObj("logs/b.txt", 7, "GLACIER"),
		usageObj("logs/deep/c.txt", 11, "STANDARD"),
	}
	whole := buildUsageTree(objs, "")

	paged := newUsageNode("", "", true)
	for _, o := range objs {
		addUsageObjects(paged, []s3t.Object{o}, "")
	}
	if paged.bytes != whole.bytes || paged.objects != whole.objects {
		t.Fatalf("paged root = %d/%d, whole = %d/%d",
			paged.bytes, paged.objects, whole.bytes, whole.objects)
	}
	if got, want := paged.children["logs"].children["deep"].bytes, whole.children["logs"].children["deep"].bytes; got != want {
		t.Errorf("paged logs/deep = %d bytes, want %d", got, want)
	}
	if got, want := len(paged.classes), len(whole.classes); got != want {
		t.Errorf("paged classes = %d, want %d", got, want)
	}
}

func TestBuildUsageTreeAccumulatesUpTheTree(t *testing.T) {
	objs := []s3t.Object{
		usageObj("data/2024/a.bin", 100, "STANDARD"),
		usageObj("data/2024/b.bin", 200, "GLACIER"),
		usageObj("data/2025/c.bin", 50, "STANDARD"),
		usageObj("top.txt", 10, ""),
		usageObj("data/", 0, ""), // folder marker: not content
	}
	root := buildUsageTree(objs, "")

	if root.bytes != 360 || root.objects != 4 {
		t.Fatalf("root = %d bytes / %d objects, want 360/4", root.bytes, root.objects)
	}
	data := root.children["data"]
	if data == nil || !data.isDir {
		t.Fatal("data/ should be a directory node")
	}
	if data.bytes != 350 || data.objects != 3 {
		t.Errorf("data/ = %d bytes / %d objects, want 350/3", data.bytes, data.objects)
	}
	y2024 := data.children["2024"]
	if y2024 == nil || y2024.bytes != 300 || y2024.objects != 2 {
		t.Errorf("data/2024 = %+v, want 300 bytes / 2 objects", y2024)
	}
	leaf := y2024.children["a.bin"]
	if leaf == nil || leaf.isDir || leaf.bytes != 100 {
		t.Errorf("leaf = %+v, want a 100-byte file", leaf)
	}
	// An absent storage class reports as STANDARD rather than as "".
	if root.classes["STANDARD"] != 160 || root.classes["GLACIER"] != 200 {
		t.Errorf("class split = %v, want STANDARD 160 / GLACIER 200", root.classes)
	}
	if data.fullPath != "data/" || leaf.fullPath != "data/2024/a.bin" {
		t.Errorf("full paths wrong: %q / %q", data.fullPath, leaf.fullPath)
	}
}

func TestBuildUsageTreeHonoursPrefix(t *testing.T) {
	objs := []s3t.Object{
		usageObj("logs/app/1.log", 10, ""),
		usageObj("logs/app/2.log", 20, ""),
	}
	root := buildUsageTree(objs, "logs/")
	if root.bytes != 30 {
		t.Fatalf("root bytes = %d, want 30", root.bytes)
	}
	// The prefix is stripped, so the first level below it is "app", not "logs".
	if root.children["logs"] != nil || root.children["app"] == nil {
		t.Errorf("children = %v, want the tree rooted at the prefix", root.children)
	}
	if got := root.children["app"].fullPath; got != "logs/app/" {
		t.Errorf("child fullPath = %q, want logs/app/", got)
	}
}

func TestSortedChildrenBySizeThenName(t *testing.T) {
	root := buildUsageTree([]s3t.Object{
		usageObj("small.bin", 1, ""),
		usageObj("big.bin", 100, ""),
		usageObj("mid-b.bin", 50, ""),
		usageObj("mid-a.bin", 50, ""),
	}, "")
	got := root.sortedChildren()
	want := []string{"big.bin", "mid-a.bin", "mid-b.bin", "small.bin"}
	if len(got) != len(want) {
		t.Fatalf("got %d children, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].name != want[i] {
			t.Errorf("child %d = %q, want %q", i, got[i].name, want[i])
		}
	}
}

func TestUsageBar(t *testing.T) {
	if got := usageBar(5, 10, 10); got != "[#####     ]" {
		t.Errorf("half bar = %q", got)
	}
	if got := usageBar(10, 10, 4); got != "[####]" {
		t.Errorf("full bar = %q", got)
	}
	if got := usageBar(0, 10, 4); got != "[    ]" {
		t.Errorf("empty bar = %q", got)
	}
	// A tiny but non-zero share must still show: rounding it away would say
	// "this holds nothing", which is exactly the wrong answer.
	if got := usageBar(1, 1_000_000, 10); !strings.Contains(got, "#") {
		t.Errorf("tiny share = %q, want at least one mark", got)
	}
	// A whole of zero must not divide by it.
	if got := usageBar(0, 0, 3); got != "[   ]" {
		t.Errorf("zero whole = %q", got)
	}
	if usageBar(1, 2, 0) != "" {
		t.Error("zero width should render nothing")
	}
}

func TestUsageRowAndBreakdown(t *testing.T) {
	root := buildUsageTree([]s3t.Object{
		usageObj("dir/a.bin", 300, "STANDARD"),
		usageObj("dir/b.bin", 100, "GLACIER"),
	}, "")
	dir := root.children["dir"]
	primary, secondary := usageRow(dir, root.bytes)
	if !strings.Contains(primary, "100.0%") || !strings.Contains(primary, "2 obj") || !strings.Contains(primary, "dir/") {
		t.Errorf("row = %q", primary)
	}
	if secondary != "dir/" {
		t.Errorf("secondary = %q, want the full path", secondary)
	}
	// The breakdown is what makes the view a cost hint: largest class first.
	if got := classBreakdown(dir); !strings.HasPrefix(got, "STANDARD") || !strings.Contains(got, "GLACIER") {
		t.Errorf("breakdown = %q", got)
	}
}

func TestUsageTitle(t *testing.T) {
	root := buildUsageTree([]s3t.Object{usageObj("a", 1024, "")}, "")
	if got := usageTitle("bkt", root); !strings.Contains(got, "bkt") || !strings.Contains(got, "1 object") {
		t.Errorf("title = %q", got)
	}
}
