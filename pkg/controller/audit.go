package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// auditFileName is the log's name under the state directory.
const auditFileName = "activity.log"

// auditMaxBytes caps the file. Past it the log is rotated once to
// activity.log.1, so a long-running install cannot fill a disk and the
// previous window is still readable.
const auditMaxBytes = 2 << 20

// AuditPath is where the persistent log lives: $XDG_STATE_HOME if set, else
// ~/.local/state. Not next to config.json — this is state, not configuration,
// and a user who syncs their config directory does not want an append-only log
// travelling with it.
func AuditPath(homeDir string) string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		base = filepath.Join(homeDir, ".local", "state")
	}
	return filepath.Join(base, "s3duck-tui", auditFileName)
}

// auditLine formats one entry. Tab-separated so it can be cut(1) apart, with
// an RFC 3339 timestamp so it sorts and parses.
func auditLine(when time.Time, profile, msg string) string {
	if profile == "" {
		profile = "-"
	}
	// A newline in a message would forge a log entry; a tab would shift the
	// columns. Both are folded to spaces.
	msg = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(msg)
	return fmt.Sprintf("%s\t%s\t%s\n", when.UTC().Format(time.RFC3339), profile, msg)
}

// appendAudit writes one line to path, creating the directory as needed and
// rotating the file when it outgrows maxBytes.
//
// Errors are returned rather than surfaced: an unwritable log must never
// interrupt the operation it is describing, and the caller (logActivity)
// deliberately drops them.
func appendAudit(path, line string, maxBytes int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if fi, err := os.Stat(path); err == nil && maxBytes > 0 && fi.Size()+int64(len(line)) > maxBytes {
		// One generation only: two files is enough to answer "what happened
		// recently", and unbounded rotation is a disk-space surprise.
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return err
}

// auditProfile names the profile for the log: the open one, or "-" on the
// profiles screen.
func (c *Controller) auditProfile() string {
	if c.activeConfig == nil {
		return ""
	}
	return c.activeConfig.Name
}

// rememberLocation records where this profile is browsing, so reopening it
// lands here. Kept in memory and written out with the rest of the config when
// the app exits or the profile is closed — one config rewrite per navigation
// would mean rewriting a file full of credentials dozens of times a minute.
func (c *Controller) rememberLocation() {
	if c.activeConfig == nil || c.localMode() {
		return
	}
	if c.currentBucket == nil || c.currentBucket.Key == nil {
		c.activeConfig.LastBucket = ""
		c.activeConfig.LastPrefix = ""
		return
	}
	c.activeConfig.LastBucket = *c.currentBucket.Key
	c.activeConfig.LastPrefix = c.currentPath
}

// persistProfiles writes the profile list back, carrying whatever was recorded
// into it (the last location). Failures are logged, not shown: the user is
// usually on their way out when this runs.
func (c *Controller) persistProfiles() {
	if c.params == nil {
		return
	}
	if err := c.params.WriteConfig(); err != nil {
		c.logActivity("could not save the profile list: %v", err)
	}
}

// restoreLocation navigates to the profile's remembered location, if it has
// one. Called right after the profile is opened, in place of the bucket list.
//
// Must run on the UI goroutine: jumpTo clears the filter box and the details
// pane synchronously before spawning its own goroutine for the network part,
// so calling it from a background goroutine would touch widgets off-thread.
func (c *Controller) restoreLocation() bool {
	if c.activeConfig == nil || c.activeConfig.LastBucket == "" {
		return false
	}
	// jumpTo resolves the bucket and refreshes the client, and reports its own
	// errors — a bucket that has since been deleted or lost its permissions
	// simply leaves the user on the bucket list.
	c.jumpTo(c.activeConfig.LastBucket, c.activeConfig.LastPrefix, "")
	return true
}
