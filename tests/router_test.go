package tests

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeProxy(t *testing.T) {
	cases := []struct {
		line   string
		scheme string
		want   string
	}{
		{"1.2.3.4:8080", "http", "http://1.2.3.4:8080"},
		{"http://1.2.3.4:8080", "http", "http://1.2.3.4:8080"},
		{"https://1.2.3.4:8443", "http", "https://1.2.3.4:8443"},
		{"socks5://1.2.3.4:1080", "http", "socks5://1.2.3.4:1080"},
		{"1.2.3.4:8080:user:pass", "http", "http://user:pass@1.2.3.4:8080"},
		{"socks5://user:pass@1.2.3.4:1080", "http", "socks5://user:pass@1.2.3.4:1080"},
		{"1.2.3.4:8080 # fast", "http", "http://1.2.3.4:8080"},
		{"", "http", ""},
		{"# comment", "http", ""},
		{"not a proxy", "http", ""},
		{"1.2.3.4", "http", ""},
		{"1.2.3.4:99999", "http", ""},
		{"ftp://1.2.3.4:21", "http", ""},
		{"5.6.7.8:3128", "socks5", "socks5://5.6.7.8:3128"},
	}
	for _, tc := range cases {
		got, ok := NormalizeProxy(tc.line, tc.scheme)
		if tc.want == "" {
			if ok {
				t.Errorf("NormalizeProxy(%q) = %q, want rejected", tc.line, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("NormalizeProxy(%q) = %q,%v want %q", tc.line, got, ok, tc.want)
		}
	}
}

func TestResolveModel(t *testing.T) {
	cfg := &Config{
		Provider: map[string]*Provider{
			"LLM Povider": {
				Options: ProviderOptions{BaseURL: "http://a/v1"},
				Models: map[string]*Model{
					// namespace form: clients ask for "llm/minimax-m3"
					"minimax-m3": {Limit: ModelLimit{Context: 10}},
					// exact key wins, and upstreamName overrides the id
					"llm/minimax-m2.7": {Upstream: "minimax-m2.7"},
				},
			},
			"Other": {
				Options: ProviderOptions{BaseURL: "http://b/v1"},
				Models: map[string]*Model{
					"shared": {},
				},
			},
		},
		Routing: RoutingConfig{DefaultProvider: "Other"},
	}
	cfg.ApplyDefaults()

	cases := []struct{ model, provider, upstream string }{
		{"llm/minimax-m3", "LLM Povider", "minimax-m3"},     // namespace prefix stripped
		{"llm/minimax-m2.7", "LLM Povider", "minimax-m2.7"}, // exact key, upstreamName override
		{"minimax-m3", "LLM Povider", "minimax-m3"},
		{"other/shared", "Other", "shared"},
		{"shared", "Other", "shared"},
		{"gpt-4o", "Other", "gpt-4o"}, // unknown, falls back to defaultProvider
	}
	for _, tc := range cases {
		p, upstream, err := cfg.ResolveModel(tc.model)
		if err != nil {
			t.Fatalf("ResolveModel(%q): %v", tc.model, err)
		}
		if got := providerName(cfg, p); got != tc.provider {
			t.Errorf("ResolveModel(%q) provider = %q, want %q", tc.model, got, tc.provider)
		}
		if upstream != tc.upstream {
			t.Errorf("ResolveModel(%q) upstream = %q, want %q", tc.model, upstream, tc.upstream)
		}
	}
}

func TestResolveModelSingleProviderFallback(t *testing.T) {
	cfg := &Config{Provider: map[string]*Provider{
		"Solo": {Options: ProviderOptions{BaseURL: "http://a/v1"}, Models: map[string]*Model{"m": {}}},
	}}
	cfg.ApplyDefaults()
	p, upstream, err := cfg.ResolveModel("something-the-provider-does-not-list")
	if err != nil {
		t.Fatalf("single provider should absorb unknown models: %v", err)
	}
	if providerName(cfg, p) != "Solo" || upstream != "something-the-provider-does-not-list" {
		t.Errorf("got %q / %q", providerName(cfg, p), upstream)
	}
}

func TestResolveModelAmbiguous(t *testing.T) {
	cfg := &Config{Provider: map[string]*Provider{
		"A": {Options: ProviderOptions{BaseURL: "http://a"}, Models: map[string]*Model{"dup": {}}},
		"B": {Options: ProviderOptions{BaseURL: "http://b"}, Models: map[string]*Model{"dup": {}}},
	}}
	cfg.ApplyDefaults()
	if _, _, err := cfg.ResolveModel("dup"); err == nil {
		t.Fatal("expected an error for an ambiguous model")
	} else if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("unexpected error: %v", err)
	}
	// An explicit routes entry resolves the tie.
	cfg.Routes = map[string]string{"dup": "B"}
	p, _, err := cfg.ResolveModel("dup")
	if err != nil {
		t.Fatalf("routes entry did not resolve: %v", err)
	}
	if providerName(cfg, p) != "B" {
		t.Errorf("routes entry picked the wrong provider")
	}
}

func TestResolveUpstream(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"http://x/v1", "/v1/chat/completions", "http://x/v1/chat/completions"},
		{"http://x/v1/", "/v1/chat/completions", "http://x/v1/chat/completions"},
		{"http://x/v1", "/chat/completions", "http://x/v1/chat/completions"},
		{"http://x", "/v1/models", "http://x/v1/models"},
		{"https://x/api", "/v1/responses", "https://x/api/v1/responses"},
		{"https://x/v1/beta", "/v1/beta/models", "https://x/v1/beta/models"},
		// A nested version base must not double the version when the client
		// sends the full path (live failure: /zen/v1 + /v1/chat/completions
		// produced /zen/v1/v1/chat/completions and the upstream 404'd).
		{"https://x/zen/v1", "/v1/chat/completions", "https://x/zen/v1/chat/completions"},
		{"https://x/api/v2", "/v2/messages", "https://x/api/v2/messages"},
		// A client path that is a strict suffix of the base keeps the whole
		// base prefix (live bug: /zen/v1 + /v1 produced /v1, not /zen/v1).
		{"https://x/zen/v1", "/v1", "https://x/zen/v1"},
		{"https://x/api/v2", "/v2", "https://x/api/v2"},
		// ...but a path that merely starts with "v"-ish text is left alone.
		{"https://x/vendor", "/v1/models", "https://x/vendor/v1/models"},
		{"https://x/zen/v1", "/vessels/list", "https://x/zen/v1/vessels/list"},
	}
	for _, tc := range cases {
		got, err := resolveUpstream(tc.base, tc.path, "")
		if err != nil {
			t.Fatalf("resolveUpstream(%q, %q): %v", tc.base, tc.path, err)
		}
		if got.String() != tc.want {
			t.Errorf("resolveUpstream(%q, %q) = %q, want %q", tc.base, tc.path, got, tc.want)
		}
	}
}

func TestResolveUpstreamKeepsQuery(t *testing.T) {
	got, err := resolveUpstream("http://x/v1", "/v1/chat/completions", "stream=true")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "http://x/v1/chat/completions?stream=true" {
		t.Errorf("got %q", got)
	}
}

func TestReplaceModel(t *testing.T) {
	body := []byte(`{"model":"llm/minimax-m3","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`)
	out, ok := replaceModel(body, "minimax-m3")
	if !ok {
		t.Fatal("replaceModel failed")
	}
	var got struct {
		Model    string  `json:"model"`
		Messages []any   `json:"messages"`
		Temp     float64 `json:"temperature"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("patched body is not valid json: %v", err)
	}
	if got.Model != "minimax-m3" {
		t.Errorf("model = %q", got.Model)
	}
	if len(got.Messages) != 1 || got.Temp != 0.7 {
		t.Errorf("other fields were damaged: %s", out)
	}
	if strings.Contains(string(out), "llm/") {
		t.Errorf("old model id still present: %s", out)
	}
}

// TestReplaceModelOddSpacing covers bodies the old byte-search missed: a space
// before the colon, a pretty-printed body, and a reordered object. A miss was
// silent, forwarding the client's own model id upstream.
func TestReplaceModelOddSpacing(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"space before colon", `{ "model" : "a", "messages": [] }`},
		{"pretty printed", "{\n  \"model\": \"a\",\n  \"messages\": []\n}"},
		{"no space", `{"model":"a","messages":[]}`},
		{"model not first", `{"messages":[],"model":"a"}`},
		{"duplicate keys", `{"model":"a","messages":[],"model":"a"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, ok := replaceModel([]byte(tc.body), "b")
			if !ok {
				t.Fatalf("replaceModel failed on %s", tc.body)
			}
			var got struct {
				Model    string `json:"model"`
				Messages []any  `json:"messages"`
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("patched body is not valid json: %v\n%s", err, out)
			}
			if got.Model != "b" {
				t.Errorf("model = %q, want b", got.Model)
			}
		})
	}

	// A body with no model key reports false rather than guessing.
	if _, ok := replaceModel([]byte(`{"messages":[]}`), "b"); ok {
		t.Error("replaceModel rewrote a body with no model key")
	}
}

// TestReplaceModelMultipart covers the whisper shape: a multipart/form-data
// upload whose model id rides in a form field beside the audio file. The
// rewrite has to change only that field and leave the file part byte for
// byte, since audio is data the provider has to hear exactly.
func TestReplaceModelMultipart(t *testing.T) {
	const boundary = "testboundary123"
	const contentType = "multipart/form-data; boundary=" + boundary
	audio := "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVphYmNkZWZnaGlqa2xtbm9wcXJzdA==//79"
	body := "--" + boundary + "\r\n" +
		`Content-Disposition: form-data; name="model"` + "\r\n\r\n" +
		"whisper" + "\r\n" +
		"--" + boundary + "\r\n" +
		`Content-Disposition: form-data; name="file"; filename="a.mp3"` + "\r\n" +
		"Content-Type: audio/mpeg" + "\r\n\r\n" +
		audio + "\r\n" +
		"--" + boundary + "\r\n" +
		`Content-Disposition: form-data; name="response_format"` + "\r\n\r\n" +
		"json" + "\r\n" +
		"--" + boundary + "--\r\n"

	out, ok := replaceModelCT([]byte(body), contentType, "whisper-1")
	if !ok {
		t.Fatal("replaceModelCT failed on a multipart body")
	}
	if strings.Contains(string(out), "\"model\"\r\n\r\nwhisper\r\n") {
		t.Errorf("old model id still present:\n%s", out)
	}
	if !strings.Contains(string(out), "\"model\"\r\n\r\nwhisper-1\r\n") {
		t.Errorf("new model id missing:\n%s", out)
	}
	if !strings.Contains(string(out), audio) {
		t.Errorf("file bytes were damaged by the rewrite:\n%s", out)
	}
	if !strings.Contains(string(out), "name=\"response_format\"") || !strings.Contains(string(out), "\r\n\r\njson\r\n") {
		t.Errorf("other form fields were damaged by the rewrite:\n%s", out)
	}
}

// TestReplaceModelMultipartWithoutModelField reports false rather than
// forwarding a stale id, the way the JSON path does.
func TestReplaceModelMultipartWithoutModelField(t *testing.T) {
	const boundary = "noboundary456"
	body := "--" + boundary + "\r\n" +
		`Content-Disposition: form-data; name="file"; filename="a.mp3"` + "\r\n\r\n" +
		"AAAA" + "\r\n" +
		"--" + boundary + "--\r\n"
	if _, ok := replaceModelCT([]byte(body), "multipart/form-data; boundary="+boundary, "whisper-1"); ok {
		t.Error("replaceModelCT rewrote a multipart body with no model field")
	}
}

func TestScheduleNext(t *testing.T) {
	layout := "2006-01-02 15:04:05"
	cases := []struct{ expr, from, want string }{
		{"0 0 * * *", "2026-09-30 13:45:12", "2026-10-01 00:00:00"},
		{"0 0 * * *", "2026-09-30 00:00:00", "2026-10-01 00:00:00"}, // strictly after
		{"*/15 * * * *", "2026-09-30 13:45:12", "2026-09-30 14:00:00"},
		{"0 */6 * * *", "2026-09-30 13:45:00", "2026-09-30 18:00:00"},
		{"30 8 * * 1", "2026-09-30 13:45:00", "2026-10-05 08:30:00"}, // next Monday
		{"@daily", "2026-09-30 13:45:00", "2026-10-01 00:00:00"},
		{"@midnight", "2026-09-30 13:45:00", "2026-10-01 00:00:00"},
		{"@hourly", "2026-09-30 13:45:00", "2026-09-30 14:00:00"},
		{"0 0 1 * *", "2026-09-30 13:45:00", "2026-10-01 00:00:00"},
		{"0 0 * jan *", "2026-09-30 13:45:00", "2027-01-01 00:00:00"},
		{"0,30 * * * *", "2026-09-30 13:31:00", "2026-09-30 14:00:00"},
		{"0 9-17/4 * * *", "2026-09-30 13:45:00", "2026-09-30 17:00:00"},
	}
	for _, tc := range cases {
		s, err := ParseSchedule(tc.expr)
		if err != nil {
			t.Fatalf("ParseSchedule(%q): %v", tc.expr, err)
		}
		from, err := time.ParseInLocation(layout, tc.from, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		expected, err := time.ParseInLocation(layout, tc.want, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		got := s.Next(from)
		if !got.Equal(expected) {
			t.Errorf("Next(%q from %s) = %s, want %s", tc.expr, tc.from,
				got.Format(layout), expected.Format(layout))
		}
	}
}

func TestScheduleNextMidnightTonight(t *testing.T) {
	s, err := ParseSchedule("0 0 * * *")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 9, 30, 13, 45, 12, 0, time.Local)
	got := s.Next(from)
	want := time.Date(2026, 9, 30, 0, 0, 0, 0, time.Local)
	if !got.After(want) || got.Sub(want) > 24*time.Hour {
		t.Errorf("Next = %s, want the coming midnight %s", got, want)
	}
	if got.Hour() != 0 || got.Minute() != 0 {
		t.Errorf("Next = %s, want 00:00", got)
	}
}

func TestScheduleEvery(t *testing.T) {
	s, err := ParseSchedule("@every 90s")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 9, 30, 13, 45, 12, 0, time.Local)
	got := s.Next(from)
	if d := got.Sub(from); d > 90*time.Second || d < 89*time.Second {
		t.Errorf("Next = %s, want ~90s later", got)
	}
}

func TestParseScheduleErrors(t *testing.T) {
	for _, bad := range []string{"", "0 0 * *", "0 0 * * * *", "60 0 * * *", "0 0 * * 9", "nope"} {
		if _, err := ParseSchedule(bad); err == nil {
			t.Errorf("ParseSchedule(%q) should have failed", bad)
		}
	}
}

func TestPoolRotationAndCooldown(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "ports.txt")
	if err := os.WriteFile(list, []byte("http://1.1.1.1:1\nhttp://2.2.2.2:2\nhttp://3.3.3.3:3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pool := NewPool(list, "", "rotate", true)
	if n, err := pool.Reload(); err != nil || n != 3 {
		t.Fatalf("Reload = %d, %v", n, err)
	}

	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		e, ok := pool.Take(pool.Sequence(), 0)
		if !ok {
			t.Fatal("no proxy")
		}
		if seen[e.URL] {
			t.Fatalf("rotate handed out %s twice", e.URL)
		}
		seen[e.URL] = true
	}

	// Retry attempts must walk to a different proxy.
	first, _ := pool.Take(pool.Sequence(), 0)
	second, _ := pool.Take(pool.Sequence(), 1)
	if first.URL == second.URL {
		t.Errorf("attempt 1 reused %s", first.URL)
	}

	// Benching hides an entry from the healthy rotation.
	for i := 0; i < maxProxyFails; i++ {
		pool.Failed(first, fmt.Errorf("boom"))
	}
	stats := pool.Stats()
	if stats.Benched != 1 {
		t.Errorf("Benched = %d, want 1", stats.Benched)
	}
	for i := 0; i < 5; i++ {
		e, _ := pool.Take(pool.Sequence(), 0)
		if e.URL == first.URL {
			t.Fatalf("benched proxy %s was handed out", e.URL)
		}
	}
	// A success clears the record.
	pool.Succeeded(first)
	if pool.Stats().Healthy != 3 {
		t.Errorf("Healthy = %d, want 3", pool.Stats().Healthy)
	}
}

func TestPoolSingleEntrySurvivesRetries(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "ports.txt")
	os.WriteFile(list, []byte("http://1.1.1.1:1\n"), 0o644)
	pool := NewPool(list, "", "rotate", true)
	pool.Reload()

	// A one-proxy pool must still be usable on every attempt, otherwise a
	// request silently degrades to a direct connection after the first failure.
	for attempt := 0; attempt < 5; attempt++ {
		entry, ok := pool.Take(pool.Sequence(), attempt)
		if !ok {
			t.Fatalf("attempt %d: pool ran out of proxies", attempt)
		}
		if entry.URL != "http://1.1.1.1:1" {
			t.Fatalf("attempt %d: got %q", attempt, entry.URL)
		}
	}
	// Same for a pool where everything is benched.
	entry, _ := pool.Take(pool.Sequence(), 0)
	for i := 0; i < maxProxyFails; i++ {
		pool.Failed(entry, fmt.Errorf("boom"))
	}
	for attempt := 0; attempt < 3; attempt++ {
		if _, ok := pool.Take(pool.Sequence(), attempt); !ok {
			t.Fatalf("attempt %d: benched pool ran dry", attempt)
		}
	}
}

func TestPoolReloadIfChanged(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "ports.txt")
	os.WriteFile(list, []byte("http://1.1.1.1:1\n"), 0o644)
	pool := NewPool(list, "", "rotate", true)
	pool.Reload()

	changed, err := pool.ReloadIfChanged()
	if err != nil || changed {
		t.Fatalf("ReloadIfChanged = %v, %v (file did not change)", changed, err)
	}
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(list, []byte("http://1.1.1.1:1\nhttp://2.2.2.2:2\n"), 0o644)
	changed, err = pool.ReloadIfChanged()
	if err != nil || !changed {
		t.Fatalf("ReloadIfChanged = %v, %v (file changed)", changed, err)
	}
	if pool.Len() != 2 {
		t.Errorf("Len = %d, want 2", pool.Len())
	}
}

func TestSaveAndLoadProxyList(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "ports.txt")
	entries := []ProxyEntry{
		{URL: "http://9.9.9.9:1", LatencyMS: 120, ExitIP: "1.1.1.1", CheckedAt: time.Now().UTC()},
		{URL: "socks5://9.9.9.9:2", LatencyMS: 300},
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
	if loaded[0].LatencyMS != 120 || loaded[0].ExitIP != "1.1.1.1" {
		t.Errorf("meta sidecar not merged: %+v", loaded[0])
	}
	if loaded[1].URL != "socks5://9.9.9.9:2" {
		t.Errorf("second entry = %q", loaded[1].URL)
	}
}

// ------------------------------------------------------- end to end test ---

// fakeProxy is a minimal HTTP proxy supporting absolute-form requests and
// CONNECT, so the router can be exercised without real network hops.
func fakeProxy(t *testing.T) (url string, hits *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// The counter is bumped on the accept goroutine and read by the test, so
	// it has to be atomic: under -race an unsynchronised int here is a
	// failure, not a flake.
	var counter atomic.Int64
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			counter.Add(1)
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				if req.Method == http.MethodConnect {
					upstream, err := net.Dial("tcp", req.Host)
					if err != nil {
						return
					}
					defer upstream.Close()
					io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
					go io.Copy(upstream, br)
					io.Copy(conn, upstream)
					return
				}
				// Absolute-form: the target host is in the URL.
				outreq, err := http.NewRequest(req.Method, req.URL.String(), req.Body)
				if err != nil {
					io.WriteString(conn, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
					return
				}
				outreq.Header = req.Header
				resp, err := http.DefaultTransport.RoundTrip(outreq)
				if err != nil {
					io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
					return
				}
				defer resp.Body.Close()
				conn.Write([]byte(fmt.Sprintf("HTTP/1.1 %d %s\r\n", resp.StatusCode, http.StatusText(resp.StatusCode))))
				for k, vs := range resp.Header {
					for _, v := range vs {
						fmt.Fprintf(conn, "%s: %s\r\n", k, v)
					}
				}
				fmt.Fprint(conn, "\r\n")
				io.Copy(conn, resp.Body)
			}()
		}
	}()
	return "http://" + ln.Addr().String(), &counter
}

func TestRouterForwardsThroughProxy(t *testing.T) {
	type received struct {
		path, model, auth, body string
	}
	got := make(chan received, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var probe struct {
			Model string `json:"model"`
		}
		json.Unmarshal(b, &probe)
		got <- received{path: r.URL.Path, model: probe.Model, auth: r.Header.Get("Authorization"), body: string(b)}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-1","choices":[]}`)
	}))
	defer upstream.Close()

	proxyURL, hits := fakeProxy(t)

	dir := t.TempDir()
	list := filepath.Join(dir, "ports.txt")
	if err := os.WriteFile(list, []byte(proxyURL+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Server:  ServerConfig{MaxBodyMB: 1, RequestSecs: 30},
		Proxy:   ProxyConfig{File: list, Mode: "rotate", MaxAttempts: 3},
		Auth:    AuthConfig{},
		Routing: RoutingConfig{},
		Provider: map[string]*Provider{
			"LLM Povider": {
				Options: ProviderOptions{BaseURL: upstream.URL + "/v1", APIKey: "sk-test", ReProxy: boolPtr(true)},
				Models: map[string]*Model{
					// the client asks for "llm/minimax-m3"; the provider
					// only knows the bare id.
					"minimax-m3": {Limit: ModelLimit{Context: 1000, Output: 100}},
				},
			},
		},
	}
	cfg.ApplyDefaults()

	pool := NewPool(cfg.Proxy.File, cfg.MetaPath(), cfg.Proxy.Mode, cfg.ProxyEnabled())
	if n, err := pool.Reload(); err != nil || n != 1 {
		t.Fatalf("pool reload: %d %v", n, err)
	}
	srv := NewServer(cfg, pool, newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// The client asks for "llm/minimax-m3"; the upstream should see "minimax-m3".
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"llm/minimax-m3","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	if p := resp.Header.Get("X-Noderouter-Proxy"); p != proxyURL {
		t.Errorf("X-Noderouter-Proxy = %q, want %q", p, proxyURL)
	}
	select {
	case r := <-got:
		if r.path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q, want /v1/chat/completions", r.path)
		}
		if r.model != "minimax-m3" {
			t.Errorf("upstream model = %q, want minimax-m3", r.model)
		}
		if r.auth != "Bearer sk-test" {
			t.Errorf("upstream auth = %q, want the provider key", r.auth)
		}
		if !strings.Contains(r.body, `"content":"hi"`) {
			t.Errorf("body was mangled: %s", r.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received the request")
	}
	if hits.Load() == 0 {
		t.Error("the request did not go through the proxy")
	}
	if n := srv.Proxied(); n != 1 {
		t.Errorf("proxied counter = %d, want 1", n)
	}
}

func TestRouterRetriesNextProxyOnFailure(t *testing.T) {
	// First proxy is a black hole, second one works.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // nothing is listening any more

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	goodProxy, _ := fakeProxy(t)

	dir := t.TempDir()
	list := filepath.Join(dir, "ports.txt")
	os.WriteFile(list, []byte(deadURL+"\n"+goodProxy+"\n"), 0o644)

	cfg := &Config{
		Server:   ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Proxy:    ProxyConfig{File: list, Mode: "rotate", MaxAttempts: 3},
		Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: upstream.URL, ReProxy: boolPtr(true)}, Models: map[string]*Model{}}},
	}
	cfg.ApplyDefaults()
	pool := NewPool(cfg.Proxy.File, cfg.MetaPath(), "rotate", true)
	pool.Reload()
	srv := NewServer(cfg, pool, newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// /v1/models is answered locally, so send something that must be forwarded.
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"anything","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"ok":true`) {
		t.Fatalf("status %d body %s: the router did not recover on the second proxy", resp.StatusCode, b)
	}
	if got := resp.Header.Get("X-Noderouter-Proxy"); got != goodProxy {
		t.Errorf("X-Noderouter-Proxy = %q, want the working proxy %q", got, goodProxy)
	}
	if n := srv.Retries(); n == 0 {
		t.Error("expected a retry to be counted")
	}
}

func TestRouterModelsEndpoint(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt")},
		Provider: map[string]*Provider{
			"LLM Povider": {Options: ProviderOptions{BaseURL: "http://x/v1"}, Models: map[string]*Model{
				"llm/minimax-m3": {Name: "LLM/Minimax M3", Limit: ModelLimit{Context: 1000000, Output: 128000},
					Modalities: Modalities{Input: []string{"text", "image"}, Output: []string{"text"}}},
			}},
		},
	}
	cfg.ApplyDefaults()
	pool := NewPool(cfg.Proxy.File, "", "rotate", true)
	srv := NewServer(cfg, pool, newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload struct {
		Object string `json:"object"`
		Data   []struct {
			ID        string   `json:"id"`
			OwnedBy   string   `json:"owned_by"`
			Context   int      `json:"context_length"`
			MaxOutput int      `json:"max_output_tokens"`
			Input     []string `json:"input_modalities"`
			Limit     struct {
				Context int `json:"context"`
				Output  int `json:"output"`
			} `json:"limit"`
			Modalities struct {
				Input []string `json:"input"`
			} `json:"modalities"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Object != "list" || len(payload.Data) != 1 {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	card := payload.Data[0]
	if card.ID != "llm/minimax-m3" || card.OwnedBy != "LLM Povider" {
		t.Errorf("unexpected model card: %+v", card)
	}
	if card.Context != 1000000 || card.MaxOutput != 128000 || card.Limit.Context != 1000000 {
		t.Errorf("limits not carried over: %+v", card)
	}
	if len(card.Input) != 2 || len(card.Modalities.Input) != 2 {
		t.Errorf("modalities not carried over: %+v", card)
	}
}

// TestModelsPageSplit checks the /models contract: a browser (Accept:
// text/html) gets the website shell, everyone else gets the JSON list,
// and the site's static assets are served from the embed.
func TestModelsPageSplit(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt")},
		Provider: map[string]*Provider{
			"P": {Options: ProviderOptions{BaseURL: "http://x/v1"}, Models: map[string]*Model{
				"llm/minimax-m3": {Name: "LLM/Minimax M3"},
			}},
		},
	}
	cfg.ApplyDefaults()
	pool := NewPool(cfg.Proxy.File, "", "rotate", true)
	srv := NewServer(cfg, pool, newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func(path, accept string) (*http.Response, string) {
		req, err := http.NewRequest("GET", ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	// A browser asking for the list gets the website shell.
	resp, body := get("/models", "text/html,application/xhtml+xml")
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("browser GET /models content-type = %q, want text/html", ct)
	}
	if !strings.Contains(body, `src="/js/models.js"`) {
		t.Error("browser GET /models did not return the website shell")
	}
	if !strings.Contains(body, `href="/css/site.css"`) {
		t.Error("browser GET /models shell did not link the stylesheet")
	}

	// The page's own fetch (Accept: application/json) and curl (Accept: */*)
	// get the JSON list.
	for _, accept := range []string{"application/json", "*/*", ""} {
		resp, body := get("/v1/models", accept)
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("GET /v1/models (Accept: %q) content-type = %q, want json", accept, ct)
		}
		if !strings.Contains(body, `"object":"list"`) {
			t.Errorf("GET /v1/models (Accept: %q) body = %s, want the JSON list", accept, body)
		}
	}

	// ?format= overrides the Accept header both ways.
	if _, body := get("/v1/models?format=html", "application/json"); !strings.Contains(body, `src="/js/models.js"`) {
		t.Error("?format=html did not return the website")
	}
	if _, body := get("/models?format=json", "text/html"); !strings.Contains(body, `"object":"list"`) {
		t.Error("?format=json did not return the JSON list")
	}

	// The assets the shell links to must exist and carry real types.
	for path, wantCT := range map[string]string{
		"/css/site.css":     "text/css",
		"/css/fonts.css":    "text/css",
		"/js/models.js":     "text/javascript",
		"/js/theme.js":      "text/javascript",
		"/js/marked.min.js": "text/javascript",
		"/fonts/PlantinNowVariable-Upright.woff2": "font/woff2",
		"/favicon/favicon.svg":                    "image/svg+xml",
		"/favicon/favicon-48x48.png":              "image/png",
		"/favicon/favicon-32x32.png":              "image/png",
		"/favicon/favicon-16x16.png":              "image/png",
		"/favicon/favicon-dark-48x48.png":         "image/png",
		"/favicon/favicon-dark-32x32.png":         "image/png",
		"/favicon/favicon-dark-16x16.png":         "image/png",
		"/favicon/apple-touch-icon.png":           "image/png",
		"/favicon.ico":                            "image/x-icon",
		"/img/og-image.png":                       "image/png",
		"/docs/configuration.md":                  "text/markdown",
		"/docs/proxies.md":                        "text/markdown",
		"/docs/operations.md":                     "text/markdown",
	} {
		resp, _ := get(path, "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d", path, resp.StatusCode)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, wantCT) {
			t.Errorf("GET %s: content-type %q, want %q", path, ct, wantCT)
		}
	}

	// The favicons must carry the mark, not be blank tiles. A PNG that is one
	// flat colour decodes fine and serves the right content-type, so the
	// browser shows a white square with no way to tell from the response
	// alone; only decoding the pixels catches it.
	//
	// inkPixels counts pixels the mark actually covers and transparent the
	// rest. An icon that is entirely one of the two is blank: no mark, or a
	// solid square. The apple touch icon is opaque by design (iOS composites
	// transparency over black), so it is exempt from the transparency half.
	inkPixels := func(img image.Image) (ink, transparent int) {
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				if _, _, _, a := img.At(x, y).RGBA(); a == 0 {
					transparent++
				} else {
					ink++
				}
			}
		}
		return ink, transparent
	}
	checkMark := func(label string, img image.Image, wantTransparent bool) {
		ink, transparent := inkPixels(img)
		if ink == 0 {
			t.Errorf("%s: no ink pixels, the icon would render blank", label)
		}
		if wantTransparent && transparent == 0 {
			t.Errorf("%s: no transparent pixels, the icon is a solid square", label)
		}
	}

	for _, path := range []string{
		"/favicon/favicon-16x16.png",
		"/favicon/favicon-32x32.png",
		"/favicon/favicon-48x48.png",
		"/favicon/favicon-dark-16x16.png",
		"/favicon/favicon-dark-32x32.png",
		"/favicon/favicon-dark-48x48.png",
		"/favicon/apple-touch-icon.png",
	} {
		resp, raw := get(path, "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d", path, resp.StatusCode)
			continue
		}
		img, err := png.Decode(strings.NewReader(raw))
		if err != nil {
			t.Errorf("GET %s: decode: %v", path, err)
			continue
		}
		checkMark(path, img, path != "/favicon/apple-touch-icon.png")
	}

	// favicon.ico is a container, not a bare PNG, so its frames get pulled
	// out by hand: a 6-byte header, then a 16-byte entry per frame giving
	// the size and where the image data starts. Each frame then decodes as
	// a PNG and has to carry the mark like the standalone icons do.
	_, icoRaw := get("/favicon.ico", "")
	if len(icoRaw) < 6 || icoRaw[2] != 1 {
		t.Fatalf("favicon.ico: bad header")
	}
	frames := int(icoRaw[4]) | int(icoRaw[5])<<8
	if frames == 0 {
		t.Fatal("favicon.ico: no frames")
	}
	for i := 0; i < frames; i++ {
		e := 6 + i*16
		if e+16 > len(icoRaw) {
			t.Fatalf("favicon.ico: frame %d runs past the end of the file", i)
		}
		// Width and height are separate bytes; 0 means 256.
		dim := int(icoRaw[e])
		if dim == 0 {
			dim = 256
		}
		if h := int(icoRaw[e+1]); h != dim {
			t.Errorf("favicon.ico frame %d: %dx%d, icons are square", i, dim, h)
		}
		size := int(icoRaw[e+8]) | int(icoRaw[e+9])<<8 | int(icoRaw[e+10])<<16 | int(icoRaw[e+11])<<24
		off := int(icoRaw[e+12]) | int(icoRaw[e+13])<<8 | int(icoRaw[e+14])<<16 | int(icoRaw[e+15])<<24
		if off < 0 || size < 0 || off+size > len(icoRaw) {
			t.Fatalf("favicon.ico: frame %d has an out-of-range offset/size", i)
		}
		frame, err := png.Decode(strings.NewReader(icoRaw[off : off+size]))
		if err != nil {
			t.Errorf("favicon.ico frame %d (%dx%d): decode: %v", i, dim, dim, err)
			continue
		}
		if got := frame.Bounds().Dx(); got != dim {
			t.Errorf("favicon.ico frame %d: directory says %dpx, frame is %dpx", i, dim, got)
		}
		checkMark(fmt.Sprintf("favicon.ico frame %d (%dx%d)", i, dim, dim), frame, true)
	}

	// Both pages carry the same icon set, so the docs site doesn't keep the
	// old bare-.ico links when the mark is updated here. The SVG is the one
	// that adapts to the color scheme, so every page has to link it.
	for _, p := range []struct{ path, accept string }{
		{"/models", "text/html,application/xhtml+xml"},
		{"/docs/configuration.md", "text/html,application/xhtml+xml"},
	} {
		resp, page := get(p.path, p.accept)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d", p.path, resp.StatusCode)
			continue
		}
		for _, want := range []string{
			`href="/favicon/favicon.svg"`,
			`href="/favicon/favicon-32x32.png"`,
			`href="/favicon/favicon-dark-32x32.png"`,
			`href="/favicon/apple-touch-icon.png"`,
		} {
			if !strings.Contains(page, want) {
				t.Errorf("GET %s: head does not link %s", p.path, want)
			}
		}
	}

	// The docs links in the page footer resolve to the embedded guides.
	resp, body = get("/docs/configuration.md", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /docs/configuration.md: status %d", resp.StatusCode)
	} else if !strings.Contains(body, "# ") {
		t.Error("GET /docs/configuration.md did not return the guide")
	}
	// Both representations share the URL, so caches must key on
	// Accept or a cached markdown fetch gets replayed to a browser
	// navigation (raw text until reload).
	if v := resp.Header.Get("Vary"); v != "Accept" {
		t.Errorf("GET /docs/configuration.md: Vary = %q, want Accept", v)
	}

	// A browser that navigated to a guide gets the docs website,
	// which fetches the guide back as markdown and renders it.
	resp, body = get("/docs/configuration.md", "text/html,application/xhtml+xml")
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("browser GET /docs/configuration.md content-type = %q, want text/html", ct)
	}
	if v := resp.Header.Get("Vary"); v != "Accept" {
		t.Errorf("browser GET /docs/configuration.md: Vary = %q, want Accept", v)
	}
	if !strings.Contains(body, `id="content"`) || !strings.Contains(body, "/js/marked.min.js") {
		t.Error("browser GET /docs/configuration.md did not return the docs website")
	}

	// ?format= overrides the Accept header both ways, as on /v1/models.
	if _, body = get("/docs/proxies.md?format=html", "application/json"); !strings.Contains(body, `id="content"`) {
		t.Error("?format=html did not return the docs website")
	}
	if resp, body = get("/docs/proxies.md?format=json", "text/html"); !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/markdown") || !strings.Contains(body, "# ") {
		t.Error("?format=json did not return the raw guide")
	}

	// A guide that is not there 404s for browsers and curl alike.
	for _, accept := range []string{"", "text/html,application/xhtml+xml"} {
		resp, _ = get("/docs/nope.md", accept)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET /docs/nope.md (Accept: %q): status %d, want 404", accept, resp.StatusCode)
		}
	}

	// A bare /docs has no guide of its own, so it lands on the front one
	// instead of 404ing a URL someone typed by hand.
	for _, path := range []string{"/docs", "/docs/"} {
		resp, body = get(path, "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d, want the index guide", path, resp.StatusCode)
			continue
		}
		if !strings.Contains(body, "# ") {
			t.Errorf("GET %s: body is not a guide", path)
		}
	}

	// Directory paths 404 instead of listing the embed.
	resp, _ = get("/fonts/", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /fonts/: status %d, want 404", resp.StatusCode)
	}
}

// TestStatusPageSplit is the /v1/status twin of TestModelsPageSplit: a browser
// navigating to the feed gets the status website shell, everyone else gets the
// JSON, and ?format= overrides the Accept header. The page's own fetch sends
// Accept: application/json, so that path must keep returning JSON, never the
// shell (which would recurse into fetching itself).
func TestStatusPageSplit(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt")},
		Provider: map[string]*Provider{
			"P": {Options: ProviderOptions{BaseURL: "http://x/v1"}, Models: map[string]*Model{
				"llm/minimax-m3": {Name: "LLM/Minimax M3"},
			}},
		},
	}
	cfg.ApplyDefaults()
	pool := NewPool(cfg.Proxy.File, "", "rotate", true)
	srv := NewServer(cfg, pool, newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func(path, accept string) (*http.Response, string) {
		req, err := http.NewRequest("GET", ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	// A browser navigating to the feed gets the status website shell.
	resp, body := get("/v1/status", "text/html,application/xhtml+xml")
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("browser GET /v1/status content-type = %q, want text/html", ct)
	}
	if !strings.Contains(body, `id="feed-error"`) {
		t.Error("browser GET /v1/status did not return the status website")
	}
	// Two representations on one URL: caches must key on Accept.
	if v := resp.Header.Get("Vary"); v != "Accept" {
		t.Errorf("GET /v1/status: Vary = %q, want Accept", v)
	}

	// The page's own fetch (Accept: application/json) and curl (Accept: */*)
	// get the JSON feed.
	for _, accept := range []string{"application/json", "*/*", ""} {
		resp, body := get("/v1/status", accept)
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("GET /v1/status (Accept: %q) content-type = %q, want json", accept, ct)
		}
		if !strings.Contains(body, `"requests"`) {
			t.Errorf("GET /v1/status (Accept: %q) body = %s, want the JSON snapshot", accept, body)
		}
	}

	// ?format= overrides the Accept header both ways.
	if _, body := get("/v1/status?format=html", "application/json"); !strings.Contains(body, `id="feed-error"`) {
		t.Error("?format=html did not return the status website")
	}
	if _, body := get("/v1/status?format=json", "text/html"); !strings.Contains(body, `"requests"`) {
		t.Error("?format=json did not return the JSON feed")
	}

	// The human page at /status is untouched: still the website shell, even
	// for a client that only asked for JSON.
	resp, body = get("/status", "application/json")
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET /status content-type = %q, want text/html", ct)
	}
	if !strings.Contains(body, `id="feed-error"`) {
		t.Error("GET /status did not return the status website")
	}
}

func TestRouterModelsEndpointNoDuplicateIDs(t *testing.T) {
	// Two providers exposing the same model id, and one of them renamed with
	// "as": /v1/models must not name the same model twice.
	var seenModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		seenModel = body.Model
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt")},
		Provider: map[string]*Provider{
			"Zen": {ID: "zen", Options: ProviderOptions{BaseURL: upstream.URL}, Models: map[string]*Model{
				"gpt-4o": {As: "zen/gpt-4o", Name: "Zen 4o"},
			}},
			"Other": {ID: "other", Options: ProviderOptions{BaseURL: upstream.URL}, Models: map[string]*Model{
				"gpt-4o": {Name: "Other 4o"},
			}},
		},
	}
	cfg.ApplyDefaults()
	pool := NewPool(cfg.Proxy.File, "", "rotate", false)
	srv := NewServer(cfg, pool, newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
			Name    string `json:"name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(payload.Data), payload.Data)
	}
	seen := map[string]string{}
	for _, card := range payload.Data {
		if prev, dup := seen[card.ID]; dup {
			t.Fatalf("id %q is listed twice (%s and %s)", card.ID, prev, card.OwnedBy)
		}
		seen[card.ID] = card.OwnedBy
	}
	// The explicit rename is advertised verbatim, and the model that kept the
	// bare key stays reachable under it - renaming one side of a clash is
	// enough to make the pair unique.
	if seen["zen/gpt-4o"] != "Zen" {
		t.Errorf(`"as" not advertised: %+v`, payload.Data)
	}
	if seen["gpt-4o"] != "Other" {
		t.Errorf("unrenamed model lost its id: %+v", payload.Data)
	}

	// Each advertised id routes to its own provider and keeps its config key
	// upstream.
	for id, want := range map[string]string{"zen/gpt-4o": "Zen", "gpt-4o": "Other"} {
		seenModel = ""
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"`+id+`","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("POST %q: status %d", id, resp.StatusCode)
		}
		if seenModel != "gpt-4o" {
			t.Errorf("POST %q sent upstream model %q, want the config key %q", id, seenModel, "gpt-4o")
		}
		p, _, err := cfg.ResolveModel(id)
		if err != nil {
			t.Errorf("advertised id %q does not resolve: %v", id, err)
		} else if got := providerName(cfg, p); got != want {
			t.Errorf("advertised id %q resolved to %q, want %q", id, got, want)
		}
	}

	// A single model endpoint answers for the advertised id, namespaced or not.
	for id, want := range map[string]string{"zen/gpt-4o": "Zen", "gpt-4o": "Other"} {
		one, err := http.Get(ts.URL + "/v1/models/" + id)
		if err != nil {
			t.Fatal(err)
		}
		var card struct {
			ID    string `json:"id"`
			Owned string `json:"owned_by"`
		}
		json.NewDecoder(one.Body).Decode(&card)
		one.Body.Close()
		if one.StatusCode != http.StatusOK {
			t.Errorf("GET /v1/models/%s: status %d", id, one.StatusCode)
			continue
		}
		if card.ID != id || card.Owned != want {
			t.Errorf("GET /v1/models/%s = %+v, want owned_by %q", id, card, want)
		}
	}
	if notes := cfg.ModelIDNotes(); len(notes) != 0 {
		t.Errorf("no id needed renaming, so no notes expected: %v", notes)
	}
}

func TestModelTableNamespacesUnrenamedClash(t *testing.T) {
	// Neither side renamed: both get the namespaced form, the bare key stays
	// ambiguous, and the error names the ids that do work.
	cfg := &Config{Provider: map[string]*Provider{
		"A": {ID: "a", Options: ProviderOptions{BaseURL: "http://a"}, Models: map[string]*Model{"dup": {}}},
		"B": {ID: "b", Options: ProviderOptions{BaseURL: "http://b"}, Models: map[string]*Model{"dup": {}}},
	}}
	cfg.ApplyDefaults()

	got := map[string]string{}
	for _, e := range cfg.ModelTable() {
		got[e.Provider.Name] = e.Call
	}
	if got["A"] != "a/dup" || got["B"] != "b/dup" {
		t.Fatalf("clashing ids not namespaced: %v", got)
	}
	for name, id := range map[string]string{"A": "a/dup", "B": "b/dup"} {
		p, upstream, err := cfg.ResolveModel(id)
		if err != nil {
			t.Fatalf("%q does not resolve: %v", id, err)
		}
		if providerName(cfg, p) != name || upstream != "dup" {
			t.Errorf("%q resolved to %q / %q, want %q / dup", id, providerName(cfg, p), upstream, name)
		}
	}
	_, _, err := cfg.ResolveModel("dup")
	if err == nil {
		t.Fatal("expected the bare shared key to stay ambiguous")
	}
	for _, want := range []string{"a/dup", "b/dup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguity error does not offer %q: %v", want, err)
		}
	}
	notes := cfg.ModelIDNotes()
	if len(notes) != 2 {
		t.Errorf("want a note per renamed model, got %v", notes)
	}
}

func TestModelTableKeepsUniqueIDs(t *testing.T) {
	// A config whose ids are already unique keeps them exactly as written.
	cfg := &Config{Provider: map[string]*Provider{
		"A": {Options: ProviderOptions{BaseURL: "http://a"}, Models: map[string]*Model{
			"one": {Upstream: "one-upstream"},
			"two": {As: "second", Upstream: "two-upstream"},
		}},
		"B": {Options: ProviderOptions{BaseURL: "http://b"}, Models: map[string]*Model{
			"three": {},
		}},
	}}
	cfg.ApplyDefaults()

	want := map[string]string{"one": "one-upstream", "second": "two-upstream", "three": "three"}
	for _, e := range cfg.ModelTable() {
		if e.Upstream != want[e.Call] {
			t.Errorf("Call %q upstream = %q, want %q", e.Call, e.Upstream, want[e.Call])
		}
		delete(want, e.Call)
	}
	if len(want) != 0 {
		t.Errorf("models missing from the table: %v", want)
	}
	if notes := cfg.ModelIDNotes(); len(notes) != 0 {
		t.Errorf("unexpected notes: %v", notes)
	}
	// A renamed model still resolves, and reaches the provider as its key.
	p, upstream, err := cfg.ResolveModel("second")
	if err != nil {
		t.Fatalf("renamed model does not resolve: %v", err)
	}
	if providerName(cfg, p) != "A" || upstream != "two-upstream" {
		t.Errorf("got %q / %q", providerName(cfg, p), upstream)
	}
}

func TestModelTableNamespacesEveryClash(t *testing.T) {
	// Two models asking for the same id by name cannot both have it, so both
	// are namespaced rather than one silently winning.
	cfg := &Config{Provider: map[string]*Provider{
		"A": {ID: "a", Options: ProviderOptions{BaseURL: "http://a"}, Models: map[string]*Model{
			"dup": {As: "same"},
		}},
		"B": {ID: "b", Options: ProviderOptions{BaseURL: "http://b"}, Models: map[string]*Model{
			"dup": {As: "same"},
		}},
	}}
	cfg.ApplyDefaults()

	got := map[string]string{}
	for _, e := range cfg.ModelTable() {
		got[e.Provider.Name] = e.Call
	}
	if got["A"] != "a/dup" || got["B"] != "b/dup" {
		t.Errorf("duplicate \"as\" not both namespaced: %v", got)
	}
	// Both namespaced ids resolve to their own provider.
	for name, id := range map[string]string{"A": "a/dup", "B": "b/dup"} {
		p, _, err := cfg.ResolveModel(id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if providerName(cfg, p) != name {
			t.Errorf("%q resolved to %q, want %q", id, providerName(cfg, p), name)
		}
	}
}

func TestRouterModelEndpointWithUpstreamName(t *testing.T) {
	// A model that renames its id upstream is still reachable under the key it
	// is configured with.
	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt")},
		Provider: map[string]*Provider{
			"P": {Options: ProviderOptions{BaseURL: "http://x/v1"}, Models: map[string]*Model{
				"minimax-m2.7": {Upstream: "minimax-m2.7", Name: "Minimax M2.7"},
			}},
		},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, id := range []string{"minimax-m2.7", "p/minimax-m2.7"} {
		resp, err := http.Get(ts.URL + "/v1/models/" + id)
		if err != nil {
			t.Fatal(err)
		}
		var card struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		json.NewDecoder(resp.Body).Decode(&card)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /v1/models/%s: status %d", id, resp.StatusCode)
			continue
		}
		if card.Name != "Minimax M2.7" {
			t.Errorf("GET /v1/models/%s = %+v", id, card)
		}
	}
}

func TestRouterRejectsUnknownModel(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt")},
		Provider: map[string]*Provider{
			"A": {Options: ProviderOptions{BaseURL: "http://a"}, Models: map[string]*Model{"x": {}}},
			"B": {Options: ProviderOptions{BaseURL: "http://b"}, Models: map[string]*Model{"y": {}}},
		},
	}
	cfg.ApplyDefaults()
	pool := NewPool(cfg.Proxy.File, "", "rotate", true)
	srv := NewServer(cfg, pool, newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"nope","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestRouterStreamingIsFlushed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: chunk %d\n\n", i)
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	cfg := &Config{
		Server:   ServerConfig{MaxBodyMB: 1, RequestSecs: 10},
		Proxy:    ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: upstream.URL, ReProxy: boolPtr(true)}, Models: map[string]*Model{}}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", true), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"x","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "chunk 2") {
		t.Errorf("stream was truncated: %q", body)
	}
}

// A self-signed (MITM-style) upstream is rejected by default and accepted only
// when the provider opts in, so the knob cannot be flipped by accident.
// Behind a front proxy the real client ip, the public host and the original
// scheme must reach the provider, and the chain must be appended to, not
// replaced.
// nginx caps request bodies at 1MB by default and read timeouts at 60s; the
// router must not be the thing that breaks a big body or a slow first token.
func TestRouterLargeBodyAndSlowUpstream(t *testing.T) {
	const size = 8 << 20 // 8MB, well past nginx's 1MB default

	var gotLen int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotLen = len(b)
		time.Sleep(1500 * time.Millisecond) // a slow first token
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 64, RequestSecs: 60},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: upstream.URL},
			Models: map[string]*Model{}}},
	}
	cfg.ApplyDefaults()
	ts := httptest.NewServer(NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false)).Handler())
	defer ts.Close()

	payload := fmt.Sprintf(`{"model":"x","messages":[{"role":"user","content":%q}]}`, strings.Repeat("a", size))
	start := time.Now()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if gotLen != len(payload) {
		t.Errorf("upstream received %d bytes, want %d (body was truncated)", gotLen, len(payload))
	}
	if elapsed := time.Since(start); elapsed < 1500*time.Millisecond {
		t.Errorf("answered in %s: the router did not wait for the slow upstream", elapsed)
	}
	// A body past the configured limit is refused with a clear error rather
	// than being silently cut.
	small := &Config{Server: ServerConfig{MaxBodyMB: 1, RequestSecs: 5}, Proxy: cfg.Proxy,
		Provider: cfg.Provider}
	small.ApplyDefaults()
	ts2 := httptest.NewServer(NewServer(small, NewPool(small.Proxy.File, "", "rotate", false), newLogger(false)).Handler())
	defer ts2.Close()
	big, err := http.Post(ts2.URL+"/v1/chat/completions", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer big.Body.Close()
	if big.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body status = %d, want 413", big.StatusCode)
	}
}

// TestTruncatedBodyIsNotReportedAsTooLarge checks the error the client gets
// matches what actually went wrong: a body the client never finished sending
// is a 400, not a 413 that blames its size.
func TestTruncatedBodyIsNotReportedAsTooLarge(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, MaxMediaBodyMB: 1, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: upstream.URL},
			Models: map[string]*Model{"m": {}}}},
	}
	cfg.ApplyDefaults()
	ts := httptest.NewServer(NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false)).Handler())
	defer ts.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Promise a megabyte, send a few bytes, then half-close. The server sees a
	// body that ends early - a read failure, not an oversized one. The Host
	// is one the router answers for (see server.allowedHosts); the test is
	// about the body, not the address.
	head := "POST /v1/chat/completions HTTP/1.1\r\nHost: 127.0.0.1\r\n" +
		"Content-Type: application/json\r\nContent-Length: 1048576\r\n\r\n{\"model\":\"m\""
	if _, err := io.WriteString(conn, head); err != nil {
		t.Fatal(err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("truncated body status = %d, want 400. body %s", resp.StatusCode, body)
	}
}

func TestForwardedHeadersBehindProxy(t *testing.T) {
	type headers struct {
		forChain, host, proto string
	}
	got := make(chan headers, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- headers{
			forChain: r.Header.Get("X-Forwarded-For"),
			host:     r.Header.Get("X-Forwarded-Host"),
			proto:    r.Header.Get("X-Forwarded-Proto"),
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, RequestSecs: 10},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: upstream.URL},
			Models: map[string]*Model{}}},
	}
	cfg.ApplyDefaults()
	ts := httptest.NewServer(NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false)).Handler())
	defer ts.Close()

	// Simulate nginx: it forwarded the client's chain and terminated TLS.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"x","messages":[]}`))
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	req.Header.Set("X-Forwarded-Host", "llm.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	h := <-got
	if !strings.HasPrefix(h.forChain, "203.0.113.9, 10.0.0.1, ") {
		t.Errorf("X-Forwarded-For = %q, want the client chain preserved and ours appended", h.forChain)
	}
	if h.host != "llm.example.com" {
		t.Errorf("X-Forwarded-Host = %q, want the public host from the front proxy", h.host)
	}
	if h.proto != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want https (the front proxy terminated tls)", h.proto)
	}
}

// With no front proxy the router still fills the headers from the connection.
func TestForwardedHeadersDirect(t *testing.T) {
	got := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("X-Forwarded-For")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, RequestSecs: 10},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: upstream.URL},
			Models: map[string]*Model{}}},
	}
	cfg.ApplyDefaults()
	ts := httptest.NewServer(NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false)).Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"x","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if chain := <-got; chain != "127.0.0.1" {
		t.Errorf("X-Forwarded-For = %q, want the peer address", chain)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	// Loopback and "localhost" are the only safe binds.
	for _, host := range []string{"127.0.0.1", "127.0.1.1", "localhost", "::1"} {
		if !isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = false, want true", host)
		}
	}
	// 0.0.0.0 and :: mean "every interface", which is exposed, not local.
	for _, host := range []string{"0.0.0.0", "::", "192.168.1.13", "10.0.0.5", "example.com", "", "0.0.0.1"} {
		if isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = true, want false", host)
		}
	}
}

// "re-proxy": false sends one provider direct while the pool still applies to
// everyone else; the default (unset) keeps going through the proxy.// The body budget admits small requests regardless of load, refuses a big
// body that would overflow the in-flight total with 503, allows a body that
// fills the budget exactly, and frees the slot on release.
func TestBodyBudget(t *testing.T) {
	b := NewBodyBudget(10)  // 10MB
	small := int64(1 << 20) // 1MB
	fill := int64(9 << 20)  // 9MB: 1+9 == 10, exactly full
	over := int64(10 << 20) // 10MB: 1+10 > 10, over

	if !b.Acquire(small) {
		t.Fatal("1MB body refused on an empty budget")
	}
	if b.Acquire(over) {
		t.Fatal("10MB body admitted on top of 1MB against a 10MB budget")
	}
	// Denied acquire must not have reserved anything.
	if _, inFlight, _ := b.Status(); inFlight != 1 {
		t.Fatalf("in-flight = %dMB after a refused acquire, want 1MB", inFlight)
	}
	// Exactly filling the budget is allowed.
	if !b.Acquire(fill) {
		t.Fatal("9MB body refused when it fills the budget exactly")
	}
	if _, inFlight, _ := b.Status(); inFlight != 10 {
		t.Fatalf("in-flight = %dMB, want 10", inFlight)
	}
	b.Release(small)
	b.Release(fill)
	if _, inFlight, _ := b.Status(); inFlight != 0 {
		t.Fatalf("in-flight = %dMB after both releases, want 0", inFlight)
	}
	// Zero size is a no-op that needs no release.
	if !b.Acquire(0) {
		t.Fatal("zero-size body refused")
	}
	// Disabled budget admits everything.
	off := NewBodyBudget(0)
	if !off.Acquire(1 << 30) {
		t.Fatal("disabled budget refused a request")
	}
}

// The budget is derived from the config: memoryLimitMb/3 with a 32MB floor,
// explicit value wins, negative disables.
func TestBodyBudgetDefaults(t *testing.T) {
	cfg := &Config{}
	cfg.ApplyDefaults()
	if cfg.Server.MemoryLimitMB != 100 || cfg.Server.BodyBudgetMB != 33 {
		t.Errorf("defaults = %d/%d, want 100/33", cfg.Server.MemoryLimitMB, cfg.Server.BodyBudgetMB)
	}
	cfg = &Config{Server: ServerConfig{MemoryLimitMB: 50}}
	cfg.ApplyDefaults()
	if cfg.Server.BodyBudgetMB != defaultBodyBudgetMinMB {
		t.Errorf("derived budget = %d, want the %dMB floor", cfg.Server.BodyBudgetMB, defaultBodyBudgetMinMB)
	}
	cfg = &Config{Server: ServerConfig{MemoryLimitMB: 300}}
	cfg.ApplyDefaults()
	if cfg.Server.BodyBudgetMB != 100 {
		t.Errorf("derived budget = %d, want 100 (300/3)", cfg.Server.BodyBudgetMB)
	}
	cfg = &Config{Server: ServerConfig{MemoryLimitMB: 50, BodyBudgetMB: -1}}
	cfg.ApplyDefaults()
	if cfg.Server.BodyBudgetMB != -1 {
		t.Errorf("explicit negative budget was rewritten to %d", cfg.Server.BodyBudgetMB)
	}
	if b := NewBodyBudget(-1); b.LimitBytes != 0 {
		t.Errorf("negative budget did not disable the backstop")
	}
}

// End to end: while a big body is in flight (held open by a gated upstream),
// a second big one gets 503 while a small one sails through; once the first
// completes, the budget is free again.
func TestRouterBodyBudgetEndToEnd(t *testing.T) {
	gate := make(chan struct{})
	var held int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		// Hold only the first request in flight; later ones answer at once.
		if atomic.CompareAndSwapInt32(&held, 0, 1) {
			<-gate
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 8, RequestSecs: 10, MemoryLimitMB: 100, BodyBudgetMB: 6},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: upstream.URL},
			Models: map[string]*Model{}}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	post := func(size int) int {
		payload := fmt.Sprintf(`{"model":"x","messages":[{"role":"user","content":"%s"}]}`, strings.Repeat("a", size))
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	// First 5MB body stays in flight, holding the budget.
	firstDone := make(chan int, 1)
	go func() { firstDone <- post(5 << 20) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, inFlight, _ := srv.BodiesStatus(); inFlight >= 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the 5MB body never registered against the budget")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if code := post(5 << 20); code != http.StatusServiceUnavailable {
		t.Errorf("second 5MB body: status %d, want 503 (budget saturated)", code)
	}
	if code := post(128); code != http.StatusOK {
		t.Errorf("small body while saturated: status %d, want 200 (small bodies always pass)", code)
	}

	close(gate) // release the first request
	if code := <-firstDone; code != http.StatusOK {
		t.Errorf("first 5MB body: status %d, want 200", code)
	}
	if code := post(5 << 20); code != http.StatusOK {
		t.Errorf("after the first request finished: status %d, want 200 (budget released)", code)
	}
}

// While a response is still streaming, the request body that produced it must
// not keep holding the budget: the buffered bytes cannot be replayed once the
// answer is on its way to the client, and a 10-minute completion would
// otherwise refuse unrelated uploads for its whole duration.
func TestRouterBodyBudgetFreedWhileStreaming(t *testing.T) {
	gate := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"ok\":true}\n\n")
		w.(http.Flusher).Flush()
		<-gate // hold the stream open while the budget is inspected
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 8, RequestSecs: 10, MemoryLimitMB: 100, BodyBudgetMB: 6},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: upstream.URL},
			Models: map[string]*Model{}}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	payload := fmt.Sprintf(`{"model":"x","stream":true,"messages":[{"role":"user","content":"%s"}]}`,
		strings.Repeat("a", 5<<20))
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	// The response headers have reached us, so the body behind them is long
	// past replayable: its 5MB reservation must already be back in the pool.
	if _, inFlight, _ := srv.BodiesStatus(); inFlight >= 5 {
		t.Errorf("in flight = %dMB while streaming, want 0 (budget released with the request body)", inFlight)
	}
	close(gate)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "[DONE]") {
		t.Errorf("stream truncated: %q", body)
	}
	if _, inFlight, _ := srv.BodiesStatus(); inFlight != 0 {
		t.Errorf("in flight = %dMB after completion, want 0 (released exactly once)", inFlight)
	}
}

// The client-side read timeout is derived from the larger body ceiling rather
// than configured: a second per megabyte, floor one minute.
func TestServerReadTimeout(t *testing.T) {
	cases := []struct {
		name   string
		server ServerConfig
		want   time.Duration
	}{
		{"defaults", ServerConfig{MaxBodyMB: 64, MaxMediaBodyMB: 256}, 256 * time.Second},
		{"floor for a tiny ceiling", ServerConfig{MaxBodyMB: 1, MaxMediaBodyMB: 1}, time.Minute},
		{"negative media ceiling falls back to maxBodyMb", ServerConfig{MaxBodyMB: 64, MaxMediaBodyMB: -1}, 64 * time.Second},
		{"uninitialised config still gets a floor", ServerConfig{}, time.Minute},
	}
	for _, tc := range cases {
		if got := tc.server.ReadTimeout(); got != tc.want {
			t.Errorf("%s: ReadTimeout() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRouterReProxyOptOut(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	proxyURL, hits := fakeProxy(t)
	dir := t.TempDir()
	list := filepath.Join(dir, "ports.txt")
	os.WriteFile(list, []byte(proxyURL+"\n"), 0o644)

	newServer := func(reProxy *bool) *httptest.Server {
		cfg := &Config{
			Server: ServerConfig{MaxBodyMB: 1, RequestSecs: 10},
			Proxy:  ProxyConfig{File: list, Mode: "first", MaxAttempts: 1},
			Provider: map[string]*Provider{"P": {
				Options: ProviderOptions{BaseURL: upstream.URL, ReProxy: reProxy},
				Models:  map[string]*Model{"m": {}},
			}},
		}
		cfg.ApplyDefaults()
		pool := NewPool(cfg.Proxy.File, cfg.MetaPath(), "first", true)
		pool.Reload()
		ts := httptest.NewServer(NewServer(cfg, pool, newLogger(false)).Handler())
		t.Cleanup(ts.Close)
		return ts
	}
	post := func(ts *httptest.Server) *http.Response {
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"m","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Omitted re-proxy: direct connection, no proxy header, no proxy hits.
	resp := post(newServer(nil))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("default re-proxy: status %d body %s", resp.StatusCode, body)
	}
	if p := resp.Header.Get("X-Noderouter-Proxy"); p != "" {
		t.Errorf("omitted re-proxy went through %q", p)
	}
	if hits.Load() != 0 {
		t.Error("omitted re-proxy went through the proxy")
	}

	// Explicit false is also direct.
	before := hits.Load()
	off := false
	resp2 := post(newServer(&off))
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("re-proxy=false: status %d", resp2.StatusCode)
	}
	if p := resp2.Header.Get("X-Noderouter-Proxy"); p != "" {
		t.Errorf("re-proxy=false still went through %q", p)
	}
	if got := hits.Load(); got != before {
		t.Errorf("re-proxy=false touched the proxy: hits %d -> %d", before, got)
	}

	// Explicit true behaves like the default.
	on := true
	resp3 := post(newServer(&on))
	resp3.Body.Close()
	if p := resp3.Header.Get("X-Noderouter-Proxy"); p != proxyURL {
		t.Errorf("re-proxy=true went to %q, want %q", p, proxyURL)
	}
}

func TestRouterNonV1BypassesProxy(t *testing.T) {
	var pathSeen string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathSeen = r.URL.Path
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	proxyURL, hits := fakeProxy(t)
	list := filepath.Join(t.TempDir(), "ports.txt")
	if err := os.WriteFile(list, []byte(proxyURL+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, RequestSecs: 10},
		Proxy:  ProxyConfig{File: list, Mode: "first", MaxAttempts: 1},
		Provider: map[string]*Provider{"P": {
			Options: ProviderOptions{BaseURL: upstream.URL, ReProxy: boolPtr(true)},
		}},
	}
	cfg.ApplyDefaults()
	pool := NewPool(cfg.Proxy.File, cfg.MetaPath(), "first", true)
	if _, err := pool.Reload(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(NewServer(cfg, pool, newLogger(false)).Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/random")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if pathSeen != "/random" {
		t.Fatalf("upstream path = %q, want /random", pathSeen)
	}
	if resp.Header.Get("X-Noderouter-Proxy") != "" {
		t.Error("non-/v1/ request used the proxy")
	}
	if hits.Load() != 0 {
		t.Errorf("non-/v1/ request touched proxy: %d hits", hits.Load())
	}
}

// apiKey accepts a literal, "file:<path>" (re-read per request) and "" for a
// keyless endpoint; a missing key file degrades to keyless rather than erroring.
func TestAPIKeyFromFileAndKeyless(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "zen.key")
	os.WriteFile(keyPath, []byte("  sk-from-file\n"), 0o644) // whitespace must be trimmed

	p := &Provider{Options: ProviderOptions{APIKey: "file:" + keyPath}}
	if got := p.APIKeyFor(0); got != "sk-from-file" {
		t.Errorf("file key = %q, want sk-from-file", got)
	}

	// Editing the file takes effect without a restart.
	os.WriteFile(keyPath, []byte("sk-rotated"), 0o644)
	if got := p.APIKeyFor(0); got != "sk-rotated" {
		t.Errorf("rotated key = %q, want sk-rotated", got)
	}

	// Missing file: no key, not an error.
	p2 := &Provider{Options: ProviderOptions{APIKey: "file:" + filepath.Join(dir, "gone.key")}}
	if got := p2.APIKeyFor(0); got != "" {
		t.Errorf("missing file key = %q, want empty", got)
	}

	// Literal keys are untouched, including a value that merely contains ":".
	p3 := &Provider{Options: ProviderOptions{APIKey: "sk-literal"}}
	if got := p3.APIKeyFor(0); got != "sk-literal" {
		t.Errorf("literal key = %q", got)
	}
	// apiKeys rotate with the file: prefix applied per key.
	p4 := &Provider{Options: ProviderOptions{APIKeys: keyList{"file:" + keyPath, "sk-two"}}}
	if got := p4.APIKeyFor(0); got != "sk-rotated" {
		t.Errorf("apiKeys[0] = %q, want the file key", got)
	}
	if got := p4.APIKeyFor(1); got != "sk-two" {
		t.Errorf("apiKeys[1] = %q, want sk-two", got)
	}
}

// A key file may hold a whole pool, one key per line: comments and blank lines
// are ignored, and every key in it joins the round robin.
func TestAPIKeyFilePool(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "keys.txt")
	os.WriteFile(keyPath, []byte("# pool\n  sk-91\n\nsk-92  \nsk-93 # spare\n"), 0o644)

	p := &Provider{Options: ProviderOptions{APIKey: "file:" + keyPath}}
	for i, want := range []string{"sk-91", "sk-92", "sk-93"} {
		if got := p.APIKeyFor(i); got != want {
			t.Errorf("apiKeyFor(%d) = %q, want %q", i, got, want)
		}
	}
	// The pool wraps rather than falling off the end.
	if got := p.APIKeyFor(3); got != "sk-91" {
		t.Errorf("apiKeyFor(3) = %q, want the pool to wrap to sk-91", got)
	}

	// One pool mixing a file with literal keys spreads across all of them.
	p2 := &Provider{Options: ProviderOptions{APIKeys: keyList{"file:" + keyPath, "sk-94"}}}
	want := []string{"sk-91", "sk-92", "sk-93", "sk-94"}
	for i, w := range want {
		if got := p2.APIKeyFor(i); got != w {
			t.Errorf("mixed pool apiKeyFor(%d) = %q, want %q", i, got, w)
		}
	}

	// An empty file leaves the provider keyless instead of erroring.
	empty := filepath.Join(dir, "empty.txt")
	os.WriteFile(empty, []byte("\n# nothing here\n"), 0o644)
	p3 := &Provider{Options: ProviderOptions{APIKey: "file:" + empty}}
	if got := p3.APIKeyFor(0); got != "" {
		t.Errorf("empty key file = %q, want empty", got)
	}

	// A pool whose only file is missing still serves the keys it does have.
	p4 := &Provider{Options: ProviderOptions{APIKeys: keyList{"file:" + filepath.Join(dir, "gone.txt"), "sk-95"}}}
	if got := p4.APIKeyFor(0); got != "sk-95" {
		t.Errorf("missing file in pool = %q, want sk-95", got)
	}
}

// apiKeys accepts an array, a whitespace-separated string and a multi-line
// string, and all three mean the same pool.
func TestAPIKeysAcceptedFormats(t *testing.T) {
	cases := []struct {
		name  string
		value string // the apiKeys value as it appears in models.json
	}{
		{"array", `["sk-91", "sk-92"]`},
		{"array padded", `["  sk-91  ", " sk-92 "]`},
		{"spaced string", `"  sk-91   sk-92  "`},
		{"newline string", `"sk-91\nsk-92\n"`},
		{"commented string", `"sk-91  # primary\n# spare\nsk-92\n"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "models.json")
			raw := `{"provider": {"P": {"options": {"baseURL": "http://x", "apiKeys": ` + tc.value + `}}}}`
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("config did not load: %v", err)
			}
			p := cfg.Provider["P"]
			if len(p.Options.APIKeys) != 2 {
				t.Fatalf("pool = %q, want two keys", p.Options.APIKeys)
			}
			if got := p.APIKeyFor(0); got != "sk-91" {
				t.Errorf("apiKeyFor(0) = %q, want sk-91", got)
			}
			if got := p.APIKeyFor(1); got != "sk-92" {
				t.Errorf("apiKeyFor(1) = %q, want sk-92", got)
			}
		})
	}

	// A single "file:" entry keeps its whole value, spaces and all, because a
	// path may contain them.
	dir := t.TempDir()
	path := filepath.Join(dir, "my keys.txt")
	os.WriteFile(path, []byte("sk-file\n"), 0o644)
	cfg := &Config{Provider: map[string]*Provider{"P": {Options: ProviderOptions{
		APIKeys: keyList{"file:" + path},
	}}}}
	cfg.ApplyDefaults()
	if len(cfg.Provider["P"].Options.APIKeys) != 1 {
		t.Fatalf("file entry was split: %q", cfg.Provider["P"].Options.APIKeys)
	}
	if got := cfg.Provider["P"].APIKeyFor(0); got != "sk-file" {
		t.Errorf("file entry = %q, want sk-file", got)
	}

	// A key file named in the pool, alongside a plain apiKey that is ignored.
	keyPath := filepath.Join(dir, "pool.txt")
	os.WriteFile(keyPath, []byte("sk-a\nsk-b\n"), 0o644)
	cfg2 := &Config{Provider: map[string]*Provider{"P": {Options: ProviderOptions{
		APIKey:  "sk-ignored",
		APIKeys: keyList{"file:" + keyPath},
	}}}}
	cfg2.ApplyDefaults()
	if got := cfg2.Provider["P"].APIKeyFor(0); got != "sk-a" {
		t.Errorf("apiKeys pool = %q, want sk-a (apiKey is ignored when a pool exists)", got)
	}

	// apiKey carrying several keys is treated as the pool it is.
	cfg3 := &Config{Provider: map[string]*Provider{"P": {Options: ProviderOptions{
		APIKey: "sk-x sk-y",
	}}}}
	cfg3.ApplyDefaults()
	if got := cfg3.Provider["P"].APIKeyFor(1); got != "sk-y" {
		t.Errorf("multi-key apiKey = %q, want sk-y", got)
	}
}

// A key pool picks its key the way the proxy pool picks its proxy: rotate,
// first or random. See proxy.mode for the same three names.
func TestAPIKeyPoolStrategy(t *testing.T) {
	pool := func(strategy string) *Provider {
		p := &Provider{Options: ProviderOptions{
			APIKey:      "sk-91 sk-92 sk-93",
			KeyStrategy: strategy,
		}}
		p.Options.APIKeys = normalizeKeys([]string{p.Options.APIKey})
		p.Options.APIKey = ""
		p.Options.KeyStrategy = normalizeKeyStrategy(strategy)
		return p
	}

	// rotate (the default) walks the pool one key per request and wraps.
	r := pool("")
	for i, want := range []string{"sk-91", "sk-92", "sk-93", "sk-91", "sk-92"} {
		if got := r.APIKeyFor(i); got != want {
			t.Errorf("rotate apiKeyFor(%d) = %q, want %q", i, got, want)
		}
	}

	// first always serves the head of the pool, whatever the request number.
	f := pool("first")
	for i := 0; i < 5; i++ {
		if got := f.APIKeyFor(i); got != "sk-91" {
			t.Errorf("first apiKeyFor(%d) = %q, want sk-91", i, got)
		}
	}

	// random stays inside the pool and reaches every key in a fair number of
	// draws.
	rnd := pool("random")
	seen := map[string]int{}
	const draws = 300
	for i := 0; i < draws; i++ {
		got := rnd.APIKeyFor(i)
		switch got {
		case "sk-91", "sk-92", "sk-93":
			seen[got]++
		default:
			t.Fatalf("random picked %q, which is not in the pool", got)
		}
	}
	if len(seen) != 3 {
		t.Errorf("random only reached %d of 3 keys in %d draws: %v", len(seen), draws, seen)
	}
	for key, n := range seen {
		if n < draws/6 { // far from the expected third, so this is not luck
			t.Errorf("random picked %s only %d times in %d draws", key, n, draws)
		}
	}

	// An unknown strategy falls back to rotate rather than failing the config,
	// exactly as proxy.mode does.
	for _, bad := range []string{"", "  ", "nonsense", "sequential"} {
		if got := normalizeKeyStrategy(bad); got != keyStrategyRotate {
			t.Errorf("normalizeKeyStrategy(%q) = %q, want rotate", bad, got)
		}
	}
	for _, ok := range []string{"rotate", " First ", "RANDOM"} {
		want := map[string]string{"rotate": keyStrategyRotate, " First ": keyStrategyFirst, "RANDOM": keyStrategyRandom}[ok]
		if got := normalizeKeyStrategy(ok); got != want {
			t.Errorf("normalizeKeyStrategy(%q) = %q, want %q", ok, got, want)
		}
	}

	// A single-key pool answers the same whatever the strategy.
	solo := &Provider{Options: ProviderOptions{APIKey: "sk-only", KeyStrategy: "random"}}
	for i := 0; i < 3; i++ {
		if got := solo.APIKeyFor(i); got != "sk-only" {
			t.Errorf("single-key pool = %q, want sk-only", got)
		}
	}
	// And a keyless provider stays keyless.
	keyless := &Provider{Options: ProviderOptions{KeyStrategy: "rotate"}}
	if got := keyless.APIKeyFor(0); got != "" {
		t.Errorf("keyless provider = %q, want empty", got)
	}
}

// A keyless provider takes no credential, so nothing credential-shaped may go
// upstream: not an empty Bearer header, not the client's own key, and not the
// router's auth.token. A stand-in key the client was made to send by its SDK
// would only be a key for the provider to reject on the router's behalf.
func TestRouterKeylessAuthorization(t *testing.T) {
	type hdr struct{ auth string }
	got := make(chan hdr, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- hdr{auth: r.Header.Get("Authorization")}
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	newServer := func(token string) string {
		cfg := &Config{
			Server:   ServerConfig{MaxBodyMB: 1, RequestSecs: 10},
			Auth:     AuthConfig{Token: token},
			Proxy:    ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
			Provider: map[string]*Provider{"Zen": {Options: ProviderOptions{BaseURL: upstream.URL, APIKey: ""}, Models: map[string]*Model{}}},
		}
		cfg.ApplyDefaults()
		srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)
		return ts.URL
	}
	post := func(url, auth string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions",
			strings.NewReader(`{"model":"x","messages":[]}`))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// No router auth: the client's key is dropped, not carried to a provider
	// that never asked for one.
	url := newServer("")
	resp := post(url, "Bearer client-key")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("keyless with a client key: status %d", resp.StatusCode)
	}
	if h := <-got; h.auth != "" {
		t.Errorf("keyless upstream saw Authorization = %q, want none", h.auth)
	}

	// A stand-in key is the case that used to fail: the router accepted it,
	// forwarded it, and the provider answered 401 in its own dialect.
	resp1 := post(url, "Bearer example")
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("stand-in key against a keyless provider: status %d", resp1.StatusCode)
	}
	if h := <-got; h.auth != "" {
		t.Errorf("stand-in key reached the keyless upstream as %q", h.auth)
	}

	// A client that sent nothing at all must not get an empty Bearer header.
	resp0 := post(url, "")
	resp0.Body.Close()
	if h := <-got; h.auth != "" {
		t.Errorf("keyless upstream saw Authorization = %q, want none", h.auth)
	}

	// With router auth, the router's own token must not leak upstream.
	url = newServer("router-secret")
	resp2 := post(url, "Bearer router-secret")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("router token: status %d", resp2.StatusCode)
	}
	if h := <-got; h.auth != "" {
		t.Errorf("router token leaked upstream as %q", h.auth)
	}
}

func TestRouterTLSVerificationOptIn(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	proxyURL, _ := fakeProxy(t)
	dir := t.TempDir()
	list := filepath.Join(dir, "ports.txt")
	os.WriteFile(list, []byte(proxyURL+"\n"), 0o644)

	newServer := func(skip bool) *httptest.Server {
		cfg := &Config{
			Server: ServerConfig{MaxBodyMB: 1, RequestSecs: 10},
			Proxy:  ProxyConfig{File: list, Mode: "first", MaxAttempts: 1},
			Provider: map[string]*Provider{"P": {
				Options: ProviderOptions{BaseURL: upstream.URL, SkipTLSVerify: skip},
				Models:  map[string]*Model{},
			}},
		}
		cfg.ApplyDefaults()
		pool := NewPool(cfg.Proxy.File, cfg.MetaPath(), "first", true)
		pool.Reload()
		ts := httptest.NewServer(NewServer(cfg, pool, newLogger(false)).Handler())
		t.Cleanup(ts.Close)
		return ts
	}

	post := func(ts *httptest.Server) (int, string) {
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"x","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, body := post(newServer(false)); code != http.StatusBadGateway {
		t.Errorf("with verification on: status %d body %s, want 502 (the self-signed cert must be rejected)", code, body)
	}
	if code, body := post(newServer(true)); code != http.StatusOK {
		t.Errorf("with insecureSkipVerify: status %d body %s, want 200", code, body)
	}
}

func TestRouterTokenAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		Server:   ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Auth:     AuthConfig{Token: "letmein"},
		Proxy:    ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: upstream.URL, ReProxy: boolPtr(true)}, Models: map[string]*Model{}}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	post := func(token string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"x","messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := post("wrong")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}

	resp2 := post("letmein")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp2.StatusCode)
	}
}

func TestRouterAdminToken(t *testing.T) {
	cfg := &Config{
		Server:   ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Auth:     AuthConfig{AdminToken: "secret"},
		Proxy:    ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt")},
		Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: "http://x"}, Models: map[string]*Model{}}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", true), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/noderouter/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/noderouter/status", nil)
	req.Header.Set("X-Noderouter-Token", "secret")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("authenticated status = %d, want 200", resp2.StatusCode)
	}
}

func TestRouterDynamicAdminToken(t *testing.T) {
	cfg := &Config{
		Server:   ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Auth:     AuthConfig{AdminToken: ""}, // No admin token configured
		Proxy:    ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt")},
		Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: "http://x"}, Models: map[string]*Model{}}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", true), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Unauthenticated request should fail
	resp, err := http.Get(ts.URL + "/noderouter/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d, want 401", resp.StatusCode)
	}

	// Request with the dynamic token should succeed
	dynamicToken := srv.DynamicAdminToken()
	if dynamicToken == "" {
		t.Fatal("dynamic admin token was not generated")
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/noderouter/status", nil)
	req.Header.Set("X-Noderouter-Token", dynamicToken)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("authenticated with dynamic token status = %d, want 200", resp2.StatusCode)
	}

	// Bearer token format should also work
	req3, _ := http.NewRequest(http.MethodGet, ts.URL+"/noderouter/status", nil)
	req3.Header.Set("Authorization", "Bearer "+dynamicToken)
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("authenticated with Bearer token status = %d, want 200", resp3.StatusCode)
	}
}

func TestSocks5Handshake(t *testing.T) {
	// A one-shot SOCKS5 server that records the request and replies 0x00.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- nil
			return
		}
		defer conn.Close()
		head := make([]byte, 3)
		if _, err := io.ReadFull(conn, head); err != nil {
			done <- nil
			return
		}
		conn.Write([]byte{0x05, 0x00}) // no auth
		req := make([]byte, 10)        // ver, cmd, rsv, atyp=1, 4 bytes ip, 2 bytes port
		io.ReadFull(conn, req)
		conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		done <- req
	}()

	proxyURL, _ := url.Parse("socks5://" + ln.Addr().String())
	client := newProxyClient(proxyURL, 5*time.Second, 1)
	resp, err := client.Get("http://example.invalid/test")
	if err == nil {
		resp.Body.Close()
	}
	req := <-done
	if req == nil {
		t.Skip("could not read the socks5 request")
	}
	if req[1] != 0x01 {
		t.Errorf("socks5 command = 0x%02x, want 0x01 (CONNECT)", req[1])
	}
	if req[3] != 0x01 && req[3] != 0x03 {
		t.Errorf("socks5 address type = %d, want ipv4 or domain", req[3])
	}
}

func TestCheckProxyDirect(t *testing.T) {
	// A local "what is my ip" endpoint reached through a fake proxy.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "203.0.113.7")
	}))
	defer target.Close()
	proxyURL, _ := fakeProxy(t)
	p, _ := url.Parse(proxyURL)
	client := newProxyClient(p, 5*time.Second, 1)
	resp, err := client.Get(target.URL)
	if err != nil {
		t.Skipf("fake proxy cannot reach the test server: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != "203.0.113.7" {
		t.Errorf("body = %q", body)
	}
}

func TestBareScheme(t *testing.T) {
	// "all" is a filter, not a scheme: bare ip:port lines must never come out
	// as all://1.2.3.4:8080, which no transport could dial.
	for _, protocol := range []string{"all", "", "any", "http", "socks5"} {
		if got := bareScheme(protocol); got != "http" && got != "socks5" {
			t.Errorf("bareScheme(%q) = %q, want a real scheme", protocol, got)
		}
	}
	if p, ok := NormalizeProxy("1.2.3.4:8080", bareScheme("all")); !ok || p != "http://1.2.3.4:8080" {
		t.Errorf("bare line under protocol=all gave %q,%v", p, ok)
	}
	if p, ok := NormalizeProxy("1.2.3.4:8080", bareScheme("socks5")); !ok || p != "socks5://1.2.3.4:8080" {
		t.Errorf("bare line under protocol=socks5 gave %q,%v", p, ok)
	}
}

func TestLoadProxiesFileBareLines(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "ports.txt")
	os.WriteFile(list, []byte("1.2.3.4:8080\nsocks5://5.6.7.8:1080\n"), 0o644)
	got, err := LoadProxiesFile(list, "all")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "http://1.2.3.4:8080" {
		t.Errorf("LoadProxiesFile(protocol=all) = %v", got)
	}
	for _, p := range got {
		if strings.HasPrefix(p, "all://") {
			t.Errorf("proxy %q has an undialable scheme", p)
		}
	}
}

func TestSourceEndpoint(t *testing.T) {
	cases := []struct {
		name string
		want string
		ok   bool
	}{
		{"iplocate", sourceURLs["iplocate"], true},
		{"IPLocate", sourceURLs["iplocate"], true},
		{" iplocate ", sourceURLs["iplocate"], true},
		{"https://example.com/list.txt", "https://example.com/list.txt", true},
		{"http://example.com/list.txt", "http://example.com/list.txt", true},
		{"proxyscrape", "", false},
		{"geonode", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := sourceEndpoint(tc.name)
		if ok != tc.ok || got != tc.want {
			t.Errorf("sourceEndpoint(%q) = %q,%v want %q,%v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
	if !strings.Contains(sourceURLs["iplocate"], "iplocate/free-proxy-list") {
		t.Errorf("iplocate preset points somewhere unexpected: %q", sourceURLs["iplocate"])
	}
}

func TestSchemeAllowed(t *testing.T) {
	cases := []struct {
		scheme, protocol string
		want             bool
	}{
		{"http", "http", true},
		{"https", "http", true}, // an https proxy is still an http proxy
		{"socks5", "http", false},
		{"http", "all", true},
		{"socks5", "all", true},
		{"socks4", "all", false}, // this build cannot dial socks4
		{"http", "", true},
		{"socks5", "socks5", true},
		{"http", "socks5", false},
	}
	for _, tc := range cases {
		if got := schemeAllowed(tc.scheme, tc.protocol); got != tc.want {
			t.Errorf("schemeAllowed(%q, %q) = %v, want %v", tc.scheme, tc.protocol, got, tc.want)
		}
	}
}

func TestFetchRawList(t *testing.T) {
	body := strings.Join([]string{
		"http://1.1.1.1:8080",
		"socks5://2.2.2.2:1080",
		"socks4://3.3.3.3:1080",
		"https://4.4.4.4:8443",
		"5.5.5.5:3128",
		"# a comment",
		"",
		"http://1.1.1.1:8080", // duplicate
		"garbage line",
	}, "\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer srv.Close()

	all, err := FetchRawList(context.Background(), srv.URL, "all")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"http://1.1.1.1:8080", "socks5://2.2.2.2:1080", "https://4.4.4.4:8443", "http://5.5.5.5:3128"}
	if len(all) != len(want) {
		t.Fatalf("all = %v, want %v", all, want)
	}
	for i := range want {
		if all[i] != want[i] {
			t.Errorf("all[%d] = %q, want %q", i, all[i], want[i])
		}
	}

	// protocol=http keeps only the http-family entries and drops socks4.
	httpOnly, err := FetchRawList(context.Background(), srv.URL, "http")
	if err != nil {
		t.Fatal(err)
	}
	wantHTTP := []string{"http://1.1.1.1:8080", "https://4.4.4.4:8443", "http://5.5.5.5:3128"}
	if len(httpOnly) != len(wantHTTP) {
		t.Fatalf("http = %v, want %v", httpOnly, wantHTTP)
	}
	for i := range wantHTTP {
		if httpOnly[i] != wantHTTP[i] {
			t.Errorf("http[%d] = %q, want %q", i, httpOnly[i], wantHTTP[i])
		}
	}
}

func TestFetchRawListErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/empty" {
			io.WriteString(w, "nothing useful here\n")
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := FetchRawList(context.Background(), srv.URL+"/empty", "all"); err == nil {
		t.Error("a list with no usable proxies should error")
	}
	if _, err := FetchRawList(context.Background(), srv.URL+"/missing", "all"); err == nil {
		t.Error("a 404 should error")
	}
}

// A refresh must merge every source into one deduplicated candidate list.
func TestHarvestMergesSources(t *testing.T) {
	shared := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a":
			io.WriteString(w, "http://1.1.1.1:8080\nhttp://2.2.2.2:8080\n")
		case "/b":
			io.WriteString(w, "http://2.2.2.2:8080\nsocks5://3.3.3.3:1080\n") // 2.2.2.2 is a duplicate
		case "/c":
			io.WriteString(w, "socks4://4.4.4.4:1080\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer shared.Close()

	dir := t.TempDir()
	local := filepath.Join(dir, "local.txt")
	// The file source keeps socks4 (it is a valid list line) so it exercises
	// the "parsed but not dialable" path.
	os.WriteFile(local, []byte("http://5.5.5.5:3128\nsocks4://6.6.6.6:1080\n"), 0o644)

	cfg := &Config{}
	cfg.Proxy.Sources = []string{shared.URL + "/a", shared.URL + "/b", "file", shared.URL + "/c", shared.URL + "/gone"}
	cfg.Proxy.File = local
	cfg.Proxy.Protocol = "all"
	cfg.Proxy.Test.Enabled = boolPtr(false)
	cfg.SetPath(filepath.Join(dir, "models.json"))
	cfg.ApplyDefaults()

	entries, res, err := Harvest(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 1.1.1.1, 2.2.2.2, 3.3.3.3 (socks5) and 5.5.5.5 survive; 2.2.2.2 was listed
	// twice, socks4 is not dialable (skipped in the raw list and in the file),
	// and /gone failed without aborting the run.
	if len(entries) != 4 {
		t.Errorf("entries = %v, want 4 deduped dialable proxies", entries)
	}
	if res.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (the socks4 entry from the file source)", res.Skipped)
	}
	if len(res.Sources) != 3 {
		t.Errorf("Sources = %v, want 3 (only the successful sources)", res.Sources)
	}
}

func TestHarvestFromFileSource(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.txt")
	os.WriteFile(in, []byte("# list\n1.2.3.4:8080\nhttp://5.6.7.8:3128\n1.2.3.4:8080\nbogus\n"), 0o644)
	cfg := &Config{}
	cfg.Proxy.Sources = []string{"file"}
	cfg.Proxy.File = in
	cfg.Proxy.Protocol = "http"
	cfg.Proxy.Test.Enabled = boolPtr(false)
	cfg.SetPath(filepath.Join(dir, "models.json"))
	cfg.ApplyDefaults()

	entries, res, err := Harvest(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Fetched != 2 || len(entries) != 2 {
		t.Fatalf("Fetched=%d entries=%d, want 2 and 2", res.Fetched, len(entries))
	}
}

func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	raw := `{
      "server": { "port": 1234 },
      "proxy": { "refresh": "*/30 * * * *", "sources": ["geonode"] },
      "provider": {
        "LLM Povider": {
          "options": { "baseURL": "http://example.com/v1", "apiKey": "sk-1" },
          "models": { "llm/minimax-m2.7": { "name": "x", "limit": { "context": 5, "output": 6 } } }
        }
      }
    }`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != 1234 || cfg.Server.Host != defaultListenHost {
		t.Errorf("defaults not applied: %+v", cfg.Server)
	}
	if cfg.Proxy.Refresh != "*/30 * * * *" || cfg.Proxy.File != expandPath(defaultPortsPath) {
		t.Errorf("proxy defaults wrong: %+v", cfg.Proxy)
	}
	if len(cfg.Provider["LLM Povider"].Models) != 1 {
		t.Errorf("models not parsed")
	}
	if got := cfg.Provider["LLM Povider"].Models["llm/minimax-m2.7"].Provider; got != "LLM Povider" {
		t.Errorf("model provider backfill = %q", got)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Errorf("saved config does not reload: %v", err)
	}
}
