package router

// The status tracker's history used to be RAM-only: restarting the router reset
// the window the page and /v1/status report. These tests pin the file-backed
// half against the failure that matters - a restart losing the history - and
// against the ways a file could lie about it: an expired hour coming back, a
// document from another build being half-read, a corrupt file stopping the
// router from recording anything at all.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestStatusHistorySurvivesARestart records traffic into one tracker, saves,
// then loads it into a fresh tracker - the restart - and checks every rollup
// against the hand count, the model+provider split included, because that is
// the one only this file can carry: it is four maps deep and a lost level is
// invisible on the page (the split just renders empty).
func TestStatusHistorySurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	at := time.Now().UTC().Add(-30 * time.Minute)

	first := NewStatusTracker(7 * 24 * time.Hour)
	if _, err := first.UseFile(path); err != nil {
		t.Fatalf("UseFile: %v", err)
	}
	for i := 0; i < 3; i++ {
		first.Record(true, 200, at, "gpt-x", "alpha")
	}
	first.Record(false, 502, at, "gpt-x", "beta")
	first.Record(true, 200, at) // no model or provider: global only
	// Traffic that aged out before the save, and a model whose only traffic
	// did: neither may come back from the file.
	first.Record(true, 200, at.Add(-9*24*time.Hour))
	first.Record(true, 200, at.Add(-9*24*time.Hour), "retired", "alpha")
	if err := first.SaveIfDirty(); err != nil {
		t.Fatalf("SaveIfDirty: %v", err)
	}

	second := NewStatusTracker(7 * 24 * time.Hour)
	hours, err := second.UseFile(path)
	if err != nil {
		t.Fatalf("UseFile after a restart: %v", err)
	}
	if hours != 1 {
		t.Errorf("restored hours = %d, want 1: the one hour with traffic inside the window", hours)
	}

	snap := second.Snapshot()
	if got := statusInt(t, snap, "total"); got != 5 {
		t.Errorf("total after the restart = %d, want 5", got)
	}
	if got := statusInt(t, snap, "failed"); got != 1 {
		t.Errorf("failed after the restart = %d, want 1", got)
	}

	models := snap["modelStats"].([]map[string]any)
	if got := entryNames(models); len(got) != 1 || got[0] != "gpt-x" {
		t.Fatalf("modelStats after the restart = %v, want [gpt-x] (the retired model must not come back)", got)
	}
	x := statusEntryByName(t, models, "gpt-x")
	if got := statusInt(t, x, "total"); got != 4 {
		t.Errorf("gpt-x total = %d, want 4", got)
	}
	if got := statusInt(t, x, "failed"); got != 1 {
		t.Errorf("gpt-x failed = %d, want 1", got)
	}

	split := x["providerStats"].([]map[string]any)
	if got := entryNames(split); len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("gpt-x providerStats after the restart = %v, want [alpha beta]", got)
	}
	if got := statusInt(t, statusEntryByName(t, split, "alpha"), "total"); got != 3 {
		t.Errorf("gpt-x/alpha total = %d, want 3", got)
	}
	if got := statusInt(t, statusEntryByName(t, split, "beta"), "total"); got != 1 {
		t.Errorf("gpt-x/beta total = %d, want 1", got)
	}

	providers := snap["providerStats"].([]map[string]any)
	if got := entryNames(providers); len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("providerStats after the restart = %v, want [alpha beta]", got)
	}
	alphaModels := statusEntryByName(t, providers, "alpha")["modelStats"].([]map[string]any)
	if got := entryNames(alphaModels); len(got) != 1 || got[0] != "gpt-x" {
		t.Errorf("alpha's models after the restart = %v, want [gpt-x]", got)
	}

	// And the restored history takes new traffic on top of it, rather than
	// being replaced by it.
	second.Record(true, 200, at, "gpt-x", "alpha")
	if got := statusInt(t, statusEntryByName(t, second.Snapshot()["modelStats"].([]map[string]any), "gpt-x"), "total"); got != 5 {
		t.Errorf("gpt-x total after new traffic = %d, want 5 (4 restored + 1)", got)
	}
}

// TestStatusFileIsOnlyWrittenWhenSomethingChanged pins the flush behaviour the
// serve loop depends on: the first flush writes a file (so its presence does
// not depend on traffic), a clean tracker rewrites nothing, and recording a
// request makes the next flush write again.
func TestStatusFileIsOnlyWrittenWhenSomethingChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")

	tr := NewStatusTracker(7 * 24 * time.Hour)
	if _, err := tr.UseFile(path); err != nil {
		t.Fatalf("UseFile: %v", err)
	}
	if !tr.Dirty() {
		t.Error("a tracker that has never written its file should be dirty")
	}
	if err := tr.SaveIfDirty(); err != nil {
		t.Fatalf("SaveIfDirty: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no file after the first flush: %v", err)
	}
	if tr.Dirty() {
		t.Error("SaveIfDirty left the tracker dirty with nothing recorded in between")
	}

	// Nothing new to say, so nothing is written: removing the file and
	// flushing again must not bring it back.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := tr.SaveIfDirty(); err != nil {
		t.Fatalf("SaveIfDirty on a clean tracker: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a clean tracker wrote its file again: stat err = %v, want not-exist", err)
	}

	tr.Record(true, 200, time.Now())
	if !tr.Dirty() {
		t.Error("recording a request did not mark the history dirty")
	}
	if err := tr.SaveIfDirty(); err != nil {
		t.Fatalf("SaveIfDirty after a record: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the recorded hour was not written: %v", err)
	}
}

// TestStatusFileFromAnotherBuildIsNotHalfRead covers the two ways a file is not
// the one this build wrote. Neither may be merged, and neither may stop the
// router: the history starts fresh and the next flush replaces the file.
func TestStatusFileFromAnotherBuildIsNotHalfRead(t *testing.T) {
	dir := t.TempDir()

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte(`{"version": 1, "buckets": {`), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := NewStatusTracker(7 * 24 * time.Hour)
	if _, err := tr.UseFile(corrupt); err == nil {
		t.Error("a truncated document should be reported, not silently ignored")
	}
	tr.Record(true, 200, time.Now())
	if got := statusInt(t, tr.Snapshot(), "total"); got != 1 {
		t.Errorf("total after a bad file = %d, want 1: recording must keep working", got)
	}
	if err := tr.SaveIfDirty(); err != nil {
		t.Fatalf("SaveIfDirty over a bad file: %v", err)
	}
	var doc statusDocument
	data, err := os.ReadFile(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("the flush did not replace the bad file with a valid one: %v", err)
	}
	if doc.Version != statusFileVersion {
		t.Errorf("rewritten file version = %d, want %d", doc.Version, statusFileVersion)
	}

	newer := filepath.Join(dir, "newer.json")
	if err := os.WriteFile(newer, []byte(`{"version": 99, "buckets": {"1": {"total": 5, "success": 5}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh := NewStatusTracker(7 * 24 * time.Hour)
	if _, err := fresh.UseFile(newer); err == nil {
		t.Error("a document from a newer router should be refused rather than read")
	}
	if got := statusInt(t, fresh.Snapshot(), "total"); got != 0 {
		t.Errorf("total after refusing a newer file = %d, want 0 (nothing merged)", got)
	}
}

// TestOlderHistoryIsRepairedOnce covers the upgrade path. A document written by
// the build whose global rollup counted a forwarded request twice is corrected
// on load - exactly, not approximately, because the excess is recoverable from
// the detail rollups - and then written back in the current format, so the
// correction cannot be applied a second time.
func TestOlderHistoryIsRepairedOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	hour := strconv.FormatInt(time.Now().UTC().Add(-30*time.Minute).Truncate(time.Hour).Unix(), 10)

	// Two forwarded requests that succeeded and one that failed, plus one
	// request that never reached a provider. The detail rollups are right: the
	// global ones hold every forwarded request twice.
	legacy := statusDocument{
		Version: statusLegacyVersion,
		SavedAt: time.Now().UTC(),
		Buckets: map[string]statusCounts{hour: {Total: 7, Success: 5, Failed: 2}},
		Models: map[string]map[string]statusCounts{
			"m": {hour: {Total: 3, Success: 2, Failed: 1}},
		},
		Providers: map[string]map[string]statusCounts{
			"P": {hour: {Total: 3, Success: 2, Failed: 1}},
		},
		Pairs: map[string]map[string]map[string]statusCounts{
			"m": {"P": {hour: {Total: 3, Success: 2, Failed: 1}}},
		},
	}
	buf, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}

	tr := NewStatusTracker(7 * 24 * time.Hour)
	hours, err := tr.UseFile(path)
	if err != nil {
		t.Fatalf("UseFile: %v", err)
	}
	if hours != 1 {
		t.Errorf("restored hours = %d, want 1", hours)
	}
	if got := tr.RepairedHours(); got != 1 {
		t.Errorf("repaired hours = %d, want 1", got)
	}

	snap := tr.Snapshot()
	if got := statusInt(t, snap, "total"); got != 4 {
		t.Errorf("total = %d, want 4: three forwarded requests and one that never reached a provider", got)
	}
	if got := statusInt(t, snap, "success"); got != 3 {
		t.Errorf("success = %d, want 3", got)
	}
	if got := statusInt(t, snap, "failed"); got != 1 {
		t.Errorf("failed = %d, want 1", got)
	}
	statusFloat(t, snap, "uptimePercent", 75)

	// The detail rollups were never wrong and are untouched.
	models := snap["modelStats"].([]map[string]any)
	if got := statusInt(t, statusEntryByName(t, models, "m"), "total"); got != 3 {
		t.Errorf("model m total = %d, want 3", got)
	}
	providers := snap["providerStats"].([]map[string]any)
	if got := statusInt(t, statusEntryByName(t, providers, "P"), "total"); got != 3 {
		t.Errorf("provider P total = %d, want 3", got)
	}

	// The repaired history is written back in the current format, and loading
	// that changes nothing: the correction is one-time, not applied per load.
	if err := tr.SaveIfDirty(); err != nil {
		t.Fatalf("SaveIfDirty: %v", err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc statusDocument
	if err := json.Unmarshal(saved, &doc); err != nil {
		t.Fatalf("the repaired document does not parse: %v", err)
	}
	if doc.Version != statusFileVersion {
		t.Errorf("saved format = %d, want %d", doc.Version, statusFileVersion)
	}

	again := NewStatusTracker(7 * 24 * time.Hour)
	if _, err := again.UseFile(path); err != nil {
		t.Fatalf("UseFile after the repair: %v", err)
	}
	if got := again.RepairedHours(); got != 0 {
		t.Errorf("repaired hours on the second load = %d, want 0: a repaired document must not be repaired again", got)
	}
	snap = again.Snapshot()
	if got := statusInt(t, snap, "total"); got != 4 {
		t.Errorf("total after a second load = %d, want 4 unchanged", got)
	}
	if got := statusInt(t, snap, "success"); got != 3 {
		t.Errorf("success after a second load = %d, want 3 unchanged", got)
	}
}

// TestStatusFileDoesNotWidenTheWindow checks that the retention window belongs
// to the running router, not to the file: a document holding three days of
// history, loaded by a router configured to report one day, contributes only
// the hours inside that day.
func TestStatusFileDoesNotWidenTheWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	now := time.Now().UTC()

	writer := NewStatusTracker(7 * 24 * time.Hour)
	if _, err := writer.UseFile(path); err != nil {
		t.Fatal(err)
	}
	writer.Record(true, 200, now.Add(-2*time.Hour))
	writer.Record(true, 200, now.Add(-70*time.Hour))
	if err := writer.SaveIfDirty(); err != nil {
		t.Fatal(err)
	}

	reader := NewStatusTracker(24 * time.Hour)
	hours, err := reader.UseFile(path)
	if err != nil {
		t.Fatalf("UseFile: %v", err)
	}
	if hours != 1 {
		t.Errorf("restored hours = %d, want 1: the 70-hour-old bucket is outside a one-day window", hours)
	}
	if got := statusInt(t, reader.Snapshot(), "total"); got != 1 {
		t.Errorf("total = %d, want 1", got)
	}
}
