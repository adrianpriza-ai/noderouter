package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// RoundProxies provider: the free list, most-recently-checked first, and
// only the low-uptime entries — the same query the published API docs use.

const roundProxiesMaxLimit = 1000

func init() {
	RegisterProvider(&Provider{
		Name:    "roundproxies",
		Aliases: []string{"roundproxies.com", "round proxies", "round"},
		Fetch: func(ctx context.Context, req FetchRequest, notify func(string, ...any)) ([]string, error) {
			notify("fetching roundproxies", "limit", req.Limit)
			return FetchRoundProxies(ctx, req.Limit, req.Protocol)
		},
	})
}

// FetchRoundProxies pulls the RoundProxies free list. The API mirrors the
// Geonode shape: a "data" array of {ip, port, protocols[]}. A bounded limit
// keeps the payload small; uptime=low matches the query the docs link to.
func FetchRoundProxies(ctx context.Context, limit int, protocol string) ([]string, error) {
	limit = clamp(limit, 1, roundProxiesMaxLimit)
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	endpoint := fmt.Sprintf("https://roundproxies.com/api/get-free-proxies/?limit=%d&page=1&sort_by=lastChecked&sort_type=desc&uptime=low", limit)
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
		return nil, fmt.Errorf("roundproxies: %s", resp.Status)
	}
	var payload struct {
		Data []struct {
			IP        string   `json:"ip"`
			Port      string   `json:"port"`
			Protocols []string `json:"protocols"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	var out []string
	for _, item := range payload.Data {
		chosen := "http"
		for _, p := range item.Protocols {
			if supportedProtocols[strings.ToLower(p)] {
				chosen = strings.ToLower(p)
				break
			}
		}
		if protocol != "" && protocol != "all" && protocol != "any" && chosen != protocol {
			continue
		}
		if p, ok := NormalizeProxy(chosen+"://"+item.IP+":"+item.Port, chosen); ok {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("roundproxies: no usable proxies in the response")
	}
	return out, nil
}
