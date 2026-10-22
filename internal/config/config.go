package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	filename      = "config.json"
	configFolder  = ".config"
	configDirName = "s3duck-tui"
)

type Params struct {
	HomeDir  string
	FileName string
	Config   []*Config
	// LoadErr holds a non-fatal startup error (missing/corrupt config file).
	// The app starts with an empty profile list and surfaces this in the UI
	// instead of crashing.
	LoadErr error
}

type Config struct {
	Name      string  `json:"name"`
	BaseUrl   string  `json:"base_url"`
	Region    *string `json:"region"`
	AccessKey string  `json:"access_key"`
	SecretKey string  `json:"secret_key"`
	// SessionToken is the STS session token that accompanies temporary
	// credentials (assume-role, SSO, MFA). Empty for long-lived key pairs.
	SessionToken string `json:"session_token,omitempty"`
	IgnoreSsl    bool   `json:"ignore_ssl"`
	// DownloadDir is the destination for downloads. Empty: a download asks
	// with a directory picker, and ~/Downloads stands in wherever nothing
	// asks — verify, version downloads, failure-list exports, the sync form's
	// local default and a new local pane's start. A leading "~" is expanded
	// to the user's home directory.
	DownloadDir string `json:"download_dir,omitempty"`
	// MaxBytesPerSec caps transfer throughput (upload + download) for this
	// profile. 0 (the default) means unlimited.
	MaxBytesPerSec int64 `json:"max_bytes_per_sec,omitempty"`
	// TransferWorkers is how many objects one download or one sync moves at
	// a time. 0 (the default) means the built-in width of 4. It does not
	// change how many jobs run at once, and the bandwidth cap above still
	// bounds them all together.
	TransferWorkers int `json:"transfer_workers,omitempty"`
	// Bookmarks are saved bucket+prefix locations for this profile.
	Bookmarks []Bookmark `json:"bookmarks,omitempty"`

	// AWSProfile delegates credentials to the AWS SDK's own resolution chain
	// for that ~/.aws profile, instead of the static keys above. It is what
	// makes SSO, assume-role and credential_process profiles usable — and
	// what keeps their credentials refreshed, which a stored session token
	// can never be. Empty means "use the keys above".
	AWSProfile string `json:"aws_profile,omitempty"`

	// ReadOnly blocks every mutating operation for this profile: the guard is
	// in the controller, so a production profile can be browsed, searched and
	// downloaded from but not written to or deleted from by accident.
	ReadOnly bool `json:"read_only,omitempty"`

	// NoMimeDetect switches off Content-Type derivation on upload for buckets
	// whose types are managed elsewhere. Detection is on by default.
	NoMimeDetect bool `json:"no_mime_detect,omitempty"`
	// SSE is the server-side encryption applied to objects this app creates:
	// "" (bucket default), "AES256" or "aws:kms".
	SSE string `json:"sse,omitempty"`
	// SSEKMSKeyID names the CMK for SSE="aws:kms"; empty uses the account default.
	SSEKMSKeyID string `json:"sse_kms_key_id,omitempty"`
	// ChecksumAlgo asks S3 to verify an extra checksum on every write
	// ("CRC32C", "CRC32", "SHA256", "SHA1"). Empty sends none.
	ChecksumAlgo string `json:"checksum_algo,omitempty"`
	// VerifyDownloads re-reads each downloaded file and compares it against
	// the object's checksum (or its ETag, for a single-part object) before
	// the download counts as a success.
	VerifyDownloads bool `json:"verify_downloads,omitempty"`

	// Trash makes delete a move into TrashPrefix instead of a removal, for
	// profiles where an accidental delete is unrecoverable. Undo only ever
	// covered move/rename; this covers delete.
	Trash bool `json:"trash,omitempty"`
	// TrashPrefix is where a trashed object lands. Empty means DefaultTrashPrefix.
	TrashPrefix string `json:"trash_prefix,omitempty"`

	// LastBucket / LastPrefix are where this profile was last browsing, so
	// reopening it lands where you left off instead of at the bucket list.
	LastBucket string `json:"last_bucket,omitempty"`
	LastPrefix string `json:"last_prefix,omitempty"`
}

// DefaultTrashPrefix is where safe-delete moves objects when the profile does
// not name a prefix of its own. The leading dot keeps it out of the way of
// ordinary listings on backends that hide dot-prefixes, and the name is
// distinctive enough to grep for.
const DefaultTrashPrefix = ".s3duck-trash/"

// Trashed reports the trash prefix in force for this profile ("" when safe
// delete is off), always terminated with a slash so it reads as a folder.
func (c *Config) Trashed() string {
	if c == nil || !c.Trash {
		return ""
	}
	p := c.TrashPrefix
	if p == "" {
		p = DefaultTrashPrefix
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// Bookmark is a saved location within a profile's storage.
type Bookmark struct {
	Name   string `json:"name"`
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix"`
}

// WriteConfig saves the whole in-memory profile list. It is for explicit user
// edits (create, edit, copy, delete, options, bookmarks); the automatic save
// of the last location goes through SaveLocation instead.
func (p *Params) WriteConfig() error {
	// If the last load failed, the in-memory list does NOT reflect what the
	// file held — saving would overwrite every previously stored profile with
	// the (empty + new) list. Preserve the unreadable original first so it
	// stays recoverable by hand.
	if p.LoadErr != nil {
		if _, err := os.Stat(p.FileName); err == nil {
			bak := backupName(p.FileName)
			if err := os.Rename(p.FileName, bak); err != nil {
				return fmt.Errorf("failed to back up unreadable config before overwriting: %w", err)
			}
		}
		// Cleared only once the original is safe: a failed rename above
		// leaves the guard in place for the next attempt.
		p.LoadErr = nil
	}
	return writeConfigFile(p.FileName, p.Config)
}

// backupName picks where an unreadable config is moved before the first save
// over it. config.json.bak when that is free; otherwise a numbered sibling, so
// a second corruption can never overwrite the first backup — which may be the
// only remaining copy of every stored credential.
func backupName(fileName string) string {
	bak := fileName + ".bak"
	for i := 1; ; i++ {
		if _, err := os.Stat(bak); os.IsNotExist(err) {
			return bak
		}
		bak = fmt.Sprintf("%s.bak.%d", fileName, i)
	}
}

// writeConfigFile marshals list into fileName.
func writeConfigFile(fileName string, list []*Config) error {
	file, err := json.Marshal(list)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	return writeConfigBytes(fileName, file)
}

// writeConfigBytes replaces fileName with file.
func writeConfigBytes(fileName string, file []byte) error {
	// Write-then-rename, never truncate-in-place: a crash or full disk midway
	// through os.WriteFile would leave a partial config.json holding every
	// profile's credentials, and the next save would wipe them all.
	tmp := fileName + ".tmp"
	if err := os.WriteFile(tmp, file, 0600); err != nil {
		return fmt.Errorf("failed to write config file %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, fileName); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("failed to replace config file %s: %w", fileName, err)
	}
	return nil
}

// SaveLocation records where the named profile was last browsing. It is the
// one save the app makes on its own — on every exit and every return to the
// profiles screen — so it touches nothing but those two fields of that one
// profile, as they are on disk now:
//
//   - A config that failed to load is never written: the startup save used to
//     go through WriteConfig, which moved the unreadable file aside, replaced
//     it with an empty list and cleared the error before it was shown, so a
//     single typo in config.json silently cost every profile.
//   - The file is re-read rather than overwritten from memory, so quitting one
//     instance no longer erases the profiles and bookmarks another instance
//     saved since this one started.
//   - The two fields are patched into the file's own JSON, not round-tripped
//     through Config: decoding into the struct and re-encoding it silently
//     dropped every field this build does not know — one a newer s3duck
//     wrote, say — from every profile, on every quit.
//
// Nothing is written when the location has not changed.
func (p *Params) SaveLocation(name, bucket, prefix string) error {
	if p.LoadErr != nil || p.FileName == "" {
		return nil
	}
	raw, err := os.ReadFile(p.FileName)
	if err != nil {
		return fmt.Errorf("failed to read config file %s: %w", p.FileName, err)
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return fmt.Errorf("failed to parse config file %s: %w", p.FileName, err)
	}
	for _, e := range entries {
		if e == nil || rawString(e["name"]) != name {
			continue
		}
		if rawString(e["last_bucket"]) == bucket && rawString(e["last_prefix"]) == prefix {
			return nil
		}
		setRawString(e, "last_bucket", bucket)
		setRawString(e, "last_prefix", prefix)
		file, err := json.Marshal(entries)
		if err != nil {
			return fmt.Errorf("failed to marshal config: %w", err)
		}
		return writeConfigBytes(p.FileName, file)
	}
	return nil
}

// rawString decodes a JSON string field, reading a missing or non-string one
// as "".
func rawString(v json.RawMessage) string {
	var s string
	if len(v) == 0 || json.Unmarshal(v, &s) != nil {
		return ""
	}
	return s
}

// setRawString stores s under key, or removes the key for "" — the omitempty
// the Config struct would have applied.
func setRawString(e map[string]json.RawMessage, key, s string) {
	if s == "" {
		delete(e, key)
		return
	}
	b, _ := json.Marshal(s)
	e[key] = b
}

// NameExists reports whether a profile with the given name is already stored.
// Profiles are looked up by name in the UI, so duplicates would make the
// details pane (and anything else name-keyed) resolve to the wrong profile.
func (p *Params) NameExists(name string) bool {
	for _, c := range p.Config {
		if c != nil && c.Name == name {
			return true
		}
	}
	return false
}

func (p *Params) NewConfiguration(config *Config) error {
	if config.Name == "" {
		return errors.New("empty name not allowed")
	}
	if p.NameExists(config.Name) {
		return fmt.Errorf("a profile named %q already exists", config.Name)
	}

	return p.adopt(append(p.Config[:len(p.Config):len(p.Config)], config))
}

// adopt saves next as the profile list and keeps it only if the save worked.
// Appending first and saving second left a failed create or copy (full disk,
// read-only directory) listed anyway: openable, refusing a retry under the
// same name as "already exists", and written out by the next unrelated save.
func (p *Params) adopt(next []*Config) error {
	prev := p.Config
	p.Config = next
	if err := p.WriteConfig(); err != nil {
		p.Config = prev
		return err
	}
	return nil
}

func LoadConfiguration(fileName string) ([]*Config, error) {
	var config []*Config
	configFile, err := os.ReadFile(fileName)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", fileName, err)
	}
	if err := json.Unmarshal(configFile, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", fileName, err)
	}
	// A null entry ("[null]", a stray comma fixed by hand) decodes as a nil
	// profile, and every screen that lists profiles dereferences them: the
	// app panicked at startup instead of reaching its config-error path.
	kept := config[:0]
	for _, c := range config {
		if c != nil {
			kept = append(kept, c)
		}
	}
	return kept, nil
}

func FileExist(fileName string) (bool, error) {
	_, err := os.Stat(fileName)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err == nil {
		return true, err
	}
	return false, err
}

// CopyConfig stores conf as a new profile.
//
// conf arrives as a shallow copy of the profile being duplicated, so its
// reference fields still point into the original's. Bookmarks is the one that
// bites: the two slices share a backing array, and whichever profile appends
// into the spare capacity second overwrites the entry the first just added —
// silently, since both slices stay the right length. Clone them here, where
// the copy is made, rather than relying on every caller to remember.
func (p *Params) CopyConfig(conf Config) error {
	if p.NameExists(conf.Name) {
		return fmt.Errorf("a profile named %q already exists", conf.Name)
	}
	return p.adopt(append(p.Config[:len(p.Config):len(p.Config)], conf.clone()))
}

// clone returns an independent copy of c: no slice or pointer field is shared
// with the original, so edits to either cannot reach the other.
func (c *Config) clone() *Config {
	out := *c
	if c.Bookmarks != nil {
		out.Bookmarks = make([]Bookmark, len(c.Bookmarks))
		copy(out.Bookmarks, c.Bookmarks)
	}
	if c.Region != nil {
		region := *c.Region
		out.Region = &region
	}
	return &out
}

// DeleteConfig removes the profile at position i. The index comes from the
// profiles list widget, captured when the confirmation opened, so it is
// checked rather than trusted.
//
// The in-memory list is only adopted once the save succeeded. Dropping the
// entry first left the UI showing a profile that the slice no longer held
// whenever the write failed, and every later action addresses a profile by
// its row — so the next edit or delete indexed past the end and took the
// terminal down with it.
func (p *Params) DeleteConfig(i int) error {
	if i < 0 || i >= len(p.Config) {
		return fmt.Errorf("no profile at position %d", i)
	}

	kept := make([]*Config, 0, len(p.Config)-1)
	kept = append(kept, p.Config[:i]...)
	kept = append(kept, p.Config[i+1:]...)

	prev := p.Config
	p.Config = kept
	if err := p.WriteConfig(); err != nil {
		p.Config = prev
		return err
	}
	return nil
}

func CreateEmptyConfig(configFile string) error {
	if err := os.MkdirAll(filepath.Dir(configFile), 0700); err != nil {
		return err
	}
	a, _ := json.Marshal(make([]Config, 0))
	return os.WriteFile(configFile, a, 0600)
}

func NewParams() *Params {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return &Params{LoadErr: fmt.Errorf("can't get user home dir: %w", err)}
	}

	configFile := path.Join(homeDir, configFolder, configDirName, filename)
	params := &Params{HomeDir: homeDir, FileName: configFile}

	exists, err := FileExist(configFile)
	if err != nil {
		params.LoadErr = err
		return params
	}
	if !exists {
		if err := CreateEmptyConfig(configFile); err != nil {
			params.LoadErr = fmt.Errorf("failed to create config: %w", err)
			return params
		}
	}

	// The file holds secret keys and is documented as 0600, but only a save
	// wrote that mode: a hand-made config.json, or one from builds that
	// wrote it 0700, stayed as it was until the first edit. Best effort.
	_ = os.Chmod(configFile, 0600)

	config, err := LoadConfiguration(configFile)
	if err != nil {
		params.LoadErr = err
		return params
	}
	params.Config = config
	return params
}
