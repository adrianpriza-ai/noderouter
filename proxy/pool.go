package proxy

import (
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"noderouter/config"
	"noderouter/logx"
)

// Pool is the in-memory view of the proxy list file. It is reloaded when the
// file changes on disk and after every scheduled refresh.
type Pool struct {
	mu      sync.RWMutex
	entries []ProxyEntry
	list    string
	meta    string
	modTime time.Time
	size    int64
	loaded  time.Time

	mode    string
	enabled bool
	cursor  uint64
	health  map[string]*proxyHealth
	rr      *rand.Rand
}

type proxyHealth struct {
	fails   int
	until   time.Time // skip until this time
	lastErr string
	last    time.Time // when the last failure was recorded
}

const (
	MaxProxyFails = 3
	proxyCooldown = 5 * time.Minute
	// proxyHealthTTL bounds how long a trailing failure count is kept
	// without another failure. Past it the entry is dropped so the map
	// cannot grow with every URL ever tried.
	proxyHealthTTL = time.Hour
)

// NewPool creates an empty pool bound to the given files.
func NewPool(listPath, metaPath, mode string, enabled bool) *Pool {
	return &Pool{
		list:    config.ExpandPath(listPath),
		meta:    config.ExpandPath(metaPath),
		mode:    mode,
		enabled: enabled,
		health:  map[string]*proxyHealth{},
		rr:      rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Reload re-reads the list file.
func (p *Pool) Reload() (int, error) {
	entries, err := LoadProxyList(p.list, p.meta)
	var mod time.Time
	var size int64
	if st, statErr := os.Stat(p.list); statErr == nil {
		mod = st.ModTime()
		size = st.Size()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		return 0, err
	}
	p.entries = entries
	p.modTime = mod
	p.size = size
	p.loaded = time.Now()
	// Health records for URLs that fell out of the list are dropped too;
	// otherwise the map leaks one entry per dropped proxy forever.
	keep := make(map[string]bool, len(entries))
	for _, e := range entries {
		keep[e.URL] = true
	}
	for u := range p.health {
		if !keep[u] {
			delete(p.health, u)
		}
	}
	return len(entries), nil
}

// ReloadIfChanged reloads only when the file's mtime or size moved, which
// lets an outside `noderouter convert` run take effect without a restart.
func (p *Pool) ReloadIfChanged() (bool, error) {
	st, err := os.Stat(p.list)
	if err != nil {
		return false, err
	}
	p.mu.RLock()
	unchanged := st.ModTime().Equal(p.modTime) && st.Size() == p.size
	p.mu.RUnlock()
	if unchanged {
		return false, nil
	}
	if _, err := p.Reload(); err != nil {
		return false, err
	}
	return true, nil
}

func (p *Pool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.entries)
}

// Sequence returns a token identifying one new request. Pass it to Take for
// every attempt of that request so retries walk to the next proxy while
// consecutive requests keep rotating through the list.
func (p *Pool) Sequence() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	seq := p.cursor
	p.cursor++
	return seq
}

// Take returns the proxy to use for attempt i (0-based) of the request
// identified by seq, and whether a proxy is available at all.
//
// The attempt number offsets the starting point in the list, so consecutive
// attempts land on different proxies; from there the pool walks forward to the
// first entry that is not benched. With a single proxy, or when everything is
// benched, the same entry is reused rather than failing the request outright.
func (p *Pool) Take(seq uint64, attempt int) (ProxyEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := len(p.entries)
	if n == 0 {
		return ProxyEntry{}, false
	}
	now := time.Now()
	healthy := func(i int) bool {
		h := p.health[p.entries[i].URL]
		return h == nil || !now.Before(h.until)
	}

	var base int
	switch p.mode {
	case "first": // the head of the list is the fastest proxy that was tested
		base = 0
	case "random":
		base = p.rr.Intn(n)
	default: // rotate
		base = (int(seq) + attempt) % n
	}

	for i := 0; i < n; i++ {
		if idx := (base + i) % n; healthy(idx) {
			return p.entries[idx], true
		}
	}
	return p.entries[base], true
}

// Succeeded clears the failure counter for a proxy.
func (p *Pool) Succeeded(entry ProxyEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.health, entry.URL)
}

// Failed puts a proxy in a short cooldown once it fails repeatedly.
func (p *Pool) Failed(entry ProxyEntry, reason error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.sweepHealthLocked(now)
	h := p.health[entry.URL]
	if h == nil {
		h = &proxyHealth{}
		p.health[entry.URL] = h
	}
	h.fails++
	h.lastErr = reason.Error()
	h.last = now
	if h.fails >= MaxProxyFails {
		h.until = now.Add(proxyCooldown)
		h.fails = 0
		logx.Warn("proxy benched", "proxy", RedactURL(entry.URL), "for", proxyCooldown.String(), "reason", h.lastErr)
	}
}

// sweepHealthLocked drops entries that carry no information anymore:
// benched proxies whose cooldown has run out (they start clean) and
// trailing failure counts that have not been repeated within
// proxyHealthTTL. Caller must hold p.mu.
func (p *Pool) sweepHealthLocked(now time.Time) {
	for u, h := range p.health {
		switch {
		case !h.until.IsZero() && !now.Before(h.until):
			delete(p.health, u)
		case h.until.IsZero() && now.Sub(h.last) > proxyHealthTTL:
			delete(p.health, u)
		}
	}
}

// Stats summarises the pool for the status page.
type PoolStats struct {
	Enabled   bool      `json:"enabled"`
	Mode      string    `json:"mode"`
	File      string    `json:"file"`
	Size      int64     `json:"fileSize"`
	Count     int       `json:"count"`
	Healthy   int       `json:"healthy"`
	Benched   int       `json:"benched"`
	LoadedAt  time.Time `json:"loadedAt,omitempty"`
	UpdatedAt time.Time `json:"fileModified,omitempty"`
	NextProxy string    `json:"nextProxy,omitempty"`
}

func (p *Pool) Stats() PoolStats {
	p.mu.RLock()
	defer p.mu.RUnlock()
	stats := PoolStats{
		Enabled: p.enabled, Mode: p.mode, File: p.list, Size: p.size,
		Count: len(p.entries), LoadedAt: p.loaded,
	}
	if !p.modTime.IsZero() {
		stats.UpdatedAt = p.modTime
	}
	now := time.Now()
	for _, e := range p.entries {
		if h := p.health[e.URL]; h != nil && now.Before(h.until) {
			stats.Benched++
		} else {
			stats.Healthy++
		}
	}
	if len(p.entries) > 0 {
		stats.NextProxy = RedactURL(p.entries[int(p.cursor%uint64(len(p.entries)))].URL)
	}
	return stats
}

// Describe renders the current pool for logs.
func (p *Pool) Describe() string {
	stats := p.Stats()
	if stats.Count == 0 {
		return fmt.Sprintf("0 proxies from %s", stats.File)
	}
	return fmt.Sprintf("%d proxies from %s (mode %s, %d benched)", stats.Count, stats.File, stats.Mode, stats.Benched)
}

// RedactURL strips the userinfo (user:password@) from a proxy URL, so a
// credentialed hop can be named in a response header, an error message or a
// log line without handing out the gateway password. Only the bytes before
// the last @ in the authority go: the scheme, host and port - the part that
// says which hop served the request - stay exactly as they were.
func RedactURL(raw string) string {
	scheme := strings.Index(raw, "://")
	if scheme < 0 {
		return raw
	}
	authority := raw[scheme+3:]
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return raw
	}
	// An @ past the first path/query separator belongs to the path, not to
	// credentials. Net/url splits the authority at the last @ as well, so
	// this agrees with how the URL would have been parsed.
	if sep := strings.IndexAny(authority, "/?#"); sep >= 0 && at > sep {
		return raw
	}
	return raw[:scheme+3] + authority[at+1:]
}

// parseProxyURL is a small helper that rejects proxy URLs we cannot dial.
func ParseProxyURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// url.Error echoes the URL it was handed, and a proxy URL may carry
		// credentials: report a redacted copy with the parser's reason.
		reason := error(err)
		var uerr *url.Error
		if errors.As(err, &uerr) {
			reason = uerr.Err
		}
		return nil, fmt.Errorf("bad proxy url %q: %v", RedactURL(raw), reason)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	case "socks4", "socks4a":
		return nil, fmt.Errorf("socks4 is not supported by this build")
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("proxy %q has no host", RedactURL(raw))
	}
	return u, nil
}
