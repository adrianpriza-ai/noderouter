package proxy

import (
	"context"
	"os"

	"noderouter/config"
)

// Private-file provider: the user's own proxies, kept in a separate file so
// the routine harvest of the free sources never overwrites them. Same line
// format as the public list (scheme://user:pass@host:port, host:port, ...);
// entries that fail the test are left out of ports.txt but stay on disk here.

func init() {
	RegisterProvider(&Provider{
		Name:      "private",
		Aliases:   []string{"privatefile", "ports-private", "ports_private", "myproxies", "own"},
		Protected: true,
		Fetch: func(_ context.Context, req FetchRequest, notify func(string, ...any)) ([]string, error) {
			if req.PrivateFile == "" {
				notify("no private proxy file configured", "key", "proxy.privateFile")
				return nil, nil
			}
			path := config.ExpandPath(req.PrivateFile)
			// Missing file is not an error: most users never have private
			// proxies and it must not spam a failure on every refresh.
			if _, err := os.Stat(path); err != nil {
				if os.IsNotExist(err) {
					notify("no private proxy file", "path", path)
					return nil, nil
				}
				return nil, err
			}
			notify("loading private proxy file", "file", req.PrivateFile)
			return LoadProxiesFile(req.PrivateFile, req.Protocol)
		},
	})
}
