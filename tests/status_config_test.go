package tests

// The request history behind /status and /v1/status is kept on disk so a
// restart does not reset the window. The config shape is what decides that, and
// the config a user already has predates the section entirely: updating the
// binary must not require editing the file for the history to start surviving.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A config with no "status" section at all - every config written before this
// existed - keeps its history: the default is on, in the data directory the
// rest of the tool uses.
func TestConfigWithoutAStatusSectionKeepsItsHistory(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `{"provider":{"P":{"options":{"baseURL":"http://x/v1"},"models":{"m":{}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.StatusEnabled() {
		t.Error("an older config has status persistence off; a restart would throw the history away")
	}
	if got := cfg.StatusFilePath(); got != defaultStatusPath {
		t.Errorf("status file = %q, want %q", got, defaultStatusPath)
	}
	if got, want := cfg.StatusSaveEvery(), time.Duration(defaultStatusSaveSecs)*time.Second; got != want {
		t.Errorf("save interval = %v, want %v", got, want)
	}
}

// The section is read as written: off, a file of its own, and the negative
// saveSeconds that means "only at shutdown".
func TestStatusSectionIsHonoured(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `{"status":{"enabled":false},"provider":{"P":{"options":{"baseURL":"http://x/v1"},"models":{"m":{}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StatusEnabled() {
		t.Error("status.enabled false was ignored")
	}
	if got := cfg.StatusFilePath(); got != "" {
		t.Errorf("status file = %q, want \\\"\\\" with persistence off", got)
	}
	if got := cfg.StatusSaveEvery(); got != 0 {
		t.Errorf("save interval = %v, want 0 with persistence off", got)
	}

	cfg, err = LoadConfig(writeConfig(t, `{"status":{"file":"~/elsewhere/history.json","saveSeconds":120},"provider":{"P":{"options":{"baseURL":"http://x/v1"},"models":{"m":{}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.StatusFilePath(); got != "~/elsewhere/history.json" {
		t.Errorf("status file = %q, want the configured path (expanded by the caller)", got)
	}
	if got := cfg.StatusSaveEvery(); got != 120*time.Second {
		t.Errorf("save interval = %v, want 2m", got)
	}

	cfg, err = LoadConfig(writeConfig(t, `{"status":{"saveSeconds":-1},"provider":{"P":{"options":{"baseURL":"http://x/v1"},"models":{"m":{}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.StatusSaveEvery(); got != 0 {
		t.Errorf("save interval = %v, want 0 for a negative saveSeconds (shutdown only)", got)
	}
	if got := cfg.StatusFilePath(); got != defaultStatusPath {
		t.Errorf("status file = %q, want the default when only saveSeconds is set", got)
	}
}

// The shipped example has to be a config someone can copy into place, so its
// status section has to parse and mean what the docs say.
func TestShippedExampleConfigCarriesTheStatusSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.jsonc")
	raw, err := os.ReadFile("../noderouter.jsonc")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("the shipped example does not load: %v", err)
	}
	if !cfg.StatusEnabled() {
		t.Error("the shipped example turns status persistence off")
	}
	if got := cfg.StatusFilePath(); got != defaultStatusPath {
		t.Errorf("shipped example status file = %q, want %q", got, defaultStatusPath)
	}
}
