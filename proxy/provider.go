package proxy

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Provider plumbing. Each free-proxy source lives in its own provider_*.go
// file and registers itself under the names it answers to in proxy.sources.
// Harvest resolves every configured source through this registry, so adding
// an upstream means adding one file — the fetch loop, the config and the
// error messages all pick it up on their own.

// FetchRequest carries everything a provider may need to build its request:
// the raw proxy.sources entry (so URL-bound sources can resolve it) plus the
// config tunables, which each upstream takes in a different shape.
type FetchRequest struct {
	Source    string // the raw entry from proxy.sources
	Protocol  string // proxy.protocol: http | socks5 | all | ...
	TimeoutMS int    // proxy.maxTimeoutMs, ProxyScrape's response-time filter
	Limit     int    // proxy.limit, how many proxies to fetch
	File      string // proxy.file, the local list the "file" source re-reads
	// PrivateFile is proxy.privateFile, the user's own proxy list the
	// "private" source re-reads. Kept separate from File so the private
	// entries are never overwritten by a harvest of the free sources.
	PrivateFile string
}

// FetchFunc returns the proxies one source offers. notify is the harvest
// progress logger: a provider logs its own "fetching ..." line through it, so
// the log keeps naming the source that is actually being hit.
type FetchFunc func(ctx context.Context, req FetchRequest, notify func(string, ...any)) ([]string, error)

// Provider is one free-proxy source: the spellings it is known by in
// proxy.sources and the code that fetches it.
type Provider struct {
	Name    string   // canonical spelling used in proxy.sources
	Aliases []string // the other spellings accepted for it
	Fetch   FetchFunc
	// Protected marks entries that must survive dedup and the test.keep cap:
	// the user's own proxies are always kept if they test alive. Free-provider
	// entries colliding on a host:port yield to them.
	Protected bool
}

// registry keeps providers findable by any of their spellings while holding a
// stable registration order for listings.
var registry = struct {
	mu    sync.RWMutex
	byKey map[string]*Provider
	order []*Provider
}{byKey: map[string]*Provider{}}

// canonicalSource is how source names compare: case- and space-insensitively,
// so "Proxy Scrape" and "proxyscrape" are the same source.
func canonicalSource(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// RegisterProvider adds a provider under its name and aliases, replacing any
// provider previously registered under the same canonical name. Each provider
// file calls this from init, which is what makes a source a self-contained
// module.
func RegisterProvider(p *Provider) {
	if p == nil || p.Name == "" || p.Fetch == nil {
		panic("proxy: a provider needs a name and a fetch function")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	var old *Provider
	for i, existing := range registry.order {
		if existing.Name == p.Name {
			registry.order[i], old = p, existing
			break
		}
	}
	if old == nil {
		registry.order = append(registry.order, p)
	} else {
		// Drop the previous registration's keys first, so re-registering
		// with a shorter alias list cannot leave stale spellings behind.
		for key, existing := range registry.byKey {
			if existing == old {
				delete(registry.byKey, key)
			}
		}
	}
	for _, key := range append([]string{p.Name}, p.Aliases...) {
		registry.byKey[canonicalSource(key)] = p
	}
}

// LookupProvider finds a provider by name or alias.
func LookupProvider(name string) (*Provider, bool) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	p, ok := registry.byKey[canonicalSource(name)]
	return p, ok
}

// ProviderNames lists the registered sources in registration order: what
// proxy.sources accepts and what an unknown-source error suggests.
func ProviderNames() []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	out := make([]string, 0, len(registry.order))
	for _, p := range registry.order {
		out = append(out, p.Name)
	}
	return out
}

// resolveSource maps one proxy.sources entry to the provider that fetches it:
// a registered name or alias first, then the raw-list presets (iplocate, ...)
// and any http(s) URL, which are read as plain one-proxy-per-line lists.
func resolveSource(source string) (*Provider, bool) {
	if p, ok := LookupProvider(source); ok {
		return p, true
	}
	if _, ok := SourceEndpoint(source); ok {
		return rawListProvider, true
	}
	return nil, false
}

// unknownSourceError names every spelling proxy.sources accepts: the
// registered providers, the raw-list presets, and any http(s) URL.
func unknownSourceError(source string) error {
	presets := make([]string, 0, len(SourceURLs))
	for name := range SourceURLs {
		presets = append(presets, name)
	}
	sort.Strings(presets)
	hint := append(ProviderNames(), presets...)
	return fmt.Errorf("unknown source %q (use %s, or a URL)", source, strings.Join(hint, ", "))
}

// harvestClient is the shared HTTP client every provider fetches through;
// the tests swap it to point the fetchers at a local server.
var harvestClient = &http.Client{Timeout: 20 * time.Second}

// protocolsToFetch expands a protocol filter into the concrete schemes that
// should be requested. "all" (or empty) asks for every scheme this build can
// dial; anything else is requested as-is. Providers that need one request per
// protocol fan out over this.
func protocolsToFetch(protocol string) []string {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	switch protocol {
	case "", "all", "any":
		return []string{"http", "socks5"}
	default:
		return []string{protocol}
	}
}
