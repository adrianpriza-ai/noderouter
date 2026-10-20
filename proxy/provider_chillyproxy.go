package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// ChillyProxy provider: one request per protocol, since its plain-text output
// is bare ip:port lines with no scheme attached. Each line is tagged with the
// protocol it was fetched under. socks4 proxies are emitted but dropped later
// by the dialability filter.

func init() {
	RegisterProvider(&Provider{
		Name:    "chillyproxy",
		Aliases: []string{"chilly"},
		Fetch: func(ctx context.Context, req FetchRequest, notify func(string, ...any)) ([]string, error) {
			notify("fetching chillyproxy", "protocol", req.Protocol)
			return FetchChillyProxy(ctx, req.Protocol)
		},
	})
}

// FetchChillyProxy pulls the ChillyProxy free list, fanning out across the
// protocols requested by the filter.
func FetchChillyProxy(ctx context.Context, protocol string) ([]string, error) {
	protos := protocolsToFetch(protocol)
	var (
		mu   sync.Mutex
		out  []string
		seen = map[string]bool{}
	)
	var wg sync.WaitGroup
	errCh := make(chan error, len(protos))
	for _, p := range protos {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			endpoint := fmt.Sprintf(
				"https://chillyproxy.com/api/tools/free-proxies?protocol=%s&country=ALL",
				url.QueryEscape(p))
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
			if err != nil {
				errCh <- err
				return
			}
			req.Header.Set("User-Agent", "noderouter/"+Version)
			resp, err := harvestClient.Do(req)
			if err != nil {
				errCh <- err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errCh <- fmt.Errorf("chillyproxy: %s", resp.Status)
				return
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			if err != nil {
				errCh <- err
				return
			}
			mu.Lock()
			for _, line := range strings.Split(string(body), "\n") {
				if proxy, ok := NormalizeProxy(line, p); ok && !seen[proxy] {
					seen[proxy] = true
					out = append(out, proxy)
				}
			}
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("chillyproxy: no usable proxies in the response")
	}
	return out, nil
}
