package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"noderouter/config"
	"noderouter/logx"
)

// Version is the router version reported in User-Agent headers to proxy
// sources. main sets it from the build; the default matches a dev build.
var Version = "dev"

// Pull free proxies from the configured sources — each one a self-registering
// provider module (see provider.go) — test them concurrently, and write the
// survivors to a text file (one proxy per line, fastest first) that the
// router re-reads on every refresh.

var supportedProtocols = map[string]bool{"http": true, "https": true, "socks4": true, "socks5": true}

// dialableProtocols are the schemes this build can actually connect through.
// socks4 is parsed but skipped, as this build cannot dial it.
var dialableProtocols = map[string]bool{"http": true, "https": true, "socks5": true, "socks5h": true}

// ProxyEntry is one line of the ports file, plus what we learned when testing.
type ProxyEntry struct {
	URL       string    `json:"url"`
	LatencyMS int64     `json:"latencyMs,omitempty"`
	ExitIP    string    `json:"exitIp,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitempty"`
}

// NormalizeProxy turns one line of a proxy list into a usable proxy URL.
//
// Handles the shapes these lists show up in:
//
//	ip:port
//	scheme://ip:port
//	ip:port:user:pass
//	scheme://user:pass@ip:port
//
// Blank lines, comments and anything unparseable return "", false.
func NormalizeProxy(line, defaultScheme string) (string, bool) {
	if defaultScheme == "" {
		defaultScheme = "http"
	}
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
		return "", false
	}
	if i := strings.Index(line, " #"); i >= 0 { // strip inline comment
		line = strings.TrimSpace(line[:i])
	}
	if strings.Contains(line, "://") {
		scheme, rest, _ := strings.Cut(line, "://")
		scheme = strings.ToLower(strings.TrimSpace(scheme))
		if !supportedProtocols[scheme] {
			return "", false
		}
		if strings.Contains(rest, "@") { // credentials already in URL form
			if err := validateHostPort(rest[strings.LastIndex(rest, "@")+1:]); err != nil {
				return "", false
			}
			return scheme + "://" + rest, true
		}
		if err := validateHostPort(rest); err != nil {
			return "", false
		}
		return scheme + "://" + rest, true
	}

	parts := strings.Split(line, ":")
	switch len(parts) {
	case 2: // ip:port
		if err := validateHostPort(line); err != nil {
			return "", false
		}
		return defaultScheme + "://" + line, true
	case 4: // ip:port:user:pass
		host, port, user, password := parts[0], parts[1], parts[2], parts[3]
		if user == "" || password == "" || validateHostPort(host+":"+port) != nil {
			return "", false
		}
		return defaultScheme + "://" + user + ":" + password + "@" + host + ":" + port, true
	}
	return "", false
}

func validateHostPort(hostport string) error {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return err
	}
	if host == "" {
		return fmt.Errorf("no host in %q", hostport)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 || p > 65535 {
		return fmt.Errorf("bad port in %q", hostport)
	}
	return nil
}

// HostPortKey returns a stable "host:port" key for a proxy URL, ignoring the
// scheme and any embedded credentials. Two proxies that differ only in scheme
// but share the same host and port (e.g. http://1.2.3.4:8080 and
// socks5://1.2.3.4:8080) therefore deduplicate to the same key.
func HostPortKey(proxy string) string {
	u, err := url.Parse(proxy)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if host == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = u.Scheme
	}
	return net.JoinHostPort(host, port)
}

// BareScheme is the default scheme for list lines that carry none. A protocol
// filter of "all" is not itself a scheme, so it must not end up in a URL.
func BareScheme(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "", "all", "any":
		return "http"
	default:
		return strings.ToLower(strings.TrimSpace(protocol))
	}
}

// SchemeAllowed reports whether a proxy scheme is in scope for a protocol
// filter. "all" (or an empty filter) means every scheme this build can dial.
func SchemeAllowed(scheme, protocol string) bool {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	switch protocol {
	case "", "all", "any":
		return dialableProtocols[scheme]
	case "http":
		return scheme == "http" || scheme == "https"
	default:
		return scheme == protocol
	}
}

// CheckProxy tries one proxy against the test endpoints. The first one that
// answers wins; the exit IP is recorded so duplicate exits are visible.
func CheckProxy(ctx context.Context, proxy string, testURLs []string, timeout time.Duration, skipVerify bool) (ProxyEntry, bool) {
	u, err := url.Parse(proxy)
	if err != nil {
		return ProxyEntry{}, false
	}
	client := newProxyClientTLS(u, timeout, 1, skipVerify)
	defer client.CloseIdleConnections()

	for _, target := range testURLs {
		start := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			continue
		}
		exitIP := strings.Join(strings.Fields(string(body)), " ")
		if len(exitIP) > 60 {
			exitIP = exitIP[:60]
		}
		return ProxyEntry{
			URL:       proxy,
			LatencyMS: latencyMS(time.Since(start)),
			ExitIP:    exitIP,
			CheckedAt: time.Now().UTC(),
		}, true
	}
	return ProxyEntry{}, false
}

// latencyMS is a duration as whole milliseconds, but never zero: a proxy on
// the same machine as the router answers well inside a millisecond, and
// truncating that to 0 makes a measured proxy indistinguishable from an
// unmeasured one - it prints as "0ms", ties with every other zero at the top of
// a fastest-first sort, and is dropped from the JSON entirely by omitempty.
// One millisecond is still the honest order of magnitude for a sub-millisecond
// hop, and it is the smallest number that says "measured".
func latencyMS(d time.Duration) int64 {
	ms := d.Milliseconds()
	if ms < 1 {
		return 1
	}
	return ms
}

// TestProxies checks a list of proxies concurrently, reporting progress every
// 25 finishes. Results come back fastest first.
func TestProxies(ctx context.Context, proxies []string, workers int, timeout time.Duration, testURLs []string, skipVerify bool) []ProxyEntry {
	if len(proxies) == 0 {
		return nil
	}
	workers = clamp(workers, 1, 200)
	logx.Info("testing proxies", "count", len(proxies), "workers", workers, "timeout", timeout)

	var (
		mu      sync.Mutex
		working []ProxyEntry
		done    int
	)
	jobs := make(chan string)
	var wg sync.WaitGroup
	worker := func() {
		defer wg.Done()
		for p := range jobs {
			entry, ok := CheckProxy(ctx, p, testURLs, timeout, skipVerify)
			mu.Lock()
			done++
			if ok {
				working = append(working, entry)
			}
			n := done
			alive := len(working)
			mu.Unlock()
			if n%25 == 0 || n == len(proxies) {
				logx.Info("proxy check", "done", fmt.Sprintf("%d/%d", n, len(proxies)), "alive", alive)
			}
		}
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go worker()
	}
	for _, p := range proxies {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return sortEntries(working)
		case jobs <- p:
		}
	}
	close(jobs)
	wg.Wait()
	return sortEntries(working)
}

// mergeByLatency interleaves two latency-sorted lists, preserving order.
func mergeByLatency(a, b []ProxyEntry) []ProxyEntry {
	out := make([]ProxyEntry, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i].LatencyMS <= b[j].LatencyMS {
			out = append(out, a[i])
			i++
		} else {
			out = append(out, b[j])
			j++
		}
	}
	out = append(out, a[i:]...)
	out = append(out, b[j:]...)
	return out
}

func sortEntries(entries []ProxyEntry) []ProxyEntry {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].LatencyMS < entries[j].LatencyMS })
	return entries
}

// SplitStale separates the entries whose last successful check is missing or
// older than ttl from the ones that are still considered fresh. A ttl of 0
// makes everything stale, which is how a forced re-check is asked for.
//
// This is what keeps a restart cheap: an entry checked two hours ago against a
// six hour window is trusted and left alone.
func SplitStale(entries []ProxyEntry, ttl time.Duration, now time.Time) (stale, fresh []ProxyEntry) {
	for _, e := range entries {
		if e.CheckedAt.IsZero() || now.Sub(e.CheckedAt) >= ttl {
			stale = append(stale, e)
			continue
		}
		fresh = append(fresh, e)
	}
	return stale, fresh
}

// MergeRevalidated folds the results of a re-check back into the previous list.
// Entries that were re-tested are replaced by their new measurement or dropped
// when they failed; entries that were still fresh keep their place and their
// numbers untouched. The output preserves the input order, so the fastest-first
// ranking written by the last full harvest survives a partial re-check.
func MergeRevalidated(entries, stale, working []ProxyEntry) (merged []ProxyEntry, dropped int) {
	survivors := make(map[string]ProxyEntry, len(working))
	for _, e := range working {
		survivors[e.URL] = e
	}
	retested := make(map[string]bool, len(stale))
	for _, e := range stale {
		retested[e.URL] = true
	}
	merged = make([]ProxyEntry, 0, len(entries))
	for _, e := range entries {
		if survivor, passed := survivors[e.URL]; passed {
			merged = append(merged, survivor)
			continue
		}
		if retested[e.URL] {
			dropped++
			continue
		}
		merged = append(merged, e)
	}
	return merged, dropped
}

// SaveProxyList writes the plain text proxy list (port list) and a sidecar
// meta file with latency/exit IP, so the router can rank proxies.
func SaveProxyList(entries []ProxyEntry, listPath, metaPath string) error {
	listPath = config.ExpandPath(listPath)
	if listPath == "" {
		return fmt.Errorf("empty proxy list path")
	}
	if err := os.MkdirAll(filepath.Dir(listPath), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.URL)
		b.WriteByte('\n')
	}
	mode := os.FileMode(0o644)
	for _, e := range entries {
		if strings.Contains(e.URL, "@") {
			// The private proxies embed user:pass — drop group/other bits.
			mode = 0o600
			break
		}
	}
	if err := writeFileAtomic(listPath, []byte(b.String()), mode); err != nil {
		return err
	}
	if metaPath == "" {
		return nil
	}
	meta, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	// The sidecar marshals the same ProxyEntry.URL values as the list, so it
	// carries the same credentials: a world-readable meta would hand every
	// private proxy's user:pass to any local user on a shared host. Use the
	// list's mode — 0600 when any entry embeds credentials, 0644 otherwise.
	return writeFileAtomic(config.ExpandPath(metaPath), append(meta, '\n'), mode)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".noderouter-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// LoadProxyList reads the ports file plus the meta sidecar, if present.
func LoadProxyList(listPath, metaPath string) ([]ProxyEntry, error) {
	listPath = config.ExpandPath(listPath)
	data, err := os.ReadFile(listPath)
	if err != nil {
		return nil, err
	}
	meta := map[string]ProxyEntry{}
	if metaPath != "" {
		if raw, err := os.ReadFile(config.ExpandPath(metaPath)); err == nil {
			var entries []ProxyEntry
			if json.Unmarshal(raw, &entries) == nil {
				for _, e := range entries {
					meta[e.URL] = e
				}
			}
		}
	}

	seen := map[string]bool{}
	seenHostPort := map[string]bool{}
	var out []ProxyEntry
	bad := 0
	for _, line := range strings.Split(string(data), "\n") {
		p, ok := NormalizeProxy(line, "http")
		if !ok {
			if strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "#") {
				bad++
			}
			continue
		}
		if seen[p] {
			continue
		}
		hkey := HostPortKey(p)
		if hkey != "" && seenHostPort[hkey] {
			bad++
			continue
		}
		seen[p] = true
		if hkey != "" {
			seenHostPort[hkey] = true
		}
		entry := meta[p]
		entry.URL = p
		out = append(out, entry)
	}
	if bad > 0 {
		logx.Info("proxy list", "file", listPath, "usable", len(out), "unparseable", bad)
	}
	return out, nil
}

// Harvest fetches fresh proxies from the configured sources, tests them and
// writes the result to the ports file.
type HarvestResult struct {
	Fetched  int
	Alive    int
	Skipped  int
	Duration time.Duration
	Sources  []string
}

func Harvest(ctx context.Context, cfg *config.Config, progress func(string, ...any)) ([]ProxyEntry, HarvestResult, error) {
	var res HarvestResult
	started := time.Now()
	notify := func(msg string, args ...any) {
		if progress != nil {
			progress(msg, args...)
		} else {
			logx.Info(msg, args...)
		}
	}

	var candidates []string
	seen := map[string]bool{}
	seenHostPort := map[string]bool{}
	protectedSet := map[string]bool{} // URLs the private-file source returned
	// Protected entries win host:port collisions and survive the keep cap, so
	// the user's own working proxy is never silently dropped. Collect them
	// separately and let them displace free entries on the same host:port.
	var protectedCandidates []string
	req := FetchRequest{
		Protocol:    cfg.Proxy.Protocol,
		TimeoutMS:   cfg.Proxy.MaxTimeoutMS,
		Limit:       cfg.Proxy.Limit,
		File:        cfg.Proxy.File,
		PrivateFile: cfg.Proxy.PrivateFile,
	}
	for _, source := range cfg.Proxy.Sources {
		source = strings.TrimSpace(source)
		if lower := strings.ToLower(source); lower == "" || lower == "none" {
			continue
		}
		prov, ok := resolveSource(source)
		if !ok {
			notify("source failed", "source", source, "error", unknownSourceError(source))
			continue
		}
		req.Source = source
		list, err := prov.Fetch(ctx, req, notify)
		if err != nil {
			notify("source failed", "source", source, "error", err)
			continue
		}
		// Registered providers are summarised by their canonical name; a raw
		// list keeps the URL or preset the user actually wrote.
		label := source
		if prov != rawListProvider {
			label = prov.Name
		}
		res.Sources = append(res.Sources, fmt.Sprintf("%s: %d", label, len(list)))
		for _, p := range list {
			if seen[p] {
				continue
			}
			seen[p] = true
			if prov.Protected {
				protectedSet[p] = true
				protectedCandidates = append(protectedCandidates, p)
				hkey := HostPortKey(p)
				if hkey != "" {
					seenHostPort[hkey] = true
				}
				continue
			}
			hkey := HostPortKey(p)
			if hkey != "" && seenHostPort[hkey] {
				res.Skipped++
				continue
			}
			if hkey != "" {
				seenHostPort[hkey] = true
			}
			candidates = append(candidates, p)
		}
	}
	// Protected entries are always present; drop free entries that sit on the
	// same host:port as one of them, since the private one is authoritative.
	protectedHP := map[string]bool{}
	for _, p := range protectedCandidates {
		if hk := HostPortKey(p); hk != "" {
			protectedHP[hk] = true
		}
	}
	out := make([]string, 0, len(candidates)+len(protectedCandidates))
	for _, p := range candidates {
		if hk := HostPortKey(p); hk != "" && protectedHP[hk] {
			continue
		}
		out = append(out, p)
	}
	candidates = append(protectedCandidates, out...)
	res.Fetched = len(candidates)

	// Only keep proxies this build can actually dial.
	var dialable []string
	for _, p := range candidates {
		u, err := url.Parse(p)
		if err != nil || !dialableProtocols[u.Scheme] {
			res.Skipped++
			continue
		}
		dialable = append(dialable, p)
	}
	if res.Skipped > 0 {
		notify("skipping unsupported proxies", "count", res.Skipped, "supported", "http, https, socks5")
	}
	if len(dialable) == 0 {
		res.Duration = time.Since(started)
		return nil, res, fmt.Errorf("no usable proxies from %v", cfg.Proxy.Sources)
	}

	if !cfg.TestEnabled() {
		entries := make([]ProxyEntry, 0, len(dialable))
		for _, p := range dialable {
			entries = append(entries, ProxyEntry{URL: p})
		}
		res.Alive, res.Duration = len(entries), time.Since(started)
		return entries, res, nil
	}

	entries := TestProxies(ctx, dialable, cfg.Proxy.Test.Workers,
		time.Duration(cfg.Proxy.Test.TimeoutMS)*time.Millisecond, cfg.Proxy.Test.URLs,
		cfg.Proxy.Test.SkipTLSVerify)
	if cfg.Proxy.Test.Keep > 0 && len(entries) > cfg.Proxy.Test.Keep {
		// test.keep caps the working pool, but the private proxies must never be
		// collateral damage: keep them all and cap only the free ones.
		var regularEntries, protectedEntries []ProxyEntry
		for _, e := range entries {
			if protectedSet[e.URL] {
				protectedEntries = append(protectedEntries, e)
			} else {
				regularEntries = append(regularEntries, e)
			}
		}
		if len(regularEntries) > cfg.Proxy.Test.Keep {
			regularEntries = regularEntries[:cfg.Proxy.Test.Keep]
		}
		// Re-merge fastest-first: both slices are already latency-sorted.
		entries = mergeByLatency(regularEntries, protectedEntries)
	}
	res.Alive, res.Duration = len(entries), time.Since(started)
	return entries, res, nil
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
