package config

// The provider key pool. A "file:" entry stands for every key in a file on
// disk, so a rotated credential takes effect without a restart. Rereading that
// file per *attempt* is wasted work though: one request that retries ten times
// opens the same file ten times and cannot get ten different answers out of it.
// So the parse is kept, and dropped as soon as the file's modification time or
// size moves - which is what a rotation does.

import (
	"os"
	"strings"
	"sync"
	"time"

	"noderouter/logx"
)

const (
	// keyCacheTTL is how long a key file nobody has reread lately keeps its
	// parsed keys. A config reload can name a different file, so the cache is
	// swept rather than left to accumulate.
	keyCacheTTL = 5 * time.Minute
	// maxCachedKeyFiles caps how many key files are held at once. An entry is
	// one stamp plus the parsed keys, so this is a backstop against a
	// pathological config, not a number worth tuning.
	maxCachedKeyFiles = 64
)

type cachedKeyFile struct {
	keys     []string
	mod      time.Time
	size     int64
	lastUsed time.Time
}

var keyCache = struct {
	mu sync.Mutex
	m  map[string]*cachedKeyFile
}{m: map[string]*cachedKeyFile{}}

// readKeyEntry resolves one pool entry to the keys behind it: a literal is
// itself, a "file:" path is every key in that file.
func readKeyEntry(entry string) []string {
	rest, ok := strings.CutPrefix(entry, "file:")
	if !ok {
		if entry == "" {
			return nil
		}
		return []string{entry}
	}
	return readKeyFile(ExpandPath(rest))
}

// readKeyFile returns the keys in the file at path, parsing it only when the
// file has changed since the last read. The returned slice is shared and must
// not be modified; callers copy the keys out of it.
//
// A file that cannot be read contributes no keys and no cache entry, so a key
// file that appears later is still picked up. The stat is taken before the
// read, which can only ever cache keys that are newer than the stamp they are
// filed under, so the next call rereads and converges on the truth.
func readKeyFile(path string) []string {
	fi, err := os.Stat(path)
	if err != nil {
		logx.Std.Debug("api key file unreadable", "path", path, "error", err)
		return nil
	}

	now := time.Now()
	keyCache.mu.Lock()
	defer keyCache.mu.Unlock()

	if e := keyCache.m[path]; e != nil && e.size == fi.Size() && e.mod.Equal(fi.ModTime()) {
		e.lastUsed = now
		return e.keys
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		logx.Std.Debug("api key file unreadable", "path", path, "error", err)
		return nil
	}
	keys := parseKeyFile(raw)

	evictKeyFilesLocked(now)
	e := &cachedKeyFile{keys: keys, mod: fi.ModTime(), size: fi.Size(), lastUsed: now}
	keyCache.m[path] = e
	return keys
}

// parseKeyFile splits a key file into keys: "#" comments and blank lines are
// ignored, like in the proxy list, and every remaining field is one key.
func parseKeyFile(raw []byte) []string {
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		out = append(out, strings.Fields(line)...)
	}
	return out
}

// evictKeyFilesLocked drops idle-expired entries and trims to the cap. Caller
// must hold the cache mutex. Same shape as proxy's transport cache, because it
// is the same job: keep a hot-path lookup off the disk without letting the
// map behind it grow forever.
func evictKeyFilesLocked(now time.Time) {
	for path, e := range keyCache.m {
		if now.Sub(e.lastUsed) > keyCacheTTL {
			delete(keyCache.m, path)
		}
	}
	for len(keyCache.m) >= maxCachedKeyFiles {
		oldestPath := ""
		var oldest time.Time
		first := true
		for path, e := range keyCache.m {
			if first || e.lastUsed.Before(oldest) {
				oldestPath, oldest, first = path, e.lastUsed, false
			}
		}
		delete(keyCache.m, oldestPath)
	}
}
