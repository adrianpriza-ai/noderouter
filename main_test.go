package main

// The -v flag is the only thing that separates a quiet router from a loud one,
// and it is applied in exactly one place: newLogger, called with the flag from
// runServe and runConvert. Testing the flag end to end would mean booting a
// server; testing the seam it is wired through keeps it honest for a fraction
// of the cost.

import (
	"bytes"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"noderouter/config"
	"noderouter/logx"
	"noderouter/proxy"
	"noderouter/router"
)

// capture swaps os.Stderr for a pipe and returns a function giving back
// everything written to it. newLogger binds the handler to os.Stderr as it is
// called, so the swap has to be in place first.
func capture(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = buf.ReadFrom(r)
	}()

	var finalized bool
	return func() string {
		if finalized {
			return buf.String()
		}
		finalized = true
		os.Stderr = orig
		_ = w.Close()
		<-done
		_ = r.Close()
		return buf.String()
	}
}

// `-v` must open the debug level, and running without it must not. If this
// breaks, either the site runs silent with -v or it drowns in per-request
// detail by default - both of which are reported as "the router says nothing".
func TestVerboseFlagRaisesLogLevel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		verbose  bool
		wantLogs bool
	}{
		{"serve -v", true, true},
		{"serve without -v", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finish := capture(t)
			log := newLogger(tc.verbose)
			log.Debug("per-request detail")
			got := finish()
			if (got != "") != tc.wantLogs {
				t.Errorf("newLogger(%v): debug line produced %q, want output: %v", tc.verbose, got, tc.wantLogs)
			}
		})
	}
}

// -v is additive, never subtractive: warnings are how an operator learns the
// router is exposed or a proxy is intercepting TLS, so they must survive it.
func TestVerboseKeepsWarnings(t *testing.T) {
	finish := capture(t)
	newLogger(true).Warn("listening on 0.0.0.0 with no auth.token set")
	out := finish()
	if !bytes.Contains([]byte(out), []byte("no auth.token set")) {
		t.Errorf("output %q is missing the warning", out)
	}
	if !bytes.Contains([]byte(out), []byte("level=WARN")) {
		t.Errorf("output %q did not record the warning at WARN level", out)
	}
}

// The level must be inspectable, not just observable, so the seam can be
// asserted without emitting a line that depends on log formatting.
func TestNewLoggerLevel(t *testing.T) {
	if !newLogger(true).Enabled(nil, slog.LevelDebug) {
		t.Error("newLogger(true) should enable debug")
	}
	if newLogger(false).Enabled(nil, slog.LevelDebug) {
		t.Error("newLogger(false) should not enable debug")
	}
	if !newLogger(false).Enabled(nil, slog.LevelInfo) {
		t.Error("newLogger(false) should still enable info")
	}
}

// -v must raise the level of the *package-level* logger as well, not just the
// one the command carries: config, proxy and router helpers log through
// logx.Std, so a flag that only relevels the carried logger silently drops
// their debug lines (config's "api key file unreadable" included). newLogger
// is the seam every -v flag is wired through - runServe, runConvert and
// runCheck all build their logger here - so this is the one place that pins
// the wiring; the original regression was runServe building a logger and
// never publishing it.
func TestVerboseFlagRaisesPackageLevelLog(t *testing.T) {
	orig := logx.Std
	t.Cleanup(func() { logx.Std = orig })

	for _, tc := range []struct {
		name     string
		verbose  bool
		wantLogs bool
	}{
		{"-v raises the package logger", true, true},
		{"without -v it stays at info", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finish := capture(t)
			newLogger(tc.verbose) // what runServe and runCheck do with the flag
			logx.Std.Debug("api key file unreadable", "path", "/tmp/key")
			out := finish()
			if (out != "") != tc.wantLogs {
				t.Errorf("package-level Debug after newLogger(%v): produced %q, want output: %v", tc.verbose, out, tc.wantLogs)
			}
			if got := logx.Std.Enabled(nil, slog.LevelDebug); got != tc.wantLogs {
				t.Errorf("newLogger(%v): logx.Std debug enabled = %v, want %v", tc.verbose, got, tc.wantLogs)
			}
		})
	}
}

// The server struct runServe builds must carry the derived ReadTimeout, not
// just the config helper that computes it: the helper has its own test in
// tests/, but only this seam proves the struct production actually serves
// with has the field set. Without it a client can declare a big
// Content-Length, send a few bytes and stall, pinning ~that much of the body
// budget open forever - a slowloris against a 503-ing endpoint.
func TestHTTPServerCarriesReadTimeout(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.MaxBodyMB = 64
	cfg.Server.MaxMediaBodyMB = 256
	srv := newHTTPServer(cfg, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	if want := cfg.Server.ReadTimeout(); srv.ReadTimeout != want {
		t.Errorf("ReadTimeout = %v, want the derived %v", srv.ReadTimeout, want)
	}
	if want := 256 * time.Second; srv.ReadTimeout != want {
		t.Errorf("ReadTimeout = %v, want %v (a second per MB of the larger ceiling)", srv.ReadTimeout, want)
	}
	if want := 20 * time.Second; srv.ReadHeaderTimeout != want {
		t.Errorf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, want)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0: a streamed completion would be cut off", srv.WriteTimeout)
	}
	if want := 120 * time.Second; srv.IdleTimeout != want {
		t.Errorf("IdleTimeout = %v, want %v", srv.IdleTimeout, want)
	}
	if srv.Handler == nil {
		t.Error("Handler is nil, the server would 404 everything")
	}
}

// configPath decides which file every subcommand reads, so it has to honour
// the flag first, then the environment, then the documented default.
func TestConfigPathPrecedence(t *testing.T) {
	t.Setenv("NODEROUTER_CONFIG", "")
	if got := configPath("/tmp/explicit.json"); got != "/tmp/explicit.json" {
		t.Errorf("the -config flag should win, got %q", got)
	}

	t.Setenv("NODEROUTER_CONFIG", "/tmp/from-env.json")
	if got := configPath(""); got != "/tmp/from-env.json" {
		t.Errorf("NODEROUTER_CONFIG should be used when no flag is given, got %q", got)
	}
	if got := configPath("/tmp/explicit.json"); got != "/tmp/explicit.json" {
		t.Errorf("the -config flag should still beat the environment, got %q", got)
	}

	t.Setenv("NODEROUTER_CONFIG", "")
	if got, want := configPath(""), config.DefaultConfigPath; got != want {
		t.Errorf("with neither set the default should be used, got %q want %q", got, want)
	}
}

// keygen has to produce a key the router will actually accept: appended to the
// file auth.apiKeys reads per request, in the shape that reader parses, with a
// snippet that says how to wire it and call it. Anything less and the command
// only looks like it worked.
func TestKeygenMintsAppendsAndPrints(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "client-keys.txt")

	cfg := &config.Config{}
	cfg.ApplyDefaults()

	var buf bytes.Buffer
	if err := keygen(&buf, cfg, keyPath, 2, "alice"); err != nil {
		t.Fatalf("keygen: %v", err)
	}

	raw, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key file: %v", err)
	}
	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", fi.Mode().Perm())
	}

	// The router's own reader is the contract the file exists for.
	var probe config.Config
	probe.Auth.APIKeys = config.KeyList{"file:" + keyPath}
	probe.ApplyDefaults()
	keys, required := probe.Auth.ClientKeys()
	if !required || len(keys) != 2 {
		t.Fatalf("ClientKeys = %v,%v want the two minted keys", keys, required)
	}
	for _, key := range keys {
		if !strings.HasPrefix(key, "sk-nr-") {
			t.Errorf("key %q does not look like a client key", key)
		}
		if !strings.Contains(string(raw), key+" # alice") {
			t.Errorf("key file %q does not carry %q with its label", raw, key)
		}
		if !strings.Contains(buf.String(), key) {
			t.Errorf("snippet does not print %q:\n%s", key, buf.String())
		}
	}

	// A second run appends instead of clobbering, and -n 0 clamps to one
	// key rather than writing none.
	buf.Reset()
	if err := keygen(&buf, cfg, keyPath, 0, ""); err != nil {
		t.Fatalf("second keygen: %v", err)
	}
	raw, err = os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Split(strings.TrimSpace(string(raw)), "\n")); got != 3 {
		t.Errorf("key file holds %d lines, want 3 (two keys, then one more); content:\n%s", got, raw)
	}

	// Three wiring states, each with its own answer: the config already
	// reads the file, it points elsewhere, or it has no auth at all.
	wired := &config.Config{Auth: config.AuthConfig{APIKeys: config.KeyList{"file:" + keyPath}}}
	wired.ApplyDefaults()
	buf.Reset()
	if err := keygen(&buf, wired, keyPath, 1, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "already reads") {
		t.Errorf("a config that reads the file should be told the key is live:\n%s", buf.String())
	}

	elsewhere := &config.Config{Auth: config.AuthConfig{APIKeys: config.KeyList{"sk-somewhere-else"}}}
	elsewhere.ApplyDefaults()
	buf.Reset()
	if err := keygen(&buf, elsewhere, keyPath, 1, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "not accepted yet") {
		t.Errorf("a config pointing elsewhere should be told so:\n%s", buf.String())
	}

	buf.Reset()
	if err := keygen(&buf, cfg, keyPath, 1, ""); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\"auth\": { \"apiKeys\": [\"file:", "OPENAI_BASE_URL=", "OPENAI_API_KEY=", "Authorization: Bearer "} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("snippet is missing %q:\n%s", want, buf.String())
		}
	}
}

// Minted keys come from crypto/rand, so no two runs hand out the same one and
// none is short enough to guess.
func TestKeygenKeysAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		key, err := mintClientKey()
		if err != nil {
			t.Fatal(err)
		}
		if seen[key] {
			t.Fatalf("key %q was minted twice", key)
		}
		seen[key] = true
		if len(key) < 30 {
			t.Errorf("key %q is shorter than a guess-resistant key should be", key)
		}
	}
}

// The background re-check has to wake on the window the operator configured:
// reValidateAfter is the answer to both "how long is a check valid" and "how
// often do we look", so shortening it to 90m must shorten the schedule too.
// "0" means "re-test everything" - a window, not an interval - and must not
// become a ticker that spins on zero.
func TestRecheckEveryFollowsTheWindow(t *testing.T) {
	cfg := &config.Config{}
	cfg.ApplyDefaults()
	if got := recheckEvery(cfg); got != config.DefaultReValidateAfter {
		t.Errorf("default interval = %v, want the reValidateAfter default %v", got, config.DefaultReValidateAfter)
	}
	if d, err := time.ParseDuration(cfg.Proxy.Test.ReValidateAfter); err != nil || d != 6*time.Hour {
		t.Errorf("default reValidateAfter = %q, want the 6h the docs promise", cfg.Proxy.Test.ReValidateAfter)
	}

	cfg.Proxy.Test.ReValidateAfter = "90m"
	if got := recheckEvery(cfg); got != 90*time.Minute {
		t.Errorf("interval = %v, want 90m", got)
	}

	// A window of 0 re-tests everything each pass, which is not a usable
	// interval; it falls back to the default rather than firing continuously.
	cfg.Proxy.Test.ReValidateAfter = "0"
	if got := recheckEvery(cfg); got != config.DefaultReValidateAfter {
		t.Errorf("interval for reValidateAfter=0 = %v, want the default %v", got, config.DefaultReValidateAfter)
	}

	cfg.Proxy.Test.ReValidateAfter = "-1h"
	if got := recheckEvery(cfg); got != config.DefaultReValidateAfter {
		t.Errorf("interval for a negative window = %v, want the default %v", got, config.DefaultReValidateAfter)
	}
}

// testRouter builds the smallest server openStatusHistory can be handed. The
// pool is never reloaded, so nothing here reads the user's proxy file.
func testRouter(t *testing.T, cfg *config.Config) *router.Server {
	t.Helper()
	pool := proxy.NewPool(cfg.Proxy.File, cfg.MetaPath(), cfg.Proxy.Mode, cfg.ProxyEnabled())
	return router.NewServer(cfg, pool, newLogger(false))
}

// An update means a restart, and a restart must not throw the status window
// away: the history the previous run left on disk is merged back at startup,
// before the listener opens.
func TestStatusHistoryIsRestoredAtStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	at := time.Now().UTC().Add(-45 * time.Minute)

	// What the previous run left behind.
	first := router.NewStatusTracker(7 * 24 * time.Hour)
	if _, err := first.UseFile(path); err != nil {
		t.Fatal(err)
	}
	first.Record(true, 200, at, "gpt-x", "alpha")
	first.Record(false, 502, at, "gpt-x", "alpha")
	if err := first.SaveIfDirty(); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Status: config.StatusConfig{File: path}}
	cfg.ApplyDefaults()
	srv := testRouter(t, cfg)

	if got := openStatusHistory(cfg, srv, newLogger(false)); got != path {
		t.Errorf("openStatusHistory = %q, want %q", got, path)
	}
	snap := srv.StatusSnapshot()
	if got, _ := snap["total"].(int); got != 2 {
		t.Errorf("restored total = %v, want 2", snap["total"])
	}
	if got, _ := snap["failed"].(int); got != 1 {
		t.Errorf("restored failed = %v, want 1", snap["failed"])
	}
	if got := srv.Status.FilePath(); got != path {
		t.Errorf("tracker file = %q, want %q", got, path)
	}
}

// The switch is the whole story: with status.enabled false the file is neither
// read nor written, so a box that wants the old RAM-only behaviour gets exactly
// that - and the history already on disk is left alone rather than clobbered.
func TestStatusHistoryStaysOffWhenDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")

	first := router.NewStatusTracker(7 * 24 * time.Hour)
	if _, err := first.UseFile(path); err != nil {
		t.Fatal(err)
	}
	first.Record(true, 200, time.Now(), "gpt-x", "alpha")
	if err := first.SaveIfDirty(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Status: config.StatusConfig{Enabled: config.BoolPtr(false), File: path}}
	cfg.ApplyDefaults()
	srv := testRouter(t, cfg)

	if got := openStatusHistory(cfg, srv, newLogger(false)); got != "" {
		t.Errorf("openStatusHistory = %q, want \"\" when persistence is off", got)
	}
	if got := srv.StatusSnapshot()["total"]; got != 0 {
		t.Errorf("total = %v, want 0: nothing should have been loaded", got)
	}
	srv.Status.Record(true, 200, time.Now())
	if err := srv.Status.SaveIfDirty(); err != nil {
		t.Fatalf("SaveIfDirty with persistence off: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a router with persistence off rewrote the history file it did not adopt")
	}
}

// The starter config is the first thing a new install reads, so it has to carry
// the section that keeps the history - a key nobody can guess is not a default.
func TestStarterConfigCarriesTheStatusSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	if err := writeStarterConfig(path); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("the starter config does not load: %v", err)
	}
	if !cfg.StatusEnabled() {
		t.Error("the starter config turns status persistence off")
	}
	if got := cfg.StatusFilePath(); got != config.DefaultStatusPath {
		t.Errorf("starter config status file = %q, want %q", got, config.DefaultStatusPath)
	}
	if got, want := cfg.StatusSaveEvery(), time.Duration(config.DefaultStatusSaveSecs)*time.Second; got != want {
		t.Errorf("starter config save interval = %v, want %v", got, want)
	}
}
