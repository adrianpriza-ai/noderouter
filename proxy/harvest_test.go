package proxy

// The fetchers talk to third-party APIs whose failure modes are all different,
// and they are the only part of the router that runs unattended overnight. The
// tests here stand in for those APIs over a local server; what matters is that
// each one turns a broken or hostile response into a clear error rather than a
// short proxy list nobody notices.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSource serves canned responses per path and records what was asked for.
// The fetchers that fan out do so concurrently, so the recording is guarded:
// this is a shared server across several in-flight requests.
type fakeSource struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string // every request URI, in order
}

// requested returns the recorded request URIs.
func (f *fakeSource) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// newFakeSource starts a server over the given path -> body (or status) map.
// newFakeSource starts a server over the given path -> response map.
// A response is a string body, any other value is JSON-encoded, and an int is
// an HTTP status. A response of the form queryResponse answers differently per
// query string, which is how one path serves each protocol a fan-out asks for.
func newFakeSource(t *testing.T, routes map[string]any) *fakeSource {
	t.Helper()
	f := &fakeSource{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.RequestURI())
		f.mu.Unlock()

		v, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		// A queryResponse answers differently per query string, which is how one
		// path serves each protocol a fan-out asks for. The empty key is the
		// fallback for a query that matches nothing.
		if qr, ok := v.(queryResponse); ok {
			body, matched := qr.responses[r.URL.RawQuery]
			if !matched {
				body = qr.responses[""]
			}
			writeFakeBody(w, body)
			return
		}
		writeFakeBody(w, v)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// ------------------------------------------------------------ geonode ---

// Geonode answers with a nested payload; a limit past its cap comes back as a
// null "data", which is why the caller clamps.
func TestFetchGeoNode(t *testing.T) {
	srv := newFakeSource(t, map[string]any{
		"/api/proxy-list": map[string]any{
			"data": []map[string]any{
				{"ip": "1.1.1.1", "port": "8080", "protocols": []string{"http", "socks5"}},
				{"ip": "2.2.2.2", "port": "3128", "protocols": []string{"socks5"}},
			},
		},
	})

	// Point the fetcher at the local server by rewriting the endpoint.
	useFakeSource(t, srv)
	got, err := FetchGeoNode(context.Background(), 10, "all")
	if err != nil {
		t.Fatalf("FetchGeoNode: %v", err)
	}
	want := []string{"http://1.1.1.1:8080", "socks5://2.2.2.2:3128"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The protocol filter has to keep only proxies of the requested scheme.
func TestFetchGeoNodeHonoursProtocolFilter(t *testing.T) {
	srv := newFakeSource(t, map[string]any{
		"/api/proxy-list": map[string]any{
			"data": []map[string]any{
				{"ip": "1.1.1.1", "port": "8080", "protocols": []string{"http"}},
				{"ip": "2.2.2.2", "port": "3128", "protocols": []string{"socks5"}},
				{"ip": "3.3.3.3", "port": "1080", "protocols": []string{"socks4"}},
			},
		},
	})

	useFakeSource(t, srv)
	got, err := FetchGeoNode(context.Background(), 10, "socks5")
	if err != nil {
		t.Fatalf("FetchGeoNode: %v", err)
	}
	if len(got) != 1 || got[0] != "socks5://2.2.2.2:3128" {
		t.Errorf("got %v, want only the socks5 proxy", got)
	}
}

// ------------------------------------------------------- roundproxies ---

func TestFetchRoundProxies(t *testing.T) {
	srv := newFakeSource(t, map[string]any{
		"/api/get-free-proxies/": map[string]any{
			"data": []map[string]any{
				{"ip": "1.1.1.1", "port": "8080", "protocols": []string{"http"}},
				{"ip": "2.2.2.2", "port": "1080", "protocols": []string{"socks5"}},
			},
		},
	})
	useFakeSource(t, srv)
	got, err := FetchRoundProxies(context.Background(), 10, "all")
	if err != nil {
		t.Fatalf("FetchRoundProxies: %v", err)
	}
	// The faux check confirms page=1 and uptime=low reached the server.
	seen := srv.requested()
	if len(seen) != 1 || !strings.Contains(seen[0], "uptime=low") || !strings.Contains(seen[0], "limit=10") {
		t.Errorf("requests = %v, want uptime=low and limit=10", seen)
	}
	if len(got) != 2 || got[0] != "http://1.1.1.1:8080" || got[1] != "socks5://2.2.2.2:1080" {
		t.Errorf("got %v", got)
	}
}

func TestFetchRoundProxiesHonoursProtocolFilter(t *testing.T) {
	srv := newFakeSource(t, map[string]any{
		"/api/get-free-proxies/": map[string]any{
			"data": []map[string]any{
				{"ip": "1.1.1.1", "port": "8080", "protocols": []string{"http"}},
				{"ip": "2.2.2.2", "port": "1080", "protocols": []string{"socks5"}},
			},
		},
	})
	useFakeSource(t, srv)
	got, err := FetchRoundProxies(context.Background(), 10, "socks5")
	if err != nil {
		t.Fatalf("FetchRoundProxies: %v", err)
	}
	if len(got) != 1 || got[0] != "socks5://2.2.2.2:1080" {
		t.Errorf("got %v, want only the socks5 proxy", got)
	}
}

func TestFetchRoundProxiesReportsHTTPError(t *testing.T) {
	srv := newFakeSource(t, map[string]any{"/api/get-free-proxies/": http.StatusTeapot})
	useFakeSource(t, srv)
	if _, err := FetchRoundProxies(context.Background(), 10, "all"); err == nil || !strings.Contains(err.Error(), "roundproxies") {
		t.Errorf("err = %v, want the roundproxies error", err)
	}
}

func TestFetchRoundProxiesRejectsMalformedJSON(t *testing.T) {
	srv := newFakeSource(t, map[string]any{"/api/get-free-proxies/": "{not json"})
	useFakeSource(t, srv)
	if _, err := FetchRoundProxies(context.Background(), 10, "all"); err == nil {
		t.Error("expected an error for a malformed payload")
	}
}

// A null "data" payload is what the API returns when the limit exceeds its
// cap. That is an empty list, not a crash.
func TestFetchGeoNodeHandlesNullData(t *testing.T) {
	srv := newFakeSource(t, map[string]any{
		"/api/proxy-list": map[string]any{"data": nil},
	})

	useFakeSource(t, srv)
	got, err := FetchGeoNode(context.Background(), 100000, "all")
	if err != nil {
		t.Fatalf("FetchGeoNode with a null data payload: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want an empty list", got)
	}
}

// A malformed payload must be an error: silently returning nothing here would
// look like "the source is empty" and quietly shrink the pool.
func TestFetchGeoNodeRejectsMalformedJSON(t *testing.T) {
	srv := newFakeSource(t, map[string]any{"/api/proxy-list": "{not json"})

	useFakeSource(t, srv)
	if _, err := FetchGeoNode(context.Background(), 10, "all"); err == nil {
		t.Error("expected an error for a malformed payload")
	}
}

// A non-200 answer must name the status so the log says which source failed.
func TestFetchGeoNodeReportsHTTPError(t *testing.T) {
	srv := newFakeSource(t, map[string]any{"/api/proxy-list": http.StatusInternalServerError})

	useFakeSource(t, srv)
	_, err := FetchGeoNode(context.Background(), 10, "all")
	if err == nil {
		t.Fatal("expected an error for a 404 response")
	}
	if !strings.Contains(err.Error(), "geonode") {
		t.Errorf("error = %v, want it to name the source", err)
	}
}

// ------------------------------------------------------- chillyproxy ---

// ChillyProxy serves bare ip:port lines, one protocol per request, so the
// fetcher fans out and tags each line with the protocol it was fetched under.
func TestFetchChillyProxyTagsEachProtocol(t *testing.T) {
	srv := newFakeSource(t, map[string]any{
		"/api/tools/free-proxies": queryResponse{responses: map[string]any{
			"protocol=http&country=ALL":   "1.1.1.1:8080\n2.2.2.2:3128\n",
			"protocol=socks5&country=ALL": "3.3.3.3:1080\n",
		}},
	})

	useFakeSource(t, srv)
	got, err := FetchChillyProxy(context.Background(), "all")
	if err != nil {
		t.Fatalf("FetchChillyProxy: %v", err)
	}
	want := map[string]bool{
		"http://1.1.1.1:8080":   true,
		"http://2.2.2.2:3128":   true,
		"socks5://3.3.3.3:1080": true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %d entries", got, len(want))
	}
	for _, u := range got {
		if !want[u] {
			t.Errorf("unexpected proxy %q in %v", u, got)
		}
	}
	// Both protocols have to be represented, or the fan-out did not happen.
	var sawHTTP, sawSocks bool
	for _, u := range got {
		if strings.HasPrefix(u, "http://") {
			sawHTTP = true
		}
		if strings.HasPrefix(u, "socks5://") {
			sawSocks = true
		}
	}
	if !sawHTTP || !sawSocks {
		t.Errorf("got %v, want both http and socks5 proxies", got)
	}
}

// A single protocol filter means a single request, and only that scheme in the
// result.
func TestFetchChillyProxyWithProtocolFilter(t *testing.T) {
	srv := newFakeSource(t, map[string]any{
		"/api/tools/free-proxies": "3.3.3.3:1080\n",
	})

	useFakeSource(t, srv)
	got, err := FetchChillyProxy(context.Background(), "socks5")
	if err != nil {
		t.Fatalf("FetchChillyProxy: %v", err)
	}
	if len(got) != 1 || got[0] != "socks5://3.3.3.3:1080" {
		t.Errorf("got %v, want only the socks5 proxy", got)
	}
	// One protocol filter means one request, not the full fan-out.
	asked := srv.requested()
	if len(asked) != 1 {
		t.Fatalf("made %d requests (%v), want exactly 1", len(asked), asked)
	}
	if !strings.Contains(asked[0], "protocol=socks5") {
		t.Errorf("request %q, want it to ask for the socks5 protocol", asked[0])
	}
}

// A line repeated within one protocol's response is deduplicated. The
// cross-protocol case is left to Harvest, which is where the host:port dedupe
// lives: FetchChillyProxy tags each line with the scheme it was fetched under,
// so the same host:port under two schemes is two entries by the time they
// leave this function.
func TestFetchChillyProxyDedupesWithinAProtocol(t *testing.T) {
	srv := newFakeSource(t, map[string]any{
		"/api/tools/free-proxies": queryResponse{responses: map[string]any{
			"protocol=http&country=ALL":   "1.1.1.1:8080\n1.1.1.1:8080\n2.2.2.2:3128\n",
			"protocol=socks5&country=ALL": "3.3.3.3:1080\n",
		}},
	})

	useFakeSource(t, srv)
	got, err := FetchChillyProxy(context.Background(), "all")
	if err != nil {
		t.Fatalf("FetchChillyProxy: %v", err)
	}
	seen := map[string]int{}
	for _, u := range got {
		seen[u]++
	}
	if len(got) != 3 {
		t.Errorf("got %v, want the repeat collapsed to 3 entries", got)
	}
	for u, n := range seen {
		if n > 1 {
			t.Errorf("%s appears %d times, want once", u, n)
		}
	}
}

// An empty response is an error, so the caller reports the source as failed
// rather than as having contributed nothing.
func TestFetchChillyProxyEmptyResponse(t *testing.T) {
	srv := newFakeSource(t, map[string]any{
		"/api/tools/free-proxies": "\n\n# nothing here\n",
	})

	useFakeSource(t, srv)
	_, err := FetchChillyProxy(context.Background(), "all")
	if err == nil {
		t.Fatal("expected an error for an empty response")
	}
	if !strings.Contains(err.Error(), "no usable proxies") {
		t.Errorf("error = %v, want it to say the response held no proxies", err)
	}
}

// One protocol failing must fail the whole fetch rather than returning a
// partial list that looks complete.
func TestFetchChillyProxyReportsPartialFailure(t *testing.T) {
	srv := newFakeSource(t, map[string]any{
		"/api/tools/free-proxies": queryResponse{responses: map[string]any{
			"protocol=http&country=ALL":   "1.1.1.1:8080\n",
			"protocol=socks5&country=ALL": http.StatusInternalServerError,
		}},
	})

	useFakeSource(t, srv)
	_, err := FetchChillyProxy(context.Background(), "all")
	if err == nil {
		t.Fatal("expected an error when one protocol's request fails")
	}
	if !strings.Contains(err.Error(), "chillyproxy") {
		t.Errorf("error = %v, want it to name the source", err)
	}
}

// ------------------------------------------------------- freeproxydb ---

// FreeProxyDB hands back connect_string values that already carry the scheme.
func TestFetchFreeProxyDBUsesConnectString(t *testing.T) {
	srv := newFakeSource(t, map[string]any{
		"/api/proxy/search": queryResponse{responses: map[string]any{
			"country=&protocol=http&anonymity=&speed=0,60&https=0&page_index=1&page_size=50": map[string]any{"data": map[string]any{"data": []map[string]any{
				{"connect_string": "1.1.1.1:8080", "ip": "1.1.1.1", "port": 8080, "protocol": "http"},
				{"connect_string": "http://2.2.2.2:3128", "ip": "2.2.2.2", "port": 3128, "protocol": "http"},
			}}},
			"country=&protocol=socks5&anonymity=&speed=0,60&https=0&page_index=1&page_size=50": map[string]any{"data": map[string]any{"data": []map[string]any{
				{"connect_string": "3.3.3.3:1080", "ip": "3.3.3.3", "port": 1080, "protocol": "socks5"},
			}}},
		}},
	})

	useFakeSource(t, srv)
	got, err := FetchFreeProxyDB(context.Background(), "all")
	if err != nil {
		t.Fatalf("FetchFreeProxyDB: %v", err)
	}
	want := map[string]bool{
		"http://1.1.1.1:8080":   true,
		"http://2.2.2.2:3128":   true,
		"socks5://3.3.3.3:1080": true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %d entries", got, len(want))
	}
	for _, u := range got {
		if !want[u] {
			t.Errorf("unexpected proxy %q in %v", u, got)
		}
	}
}

// A missing connect_string is rebuilt from the protocol, ip and port rather
// than dropping the entry.
func TestFetchFreeProxyDBSynthesizesConnectString(t *testing.T) {
	srv := newFakeSource(t, map[string]any{
		"/api/proxy/search": map[string]any{"data": map[string]any{"data": []map[string]any{
			{"connect_string": "", "ip": "1.1.1.1", "port": 8080, "protocol": "http"},
		}}},
	})

	useFakeSource(t, srv)
	got, err := FetchFreeProxyDB(context.Background(), "http")
	if err != nil {
		t.Fatalf("FetchFreeProxyDB: %v", err)
	}
	if len(got) != 1 || got[0] != "http://1.1.1.1:8080" {
		t.Errorf("got %v, want the synthesized http://1.1.1.1:8080", got)
	}
}

// The reason this fetcher makes one request per protocol: a single combined
// request returns a page dominated by http, starving the socks results.
func TestFetchFreeProxyDBFansOutPerProtocol(t *testing.T) {
	srv := newFakeSource(t, map[string]any{
		"/api/proxy/search": queryResponse{responses: map[string]any{
			"country=&protocol=http&anonymity=&speed=0,60&https=0&page_index=1&page_size=50": map[string]any{"data": map[string]any{"data": []map[string]any{
				{"connect_string": "1.1.1.1:8080", "protocol": "http"},
			}}},
			"country=&protocol=socks5&anonymity=&speed=0,60&https=0&page_index=1&page_size=50": map[string]any{"data": map[string]any{"data": []map[string]any{
				{"connect_string": "3.3.3.3:1080", "protocol": "socks5"},
			}}},
		}},
	})

	useFakeSource(t, srv)
	got, err := FetchFreeProxyDB(context.Background(), "all")
	if err != nil {
		t.Fatalf("FetchFreeProxyDB: %v", err)
	}
	// The whole reason this fetcher fans out: without a per-protocol request the
	// socks results never arrive, because the single page fills with http.
	var sawSocks bool
	for _, u := range got {
		if strings.HasPrefix(u, "socks5://") {
			sawSocks = true
		}
	}
	if !sawSocks {
		t.Errorf("got %v, want the socks5 results represented", got)
	}
}

func TestFetchFreeProxyDBEmptyAndErrors(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		srv := newFakeSource(t, map[string]any{
			"/api/proxy/search": map[string]any{"data": map[string]any{"data": []map[string]any{}}},
		})
		useFakeSource(t, srv)
		_, err := FetchFreeProxyDB(context.Background(), "http")
		if err == nil {
			t.Fatal("expected an error for an empty response")
		}
		if !strings.Contains(err.Error(), "no usable proxies") {
			t.Errorf("error = %v, want it to say the response held no proxies", err)
		}
	})

	t.Run("http error", func(t *testing.T) {
		srv := newFakeSource(t, map[string]any{"/api/proxy/search": http.StatusBadGateway})
		useFakeSource(t, srv)
		_, err := FetchFreeProxyDB(context.Background(), "http")
		if err == nil {
			t.Fatal("expected an error for a 502 response")
		}
		if !strings.Contains(err.Error(), "freeproxydb") {
			t.Errorf("error = %v, want it to name the source", err)
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		srv := newFakeSource(t, map[string]any{"/api/proxy/search": "{oops"})
		useFakeSource(t, srv)
		if _, err := FetchFreeProxyDB(context.Background(), "http"); err == nil {
			t.Fatal("expected an error for a malformed payload")
		}
	})
}

// -------------------------------------------------------- proxyscrape ---

// A "protocol=all" response mixes prefixed and bare lines; the bare ones get
// the default scheme rather than the literal string "all".
func TestFetchProxyScrapeDoesNotEmitAllAsAScheme(t *testing.T) {
	srv := newFakeSource(t, map[string]any{
		"/v4/free-proxy-list/get": "http://1.1.1.1:8080\n2.2.2.2:3128\nsocks5://3.3.3.3:1080\n",
	})

	useFakeSource(t, srv)
	got, err := FetchProxyScrape(context.Background(), 1000, "all", 10)
	if err != nil {
		t.Fatalf("FetchProxyScrape: %v", err)
	}
	for _, u := range got {
		if strings.HasPrefix(u, "all://") {
			t.Errorf("got %q; the filter must not become the scheme", u)
		}
	}
	if len(got) != 3 {
		t.Errorf("got %v, want 3 proxies", got)
	}
}

// ---------------------------------------------------------- raw lists ---

// A raw list is deduped, filtered by protocol and capped, since a single source
// can otherwise contribute thousands of untested proxies.
func TestFetchRawListDedupesFiltersAndCaps(t *testing.T) {
	var body strings.Builder
	for i := range rawListMaxProxies + 50 {
		fmt.Fprintf(&body, "10.0.%d.%d:8080\n", i/256, i%256)
		body.WriteString("http://1.1.1.1:8080\n") // a duplicate
		body.WriteString("socks5://2.2.2.2:1080\n")
		body.WriteString("garbage\n")
	}

	srv := newFakeSource(t, map[string]any{"/list": body.String()})

	t.Run("caps the list", func(t *testing.T) {
		got, err := FetchRawList(context.Background(), srv.URL+"/list", "all")
		if err != nil {
			t.Fatalf("FetchRawList: %v", err)
		}
		if len(got) != rawListMaxProxies {
			t.Errorf("got %d proxies, want the cap of %d", len(got), rawListMaxProxies)
		}
	})

	t.Run("honours the protocol filter", func(t *testing.T) {
		got, err := FetchRawList(context.Background(), srv.URL+"/list", "socks5")
		if err != nil {
			t.Fatalf("FetchRawList: %v", err)
		}
		if len(got) == 0 {
			t.Fatal("got no proxies for socks5, want at least the socks5 line")
		}
		for _, u := range got {
			if !strings.HasPrefix(u, "socks5://") {
				t.Errorf("got %q under a socks5 filter", u)
			}
		}
	})
}

// A list with nothing usable has to be an error, so the source is reported as
// failed rather than as having silently contributed nothing.
func TestFetchRawListErrors(t *testing.T) {
	srv := newFakeSource(t, map[string]any{
		"/empty":  "garbage\nmore garbage\n",
		"/broken": "{",
	})

	if _, err := FetchRawList(context.Background(), srv.URL+"/empty", "all"); err == nil {
		t.Error("expected an error for a list with no usable proxies")
	}
	if _, err := FetchRawList(context.Background(), srv.URL+"/missing", "all"); err == nil {
		t.Error("expected an error for a 404 response")
	}
	// A URL that cannot even be requested.
	if _, err := FetchRawList(context.Background(), "http://127.0.0.1:1/nope", "all"); err == nil {
		t.Error("expected an error for an unreachable endpoint")
	}
}

// SourceEndpoint is what lets a source be named or spelled out as a URL, which
// is how new lists get added without a code change.
func TestSourceEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		want  string
		found bool
	}{
		{"known name", "iplocate", SourceURLs["iplocate"], true},
		{"name is case-insensitive", "  IPLocate ", SourceURLs["iplocate"], true},
		{"http url", "http://example.com/list.txt", "http://example.com/list.txt", true},
		{"https url", "https://example.com/list.txt", "https://example.com/list.txt", true},
		{"unknown name", "nosuchsource", "", false},
		{"unsupported scheme", "ftp://example.com/list.txt", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := SourceEndpoint(tc.in)
			if ok != tc.found {
				t.Fatalf("SourceEndpoint(%q) ok = %v, want %v", tc.in, ok, tc.found)
			}
			if got != tc.want {
				t.Errorf("SourceEndpoint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// -------------------------------------------------------------- saving ---

// SaveProxyList writes the plain list and the meta sidecar together; the router
// needs both to rank proxies.
func TestSaveProxyListWritesListAndMeta(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "nested", "ports.txt")
	meta := filepath.Join(dir, "nested", "ports.meta.txt")

	entries := []ProxyEntry{
		{URL: "http://1.1.1.1:8080", LatencyMS: 120, ExitIP: "9.9.9.9", CheckedAt: timeNow()},
		{URL: "http://2.2.2.2:3128", LatencyMS: 80, ExitIP: "8.8.8.8", CheckedAt: timeNow()},
	}
	if err := SaveProxyList(entries, list, meta); err != nil {
		t.Fatalf("SaveProxyList: %v", err)
	}

	// The list is one proxy per line, in the order given.
	raw, err := os.ReadFile(list)
	if err != nil {
		t.Fatalf("read list: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 || lines[0] != entries[0].URL || lines[1] != entries[1].URL {
		t.Errorf("list = %q, want the two URLs in order", raw)
	}

	// The sidecar round-trips the measurements.
	rawMeta, err := os.ReadFile(meta)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	var got []ProxyEntry
	if err := json.Unmarshal(rawMeta, &got); err != nil {
		t.Fatalf("meta is not valid JSON: %v", err)
	}
	if len(got) != 2 || got[0].LatencyMS != 120 || got[0].ExitIP != "9.9.9.9" {
		t.Errorf("meta = %+v, want the latencies and exit IPs preserved", got)
	}

	// And the pair reloads into the same entries.
	loaded, err := LoadProxyList(list, meta)
	if err != nil {
		t.Fatalf("LoadProxyList: %v", err)
	}
	if len(loaded) != 2 || loaded[0].LatencyMS != 120 {
		t.Errorf("loaded = %+v, want the measurements restored", loaded)
	}
}

// The meta sidecar marshals the same ProxyEntry.URL values as the list, so it
// carries the same credentials — on a shared host a world-readable meta would
// hand every private proxy's user:pass to any local user. With a credentialed
// entry both files must be 0600; with only free proxies the meta keeps 0644.
func TestSaveProxyListMetaModeMatchesList(t *testing.T) {
	t.Run("credentialed entries get 0600 on both files", func(t *testing.T) {
		dir := t.TempDir()
		list := filepath.Join(dir, "ports.txt")
		meta := filepath.Join(dir, "ports.meta.txt")

		entries := []ProxyEntry{
			{URL: "http://gwuser:s3cr3t@10.0.0.1:8080", LatencyMS: 120, CheckedAt: timeNow()},
			{URL: "http://1.1.1.1:8080", LatencyMS: 80, CheckedAt: timeNow()},
		}
		if err := SaveProxyList(entries, list, meta); err != nil {
			t.Fatalf("SaveProxyList: %v", err)
		}

		for name, path := range map[string]string{"list": list, "meta": meta} {
			st, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat %s: %v", name, err)
			}
			if perm := st.Mode().Perm(); perm != 0o600 {
				t.Errorf("%s mode = %o, want 600", name, perm)
			}
		}

		// The credentials are still in the files — just not readable by others.
		rawMeta, err := os.ReadFile(meta)
		if err != nil {
			t.Fatalf("read meta: %v", err)
		}
		if !strings.Contains(string(rawMeta), "s3cr3t") {
			t.Errorf("meta should still carry the entries, got %q", rawMeta)
		}
	})

	t.Run("free-only entries keep 0644 on meta", func(t *testing.T) {
		dir := t.TempDir()
		list := filepath.Join(dir, "ports.txt")
		meta := filepath.Join(dir, "ports.meta.txt")

		entries := []ProxyEntry{
			{URL: "http://1.1.1.1:8080", LatencyMS: 120, CheckedAt: timeNow()},
			{URL: "http://2.2.2.2:3128", LatencyMS: 80, CheckedAt: timeNow()},
		}
		if err := SaveProxyList(entries, list, meta); err != nil {
			t.Fatalf("SaveProxyList: %v", err)
		}

		st, err := os.Stat(meta)
		if err != nil {
			t.Fatalf("stat meta: %v", err)
		}
		if perm := st.Mode().Perm(); perm != 0o644 {
			t.Errorf("meta mode = %o, want 644", perm)
		}

		st, err = os.Stat(list)
		if err != nil {
			t.Fatalf("stat list: %v", err)
		}
		if perm := st.Mode().Perm(); perm != 0o644 {
			t.Errorf("list mode = %o, want 644", perm)
		}
	})
}

// An empty meta path writes just the list, which is what a caller that does not
// want measurements passes.
func TestSaveProxyListWithoutMeta(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "ports.txt")
	if err := SaveProxyList([]ProxyEntry{{URL: "http://1.1.1.1:8080"}}, list, ""); err != nil {
		t.Fatalf("SaveProxyList: %v", err)
	}
	if _, err := os.Stat(list); err != nil {
		t.Errorf("list was not written: %v", err)
	}
	if entries, err := os.ReadDir(dir); err != nil {
		t.Fatal(err)
	} else if len(entries) != 1 {
		t.Errorf("wrote %d files, want only the list", len(entries))
	}
}

// An empty list path is a caller bug and must be refused rather than writing
// somewhere surprising.
func TestSaveProxyListRejectsEmptyPath(t *testing.T) {
	if err := SaveProxyList([]ProxyEntry{{URL: "http://1.1.1.1:8080"}}, "", ""); err == nil {
		t.Error("expected an error for an empty list path")
	}
}

// writeFileAtomic is what makes a save safe to read concurrently: the temp file
// has to be gone once it returns, whatever happened.
func TestWriteFileAtomicLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "ports.txt")

	if err := writeFileAtomic(dest, []byte("http://1.1.1.1:8080\n"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "ports.txt" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only ports.txt", names)
	}
}

// The write has to replace the old contents outright, not append or interleave
// with them.
func TestWriteFileAtomicReplacesContents(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "ports.txt")

	if err := os.WriteFile(dest, []byte("old contents that are longer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(dest, []byte("new\n"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new\n" {
		t.Errorf("contents = %q, want the file replaced outright", got)
	}
}

// The mode is applied, since the ports file carries no secrets but the config
// does and they share the helper's caller.
func TestWriteFileAtomicAppliesMode(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "secret.json")
	if err := writeFileAtomic(dest, []byte("{}"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	st, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

// Two concurrent writes to the same destination must not interleave: each gets
// its own temp file and the rename is atomic.
func TestWriteFileAtomicConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "ports.txt")

	const writers = 8
	done := make(chan error, writers)
	for i := range writers {
		go func() {
			done <- writeFileAtomic(dest, []byte(strings.Repeat("x", i+1)), 0o644)
		}()
	}
	for range writers {
		if err := <-done; err != nil {
			t.Fatalf("concurrent writeFileAtomic: %v", err)
		}
	}

	// Whatever won, the file is intact and no temp files remain.
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("destination missing after concurrent writes: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only the destination", names)
	}
}

// A write into a directory that does not exist must fail cleanly rather than
// panic or leave a stray temp file behind.
func TestWriteFileAtomicMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	if err := writeFileAtomic(filepath.Join(dir, "ports.txt"), []byte("x"), 0o644); err == nil {
		t.Error("expected an error writing into a missing directory")
	}
	if entries, err := os.ReadDir(filepath.Dir(dir)); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tmp") {
				t.Errorf("left a temp file behind: %s", e.Name())
			}
		}
	}
}

// LoadProxyList dedupes by host:port as well as by full URL, so the same proxy
// listed under two schemes is one entry.
func TestLoadProxyListDedupesByHostPort(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "ports.txt")
	content := "http://1.2.3.4:8080\nsocks5://1.2.3.4:8080\nhttp://5.6.7.8:3128\n"
	if err := os.WriteFile(list, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := LoadProxyList(list, "")
	if err != nil {
		t.Fatalf("LoadProxyList: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries (%v), want 2", len(entries), entries)
	}
	if entries[0].URL != "http://1.2.3.4:8080" || entries[1].URL != "http://5.6.7.8:3128" {
		t.Errorf("got %v, want the first spelling kept and the other proxies", entries)
	}
}

// -------------------------------------------------------- normalisation ---

// BareScheme turns a protocol filter into a real scheme, so "all" never ends up
// in a URL.
func TestBareScheme(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "http"},
		{"all", "http"},
		{"any", "http"},
		{"ALL", "http"},
		{"  all  ", "http"},
		{"http", "http"},
		{"socks5", "socks5"},
		{"  SOCKS5 ", "socks5"},
	} {
		if got := BareScheme(tc.in); got != tc.want {
			t.Errorf("BareScheme(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// SchemeAllowed decides what survives the protocol filter, and socks4 has to be
// excluded: it is parsed but this build cannot dial it.
func TestSchemeAllowed(t *testing.T) {
	for _, tc := range []struct {
		scheme, protocol string
		want             bool
	}{
		{"http", "", true},
		{"http", "all", true},
		{"http", "any", true},
		{"socks5", "all", true},
		{"socks5h", "all", true},
		{"socks4", "all", false}, // parsed but not dialable
		{"socks4a", "all", false},
		{"http", "http", true},
		{"https", "http", true}, // https proxies serve plain http too
		{"socks5", "http", false},
		{"socks5", "socks5", true},
		{"http", "socks5", false},
	} {
		if got := SchemeAllowed(tc.scheme, tc.protocol); got != tc.want {
			t.Errorf("SchemeAllowed(%q, %q) = %v, want %v", tc.scheme, tc.protocol, got, tc.want)
		}
	}
}

// protocolsToFetch expands a filter into the requests to make; "all" has to ask
// for every scheme the build can dial, or socks results never arrive.
func TestProtocolsToFetch(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", []string{"http", "socks5"}},
		{"all", []string{"http", "socks5"}},
		{"any", []string{"http", "socks5"}},
		{"  ALL ", []string{"http", "socks5"}},
		{"http", []string{"http"}},
		{"socks5", []string{"socks5"}},
		{"socks4", []string{"socks4"}},
	} {
		got := protocolsToFetch(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("protocolsToFetch(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("protocolsToFetch(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}

// HostPortKey is what makes cross-scheme dedupe work, and it has to ignore
// credentials and the scheme while keeping the port.
func TestHostPortKey(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"http://1.2.3.4:8080", "1.2.3.4:8080"},
		{"socks5://1.2.3.4:8080", "1.2.3.4:8080"},
		{"socks5://user:pass@1.2.3.4:1080", "1.2.3.4:1080"},
		{"https://example.com:443", "example.com:443"},
		{"not a url at all", ""},
	} {
		if got := HostPortKey(tc.in); got != tc.want {
			t.Errorf("HostPortKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ------------------------------------------------------------- helpers ---

// queryResponse maps a request's raw query string to the response for it. The
// fetchers that fan out (ChillyProxy, FreeProxyDB) differ only by a protocol
// parameter, so this is how one fake path serves all of them.
type queryResponse struct{ responses map[string]any }

// writeFakeBody answers one request: an int is an HTTP status, a string is a
// verbatim body, anything else is JSON. A nil body is empty, which is how a
// queryResponse with no fallback registered answers an unexpected query.
func writeFakeBody(w http.ResponseWriter, v any) {
	switch body := v.(type) {
	case nil:
	case int:
		http.Error(w, http.StatusText(body), body)
	case string:
		_, _ = w.Write([]byte(body))
	default:
		_ = json.NewEncoder(w).Encode(body)
	}
}

// useFakeSource points the harvest client at a local test server for the
// duration of the test, then puts the real client back. The fetchers build
// absolute third-party URLs, so redirecting at the transport layer is how the
// suite exercises their request construction and response handling without
// reaching the network.
func useFakeSource(t *testing.T, srv *fakeSource) {
	t.Helper()
	orig := harvestClient
	harvestClient = &http.Client{
		Timeout: orig.Timeout,
		Transport: &rewriteTransport{
			host: srv.Listener.Addr().String(),
		},
	}
	t.Cleanup(func() { harvestClient = orig })
}

// rewriteTransport sends every request to the test server while preserving the
// path and query the fetcher built, so the endpoint construction stays under
// test alongside the parsing.
type rewriteTransport struct{ host string }

func (rt *rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = rt.host
	// The fetchers' endpoints are absolute third-party URLs, so only the host
	// is rewritten: the path and query they built are what these tests are
	// actually checking, and the fake source routes on them.
	clone.Host = ""
	return http.DefaultTransport.RoundTrip(clone)
}

// timeNow is a fixed clock point for entries that need a CheckedAt.
func timeNow() time.Time { return time.Now().UTC() }

// TestFakeSourceRecordsRequestURIs documents the fake's own contract: the
// handlers route on Path but records RequestURI, so a queryResponse keyed by
// the raw query matches what a fetcher actually sent.
func TestFakeSourceRecordsRequestURIs(t *testing.T) {
	srv := newFakeSource(t, map[string]any{
		"/thing": queryResponse{responses: map[string]any{
			"a=1": "first",
			"b=2": "second",
		}},
	})

	for _, tc := range []struct{ query, want string }{
		{"?a=1", "first"},
		{"?b=2", "second"},
		{"?c=3", ""}, // no fallback registered
	} {
		resp, err := http.Get(srv.URL + "/thing" + tc.query)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.query, err)
		}
		body := make([]byte, 16)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		if got := string(body[:n]); got != tc.want {
			t.Errorf("GET ?%s = %q, want %q", tc.query, got, tc.want)
		}
	}
	recorded := srv.requested()
	if len(recorded) != 3 {
		t.Errorf("recorded %v, want three request URIs", recorded)
	}
	if recorded[0] != "/thing?a=1" {
		t.Errorf("recorded %q, want the query included", recorded[0])
	}
}

// TestFakeSourceRoutesOnPath documents the second half of the fake's contract:
// routes are keyed by path, so two sources on one host do not collide.
func TestFakeSourceRoutesOnPath(t *testing.T) {
	srv := newFakeSource(t, map[string]any{
		"/a": "from a",
		"/b": "from b",
	})

	for path, want := range map[string]string{"/a": "from a", "/b": "from b"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != want {
			t.Errorf("GET %s = %q, want %q", path, body, want)
		}
	}

	// An unregistered path 404s, which is how the error-path tests work.
	resp, err := http.Get(srv.URL + "/missing")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /missing = %d, want 404", resp.StatusCode)
	}
}
