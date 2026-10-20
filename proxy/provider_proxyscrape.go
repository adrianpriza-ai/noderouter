package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ProxyScrape provider: the free list, filtered by response time.

const (
	proxyScrapeMaxTimeoutMS = 15000
	proxyScrapeMaxLimit     = 2000
)

func init() {
	RegisterProvider(&Provider{
		Name:    "proxyscrape",
		Aliases: []string{"proxy scrape", "scrape"},
		Fetch: func(ctx context.Context, req FetchRequest, notify func(string, ...any)) ([]string, error) {
			notify("fetching proxyscrape", "protocol", req.Protocol, "timeoutMs", req.TimeoutMS, "limit", req.Limit)
			return FetchProxyScrape(ctx, req.TimeoutMS, req.Protocol, req.Limit)
		},
	})
}

// FetchProxyScrape pulls the free list. timeoutMS is milliseconds, the unit
// the API expects; it is clamped to the documented 15000 cap.
func FetchProxyScrape(ctx context.Context, timeoutMS int, protocol string, limit int) ([]string, error) {
	timeoutMS = clamp(timeoutMS, 1, proxyScrapeMaxTimeoutMS)
	limit = clamp(limit, 1, proxyScrapeMaxLimit)
	if protocol == "" {
		protocol = "http"
	}
	endpoint := fmt.Sprintf(
		"https://api.proxyscrape.com/v4/free-proxy-list/get?request=display_proxies"+
			"&proxy_format=protocolipport&format=text&protocol=%s&timeout=%d&limit=%d",
		url.QueryEscape(protocol), timeoutMS, limit)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "noderouter/"+Version)
	resp, err := harvestClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxyscrape: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	// A "protocol=all" response is a mix of prefixed lines and bare ip:port
	// lines, so bare ones get the default scheme rather than "all".
	bare := BareScheme(protocol)
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		if p, ok := NormalizeProxy(line, bare); ok {
			out = append(out, p)
		}
	}
	return out, nil
}
