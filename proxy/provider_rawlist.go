package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Raw-list provider: any plain one-proxy-per-line list. This covers the
// SourceURLs presets (usable by name in proxy.sources) and any http(s) URL
// written there, so a new list needs no code change.

const rawListMaxProxies = 2000

// SourceURLs are known raw proxy lists, usable by name in proxy.sources.
// Any http(s) URL in proxy.sources is also fetched and parsed as a raw list,
// so new lists need no code change.
var SourceURLs = map[string]string{
	"iplocate": "https://raw.githubusercontent.com/iplocate/free-proxy-list/refs/heads/main/all-proxies.txt",
}

// SourceEndpoint resolves a source name to a raw list URL.
func SourceEndpoint(name string) (string, bool) {
	if url, ok := SourceURLs[strings.ToLower(strings.TrimSpace(name))]; ok {
		return url, true
	}
	if strings.HasPrefix(name, "http://") || strings.HasPrefix(name, "https://") {
		return name, true
	}
	return "", false
}

// rawListProvider fetches the entry itself: a preset from SourceURLs or an
// http(s) URL. resolveSource routes to it instead of registering it under a
// name, since what it fetches is whatever the user wrote.
var rawListProvider = &Provider{
	Name: "rawlist",
	Fetch: func(ctx context.Context, req FetchRequest, notify func(string, ...any)) ([]string, error) {
		endpoint, ok := SourceEndpoint(req.Source)
		if !ok {
			return nil, fmt.Errorf("%s: not a raw list source", req.Source)
		}
		notify("fetching raw list", "source", req.Source, "protocol", req.Protocol)
		return FetchRawList(ctx, endpoint, req.Protocol)
	},
}

// FetchRawList downloads a plain-text proxy list (one proxy per line) from a
// URL. Blank lines, comments, duplicates and schemes outside the protocol
// filter are dropped.
func FetchRawList(ctx context.Context, endpoint, protocol string) ([]string, error) {
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
		return nil, fmt.Errorf("%s: %s", endpoint, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		proxy, ok := NormalizeProxy(line, "http")
		if !ok || seen[proxy] {
			continue
		}
		if u, err := url.Parse(proxy); err != nil || !SchemeAllowed(u.Scheme, protocol) {
			continue
		}
		seen[proxy] = true
		out = append(out, proxy)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no usable proxies in the response", endpoint)
	}
	if len(out) > rawListMaxProxies {
		out = out[:rawListMaxProxies]
	}
	return out, nil
}
