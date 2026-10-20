package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A "file:" entry is parsed once and reused while the file stands still, so a
// retry loop does not open it once per attempt. The stat is what decides: a
// rewrite that lands on the same size and timestamp is invisible to it, and the
// cached parse is what answers.
func TestAPIKeyFileIsParsedOnceUntilItChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.txt")
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)

	if err := os.WriteFile(path, []byte("sk-old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	p := &Provider{Options: ProviderOptions{APIKey: "file:" + path}}
	if got := p.APIKeyFor(0); got != "sk-old" {
		t.Fatalf("apiKeyFor(0) = %q, want sk-old", got)
	}

	// Same length, timestamp put back where it was: nothing stat can see has
	// changed, so the cached keys stand.
	if err := os.WriteFile(path, []byte("sk-new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if got := p.APIKeyFor(0); got != "sk-old" {
		t.Errorf("unchanged file served %q, want the cached sk-old", got)
	}

	// A moved timestamp is a rotation, and it must be picked up.
	next := stamp.Add(time.Second)
	if err := os.Chtimes(path, next, next); err != nil {
		t.Fatal(err)
	}
	if got := p.APIKeyFor(0); got != "sk-new" {
		t.Errorf("rotated key = %q, want sk-new", got)
	}
}

// Rotations happen after the router is already running, so a key file that does
// not exist yet must not be remembered as missing.
func TestAPIKeyFileCreatedLaterIsStillRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "later.key")
	p := &Provider{Options: ProviderOptions{APIKey: "file:" + path}}

	for i := 0; i < 3; i++ {
		if got := p.APIKeyFor(i); got != "" {
			t.Fatalf("missing key file = %q, want empty", got)
		}
	}

	if err := os.WriteFile(path, []byte("sk-late\n# and a spare\nsk-late-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := p.APIKeyFor(0); got != "sk-late" {
		t.Errorf("key file created later = %q, want sk-late", got)
	}
	if got := p.APIKeyFor(1); got != "sk-late-2" {
		t.Errorf("second key = %q, want sk-late-2", got)
	}
}

// More key files than the cache holds: eviction is a miss, never a wrong or
// empty answer.
func TestMoreKeyFilesThanTheCacheHolds(t *testing.T) {
	dir := t.TempDir()
	providers := map[string]*Provider{}
	for i := 0; i < 80; i++ {
		name := fmt.Sprintf("P%02d", i)
		path := filepath.Join(dir, name+".key")
		if err := os.WriteFile(path, []byte("sk-"+name), 0o600); err != nil {
			t.Fatal(err)
		}
		providers[name] = &Provider{Options: ProviderOptions{APIKey: "file:" + path}}
	}

	for name, p := range providers {
		if got, want := p.APIKeyFor(0), "sk-"+name; got != want {
			t.Errorf("%s apiKeyFor(0) = %q, want %q", name, got, want)
		}
	}
}

// Every request shares the cache, so the same file must answer the same for all
// of them. Run with -race for this to mean anything.
func TestAPIKeyFileCacheIsConcurrencySafe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.txt")
	if err := os.WriteFile(path, []byte("sk-shared"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &Provider{Options: ProviderOptions{APIKey: "file:" + path}}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if got := p.APIKeyFor(j); got != "sk-shared" {
					t.Errorf("concurrent apiKeyFor = %q, want sk-shared", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}
