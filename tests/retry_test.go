package tests

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryDefaults(t *testing.T) {
	// Nothing configured: ten tries, with a wait that grows and then stops.
	c := &Config{}
	c.ApplyDefaults()
	if c.Retry.MaxAttempts != defaultRetryAttempts {
		t.Errorf("MaxAttempts = %d, want %d", c.Retry.MaxAttempts, defaultRetryAttempts)
	}
	if c.Retry.BackoffMS != defaultRetryBackoffMS || c.Retry.MaxBackoffMS != defaultRetryMaxBackoffMS {
		t.Errorf("backoff = %d/%d ms, want %d/%d",
			c.Retry.BackoffMS, c.Retry.MaxBackoffMS, defaultRetryBackoffMS, defaultRetryMaxBackoffMS)
	}

	// proxy.maxAttempts was the budget before retry.maxAttempts existed, so
	// a config that sets it must keep the number it asked for.
	old := &Config{Proxy: ProxyConfig{MaxAttempts: 3}}
	old.ApplyDefaults()
	if old.Retry.MaxAttempts != 3 || old.Proxy.MaxAttempts != 3 {
		t.Errorf("proxy.maxAttempts=3 gave retry %d (proxy %d), want 3 and 3",
			old.Retry.MaxAttempts, old.Proxy.MaxAttempts)
	}

	// An explicit retry block wins over the older field.
	both := &Config{Proxy: ProxyConfig{MaxAttempts: 3}, Retry: RetryConfig{MaxAttempts: 7}}
	both.ApplyDefaults()
	if both.Retry.MaxAttempts != 7 {
		t.Errorf("MaxAttempts = %d, want the explicit 7", both.Retry.MaxAttempts)
	}

	// 1 means "never retry", and must survive the defaulting.
	off := &Config{Retry: RetryConfig{MaxAttempts: 1}}
	off.ApplyDefaults()
	if off.Retry.MaxAttempts != 1 {
		t.Errorf("MaxAttempts = %d, want 1 to mean no retries", off.Retry.MaxAttempts)
	}

	// A negative backoff turns the wait off; a backoff above the cap is
	// pulled down to it.
	quiet := &Config{Retry: RetryConfig{BackoffMS: -1, MaxBackoffMS: 200, TotalSecs: -1}}
	quiet.ApplyDefaults()
	if quiet.Retry.BackoffMS != 0 {
		t.Errorf("BackoffMS = %d, want 0 (negative disables)", quiet.Retry.BackoffMS)
	}
	if quiet.Retry.TotalSecs != 0 {
		t.Errorf("TotalSecs = %d, want 0 (negative disables)", quiet.Retry.TotalSecs)
	}
	over := &Config{Retry: RetryConfig{BackoffMS: 5000, MaxBackoffMS: 1000}}
	over.ApplyDefaults()
	if over.Retry.BackoffMS != 1000 {
		t.Errorf("BackoffMS = %d, want it capped at 1000", over.Retry.BackoffMS)
	}
}

func TestRetryConfigFromJSON(t *testing.T) {
	raw := `{"retry":{"maxAttempts":10,"backoffMs":250,"maxBackoffMs":3000,
	          "statuses":[401],"skipStatuses":[529],"totalSecs":60}}`
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	c.ApplyDefaults()
	if c.Retry.MaxAttempts != 10 || c.Retry.BackoffMS != 250 || c.Retry.MaxBackoffMS != 3000 {
		t.Errorf("retry block not read: %+v", c.Retry)
	}
	if c.Retry.TotalSecs != 60 {
		t.Errorf("TotalSecs = %d, want 60", c.Retry.TotalSecs)
	}
	if !c.Retry.RetryableStatus(401) {
		t.Error("401 should be retryable once it is listed")
	}
	if c.Retry.RetryableStatus(529) {
		t.Error("529 should be skipped once it is listed as skipped")
	}
}

func TestRetryableStatus(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		{http.StatusOK, false},
		{http.StatusBadRequest, false},          // malformed: the same answer forever
		{http.StatusUnauthorized, false},        // rejected key: see the key-rotation test
		{http.StatusForbidden, false},           //
		{http.StatusNotFound, false},            // no such model
		{http.StatusUnprocessableEntity, false}, //
		{http.StatusRequestTimeout, true},       //
		{http.StatusTooEarly, true},             //
		{http.StatusTooManyRequests, true},      //
		{http.StatusBadGateway, true},           //
		{http.StatusServiceUnavailable, true},   //
		{http.StatusGatewayTimeout, true},       //
		{522, true},                             //
		{524, true},                             //
		{529, true},                             //
	}
	retry := &RetryConfig{}
	for _, tc := range cases {
		if got := retry.RetryableStatus(tc.code); got != tc.want {
			t.Errorf("retryableStatus(%d) = %v, want %v", tc.code, got, tc.want)
		}
	}

	// Lists are additions and removals on top of the defaults.
	added := &RetryConfig{Statuses: []int{401, 403}}
	if !added.RetryableStatus(401) || !added.RetryableStatus(429) {
		t.Error("an added status should retry, and must not drop the defaults")
	}
	removed := &RetryConfig{SkipStatuses: []int{429, 500}}
	if removed.RetryableStatus(429) || removed.RetryableStatus(500) {
		t.Error("a skipped status should not retry")
	}
	if !removed.RetryableStatus(503) {
		t.Error("skipping one status must not skip the rest")
	}
	// A status in both lists loses: asking not to retry is the safer reading.
	both := &RetryConfig{Statuses: []int{402}, SkipStatuses: []int{402}}
	if both.RetryableStatus(402) {
		t.Error("skipStatuses should win over statuses")
	}
}

func TestRetryBackoffGrowsAndCaps(t *testing.T) {
	retry := &RetryConfig{BackoffMS: 100, MaxBackoffMS: 800}
	// Half-jitter: every wait sits in [half, full], so the growth is visible
	// without being exact.
	for attempt, want := range map[int]time.Duration{
		0: 100 * time.Millisecond,
		1: 200 * time.Millisecond,
		2: 400 * time.Millisecond,
		3: 800 * time.Millisecond,
		9: 800 * time.Millisecond, // capped, and no overflow at attempt 62+
	} {
		for i := 0; i < 20; i++ {
			got := retry.Backoff(attempt, nil)
			if got < want/2 || got > want {
				t.Fatalf("attempt %d: backoff = %s, want between %s and %s", attempt, got, want/2, want)
			}
		}
	}
	// A big attempt count must not wrap the shift into a negative wait.
	if got := retry.Backoff(200, nil); got > 800*time.Millisecond {
		t.Errorf("backoff at attempt 200 = %s, want it capped rather than wrapped", got)
	}
	// No ceiling means no waiting at all, whatever the attempt number.
	if got := (&RetryConfig{MaxBackoffMS: 0, BackoffMS: 100}).Backoff(3, nil); got != 0 {
		t.Errorf("backoff with maxBackoffMs=0 = %s, want 0", got)
	}
	// And a negative backoff turns the wait off without touching the cap.
	if got := (&RetryConfig{MaxBackoffMS: 8000, BackoffMS: -1}).Backoff(3, nil); got != 0 {
		t.Errorf("backoff with backoffMs=-1 = %s, want 0", got)
	}
}

func TestRetryBackoffHonoursRetryAfter(t *testing.T) {
	retry := &RetryConfig{BackoffMS: 100, MaxBackoffMS: 10_000}
	if got := retry.Backoff(0, http.Header{"Retry-After": []string{"2"}}); got != 2*time.Second {
		t.Errorf("Retry-After: 2 gave %s, want 2s", got)
	}
	// Longer than the ceiling: clamped, so one header cannot pin a request
	// open for an hour.
	if got := retry.Backoff(0, http.Header{"Retry-After": []string{"600"}}); got != 10*time.Second {
		t.Errorf("Retry-After: 600 gave %s, want the 10s cap", got)
	}
	// The date form works too, and a past date means "now".
	now := time.Now()
	soon := now.Add(3 * time.Second).UTC().Format(http.TimeFormat)
	if got := retry.Backoff(0, http.Header{"Retry-After": []string{soon}}); got < 2*time.Second || got > 3*time.Second {
		t.Errorf("Retry-After date gave %s, want ~3s", got)
	}
	past := now.Add(-time.Minute).UTC().Format(http.TimeFormat)
	if got := retry.Backoff(0, http.Header{"Retry-After": []string{past}}); got != 0 {
		t.Errorf("Retry-After in the past gave %s, want 0", got)
	}
	// Nonsense is no opinion, so the normal backoff applies.
	if got := retry.Backoff(0, http.Header{"Retry-After": []string{"soon"}}); got == 0 {
		t.Error("an unparseable Retry-After should fall back to the backoff")
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, false},
		{"5", 5 * time.Second, true},
		{"  12  ", 12 * time.Second, true},
		{"0", 0, true},
		{"-3", 0, true},
		{"nonsense", 0, false},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		{now.Add(-90 * time.Second).Format(http.TimeFormat), 0, true},
	}
	for _, tc := range cases {
		got, ok := parseRetryAfter(tc.in, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseRetryAfter(%q) = %s,%v want %s,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSleepCtxStopsWhenClientLeaves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if sleepCtx(ctx, time.Minute) {
		t.Error("sleepCtx reported a full wait after the context was cancelled")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("sleepCtx took %s, want it to return as soon as the client went away", elapsed)
	}
	if !sleepCtx(context.Background(), 0) {
		t.Error("a zero wait should report success")
	}
}

// retryTestServer wires a router in front of an upstream that answers with
// whatever handler says, with the proxy pool off so only the retry path is
// under test.
func retryTestServer(t *testing.T, retry RetryConfig, options ProviderOptions, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)

	options.BaseURL = upstream.URL
	cfg := &Config{
		Server:   ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Proxy:    ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Retry:    retry,
		Provider: map[string]*Provider{"P": {Options: options, Models: map[string]*Model{"m": {}}}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(func() { t.Logf("retries counted: %d", srv.Retries()) })
	return ts
}

// postForward sends one request the router has to forward upstream.
func postForward(t *testing.T, ts *httptest.Server) (int, string) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// noWait is the retry config tests use when they only care about the number
// of tries: backoffMs -1 means retry immediately.
var noWait = RetryConfig{MaxAttempts: 10, BackoffMS: -1}

func TestRouterRetriesProviderErrorThenSucceeds(t *testing.T) {
	var hits atomic.Int64
	ts := retryTestServer(t, noWait, ProviderOptions{}, func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":{"message":"upstream is having a moment"}}`)
			return
		}
		io.WriteString(w, `{"ok":true}`)
	})

	code, body := postForward(t, ts)
	if code != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("status %d body %s, want the third attempt's 200", code, body)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("upstream saw %d requests, want 3", got)
	}
}

func TestRouterGivesUpAfterTenTriesAndReturnsProviderError(t *testing.T) {
	var hits atomic.Int64
	ts := retryTestServer(t, noWait, ProviderOptions{}, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":{"message":"model overloaded"}}`)
	})

	code, body := postForward(t, ts)
	// The provider's own error is the answer, not a synthesised 502: the
	// client can see what actually went wrong.
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want the provider's 503", code)
	}
	if !strings.Contains(body, "model overloaded") {
		t.Errorf("body = %s, want the provider's error text", body)
	}
	if got := hits.Load(); got != 10 {
		t.Errorf("upstream saw %d requests, want exactly 10", got)
	}
}

func TestRouterDoesNotRetryPermanentError(t *testing.T) {
	var hits atomic.Int64
	ts := retryTestServer(t, noWait, ProviderOptions{}, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"unknown model"}}`)
	})

	code, body := postForward(t, ts)
	if code != http.StatusBadRequest || !strings.Contains(body, "unknown model") {
		t.Fatalf("status %d body %s, want the 400 passed straight through", code, body)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("upstream saw %d requests, want 1: a 400 repeats the same answer", got)
	}
}

func TestRouterRetryRotatesApiKey(t *testing.T) {
	// A two-key pool and 401 listed as retryable: the retry has to land on the
	// other key, or the whole budget is spent on a key already refused.
	retry := noWait
	retry.Statuses = []int{401}
	var mu sync.Mutex
	var keys []string
	ts := retryTestServer(t, retry, ProviderOptions{
		APIKeys:     keyList{"sk-first", "sk-second"},
		KeyStrategy: keyStrategyRotate,
	}, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Authorization"))
		attempt := len(keys)
		mu.Unlock()
		if attempt == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"key spent"}}`)
			return
		}
		io.WriteString(w, `{"ok":true}`)
	})

	code, body := postForward(t, ts)
	if code != http.StatusOK {
		t.Fatalf("status %d body %s, want the second key to succeed", code, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(keys))
	}
	if keys[0] == keys[1] {
		t.Errorf("both attempts sent %s, want the retry to rotate to the next key", keys[0])
	}
	if keys[0] != "Bearer sk-first" && keys[0] != "Bearer sk-second" {
		t.Errorf("first attempt sent %q, want a key from the pool", keys[0])
	}
}

// totalSecsRouter wires a router whose retry sequence is bounded by totalSecs,
// in front of the given upstream.
func totalSecsRouter(t *testing.T, totalSecs int, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	cfg := &Config{
		Server:   ServerConfig{MaxBodyMB: 1, RequestSecs: 30},
		Proxy:    ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Retry:    RetryConfig{MaxAttempts: 10, TotalSecs: totalSecs},
		Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: upstream.URL}, Models: map[string]*Model{"m": {}}}},
	}
	cfg.ApplyDefaults()
	ts := httptest.NewServer(NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false)).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestRetryTotalSecsHandsBackTheProviderError(t *testing.T) {
	// Cut short while waiting: the answer in hand is the provider's, so that
	// is what the client gets - not a 502 the router made up.
	//
	// The shape of this is load-proof, not just usually right: the budget has
	// to expire during the backoff wait, never during attempt 0. Attempt 0 is
	// a local httptest upstream, but the full suite runs proxy's 50-worker
	// tests in parallel with it, so the attempt gets a 3s budget (plenty for
	// an instant answer, even under contention) while Retry-After: 30 asks for
	// a 30s wait that is clamped to maxBackoffMs (8s) - the wait outlasts the
	// budget by seconds either way, so there is no timing window in which the
	// deadline lands mid-attempt and turns the provider's 429 into a 502.
	var hits atomic.Int64
	ts := totalSecsRouter(t, 3, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"slow down"}}`)
	})

	start := time.Now()
	code, body := postForward(t, ts)
	elapsed := time.Since(start)
	if code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want the provider's 429", code)
	}
	if !strings.Contains(body, "slow down") {
		t.Errorf("body = %q, want the provider's error passed through", body)
	}
	if elapsed > 10*time.Second {
		t.Errorf("sequence took %s, want it bounded by totalSecs=3", elapsed)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("upstream saw %d requests, want 1: the budget ran out during the wait", got)
	}
}

func TestRetryTotalSecsBoundsAnAttemptInFlight(t *testing.T) {
	// Cut short mid-attempt: there is no answer in hand, so the router says
	// so. The point is that ten slow tries do not become a multi-minute hang.
	var hits atomic.Int64
	ts := totalSecsRouter(t, 1, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// Each try outlasts its share of the budget, so the deadline lands
		// on an attempt in flight rather than between two of them.
		time.Sleep(400 * time.Millisecond)
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	start := time.Now()
	code, body := postForward(t, ts)
	elapsed := time.Since(start)
	if code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 when an attempt is cut off with no answer", code)
	}
	if !strings.Contains(body, "deadline exceeded") {
		t.Errorf("body = %q, want it to name the deadline", body)
	}
	if elapsed > 10*time.Second {
		t.Errorf("sequence took %s, want it bounded by totalSecs=1", elapsed)
	}
	if got := hits.Load(); got < 1 || got > 10 {
		t.Errorf("upstream saw %d requests, want between 1 and 10", got)
	}
}

func TestRouterRetryStopsWhenClientDisconnects(t *testing.T) {
	var hits atomic.Int64
	// Retry-After 30s: without a disconnect the router would sit on that wait
	// instead of trying again.
	ts := retryTestServer(t, RetryConfig{MaxAttempts: 10, MaxBackoffMS: 30000}, ProviderOptions{},
		func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("request took %s after the client left, want the router to stop", elapsed)
	}
	if got := hits.Load(); got > 2 {
		t.Errorf("upstream saw %d requests, want the retry loop to stop at the disconnect", got)
	}
}
