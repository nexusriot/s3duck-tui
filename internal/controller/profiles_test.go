package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nexusriot/s3duck-tui/internal/model"
)

type fakeProber struct {
	fail       map[string]error
	asked      []string
	refreshed  []string
	refreshErr error
}

func (f *fakeProber) RefreshClient(b *string) error {
	f.refreshed = append(f.refreshed, *b)
	return f.refreshErr
}

func (f *fakeProber) HasPrefix(_ context.Context, b *model.Object, _ string) (bool, error) {
	f.asked = append(f.asked, *b.Key)
	return true, f.fail[*b.Key]
}

func TestCheckPublicBuckets(t *testing.T) {
	ok := &fakeProber{}
	n, err := checkPublicBuckets(context.Background(), ok, []string{"a", " b ", "a", ""})
	if err != nil || n != 2 || strings.Join(ok.asked, ",") != "a,b" {
		t.Errorf("all readable: n=%d err=%v asked=%v; want 2 distinct, trimmed", n, err, ok.asked)
	}
	// Each bucket is checked in its own region, as browsing reaches it.
	if strings.Join(ok.refreshed, ",") != "a,b" {
		t.Errorf("regions pinned for %v, want a,b", ok.refreshed)
	}
	bad := &fakeProber{fail: map[string]error{"b": errors.New("AccessDenied")}}
	if _, err := checkPublicBuckets(context.Background(), bad, []string{"a", "b", "c"}); err == nil || !strings.Contains(err.Error(), "public bucket b") {
		t.Errorf("err = %v, want it to name bucket b", err)
	}
	// A region lookup that fails is not the verdict: the listing is.
	noRegion := &fakeProber{refreshErr: errors.New("Forbidden")}
	if n, err := checkPublicBuckets(context.Background(), noRegion, []string{"a"}); err != nil || n != 1 {
		t.Errorf("a failed region lookup failed the check: n=%d err=%v", n, err)
	}
	if _, err := checkPublicBuckets(context.Background(), ok, []string{" ", ""}); !errors.Is(err, model.ErrNoPublicBuckets) {
		t.Errorf("none named: err = %v", err)
	}
}
