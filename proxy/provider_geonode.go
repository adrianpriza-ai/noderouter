package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Geonode provider: its list, sorted by response time.

const geonodeMaxLimit = 500

func init() {
	RegisterProvider(&Provider{
		Name: "geonode",
		Fetch: func(ctx context.Context, req FetchRequest, notify func(string, ...any)) ([]string, error) {
			notify("fetching geonode", "limit", req.Limit)
			return FetchGeoNode(ctx, req.Limit, req.Protocol)
		},
	})
}

// FetchGeoNode pulls the Geonode list. The API returns a null "data" payload
// when the limit exceeds its cap, hence the clamp.
func FetchGeoNode(ctx context.Context, limit int, protocol string) ([]string, error) {
	limit = clamp(limit, 1, geonodeMaxLimit)
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	endpoint := fmt.Sprintf("https://proxylist.geonode.com/api/proxy-list?page=1&limit=%d&sort_by=responseTime&sort_type=asc", limit)
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
		return nil, fmt.Errorf("geonode: %s", resp.Status)
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
	return out, nil
}
