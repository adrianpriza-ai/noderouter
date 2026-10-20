package proxy

import (
	"context"
	"fmt"
	"time"

	"noderouter/config"
	"noderouter/logx"
)

// RevalidateResult reports what one re-validation pass did, so a caller can
// log or print it without re-deriving the numbers from the entry slices.
type RevalidateResult struct {
	Total    int // entries the list held when the pass started
	Fresh    int // entries left alone, checked inside the window
	Stale    int // entries the pass re-tested
	Alive    int // of those, the ones that answered
	Dropped  int // of those, the ones that did not
	Duration time.Duration
	// Entries is the list after the merge, in the order it is written to disk.
	Entries []ProxyEntry
	// Working is only the entries that answered, fastest first.
	Working []ProxyEntry
	Written bool
}

// Revalidate re-tests the entries in the list file whose last successful check
// is older than cfg.ReValidateAfter() and writes the survivors back in place.
// It is `noderouter check` as a library call: the scheduled run in serve uses
// it, and check is a thin wrapper over it, so the pool can never be
// re-validated two different ways.
//
// Unlike Harvest it never fetches from the sources. The list on disk is the
// input as well as the output, so the ranking and the private proxies survive
// a re-check untouched.
//
// A list that is still fresh costs nothing: no proxy is dialled and the file is
// left exactly as it was. force ignores the window and re-tests everything.
//
// A pass that leaves nothing alive keeps the previous list instead of writing
// an empty one: an empty ports file reads as "no proxies", and the router would
// then quietly fall back to going direct, which is the one outcome that must
// not happen by itself in the middle of the night.
func Revalidate(ctx context.Context, cfg *config.Config, save, force bool, notify func(string, ...any)) (RevalidateResult, error) {
	var res RevalidateResult
	if cfg == nil {
		return res, fmt.Errorf("no config")
	}
	if notify == nil {
		notify = logx.Info
	}
	started := time.Now()
	window := cfg.ReValidateAfter()

	listPath := config.ExpandPath(cfg.Proxy.File)
	entries, err := LoadProxyList(listPath, config.MetaPathFor(listPath))
	if err != nil {
		return res, err
	}
	res.Total = len(entries)
	if len(entries) == 0 {
		return res, fmt.Errorf("no proxies in %s", listPath)
	}

	stale, fresh := SplitStale(entries, window, time.Now())
	if force {
		stale, fresh = entries, nil
	}
	res.Stale, res.Fresh = len(stale), len(fresh)
	res.Entries = entries
	if len(stale) == 0 {
		res.Duration = time.Since(started)
		return res, nil
	}

	targets := make([]string, len(stale))
	for i, e := range stale {
		targets[i] = e.URL
	}
	notify("re-checking proxies", "due", len(stale), "fresh", len(fresh), "window", window.String())
	working := TestProxies(ctx, targets, cfg.Proxy.Test.Workers,
		time.Duration(cfg.Proxy.Test.TimeoutMS)*time.Millisecond,
		cfg.Proxy.Test.URLs, cfg.Proxy.Test.SkipTLSVerify)
	res.Working = working
	res.Alive = len(working)

	merged, dropped := MergeRevalidated(entries, stale, working)
	res.Dropped = dropped
	res.Entries = merged
	res.Duration = time.Since(started)

	if save {
		switch {
		case len(merged) == 0:
			// Not a usable pool any more, so keep what is on disk rather than
			// emptying the file the router dials through.
			notify("every re-checked proxy failed, keeping the previous list",
				"tested", len(stale), "file", listPath)
		default:
			if err := SaveProxyList(merged, cfg.Proxy.File, cfg.MetaPath()); err != nil {
				return res, err
			}
			res.Written = true
		}
	}
	notify("proxy pool re-checked", "alive", len(merged), "dropped", dropped,
		"tested", len(stale), "took", res.Duration.Round(time.Millisecond).String())
	return res, nil
}
