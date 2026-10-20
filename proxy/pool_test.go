package proxy

// Pool's job is to hand out the next proxy to try and to keep the bad ones out
// of the way for a while. The cooldown bookkeeping is the part worth pinning:
// it is time-dependent, it is shared across every request, and a mistake there
// either benches a good proxy forever or never retires a dead one.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeList writes a ports file and returns its path.
func writeList(t *testing.T, proxies ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ports.txt")
	if err := os.WriteFile(path, []byte(strings.Join(proxies, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write list: %v", err)
	}
	return path
}

// newLoadedPool builds a pool over a temporary list of proxies.
func newLoadedPool(t *testing.T, mode string, proxies ...string) *Pool {
	t.Helper()
	path := writeList(t, proxies...)
	p := NewPool(path, "", mode, true)
	if _, err := p.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return p
}

func entry(url string) ProxyEntry { return ProxyEntry{URL: url} }

const (
	p1 = "http://1.1.1.1:8080"
	p2 = "http://2.2.2.2:8080"
	p3 = "http://3.3.3.3:8080"
)

// Stats is what /noderouter/status reports, so its counts have to describe the
// pool that is actually loaded.
func TestStatsDescribesTheLoadedPool(t *testing.T) {
	path := writeList(t, p1, p2, p3)
	p := NewPool(path, "", "rotate", true)

	// Before a load there is nothing to report and no file metadata.
	stats := p.Stats()
	if stats.Count != 0 || stats.Healthy != 0 || stats.Benched != 0 {
		t.Errorf("an unloaded pool reported count=%d healthy=%d benched=%d, want all zero",
			stats.Count, stats.Healthy, stats.Benched)
	}
	if stats.Mode != "rotate" || !stats.Enabled {
		t.Errorf("stats = %+v, want mode rotate and enabled", stats)
	}
	if !stats.UpdatedAt.IsZero() {
		t.Errorf("UpdatedAt = %s, want the zero time before a load", stats.UpdatedAt)
	}
	if stats.NextProxy != "" {
		t.Errorf("NextProxy = %q, want empty for an empty pool", stats.NextProxy)
	}

	if _, err := p.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	stats = p.Stats()
	if stats.Count != 3 || stats.Healthy != 3 {
		t.Errorf("stats = %+v, want 3 proxies all healthy", stats)
	}
	if stats.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should carry the list file's mtime after a load")
	}
	if stats.File != path {
		t.Errorf("File = %q, want %q", stats.File, path)
	}

	// NextProxy is a preview of the next rotation step, so it has to name one
	// of the proxies that are actually in the list.
	found := false
	for _, u := range []string{p1, p2, p3} {
		if stats.NextProxy == u {
			found = true
		}
	}
	if !found {
		t.Errorf("NextProxy = %q, want one of the loaded proxies", stats.NextProxy)
	}
}

// Benched proxies have to be reported separately from healthy ones, since that
// count is what tells an operator the list has gone bad.
func TestStatsCountsBenchedProxies(t *testing.T) {
	p := newLoadedPool(t, "rotate", p1, p2, p3)

	for range MaxProxyFails {
		p.Failed(entry(p2), os.ErrDeadlineExceeded)
	}

	stats := p.Stats()
	if stats.Benched != 1 {
		t.Errorf("Benched = %d, want 1", stats.Benched)
	}
	if stats.Healthy != 2 {
		t.Errorf("Healthy = %d, want the other 2 still healthy", stats.Healthy)
	}
	if stats.Count != 3 {
		t.Errorf("Count = %d, want 3", stats.Count)
	}
}

// The bench threshold is the whole cooldown mechanism: below it a flaky proxy
// keeps being used, at it the proxy is put aside.
func TestFailedBenchesOnlyAtTheThreshold(t *testing.T) {
	p := newLoadedPool(t, "rotate", p1, p2, p3)

	// One short of the threshold: still in rotation.
	for i := 1; i < MaxProxyFails; i++ {
		p.Failed(entry(p1), os.ErrDeadlineExceeded)
		if got := p.Stats().Benched; got != 0 {
			t.Fatalf("after %d/%d failures Benched = %d, want 0", i, MaxProxyFails, got)
		}
	}

	// The failure that reaches it benches the proxy.
	p.Failed(entry(p1), os.ErrDeadlineExceeded)
	if got := p.Stats().Benched; got != 1 {
		t.Errorf("after %d failures Benched = %d, want 1", MaxProxyFails, got)
	}
}

// A benched proxy is skipped and the next one is used instead, which is what
// stops one dead proxy from failing every request.
func TestTakeSkipsBenchedProxies(t *testing.T) {
	p := newLoadedPool(t, "rotate", p1, p2, p3)

	for range MaxProxyFails {
		p.Failed(entry(p1), os.ErrDeadlineExceeded)
	}

	// With p1 benched, every pick must be p2 or p3, never p1.
	for seq := range 10 {
		got, ok := p.Take(uint64(seq), 0)
		if !ok {
			t.Fatal("Take reported no proxy available")
		}
		if got.URL == p1 {
			t.Fatalf("Take returned benched proxy %s", got.URL)
		}
	}
}

// A success clears the failure history, so a proxy that recovers is not kept
// out on stale evidence.
func TestSucceededClearsTheFailureHistory(t *testing.T) {
	p := newLoadedPool(t, "rotate", p1, p2)

	// Build up a failure count that has not yet benched it.
	for range MaxProxyFails - 1 {
		p.Failed(entry(p1), os.ErrDeadlineExceeded)
	}

	p.Succeeded(entry(p1))

	p.mu.RLock()
	h := p.health[p1]
	p.mu.RUnlock()
	if h != nil {
		t.Errorf("Succeeded left %+v behind, want the health record removed", h)
	}
	if got := p.Stats().Benched; got != 0 {
		t.Errorf("Benched = %d after a success, want 0", got)
	}
}

// Succeeded on a proxy with no record must not panic: it is called on every
// successful request, including for proxies that never failed.
func TestSucceededOnUnknownProxyIsHarmless(t *testing.T) {
	p := newLoadedPool(t, "rotate", p1)
	p.Succeeded(entry(p1))
	p.Succeeded(entry("http://9.9.9.9:1"))
}

// The cooldown has to expire: a benched proxy is retried once the window has
// passed, otherwise a one-off network blip costs a proxy for five minutes.
func TestBenchedProxyReturnsAfterTheCooldown(t *testing.T) {
	p := newLoadedPool(t, "rotate", p1, p2, p3)

	for range MaxProxyFails {
		p.Failed(entry(p1), os.ErrDeadlineExceeded)
	}
	if p.Stats().Benched != 1 {
		t.Fatal("setup: p1 should be benched")
	}

	// Expire the cooldown by hand; waiting out proxyCooldown would make this
	// test take five minutes.
	p.mu.Lock()
	p.health[p1].until = time.Now().Add(-time.Second)
	p.mu.Unlock()

	if got := p.Stats().Benched; got != 0 {
		t.Errorf("Benched = %d after the cooldown expired, want 0", got)
	}

	// And it is handed out again. Take is only asked for p1's own rotation
	// step (seq 0), since rotate mode starts elsewhere for every other seq.
	if got, ok := p.Take(0, 0); !ok || got.URL != p1 {
		t.Errorf("Take = %+v (ok=%v), want the recovered proxy %s", got, ok, p1)
	}
}

// The failure counter resets when a proxy is benched, so a second round of
// failures starts from zero rather than benching it again immediately.
func TestBenchingResetsTheFailureCount(t *testing.T) {
	p := newLoadedPool(t, "rotate", p1, p2)

	for range MaxProxyFails {
		p.Failed(entry(p1), os.ErrDeadlineExceeded)
	}
	p.mu.RLock()
	h := p.health[p1]
	fails := 0
	if h != nil {
		fails = h.fails
	}
	p.mu.RUnlock()
	if fails != 0 {
		t.Errorf("fails = %d after benching, want it reset to 0", fails)
	}
}

// The health map must not grow without bound: a URL that fell out of the list
// carries no information worth keeping.
func TestReloadDropsHealthForProxiesNoLongerInTheList(t *testing.T) {
	path := writeList(t, p1, p2)
	p := NewPool(path, "", "rotate", true)
	if _, err := p.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// One proxy in the list fails, one was tried and never made the list.
	p.Failed(entry(p1), os.ErrDeadlineExceeded)
	p.Failed(entry("http://gone.example:1"), os.ErrDeadlineExceeded)

	// Rewrite the list without p1.
	if err := os.WriteFile(path, []byte(p2+"\n"), 0o644); err != nil {
		t.Fatalf("rewrite list: %v", err)
	}
	if _, err := p.Reload(); err != nil {
		t.Fatalf("second reload: %v", err)
	}

	p.mu.RLock()
	defer p.mu.RUnlock()
	for url := range p.health {
		if url == p2 {
			continue
		}
		t.Errorf("health still holds %s, want only proxies in the current list", url)
	}
}

// A proxy that fails once in a long while should not be remembered forever
// either, so the sweep drops trailing failures that went stale.
func TestHealthSweepDropsStaleFailures(t *testing.T) {
	p := newLoadedPool(t, "rotate", p1, p2)

	p.Failed(entry(p1), os.ErrDeadlineExceeded)

	// Backdate the record past the TTL and let the next failure sweep it.
	p.mu.Lock()
	p.health[p1].last = time.Now().Add(-2 * proxyHealthTTL)
	p.mu.Unlock()

	p.Failed(entry(p2), os.ErrDeadlineExceeded)

	p.mu.RLock()
	_, stillThere := p.health[p1]
	p.mu.RUnlock()
	if stillThere {
		t.Error("a failure record past the TTL should have been swept")
	}
}

// An entirely benched pool must still hand out a proxy: degrading to a proxy we
// know is bad beats failing every request outright.
func TestTakeOnFullyBenchedPoolStillReturns(t *testing.T) {
	p := newLoadedPool(t, "rotate", p1, p2)

	for _, u := range []string{p1, p2} {
		for range MaxProxyFails {
			p.Failed(entry(u), os.ErrDeadlineExceeded)
		}
	}
	if got := p.Stats().Benched; got != 2 {
		t.Fatalf("setup: Benched = %d, want 2", got)
	}

	got, ok := p.Take(0, 0)
	if !ok {
		t.Fatal("Take should still return a proxy when everything is benched")
	}
	if got.URL != p1 && got.URL != p2 {
		t.Errorf("Take = %s, want one of the benched proxies rather than a new one", got.URL)
	}
}

// An empty pool reports unavailability rather than an empty entry.
func TestTakeOnEmptyPool(t *testing.T) {
	p := NewPool(filepath.Join(t.TempDir(), "none.txt"), "", "rotate", true)
	got, ok := p.Take(0, 0)
	if ok {
		t.Errorf("Take on an empty pool returned %+v, ok=true; want ok=false", got)
	}
	if got.URL != "" {
		t.Errorf("Take on an empty pool returned URL %q, want empty", got.URL)
	}
	if p.Len() != 0 {
		t.Errorf("Len = %d on an empty pool, want 0", p.Len())
	}
}

// "first" mode always starts at the head, which is the tested-fastest proxy.
func TestTakeFirstModeUsesTheHead(t *testing.T) {
	p := newLoadedPool(t, "first", p1, p2, p3)
	for seq := range 5 {
		got, ok := p.Take(uint64(seq), 0)
		if !ok || got.URL != p1 {
			t.Errorf("first mode: Take = %+v (ok=%v), want %s", got, ok, p1)
		}
	}
}

// "rotate" mode advances with the request token and the attempt number, so
// retries of one request walk forward instead of retrying the same proxy.
func TestTakeRotateModeWalksForward(t *testing.T) {
	p := newLoadedPool(t, "rotate", p1, p2, p3)

	// Consecutive requests land on consecutive entries.
	for seq := range 6 {
		got, _ := p.Take(uint64(seq), 0)
		want := []string{p1, p2, p3}[seq%3]
		if got.URL != want {
			t.Errorf("seq %d: Take = %s, want %s", seq, got.URL, want)
		}
	}

	// A retry of the first request moves on rather than repeating itself.
	first, _ := p.Take(0, 0)
	retry, _ := p.Take(0, 1)
	if first.URL == retry.URL {
		t.Errorf("attempt 1 of request 0 returned %s again, want the next proxy", retry.URL)
	}
}

// "random" mode has to actually spread out; over enough draws every proxy
// should show up.
func TestTakeRandomModeCoversTheList(t *testing.T) {
	p := newLoadedPool(t, "random", p1, p2, p3)

	seen := map[string]bool{}
	for seq := range 200 {
		got, ok := p.Take(uint64(seq), 0)
		if !ok {
			t.Fatal("Take reported no proxy")
		}
		seen[got.URL] = true
	}
	for _, u := range []string{p1, p2, p3} {
		if !seen[u] {
			t.Errorf("random mode never returned %s over 200 draws", u)
		}
	}
}

// Sequence hands out a distinct token per request, which is what makes rotate
// mode advance.
func TestSequenceAdvances(t *testing.T) {
	p := newLoadedPool(t, "rotate", p1, p2)
	first := p.Sequence()
	second := p.Sequence()
	if second != first+1 {
		t.Errorf("Sequence returned %d then %d, want consecutive tokens", first, second)
	}
}

// ReloadIfChanged is what lets an outside `noderouter convert` take effect
// without a restart, and it must not reload when nothing moved.
func TestReloadIfChangedOnlyReloadsOnAChange(t *testing.T) {
	path := writeList(t, p1, p2)
	p := NewPool(path, "", "rotate", true)
	if _, err := p.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	changed, err := p.ReloadIfChanged()
	if err != nil {
		t.Fatalf("ReloadIfChanged: %v", err)
	}
	if changed {
		t.Error("ReloadIfChanged reported a change when the file had not moved")
	}

	// A different size is a change even at the same mtime granularity.
	if err := os.WriteFile(path, []byte(p1+"\n"+p2+"\n"+p3+"\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	changed, err = p.ReloadIfChanged()
	if err != nil {
		t.Fatalf("ReloadIfChanged after a rewrite: %v", err)
	}
	if !changed {
		t.Error("ReloadIfChanged did not notice the rewritten list")
	}
	if p.Len() != 3 {
		t.Errorf("Len = %d after the reload, want 3", p.Len())
	}
}

// A missing list file is reported rather than treated as an empty pool, so
// callers can say "run convert" rather than "no proxies available".
func TestReloadOnMissingFileFails(t *testing.T) {
	p := NewPool(filepath.Join(t.TempDir(), "absent.txt"), "", "rotate", true)
	n, err := p.Reload()
	if err == nil {
		t.Fatalf("Reload on a missing file returned %d, nil; want an error", n)
	}
	if !os.IsNotExist(err) {
		t.Errorf("error = %v, want a not-exist error", err)
	}

	// ReloadIfChanged surfaces the same failure.
	if _, err := p.ReloadIfChanged(); err == nil {
		t.Error("ReloadIfChanged on a missing file returned nil, want an error")
	}
}

// Describe is printed in the startup banner, so it has to read sensibly for
// both an empty and a populated pool.
func TestDescribe(t *testing.T) {
	p := NewPool(filepath.Join(t.TempDir(), "none.txt"), "", "rotate", true)
	got := p.Describe()
	if !strings.Contains(got, "0 proxies") {
		t.Errorf("Describe on an empty pool = %q, want it to say 0 proxies", got)
	}

	p2 := newLoadedPool(t, "rotate", p1, p2)
	got = p2.Describe()
	if !strings.Contains(got, "2 proxies") {
		t.Errorf("Describe = %q, want it to report 2 proxies", got)
	}
	if !strings.Contains(got, "rotate") {
		t.Errorf("Describe = %q, want it to name the mode", got)
	}

	// With a benched proxy the count has to show up, because that is the one
	// number that explains a degraded list.
	for range MaxProxyFails {
		p2.Failed(entry(p1), os.ErrDeadlineExceeded)
	}
	if got := p2.Describe(); !strings.Contains(got, "1 benched") {
		t.Errorf("Describe = %q, want it to report 1 benched", got)
	}
}
