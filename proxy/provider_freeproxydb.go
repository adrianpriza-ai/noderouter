package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// FreeProxyDB provider: the search API, one request per protocol. Handed a
// comma-separated protocol list the API only returns a single page's worth of
// entries, and the http proxies flood the top of that page — so socks4/socks5
// results were being silently starved out. One request per protocol (like
// ChillyProxy) means every requested scheme is actually represented. Its JSON
// payload carries a connect_string (with scheme), so the scheme tag survives.

func init() {
	RegisterProvider(&Provider{
		Name:    "freeproxydb",
		Aliases: []string{"free proxy db"},
		Fetch: func(ctx context.Context, req FetchRequest, notify func(string, ...any)) ([]string, error) {
			notify("fetching freeproxydb", "protocol", req.Protocol)
			return FetchFreeProxyDB(ctx, req.Protocol)
		},
	})
}

// FetchFreeProxyDB pulls the FreeProxyDB search API for each requested
// protocol and merges the results.
func FetchFreeProxyDB(ctx context.Context, protocol string) ([]string, error) {
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
				"https://freeproxydb.com/api/proxy/search?country=&protocol=%s&anonymity=&speed=0,60&https=0&page_index=1&page_size=50",
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
				errCh <- fmt.Errorf("freeproxydb: %s", resp.Status)
				return
			}
			var payload struct {
				Data struct {
					Data []struct {
						ConnectString string `json:"connect_string"`
						IP            string `json:"ip"`
						Port          int    `json:"port"`
						Protocol      string `json:"protocol"`
					} `json:"data"`
				} `json:"data"`
			}
			if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload); err != nil {
				errCh <- err
				return
			}
			mu.Lock()
			for _, item := range payload.Data.Data {
				cs := strings.TrimSpace(item.ConnectString)
				if cs == "" {
					cs = item.Protocol + "://" + item.IP + ":" + strconv.Itoa(item.Port)
				}
				if proxy, ok := NormalizeProxy(cs, item.Protocol); ok && !seen[proxy] {
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
		return nil, fmt.Errorf("freeproxydb: no usable proxies in the response")
	}
	return out, nil
}
