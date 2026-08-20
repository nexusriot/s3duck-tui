package config

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestParams(t *testing.T) *Params {
	t.Helper()
	dir := t.TempDir()
	return &Params{
		HomeDir:  dir,
		FileName: filepath.Join(dir, "config.json"),
		Config:   nil,
	}
}

func TestFileExist(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "missing.json")
	if ok, err := FileExist(missing); ok || err != nil {
		t.Errorf("FileExist(missing) = (%v, %v), want (false, nil)", ok, err)
	}

	present := filepath.Join(dir, "present.json")
	CreateEmptyConfig(present)
	if ok, err := FileExist(present); !ok || err != nil {
		t.Errorf("FileExist(present) = (%v, %v), want (true, nil)", ok, err)
	}
}

func TestNewConfigurationRejectsEmptyName(t *testing.T) {
	p := newTestParams(t)
	if err := p.NewConfiguration(&Config{Name: ""}); err == nil {
		t.Errorf("NewConfiguration with empty name = nil error, want error")
	}
	if len(p.Config) != 0 {
		t.Errorf("len(Config) = %d after rejected entry, want 0", len(p.Config))
	}
}

func TestConfigRoundTrip(t *testing.T) {
	p := newTestParams(t)
	region := "eu-central-1"

	err := p.NewConfiguration(&Config{
		Name:      "prod",
		BaseUrl:   "https://s3.example.com",
		Region:    &region,
		AccessKey: "ak",
		SecretKey: "sk",
		IgnoreSsl: true,
	})
	if err != nil {
		t.Fatalf("NewConfiguration: %v", err)
	}
	if len(p.Config) != 1 {
		t.Fatalf("len(Config) = %d, want 1", len(p.Config))
	}

	loaded, err := LoadConfiguration(p.FileName)
	if err != nil {
		t.Fatalf("LoadConfiguration: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("LoadConfiguration returned %d entries, want 1", len(loaded))
	}
	got := loaded[0]
	if got.Name != "prod" || got.BaseUrl != "https://s3.example.com" {
		t.Errorf("loaded entry mismatch: %+v", got)
	}
	if got.Region == nil || *got.Region != region {
		t.Errorf("loaded Region = %v, want %q", got.Region, region)
	}
	if !got.IgnoreSsl {
		t.Errorf("loaded IgnoreSsl = false, want true")
	}
}

func TestCopyAndDeleteConfig(t *testing.T) {
	p := newTestParams(t)
	if err := p.NewConfiguration(&Config{Name: "a"}); err != nil {
		t.Fatal(err)
	}

	p.CopyConfig(Config{Name: "a_copy"})
	if len(p.Config) != 2 {
		t.Fatalf("after CopyConfig len = %d, want 2", len(p.Config))
	}
	if reloaded, _ := LoadConfiguration(p.FileName); len(reloaded) != 2 {
		t.Errorf("persisted entries = %d, want 2", len(reloaded))
	}

	p.DeleteConfig(0)
	if len(p.Config) != 1 {
		t.Fatalf("after DeleteConfig len = %d, want 1", len(p.Config))
	}
	if p.Config[0].Name != "a_copy" {
		t.Errorf("remaining entry = %q, want %q", p.Config[0].Name, "a_copy")
	}
	if reloaded, _ := LoadConfiguration(p.FileName); len(reloaded) != 1 || reloaded[0].Name != "a_copy" {
		t.Errorf("persisted state after delete = %+v, want single 'a_copy'", reloaded)
	}
}

func TestCreateEmptyConfigCreatesParentDirs(t *testing.T) {
	nested := filepath.Join(t.TempDir(), "a", "b", "config.json")
	CreateEmptyConfig(nested)

	if ok, err := FileExist(nested); !ok || err != nil {
		t.Fatalf("FileExist(nested) = (%v, %v), want (true, nil)", ok, err)
	}
	if entries, _ := LoadConfiguration(nested); len(entries) != 0 {
		t.Errorf("empty config loaded %d entries, want 0", len(entries))
	}
}

func TestDuplicateProfileNamesRejected(t *testing.T) {
	p := newTestParams(t)
	if err := p.NewConfiguration(&Config{Name: "prod"}); err != nil {
		t.Fatal(err)
	}

	// Profiles are resolved by name in the UI, so a duplicate would make the
	// details pane show one profile while opening another.
	if err := p.NewConfiguration(&Config{Name: "prod"}); err == nil {
		t.Error("NewConfiguration accepted a duplicate name")
	}
	if err := p.CopyConfig(Config{Name: "prod"}); err == nil {
		t.Error("CopyConfig accepted a duplicate name")
	}
	if len(p.Config) != 1 {
		t.Errorf("len(Config) = %d after rejected duplicates, want 1", len(p.Config))
	}
}

func TestWriteConfigIsAtomic(t *testing.T) {
	p := newTestParams(t)
	if err := p.NewConfiguration(&Config{Name: "one"}); err != nil {
		t.Fatal(err)
	}

	// The write-then-rename must not leave its temp file behind.
	if ok, _ := FileExist(p.FileName + ".tmp"); ok {
		t.Error("temp file left behind after WriteConfig")
	}
	if loaded, err := LoadConfiguration(p.FileName); err != nil || len(loaded) != 1 {
		t.Fatalf("persisted state = %v, %v", loaded, err)
	}
}

func TestWriteConfigBacksUpUnreadableOriginal(t *testing.T) {
	p := newTestParams(t)

	// Simulate a corrupt config: the load failed, so the in-memory list does
	// NOT reflect the file. Saving must preserve the original for manual
	// recovery instead of silently wiping every stored profile.
	if err := os.WriteFile(p.FileName, []byte(`[{"name": "prod", "access_`), 0600); err != nil {
		t.Fatal(err)
	}
	_, loadErr := LoadConfiguration(p.FileName)
	if loadErr == nil {
		t.Fatal("expected the truncated config to fail loading")
	}
	p.LoadErr = loadErr

	if err := p.NewConfiguration(&Config{Name: "fresh"}); err != nil {
		t.Fatalf("saving after a load error: %v", err)
	}

	backup, err := os.ReadFile(p.FileName + ".bak")
	if err != nil {
		t.Fatalf("no backup of the unreadable original: %v", err)
	}
	if string(backup) != `[{"name": "prod", "access_` {
		t.Errorf("backup content = %q", backup)
	}
	if p.LoadErr != nil {
		t.Error("LoadErr should be cleared once the state is safely persisted")
	}
	if loaded, err := LoadConfiguration(p.FileName); err != nil || len(loaded) != 1 || loaded[0].Name != "fresh" {
		t.Errorf("new state = %+v, %v", loaded, err)
	}
}
