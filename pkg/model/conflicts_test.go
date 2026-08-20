package model

import (
	"errors"
	"fmt"
	"testing"

	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

func TestCommonPrefix(t *testing.T) {
	tests := []struct {
		name string
		keys []string
		want string
	}{
		{"no keys", nil, ""},
		{"one key keeps its folder", []string{"a/b/c.txt"}, "a/b/"},
		{"one root key has no prefix", []string{"c.txt"}, ""},
		{"shared folder", []string{"a/b/c.txt", "a/b/d.txt"}, "a/b/"},
		{"diverging subfolders", []string{"a/b/c.txt", "a/e/d.txt"}, "a/"},
		{"nothing in common", []string{"a/x.txt", "b/y.txt"}, ""},
		// A shared name fragment is not a prefix boundary: "a/report1" and
		// "a/report2" share "a/report", but listing that would be listing half
		// a name, so the prefix cuts back to the folder.
		{"partial segment is not a boundary", []string{"a/report1", "a/report2"}, "a/"},
		{"folder candidate and a file under it", []string{"a/b/", "a/b/c.txt"}, "a/b/"},
		{"deeply nested", []string{"x/y/z/1", "x/y/z/2", "x/y/w/3"}, "x/y/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CommonPrefix(tt.keys); got != tt.want {
				t.Errorf("CommonPrefix(%v) = %q, want %q", tt.keys, got, tt.want)
			}
		})
	}
}

// TestCommonPrefixCoversItsKeys is the property the listing path depends on:
// whatever prefix comes back, every key must be under it — otherwise a
// conflict scan would list somewhere that cannot contain what it is looking
// for and report "no conflicts" for objects that exist.
func TestCommonPrefixCoversItsKeys(t *testing.T) {
	sets := [][]string{
		{"a/b/c.txt", "a/b/d.txt"},
		{"a/b/c.txt", "a/e/d.txt"},
		{"a/x.txt", "b/y.txt"},
		{"one.txt"},
		{"deep/nest/ed/1", "deep/nest/2", "deep/3"},
	}
	for _, keys := range sets {
		prefix := CommonPrefix(keys)
		for _, k := range keys {
			if len(k) < len(prefix) || k[:len(prefix)] != prefix {
				t.Errorf("CommonPrefix(%v) = %q, which %q is not under", keys, prefix, k)
			}
		}
	}
}

// TestIsNotFound pins every spelling of "no such key" the conflict scan must
// recognise. A HEAD response has no body for the SDK to parse an error shape
// from, so real backends answer with an untyped API error — and because a
// failed scan blocks the write, a missed spelling would make every rename and
// copy on that backend refuse to run.
func TestIsNotFound(t *testing.T) {
	found := []struct {
		name string
		err  error
	}{
		{"typed NotFound", &s3t.NotFound{}},
		{"typed NoSuchKey", &s3t.NoSuchKey{}},
		{"untyped NotFound code (what MinIO returns for HEAD)", &smithy.GenericAPIError{Code: "NotFound", Message: "Not Found"}},
		{"untyped NoSuchKey code", &smithy.GenericAPIError{Code: "NoSuchKey"}},
		{"bare 404 code", &smithy.GenericAPIError{Code: "404"}},
		{"wrapped", fmt.Errorf("checking: %w", &smithy.GenericAPIError{Code: "NotFound"})},
	}
	for _, tt := range found {
		t.Run(tt.name, func(t *testing.T) {
			if !isNotFound(tt.err) {
				t.Errorf("isNotFound(%v) = false, want true", tt.err)
			}
		})
	}

	// Anything else must stay an error: a denied or broken destination check
	// must not be read as "the destination is free".
	other := []error{
		errors.New("connection refused"),
		&smithy.GenericAPIError{Code: "AccessDenied"},
		&smithy.GenericAPIError{Code: "InternalError"},
	}
	for _, err := range other {
		if isNotFound(err) {
			t.Errorf("isNotFound(%v) = true, want false", err)
		}
	}
}
