package router

// Endpoint health. A provider with several upstream endpoints is only worth
// having as long as one of them answers, and a pool that keeps spending every
// third request on a dead mirror is a pool that has stopped helping. So
// failures that are the endpoint's own fault - a connection that never reached
// it, with no proxy in the way - are counted, and an endpoint that fails too
// often in a row is benched: skipped when a request picks an endpoint, until a
// probe or the cooldown brings it back.
//
// Nothing here counts a provider's own answer. A 429 or a 500 is an endpoint
// saying something; only silence is a reason to bench it. And when every
// endpoint of a provider is benched the pool is handed back whole, because
// trying is the only way to learn that one came back, while refusing the
// request can never serve it at all.

import (
	"context"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"noderouter/config"
	"noderouter/proxy"
)

// probeTimeout bounds one health probe. It is deliberately short: the probe
// only has to tell "reachable" from "not", and a benched endpoint that hangs
// is a benched endpoint either way.
const probeTimeout = 10 * time.Second

// healthTarget is one benched endpoint, named with the provider it belongs to.
type healthTarget struct {
	Provider string
	Endpoint string
}

// endpointHealth tracks every provider's endpoints between requests. The
// fields are settled once, at startup; only the state map is mutable.
type endpointHealth struct {
	threshold int           // consecutive failures before benching; 0 = benching off
	bench     time.Duration // how long one bench lasts

	mu    sync.Mutex
	state map[string]*endpointMark
}

type endpointMark struct {
	fails        int
	benchedUntil time.Time
}

func newEndpointHealth(threshold int, bench time.Duration) *endpointHealth {
	return &endpointHealth{threshold: threshold, bench: bench, state: map[string]*endpointMark{}}
}

func endpointKey(provider, endpoint string) string { return provider + "\x00" + endpoint }

// Failure counts one attempt that never reached the endpoint. The threshold
// consecutive ones bench it, and the counter then starts again from zero, so
// the next bench is earned by the next run of failures rather than by a
// failure a week old.
func (h *endpointHealth) Failure(provider, endpoint string) {
	if h == nil || h.threshold <= 0 || endpoint == "" {
		return
	}
	key := endpointKey(provider, endpoint)
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.state[key]
	if m == nil {
		m = &endpointMark{}
		h.state[key] = m
	}
	m.fails++
	if m.fails >= h.threshold {
		m.fails = 0
		m.benchedUntil = time.Now().Add(h.bench)
	}
}

// Success clears whatever was recorded against the endpoint: it answered, so
// the bench is lifted at once instead of waiting out the cooldown.
func (h *endpointHealth) Success(provider, endpoint string) {
	if h == nil || endpoint == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.state, endpointKey(provider, endpoint))
}

// Filter drops the benched endpoints from pool, returning the pool unchanged
// when benching is off or when the whole pool is benched. The cooldown is
// checked here rather than in a timer, so an endpoint nobody probed comes back
// on its own when the bench runs out.
func (h *endpointHealth) Filter(provider string, pool []string) []string {
	if h == nil || h.threshold <= 0 {
		return pool
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	out := make([]string, 0, len(pool))
	for _, e := range pool {
		if m := h.state[endpointKey(provider, e)]; m != nil && now.Before(m.benchedUntil) {
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return pool
	}
	return out
}

// Benched lists the endpoints currently out of rotation, sorted so status
// output and the probe pass see the same order every time. Marks whose
// cooldown has run out are swept while the map is walked: they are no longer
// benching anything, and the next success or failure starts them fresh.
func (h *endpointHealth) Benched() []healthTarget {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	var out []healthTarget
	for key, m := range h.state {
		if !now.Before(m.benchedUntil) {
			delete(h.state, key)
			continue
		}
		provider, endpoint, _ := strings.Cut(key, "\x00")
		out = append(out, healthTarget{Provider: provider, Endpoint: endpoint})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Endpoint < out[j].Endpoint
	})
	return out
}

// BenchedEndpoints lists one provider's benched endpoints, for its row in
// /noderouter/status.
func (h *endpointHealth) BenchedEndpoints(provider string) []string {
	var out []string
	for _, t := range h.Benched() {
		if t.Provider == provider {
			out = append(out, t.Endpoint)
		}
	}
	return out
}

// RecheckEndpoints probes every benched endpoint once and lifts the bench of
// the ones that answer, returning how many came back. The probe goes direct -
// no proxy pool - because a flaky proxy must not be what keeps a healthy
// endpoint out of rotation, and it asks for GET <base>/models: every
// OpenAI-compatible upstream answers that path, and any answer at all (a 401
// or a 404 included) says the endpoint is reachable again. Only a request
// that never got an answer keeps it benched.
func (s *Server) RecheckEndpoints(ctx context.Context) int {
	targets := s.endpoints.Benched()
	restored := 0
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		ref := s.cfg.ProviderByName(t.Provider)
		if ref == nil {
			// The provider is no longer configured; its mark goes with it.
			s.endpoints.Success(t.Provider, t.Endpoint)
			continue
		}
		if !s.probeEndpoint(ctx, ref.Provider, t.Endpoint) {
			continue
		}
		s.endpoints.Success(t.Provider, t.Endpoint)
		restored++
		s.log.Debug("endpoint back in the pool", "provider", t.Provider, "endpoint", t.Endpoint)
	}
	return restored
}

// probeEndpoint asks one endpoint whether it is back. The credential is the
// provider's own, as a forwarded request would carry it: a probe that arrived
// unauthenticated would be answered with a 401 on gateways that have one -
// still an answer, but a less informative one.
func (s *Server) probeEndpoint(ctx context.Context, provider *config.Provider, baseURL string) bool {
	target, err := ResolveUpstream(baseURL, "/models", "")
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), http.NoBody)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/json")
	applyAuth(req, provider, 0, s.cfg.Auth)
	tr := proxy.NewTransport(nil, probeTimeout, 0, provider.Options.SkipTLSVerify)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	return true
}

// pickEndpoint chooses the upstream endpoint for one attempt: the provider's
// resolved pool, minus whatever is currently benched, handed to the provider's
// own strategy. "" means the provider has nowhere to send the request, which
// the caller refuses on.
func (s *Server) pickEndpoint(provider *config.Provider, name string, seq, attempt int) string {
	pool := provider.EndpointPool()
	if len(pool) == 0 {
		return ""
	}
	return provider.Options.PickEndpoint(s.endpoints.Filter(name, pool), seq, attempt)
}
