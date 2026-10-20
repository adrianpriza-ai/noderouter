package router

// The status tracker keeps four rollups of the same traffic: global, per model,
// per provider, and per model+provider. The pair rollup is the one the public
// page reads to show how a model's requests split across the providers that
// served it, and it is the only one that cannot be checked by eye - the numbers
// come from the same records as the other three, so a missing or misspelled key
// would render a split of nothing without failing anything.
//
// These tests pin the split against hand-counted traffic, and pin the window:
// a pair with nothing left inside the retention window is gone from both the
// model's split and the provider's model list.

import (
	"sync"
	"testing"
	"time"
)

func statusEntryByName(t *testing.T, list []map[string]any, name string) map[string]any {
	t.Helper()
	for _, e := range list {
		if e["name"] == name {
			return e
		}
	}
	t.Fatalf("no entry named %q in %v", name, entryNames(list))
	return nil
}

func entryNames(list []map[string]any) []string {
	names := make([]string, 0, len(list))
	for _, e := range list {
		names = append(names, e["name"].(string))
	}
	return names
}

func statusInt(t *testing.T, entry map[string]any, key string) int {
	t.Helper()
	v, ok := entry[key].(int)
	if !ok {
		t.Fatalf("%v: %q = %#v, want an int", entry["name"], key, entry[key])
	}
	return v
}

func statusFloat(t *testing.T, entry map[string]any, key string, want float64) {
	t.Helper()
	got, ok := entry[key].(float64)
	if !ok {
		t.Fatalf("%v: %q = %#v, want a number", entry["name"], key, entry[key])
	}
	if diff := got - want; diff > 0.01 || diff < -0.01 {
		t.Errorf("%v: %q = %v, want %v", entry["name"], key, got, want)
	}
}

// TestStatusSnapshotSplitsAModelAcrossProviders records one model through two
// providers and checks every rollup against the hand count.
func TestStatusSnapshotSplitsAModelAcrossProviders(t *testing.T) {
	tr := NewStatusTracker(7 * 24 * time.Hour)
	at := time.Now().UTC().Add(-30 * time.Minute)

	for i := 0; i < 3; i++ {
		tr.Record(true, 200, at, "gpt-x", "alpha")
	}
	tr.Record(false, 502, at, "gpt-x", "beta")
	tr.Record(true, 200, at, "gpt-y", "beta")
	tr.Record(true, 200, at) // no model or provider: global only
	// An old pair must not survive into the split.
	tr.Record(true, 200, at.Add(-8*24*time.Hour), "retired-model", "alpha")

	snap := tr.Snapshot()

	// Global: six requests in the window (five with a model or provider, one
	// with neither), one of them failed.
	if got := statusInt(t, snap, "total"); got != 6 {
		t.Errorf("global total = %d, want 6", got)
	}
	if got := statusInt(t, snap, "failed"); got != 1 {
		t.Errorf("global failed = %d, want 1", got)
	}
	statusFloat(t, snap, "uptimePercent", 100*5/6.0)

	models := snap["modelStats"].([]map[string]any)
	if got := entryNames(models); len(got) != 2 || got[0] != "gpt-x" || got[1] != "gpt-y" {
		t.Fatalf("modelStats names = %v, want [gpt-x gpt-y] (sorted, and the retired model pruned)", got)
	}
	x := statusEntryByName(t, models, "gpt-x")
	if got := statusInt(t, x, "total"); got != 4 {
		t.Errorf("gpt-x total = %d, want 4", got)
	}
	if got := statusInt(t, x, "failed"); got != 1 {
		t.Errorf("gpt-x failed = %d, want 1", got)
	}

	// The split is the point: three clean requests through alpha, one failure
	// through beta, each with its own uptime rather than the model's 75%.
	if got := x["providers"].([]string); len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("gpt-x providers = %v, want [alpha beta]", got)
	}
	split := x["providerStats"].([]map[string]any)
	if len(split) != 2 {
		t.Fatalf("gpt-x providerStats = %v, want alpha and beta", entryNames(split))
	}
	alpha := statusEntryByName(t, split, "alpha")
	if got := statusInt(t, alpha, "total"); got != 3 {
		t.Errorf("gpt-x/alpha total = %d, want 3", got)
	}
	statusFloat(t, alpha, "uptimePercent", 100)
	beta := statusEntryByName(t, split, "beta")
	if got := statusInt(t, beta, "total"); got != 1 {
		t.Errorf("gpt-x/beta total = %d, want 1", got)
	}
	statusFloat(t, beta, "uptimePercent", 0)

	// A day and an hour are carried for the pair too, so the split can be
	// read at the same resolution as everything else.
	if days := alpha["days"].([]map[string]any); len(days) != 7 {
		t.Errorf("gpt-x/alpha days = %d entries, want the 7 day window", len(days))
	}
	if hours := alpha["hours"].([]map[string]any); len(hours) != 1 {
		t.Errorf("gpt-x/alpha hours = %d entries, want the one hour with traffic", len(hours))
	}

	// The providers view carries each model's numbers through that provider,
	// not the model's numbers everywhere: gpt-x appears under both, at 3 and
	// at 1, rather than twice at 4.
	providers := snap["providerStats"].([]map[string]any)
	if got := entryNames(providers); len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("providerStats names = %v, want [alpha beta]", got)
	}
	alphaModels := statusEntryByName(t, providers, "alpha")["modelStats"].([]map[string]any)
	if got := entryNames(alphaModels); len(got) != 1 || got[0] != "gpt-x" {
		t.Fatalf("alpha modelStats = %v, want [gpt-x] only", got)
	}
	if got := statusInt(t, alphaModels[0], "total"); got != 3 {
		t.Errorf("alpha's gpt-x total = %d, want 3 (alpha's own traffic)", got)
	}
	betaModels := statusEntryByName(t, providers, "beta")["modelStats"].([]map[string]any)
	if got := entryNames(betaModels); len(got) != 2 || got[0] != "gpt-x" || got[1] != "gpt-y" {
		t.Fatalf("beta modelStats = %v, want [gpt-x gpt-y]", got)
	}
	if got := statusInt(t, statusEntryByName(t, betaModels, "gpt-x"), "total"); got != 1 {
		t.Errorf("beta's gpt-x total = %d, want 1", got)
	}

	// The day the traffic landed on rolls up to the same six requests, so the
	// week track and the split cannot disagree.
	day := at.Format("2006-01-02")
	days := snap["days"].([]map[string]any)
	if len(days) != 7 {
		t.Fatalf("days = %d entries, want 7", len(days))
	}
	for _, d := range days {
		if d["date"] != day {
			continue
		}
		if got := statusInt(t, d, "total"); got != 6 {
			t.Errorf("day %s total = %d, want 6", day, got)
		}
		return
	}
	t.Errorf("no daily bucket for %s in %v", day, days)
}

// TestStatusSnapshotEmpty covers a router that has just started: every list is
// empty rather than missing, the week is still seven labelled days, and the
// label is operational because there is nothing to report rather than because
// anything succeeded.
func TestStatusSnapshotEmpty(t *testing.T) {
	snap := NewStatusTracker(0).Snapshot() // 0 falls back to the 7 day default

	if len(snap["modelStats"].([]map[string]any)) != 0 {
		t.Error("a fresh tracker reported models")
	}
	if len(snap["providerStats"].([]map[string]any)) != 0 {
		t.Error("a fresh tracker reported providers")
	}
	if days := snap["days"].([]map[string]any); len(days) != 7 {
		t.Errorf("days = %d entries, want 7 even with no traffic", len(days))
	}
	if got := snap["status"]; got != "operational" {
		t.Errorf("status = %v, want operational for an empty window", got)
	}
	if got := snap["uptime"]; got != "no data" {
		t.Errorf("uptime = %v, want \"no data\"", got)
	}
	if got := snap["total"]; got != 0 {
		t.Errorf("total = %v, want 0", got)
	}
}

// TestLazyStatusInitIsRaceFree hammers the fallback path from goroutines at
// once, on a Server built by hand with Status left nil. Every read and the
// single lazy write of s.Status go through statusTracker, so under -race this
// fails if the lazy init ever reverts to a bare "if s.Status == nil" field
// assignment in the helpers.
func TestLazyStatusInitIsRaceFree(t *testing.T) {
	const goroutines, perGoroutine = 8, 50
	s := &Server{}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(even bool) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				s.RecordStatus(true)
				s.RecordStatusDetail(true, "m", "p")
				if even {
					_ = s.StatusSnapshot()
				}
			}
		}(g%2 == 0)
	}
	wg.Wait()

	snap := s.StatusSnapshot()
	if got := statusInt(t, snap, "total"); got != goroutines*perGoroutine {
		t.Errorf("total = %d, want %d", got, goroutines*perGoroutine)
	}
}
