package router

import (
	"bytes"
	"context"
	cryptoRand "crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"noderouter/config"
	"noderouter/logx"
	"noderouter/proxy"
)

// Version is the router version reported by the index endpoint. main sets it
// from the build; the default matches a dev build.
var Version = "dev"

// Server is the router: it exposes an OpenAI-compatible surface on a local
// port, resolves the requested model to a provider from the config, and sends
// the request upstream through one of the proxies in the pool.
type Server struct {
	cfg      *config.Config
	pool     *proxy.Pool
	log      *logx.Logger
	started  time.Time
	created  int64 // model card timestamp, set once at startup
	requests atomic.Int64
	errors   atomic.Int64
	retries  atomic.Int64
	proxied  atomic.Int64
	direct   atomic.Int64
	lastErr  atomic.Value // string

	// NextRefreshUnix reports when the proxy list is refreshed again, for status.
	NextRefreshUnix atomic.Int64
	// NextRecheckUnix reports when the proxy pool is re-checked again, for status.
	NextRecheckUnix atomic.Int64
	warnedInsecure  atomic.Bool
	OnRefresh       func(context.Context) error
	RefreshMu       sync.Mutex

	// bodies is the hard backstop on buffered request bytes in flight; see
	// memory.go.
	bodies *BodyBudget

	// endpoints tracks which upstream endpoints are answering, so a dead one
	// is benched out of the pool instead of being retried on every Nth
	// request; see endpoints.go.
	endpoints *endpointHealth

	// Status is the public request-success/failure log with 7-day retention.
	// Read it through statusTracker rather than assigned on demand: a lazy
	// write here would race the request goroutines that read it.
	Status     *StatusTracker
	statusOnce sync.Once

	// rr gives each unified-model request the next starting endpoint, so
	// load spreads across the group round-robin.
	rr atomic.Int64

	// dynamicAdminToken is a randomly generated token used when cfg.Auth.AdminToken
	// is empty. It provides a default security barrier for /noderouter/* endpoints.
	dynamicAdminToken string

	// forwardHosts is the set of hostnames the router answers forwarded
	// requests for, resolved from the config once at startup; see
	// hostRefusal.
	forwardHosts map[string]bool
}

// DynamicAdminToken returns the dynamically generated admin token, or empty string if none was generated.
// This is primarily used for testing.
func (s *Server) DynamicAdminToken() string {
	return s.dynamicAdminToken
}

// generateDynamicToken creates a cryptographically random token for admin endpoints.
// It generates 18 random bytes and encodes them as base64, resulting in a 24-character token
// with a "noro_" prefix for identification.
func generateDynamicToken() (string, error) {
	bytes := make([]byte, 18)
	if _, err := cryptoRand.Read(bytes); err != nil {
		return "", fmt.Errorf("failed to generate random token: %w", err)
	}
	return "noro_" + base64.URLEncoding.EncodeToString(bytes), nil
}

func NewServer(cfg *config.Config, pool *proxy.Pool, log *logx.Logger) *Server {
	s := &Server{cfg: cfg, pool: pool, log: log, started: time.Now(), created: time.Now().Unix(), bodies: NewBodyBudget(cfg.Server.BodyBudgetMB),
		endpoints: newEndpointHealth(cfg.Endpoints.Threshold(), cfg.Endpoints.Bench()), Status: NewStatusTracker(7 * 24 * time.Hour)}
	s.lastErr.Store("")
	s.forwardHosts = allowedHostSet(cfg)

	// Generate a dynamic admin token if none is configured
	if cfg.Auth.AdminToken == "" {
		token, err := generateDynamicToken()
		if err != nil {
			log.Error("failed to generate dynamic admin token", "error", err)
			log.Warn("/noderouter/* admin routes are locked out until the router restarts with a working token generator; set auth.adminToken in the config to enable them now")
		} else {
			s.dynamicAdminToken = token
			log.Info("admin token", "token", token, "note", "use this token for /noderouter/* endpoints (set auth.adminToken in config to use a fixed token)")
		}
	}

	// Say so when a model id had to be renamed to keep /v1/models unique, so
	// the ids clients see are never a surprise.
	for _, note := range cfg.ModelIDNotes() {
		log.Warn("model id: " + note)
	}
	return s
}

type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	_ = f.rc.Flush()
	return n, err
}

// hop-by-hop headers we never copy upstream or back down.
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func removeHopHeaders(h http.Header) {
	for _, key := range hopHeaders {
		h.Del(key)
	}
}

// Handler builds the mux: local endpoints first, everything else is proxied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /status", s.handleStatusPage)
	mux.HandleFunc("GET /status/", s.handleStatusPage)
	// The public status feed is JSON; a browser that navigated here gets the
	// status website instead (see wantsPage), the same split as /v1/models.
	mux.HandleFunc("GET /v1/status", s.serveStatus)
	mux.HandleFunc("GET /v1/status/{$}", s.serveStatus)
	// The root is JSON (the quick index); a browser that navigated here gets
	// the landing page instead, the same split as /v1/models (see wantsPage).
	mux.HandleFunc("GET /{$}", s.serveHome)
	// The model list is JSON; a browser that navigated here gets the
	// website instead (see wantsPage).
	mux.HandleFunc("GET /v1/models", s.serveModels)
	mux.HandleFunc("GET /models", s.serveModels)
	// The embedded site, grouped by type of asset: stylesheets under /css,
	// scripts (including the markdown renderer) under /js, images under
	// /img, fonts and favicons under their own folders.
	mux.HandleFunc("GET /css/{file...}", s.serveSiteAsset)
	mux.HandleFunc("GET /js/{file...}", s.serveSiteAsset)
	mux.HandleFunc("GET /img/{file...}", s.serveSiteAsset)
	mux.HandleFunc("GET /fonts/{file...}", s.serveSiteAsset)
	mux.HandleFunc("GET /favicon/{file...}", s.serveSiteAsset)
	// The guides the models site links to from its footer. /docs is registered
	// as well as /docs/{file...}: the wildcard alone leaves the bare path to
	// the mux, which sends it to /docs/ with a 307 before the handler sees it.
	mux.HandleFunc("GET /docs", s.serveDocs)
	mux.HandleFunc("GET /docs/{file...}", s.serveDocs)
	// The favicon every browser asks for at the root. Without this
	// route the request falls through to the proxy.
	mux.HandleFunc("GET /favicon.ico", s.serveSiteRootFile)
	mux.HandleFunc("GET /README.md", s.serveDocs)
	// The wildcard, not {id}: an advertised id may be namespaced
	// ("zen/gpt-4o"), and a client that picked that id out of the list has to
	// be able to ask for it.
	mux.HandleFunc("GET /v1/models/{id...}", s.handleModel)
	mux.HandleFunc("GET /noderouter/status", s.admin(s.handleStatus))
	mux.HandleFunc("GET /noderouter/routes", s.admin(s.handleRoutes))
	mux.HandleFunc("POST /noderouter/refresh", s.admin(s.handleRefresh))
	mux.HandleFunc("POST /noderouter/reload", s.admin(s.handleReload))
	mux.HandleFunc("/", s.handleForward)
	return s.withStatusTracking(withRecovery(s.log, refuseTraversal(mux)))
}

// refuseTraversal turns away a request whose path walks up a level, before the
// mux gets to answer it. http.ServeMux replies to a path that is not canonical
// with a 301 to the cleaned one, which sends "/docs/../nosuch.md" out of /docs
// and on to the catch-all passthrough - the opposite of refusing it, and a
// redirect a client would follow on its own. A 404 keeps the answer local and
// is what the docs handler would have said anyway.
//
// Only a whole ".." segment is refused: a model id or a file name that merely
// contains two dots is an ordinary path, and a redundant "." segment or a
// doubled slash still gets the mux's own redirect.
func refuseTraversal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, seg := range strings.Split(r.URL.Path, "/") {
			if seg == ".." {
				http.NotFound(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// admin guards /noderouter/* endpoints. It requires either the admin token -
// the configured auth.adminToken, or the one generated at startup when it is
// unset - or any key from the client api key pool, so an operator who only
// configured auth.apiKeys does not need a second credential to check status.
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	configToken := s.cfg.Auth.AdminToken
	return func(w http.ResponseWriter, r *http.Request) {
		// Determine which token to validate
		var expectedToken string
		if configToken != "" {
			expectedToken = configToken
		} else {
			expectedToken = s.dynamicAdminToken
		}

		// Fail closed: an empty expected token admits nobody. This state is
		// only reachable when the startup token generation failed and no
		// auth.adminToken is configured; without this guard an absent
		// X-Noderouter-Token header compares equal to "" and every admin
		// route would be served with no credential at all.
		if expectedToken == "" {
			writeJSONError(w, http.StatusUnauthorized, "admin token required")
			return
		}

		// The admin token still answers in its two header formats. Past that,
		// any key from auth.apiKeys works here too, in the same three
		// spellings a forwarded request accepts: one credential, both hops.
		// A pool that resolved to no keys matches nothing, so a broken key
		// file leaves the admin token working and admits nobody else.
		keys, _ := s.cfg.Auth.ClientKeys()
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Noderouter-Token")), []byte(expectedToken)) != 1 &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+expectedToken)) != 1 &&
			!presentedKeyMatches(r, keys) {
			writeJSONError(w, http.StatusUnauthorized, "admin token required")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"uptime":  time.Since(s.started).Round(time.Second).String(),
		"proxies": s.pool.Len(),
	})
}

type modelCard struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	Created     int64  `json:"created"`
	OwnedBy     string `json:"owned_by"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// Kind is the model's primary category (chat, image, image_edit, systemone,
	// embedding, audio, tts, stt, ocr, moderation, video), so a client knows
	// which endpoint answers first without guessing from the id.
	Kind string `json:"kind,omitempty"`
	// Kinds lists the complete capability set when a model has more than one
	// capability. Kind remains the primary legacy field.
	Kinds []string `json:"kinds,omitempty"`
	// Endpoint is the path a client POSTs this model to: /v1/ocr for an OCR
	// model, /v1/videos/generations for a video model, /v1/moderations for a
	// moderation model, /v1/audio/speech for a tts model,
	// /v1/audio/transcriptions for a whisper-style stt model, /v1/messages on
	// an Anthropic-dialect provider and so on. It is the router's own answer
	// to "where does this go", so a client - the models site included - does
	// not have to keep a second kind-to-endpoint table of its own.
	Endpoint string `json:"endpoint,omitempty"`
	// API is the provider's upstream dialect: "openai" or "anthropic". It is
	// only set when it is not the default, so the common case stays quiet.
	API string `json:"api,omitempty"`
	// Surface is the upstream wire format: "chat" (the default) or
	// "responses". It is only set when it is not the default, so a card for a
	// chat provider stays quiet. When it is "responses" the router accepts
	// /v1/chat/completions for this model too, translated.
	Surface string `json:"surface,omitempty"`
	// Accepts lists every endpoint this model answers on from the router's own
	// address, including the single endpoint of a specialist model, so a client
	// does not have to infer routing from kind.
	Accepts   []string `json:"accepts,omitempty"`
	Backends  []string `json:"backends,omitempty"`
	Context   int      `json:"context_length,omitempty"`
	MaxOutput int      `json:"max_output_tokens,omitempty"`
	Input     []string `json:"input_modalities,omitempty"`
	Output    []string `json:"output_modalities,omitempty"`
	Limit     struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
	Modalities struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
}

// endpointForKind maps a model's kind onto the endpoint that answers for it.
// The chat kind is deliberately absent: a chat model answers on
// /v1/chat/completions, on /v1/responses and - on an Anthropic-dialect
// provider - on /v1/messages, which cardForProvider settles from the
// provider's dialect and surface. Every path here is one handleForward
// forwards untouched, and the same one the models site's KINDS table uses.
func endpointForKind(kind string) string {
	switch kind {
	case config.KindImage:
		return "/v1/images/generations"
	case config.KindImageEdit:
		return "/v1/images/edits"
	case config.KindSystemOne:
		return "/v1/systemone"
	case config.KindEmbedding:
		return "/v1/embeddings"
	case config.KindAudio, config.KindTTS:
		return "/v1/audio/speech"
	case config.KindSTT:
		return "/v1/audio/transcriptions"
	case config.KindOCR:
		return "/v1/ocr"
	case config.KindModeration:
		return "/v1/moderations"
	case config.KindVideo:
		return "/v1/videos/generations"
	default:
		return ""
	}
}

func endpointsForKinds(kinds []string) []string {
	var endpoints []string
	for _, kind := range kinds {
		endpoint := endpointForKind(kind)
		if endpoint == "" {
			continue
		}
		seen := false
		for _, existing := range endpoints {
			if existing == endpoint {
				seen = true
				break
			}
		}
		if !seen {
			endpoints = append(endpoints, endpoint)
		}
	}
	return endpoints
}

// cardForProvider builds the card, adding the surface and the endpoint list
// when the provider has one. opts carries the provider so the card can say
// whether this model is also reachable through the other surface.
func (s *Server) cardForProvider(id, provider string, m *config.Model, api string, opts *config.ProviderOptions) modelCard {
	dialect := api // kept before the blanking below: the dialect decides where a chat model answers
	if api == "openai" {
		api = "" // the default; leave it out of the card
	}
	card := modelCard{
		ID: id, Object: "model", Created: s.created, OwnedBy: provider,
		Name: m.Name, Description: m.Description, Kind: m.ResolveKind(), API: api,
		Context: m.Limit.Context, MaxOutput: m.Limit.Output,
		Input: m.Modalities.Input, Output: m.Modalities.Output,
		Limit: struct {
			Context int `json:"context"`
			Output  int `json:"output"`
		}{Context: m.Limit.Context, Output: m.Limit.Output},
		Modalities: struct {
			Input  []string `json:"input"`
			Output []string `json:"output"`
		}{Input: m.Modalities.Input, Output: m.Modalities.Output},
	}
	resolvedKinds := m.ResolveKinds()
	if len(resolvedKinds) > 1 {
		card.Kinds = resolvedKinds
	}
	if opts != nil {
		if surface := opts.Surface(); surface != config.SurfaceChat {
			card.Surface = surface
		}
	}
	// Every model lists every router endpoint it answers on. Chat models get
	// both OpenAI wire formats because the router translates between them;
	// Anthropic chat models answer on the Messages endpoint natively and on
	// /v1/chat/completions through the translator. Specialist endpoints are
	// passed through unchanged.
	accepted := endpointsForKinds(resolvedKinds)
	for _, kind := range resolvedKinds {
		if kind != config.KindChat {
			continue
		}
		if dialect == "anthropic" {
			accepted = append(accepted, "/v1/messages", "/v1/chat/completions")
		} else {
			accepted = append(accepted, "/v1/chat/completions", "/v1/responses")
		}
		break
	}
	if len(accepted) > 0 {
		seen := make(map[string]bool, len(accepted))
		for _, endpoint := range accepted {
			if seen[endpoint] {
				continue
			}
			seen[endpoint] = true
			card.Accepts = append(card.Accepts, endpoint)
		}
	}
	// Where a client POSTs this model. The kind decides it for every
	// specialist kind; chat is decided by the dialect and the surface, since a
	// chat model answers on /v1/chat/completions, on /v1/responses (translated
	// when the provider is native to only one) or on /v1/messages for the
	// Anthropic dialect.
	switch {
	case endpointForKind(card.Kind) != "":
		card.Endpoint = endpointForKind(card.Kind)
	case dialect == "anthropic":
		card.Endpoint = "/v1/messages"
	case card.Surface == config.SurfaceResponses:
		card.Endpoint = "/v1/responses"
	default:
		card.Endpoint = "/v1/chat/completions"
	}
	return card
}

func sortCards(cards []modelCard) {
	sort.Slice(cards, func(i, j int) bool { return cards[i].ID < cards[j].ID })
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	data := []modelCard{}
	for _, e := range s.cfg.ModelTable() {
		card := s.cardForProvider(e.Call, e.Provider.Name, e.Model,
			e.Provider.Provider.Options.Dialect(), &e.Provider.Provider.Options)
		// Backends ride along on the row for group entries; no per-entry
		// ModelBackends pass over the table (that was O(n^2) per request).
		if len(e.Backends) > 0 {
			names := make([]string, 0, len(e.Backends))
			for _, b := range e.Backends {
				names = append(names, b.Provider.Name)
			}
			card.Backends = names
		}
		data = append(data, card)
	}
	sortCards(data)
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(r.PathValue("id"), "/")
	if id == "" {
		writeJSONError(w, http.StatusNotFound, "no model id")
		return
	}
	ref, m, err := s.cfg.ModelCard(id)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	card := s.cardForProvider(id, ref.Name, m,
		ref.Provider.Options.Dialect(), &ref.Provider.Options)
	if b := s.cfg.ModelBackends(id); len(b) > 0 {
		card.Backends = b
	}
	writeJSON(w, http.StatusOK, card)
}

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	type route struct {
		Model    string `json:"model"`
		Config   string `json:"configId,omitempty"`
		Provider string `json:"provider"`
		Upstream string `json:"upstreamModel"`
	}
	routes := []route{}
	for _, e := range s.cfg.ModelTable() {
		if len(e.Backends) > 0 {
			// A unified group lists each endpoint that serves it.
			for _, b := range e.Backends {
				routes = append(routes, route{Model: e.Call, Provider: b.Provider.Name, Upstream: b.Upstream})
			}
			continue
		}
		r := route{Model: e.Call, Provider: e.Provider.Name, Upstream: e.Upstream}
		if e.Call != e.Key {
			r.Config = e.Key
		}
		routes = append(routes, r)
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Model < routes[j].Model })
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": routes})
}

// handleStatus answers GET /noderouter/status: operator-facing runtime
// counters and state for this process.
//
// Its "requests" and the public status page's "total" are two different
// counts on purpose, and will not agree. "requests" is every request that
// reached handleForward, on any path, for the lifetime of the process: /v1/*
// API calls, the non-/v1 alias forwards, everything. It also seeds each
// request's keySeq, so it is taken at the door, before auth or routing can
// reject the request. The status page (StatusTracker) instead counts a
// windowed, outcome-based sample: only routes under /v1/, minus the local
// /v1/models and /v1/status, retained for 7 days. So this counter is a
// process-workload total and that one is an API-health rate; "requests" >=
// status "total" is the normal state, not a bug.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	last, _ := s.lastErr.Load().(string)
	status := map[string]any{
		"uptime":    time.Since(s.started).Round(time.Second).String(),
		"config":    s.cfg.Path(),
		"listen":    fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port),
		"requests":  s.requests.Load(),
		"errors":    s.errors.Load(),
		"retries":   s.retries.Load(),
		"viaProxy":  s.proxied.Load(),
		"direct":    s.direct.Load(),
		"lastError": last,
		"proxy":     s.pool.Stats(),
		"providers": s.providerSummary(),
		"endpoints": s.endpointStatus(),
		"refresh":   s.cfg.Proxy.Refresh,
		"retry":     s.retryStatus(),
		"memory":    s.memoryStatus(),
	}
	if next := s.NextRefreshUnix.Load(); next > 0 {
		status["nextRefresh"] = time.Unix(next, 0).Format(time.RFC3339)
	}
	if next := s.NextRecheckUnix.Load(); next > 0 {
		status["nextRecheck"] = time.Unix(next, 0).Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, status)
}

// retryStatus reports the retry policy in force, so a request that took ten
// tries can be told apart from one that took three because the config said so.
func (s *Server) retryStatus() map[string]any {
	retry := &s.cfg.Retry
	return map[string]any{
		"maxAttempts":  retry.MaxAttempts,
		"backoffMs":    retry.BackoffMS,
		"maxBackoffMs": retry.MaxBackoffMS,
		"statuses":     sortedStatuses(retry),
		"totalSecs":    retry.TotalSecs,
	}
}

// sortedStatuses lists the statuses currently worth another try.
func sortedStatuses(retry *config.RetryConfig) []int {
	set := map[int]bool{}
	for code := range config.DefaultRetryStatuses {
		set[code] = true
	}
	for _, code := range retry.Statuses {
		set[code] = true
	}
	for _, code := range retry.SkipStatuses {
		delete(set, code)
	}
	out := make([]int, 0, len(set))
	for code := range set {
		out = append(out, code)
	}
	sort.Ints(out)
	return out
}

// memoryStatus reports the soft GC limit and the hard body budget for the
// status endpoint.
func (s *Server) memoryStatus() map[string]any {
	budgetLimit, inFlight, budgetOn := s.bodies.Status()
	out := map[string]any{
		"gcLimitMb": s.cfg.Server.MemoryLimitMB,
		"source":    "config",
	}
	if os.Getenv("GOMEMLIMIT") != "" {
		out["source"] = "GOMEMLIMIT"
	}
	if budgetOn {
		out["bodyBudgetMb"] = budgetLimit
		out["bodiesInFlightMb"] = inFlight
	} else {
		out["bodyBudgetMb"] = nil
	}
	return out
}

// endpointStatus reports the endpoint health policy in force and how many
// endpoints are out of rotation right now, so a provider that is running on
// its spares is visible without reading the config back.
func (s *Server) endpointStatus() map[string]any {
	return map[string]any{
		"failThreshold": s.endpoints.threshold, // 0 = benching off
		"benchSecs":     int(s.endpoints.bench / time.Second),
		"recheckSecs":   int(s.cfg.Endpoints.RecheckEvery() / time.Second), // 0 = no probes
		"benched":       len(s.endpoints.Benched()),
	}
}

func (s *Server) providerSummary() []map[string]any {
	out := make([]map[string]any, 0)
	for _, ref := range s.cfg.Providers() {
		if ref.Provider.Hidden {
			continue
		}
		row := map[string]any{
			"name":     ref.Name,
			"models":   len(ref.Provider.Models),
			"useProxy": ref.Provider.Options.ProviderUsesProxy(),
			"surface":  ref.Provider.Options.Surface(),
		}
		// baseURL is the endpoint a request starts on; a pool also reports
		// every endpoint and the strategy picking between them, so the status
		// answer says what is in force without reading the config back.
		if pool := ref.Provider.EndpointPool(); len(pool) > 0 {
			row["baseURL"] = pool[0]
			if len(pool) > 1 {
				row["baseURLs"] = pool
				row["endpointStrategy"] = ref.Provider.Options.EndpointStrategy
			}
		} else {
			row["baseURL"] = ref.Provider.Options.BaseURL
		}
		if ref.Provider.Options.Translates() {
			// Worth saying out loud: this provider only speaks one wire, so a
			// client on the other one is being translated. An Anthropic
			// provider speaks the Messages API; the rest speak one of the two
			// OpenAI surfaces.
			if ref.Provider.Options.Dialect() == "anthropic" {
				row["translates"] = []string{"chat", "anthropic"}
			} else {
				row["translates"] = []string{"chat", "responses"}
			}
		}
		if ref.Provider.MediaCapable() {
			row["mediaModels"] = true
		}
		if benched := s.endpoints.BenchedEndpoints(ref.Name); len(benched) > 0 {
			row["benchedEndpoints"] = benched
		}
		out = append(out, row)
	}
	return out
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	out := map[string]any{"proxies": s.pool.Len()}
	// "refresh now" means everything worth refreshing: the benched endpoints
	// are probed too, so an upstream that came back is back in the pool by// the time this returns.
	if restored := s.RecheckEndpoints(ctx); restored > 0 {
		out["endpointsRestored"] = restored
	}
	if s.OnRefresh == nil {
		if len(out) == 1 {
			writeJSONError(w, http.StatusNotImplemented, "proxy refresh is disabled in this build")
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if err := s.OnRefresh(ctx); err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	out["proxies"] = s.pool.Len()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	n, err := s.pool.Reload()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proxies": n})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "noderouter " + Version,
		"listen":  fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port),
		"config":  s.cfg.Path(),
		"proxies": s.pool.Len(),
		"routes": map[string]string{
			"GET  /v1/models":               "list configured models (the models website in a browser)",
			"GET  /models":                  "same list, or the models website in a browser",
			"POST /noderouter/refresh":      "fetch, test and save proxies now",
			"POST /noderouter/reload":       "re-read the proxy list file",
			"GET  /noderouter/status":       "runtime counters and pool state",
			"POST /v1/chat/completions":     "chat models",
			"POST /v1/responses":            "OpenAI Responses API (translated when the provider is not native)",
			"POST /v1/images/generations":   "image models (kind: image)",
			"POST /v1/images/edits":         "image editing models (kind: image_edit)",
			"POST /v1/systemone":            "System One models (kind: systemone)",
			"POST /v1/messages":             "Anthropic-dialect providers (chat-completions clients are translated onto it)",
			"POST /v1/ocr":                  "OCR models (kind: ocr)",
			"POST /v1/moderations":          "moderation models (kind: moderation)",
			"POST /v1/videos/generations":   "video models (kind: video)",
			"POST /v1/audio/speech":         "text-to-speech models (kind: tts or audio)",
			"POST /v1/audio/transcriptions": "speech-to-text models (kind: stt, whisper-style multipart upload)",
			"*":                             "forwarded to the provider for the requested model",
		},
	})
}

// handleForward is the whole point: resolve the model, then send the request
// upstream, retrying across proxies when the hop fails.
func (s *Server) handleForward(w http.ResponseWriter, r *http.Request) {
	// One number per request, taken from an Add at the door: it is what
	// /noderouter/status reports as "requests", and it is this request's
	// starting place in the endpoint and key pools below (keySeq). Taking it
	// here, rather than loading a shared counter later, means two concurrent
	// requests can never draw the same starting point. "requests" therefore
	// counts every forwarded path for the life of the process, while the
	// public status page's "total" is the narrower /v1/* window - see
	// handleStatus.
	keySeq := int(s.requests.Add(1) - 1)

	// Check the client's api key before buffering anything. With neither
	// auth.token nor auth.apiKeys configured there is no key to check, so
	// any bearer - or no header at all - is accepted; that is the
	// zero-config default a loopback router runs on.
	if keys, required := s.cfg.Auth.ClientKeys(); required && !presentedKeyMatches(r, keys) {
		s.errors.Add(1)
		writeJSONError(w, http.StatusUnauthorized, "invalid api key")
		return
	}

	// The browser's own mark on the request. A page on another origin sends
	// Origin, and a "simple" one - POST, JSON labelled text/plain, no
	// credentials - is delivered by the browser without a CORS preflight
	// even though the response stays unreadable; with no key gate above,
	// that would let any web page the operator visits drive the router to
	// present the real provider key upstream. The origin is compared against
	// Host, which is on the browser's forbidden-header list - the page
	// driving the request cannot write it. X-Forwarded-Host is deliberately
	// NOT consulted: a page can set that header to whatever the check wants
	// to see and walk straight back in, so a front proxy has to pass the
	// original Host through, as the shipped nginx example does. Non-browser
	// clients send no Origin and no fetch metadata and pass untouched.
	if reason := crossSiteRefusal(r); reason != "" {
		s.errors.Add(1)
		s.log.Warn("request refused: cross-site", "reason", reason, "host", r.Host)
		writeJSONError(w, http.StatusForbidden, reason)
		return
	}

	// The host the request was addressed to. The Origin check above is only
	// as strong as the name in Host, and DNS rebinding is how a page defeats
	// that: the attacker points their own domain at the router, so the
	// browser's Origin matches Host and the request above sails through -
	// same-origin, response readable. What the attacker cannot do is write
	// server.allowedHosts, so a host the operator never declared is turned
	// away here. Host stays the only input consulted, as above: the page
	// cannot forge it, while X-Forwarded-Host it can.
	if reason := s.hostRefusal(r); reason != "" {
		s.errors.Add(1)
		s.log.Warn("request refused: unknown host", "reason", reason, "host", r.Host)
		writeJSONError(w, http.StatusForbidden, reason)
		return
	}

	// The read ceiling for this request. A body is read against the media ceiling
	// whenever any configured provider serves a model that takes media, because
	// only the body says which model was asked for; a body that turns out not to
	// be media is checked against the ordinary ceiling below. Reading a little
	// more than a prompt to find that out costs nothing, since the budget below
	// is what actually bounds memory.
	readLimitMB := s.cfg.Server.MaxBodyMB
	if s.cfg.Server.MaxMediaBodyMB > readLimitMB && s.mediaCapable() {
		readLimitMB = s.cfg.Server.MaxMediaBodyMB
	}
	limit := int64(readLimitMB) << 20
	// One claim on the body budget, held for as long as the buffered bytes are
	// alive. It is taken before the read (so a declared body is accounted the
	// moment it is admitted) and grown by the read itself (so a chunked body,
	// which declares no length, is accounted as its bytes arrive rather than
	// after they are already in RAM). The deferred release is the catch-all
	// for every exit; the early release at the finishTr exits below fires when
	// the buffered body can no longer be replayed, so a long streaming
	// completion does not keep unrelated uploads refused for its whole
	// duration.
	claim := NewReservation(s.bodies)
	defer claim.release()
	if r.ContentLength > 0 && r.ContentLength <= limit {
		// A declared length inside the ceiling is what the body will occupy,
		// so claim it before a byte is read: a client that announces 30MB and
		// then stalls is refused instead of pinning the budget while it does.
		if !claim.reserve(r.ContentLength) {
			s.log.Warn("request refused: body budget full", "declared", r.ContentLength)
			s.handleBudgetExceeded(w, s.bodies)
			return
		}
	}
	// A chunked body, or one declared past the ceiling, is charged by the read
	// itself (see readBody). Claiming the ceiling up front would be a way for
	// a client that merely *says* it will send 30MB to eat the whole budget
	// while sending nothing.
	body, err := s.readBody(r, readLimitMB, claim)
	if err != nil {
		// Only a body that genuinely blew the ceiling is a 413. A truncated
		// upload, a dropped connection or a cancelled request is a read
		// failure, and blaming its size would be a lie.
		var tooLarge bodyTooLargeError
		var full bodyBudgetFullError
		switch {
		case errors.As(err, &full):
			// The budget ran out mid-read, so the bytes are dropped where they
			// stand rather than buffered to the end. handleBudgetExceeded
			// counts the error itself, like every other refusal.
			s.log.Warn("request refused: body budget full mid-read", "held", claim.Held())
			s.handleBudgetExceeded(w, s.bodies)
		case errors.As(err, &tooLarge):
			s.errors.Add(1)
			writeJSONError(w, http.StatusRequestEntityTooLarge, err.Error())
		default:
			s.errors.Add(1)
			writeJSONError(w, http.StatusBadRequest, "could not read request body: "+err.Error())
		}
		return
	}
	// A body read against the media ceiling that turns out not to be media is
	// still held to the ordinary one. Nothing has been forwarded yet, so this
	// is the cheap place to say no.
	model := peekModel(body, r.Header.Get("Content-Type"))
	if int64(len(body)) > int64(s.cfg.Server.MaxBodyMB)<<20 && !s.mediaModelName(model) {
		s.errors.Add(1)
		writeJSONError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("request body over %dMB", s.cfg.Server.MaxBodyMB))
		return
	}
	// The read grew the claim to exactly what it buffered, so the only
	// correction left is a client that declared more than it sent: the
	// difference was never held and goes back now. grow handles both
	// directions, so this is the same check whether the length was declared or
	// discovered by the read.
	if !claim.grow(int64(len(body))) {
		s.log.Warn("request refused: body budget full", "bytes", len(body))
		s.handleBudgetExceeded(w, s.bodies)
		return
	}

	var backends []config.Backend
	if model == "" {
		// Nothing in the payload names a model: this is a plain passthrough
		// request, so send it to the default provider.
		p, err := s.cfg.DefaultProvider()
		if err != nil {
			s.errors.Add(1)
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		backends = []config.Backend{{Provider: p}}
	} else {
		backends, err = s.cfg.ResolveBackends(model)
		if err != nil {
			s.errors.Add(1)
			s.recordForwardStatus(r, false, model, "")
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// A unified model has several endpoints: each request starts at the next
	// one in line, and a retry walks on to the following endpoint, so the
	// same id load-spreads across its providers and fails over between them.
	start := 0
	if len(backends) > 1 {
		start = int(s.rr.Add(1)-1) % len(backends)
	}
	attempts := max(1, s.cfg.Retry.MaxAttempts)
	retry := &s.cfg.Retry
	// retry.totalSecs, when set, bounds the whole sequence. It is derived
	// from the request's context rather than replacing it, so the client's
	// own deadline and cancellation still win.
	ctx, cancelRetries := retry.Deadline(r.Context())
	defer cancelRetries()
	seq := s.pool.Sequence() // one rotation step per request, shared by retries
	var lastErr error
	lastProvider := ""
	attempt := 0
	for ; attempt < attempts; attempt++ {
		// The endpoint rotates per attempt, so a retry after a dead proxy or
		// a retryable status lands on the next provider of a unified model.
		be := backends[(start+attempt)%len(backends)]
		provider := be.Provider
		upstreamModel := be.Upstream
		// A model-less passthrough names no provider; look it up so the
		// health marks and the logs still say which one this attempt is on.
		provName := be.Name
		if provName == "" {
			provName = config.ProviderName(s.cfg, provider)
		}
		lastProvider = provName
		// The upstream endpoint comes from the provider's pool, minus whatever
		// is benched, picked with the same sequence the api-key pool uses: a
		// retry after a dead endpoint lands on the next one, never the same
		// one twice in a row.
		baseURL := s.pickEndpoint(provider, provName, keySeq, attempt)
		if baseURL == "" {
			lastErr = fmt.Errorf("provider %q has no baseURL", be.Name)
			continue
		}
		if provider.Options.SkipTLSVerify {
			s.warnedInsecure.CompareAndSwap(false, true)
			s.log.Warn("TLS verification is DISABLED for provider " + config.ProviderName(s.cfg, provider) +
				" (insecureSkipVerify): the proxy can read your api key and your prompts")
		}
		// Only forwarded /v1/ requests may use the pool, and providers must
		// explicitly opt in with "re-proxy": true. Local routes never reach
		// this handler; non-/v1/ passthroughs always connect directly.
		useProxy := strings.HasPrefix(r.URL.Path, "/v1/") &&
			s.cfg.ProxyEnabled() && provider.Options.ProviderUsesProxy()
		if s.cfg.ProxyEnabled() && !useProxy {
			s.log.Debug("proxy pool bypassed", "provider", be.Name, "path", r.URL.Path)
		}

		// A provider declared on one OpenAI surface is reachable through the
		// other one too: rewrite the request, the path and the answer so a
		// chat-completions client and a provider-only-Responses endpoint can
		// still meet. Computed per attempt because a unified model may mix
		// surfaces across its endpoints.
		attemptBody := body
		if model != "" && upstreamModel != model {
			if patched, ok := ReplaceModel(attemptBody, r.Header.Get("Content-Type"), upstreamModel); ok {
				attemptBody = patched
			} else {
				// Should not happen for a body that named a model, but say so
				// rather than silently forwarding the client's id upstream.
				s.log.Warn("could not rewrite model id; forwarding the client's id",
					"model", model, "upstreamModel", upstreamModel)
			}
		}
		tr := (*translation)(nil)
		forwardPath := r.URL.Path
		if clientWire, prefix, ok := wireOfPath(r.URL.Path); ok {
			upstreamWire := wireForProvider(provider)
			// The translator bridges the chat wire to either upstream wire:
			// a Responses provider, or an Anthropic Messages one. The one
			// crossing it cannot do is a Responses client against an Anthropic
			// provider - that would need the Answers wire translated into a
			// Messages request, which nothing here can express. Refuse clearly
			// instead of mistargeting the request.
			if clientWire == wireResponses && upstreamWire == wireAnthropic {
				s.errors.Add(1)
				writeJSONError(w, http.StatusBadRequest,
					"this provider speaks the Anthropic Messages API and has no Responses endpoint; use POST /v1/chat/completions or POST /v1/messages for this model, or point an OpenAI-compatible provider at it")
				return
			}
			tr = plan(inspectShape(attemptBody), clientWire, upstreamWire)
			if tr != nil {
				rewritten, err := tr.Request(attemptBody)
				if err != nil {
					// The client asked for something the provider's wire cannot
					// express. Saying so beats forwarding a request that would
					// come back as a confusing provider-side error.
					s.errors.Add(1)
					writeJSONError(w, http.StatusBadRequest, err.Error())
					return
				}
				attemptBody = rewritten
				// The upstream endpoint is the suffix of the wire the provider
				// speaks, under the same prefix the client used: a chat body
				// posted to /v1/chat/completions leaves for /v1/messages when
				// the provider is Anthropic.
				forwardPath = prefix + endpointSuffix[upstreamWire]
				s.log.Debug("translating request", "from", tr.from.String(), "to", tr.to.String(), "path", r.URL.Path)
			}
		}

		target, err := ResolveUpstream(baseURL, forwardPath, r.URL.RawQuery)
		if err != nil {
			s.errors.Add(1)
			writeJSONError(w, http.StatusBadGateway, err.Error())
			return
		}
		outreq, err := s.buildUpstreamRequest(r, target, attemptBody, provider)
		if err != nil {
			s.errors.Add(1)
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}

		var entry proxy.ProxyEntry
		haveProxy := false
		if useProxy {
			entry, haveProxy = s.pool.Take(seq, attempt)
			if !haveProxy {
				// Nothing in the pool: fall back to a direct connection.
				s.log.Warn("no proxies available, going direct", "model", model)
			}
		}

		resp, err := s.roundTrip(ctx, outreq, attemptBody, provider, entry, haveProxy, keySeq+attempt)
		if err != nil {
			lastErr = err
			if haveProxy {
				s.pool.Failed(entry, err)
			} else {
				// No proxy in the way, so this failure is the endpoint's own:
				// count it against the endpoint, and bench it if it keeps// happening. A failure behind a proxy could be either hop's
				// fault, and the proxy pool benches its own side already.
				s.endpoints.Failure(provName, baseURL)
			}
			if attempt+1 >= attempts || !retry.TimeLeft(ctx) {
				s.log.Debug("attempt failed, out of attempts", "attempt", attempt+1, "error", err)
				break
			}
			// The hop never reached the provider, so there is nothing to back
			// off from: the next proxy is tried straight away.
			s.retries.Add(1)
			s.log.Debug("attempt failed", "attempt", attempt+1, "of", attempts, "error", err)
			continue
		}
		if haveProxy {
			s.pool.Succeeded(entry)
		}
		// An answer is an answer: even a 429 or a 500 proves the endpoint is
		// reachable, so whatever bench it was under is lifted right here
		// rather than waiting out the cooldown.
		s.endpoints.Success(provName, baseURL)
		// A retryable answer is another try, but only while nothing has been
		// written to the client - which is guaranteed here, since the first
		// write happens below and returns.
		if retry.RetryableStatus(resp.StatusCode) {
			if attempt+1 >= attempts || !retry.TimeLeft(ctx) {
				s.log.Debug("retries exhausted, returning provider error",
					"tries", attempt+1, "of", attempts, "status", resp.StatusCode, "model", model)
				claim.release() // the body can never be replayed from here on
				s.recordForwardStatus(r, resp.StatusCode < 400, model, provName)
				s.finishTr(w, resp, entry, haveProxy, model, upstreamModel, tr)
				return
			}
			wait := retry.Backoff(attempt, resp.Header)
			s.log.Debug("provider error, waiting to retry",
				"attempt", attempt+1, "of", attempts, "status", resp.StatusCode, "wait", wait.String())
			if !config.SleepCtx(ctx, wait) {
				// The client hung up, or the sequence ran out of budget.
				// Either way the provider's answer is the best one there is,
				// and it is still unread: hand it over rather than inventing
				// an error of our own.
				s.log.Debug("retry sequence cut short", "tries", attempt+1, "status", resp.StatusCode)
				claim.release() // the body can never be replayed from here on
				s.recordForwardStatus(r, resp.StatusCode < 400, model, provName)
				s.finishTr(w, resp, entry, haveProxy, model, upstreamModel, tr)
				return
			}
			s.retries.Add(1)
			drainAndClose(resp)
			continue
		}
		claim.release() // the body can never be replayed from here on
		s.recordForwardStatus(r, resp.StatusCode < 400, model, provName)
		s.finishTr(w, resp, entry, haveProxy, model, upstreamModel, tr)
		return
	}

	s.errors.Add(1)
	msg := "all upstream attempts failed"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	s.lastErr.Store(msg)
	s.recordForwardStatus(r, false, model, lastProvider)
	s.log.Error("request failed", "path", r.URL.Path, "model", model, "tries", attempt, "of", attempts, "error", msg)
	writeJSONError(w, http.StatusBadGateway, msg)
}

// finishTr is finish plus the translation the request needed. Every exit from
// the retry loop holding a response goes through here.
func (s *Server) finishTr(w http.ResponseWriter, resp *http.Response, entry proxy.ProxyEntry, viaProxy bool, model, upstreamModel string, tr *translation) {
	s.lastErr.Store("")
	if viaProxy {
		s.proxied.Add(1)
		s.log.Debug("forwarded", "model", model, "upstreamModel", upstreamModel, "proxy", proxy.RedactURL(entry.URL), "status", resp.StatusCode)
	} else {
		s.direct.Add(1)
	}
	if tr != nil {
		s.log.Debug("translating response", "to", tr.from.String(), "from", tr.to.String(), "status", resp.StatusCode)
	}
	s.writeResponseTr(w, resp, entry, viaProxy, tr)
}

// readBody buffers the whole request so failed proxy attempts can replay it.
// When the client sent Content-Length, the buffer is allocated exactly once
// instead of growing by repeated doubling, which is where big uploads used to
// cost ~2x their size in transient garbage. limitMB is the ceiling to read
// against; the caller has already worked out whether media is in play.
//
// The buffered bytes are charged to claim as they arrive, not just after the
// read: a chunked upload declares no length, so this is the only point where
// the bytes can be accounted at all. When the budget runs out mid-read the
// body is abandoned where it stands rather than grown to the ceiling, and the
// caller answers 503 (see bodyBudgetFullError).
func (s *Server) readBody(r *http.Request, limitMB int, claim *Reservation) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	limit := int64(limitMB) << 20
	if r.ContentLength > 0 && r.ContentLength <= limit {
		data := make([]byte, r.ContentLength)
		if _, err := io.ReadFull(r.Body, data); err != nil {
			return nil, err
		}
		return data, nil
	}
	// Either the length was unknown or it was declared past the ceiling, so
	// the body is read a chunk at a time and the claim follows it along. The
	// read stops the moment it crosses the ceiling, which is what distinguishes
	// a genuinely oversized body from a read failure: an honest client that
	// sends exactly the ceiling still gets through.
	var buf bytes.Buffer
	chunk := make([]byte, 64<<10)
	var total int64
	for {
		n, err := r.Body.Read(chunk)
		if n > 0 {
			total += int64(n)
			if total > limit {
				return nil, bodyTooLargeError{limitMB: limitMB}
			}
			if !claim.grow(total) {
				return nil, bodyBudgetFullError{limitMB: limitMB}
			}
			buf.Write(chunk[:n])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buf.Bytes(), nil
			}
			return nil, err
		}
	}
}

// bodyBudgetFullError is readBody's signal that the in-flight budget ran out
// while the body was still arriving. It is deliberately its own type, the way
// bodyTooLargeError is: the request is refused because the router is busy
// (503), not because the client sent too much (413) and not because the
// transfer broke (400).
type bodyBudgetFullError struct{ limitMB int }

func (e bodyBudgetFullError) Error() string {
	return fmt.Sprintf("server busy: body budget full while reading a %dMB request", e.limitMB)
}

// bodyTooLargeError is the one readBody failure that is the client's fault: the
// body genuinely exceeded the ceiling. Every other error is a read failure (a
// truncated upload, a network error, a cancelled request), and handleForward
// checks for this type so it can answer 413 only for the real thing.
type bodyTooLargeError struct{ limitMB int }

func (e bodyTooLargeError) Error() string {
	return fmt.Sprintf("request body over %dMB", e.limitMB)
}

// mediaCapable reports whether any configured provider serves a model that
// takes audio, video, image or file input - or is an OCR or video model,
// whose payload is the media itself. Only then is the larger media body
// ceiling ever opened, so a text-only setup keeps refusing big bodies exactly
// as before.
func (s *Server) mediaCapable() bool {
	return s.cfg.AnyMediaCapable()
}

// mediaModelName reports whether the named model takes media, consulting the
// model's own answer first (declared modalities, or a media-bearing kind such
// as ocr or video) and the provider's mediaModels fallback second. Handles the
// config key, the advertised id and the upstream name, so any of the three
// names a client could send resolves the same way. A model with no modalities
// declared and no media-bearing kind is text-only, so it keeps the ordinary
// limit.
func (s *Server) mediaModelName(model string) bool {
	if model == "" || s.cfg.Server.MaxMediaBodyMB <= s.cfg.Server.MaxBodyMB {
		return false
	}
	ref, m, err := s.cfg.ModelCard(model)
	if err != nil || m == nil {
		// The advertised table skips hidden providers and hidden models, so a
		// hidden model never reaches ModelCard. Search the full provider map so
		// a hidden media model still opens the right body ceiling.
		for _, ref := range s.cfg.Providers() {
			if m, ok := ref.Provider.Models[model]; ok && m != nil {
				if m.TakesMedia() {
					return true
				}
				break
			}
		}
		return false
	}
	if m.TakesMedia() {
		return true
	}
	for _, name := range ref.Provider.Options.MediaModels {
		if name == model || (m != nil && (name == m.Name || name == m.Upstream)) {
			return true
		}
	}
	return false
}

// roundTrip performs one attempt. The body is re-attached on every attempt so
// a retry through a different proxy sends the same request.
//
// keySeq is this attempt's place in the provider's key pool, so a retry after
// a 429 or a rejected key walks to the next key instead of asking the same
// spent one again.
func (s *Server) roundTrip(ctx context.Context, tmpl *http.Request, body []byte, provider *config.Provider, entry proxy.ProxyEntry, viaProxy bool, keySeq int) (*http.Response, error) {
	timeout := time.Duration(s.cfg.Server.RequestSecs) * time.Second
	if provider.Options.TimeoutSecs > 0 {
		timeout = time.Duration(provider.Options.TimeoutSecs) * time.Second
	}
	skipVerify := provider.Options.SkipTLSVerify
	tr := proxy.NewTransport(nil, timeout, 0, skipVerify)
	label := "direct"

	if viaProxy {
		u, err := proxy.ParseProxyURL(entry.URL)
		if err != nil {
			return nil, err
		}
		tr = proxy.NewTransport(u, timeout, 0, skipVerify)
		// The label ends up in the 502 body and the failure log, so it
		// carries the hop, never the credentials on it.
		label = proxy.RedactURL(entry.URL)
	}

	attempt := tmpl.Clone(ctx)
	if len(body) > 0 {
		attempt.Body = io.NopCloser(bytes.NewReader(body))
		attempt.ContentLength = int64(len(body))
	} else {
		attempt.Body = http.NoBody
	}
	applyAuth(attempt, provider, keySeq, s.cfg.Auth)
	resp, err := tr.RoundTrip(attempt)
	if err != nil {
		return nil, fmt.Errorf("via %s: %w", label, err)
	}
	return resp, nil
}

func (s *Server) buildUpstreamRequest(r *http.Request, target *url.URL, body []byte, provider *config.Provider) (*http.Request, error) {
	// The body is nil here on purpose: roundTrip attaches a fresh reader for
	// each attempt so retries can replay the request.
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), nil)
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	for key, values := range r.Header {
		for _, v := range values {
			req.Header.Add(key, v)
		}
	}
	removeHopHeaders(req.Header)
	req.Header.Del("Host")
	// The router's own admin header authenticates to /noderouter/*, never
	// to a provider: it stays on this box.
	req.Header.Del("X-Noderouter-Token")
	// Some upstreams want a particular User-Agent; the configured one
	// replaces whatever the client (or Go itself) sent. It is set before
	// options.headers so an explicit "User-Agent" entry there still wins,
	// the way it does for the credential.
	if ua := strings.TrimSpace(provider.Options.UserAgent); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	// The provider credential is applied per attempt in roundTrip, so a retry
	// can take the next key from the pool.
	for k, v := range provider.Options.Headers {
		req.Header.Set(k, v)
	}
	setForwardedHeaders(req, r)
	return req, nil
}

// applyAuth sets the provider credential for one attempt. An explicit
// options.headers entry still wins, the way it did when this ran once per
// request.
func applyAuth(req *http.Request, provider *config.Provider, keySeq int, auth config.AuthConfig) {
	// The router's own credential is for this hop only, so it goes before
	// anything else reads the headers: a client that authenticated to the
	// router with one of the accepted keys must not hand that key to the
	// provider. A keyless provider drops the client's credential entirely
	// below, so this is what keeps the router's own keys off the wire for a
	// provider that does have one of its own.
	dropRouterAuth(req, auth)
	// An explicit credential in options.headers is authoritative, in either
	// dialect.
	for k := range provider.Options.Headers {
		if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "x-api-key") {
			return
		}
	}
	key := provider.APIKeyFor(keySeq)
	// Anthropic messages authenticate with x-api-key, not a bearer token, and
	// require an anthropic-version header. The router adds the version even
	// for a keyless provider so a raw curl to /v1/messages works.
	if provider.Options.Dialect() == "anthropic" {
		if key != "" {
			req.Header.Set("x-api-key", key)
			req.Header.Del("Authorization")
		} else {
			dropClientAuth(req)
		}
		req.Header.Set("anthropic-version", provider.Options.AnthropicVersionHeader())
		return
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
		// The router is supplying the credential now, so the client's other
		// spelling of it goes: an openai provider that reads x-api-key would
		// otherwise be handed the client's key beside the router's own.
		req.Header.Del("X-Api-Key")
		return
	}
	// A keyless provider takes no credential, so the client's is dropped
	// rather than forwarded. Sending nothing already works against such an
	// endpoint; forwarding a stand-in - "none", "example", whatever an SDK
	// insists on putting in the header - only hands the provider a key to
	// reject, and its 401 comes back blaming the client for a credential the
	// router never asked for. Nothing was validated on the way in either: the
	// gate is auth.apiKeys, and an open gate admits a key without vouching
	// for it. A key has no business travelling further than the router.
	dropClientAuth(req)
}

// dropClientAuth removes the client's own credential from an outgoing request,
// in both dialects, so a keyless provider sees a request carrying no credential
// at all rather than one it is free to judge. X-Noderouter-Token is already
// stripped in buildUpstreamRequest and never gets this far.
func dropClientAuth(req *http.Request) {
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")
}

// dropRouterAuth removes the router's own api keys from an outgoing request.
// Both credential dialects are checked, because either can be what a client
// authenticated with. X-Noderouter-Token never leaves the router at all; see
// buildUpstreamRequest.
func dropRouterAuth(req *http.Request, auth config.AuthConfig) {
	keys, required := auth.ClientKeys()
	if !required {
		return
	}
	for _, header := range []string{"Authorization", "X-Api-Key"} {
		value := req.Header.Get(header)
		if value == "" {
			continue
		}
		if matchesAnyKey(value, keys) || matchesAnyKey(strings.TrimPrefix(value, "Bearer "), keys) {
			req.Header.Del(header)
		}
	}
}

// presentedKeyMatches reports whether any credential the client sent is one
// of the keys the router accepts. Three spellings are read because three
// kinds of client talk to this router: "Authorization: Bearer" (the OpenAI
// SDKs), "x-api-key" (the Anthropic SDKs) and "X-Noderouter-Token" (curl,
// and the header the admin routes already use).
func presentedKeyMatches(r *http.Request, keys []string) bool {
	presented := make([]string, 0, 3)
	if v := r.Header.Get("Authorization"); v != "" {
		presented = append(presented, strings.TrimPrefix(v, "Bearer "))
	}
	if v := r.Header.Get("X-Api-Key"); v != "" {
		presented = append(presented, v)
	}
	if v := r.Header.Get("X-Noderouter-Token"); v != "" {
		presented = append(presented, v)
	}
	ok := false
	for _, candidate := range presented {
		if matchesAnyKey(candidate, keys) {
			ok = true
		}
	}
	return ok
}

// matchesAnyKey reports whether cand is one of keys. Every comparison runs,
// so the answer does not say how deep into the pool the match was.
func matchesAnyKey(cand string, keys []string) bool {
	if cand == "" {
		return false
	}
	ok := false
	for _, key := range keys {
		if subtle.ConstantTimeCompare([]byte(cand), []byte(key)) == 1 {
			ok = true
		}
	}
	return ok
}

// crossSiteRefusal says why a request must be turned away at the door, or ""
// when it may proceed. Browsers label a cross-origin fetch with an Origin
// header - always on POST, including the credential-less "simple" POST a
// hostile page can fire at the router without a preflight - and nearly every
// modern browser request additionally carries Sec-Fetch-Site. The origin is
// compared against r.Host rather than any X-Forwarded-* header: Host is set by
// the browser to the origin it is talking to and pages may not forge it,
// while X-Forwarded-Host is an ordinary header a page is free to fabricate.
// "null" - a sandboxed iframe or a file:// page - matches no host and is
// refused with the rest; Sec-Fetch-Site: cross-site covers the few requests
// that carry fetch metadata but no Origin, such as a cross-site simple GET.
func crossSiteRefusal(r *http.Request) string {
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) {
			return fmt.Sprintf("cross-origin request refused: %q is not this host", origin)
		}
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return "cross-origin request refused"
	}
	return ""
}

// hostRefusal says why a request must be turned away at the door for the
// host it was addressed to, or "" when the router answers for that host.
//
// The rule: loopback, the configured listen host, and every entry of
// server.allowedHosts pass; anything else is refused. A page that drives a
// browser to the router through DNS rebinding still chooses neither the Host
// the browser sends nor the origin it displays, so its rebound request arrives
// under a name only the attacker's DNS serves - which is precisely the name
// that is missing from the allowlist. X-Forwarded-Host is not consulted: a
// page is free to write that header and would simply vouch for its own host.
func (s *Server) hostRefusal(r *http.Request) string {
	host := hostOnly(r.Host)
	// A client that names no host at all is not a browser: browsers always
	// send one, and a rebind carries the attacker's name in the one they
	// always send. Such a request is left alone rather than refused.
	if host == "" {
		return ""
	}
	if s.forwardHosts[host] {
		return ""
	}
	// Loopback is a rule, not an entry: 127.0.0.0/8 and ::1 all pass, and
	// so does any IPv4 address wrapped in an IPv6 one.
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return ""
	}
	return fmt.Sprintf("host %q is not one this router answers for: add it to server.allowedHosts", host)
}

// allowedHostSet resolves, once at startup, which hosts the router answers
// forwarded requests for. It is the configured listen host - when that names
// an address a client could put in a Host header, rather than a wildcard
// interface - plus every entry of server.allowedHosts. Loopback is checked
// per request instead of enumerated, since there are 16 million addresses
// in 127.0.0.0/8 alone.
func allowedHostSet(cfg *config.Config) map[string]bool {
	hosts := map[string]bool{"localhost": true}
	for _, entry := range cfg.Server.AllowedHosts {
		if host := hostOnly(entry); host != "" {
			hosts[host] = true
		}
	}
	switch host := hostOnly(cfg.Server.Host); host {
	case "", "0.0.0.0", "::", "*":
		// The unspecified address and its friends mean "every
		// interface", which is not a name a client can address.
	default:
		if !strings.HasPrefix(host, "*") {
			hosts[host] = true
		}
	}
	return hosts
}

// hostOnly strips the port and any IPv6 brackets, lowercases the result and
// drops a trailing root dot, so "LLM.Example.COM.:443", "llm.example.com"
// and "[::1]:8080" each reduce to the name they denote. Host headers reach
// the router as the client spelled them; a configured entry copied out of a
// whole URL still resolves to the host that names.
func hostOnly(hostport string) string {
	host := strings.TrimSpace(hostport)
	if host == "" {
		return ""
	}
	// "https://llm.example.com" is tolerated in the config list: take the
	// host out of it. url.Parse reports an error, or no host, for every
	// plain host:port spelling, which then keeps its literal form.
	if u, err := url.Parse(host); err == nil && u.Host != "" {
		host = u.Host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		// An IPv6 literal that arrived without a port: SplitHostPort
		// takes it apart the other way and reports an error.
		host = host[1 : len(host)-1]
	}
	host = strings.ToLower(host)
	return strings.TrimSuffix(host, ".")
}

// drainAndClose throws away a response nobody is going to read. Draining a
// bounded amount first lets the connection go back to the transport's idle
// pool instead of being torn down, which matters when every retry runs over a
// different proxy.
func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

// setForwardedHeaders records who the request really came from, appending to
// what is already there rather than replacing it. Behind nginx (or any other
// front proxy) the incoming X-Forwarded-* chain is authoritative, and a router
// that overwrote it would hide the real client from the provider.
func setForwardedHeaders(req, incoming *http.Request) {
	// Host: prefer what the front proxy says the public name was.
	if host := firstValue(incoming.Header, "X-Forwarded-Host"); host != "" {
		req.Header.Set("X-Forwarded-Host", host)
	} else {
		req.Header.Set("X-Forwarded-Host", incoming.Host)
	}

	// Proto: a front proxy terminating TLS tells us https, not our own http.
	proto := firstValue(incoming.Header, "X-Forwarded-Proto")
	if proto == "" {
		proto = "http"
	}
	req.Header.Set("X-Forwarded-Proto", proto)

	// For: append this hop to the existing chain (client, proxy1, ..., us).
	var chain []string
	if existing := incoming.Header.Values("X-Forwarded-For"); len(existing) > 0 {
		for _, part := range existing {
			for _, addr := range strings.Split(part, ",") {
				if addr = strings.TrimSpace(addr); addr != "" {
					chain = append(chain, addr)
				}
			}
		}
	}
	if ip, _, err := net.SplitHostPort(incoming.RemoteAddr); err == nil && ip != "" {
		chain = append(chain, ip)
	} else if incoming.RemoteAddr != "" {
		chain = append(chain, incoming.RemoteAddr)
	}
	if len(chain) > 0 {
		req.Header.Set("X-Forwarded-For", strings.Join(chain, ", "))
	}
}

func firstValue(h http.Header, key string) string {
	if v := h.Get(key); v != "" {
		// A front proxy may send a comma separated chain; the first is the
		// original client-facing hop.
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	return ""
}

// writeResponseTr writes the upstream answer to the client: headers copied,
// status passed, body streamed through chunk by chunk. It carries the
// translation too, because every exit from handleForward funnels through it -
// a request that crossed surfaces arrives with its tr already planned, and a
// request that did not arrives with tr nil and is the plain passthrough.
func (s *Server) writeResponseTr(w http.ResponseWriter, resp *http.Response, entry proxy.ProxyEntry, viaProxy bool, tr *translation) {
	defer resp.Body.Close()
	removeHopHeaders(resp.Header)
	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	if viaProxy {
		// Which hop answered, minus any credentials baked into the proxy
		// URL: this header goes to every client that gets a response.
		w.Header().Set("X-Noderouter-Proxy", proxy.RedactURL(entry.URL))
	}
	rc := http.NewResponseController(w)
	fw := flushWriter{w: w, rc: rc}

	// An error answer belongs to the provider in its own words: the client
	// asked in one shape and the provider refused, so a translated error would
	// be a message in a dialect nobody asked for. Only 2xx answers get
	// rewritten.
	if tr == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		w.WriteHeader(resp.StatusCode)
		if resp.StatusCode >= 400 {
			s.errors.Add(1)
		}
		_, err := io.Copy(fw, resp.Body)
		if err != nil && !errors.Is(err, context.Canceled) {
			s.log.Debug("response stream ended early", "error", err)
		}
		return
	}

	// A translated body is a different length from the one upstream sent, and
	// the headers describing it no longer apply.
	resp.Header.Del("Content-Length")
	w.Header().Del("Content-Length")

	if isEventStream(resp) {
		// Streaming: translate as it arrives. Content-Type stays
		// text/event-stream, which is what the client's decoder expects.
		w.WriteHeader(resp.StatusCode)
		if err := tr.translateStream(resp.Body, fw); err != nil {
			if !errors.Is(err, context.Canceled) {
				s.log.Debug("response stream ended early", "error", err)
			}
		}
		return
	}

	// Not a stream: the whole answer is in hand, so rewrite it and fix the
	// length before anything is written.
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		// The provider's answer was cut short. Report that rather than a
		// half-translated body.
		s.errors.Add(1)
		writeJSONError(w, http.StatusBadGateway, "upstream response ended early: "+err.Error())
		return
	}
	out, err := tr.Response(raw)
	if err != nil {
		s.errors.Add(1)
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(out)))
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(out); err != nil {
		s.log.Debug("response write ended early", "error", err)
	}
}

// isEventStream reports whether an upstream answer is a streamed one. The
// content type is the only reliable signal: a client asking for a stream and a
// provider answering with one always say so in the header.
func isEventStream(resp *http.Response) bool {
	ct := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "text/event-stream")
}

// ResolveUpstream joins a provider baseURL with the incoming path without
// duplicating or dropping a shared /v1 style prefix.
func ResolveUpstream(baseURL, path, rawQuery string) (*url.URL, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("bad baseURL %q: %w", baseURL, err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("baseURL %q needs a scheme and host", baseURL)
	}
	basePath := strings.TrimSuffix(base.Path, "/")
	reqPath := path
	if basePath != "" {
		switch {
		case strings.HasPrefix(reqPath, basePath+"/"), reqPath == basePath:
			// already contained
		case strings.HasSuffix(basePath, reqPath):
			// base is the longer form, e.g. base /zen/v1 + path /v1: keep the
			// whole base path rather than the shorter client form.
			reqPath = basePath
		default:
			// A base ending in a version segment (/v1, /zen/v1, /api/v2) plus a
			// client path starting with the same segment would double it
			// (/zen/v1 + /v1/chat -> /zen/v1/v1/chat); fold one copy away.
			baseSeg := basePath[strings.LastIndex(basePath, "/")+1:]
			reqPath = strings.TrimPrefix(reqPath, "/")
			if baseSeg != "" && strings.HasPrefix(reqPath, baseSeg+"/") &&
				(strings.HasPrefix(baseSeg, "v") || strings.HasPrefix(baseSeg, "api")) {
				reqPath = reqPath[strings.Index(reqPath, "/")+1:]
			}
			reqPath = basePath + "/" + reqPath
		}
	}
	base.Path = reqPath
	if rawQuery != "" {
		base.RawQuery = rawQuery
	}
	return base, nil
}

// peekModel pulls "model" out of a request body without failing on other
// shapes. JSON is the common case; a whisper-style transcription upload is
// multipart/form-data with the model id in a form field, and without reading
// it there the router could not route the upload at all. contentType decides
// which parser runs; an empty or unparseable one leaves JSON the fallback.
func peekModel(body []byte, contentType string) string {
	if len(body) == 0 {
		return ""
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err == nil {
		return probe.Model
	}
	boundary := multipartBoundary(contentType)
	if boundary == "" {
		return ""
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			return ""
		}
		// The model field is a plain value; the audio rides in a file part
		// that can be megabytes and is never the model id.
		if part.FormName() != "model" || part.FileName() != "" {
			continue
		}
		value, err := io.ReadAll(io.LimitReader(part, 4096))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(value))
	}
}

// multipartBoundary extracts the boundary from a Content-Type header, and
// reports "" for anything that is not a multipart/form-data type - which is
// how callers tell a multipart body from every other encoding without
// parsing the body first.
func multipartBoundary(contentType string) string {
	media, params, err := mime.ParseMediaType(contentType)
	if err != nil || media != "multipart/form-data" {
		return ""
	}
	return params["boundary"]
}

// ReplaceModel swaps the model field in a request body, editing only that
// one value so the rest of the payload is preserved byte for byte. A
// multipart/form-data body - a whisper transcription upload, where the model
// id is a form field beside the audio file - is rewritten on its own terms:
// the same boundary, the same parts in the same order with their headers, and
// only the model field's value changed, so the file part rides through
// untouched. Any other encoding takes the JSON path below.
func ReplaceModel(body []byte, contentType, model string) ([]byte, bool) {
	if boundary := multipartBoundary(contentType); boundary != "" {
		return replaceMultipartModel(body, boundary, model)
	}
	if len(body) == 0 {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil {
		return nil, false
	} else if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, false
	}
	var spans [][2]int64
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, _ := tok.(string)
		// InputOffset is the end of the key token; after Decode it is the end
		// of the value. The window between the two holds the colon and any
		// whitespace, then the value itself.
		start := dec.InputOffset()
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false
		}
		end := dec.InputOffset()
		if key != "model" {
			continue
		}
		valStart := start + int64(valueOffset(body[start:end]))
		// Remember every occurrence: a duplicate top-level key is malformed
		// JSON, but peekModel reads the last one, so rewriting only the first
		// would leave the id the router actually routes on.
		spans = append(spans, [2]int64{valStart, end})
	}
	if len(spans) == 0 {
		return nil, false
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return nil, false
	}
	out := make([]byte, 0, len(body)+len(spans)*(len(encoded)-1))
	prev := int64(0)
	for _, sp := range spans {
		out = append(out, body[prev:sp[0]]...)
		out = append(out, encoded...)
		prev = sp[1]
	}
	out = append(out, body[prev:]...)
	return out, true
}

// valueOffset returns the index of a value's first byte in the window between
// a key token and the value's end, skipping the separating colon and any
// whitespace around it.
func valueOffset(b []byte) int {
	for i, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r', ':':
		default:
			return i
		}
	}
	return len(b)
}

// replaceMultipartModel rewrites the "model" form field of a buffered
// multipart/form-data body. The parts are re-emitted with the same boundary,
// in arrival order, with their original headers; only the model field's value
// changes, so a multi-megabyte audio file part arrives at the provider
// exactly as the client sent it. A body with no plain model field reports
// false, the way the JSON path does, rather than forwarding a stale id.
func replaceMultipartModel(body []byte, boundary, model string) ([]byte, bool) {
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	type part struct {
		header textproto.MIMEHeader
		value  []byte
	}
	var parts []part
	found := false
	for {
		p, err := mr.NextPart()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, false // malformed multipart: let the caller report it
		}
		value, err := io.ReadAll(p)
		if err != nil {
			return nil, false
		}
		if p.FormName() == "model" && p.FileName() == "" {
			value = []byte(model)
			found = true
		}
		parts = append(parts, part{header: p.Header, value: value})
	}
	if !found {
		return nil, false
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.SetBoundary(boundary); err != nil {
		return nil, false
	}
	for _, pt := range parts {
		pw, err := w.CreatePart(pt.header)
		if err != nil {
			return nil, false
		}
		if _, err := pw.Write(pt.value); err != nil {
			return nil, false
		}
	}
	if err := w.Close(); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{
		"message": msg,
		"type":    "noderouter_error",
		"code":    code,
	}})
}

// trackingWriter records whether the handler has already put a status line or
// any bytes on the wire. It sits between the handlers and the real
// ResponseWriter so withRecovery can tell an unanswered request from one that
// is already streaming. Unwrap keeps http.ResponseController (and therefore
// flushing) reaching the real writer through it.
type trackingWriter struct {
	http.ResponseWriter
	wrote bool
}

func (t *trackingWriter) WriteHeader(code int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *trackingWriter) Write(b []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(b)
}

func (t *trackingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// withRecovery turns a handler panic into a 500 - but only while the client is
// still unanswered. Once a status line (or any bytes) are on the wire,
// writing an error object after them would append JSON to a possibly
// streaming response, so the panic is logged and the connection is left to
// close truncated, which is what the client sees anyway. A panic carrying
// http.ErrAbortHandler is the handler's own request to stop quietly and gets
// neither a response nor an error log.
func withRecovery(log *logx.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &trackingWriter{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					log.Debug("handler aborted", "path", r.URL.Path)
					return
				}
				if tw.wrote {
					log.Error("panic in handler after the response started",
						"path", r.URL.Path, "panic", rec)
					return
				}
				log.Error("panic in handler", "path", r.URL.Path, "panic", rec)
				writeJSONError(tw, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(tw, r)
	})
}

// Proxied reports how many requests were served through a proxy.
func (s *Server) Proxied() int64 { return s.proxied.Load() }

// Retries reports how many upstream attempts were retries.
func (s *Server) Retries() int64 { return s.retries.Load() }

// BodiesStatus reports the in-flight request-body budget, for tests and the
// status endpoint.
func (s *Server) BodiesStatus() (limitMB, inFlightMB int64, enabled bool) {
	return s.bodies.Status()
}

// IsLoopbackHost reports whether a listen host is loopback-only. The
// unspecified address (0.0.0.0, ::) means "every interface", so it is the
// opposite of loopback.
func IsLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() && !ip.IsUnspecified()
	}
	return false
}
