package tests

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A re-validation window that is deliberately large, so tests can decide which
// entries are stale without depending on the default.
const testTTL = 6 * time.Hour

func age(d time.Duration) time.Time { return time.Now().Add(-d).UTC() }

// revalidateConfig points a config at the temp list file and the local test
// endpoint, so Revalidate reads and writes exactly that file.
func revalidateConfig(t *testing.T, listPath, testURL string) *Config {
	t.Helper()
	cfg := &Config{}
	cfg.ApplyDefaults()
	cfg.Proxy.File = listPath
	cfg.Proxy.Test.URLs = []string{testURL}
	cfg.Proxy.Test.Workers = 4
	cfg.Proxy.Test.TimeoutMS = 2000
	return cfg
}

// A list checked inside the window costs nothing: no proxy is dialled and the
// file is left byte for byte as it was. This is what keeps a 6h schedule from
// hammering the pool every time the router restarts.
func TestRevalidateLeavesFreshListAlone(t *testing.T) {
	var hits atomic.Int64
	target := hitCounter(t, &hits)

	list := filepath.Join(t.TempDir(), "ports.txt")
	entries := []ProxyEntry{
		{URL: "http://127.0.0.1:9", CheckedAt: age(1 * time.Hour)},
		{URL: "http://127.0.0.1:10", CheckedAt: age(3 * time.Hour)},
	}
	if err := SaveProxyList(entries, list, metaPathFor(list)); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, list)

	cfg := revalidateConfig(t, list, target.URL)
	res, err := Revalidate(context.Background(), cfg, true, false, nil)
	if err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if res.Stale != 0 || res.Fresh != 2 {
		t.Errorf("stale=%d fresh=%d, want 0/2", res.Stale, res.Fresh)
	}
	if res.Written {
		t.Error("a fresh list was written back anyway")
	}
	if got := readFile(t, list); got != before {
		t.Errorf("the file changed:\n%s\nwant:\n%s", got, before)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("test endpoint hit %d times, want 0 for a fresh list", n)
	}
}

// The pass the schedule runs: the expired entry is re-measured, the dead one
// drops out, the fresh one keeps its place and its old numbers, and the file on
// disk carries the new checkedAt so the next pass skips it too.
func TestRevalidateRewritesOnlyExpiredEntries(t *testing.T) {
	target := ipTarget(t)

	list := filepath.Join(t.TempDir(), "ports.txt")
	good, _ := fakeProxy(t)
	freshOne, _ := fakeProxy(t)
	entries := []ProxyEntry{
		{URL: good, LatencyMS: 40, CheckedAt: age(30 * time.Hour)}, // stale, still alive
		{URL: "http://127.0.0.1:11", LatencyMS: 5, CheckedAt: age(30 * time.Hour)},
		{URL: freshOne, CheckedAt: age(1 * time.Hour)}, // fresh: never touched
	}
	if err := SaveProxyList(entries, list, metaPathFor(list)); err != nil {
		t.Fatal(err)
	}

	cfg := revalidateConfig(t, list, target.URL)
	res, err := Revalidate(context.Background(), cfg, true, false, nil)
	if err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if res.Stale != 2 || res.Fresh != 1 {
		t.Fatalf("stale=%d fresh=%d, want 2/1", res.Stale, res.Fresh)
	}
	if res.Alive != 1 || res.Dropped != 1 {
		t.Errorf("alive=%d dropped=%d, want 1/1", res.Alive, res.Dropped)
	}
	if !res.Written {
		t.Error("the survivors were not written back")
	}
	if len(res.Entries) != 2 {
		t.Fatalf("merged %d entries, want 2: %+v", len(res.Entries), res.Entries)
	}
	// Order follows the previous file, so the fastest-first ranking survives.
	if res.Entries[0].URL != good {
		t.Errorf("first entry = %q, want the re-tested %q", res.Entries[0].URL, good)
	}
	if time.Since(res.Entries[0].CheckedAt) > time.Minute {
		t.Errorf("re-tested entry kept checkedAt %v, so it would be re-tested again", res.Entries[0].CheckedAt)
	}
	if res.Entries[1].CheckedAt != entries[2].CheckedAt {
		t.Errorf("fresh entry was rewritten: %v", res.Entries[1].CheckedAt)
	}
	if res.Entries[1].URL != freshOne {
		t.Errorf("second entry = %q, want the fresh %q", res.Entries[1].URL, freshOne)
	}

	reloaded, err := LoadProxyList(list, metaPathFor(list))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range reloaded {
		switch e.URL {
		case good:
			if time.Since(e.CheckedAt) > time.Minute {
				t.Errorf("checkedAt not refreshed on disk: %v", e.CheckedAt)
			}
			if e.LatencyMS == 0 || e.ExitIP == "" {
				t.Errorf("re-tested entry lost its measurement: %+v", e)
			}
		case freshOne:
			if !e.CheckedAt.Equal(entries[2].CheckedAt) {
				t.Errorf("fresh entry was re-measured: %v", e.CheckedAt)
			}
		case "http://127.0.0.1:11":
			t.Errorf("dead entry survived on disk")
		default:
			t.Errorf("unexpected entry %q on disk", e.URL)
		}
	}
}

// A whole pool that answers nothing is not a pool: writing it out would empty
// the ports file, and the router would silently go direct. The previous list
// stays instead.
func TestRevalidateKeepsListWhenNothingSurvives(t *testing.T) {
	target := ipTarget(t)

	list := filepath.Join(t.TempDir(), "ports.txt")
	entries := []ProxyEntry{
		{URL: "http://127.0.0.1:12", CheckedAt: age(30 * time.Hour)},
		{URL: "http://127.0.0.1:13", CheckedAt: age(30 * time.Hour)},
	}
	if err := SaveProxyList(entries, list, metaPathFor(list)); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, list)

	cfg := revalidateConfig(t, list, target.URL)
	res, err := Revalidate(context.Background(), cfg, true, false, nil)
	if err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if res.Alive != 0 || res.Dropped != 2 {
		t.Errorf("alive=%d dropped=%d, want 0/2", res.Alive, res.Dropped)
	}
	if res.Written {
		t.Error("an empty list was written, which reads as 'no proxies'")
	}
	if got := readFile(t, list); got != before {
		t.Errorf("the file was emptied:\n%s\nwant:\n%s", got, before)
	}
}

// -force ignores the window, which is how a list someone just wrote by hand
// gets tested even though it claims to be fresh.
func TestRevalidateForceRetestsFreshEntries(t *testing.T) {
	target := ipTarget(t)

	list := filepath.Join(t.TempDir(), "ports.txt")
	good, hits := fakeProxy(t)
	entries := []ProxyEntry{
		{URL: good, CheckedAt: age(2 * time.Minute)},
		{URL: "http://127.0.0.1:14", CheckedAt: age(2 * time.Minute)},
	}
	if err := SaveProxyList(entries, list, metaPathFor(list)); err != nil {
		t.Fatal(err)
	}

	cfg := revalidateConfig(t, list, target.URL)
	res, err := Revalidate(context.Background(), cfg, true, true, nil)
	if err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if res.Stale != 2 || res.Fresh != 0 {
		t.Errorf("stale=%d fresh=%d, want 2/0 for a forced pass", res.Stale, res.Fresh)
	}
	if hits == nil || hits.Load() == 0 {
		t.Error("the fake proxy was never dialled with -force")
	}
	if res.Dropped != 1 || len(res.Entries) != 1 {
		t.Errorf("dropped=%d entries=%d, want 1 and 1", res.Dropped, len(res.Entries))
	}
}

// check -save=false measures and reports without touching the file.
func TestRevalidateWithoutSaveLeavesTheFile(t *testing.T) {
	target := ipTarget(t)

	list := filepath.Join(t.TempDir(), "ports.txt")
	good, _ := fakeProxy(t)
	entries := []ProxyEntry{{URL: good, CheckedAt: age(30 * time.Hour)}}
	if err := SaveProxyList(entries, list, metaPathFor(list)); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, list)

	cfg := revalidateConfig(t, list, target.URL)
	res, err := Revalidate(context.Background(), cfg, false, false, nil)
	if err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if res.Written {
		t.Error("save=false wrote the list anyway")
	}
	if got := readFile(t, list); got != before {
		t.Errorf("the file changed with save=false:\n%s", got)
	}
	if len(res.Entries) != 1 {
		t.Errorf("merged %d entries, want the one live proxy", len(res.Entries))
	}
}

// An empty or missing list is an error, not a silent success that leaves the
// scheduler believing the pool was re-checked.
func TestRevalidateRejectsEmptyList(t *testing.T) {
	cfg := revalidateConfig(t, filepath.Join(t.TempDir(), "ports.txt"), "http://127.0.0.1:1")
	if _, err := Revalidate(context.Background(), cfg, true, false, nil); err == nil {
		t.Error("a missing list should be an error")
	}

	empty := filepath.Join(t.TempDir(), "ports.txt")
	if err := os.WriteFile(empty, []byte("# nothing here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Proxy.File = empty
	if _, err := Revalidate(context.Background(), cfg, true, false, nil); err == nil {
		t.Error("a list with no usable proxies should be an error")
	}
}

// The scheduled run logs what it did: a pass that dialled nothing because
// everything was fresh is silent, and a pass that changed the pool is not.
func TestRevalidateReportsProgress(t *testing.T) {
	target := ipTarget(t)

	list := filepath.Join(t.TempDir(), "ports.txt")
	good, _ := fakeProxy(t)
	if err := SaveProxyList([]ProxyEntry{{URL: good, CheckedAt: age(30 * time.Hour)}}, list, metaPathFor(list)); err != nil {
		t.Fatal(err)
	}

	var got []string
	notify := func(msg string, args ...any) { got = append(got, msg) }
	cfg := revalidateConfig(t, list, target.URL)
	if _, err := Revalidate(context.Background(), cfg, true, false, notify); err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"re-checking proxies", "proxy pool re-checked"} {
		if !strings.Contains(joined, want) {
			t.Errorf("progress lines %q are missing %q", joined, want)
		}
	}

	// A fresh list must not claim to have re-checked anything.
	got = nil
	if err := SaveProxyList([]ProxyEntry{{URL: good, CheckedAt: age(1 * time.Hour)}}, list, metaPathFor(list)); err != nil {
		t.Fatal(err)
	}
	if _, err := Revalidate(context.Background(), cfg, true, false, notify); err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a fresh list logged %q, want nothing", got)
	}
}

// ipTarget is the local endpoint the checker dials through a proxy. It answers
// like a "what is my ip" service, so a proxy that forwards it counts as alive.
func ipTarget(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("203.0.113.7"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// hitCounter is the same endpoint with a counter, for the tests that need to
// prove the checker stayed away from the network.
func hitCounter(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprintf(w, "203.0.113.%d", hits.Load())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestSplitStale(t *testing.T) {
	now := time.Now()
	entries := []ProxyEntry{
		{URL: "http://1.1.1.1:1", CheckedAt: now.Add(-1 * time.Hour)},  // fresh
		{URL: "http://2.2.2.2:2", CheckedAt: now.Add(-7 * time.Hour)},  // stale
		{URL: "http://3.3.3.3:3", CheckedAt: now.Add(-24 * time.Hour)}, // stale
		{URL: "http://4.4.4.4:4", CheckedAt: time.Time{}},              // never checked
	}
	stale, fresh := SplitStale(entries, testTTL, now)
	if len(stale) != 3 || len(fresh) != 1 {
		t.Fatalf("stale=%d fresh=%d, want 3/1", len(stale), len(fresh))
	}
	if fresh[0].URL != "http://1.1.1.1:1" {
		t.Errorf("fresh = %q", fresh[0].URL)
	}

	// A zero window makes everything stale, which is the forced mode.
	stale, fresh = SplitStale(entries, 0, now)
	if len(stale) != 4 || len(fresh) != 0 {
		t.Errorf("ttl=0 gave stale=%d fresh=%d, want 4/0", len(stale), len(fresh))
	}

	// Exactly on the boundary counts as stale.
	boundary := now.Add(-testTTL)
	if s, _ := SplitStale([]ProxyEntry{{CheckedAt: boundary}}, testTTL, now); len(s) != 1 {
		t.Errorf("entry exactly ttl old was not treated as stale")
	}
}

func TestMergeRevalidatedKeepsFreshAndOrder(t *testing.T) {
	entries := []ProxyEntry{
		{URL: "http://a:1", LatencyMS: 10, CheckedAt: age(1 * time.Hour)}, // fresh, keep as is
		{URL: "http://b:2", LatencyMS: 20, CheckedAt: age(8 * time.Hour)}, // stale
		{URL: "http://c:3", LatencyMS: 30, CheckedAt: age(1 * time.Hour)}, // fresh, keep as is
		{URL: "http://d:4", LatencyMS: 40, CheckedAt: age(8 * time.Hour)}, // stale
	}
	stale, _ := SplitStale(entries, testTTL, time.Now())

	// Only b came back, with a new measurement. d failed and must go away.
	working := []ProxyEntry{{URL: "http://b:2", LatencyMS: 5, ExitIP: "1.2.3.4", CheckedAt: time.Now().UTC()}}

	merged, dropped := MergeRevalidated(entries, stale, working)
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
	want := []string{"http://a:1", "http://b:2", "http://c:3"}
	if len(merged) != len(want) {
		t.Fatalf("merged %d entries, want %d: %+v", len(merged), len(want), merged)
	}
	for i, u := range want {
		if merged[i].URL != u {
			t.Errorf("merged[%d] = %q, want %q (order must be preserved)", i, merged[i].URL, u)
		}
	}
	if merged[0].LatencyMS != 10 {
		t.Errorf("fresh entry was rewritten: latency %d, want 10", merged[0].LatencyMS)
	}
	if merged[1].LatencyMS != 5 || merged[1].ExitIP != "1.2.3.4" {
		t.Errorf("re-tested entry not updated: %+v", merged[1])
	}
}

// recheck mirrors exactly what the check subcommand does: split off the stale
// entries, test only those, then merge the survivors back into the list.
func recheck(t *testing.T, entries []ProxyEntry, ttl time.Duration, workers int,
	timeout time.Duration, testURLs []string) ([]ProxyEntry, []ProxyEntry, int) {
	t.Helper()
	stale, _ := SplitStale(entries, ttl, time.Now())
	if len(stale) == 0 {
		return entries, nil, 0
	}
	targets := make([]string, len(stale))
	for i, e := range stale {
		targets[i] = e.URL
	}
	working := TestProxies(context.Background(), targets, workers, timeout, testURLs, false)
	merged, dropped := MergeRevalidated(entries, stale, working)
	return merged, stale, dropped
}

// The whole point of the change: a list checked an hour ago must not touch the
// network when check runs.
func TestFreshListIsNotRetested(t *testing.T) {
	var hits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte("203.0.113.9"))
	}))
	defer target.Close()

	// A dead proxy would fail the test if it were ever dialled.
	entries := []ProxyEntry{
		{URL: "http://127.0.0.1:1", LatencyMS: 12, CheckedAt: age(2 * time.Hour)},
		{URL: "http://127.0.0.1:2", LatencyMS: 13, CheckedAt: age(3 * time.Hour)},
	}
	merged, stale, _ := recheck(t, entries, testTTL, 4, 50*time.Millisecond, []string{target.URL})

	if len(stale) != 0 {
		t.Errorf("stale = %d entries, want 0 for a list checked within the window", len(stale))
	}
	if len(merged) != 2 {
		t.Errorf("merged %d entries, want 2", len(merged))
	}
	if hits != 0 {
		t.Errorf("the health endpoint was hit %d times for a fresh list, want 0", hits)
	}
}

func TestOnlyExpiredEntriesAreRetested(t *testing.T) {
	var hits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte("203.0.113.9"))
	}))
	defer target.Close()

	proxyURL, _ := fakeProxy(t)
	entries := []ProxyEntry{
		{URL: "http://127.0.0.1:1", CheckedAt: age(1 * time.Hour)}, // fresh, skipped
		{URL: proxyURL, CheckedAt: age(30 * time.Hour)},            // expired, re-tested
	}
	merged, stale, dropped := recheck(t, entries, testTTL, 2, 2*time.Second, []string{target.URL})

	if len(stale) != 1 || stale[0].URL != proxyURL {
		t.Fatalf("stale = %+v, want only the expired entry", stale)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0 (the fake proxy is alive)", dropped)
	}
	if hits != 1 {
		t.Errorf("health endpoint hit %d times, want 1 (only the expired entry)", hits)
	}
	if len(merged) != 2 {
		t.Errorf("merged %d entries, want 2", len(merged))
	}
	// The re-tested entry must carry a fresh timestamp so the next run skips it.
	for _, e := range merged {
		if e.URL == proxyURL && time.Since(e.CheckedAt) > time.Minute {
			t.Errorf("re-tested entry kept its old checkedAt, so it would be re-tested again")
		}
	}
}

// An entry that was never checked has no timestamp, so it must be treated as
// expired rather than trusted forever.
func TestNeverCheckedCountsAsExpired(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("203.0.113.9"))
	}))
	defer target.Close()

	proxyURL, _ := fakeProxy(t)
	entries := []ProxyEntry{{URL: proxyURL}} // no CheckedAt at all
	_, stale, dropped := recheck(t, entries, testTTL, 1, 2*time.Second, []string{target.URL})
	if len(stale) != 1 || dropped != 0 {
		t.Errorf("stale=%d dropped=%d, want 1/0 for an entry with no timestamp", len(stale), dropped)
	}
}

func TestFailedExpiredProxyIsDropped(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("203.0.113.9"))
	}))
	defer target.Close()

	good, _ := fakeProxy(t)
	entries := []ProxyEntry{
		{URL: good, CheckedAt: age(30 * time.Hour)},                 // alive
		{URL: "http://127.0.0.1:1", CheckedAt: age(30 * time.Hour)}, // dead
	}
	merged, _, dropped := recheck(t, entries, testTTL, 2, 500*time.Millisecond, []string{target.URL})
	if dropped != 1 || len(merged) != 1 || merged[0].URL != good {
		t.Errorf("dropped=%d merged=%d, want 1 and only the working proxy", dropped, len(merged))
	}
}

func TestReValidateAfterConfig(t *testing.T) {
	cfg := &Config{}
	cfg.ApplyDefaults()
	if got := cfg.ReValidateAfter(); got != defaultReValidateAfter {
		t.Errorf("default reValidateAfter = %v, want %v", got, defaultReValidateAfter)
	}

	cfg.Proxy.Test.ReValidateAfter = "90m"
	if got := cfg.ReValidateAfter(); got != 90*time.Minute {
		t.Errorf("reValidateAfter = %v, want 90m", got)
	}

	// "0" is the escape hatch that restores re-testing everything on start.
	cfg.Proxy.Test.ReValidateAfter = "0"
	if got := cfg.ReValidateAfter(); got != 0 {
		t.Errorf("reValidateAfter = %v, want 0", got)
	}

	// Nonsense falls back to the default rather than disabling re-validation.
	cfg.Proxy.Test.ReValidateAfter = "not-a-duration"
	if got := cfg.ReValidateAfter(); got != defaultReValidateAfter {
		t.Errorf("bad duration gave %v, want the default %v", got, defaultReValidateAfter)
	}
	cfg.Proxy.Test.ReValidateAfter = "-5h"
	if got := cfg.ReValidateAfter(); got != defaultReValidateAfter {
		t.Errorf("negative duration gave %v, want the default %v", got, defaultReValidateAfter)
	}
}

// The saved list must still round-trip with the new field, and an older meta
// file that has no checkedAt at all must load cleanly.
func TestSaveLoadKeepsCheckedAt(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "ports.txt")
	entries := []ProxyEntry{
		{URL: "http://9.9.9.9:1", LatencyMS: 5, CheckedAt: age(2 * time.Hour)},
		{URL: "http://9.9.9.9:2", LatencyMS: 6}, // never checked
	}
	if err := SaveProxyList(entries, list, metaPathFor(list)); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadProxyList(list, metaPathFor(list))
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded %d entries, want 2", len(loaded))
	}
	if loaded[0].CheckedAt.IsZero() {
		t.Errorf("checkedAt lost on save/load")
	}
	if !loaded[1].CheckedAt.IsZero() {
		t.Errorf("unchecked entry gained a checkedAt: %v", loaded[1].CheckedAt)
	}
	// One fresh, one never checked: exactly one is due.
	stale, _ := SplitStale(loaded, testTTL, time.Now())
	if len(stale) != 1 || stale[0].URL != "http://9.9.9.9:2" {
		t.Errorf("stale = %+v, want only the unchecked entry", stale)
	}
}

func TestHostPortKeyIgnoresSchemeAndCredentials(t *testing.T) {
	cases := []struct {
		proxy string
		want  string
	}{
		{"http://1.2.3.4:8080", "1.2.3.4:8080"},
		{"socks5://1.2.3.4:8080", "1.2.3.4:8080"}, // same host:port, other scheme
		{"https://user:pass@1.2.3.4:8080", "1.2.3.4:8080"},
		{"socks5h://1.2.3.4:8080", "1.2.3.4:8080"},
		{"not a url", ""},
	}
	for _, c := range cases {
		if got := hostPortKey(c.proxy); got != c.want {
			t.Errorf("hostPortKey(%q) = %q, want %q", c.proxy, got, c.want)
		}
	}
}

func TestLoadProxyListDedupsSameHostPort(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "ports.txt")
	// Same host:port under two schemes — only the first should survive.
	content := "http://1.2.3.4:8080\nsocks5://1.2.3.4:8080\nhttp://5.6.7.8:1080\n"
	if err := os.WriteFile(list, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadProxyList(list, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded %d entries, want 2 (host:port dedup)", len(loaded))
	}
	if loaded[0].URL != "http://1.2.3.4:8080" {
		t.Errorf("first kept %q, want http://1.2.3.4:8080", loaded[0].URL)
	}
	if loaded[1].URL != "http://5.6.7.8:1080" {
		t.Errorf("second kept %q, want http://5.6.7.8:1080", loaded[1].URL)
	}
}
