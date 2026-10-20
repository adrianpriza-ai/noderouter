package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"

	"noderouter/config"
	"noderouter/logx"
	"noderouter/proxy"
	"noderouter/router"
)

// Version is the build version. It stays "dev" unless the build stamps it:
//
//	go build -ldflags "-X main.Version=1.2.3" -o noderouter .
//
// assets.go hands it to the packages that report it - the index endpoint and
// the User-Agent the proxy sources see - so one number covers all of them.
var Version = "dev"

// newLogger builds the process logger and publishes it as logx.Std, the
// package-level logger used by helpers that do not carry one around (config's
// debug lines, proxy's warnings). Every subcommand with a -v flag builds its
// logger here, so the flag raises both levels at once - the regression this
// replaces was a serve -v that raised only the logger it carried and left Std
// at Info, silently dropping logx.Std.Debug everywhere even with -v set.
func newLogger(verbose bool) *logx.Logger {
	log := logx.New(verbose)
	logx.Std = log
	return log
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve", "start", "run":
			os.Args = append(os.Args[:1], os.Args[2:]...)
			runServe(os.Args[1:])
			return
		case "init":
			os.Args = append(os.Args[:1], os.Args[2:]...)
			runInit(os.Args[1:])
			return
		case "convert", "proxies", "fetch":
			os.Args = append(os.Args[:1], os.Args[2:]...)
			runConvert(os.Args[1:])
			return
		case "check", "test":
			os.Args = append(os.Args[:1], os.Args[2:]...)
			runCheck(os.Args[1:])
			return
		case "routes", "models":
			os.Args = append(os.Args[:1], os.Args[2:]...)
			runRoutes(os.Args[1:])
			return
		case "keygen":
			os.Args = append(os.Args[:1], os.Args[2:]...)
			runKeygen(os.Args[1:])
			return
		case "version", "--version":
			fmt.Printf("noderouter %s\n", Version)
			return
		case "help", "-h", "--help":
			usage()
			return
		}
	}
	// No subcommand: serve.
	runServe(os.Args[1:])
}

func usage() {
	fmt.Printf(`noderouter %s - route LLM requests through rotating proxies

usage:
  noderouter [serve] [flags]   run the router (default command)
  noderouter init [flags]      write a starter config to ~/.noderouter/models.json
  noderouter convert [flags]   fetch + test proxies into the ports file
  noderouter check [flags]     re-test the proxies already in the ports file
  noderouter routes [flags]    print how each model is routed
  noderouter keygen [flags]    mint a client api key and show how to use it
  noderouter version

run "noderouter <command> -h" for the flags of each command.
`, Version)
}

func configPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("NODEROUTER_CONFIG"); env != "" {
		return env
	}
	return config.DefaultConfigPath
}

// ----------------------------------------------------------------- serve ---

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file (default ~/.noderouter/models.json)")
	host := fs.String("host", "", "listen host (overrides config)")
	port := fs.Int("port", 0, "listen port, 0 picks a free one (overrides config)")
	noProxy := fs.Bool("no-proxy", false, "ignore the proxy list and connect directly")
	verbose := fs.Bool("v", false, "debug logging")
	fs.Parse(args)

	// Distinguish "-port 0" (let the OS pick a free port) from the flag simply
	// not being given, which should leave the config value alone.
	portSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			portSet = true
		}
	})

	log := newLogger(*verbose)
	path := configPath(*cfgPath)

	cfg, err := config.LoadConfig(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "no config at") {
			log.Warn("no config yet, writing a starter one")
			if err := writeStarterConfig(path); err != nil {
				log.Error("could not write config", "error", err)
				os.Exit(1)
			}
			if cfg, err = config.LoadConfig(path); err != nil {
				log.Error("could not read the config we just wrote", "error", err)
				os.Exit(1)
			}
			log.Info("edit this file, then restart", "path", config.ExpandPath(path))
		} else {
			log.Error("config problem", "error", err)
			os.Exit(1)
		}
	}
	// Soft memory layer: point the Go GC at the configured ceiling before any
	// traffic. GOMEMLIMIT, when already set in the environment, wins.
	applyMemoryLimit(cfg.Server.MemoryLimitMB, log)
	if *host != "" {
		cfg.Server.Host = *host
	}
	if *port != 0 || portSet {
		cfg.Server.Port = *port
	}
	if *noProxy {
		cfg.Proxy.Enabled = config.BoolPtr(false)
	}

	// Starting never fetches or tests: it only reads the list file. Use
	// `noderouter convert`, the refresh schedule, or POST /noderouter/refresh
	// to fill or refresh the pool.
	pool := proxy.NewPool(cfg.Proxy.File, cfg.MetaPath(), cfg.Proxy.Mode, cfg.ProxyEnabled())
	if n, err := pool.Reload(); err != nil {
		log.Warn("no proxy list yet", "file", config.ExpandPath(cfg.Proxy.File), "hint", "run `noderouter convert`")
	} else {
		log.Info("proxy list loaded", "count", n, "file", config.ExpandPath(cfg.Proxy.File))
	}

	srv := router.NewServer(cfg, pool, log)
	if cfg.ProxyEnabled() && pool.Len() == 0 {
		log.Warn("no proxies in the list; requests go direct until you run `noderouter convert`")
	}

	// The request history behind /status and /v1/status is kept on disk, so a
	// restart - an update, a config edit, a crash a supervisor picks up -
	// continues the window instead of starting it over.
	statusFile := openStatusHistory(cfg, srv, log)

	// Scheduled + on-demand refresh. The mutex keeps a manual refresh from
	// racing the scheduled one over the same files.
	schedule, err := proxy.ParseSchedule(cfg.Proxy.Refresh)
	if err != nil {
		log.Error("bad proxy.refresh schedule", "value", cfg.Proxy.Refresh, "error", err)
		os.Exit(1)
	}
	refresh := func(ctx context.Context) error {
		srv.RefreshMu.Lock()
		defer srv.RefreshMu.Unlock()
		log.Info("refreshing proxies", "sources", cfg.Proxy.Sources)
		entries, res, err := proxy.Harvest(ctx, cfg, log.Info)
		if err != nil {
			return fmt.Errorf("harvest: %w", err)
		}
		if err := proxy.SaveProxyList(entries, cfg.Proxy.File, cfg.MetaPath()); err != nil {
			return fmt.Errorf("save list: %w", err)
		}
		if _, err := pool.Reload(); err != nil {
			return fmt.Errorf("reload: %w", err)
		}
		log.Info("proxy list updated", "fetched", res.Fetched, "alive", res.Alive, "saved", len(entries), "took", res.Duration.Round(time.Millisecond).String())
		return nil
	}
	srv.OnRefresh = refresh

	// File watcher: pick up lists written by `noderouter convert` or by hand.
	if cfg.ProxyEnabled() && cfg.Proxy.WatchSecs > 0 {
		go watchProxyFile(pool, time.Duration(cfg.Proxy.WatchSecs)*time.Second, log)
	}

	// Scheduler goroutine.
	stop := make(chan struct{})
	go func() {
		log.Info("proxy refresh scheduled", "cron", schedule.String(), "next", schedule.Next(time.Now()).Format(time.RFC3339))
		for {
			next := schedule.Next(time.Now())
			if next.IsZero() {
				log.Warn("schedule never fires", "cron", schedule.String())
				return
			}
			srv.NextRefreshUnix.Store(next.Unix())
			timer := time.NewTimer(time.Until(next))
			select {
			case <-timer.C:
				if cfg.ProxyEnabled() {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
					if err := refresh(ctx); err != nil {
						log.Error("scheduled refresh failed", "error", err)
					}
					cancel()
				}
			case <-stop:
				timer.Stop()
				return
			}
		}
	}()

	// Scheduled re-check of the pool already on disk: `noderouter check` as a
	// background job, on the same window the check subcommand honours
	// (proxy.test.reValidateAfter, 6h by default). It fetches nothing new - the
	// list on disk is both the input and the output - so a proxy that died since
	// the last harvest is caught in between, and the ranking survives.
	recheckInterval := recheckEvery(cfg)
	if cfg.ProxyEnabled() && cfg.TestEnabled() {
		srv.NextRecheckUnix.Store(time.Now().Add(recheckInterval).Unix())
		go func() {
			log.Info("proxy re-check scheduled", "every", recheckInterval.String(),
				"next", time.Now().Add(recheckInterval).Format(time.RFC3339))
			ticker := time.NewTicker(recheckInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
					srv.RefreshMu.Lock()
					_, err := proxy.Revalidate(ctx, cfg, true, false, log.Info)
					srv.RefreshMu.Unlock()
					cancel()
					// A failed pass leaves the file alone, so the pool in use
					// right now stays exactly as valid as it was.
					if err == nil {
						if n, rerr := pool.Reload(); rerr != nil {
							log.Warn("proxy list did not reload", "file", config.ExpandPath(cfg.Proxy.File), "error", rerr)
						} else {
							log.Debug("proxy list reloaded", "count", n)
						}
					} else {
						log.Error("scheduled re-check failed", "error", err)
					}
					srv.NextRecheckUnix.Store(time.Now().Add(recheckInterval).Unix())
				case <-stop:
					return
				}
			}
		}()
	}

	// Endpoint health: probe whatever is benched, so an upstream that came
	// back is returned to the pool without waiting out its cooldown. Only
	// benched endpoints are ever probed, so a healthy setup sends nothing of
	// its own - one GET per benched endpoint per interval, no more.
	if probeEvery := cfg.Endpoints.RecheckEvery(); probeEvery > 0 && cfg.Endpoints.Threshold() > 0 {
		go func() {
			log.Info("endpoint probe scheduled", "every", probeEvery.String())
			ticker := time.NewTicker(probeEvery)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					if n := srv.RecheckEndpoints(ctx); n > 0 {
						log.Info("endpoints back in the pool", "count", n)
					}
					cancel()
				case <-stop:
					return
				}
			}
		}()
	}

	// Periodic flush of the status history. Only the changed case writes, so a
	// quiet router touches the file once and then leaves it alone. A failure
	// (a directory that does not exist, a read-only disk) is worth saying once
	// rather than every interval.
	if every := cfg.StatusSaveEvery(); every > 0 && statusFile != "" {
		go func() {
			ticker := time.NewTicker(every)
			defer ticker.Stop()
			warned := false
			for {
				select {
				case <-ticker.C:
					if err := srv.Status.SaveIfDirty(); err != nil {
						if !warned {
							log.Warn("status history not saved", "file", statusFile, "error", err)
							warned = true
						}
					} else {
						warned = false
					}
				case <-stop:
					return
				}
			}
		}()
	}

	addr := net.JoinHostPort(cfg.Server.Host, fmt.Sprint(cfg.Server.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Error("cannot listen", "addr", addr, "error", err,
			"hint", "pick another port with -port, or set server.port in the config")
		os.Exit(1)
	}
	realAddr := ln.Addr().String()
	if ip, _, err := net.SplitHostPort(realAddr); err == nil && ip == "::" {
		realAddr = ":" + realAddr[strings.LastIndex(realAddr, ":")+1:]
	}

	httpSrv := newHTTPServer(cfg, srv.Handler())

	// Print a ready banner a client can grep for.
	fmt.Printf("noderouter %s listening on http://%s\n", Version, realAddr)
	fmt.Printf("  config   %s\n", config.ExpandPath(path))
	fmt.Printf("  proxies  %s\n", pool.Describe())
	fmt.Printf("  refresh  %s (next %s)\n", schedule.String(), schedule.Next(time.Now()).Format("15:04:05"))
	if statusFile != "" {
		fmt.Printf("  status   %s\n", statusFile)
	}
	if cfg.ProxyEnabled() && cfg.TestEnabled() {
		fmt.Printf("  recheck  every %s (next %s)\n", recheckInterval, time.Now().Add(recheckInterval).Format("15:04:05"))
	}
	if th := cfg.Endpoints.Threshold(); th > 0 {
		line := fmt.Sprintf("  endpoints bench after %d failures, back after %ds", th, int(cfg.Endpoints.Bench().Seconds()))
		if probeEvery := cfg.Endpoints.RecheckEvery(); probeEvery > 0 {
			line += fmt.Sprintf(" (probed every %s)", probeEvery)
		} else {
			line += " (no probes)"
		}
		fmt.Println(line)
	}
	fmt.Printf("  point your client at: base_url = http://%s/v1\n", realAddr)
	warnIfExposed(log, cfg)

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		log.Error("server stopped", "error", err)
		os.Exit(1)
	case s := <-sig:
		log.Info("shutting down", "signal", s.String())
	}
	close(stop)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	// Last flush, after the listener has stopped taking requests, so nothing
	// recorded up to this point is lost to the shutdown.
	if err := srv.Status.SaveIfDirty(); err != nil {
		log.Warn("status history not saved", "file", statusFile, "error", err)
	}
	fmt.Println("bye")
}

// openStatusHistory points the router at the file its request history is kept
// in and restores what that file holds, so a restart continues the status
// window rather than starting it over. It returns the expanded path, or "" when
// status persistence is turned off, which is also what the startup banner
// prints.
//
// An unreadable file is not fatal. The router runs on what it records from here
// and the first flush replaces the file, so a corrupt or foreign document costs
// the old history and nothing else.
func openStatusHistory(cfg *config.Config, srv *router.Server, log *logx.Logger) string {
	path := cfg.StatusFilePath()
	if path == "" {
		return ""
	}
	path = config.ExpandPath(path)
	hours, err := srv.Status.UseFile(path)
	if err != nil {
		log.Warn("status history not loaded; starting a fresh window", "file", path, "error", err)
	} else {
		log.Info("status history loaded", "file", path, "hours", hours)
		if n := srv.Status.RepairedHours(); n > 0 {
			log.Warn("status history repaired: an earlier build counted a forwarded request twice in the global total",
				"file", path, "hours", n, "note", "those hours now hold the request count instead")
		}
	}
	return path
}

// newHTTPServer builds the server struct runServe hands to Serve. The
// timeouts live here rather than inline so a test can pin them: without a
// ReadTimeout a client that declares a large Content-Length, sends a few
// bytes and stalls pins its body-budget reservation until the connection
// dies - ReadTimeout bounds reading the whole request (headers and body
// together), derived from the body ceiling by config.Server.ReadTimeout so
// the biggest accepted upload still has minutes to arrive. WriteTimeout is
// deliberately absent: a streamed completion can run for minutes and would
// be cut off by one.
func newHTTPServer(cfg *config.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 20 * time.Second,
		ReadTimeout:       cfg.Server.ReadTimeout(),
		// No WriteTimeout: streamed completions can run for minutes.
		IdleTimeout: 120 * time.Second,
	}
}

// applyMemoryLimit sets the soft GC ceiling (server.memoryLimitMb, default
// 100MiB). It tells the runtime to collect harder as the heap approaches the
// limit instead of hoarding freed pages - it never refuses work. An explicit
// GOMEMLIMIT in the environment takes precedence and is left untouched.
func applyMemoryLimit(mb int, log *logx.Logger) {
	if mb <= 0 {
		return
	}
	if env := os.Getenv("GOMEMLIMIT"); env != "" {
		log.Info("memory limit", "source", "GOMEMLIMIT", "value", env)
		return
	}
	debug.SetMemoryLimit(int64(mb) << 20)
	log.Info("memory limit", "source", "server.memoryLimitMb", "value", fmt.Sprintf("%dMiB", mb))
}

// warnIfExposed complains when the router is reachable from off the box but
// has nothing stopping a stranger from spending the provider's api key.
func warnIfExposed(log *logx.Logger, cfg *config.Config) {
	_, gated := cfg.Auth.ClientKeys()
	if router.IsLoopbackHost(cfg.Server.Host) || gated {
		return
	}
	log.Warn("listening on " + cfg.Server.Host + " with no auth.token or auth.apiKeys set: " +
		"anyone who can reach this port can use your provider key. Set auth.apiKeys, " +
		"or keep it on 127.0.0.1 and put nginx/tls in front of it")
}

// recheckEvery is how often the background re-check wakes. It follows
// proxy.test.reValidateAfter so that shortening the window to "90m" also
// re-checks the pool every 90 minutes; "0" means "re-test everything", which is
// a window rather than an interval and falls back to the default.
func recheckEvery(cfg *config.Config) time.Duration {
	every := cfg.ReValidateAfter()
	if every <= 0 {
		return config.DefaultReValidateAfter
	}
	return every
}

func watchProxyFile(pool *proxy.Pool, every time.Duration, log *logx.Logger) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for range ticker.C {
		changed, err := pool.ReloadIfChanged()
		if err != nil {
			continue // the file may be mid-rewrite; try again next tick
		}
		if changed {
			log.Info("proxy list changed on disk", "count", pool.Len())
		}
	}
}

// ------------------------------------------------------------------ init ---

func runInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	cfgPath := fs.String("config", "", "where to write the config")
	force := fs.Bool("force", false, "overwrite an existing config")
	fs.Parse(args)

	path := configPath(*cfgPath)
	if _, err := os.Stat(config.ExpandPath(path)); err == nil && !*force {
		fmt.Printf("%s already exists (use -force to overwrite)\n", config.ExpandPath(path))
		return
	}
	if err := writeStarterConfig(path); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", config.ExpandPath(path))
}

// writeStarterConfig creates a config that mirrors the documented example.
func writeStarterConfig(path string) error {
	path = config.ExpandPath(path)
	if path == "" {
		path = config.ExpandPath(config.DefaultConfigPath)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	stub := `{	"server": {
    "host": "127.0.0.1",
    "port": 32256,
    "memoryLimitMb": 100,
    "bodyBudgetMb": 33
  },
  "proxy": {
    "enabled": true,
    "mode": "rotate",
    "sources": ["proxyscrape", "geonode", "iplocate", "roundproxies"],
    "protocol": "all",
    "file": "~/.noderouter/ports.txt",
    "privateFile": "~/.noderouter/ports-private.txt",
    "refresh": "0 0 * * *",
    "test": { "enabled": true, "workers": 50, "timeoutMs": 2000, "reValidateAfter": "6h" }
  },
  "status": {
    "enabled": true,
    "file": "~/.noderouter/status.json",
    "saveSeconds": 30
  },
  "retry": {
    "maxAttempts": 10,
    "backoffMs": 500,
    "maxBackoffMs": 8000
  },
  "provider": {
    "LLM Povider": {
      "options": {
        "baseURL": "http://example.com/v1",
        "apiKey": "sk-...55",
        "re-proxy": true
      },
      "models": {
        "llm/minimax-m2.7": {
          "name": "LLM/Minimax M2.7",
          "limit": { "context": 180000, "output": 64000 },
          "description": "A LLM model from Minimax",
          "modalities": { "input": ["text", "image"], "output": ["text"] }
        },
        "llm/minimax-m3": {
          "name": "LLM/Minimax M3",
          "limit": { "context": 1000000, "output": 128000 },
          "description": "A LLM model from Minimax",
          "modalities": { "input": ["text", "image"], "output": ["text"] }
        }
      }
    }
  }
}
`
	return os.WriteFile(path, []byte(stub), 0o600)
}

// --------------------------------------------------------------- convert ---

func runConvert(args []string) {
	fs := flag.NewFlagSet("convert", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file to read defaults from")
	source := fs.String("source", "all", "proxyscrape, geonode, roundproxies, private, file or all")
	in := fs.String("in", "", "proxy file to read (with -source file)")
	private := fs.Bool("private", false, "also load the private proxy file (proxy.privateFile)")
	out := fs.String("out", "", "where to write the working list (default from config)")
	protocol := fs.String("protocol", "", "http, socks5 or all (default: from the config)")
	timeout := fs.Int("timeout", 10000, "max proxy response time in ms for ProxyScrape (1-15000)")
	limit := fs.Int("limit", 200, "how many proxies to fetch per source")
	workers := fs.Int("workers", 50, "concurrent tests")
	testTimeout := fs.Int("test-timeout", 2000, "per proxy test timeout in ms")
	noTest := fs.Bool("no-test", false, "skip testing and save everything fetched")
	insecure := fs.Bool("insecure", false, "keep proxies that MITM tls (needs insecureSkipVerify on the provider)")
	keep := fs.Int("keep", 0, "save only the N fastest working proxies (0 = all)")
	verbose := fs.Bool("v", false, "debug logging")
	fs.Parse(args)

	log := newLogger(*verbose)

	cfg, err := config.LoadConfig(configPath(*cfgPath))
	if err != nil {
		// Not fatal: fall back to defaults so convert works before init.
		log.Warn("using defaults", "error", err)
		cfg = &config.Config{}
		cfg.ApplyDefaults()
	}
	if *source != "all" && *source != "" {
		cfg.Proxy.Sources = strings.Split(*source, ",")
	}
	if *private {
		found := false
		for _, s := range cfg.Proxy.Sources {
			if strings.EqualFold(strings.TrimSpace(s), "private") {
				found = true
			}
		}
		if !found {
			cfg.Proxy.Sources = append(cfg.Proxy.Sources, "private")
		}
	}
	if *in != "" {
		cfg.Proxy.Sources = []string{"file"}
		cfg.Proxy.File = *in
	}
	if *out != "" {
		cfg.Proxy.File = *out
	}
	if *protocol != "" {
		cfg.Proxy.Protocol = *protocol
	}
	cfg.Proxy.MaxTimeoutMS = *timeout
	cfg.Proxy.Limit = *limit
	cfg.Proxy.Test.Workers = *workers
	cfg.Proxy.Test.TimeoutMS = *testTimeout
	cfg.Proxy.Test.Keep = *keep
	if *noTest {
		cfg.Proxy.Test.Enabled = config.BoolPtr(false)
	}
	if *insecure {
		cfg.Proxy.Test.SkipTLSVerify = true
		log.Warn("tls verification is off: proxies that intercept https will be kept, and so can read your traffic")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	entries, res, err := proxy.Harvest(ctx, cfg, log.Info)
	if err != nil {
		log.Error("no proxies to save", "error", err)
		os.Exit(1)
	}
	// Never clobber the user's own input file with the results.
	outPath := cfg.Proxy.File
	if abs, err1 := filepath.Abs(config.ExpandPath(cfg.Proxy.File)); err1 == nil {
		if inAbs, err2 := filepath.Abs(config.ExpandPath(*in)); *in != "" && err2 == nil && abs == inAbs {
			outPath = strings.TrimSuffix(cfg.Proxy.File, filepath.Ext(cfg.Proxy.File)) + "_working.txt"
			log.Info("not overwriting the input file", "output", config.ExpandPath(outPath))
		}
	}
	if err := proxy.SaveProxyList(entries, outPath, config.MetaPathFor(outPath)); err != nil {
		log.Error("could not save", "error", err)
		os.Exit(1)
	}

	fmt.Printf("\nFetched %d proxies from %s\n", res.Fetched, strings.Join(res.Sources, ", "))
	if res.Skipped > 0 {
		fmt.Printf("Skipped %d unsupported proxies (socks4)\n", res.Skipped)
	}
	fmt.Printf("Alive   %d (%.1fs)\n", res.Alive, res.Duration.Seconds())
	if len(entries) > 0 {
		fmt.Println("\nTop 10 fastest working proxies:")
		for i, e := range entries {
			if i >= 10 {
				break
			}
			fmt.Printf("  %-28s %6dms  -> %s\n", e.URL, e.LatencyMS, e.ExitIP)
		}
	}
	fmt.Printf("\nSaved %d proxies to %s\n", len(entries), config.ExpandPath(outPath))
	fmt.Println("The router picks these up automatically (or POST /noderouter/reload).")
}

// ----------------------------------------------------------------- check ---

func runCheck(args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file to read defaults from")
	in := fs.String("in", "", "proxy list to test (default from config)")
	workers := fs.Int("workers", 50, "concurrent tests")
	testTimeout := fs.Int("test-timeout", 2000, "per proxy test timeout in ms")
	save := fs.Bool("save", true, "write the working proxies back to the list")
	insecure := fs.Bool("insecure", false, "keep proxies that MITM tls")
	top := fs.Int("top", 10, "how many results to print")
	force := fs.Bool("force", false, "re-test everything, ignoring the re-validate window")
	verbose := fs.Bool("v", false, "debug logging")
	fs.Parse(args)

	// -v applies to the package-level logger too: newLogger publishes what it
	// builds as logx.Std, so config's and proxy's debug lines mean the same
	// thing in check as they do in serve. Built before LoadConfig, which is
	// where the first package-level debug line fires. check's own results
	// still print on stdout below either way.
	newLogger(*verbose)

	cfg, err := config.LoadConfig(configPath(*cfgPath))
	if err != nil {
		cfg = &config.Config{}
		cfg.ApplyDefaults()
	}
	if *in != "" {
		cfg.Proxy.File = *in
	}
	listPath := config.ExpandPath(cfg.Proxy.File)

	// Only a flag the user actually typed overrides the config's test settings.
	// The bare defaults above are what ApplyDefaults fills in anyway, so
	// letting them through would quietly ignore proxy.test.workers, timeoutMs
	// and insecureSkipVerify.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "workers":
			cfg.Proxy.Test.Workers = *workers
		case "test-timeout":
			cfg.Proxy.Test.TimeoutMS = *testTimeout
		case "insecure":
			if *insecure {
				cfg.Proxy.Test.SkipTLSVerify = true
			}
		}
	})

	// Default: re-test only what is past proxy.test.reValidateAfter, so a
	// manual check of a fresh list is a no-op. -force re-tests everything.
	// proxy.Revalidate is the same logic the scheduled re-check runs, so the
	// two can never drift apart.
	res, err := proxy.Revalidate(context.Background(), cfg, *save, *force, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if res.Stale == 0 {
		fmt.Printf("\nAll %d proxies were checked within %s, nothing to do.\n", res.Total, cfg.ReValidateAfter())
		fmt.Println("Use `noderouter check -force` to re-test them anyway.")
		return
	}

	fmt.Printf("\n%d/%d proxies alive (%d skipped as still fresh)\n", res.Alive, res.Stale, res.Fresh)
	for i, e := range res.Working {
		if i >= *top {
			break
		}
		fmt.Printf("  %-28s %6dms  -> %s\n", e.URL, e.LatencyMS, e.ExitIP)
	}
	fmt.Printf("%d proxies dropped\n", res.Dropped)

	if *save {
		if res.Written {
			fmt.Printf("\nSaved %d proxies to %s\n", len(res.Entries), listPath)
		} else {
			fmt.Printf("\nNothing saved: no proxy answered, so %s is left as it was\n", listPath)
		}
	}
}

// ---------------------------------------------------------------- routes ---

func runRoutes(args []string) {
	fs := flag.NewFlagSet("routes", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file")
	fs.Parse(args)

	cfg, err := config.LoadConfig(configPath(*cfgPath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("config  %s\nlisten  http://%s:%d/v1\nproxies %s\n\n",
		config.ExpandPath(cfg.Path()), cfg.Server.Host, cfg.Server.Port, poolSummary(cfg))

	entries := cfg.ModelTable()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Call < entries[j].Call })
	for _, e := range entries {
		var notes []string
		if e.Call != e.Key {
			notes = append(notes, fmt.Sprintf("config %q", e.Key))
		}
		if e.Upstream != e.Key {
			notes = append(notes, fmt.Sprintf("upstream %q", e.Upstream))
		}
		line := fmt.Sprintf("  %-28s -> %s", e.Call, e.Provider.Name)
		if len(notes) > 0 {
			line += " (" + strings.Join(notes, ", ") + ")"
		}
		if pool := e.Provider.Provider.EndpointPool(); len(pool) > 0 {
			line += "  " + pool[0]
			if len(pool) > 1 {
				line += fmt.Sprintf(" (+%d more endpoints)", len(pool)-1)
			}
		}
		fmt.Println(line)
	}
	if len(entries) == 0 {
		fmt.Println("  (no models configured)")
	}
	for _, note := range cfg.ModelIDNotes() {
		fmt.Println("  note: " + note)
	}
}

func poolSummary(cfg *config.Config) string {
	pool := proxy.NewPool(cfg.Proxy.File, cfg.MetaPath(), cfg.Proxy.Mode, cfg.ProxyEnabled())
	n, err := pool.Reload()
	if err != nil {
		return fmt.Sprintf("none (%v)", config.ExpandPath(cfg.Proxy.File))
	}
	return fmt.Sprintf("%d from %s", n, config.ExpandPath(cfg.Proxy.File))
}

// -------------------------------------------------------------- keygen ---

func runKeygen(args []string) {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file to read the listen address and auth from")
	out := fs.String("out", "", "key file to append to (default "+config.DefaultClientKeysPath+")")
	label := fs.String("label", "", "who the key is for, written as a comment next to it")
	count := fs.Int("n", 1, "how many keys to mint")
	fs.Parse(args)

	// The config is only consulted for the printed base URL and for whether
	// auth.apiKeys already reads the key file, so a missing one is not fatal:
	// keygen works before init, the same way convert does.
	cfg, err := config.LoadConfig(configPath(*cfgPath))
	if err != nil {
		cfg = &config.Config{}
		cfg.ApplyDefaults()
	}

	display := *out
	if display == "" {
		display = config.DefaultClientKeysPath
	}
	if err := keygen(os.Stdout, cfg, display, *count, *label); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// keygen mints count client keys, appends them to keyPath - created 0600 when
// it is missing - and prints what a client needs to use one. The keys live in
// the file rather than in the config because auth.apiKeys reads that file on
// every request: adding or revoking a client is then a line edit, not a
// restart. What the config still owes is the one line pointing at the file,
// which is printed below, along with a warning when it points elsewhere - a
// key nobody's config reads is a 401 waiting to be blamed on the router.
func keygen(w io.Writer, cfg *config.Config, keyPath string, count int, label string) error {
	if count < 1 {
		count = 1
	}
	expanded := config.ExpandPath(keyPath)
	if err := os.MkdirAll(filepath.Dir(expanded), 0o755); err != nil {
		return err
	}

	keys := make([]string, count)
	lines := make([]string, count)
	for i := range keys {
		key, err := mintClientKey()
		if err != nil {
			return err
		}
		keys[i] = key
		lines[i] = key
		if label != "" {
			// One comment per line, so each key carries its own owner
			// in a file that several keygen runs append to.
			lines[i] += " # " + label
		}
	}
	if err := appendKeyLines(expanded, strings.Join(lines, "\n")); err != nil {
		return err
	}

	noun := "key"
	if len(keys) != 1 {
		noun = "keys"
	}
	fmt.Fprintf(w, "%d %s written to %s\n", len(keys), noun, expanded)
	for _, key := range keys {
		fmt.Fprintf(w, "  %s\n", key)
	}
	if len(keys) > 1 {
		fmt.Fprintln(w, "(every key above works; the snippet uses the first)")
	}

	switch {
	case readsKeyFile(cfg, expanded):
		fmt.Fprint(w, "\nauth.apiKeys already reads this file, so the new key is live on the next request.\n")
	case len(cfg.Auth.APIKeys) > 0 || cfg.Auth.Token != "":
		fmt.Fprintf(w, "\nauth.apiKeys points somewhere else, so this key is not accepted yet. Point it at the file:\n\n  \"auth\": { \"apiKeys\": [\"%s\"] }\n", "file:"+keyPath)
	default:
		fmt.Fprintf(w, "\nadd this to the config (the router re-reads the file per request, so no restart):\n\n  \"auth\": { \"apiKeys\": [\"%s\"] }\n", "file:"+keyPath)
	}

	// A listener on 0.0.0.0 is not an address a client can use, so the
	// snippet names the loopback one instead.
	host := cfg.Server.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	base := fmt.Sprintf("http://%s:%d/v1", host, cfg.Server.Port)
	fmt.Fprintf(w, "\ngive the client:\n\n  export OPENAI_BASE_URL=%s\n  export OPENAI_API_KEY=%s\n", base, keys[0])
	fmt.Fprintf(w, "\nor as one request:\n\n  curl %s/chat/completions \\\n    -H \"Authorization: Bearer %s\" \\\n    -d '{\"model\":\"<model>\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'\n", base, keys[0])
	fmt.Fprintf(w, "\nto revoke a key, delete its line from %s; the router picks the change up on the next request.\n", keyPath)
	return nil
}

// mintClientKey returns a fresh client key. The shape is deliberate: an
// sk-nr- prefix is an ordinary api key to every SDK and to the router's own
// gate, and 24 bytes of crypto/rand leave nothing to guess.
func mintClientKey() (string, error) {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "sk-nr-" + base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// appendKeyLines adds lines to the key file, creating it 0600 and keeping a
// file that ends without a newline parseable. The chmod is best effort: a key
// file that already existed keeps whatever mode it had, and failing to
// tighten it must not cost the key that was just minted.
func appendKeyLines(path, lines string) error {
	prefix := ""
	if existing, err := os.ReadFile(path); err == nil && len(existing) > 0 && existing[len(existing)-1] != '\n' {
		prefix = "\n"
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(prefix + lines + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_ = os.Chmod(path, 0o600)
	return nil
}

// readsKeyFile reports whether auth.apiKeys already points at path, comparing
// the expanded forms so a "~" entry matches the absolute path it names.
func readsKeyFile(cfg *config.Config, path string) bool {
	for _, entry := range cfg.Auth.APIKeys {
		if rest, ok := strings.CutPrefix(entry, "file:"); ok && config.ExpandPath(rest) == path {
			return true
		}
	}
	return false
}
