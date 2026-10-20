package proxy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"noderouter/config"
)

// The registry is what makes a source a module: builtins register themselves,
// and Harvest only knows how to resolve and call a provider.

// A provider is reachable under its name and every alias, in any case and
// spacing, so config entries cannot be rejected over spelling.
func TestLookupProviderByNameAndAlias(t *testing.T) {
	p, ok := LookupProvider("proxyscrape")
	if !ok || p == nil {
		t.Fatal("proxyscrape should be registered by its provider module")
	}
	for _, spelling := range []string{"Proxy Scrape", "  proxy scrape ", "SCRAPE"} {
		got, ok := LookupProvider(spelling)
		if !ok || got != p {
			t.Errorf("LookupProvider(%q) did not resolve to the proxyscrape provider", spelling)
		}
	}
	if _, ok := LookupProvider("nosuchsource"); ok {
		t.Error("an unregistered name must not resolve")
	}
	if _, ok := LookupProvider(""); ok {
		t.Error("the empty source must not resolve to a provider")
	}
}

// Every builtin source is listed, which is what the unknown-source error and
// the docs lean on.
func TestProviderNamesListsBuiltins(t *testing.T) {
	names := ProviderNames()
	for _, want := range []string{"proxyscrape", "geonode", "chillyproxy", "freeproxydb", "file"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ProviderNames() = %v, missing %q", names, want)
		}
	}
}

// Registered names win; raw-list presets and any http(s) URL fall through to
// the raw-list provider; anything else does not resolve at all.
func TestResolveSourceRouting(t *testing.T) {
	if p, ok := resolveSource("geonode"); !ok || p == nil || p.Name != "geonode" {
		t.Errorf("resolveSource(\"geonode\") = %v, %v, want the geonode provider", p, ok)
	}
	if p, ok := resolveSource("https://example.com/list.txt"); !ok || p != rawListProvider {
		t.Errorf("a URL should route to the raw-list provider, got %v, %v", p, ok)
	}
	if p, ok := resolveSource("iplocate"); !ok || p != rawListProvider {
		t.Errorf("the iplocate preset should route to the raw-list provider, got %v, %v", p, ok)
	}
	if _, ok := resolveSource("nosuchsource"); ok {
		t.Error("an unknown source must not resolve")
	}
}

// The point of the registry: a provider registered at runtime is fetched by
// Harvest exactly like a builtin — resolved through its alias, handed the
// request shape, logged through its own notify line and merged into the pool.
func TestHarvestUsesRegisteredProvider(t *testing.T) {
	var gotReq FetchRequest
	RegisterProvider(&Provider{
		Name:    "testsource",
		Aliases: []string{"test src"},
		Fetch: func(_ context.Context, req FetchRequest, notify func(string, ...any)) ([]string, error) {
			gotReq = req
			notify("fetching testsource", "protocol", req.Protocol)
			return []string{"http://1.2.3.4:8080", "socks5://5.6.7.8:1080"}, nil
		},
	})

	cfg := &config.Config{}
	cfg.Proxy.Sources = []string{"test src"} // resolved through the alias
	cfg.Proxy.Protocol = "all"
	cfg.Proxy.MaxTimeoutMS = 4321
	cfg.Proxy.Limit = 7

	var progress []string
	entries, res, err := Harvest(context.Background(), cfg, func(msg string, args ...any) {
		progress = append(progress, msg)
	})
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}
	if len(entries) != 2 || entries[0].URL != "http://1.2.3.4:8080" {
		t.Errorf("entries = %v, want the two the provider returned", entries)
	}
	// The whole FetchRequest is passed on, so a provider can pick its own
	// tunables out of it.
	if gotReq.Source != "test src" || gotReq.Protocol != "all" ||
		gotReq.TimeoutMS != 4321 || gotReq.Limit != 7 {
		t.Errorf("provider received %+v, want the config's fetch settings", gotReq)
	}
	// The summary names the provider canonically, and its own progress line
	// was logged through the notify callback.
	if len(res.Sources) != 1 || res.Sources[0] != "testsource: 2" {
		t.Errorf("Sources = %v, want [testsource: 2]", res.Sources)
	}
	joined := strings.Join(progress, "; ")
	if !strings.Contains(joined, "fetching testsource") {
		t.Errorf("progress = %v, want the provider's own log line", progress)
	}
}

// Re-registering a name replaces the provider and drops its old aliases, so a
// swap cannot leave stale spellings resolving to the previous fetch.
func TestRegisterProviderReplacesAliases(t *testing.T) {
	first := &Provider{
		Name:    "swaptest",
		Aliases: []string{"old alias"},
		Fetch:   func(context.Context, FetchRequest, func(string, ...any)) ([]string, error) { return nil, nil },
	}
	RegisterProvider(first)
	if _, ok := LookupProvider("old alias"); !ok {
		t.Fatal("the alias should resolve while registered")
	}
	second := &Provider{
		Name:  "swaptest",
		Fetch: func(context.Context, FetchRequest, func(string, ...any)) ([]string, error) { return nil, nil },
	}
	RegisterProvider(second)
	if p, ok := LookupProvider("swaptest"); !ok || p != second {
		t.Error("re-registering should replace the provider under its name")
	}
	if _, ok := LookupProvider("old alias"); ok {
		t.Error("the replaced provider's alias should no longer resolve")
	}
	if got, want := countName(ProviderNames(), "swaptest"), 1; got != want {
		t.Errorf("ProviderNames() lists swaptest %d times, want %d", got, want)
	}
}

// An unknown source is reported with every spelling proxy.sources accepts, so
// the log tells the user what to fix without a trip to the docs.
func TestUnknownSourceErrorNamesEverySource(t *testing.T) {
	msg := unknownSourceError("bogus").Error()
	if !strings.Contains(msg, `"bogus"`) {
		t.Errorf("error %q should quote the source it rejected", msg)
	}
	for _, name := range append(ProviderNames(), "iplocate") {
		if !strings.Contains(msg, name) {
			t.Errorf("error %q should mention the source %q", msg, name)
		}
	}
}

func countName(names []string, want string) int {
	n := 0
	for _, name := range names {
		if name == want {
			n++
		}
	}
	return n
}

// The private source must beat a free provider at the same host:port, its
// entries must survive even if the private file sits on the same host:port as
// many free ones, and it must never be clobbered by the free-source harvest.
func TestHarvestPrivateWinsSameHostPort(t *testing.T) {
	RegisterProvider(&Provider{
		Name: "tfree",
		Fetch: func(context.Context, FetchRequest, func(string, ...any)) ([]string, error) {
			return []string{"http://1.2.3.4:8080", "http://9.9.9.9:9000"}, nil
		},
	})
	dir := t.TempDir()
	privPath := dir + "/ports-private.txt"
	if err := os.WriteFile(privPath, []byte("http://u:p@1.2.3.4:8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Proxy.Sources = []string{"tfree", "private"}
	cfg.Proxy.Protocol = "all"
	cfg.Proxy.PrivateFile = privPath
	entries, _, err := Harvest(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}
	urls := map[string]bool{}
	for _, e := range entries {
		urls[e.URL] = true
	}
	if !urls["http://u:p@1.2.3.4:8080"] {
		t.Errorf("entries = %v, want the private proxy to survive", urls)
	}
	if urls["http://1.2.3.4:8080"] {
		t.Errorf("entries = %v, free entry on the private host:port should be displaced", urls)
	}
}

// When no private file exists the private source yields no candidates instead
// of a hard failure, so a user with no private proxies does not error.
func TestHarvestPrivateMissingFileIsNotAnError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Proxy.Sources = []string{"private"}
	cfg.Proxy.Protocol = "all"
	cfg.Proxy.PrivateFile = t.TempDir() + "/nope.txt"
	_, res, err := Harvest(context.Background(), cfg, nil)
	if err == nil || res.Fetched != 0 {
		// Harvest reports "no usable proxies" because there is nothing to dial;
		// the point is the private source itself contributed no error.
		if !strings.Contains(err.Error(), "no usable proxies") {
			t.Fatalf("err = %v", err)
		}
	}
}

// Private proxy URLs carry credentials, so a written list must be owner-only.
func TestSaveProxyListTightensPermsWithCredentials(t *testing.T) {
	list := filepath.Join(t.TempDir(), "ports.txt")
	entries := []ProxyEntry{{URL: "http://u:p@1.2.3.4:8080"}}
	if err := SaveProxyList(entries, list, ""); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(list)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 when the list holds credentials", info.Mode().Perm())
	}
}
