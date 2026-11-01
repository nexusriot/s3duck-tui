package cli

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseS3URI(t *testing.T) {
	tests := []struct {
		in      string
		want    S3URI
		wantErr bool
	}{
		{in: "s3://bucket", want: S3URI{Bucket: "bucket"}},
		{in: "s3://bucket/", want: S3URI{Bucket: "bucket"}},
		{in: "s3://bucket/logs/", want: S3URI{Bucket: "bucket", Key: "logs/"}},
		{in: "s3://bucket/a/b.txt", want: S3URI{Bucket: "bucket", Key: "a/b.txt"}},
		// Keys are arbitrary: empty segments and spaces survive verbatim.
		{in: "s3://bucket/a//b ", want: S3URI{Bucket: "bucket", Key: "a//b "}},
		{in: "S3://Bucket/x", want: S3URI{Bucket: "Bucket", Key: "x"}},
		{in: "s3://", wantErr: true},
		{in: "s3:///key", wantErr: true},
		{in: "s3://my bucket/key", wantErr: true},
		{in: "bucket/key", wantErr: true},
		{in: "https://bucket.s3.amazonaws.com/key", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseS3URI(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseS3URI(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("ParseS3URI(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestFormatS3URIRoundTrips(t *testing.T) {
	for _, u := range []S3URI{
		{Bucket: "b"},
		{Bucket: "b", Key: "dir/"},
		{Bucket: "b", Key: "dir/file name.txt"},
		{Bucket: "b", Key: "a//b"},
	} {
		s := u.String()
		back, err := ParseS3URI(s)
		if err != nil || back != u {
			t.Errorf("%+v -> %q -> %+v, %v", u, s, back, err)
		}
	}
	if got := FormatS3URI("b", ""); got != "s3://b/" {
		t.Errorf("bucket root = %q, want s3://b/", got)
	}
}

func TestParseArgs(t *testing.T) {
	loc := func(b, k string) *S3URI { return &S3URI{Bucket: b, Key: k} }
	tests := []struct {
		name string
		args []string
		want Options
	}{
		{name: "none", args: nil, want: Options{}},
		{name: "profile long", args: []string{"--profile", "prod"}, want: Options{Profile: "prod"}},
		{name: "profile short =", args: []string{"-p=prod"}, want: Options{Profile: "prod"}},
		{name: "config", args: []string{"-c", "/tmp/c.json"}, want: Options{ConfigPath: "/tmp/c.json"}},
		{name: "version", args: []string{"--version"}, want: Options{Version: true}},
		{name: "help short", args: []string{"-h"}, want: Options{Help: true}},
		{name: "notify", args: []string{"--notify=OSC9"}, want: Options{Notify: NotifyOSC9}},
		{name: "uri only", args: []string{"s3://b/x/"}, want: Options{Location: loc("b", "x/")}},
		// Flags after the positional still count — Go's flag package would
		// have stopped at the URI and dropped the profile silently.
		{name: "flags after uri", args: []string{"s3://b/k", "--profile", "dev"},
			want: Options{Profile: "dev", Location: loc("b", "k")}},
		{name: "double dash", args: []string{"-p", "x", "--", "s3://b/-odd"},
			want: Options{Profile: "x", Location: loc("b", "-odd")}},
	}
	for _, tt := range tests {
		got, err := Parse(tt.args)
		if err != nil {
			t.Errorf("%s: Parse(%q) error: %v", tt.name, tt.args, err)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: Parse(%q) = %+v, want %+v", tt.name, tt.args, got, tt.want)
		}
	}
}

func TestParseArgsErrors(t *testing.T) {
	for _, args := range [][]string{
		{"--profile"},
		{"--profile", ""},
		{"--config="},
		{"--frobnicate"},
		{"--version=yes"},
		{"--help=please"},
		{"--notify"},
		{"--notify", "loud"},
		{"bucket/key"},
		{"s3://a/", "s3://b/"},
	} {
		_, err := Parse(args)
		if err == nil {
			t.Errorf("Parse(%q) accepted", args)
			continue
		}
		if !errors.Is(err, ErrUsage) {
			t.Errorf("Parse(%q) error %v is not a usage error", args, err)
		}
	}
}

func TestParseNotifyMode(t *testing.T) {
	if m, err := ParseNotifyMode(""); err != nil || m != NotifyAuto {
		t.Errorf("empty = %q, %v; want auto", m, err)
	}
	if m, err := ParseNotifyMode(" Off "); err != nil || m != NotifyOff {
		t.Errorf("' Off ' = %q, %v; want off", m, err)
	}
	if _, err := ParseNotifyMode("growl"); err == nil {
		t.Error("growl accepted")
	}
}

func TestUsageMentionsEveryFlag(t *testing.T) {
	u := Usage("s3duck-tui")
	for _, want := range []string{"--profile", "--config", "--notify", "--version", "--help", "s3://", NotifyOSC777} {
		if !strings.Contains(u, want) {
			t.Errorf("usage text does not mention %s", want)
		}
	}
}

// The message is what gets printed: it must say what was wrong, not "usage".
func TestUsageErrorText(t *testing.T) {
	_, err := Parse([]string{"--nope"})
	if err == nil || err.Error() != "unknown flag --nope" {
		t.Errorf("error text = %q, want %q", err, "unknown flag --nope")
	}
}
