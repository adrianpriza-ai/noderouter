package proxy

import (
	"context"
	"os"
	"strings"

	"noderouter/config"
	"noderouter/logx"
)

// File provider: re-reads the local proxy list instead of fetching anything.
// Its lines are taken as written, so undialable schemes (socks4) survive here
// and are dropped later by the dialability filter, where they can be counted.

func init() {
	RegisterProvider(&Provider{
		Name:    "file",
		Aliases: []string{"local"},
		Fetch: func(_ context.Context, req FetchRequest, notify func(string, ...any)) ([]string, error) {
			notify("loading proxy file", "file", req.File)
			return LoadProxiesFile(req.File, req.Protocol)
		},
	})
}

// LoadProxiesFile reads a local proxy list, dropping blanks, comments and dupes.
func LoadProxiesFile(path, defaultScheme string) ([]string, error) {
	path = config.ExpandPath(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	unparsed, dupes := 0, 0
	bare := BareScheme(defaultScheme)
	for _, line := range strings.Split(string(data), "\n") {
		p, ok := NormalizeProxy(line, bare)
		switch {
		case !ok:
			unparsed++
		case seen[p]:
			dupes++
		default:
			seen[p] = true
			out = append(out, p)
		}
	}
	if unparsed > 0 || dupes > 0 {
		logx.Info("proxy list", "file", path, "kept", len(out), "skipped", unparsed, "duplicates", dupes)
	}
	return out, nil
}
