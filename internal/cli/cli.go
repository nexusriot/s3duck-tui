// Package cli is the command line: the flags main parses before the TUI
// starts, and the s3:// URI form shared by the launch argument, go-to-path and
// "copy URI". It has no UI or network dependencies, so all of it is testable
// as plain functions.
package cli

import (
	"errors"
	"fmt"
	"strings"
)

// URIScheme is the prefix every location URI starts with, as the AWS CLI
// writes it.
const URIScheme = "s3://"

// S3URI is a parsed s3://bucket/key. Key is everything after the first slash,
// verbatim: a trailing slash means a folder, and empty segments ("a//b") are
// kept, because S3 keys are arbitrary and collapsing them would name a
// different object.
type S3URI struct {
	Bucket string
	Key    string
}

// String renders the URI back. A bucket with no key is written with its
// slash, which is how the AWS CLI names a bucket's root.
func (u S3URI) String() string {
	return FormatS3URI(u.Bucket, u.Key)
}

// FormatS3URI writes bucket and key as an s3:// URI. Nothing is
// percent-encoded: s3:// URIs are not URLs, and every tool that takes one
// (aws s3, s5cmd, rclone's s3 remotes) expects the raw key.
func FormatS3URI(bucket, key string) string {
	return URIScheme + bucket + "/" + key
}

// IsS3URI reports whether s is written as an s3:// URI (the scheme is matched
// without regard to case, as the AWS CLI does).
func IsS3URI(s string) bool {
	return len(s) >= len(URIScheme) && strings.EqualFold(s[:len(URIScheme)], URIScheme)
}

// ParseS3URI parses s3://bucket[/key]. The bucket must be non-empty and the
// URI must carry nothing a bucket name cannot hold before the first slash.
func ParseS3URI(s string) (S3URI, error) {
	if !IsS3URI(s) {
		return S3URI{}, fmt.Errorf("%q is not an s3:// URI", s)
	}
	rest := s[len(URIScheme):]
	bucket, key, _ := strings.Cut(rest, "/")
	if bucket == "" {
		return S3URI{}, fmt.Errorf("%q names no bucket", s)
	}
	if strings.ContainsAny(bucket, " \t\r\n?#") {
		return S3URI{}, fmt.Errorf("%q is not a valid bucket name", bucket)
	}
	return S3URI{Bucket: bucket, Key: key}, nil
}

// Notification modes for a finished background job (see --notify).
const (
	NotifyAuto    = "auto"
	NotifyOff     = "off"
	NotifyCommand = "command"
	NotifyOSC9    = "osc9"
	NotifyOSC777  = "osc777"
)

// NotifyModes lists every accepted --notify value, for the usage text and the
// error that rejects anything else.
var NotifyModes = []string{NotifyAuto, NotifyOff, NotifyCommand, NotifyOSC9, NotifyOSC777}

// ParseNotifyMode validates a --notify / S3DUCK_NOTIFY value. Empty means
// auto, so an unset variable is the default rather than an error.
func ParseNotifyMode(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return NotifyAuto, nil
	}
	for _, m := range NotifyModes {
		if s == m {
			return s, nil
		}
	}
	return "", fmt.Errorf("unknown notification mode %q (one of: %s)", s, strings.Join(NotifyModes, ", "))
}

// Options is what the command line asked for.
type Options struct {
	// ConfigPath replaces ~/.config/s3duck-tui/config.json. Empty: the default.
	ConfigPath string
	// Profile opens this profile straight away instead of the profile list.
	Profile string
	// Location opens this bucket/prefix (or reveals this object) once the
	// profile is open, in place of the profile's remembered location.
	Location *S3URI
	// Notify is the notification mode; empty means "not given on the command
	// line", so the environment variable can still decide.
	Notify string
	// Version and Help print and exit without starting the TUI.
	Version bool
	Help    bool
}

// ErrUsage marks a command-line mistake: main prints it with the usage text
// and exits 2, the conventional status for bad arguments. Test for it with
// errors.Is; the error's own text is the message alone, ready to print.
var ErrUsage = errors.New("usage")

// usageError is a command-line mistake. Its text is just what went wrong,
// without a "usage:" prefix — wrapping with %w put the sentinel's word in
// front, and unwrapping it again to drop that lost the message entirely.
type usageError struct{ msg string }

func (e usageError) Error() string        { return e.msg }
func (e usageError) Is(target error) bool { return target == ErrUsage }

func usageErr(format string, args ...any) error {
	return usageError{msg: fmt.Sprintf(format, args...)}
}

// Parse reads the arguments after the program name. Flags may come before or
// after the URI (Go's flag package stops at the first positional, so
// "s3duck-tui s3://b --profile x" would silently ignore the profile); "--"
// ends the flags. Both "--flag value" and "--flag=value" work.
func Parse(args []string) (Options, error) {
	var o Options
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		// takeValue consumes the flag's argument, from "=value" or from the
		// next word.
		takeValue := func() (string, error) {
			if hasValue {
				return value, nil
			}
			if i+1 >= len(args) {
				return "", usageErr("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		var err error
		switch name {
		case "p", "profile":
			o.Profile, err = takeValue()
			if err == nil && strings.TrimSpace(o.Profile) == "" {
				err = usageErr("%s needs a profile name", a)
			}
		case "c", "config":
			o.ConfigPath, err = takeValue()
			if err == nil && strings.TrimSpace(o.ConfigPath) == "" {
				err = usageErr("%s needs a file path", a)
			}
		case "notify":
			var v string
			if v, err = takeValue(); err == nil {
				o.Notify, err = ParseNotifyMode(v)
				if err != nil {
					err = usageErr("%v", err)
				}
			}
		case "v", "version":
			if hasValue {
				return o, usageErr("%s takes no value", a)
			}
			o.Version = true
		case "h", "help":
			if hasValue {
				return o, usageErr("%s takes no value", a)
			}
			o.Help = true
		default:
			return o, usageErr("unknown flag %s", a)
		}
		if err != nil {
			return o, err
		}
	}

	switch len(positional) {
	case 0:
	case 1:
		u, err := ParseS3URI(positional[0])
		if err != nil {
			return o, usageErr("%v", err)
		}
		o.Location = &u
	default:
		return o, usageErr("one location at most, got %d: %s", len(positional), strings.Join(positional, " "))
	}
	return o, nil
}

// Usage is the --help text. prog is the program name as invoked.
func Usage(prog string) string {
	return fmt.Sprintf(`Usage: %[1]s [flags] [s3://bucket[/prefix/ | /key]]

A terminal S3 client. With no arguments it opens on the profile list.

Flags:
  -p, --profile NAME   open this profile straight away
  -c, --config FILE    use FILE instead of ~/.config/s3duck-tui/config.json
      --notify MODE    desktop notice when a background job finishes:
                       %[2]s
                       (default auto; also S3DUCK_NOTIFY)
  -v, --version        print the version and exit
  -h, --help           print this help and exit

A location opens that bucket and folder in place of the profile's
remembered one. A key that is a folder opens the folder; any other key
opens its folder with the object highlighted. A location needs --profile
unless only one profile is configured.

Examples:
  %[1]s --profile prod
  %[1]s -p prod s3://logs/2026/10/
  %[1]s s3://public-data/README.md
`, prog, strings.Join(NotifyModes, " | "))
}
