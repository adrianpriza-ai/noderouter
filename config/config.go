package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Defaults. Everything below can be overridden from ~/.noderouter/models.json.
const (
	DefaultConfigPath = "~/.noderouter/models.json"
	DefaultPortsPath  = "~/.noderouter/ports.txt"
	// DefaultPrivatePortsPath is where the user parks their own proxies.
	// It never replaces ports.txt — harvest merges it in after testing.
	DefaultPrivatePortsPath = "~/.noderouter/ports-private.txt"

	// DefaultClientKeysPath is where `noderouter keygen` appends the client
	// keys auth.apiKeys accepts. It is read with "file:", so adding or
	// revoking a client needs no restart.
	DefaultClientKeysPath = "~/.noderouter/client-keys.txt"

	// DefaultListenPort is deliberately an uncommon high port, chosen to
	// clear the well-known dev servers (3000/3001/4000/5000/5173/8000/8080/
	// 8317/9000).
	DefaultListenHost = "127.0.0.1"
	DefaultListenPort = 32256

	// DefaultRefresh runs at 00:00 local time every day.
	DefaultRefresh = "0 0 * * *"

	// DefaultReValidateAfter is how long a successful proxy check stays valid.
	// `noderouter check` only re-tests the proxies whose last check is older
	// than this, so re-running it on a fresh list costs nothing.
	DefaultReValidateAfter = 6 * time.Hour

	// DefaultMemoryLimitMB is the soft GC ceiling (server.memoryLimitMb).
	// It is a target for the collector, not a refusal line; the hard layer is
	// server.bodyBudgetMb.
	DefaultMemoryLimitMB = 100

	// DefaultBodyBudgetMinMB is the floor for the derived body budget, so a
	// small memoryLimitMb cannot throttle ordinary chat requests.
	DefaultBodyBudgetMinMB = 32

	// DefaultMediaBodyFactor is how much larger than maxBodyMb a request that
	// carries media may be. Audio and video ride into the request base64
	// encoded, which inflates them by about a third before a model ever sees
	// them, so the ordinary ceiling is far too low for an omni model.
	DefaultMediaBodyFactor = 4

	// DefaultRetryAttempts is how many tries one request gets in total. A
	// free provider answering 429 or 503 is usually a moment's problem, so
	// the budget is generous; every try costs the request's latency, which is
	// why it stays a number rather than "keep going".
	DefaultRetryAttempts = 10

	// DefaultRetryBackoffMS is the wait after the first failure. Each further
	// failure doubles it up to DefaultRetryMaxBackoffMS, which is also the
	// ceiling on a provider's own Retry-After.
	DefaultRetryBackoffMS    = 500
	DefaultRetryMaxBackoffMS = 8000

	// Endpoint health defaults (see EndpointHealthConfig): three failures
	// bench an endpoint for a minute, and benched endpoints are probed every
	// minute until they answer again.
	DefaultEndpointFailThreshold = 3
	DefaultEndpointBenchSecs     = 60
	DefaultEndpointRecheckSecs   = 60

	// DefaultStatusPath is where the request history behind /status and
	// /v1/status is kept between runs, so restarting the router - an update,
	// a config edit, a crash a supervisor restarts - does not reset the
	// window those pages report.
	DefaultStatusPath = "~/.noderouter/status.json"

	// DefaultStatusSaveSecs is how often that file is rewritten. History
	// younger than one interval can be lost to a hard kill (SIGKILL, power
	// loss); an ordinary shutdown flushes first.
	DefaultStatusSaveSecs = 30
)

// Config is the on-disk shape of ~/.noderouter/models.json.
//
// The "provider" section matches the format used by the rest of the toolchain
// (options.baseURL / options.apiKey plus a models map). Everything else is
// noderouter specific and optional.
type Config struct {
	Server    ServerConfig         `json:"server"`
	Proxy     ProxyConfig          `json:"proxy"`
	Status    StatusConfig         `json:"status,omitempty"`
	Retry     RetryConfig          `json:"retry"`
	Endpoints EndpointHealthConfig `json:"endpoints,omitempty"`
	Auth      AuthConfig           `json:"auth"`
	Routing   RoutingConfig        `json:"routing"`
	Routes    map[string]string    `json:"routes,omitempty"`
	Provider  map[string]*Provider `json:"provider"`
	path      string               `json:"-"`

	// Lazily built, immutable-after-first-use views of the config. Requests
	// arrive at a fixed config; this keeps /v1/models, ModelCard,
	// ModelBackends and the media-ceiling checks from rebuilding sorted
	// tables on every request. Never copy a Config after use: the mutexes
	// guard these caches.
	providersOnce sync.Once
	providers     []ProviderRef
	tableOnce     sync.Once
	table         []modelRef
	mediaOnce     sync.Once
	mediaAny      bool
}

// StatusConfig is the on-disk request history the public status page and
// /v1/status read: the hourly buckets, per model and per provider, that would
// otherwise live only in RAM and start over on every restart.
//
// It is on by default. The file is a cache of what the router observed itself -
// no config, no keys, no request or response bodies - and it is pruned to the
// retention window on every load and save, so it cannot grow without bound.
// Deleting it resets the history; that is the documented way to do so.
type StatusConfig struct {
	// Enabled turns persistence off when false (nil means on). With it off,
	// the history is what the running process has seen, as before.
	Enabled *bool `json:"enabled,omitempty"`
	// File is where the history lives. A leading ~ and environment
	// variables are expanded.
	File string `json:"file,omitempty"`
	// SaveSecs is how often the file is rewritten. 0 is the default (30); a
	// negative value writes only at shutdown, for a box where the regular
	// write is not wanted.
	SaveSecs int `json:"saveSeconds,omitempty"`
}

type ServerConfig struct {
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	// AllowedHosts declares the additional hostnames the router answers
	// forwarded requests for, one bare hostname per entry (no port; the
	// comparison ignores case). Besides these, only loopback and the
	// listen host above are ever served - see router.hostRefusal.
	// Forwarded requests for any other host get a 403 that names the
	// host, which is what stops DNS rebinding: a page that tricks the
	// browser into reaching the router through a name it controls still
	// cannot put that name in this list. Reach the router by a public
	// name - directly, or through a front proxy that passes the original
	// Host through - and that name belongs here.
	AllowedHosts []string `json:"allowedHosts,omitempty"`
	MaxBodyMB    int      `json:"maxBodyMb,omitempty"`
	RequestSecs  int      `json:"requestTimeoutSecs,omitempty"`
	// MemoryLimitMB is a soft ceiling handed to the Go GC (GOMEMLIMIT): the
	// runtime collects harder as the heap approaches it instead of hoarding
	// freed pages. It does not refuse work. Defaults to 100 (MiB); set it
	// lower on a small box, higher if bodies run big. GOMEMLIMIT wins when
	// both are set.
	MemoryLimitMB int `json:"memoryLimitMb,omitempty"`
	// BodyBudgetMB is the hard backstop: the most buffered request-body bytes
	// allowed in flight at once. A request that would push past it is refused
	// with 503 instead of pushing RSS past the limit. A buffered body costs
	// roughly 3x its size resident, so the default is memoryLimitMb/3 (min
	// 32MB); 0 in the config keeps the default, a negative value disables the
	// backstop.
	BodyBudgetMB int `json:"bodyBudgetMb,omitempty"`
	// MaxMediaBodyMB is the per-request ceiling for a request that carries
	// media - audio, video, images, uploaded files - inside its JSON body.
	// Those are the requests an omni model is asked for, and they are much
	// bigger than chat: base64 inflates the payload by a third and a minute
	// of audio is a megabyte of it. Without a ceiling of their own they would
	// have to fit under maxBodyMb, which is sized for prompts. Defaults to
	// 4x maxBodyMb; a negative value means "no media ceiling beyond
	// maxBodyMb".
	MaxMediaBodyMB int `json:"maxMediaBodyMb,omitempty"`
}

// ReadTimeout bounds reading one request in full, headers and body together:
// how long the server will wait for a client to finish sending what it
// announced. It is derived rather than configured - one second per megabyte
// of the larger body ceiling, with a one-minute floor - so the biggest upload
// the router accepts still has minutes to arrive (the default media ceiling
// gets over four) while a stalled or trickling client cannot pin a connection,
// and its body-budget reservation, open forever. It says nothing about the
// response: a streamed completion can run as long as the provider takes.
func (s ServerConfig) ReadTimeout() time.Duration {
	ceiling := s.MaxBodyMB
	if s.MaxMediaBodyMB > ceiling {
		ceiling = s.MaxMediaBodyMB
	}
	if d := time.Duration(ceiling) * time.Second; d > time.Minute {
		return d
	}
	return time.Minute
}

type AuthConfig struct {
	// Token, when set, is required on every forwarded request. It is the
	// single-key spelling of APIKeys and takes the same forms, "file:"
	// included.
	Token string `json:"token,omitempty"`
	// APIKeys is the pool of client keys a forwarded request may present:
	// the same shapes a provider's apiKeys takes - an array, one whitespace
	// or newline separated string, or "file:/path" - so several clients can
	// each hold their own key, and a revoked one leaves the config (or the
	// file) instead of the whole pool. It wins over Token when both are set.
	APIKeys KeyList `json:"apiKeys,omitempty"`
	// AdminToken, when set, is required on /noderouter/* endpoints.
	AdminToken string `json:"adminToken,omitempty"`
}

// ClientKeys resolves the keys a client may present on a forwarded request.
// The second result says whether a check is configured at all: false means
// neither Token nor APIKeys was set, and the router checks nothing - any
// bearer, or no header at all, is accepted. That is the default, and what
// keeps a loopback router zero-config.
//
// A configured entry that resolves to no keys - a "file:" path that is
// missing or empty - contributes nothing but does not switch the check off:
// a gate that was asked for stays closed rather than failing open on a typo
// or a bad rotation.
func (a AuthConfig) ClientKeys() ([]string, bool) {
	entries := a.APIKeys
	if len(entries) == 0 && a.Token != "" {
		entries = KeyList{a.Token}
	}
	if len(entries) == 0 {
		return nil, false
	}
	var keys []string
	for _, entry := range entries {
		// readKeyEntry resolves "file:" through the shared, mtime-cached
		// reader, so a key file is stat'ed per request and re-read only
		// when it actually changed.
		keys = append(keys, readKeyEntry(entry)...)
	}
	return keys, true
}

type RoutingConfig struct {
	// DefaultProvider is used when a model id is unknown to the config.
	DefaultProvider string `json:"defaultProvider,omitempty"`
}

type ProxyConfig struct {
	Enabled      *bool    `json:"enabled,omitempty"`
	Mode         string   `json:"mode,omitempty"`         // rotate | first | random
	Sources      []string `json:"sources,omitempty"`      // registered providers (see proxy/provider.go) | file | a list URL
	Protocol     string   `json:"protocol,omitempty"`     // http | all
	MaxTimeoutMS int      `json:"maxTimeoutMs,omitempty"` // ProxyScrape response-time filter
	Limit        int      `json:"limit,omitempty"`        // proxies fetched per source
	File         string   `json:"file,omitempty"`         // proxy list, one proxy per line
	// PrivateFile holds the user's own proxies (private hosts, authenticated
	// gateway, ...). "tests and keeps them across refreshes". Distinct from
	// File so a harvest never overwrites it.
	PrivateFile string `json:"privateFile,omitempty"`  // user's own proxies
	Refresh     string `json:"refresh,omitempty"`      // cron expression, default 0 0 * * *
	WatchSecs   int    `json:"watchSeconds,omitempty"` // reload file when it changes, 0 = off
	// MaxAttempts is the older spelling of retry.maxAttempts and still seeds
	// it when retry leaves it out; the resolved value is written back here so
	// both fields report the budget that actually runs.
	MaxAttempts int       `json:"maxAttempts,omitempty"`
	Test        ProxyTest `json:"test"`
}

type ProxyTest struct {
	Enabled   *bool    `json:"enabled,omitempty"`
	Workers   int      `json:"workers,omitempty"`
	TimeoutMS int      `json:"timeoutMs,omitempty"`
	URLs      []string `json:"urls,omitempty"`
	Keep      int      `json:"keep,omitempty"` // 0 = keep every proxy that worked
	// SkipTLSVerify keeps proxies that man-in-the-middle TLS instead of
	// discarding them as dead. Pair it with a provider's insecureSkipVerify,
	// otherwise the router will reject the very proxies this kept.
	SkipTLSVerify bool `json:"insecureSkipVerify,omitempty"`
	// ReValidateAfter is how long a successful check stays valid, as a Go
	// duration ("6h", "90m", "24h"). `noderouter check` re-tests only the
	// proxies whose last check is older than this; "0" re-tests everything.
	// The tag is lower-camel like every other key here, and it is the spelling
	// docs/configuration.md and the shipped noderouter.jsonc use, so Config.Save
	// does not rewrite the user's file into a different name for this one.
	ReValidateAfter string `json:"reValidateAfter,omitempty"`
}

// EndpointHealthConfig is the endpoint pool's health policy: how hard an
// endpoint has to fail before it is taken out of rotation, how long it stays
// out, and how often a probe looks at the ones that are out.
//
// Only failures the router can blame on the endpoint itself count: a
// connection that never reached it, with no proxy in the way. A provider's
// own 4xx or 5xx is an answer, not a failure - the endpoint is up even when
// the model is not - and a failure behind a proxy could be either hop's
// fault, so it is left to the proxy pool's own benching.
type EndpointHealthConfig struct {
	// FailThreshold is how many consecutive failures an endpoint gets before
	// it is benched: requests then take the rest of the pool instead. 0 is the
	// default (3); a negative value turns benching off entirely.
	FailThreshold int `json:"failThreshold,omitempty"`
	// BenchSecs is how long a benched endpoint stays out. A probe (below) can
	// bring it back sooner; when there are no probes, the bench simply expires
	// here. 0 or negative is the default (60).
	BenchSecs int `json:"benchSecs,omitempty"`
	// RecheckSecs is how often benched endpoints are probed: one GET to each,
	// directly, and any answer at all - a 404 included - puts it back in the
	// pool. 0 is the default (60); a negative value turns the probes off,
	// leaving the bench to expire on its own.
	RecheckSecs int `json:"recheckSecs,omitempty"`
}

// Threshold is how many consecutive failures bench an endpoint; 0 means
// benching is off.
func (e EndpointHealthConfig) Threshold() int {
	if e.FailThreshold < 0 {
		return 0
	}
	if e.FailThreshold == 0 {
		return DefaultEndpointFailThreshold
	}
	return e.FailThreshold
}

// Bench is how long one bench lasts.
func (e EndpointHealthConfig) Bench() time.Duration {
	if e.BenchSecs <= 0 {
		return time.Duration(DefaultEndpointBenchSecs) * time.Second
	}
	return time.Duration(e.BenchSecs) * time.Second
}

// RecheckEvery is how often benched endpoints are probed; 0 means no probes.
func (e EndpointHealthConfig) RecheckEvery() time.Duration {
	if e.RecheckSecs < 0 {
		return 0
	}
	if e.RecheckSecs == 0 {
		return time.Duration(DefaultEndpointRecheckSecs) * time.Second
	}
	return time.Duration(e.RecheckSecs) * time.Second
}

// RetryConfig is how hard the router tries before giving up on a request.
type RetryConfig struct {
	// MaxAttempts is the total number of tries one request gets, the first
	// one included: 10 means nine retries. It counts every kind of failure
	// alike - a dead proxy, a 429, a 503 - because the request has not
	// reached the client either way, so there is nothing to lose by trying
	// again. Default 10; 1 disables retrying.
	MaxAttempts int `json:"maxAttempts,omitempty"`
	// BackoffMS is the wait after the first failure, doubling for each
	// further one up to MaxBackoffMS. Jitter is added on top, so a burst of
	// clients does not come back in lockstep. Default 500.
	BackoffMS int `json:"backoffMs,omitempty"`
	// MaxBackoffMS caps one wait (default 8000). A provider's Retry-After is
	// honoured up to this ceiling too: a "come back in an hour" must not pin
	// a request open for an hour.
	MaxBackoffMS int `json:"maxBackoffMs,omitempty"`
	// Statuses adds provider statuses to the ones worth another try;
	// SkipStatuses takes them away again. The defaults retry what means "not
	// now" and pass through what means "not ever".
	Statuses     []int `json:"statuses,omitempty"`
	SkipStatuses []int `json:"skipStatuses,omitempty"`
	// TotalSecs caps the wall clock the whole retry sequence may take, so a
	// request cannot outlive its client's patience even when every try hangs
	// until the per-attempt timeout. 0 means no cap beyond the request's own
	// context, which is what most callers want.
	TotalSecs int `json:"totalSecs,omitempty"`
}

// DefaultRetryStatuses are the answers that mean "not now": the request timed
// out, the provider is rate limiting, or something upstream fell over. A 400,
// 401, 403, 404 or 422 means "not ever" - the same answer arrives 9 more times
// just more slowly - so those go straight back to the client.
var DefaultRetryStatuses = map[int]bool{
	http.StatusRequestTimeout:     true, // 408
	http.StatusTooEarly:           true, // 425
	http.StatusTooManyRequests:    true, // 429
	http.StatusBadGateway:         true, // 502
	http.StatusServiceUnavailable: true, // 503
	http.StatusGatewayTimeout:     true, // 504
	529:                           true, // overloaded_error
	522:                           true, // connection timed out (Cloudflare)
	524:                           true, // a timeout occurred (Cloudflare)
}

type Provider struct {
	ID      string            `json:"id,omitempty"`
	Aliases []string          `json:"aliases,omitempty"`
	Options ProviderOptions   `json:"options"`
	Models  map[string]*Model `json:"models"`
	// Hidden keeps this provider out of /v1/models, /noderouter/routes and the
	// status summary, and out of any group it would otherwise join. Its models
	// still resolve by exact key or "<alias>/<model>", so the provider can
	// serve a request nobody can discover by browsing the lists.
	Hidden bool `json:"hidden,omitempty"`
}

// Key pool strategies, the same three the proxy pool offers in proxy.mode.
const (
	keyStrategyRotate = "rotate" // each request takes the next key (default)
	keyStrategyFirst  = "first"  // always the first key in the pool
	keyStrategyRandom = "random" // a random key per request
)

// KeyList is a pool of api keys that takes either JSON shape, so a block of
// keys can be pasted in as it stands instead of being re-wrapped:
//
//	"apiKeys": ["sk-...91", "sk-...92"]
//	"apiKeys": "sk-...91 sk-...92"
//	"apiKeys": "sk-...91\nsk-...92"
//
// Whitespace and newlines separate keys in every form; see splitKeyEntry.
type KeyList []string

func (k *KeyList) UnmarshalJSON(b []byte) error {
	var arr []string
	if err := json.Unmarshal(b, &arr); err == nil {
		*k = NormalizeKeys(arr)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("apiKeys must be an array of keys or one string holding several keys: %w", err)
	}
	*k = splitKeyEntry(s)
	return nil
}

// splitKeyEntry turns one configured value into key entries. Whitespace and
// newlines separate keys, so "  sk-...91   sk-...92  " and a multi-line string
// both work, and blank lines and "#" comments are dropped the way the proxy
// list drops them. A "file:" value is left whole, since a path may hold spaces.
func splitKeyEntry(s string) KeyList {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if strings.HasPrefix(s, "file:") {
		return KeyList{s}
	}
	var out KeyList
	for _, line := range strings.Split(s, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		for _, f := range strings.Fields(line) {
			out = append(out, f)
		}
	}
	return out
}

// NormalizeKeys flattens configured pool values into one entry per key.
func NormalizeKeys(in []string) KeyList {
	var out KeyList
	for _, s := range in {
		out = append(out, splitKeyEntry(s)...)
	}
	return out
} // NormalizeKeyStrategy settles a pool strategy the way proxy.mode settles the
// proxy one: an empty value is the default, anything unrecognised falls back to
// it rather than failing the config.
func NormalizeKeyStrategy(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case keyStrategyRotate:
		return keyStrategyRotate
	case keyStrategyFirst:
		return keyStrategyFirst
	case keyStrategyRandom:
		return keyStrategyRandom
	default:
		return keyStrategyRotate
	}
}

// Endpoint pool strategies. The first three are the choices keyStrategy and
// proxy.mode offer; "primary" is the one only endpoints have - always start at
// the first endpoint, walk on when - and only when - an attempt fails - which
// is what "first" would be if it were not already taken by the stricter,
// never-fail-over meaning.
const (
	endpointStrategyRotate  = "rotate"
	endpointStrategyFirst   = "first"
	endpointStrategyRandom  = "random"
	endpointStrategyPrimary = "primary"
)

// NormalizeEndpointStrategy settles an endpoint pool strategy the way
// NormalizeKeyStrategy settles a key one: an empty value is the default,
// "roundrobin" is accepted as a spelling of it the way proxy.mode accepts it,
// and anything unrecognised falls back to rotate rather than failing the
// config.
func NormalizeEndpointStrategy(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case endpointStrategyPrimary:
		return endpointStrategyPrimary
	case endpointStrategyFirst:
		return endpointStrategyFirst
	case endpointStrategyRandom:
		return endpointStrategyRandom
	case endpointStrategyRotate, "roundrobin":
		return endpointStrategyRotate
	default:
		return endpointStrategyRotate
	}
}

// Model kinds. A model's kind says what it is for: a chat model answers
// /v1/chat/completions, an image model paints at /v1/images/generations, an
// image-edit model accepts multipart edits at /v1/images/edits, a
// System One model returns typed decisions at /v1/systemone, an embedding
// model vectors text at /v1/embeddings, an audio model speaks, a tts model
// speaks at /v1/audio/speech, an stt (whisper) model transcribes at
// /v1/audio/transcriptions, an OCR model parses documents at /v1/ocr, a
// moderation model scores content at /v1/moderations and a video model
// renders at /v1/videos/generations. The kind is advertised in /v1/models
// and rendered by the models site; requests are still forwarded path-through,
// so the upstream decides what it accepts.
const (
	kindChat       = "chat"
	kindImage      = "image"
	kindImageEdit  = "image_edit"
	kindSystemOne  = "systemone"
	kindEmbedding  = "embedding"
	kindAudio      = "audio"
	kindTTS        = "tts"
	kindSTT        = "stt"
	kindOCR        = "ocr"
	kindModeration = "moderation"
	kindVideo      = "video"

	// kindDefault is the category a bare model falls into when neither its
	// "kind" nor its output modalities say otherwise.
	kindDefault = kindChat
)

// normalizeKind maps every accepted spelling of a kind onto one of the
// canonical values. An unrecognised kind falls back to chat rather than
// failing the config, and an explicitly empty one returns "" so callers can
// tell "unset" from "chat" (see Model.ResolveKind).
func normalizeKind(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return ""
	case "chat", "text", "llm", "completion", "completions", "text-generation":
		return kindChat
	case "image", "images", "image-generation", "imagegen", "diffusion", "text-to-image", "txt2img":
		return kindImage
	case "image_edit", "image-edit", "image-editing", "image-to-image", "img2img", "inpainting":
		return kindImageEdit
	case "systemone", "system-one", "system-one-model", "system1", "system_1", "jev", "decision", "decisions":
		return kindSystemOne
	case "embedding", "embeddings", "embed", "vector":
		return kindEmbedding
	case "audio", "voice":
		return kindAudio
	case "tts", "text-to-speech", "speech", "say":
		return kindTTS
	case "stt", "whisper", "transcription", "transcribe", "speech-to-text", "asr":
		return kindSTT
	case "ocr", "optical-character-recognition", "document-parsing":
		return kindOCR
	case "moderation", "moderations", "content-moderation", "safety":
		return kindModeration
	case "video", "videos", "video-generation", "text-to-video", "image-to-video", "t2v":
		return kindVideo
	default:
		return kindChat
	}
}

// Modalities a model can take in or give back. These are the canonical names
// /v1/models advertises and the models site renders as pills, so a config may
// spell them loosely ("video", "Video", " mp4 ") and still land on one of
// these.
const (
	ModalityText  = "text"
	ModalityImage = "image"
	ModalityAudio = "audio"
	ModalityVideo = "video"
	ModalityFile  = "file"
)

// normalizeModality maps the accepted spellings of a modality onto its
// canonical name. Anything unrecognised is lowercased and trimmed rather than
// dropped: a modality the router has never heard of is still true of the
// model, and the site shows it rather than quietly hiding it.
func normalizeModality(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "text", "txt", "texts":
		return ModalityText
	case "image", "images", "img", "vision", "picture":
		return ModalityImage
	case "audio", "sound", "speech", "voice":
		return ModalityAudio
	case "video", "videos", "movie", "clip":
		return ModalityVideo
	case "file", "files", "pdf", "document", "docs":
		return ModalityFile
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

// normalizeModalities canonicalises a modality list, dropping blanks and
// repeats while keeping the configured order, so /v1/models and the site
// agree on what a model takes.
func normalizeModalities(in []string) []string {
	if len(in) == 0 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, m := range in {
		m = normalizeModality(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// HasModality reports whether the model declares a modality on either side,
// canonicalising the name first so "Video" and "video" agree.
func (m *Model) HasModality(name string) bool {
	if m == nil {
		return false
	}
	want := normalizeModality(name)
	for _, list := range [][]string{m.Modalities.Input, m.Modalities.Output} {
		for _, got := range list {
			if normalizeModality(got) == want {
				return true
			}
		}
	}
	return false
}

// ResolveKinds settles every capability a model is advertised under. The
// legacy singular "kind" remains the primary capability; "kinds" adds
// capabilities without changing that primary value. When neither is set, a
// model that outputs images, audio or video is classified by its output
// modality, and everything else is chat.
func (m *Model) ResolveKinds() []string {
	if m == nil {
		return []string{kindDefault}
	}

	kinds := make([]string, 0, 1+len(m.Kinds))
	add := func(raw string) {
		kind := normalizeKind(raw)
		if kind == "" {
			return
		}
		for _, existing := range kinds {
			if existing == kind {
				return
			}
		}
		kinds = append(kinds, kind)
	}

	add(m.Kind)
	for _, kind := range m.Kinds {
		add(kind)
	}
	if len(kinds) > 0 {
		return kinds
	}

	for _, out := range m.Modalities.Output {
		switch normalizeKind(out) {
		case kindImage:
			return []string{kindImage}
		case kindAudio:
			return []string{kindAudio}
		case kindVideo:
			return []string{kindVideo}
		}
	}
	return []string{kindDefault}
}

// ResolveKind returns the primary capability for compatibility with callers
// that only understand the original singular kind field.
func (m *Model) ResolveKind() string {
	return m.ResolveKinds()[0]
}

// ProviderOptions configures how a provider is reached.
type ProviderOptions struct {
	BaseURL string `json:"baseURL"`
	// BaseURLs is a pool of upstream endpoints for this provider, spread the
	// way apiKeys spreads keys: one endpoint per request, picked by
	// endpointStrategy, and the next endpoint tried when an attempt fails.
	// It takes every shape apiKeys takes - an array, one whitespace or
	// newline separated string, or "file:<path>" listing endpoints one per
	// line - and it wins over baseURL when both are set, the way apiKeys
	// wins over apiKey. baseURL alone stays the single-endpoint spelling,
	// and is what a pool that resolves to nothing falls back to.
	BaseURLs KeyList `json:"baseURLs,omitempty"` // EndpointStrategy picks which endpoint of the pool a request uses:
	// rotate (the default), primary, first, or random. The first three walk on
	// to the next endpoint when an attempt fails - what differs is where a
	// request starts: rotate steps one place per request, primary at the first
	// endpoint, first nowhere at all. Anything unrecognised falls back to
	// rotate instead of failing the config.
	EndpointStrategy string `json:"endpointStrategy,omitempty"`
	// APIKey is a literal key, a "file:<path>" to read one from disk on every
	// request, or "" for a keyless endpoint, in which case the client's own
	// Authorization passes through. It also takes a pool of keys separated by
	// whitespace; apiKeys is the clearer way to write one.
	APIKey string `json:"apiKey,omitempty"`
	// API is the upstream Dialect. "openai" (the default) speaks the OpenAI
	// REST shape and authenticates with Authorization: Bearer; "anthropic"
	// speaks the Messages API and authenticates with x-api-key plus an
	// anthropic-version header. Requests are forwarded path-through either
	// way, so only the credential shape changes.
	API string `json:"api,omitempty"`
	// AnthropicVersion overrides the anthropic-version header sent to an
	// "anthropic" provider; default 2023-06-01.
	AnthropicVersion string `json:"AnthropicVersion,omitempty"`
	// SurfaceName is which of the two OpenAI wire formats the upstream speaks:
	// "chat" (the default) for /v1/chat/completions, or "responses" for
	// /v1/responses. It is independent of "api", which only decides how the
	// provider is authenticated. With "responses" set, the router accepts
	// either endpoint from the client and translates between them, so a
	// chat-completions caller and a Responses-API caller reach the same model.
	SurfaceName string `json:"surface,omitempty"`
	// MediaModels names models of this provider that accept audio, video or
	// image input. It is empty for the common case: the router reads the
	// modalities off the model itself, so an omni model only has to declare
	// them. Set it for a provider whose models do not declare their own
	// modalities but do take media, and the router will allow the larger
	// media body ceiling for requests naming one of these.
	MediaModels []string `json:"mediaModels,omitempty"`
	// APIKeys is a pool requests are spread across, one key per request.
	APIKeys KeyList `json:"apiKeys,omitempty"`
	// KeyStrategy picks which key of the pool a request uses: rotate (the
	// default), first, or random. The same choice proxy.mode offers for
	// proxies, per provider. Unlike a proxy, a key that turns out to be dead or
	// spent does not fail forward on its own - the router only retries hop
	// failures, not a 401 - so "first" really does mean the same key until the
	// pool or the strategy changes.
	KeyStrategy string            `json:"keyStrategy,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	// UserAgent replaces the User-Agent on every request to this provider.
	// Some upstreams filter or refuse requests carrying no browser-shaped
	// one - Go's own "Go-http-client/1.1", or whatever SDK the client used,
	// forwarded through - and headers is a clumsy spelling for a single
	// well-known field. Empty (the default) forwards whatever the client
	// sent; an explicit "User-Agent" entry in headers still wins.
	UserAgent   string `json:"userAgent,omitempty"`
	TimeoutSecs int    `json:"timeoutSecs,omitempty"`
	// ReProxy opts this provider into the proxy pool when explicitly true.
	// Omitted and false values connect directly. proxy.enabled stays the master
	// switch: when the pool is off, this option has nothing to enable.
	ReProxy *bool `json:"re-proxy,omitempty"`
	// SkipTLSVerify turns off certificate verification on the upstream hop.
	// Plenty of free proxies man-in-the-middle TLS, so this is often the
	// difference between "no working proxies" and "working proxies" - and it
	// also means the proxy can read your api key and your prompts. Off by
	// default; the router will not weaken TLS on its own.
	SkipTLSVerify bool `json:"insecureSkipVerify,omitempty"`
}

type Model struct {
	Name        string `json:"name,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Description string `json:"description,omitempty"`
	// As renames the model id. It becomes the id /v1/models advertises and the
	// id a request has to name to reach this model, while the config key stays
	// what goes upstream unless "upstreamName" says otherwise. Set it when two
	// Providers expose the same id, so the two are distinguishable.
	As string `json:"as,omitempty"`
	// Kind is the model's primary category: "chat" (the default), "image",
	// "image_edit", "systemone", "embedding", "audio", "ocr", "moderation"
	// or "video". When left out it is inferred from the output modalities, so an
	// image model only has to declare that it outputs images. See ResolveKind.
	Kind string `json:"kind,omitempty"`
	// Kinds adds capabilities to the primary Kind. For example, an image model
	// that supports both generation and editing can use kind "image" with
	// kinds ["image_edit"]. The primary Kind remains the first capability and
	// determines the legacy endpoint field.
	Kinds      []string   `json:"kinds,omitempty"`
	Upstream   string     `json:"upstreamName,omitempty"` // override the id sent to the provider
	Limit      ModelLimit `json:"limit"`
	Modalities Modalities `json:"modalities"`
	// Group unifies this model with models of the same group name in other
	// providers: /v1/models shows one entry under the group name, and a
	// request naming it is served by round-robin across, with failover over,
	// every provider that serves the group.
	Group string `json:"group,omitempty"`
	// Hidden keeps the model out of /v1/models, /noderouter/routes and any
	// group it would otherwise join. It still resolves by exact key or
	// "<alias>/<model>", so an unlisted model can serve a request.
	Hidden bool `json:"hidden,omitempty"`
}

type ModelLimit struct {
	Context int `json:"context,omitempty"`
	Output  int `json:"output,omitempty"`
}

type Modalities struct {
	Input  []string `json:"input,omitempty"`
	Output []string `json:"output,omitempty"`
}

// ProviderRef pairs a provider with its config key.
type ProviderRef struct {
	Name     string
	Provider *Provider
}

// ApplyDefaults fills in every value the user left out.
func (c *Config) ApplyDefaults() {
	if c.Server.Host == "" {
		c.Server.Host = DefaultListenHost
	}
	if c.Server.Port == 0 {
		c.Server.Port = DefaultListenPort
	}
	if c.Server.MaxBodyMB <= 0 {
		c.Server.MaxBodyMB = 64
	}
	if c.Server.RequestSecs <= 0 {
		c.Server.RequestSecs = 900
	}
	if c.Server.MemoryLimitMB <= 0 {
		c.Server.MemoryLimitMB = DefaultMemoryLimitMB
	}
	if c.Server.BodyBudgetMB == 0 {
		// A buffered body costs ~3x its size in resident memory (raw read,
		// model-patched copy, GC slack), so a third of the soft limit keeps the
		// worst case under it. Never below 32MB, so normal chat traffic is
		// never refused.
		budget := c.Server.MemoryLimitMB / 3
		if budget < DefaultBodyBudgetMinMB {
			budget = DefaultBodyBudgetMinMB
		}
		c.Server.BodyBudgetMB = budget
	}
	if c.Server.MaxMediaBodyMB == 0 {
		// Media requests get their own, higher ceiling. Base64 inflates the
		// payload by a third before the model sees it, so a minute of audio
		// is well over a megabyte on the wire; without this, an omni model
		// would have to fit inside the prompt-sized maxBodyMb.
		c.Server.MaxMediaBodyMB = c.Server.MaxBodyMB * DefaultMediaBodyFactor
	}
	if c.Server.MaxMediaBodyMB > 0 && c.Server.MaxMediaBodyMB < c.Server.MaxBodyMB {
		// Not a lower ceiling for media than for chat: that would refuse
		// requests the ordinary path would have taken.
		c.Server.MaxMediaBodyMB = c.Server.MaxBodyMB
	}

	if c.Status.File == "" {
		c.Status.File = DefaultStatusPath
	}
	if c.Status.SaveSecs == 0 {
		c.Status.SaveSecs = DefaultStatusSaveSecs
	}

	if c.Proxy.Mode == "" {
		c.Proxy.Mode = "rotate"
	}
	c.Proxy.Mode = strings.ToLower(c.Proxy.Mode)
	switch c.Proxy.Mode {
	case "rotate", "roundrobin", "first", "random":
	default:
		c.Proxy.Mode = "rotate"
	}
	if len(c.Proxy.Sources) == 0 {
		c.Proxy.Sources = []string{"proxyscrape", "geonode", "freeproxydb", "chillyproxy", "iplocate", "roundproxies"}
	}
	if c.Proxy.Protocol == "" {
		c.Proxy.Protocol = "all"
	}
	if c.Proxy.MaxTimeoutMS <= 0 {
		c.Proxy.MaxTimeoutMS = 10000
	}
	if c.Proxy.Limit <= 0 {
		c.Proxy.Limit = 200
	}
	if c.Proxy.File == "" {
		c.Proxy.File = DefaultPortsPath
	}
	if c.Proxy.PrivateFile == "" {
		c.Proxy.PrivateFile = DefaultPrivatePortsPath
	}
	if c.Proxy.Refresh == "" {
		c.Proxy.Refresh = DefaultRefresh
	}
	if c.Proxy.WatchSecs < 0 {
		c.Proxy.WatchSecs = 0
	} else if c.Proxy.WatchSecs == 0 {
		c.Proxy.WatchSecs = 30
	}
	// retry.maxAttempts is the budget now. proxy.maxAttempts is the older
	// spelling of the same number, so a config that sets it keeps working:
	// an explicit proxy.maxAttempts seeds the retry budget, and the resolved
	// value is written back so both fields report what actually runs.
	if c.Retry.MaxAttempts <= 0 {
		c.Retry.MaxAttempts = c.Proxy.MaxAttempts
	}
	if c.Retry.MaxAttempts <= 0 {
		c.Retry.MaxAttempts = DefaultRetryAttempts
	}
	c.Proxy.MaxAttempts = c.Retry.MaxAttempts

	if c.Retry.BackoffMS < 0 {
		// Negative means "retry at once", which is also how a config turns
		// waiting off entirely. 0 still means "unset", so the default stands.
		c.Retry.BackoffMS = 0
	} else if c.Retry.BackoffMS == 0 {
		c.Retry.BackoffMS = DefaultRetryBackoffMS
	}
	if c.Retry.MaxBackoffMS < 0 {
		c.Retry.MaxBackoffMS = 0
	} else if c.Retry.MaxBackoffMS == 0 {
		c.Retry.MaxBackoffMS = DefaultRetryMaxBackoffMS
	}
	if c.Retry.MaxBackoffMS > 0 && c.Retry.BackoffMS > c.Retry.MaxBackoffMS {
		c.Retry.BackoffMS = c.Retry.MaxBackoffMS
	}
	if c.Retry.TotalSecs < 0 {
		c.Retry.TotalSecs = 0
	}
	if c.Proxy.Test.Workers <= 0 {
		c.Proxy.Test.Workers = 50
	}
	if c.Proxy.Test.TimeoutMS <= 0 {
		c.Proxy.Test.TimeoutMS = 2000
	}
	if len(c.Proxy.Test.URLs) == 0 {
		c.Proxy.Test.URLs = DefaultTestURLs()
	}
	if c.Proxy.Test.ReValidateAfter == "" {
		c.Proxy.Test.ReValidateAfter = DefaultReValidateAfter.String()
	}
	if c.Proxy.Test.Enabled == nil {
		c.Proxy.Test.Enabled = BoolPtr(true)
	}
	if c.Proxy.Enabled == nil {
		c.Proxy.Enabled = BoolPtr(true)
	}
	if c.Provider == nil {
		c.Provider = map[string]*Provider{}
	}
	// The client key pool settles the same way a provider's does, so every
	// accepted spelling of it ends up one entry per key.
	c.Auth.APIKeys = NormalizeKeys(c.Auth.APIKeys)
	for name, p := range c.Provider {
		if p == nil {
			continue
		}
		if p.Models == nil {
			p.Models = map[string]*Model{}
		}
		// Settle the key pool once, so every accepted spelling of it - an
		// array, one whitespace-separated string, a multi-line string - ends up
		// as one entry per key. An apiKey carrying several keys is just a pool.
		p.Options.APIKeys = NormalizeKeys(p.Options.APIKeys)
		if len(p.Options.APIKeys) == 0 {
			if keys := splitKeyEntry(p.Options.APIKey); len(keys) > 1 {
				p.Options.APIKeys = keys
				p.Options.APIKey = ""
			}
		}
		p.Options.KeyStrategy = NormalizeKeyStrategy(p.Options.KeyStrategy)
		// The endpoint pool settles the same way, since it accepts the same
		// spellings: an array, one string holding several URLs, or a "file:"
		// listing them one per line.
		p.Options.BaseURLs = NormalizeKeys(p.Options.BaseURLs)
		p.Options.EndpointStrategy = NormalizeEndpointStrategy(p.Options.EndpointStrategy)
		for id, m := range p.Models {
			if m == nil {
				delete(p.Models, id)
				continue
			}
			if m.Provider == "" {
				m.Provider = name
			}
			// Fill in the primary kind before anything reads it, so a model
			// classified by its output modalities is advertised the same
			// way /v1/models and the site render it. If plural capabilities
			// were configured, canonicalise only the additional capabilities;
			// ResolveKinds still reconstructs the complete ordered list.
			resolvedKinds := m.ResolveKinds()
			m.Kind = resolvedKinds[0]
			if len(m.Kinds) > 0 {
				m.Kinds = resolvedKinds[1:]
			}
			// Canonicalise the modality names for the same reason: an
			// omni model saying "Video" should advertise "video", or the
			// site renders it with the generic text icon.
			m.Modalities.Input = normalizeModalities(m.Modalities.Input)
			m.Modalities.Output = normalizeModalities(m.Modalities.Output)
		}
	}
	c.Proxy.File = ExpandPath(c.Proxy.File)
}

// LoadConfig reads the config file and applies defaults. The body may be written
// as JSONC - // and /* */ comments and trailing commas allowed - because
// noderouter.jsonc is the example the docs tell you to copy into place, and a
// loader that only speaks strict JSON rejects it outright ("invalid character
// '/'"), comments and all. A plain JSON file has nothing to strip and comes
// through byte for byte.
func LoadConfig(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigPath
	}
	expanded := ExpandPath(path)
	raw, err := os.ReadFile(expanded)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no config at %s (run `noderouter init` to create one)", expanded)
		}
		return nil, err
	}
	cfg := &Config{}
	if err := json.Unmarshal(stripJSONC(raw), cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", expanded, err)
	}
	cfg.path = expanded
	cfg.ApplyDefaults()
	return cfg, nil
}

// stripJSONC removes the JSONC syntax around JSON's own: comments first, then
// the trailing commas they often sit next to. Two passes keep each one
// simple - once comments are gone, the trailing-comma pass only has to
// recognize JSON's own punctuation.
func stripJSONC(src []byte) []byte {
	return dropTrailingCommas(dropJSONComments(src))
}

// dropJSONComments strips // and /* */ comments written outside string
// literals, keeping the newlines so a parse error still points at the right
// line. Inside a string nothing is touched: a baseURL of
// "https://example.com/v1//x" is exactly the case a slash-happy stripper
// would mangle, and it has to come out byte for byte.
func dropJSONComments(src []byte) []byte {
	out := make([]byte, 0, len(src))
	inString, escaped := false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			out = append(out, c)
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			// Line comment: drop to the end of the line, keeping the newline.
			for i < len(src) && src[i] != '\n' {
				i++
			}
			if i < len(src) {
				out = append(out, '\n')
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			// Block comment: drop it, keeping any newlines inside it.
			for i++; i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/'); i++ {
				if src[i] == '\n' {
					out = append(out, '\n')
				}
			}
			i++ // land on '*', so the loop's own i++ steps past the '/' as well
		default:
			out = append(out, c)
		}
	}
	return out
}

// dropTrailingCommas removes the comma JSONC allows before a closing brace or
// bracket, again only outside string literals.
func dropTrailingCommas(src []byte) []byte {
	out := make([]byte, 0, len(src))
	inString, escaped := false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if c == ',' {
			// A comma whose next significant byte closes the container is the
			// trailing one; drop it rather than forwarding it to the decoder.
			j := i + 1
			for j < len(src) && (src[j] == ' ' || src[j] == '\t' || src[j] == '\n' || src[j] == '\r') {
				j++
			}
			if j < len(src) && (src[j] == '}' || src[j] == ']') {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

// Save writes the config back out with indentation.
func (c *Config) Save(path string) error {
	if path == "" {
		path = c.path
	}
	if path == "" {
		path = DefaultConfigPath
	}
	path = ExpandPath(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(buf, '\n'), 0o600)
}

func (c *Config) Path() string { return c.path }

// MetaPath is the sidecar next to the proxy list that stores latencies.
func (c *Config) MetaPath() string { return MetaPathFor(c.Proxy.File) }

func (c *Config) ProxyEnabled() bool { return c.Proxy.Enabled != nil && *c.Proxy.Enabled }

// StatusEnabled reports whether the request history is kept on disk. The
// default is yes: a restart is routine (an update, a config edit, a crash the
// supervisor picks up) and starting the counters over every time is what the
// file exists to prevent.
func (c *Config) StatusEnabled() bool { return c.Status.Enabled == nil || *c.Status.Enabled }

// StatusFilePath is where the request history is kept, or "" when persistence
// is off, so one call decides both whether and where to write. The path is not
// expanded: callers expand at the point of use, like every other file here.
func (c *Config) StatusFilePath() string {
	if !c.StatusEnabled() {
		return ""
	}
	if c.Status.File == "" {
		return DefaultStatusPath
	}
	return c.Status.File
}

// StatusSaveEvery is how often the history is flushed to disk. Zero means no
// periodic write at all, which is the answer in two cases: persistence is off,
// so there is nothing to flush, or a negative status.saveSeconds asked for the
// file to be written at shutdown only.
func (c *Config) StatusSaveEvery() time.Duration {
	if !c.StatusEnabled() || c.Status.SaveSecs <= 0 {
		return 0
	}
	return time.Duration(c.Status.SaveSecs) * time.Second
}

func (c *Config) TestEnabled() bool { return c.Proxy.Test.Enabled != nil && *c.Proxy.Test.Enabled }

// ProviderUsesProxy reports whether requests to this provider are opted into
// the proxy pool. Only an explicit "re-proxy": true enables it. proxy.enabled
// is the master switch, so a pool that is disabled stays disabled for everyone.
func (o ProviderOptions) ProviderUsesProxy() bool { return o.ReProxy != nil && *o.ReProxy }

// Dialect reports which credential shape a provider expects: "anthropic" for
// the Messages API (x-api-key + anthropic-version), "openai" otherwise.
func (o ProviderOptions) Dialect() string {
	switch strings.ToLower(strings.TrimSpace(o.API)) {
	case "anthropic", "claude", "messages", "anthropic-messages":
		return "anthropic"
	default:
		return "openai"
	}
}

// Surface reports which of the two OpenAI wire formats the provider speaks:
// SurfaceChat for /v1/chat/completions, SurfaceResponses for /v1/responses.
// Unrecognised values fall back to chat rather than failing the config, the
// same way Dialect and proxy.mode settle theirs.
//
// An anthropic provider has no Responses API, so it is always chat: there is
// nothing to translate to.
func (o ProviderOptions) Surface() string {
	if o.Dialect() == "anthropic" {
		return SurfaceChat
	}
	switch strings.ToLower(strings.TrimSpace(o.SurfaceName)) {
	case "responses", "response", "responses-api", "responses_api":
		return SurfaceResponses
	default:
		return SurfaceChat
	}
}

// Surface names for the two OpenAI wire formats a provider can speak.
const (
	SurfaceChat      = "chat"
	SurfaceResponses = "responses"
)

// MediaCapable reports whether this provider serves at least one model that
// takes media in. A provider with no models at all is not media capable: the
// router should not open the larger body ceiling for a request it cannot
// place.
func (p *Provider) MediaCapable() bool {
	if p == nil {
		return false
	}
	if len(p.Options.MediaModels) > 0 {
		return true
	}
	for _, m := range p.Models {
		if m != nil && m.TakesMedia() {
			return true
		}
	}
	return false
}

// TakesMedia reports whether a model takes audio, video, image or file input.
// A model that says nothing about its modalities is not counted: the default
// is text, and assuming media would silently raise the body ceiling for every
// provider that skipped the field. An OCR model is counted whatever it
// declares, though: its payload is the document itself - a PDF or a page
// image, often base64 - so kind "ocr" alone has to open the larger ceiling,
// or a config that classified the model but never spelled out its modalities
// would refuse its own document uploads at the ordinary body limit. A video
// model counts the same way: a clip is the payload, base64 and megabytes of it,
// whether it goes in to be understood or comes back rendered. An stt model is
// the same story in multipart: the audio file is the payload, and minutes of
// it dwarf the ordinary prompt-sized ceiling. Image-edit models are also
// media-bearing even when their config omits modalities, because the edit
// request carries image and optional mask files.
func (m *Model) TakesMedia() bool {
	if m == nil {
		return false
	}
	for _, kind := range m.ResolveKinds() {
		switch kind {
		case kindImageEdit, kindOCR, kindVideo, kindSTT:
			return true
		}
	}
	for _, in := range m.Modalities.Input {
		switch normalizeModality(in) {
		case ModalityAudio, ModalityVideo, ModalityImage, ModalityFile:
			return true
		}
	}
	return false
}

// Translates reports whether this provider speaks only one wire, which is what
// makes the router translate for it. A provider on the chat surface is already
// reachable from a chat client and needs nothing; one on the responses surface
// is reachable from a Responses client, and the router maps the chat endpoint
// onto it; an "anthropic" provider is reached the same way, translated onto
// the Messages API.
func (o ProviderOptions) Translates() bool {
	return o.Dialect() == "anthropic" || o.Surface() == SurfaceResponses
}

// AnthropicVersion is the anthropic-version header value to send, defaulting
// to the stable 2023-06-01 the Messages API expects.
func (o ProviderOptions) AnthropicVersionHeader() string {
	if v := strings.TrimSpace(o.AnthropicVersion); v != "" {
		return v
	}
	return "2023-06-01"
}

// ReValidateAfter is how long a proxy check stays valid. A missing or
// unparseable value falls back to the default; an explicit "0" means
// "re-check every proxy on every start", which restores the old behaviour.
func (c *Config) ReValidateAfter() time.Duration {
	raw := strings.TrimSpace(c.Proxy.Test.ReValidateAfter)
	if raw == "" {
		return DefaultReValidateAfter
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return DefaultReValidateAfter
	}
	return d
}

// Providers returns every configured provider, sorted by name for stable output.
func (c *Config) Providers() []ProviderRef {
	c.providersOnce.Do(func() {
		refs := make([]ProviderRef, 0, len(c.Provider))
		for name, p := range c.Provider {
			if p == nil {
				continue
			}
			refs = append(refs, ProviderRef{Name: name, Provider: p})
		}
		sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
		c.providers = refs
	})
	return append([]ProviderRef(nil), c.providers...)
}

func (r ProviderRef) aliases() []string {
	out := []string{r.Name}
	if r.Provider.ID != "" {
		out = append(out, r.Provider.ID)
	}
	out = append(out, r.Provider.Aliases...)
	return out
}

// aliasMatch ranks a model-id prefix (e.g. "llm" in "llm/minimax-m3") against
// this provider's aliases: 2 when one of them is exactly the prefix, 1 when
// one merely starts with it ("llm" against "LLM2"), 0 when none does. The rank
// is what lets an exact name outrank a longer one that only shares its start.
func (r ProviderRef) aliasMatch(prefix string) int {
	norm := normalizeName(prefix)
	if norm == "" {
		return 0
	}
	best := 0
	for _, a := range r.aliases() {
		switch na := normalizeName(a); {
		case na == norm:
			return 2
		case strings.HasPrefix(na, norm):
			best = 1
		}
	}
	return best
}

// resolvePrefix picks the provider a namespaced model id points at. An exact
// alias match wins outright, so "llm" reaches the provider called LLM even
// though another one is called LLM2. A partial match is only accepted when
// exactly one provider has one: otherwise the provider-name sort would decide,
// and "llm/minimax-m3" would quietly go to whichever of LLM2 and LLM3 happened
// to sort first. found is false when nothing matches at all, and an ambiguous
// partial match is an error naming the candidates rather than a coin flip.
func (c *Config) resolvePrefix(prefix string) (ProviderRef, bool, error) {
	var exact, partial []ProviderRef
	for _, ref := range c.Providers() {
		switch ref.aliasMatch(prefix) {
		case 2:
			exact = append(exact, ref)
		case 1:
			partial = append(partial, ref)
		}
	}
	if len(exact) > 0 {
		// Two providers claiming one alias outright is a config question rather
		// than a prefix question, and the name sort has always answered it the
		// same way, so that behaviour is left alone.
		return exact[0], true, nil
	}
	switch len(partial) {
	case 0:
		return ProviderRef{}, false, nil
	case 1:
		return partial[0], true, nil
	}
	names := make([]string, 0, len(partial))
	for _, ref := range partial {
		names = append(names, ref.Name)
	}
	return ProviderRef{}, false, fmt.Errorf("prefix %q starts the name of %s; spell the provider out in full",
		prefix, strings.Join(names, ", "))
}

func normalizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (c *Config) ProviderByName(name string) *ProviderRef {
	for _, ref := range c.Providers() {
		if ref.Name == name {
			r := ref
			return &r
		}
	}
	return nil
}

// modelRef is one configured model together with the provider that serves it
// and the two ids that surround it: the one clients call it by, and the one the
// provider expects.
type modelRef struct {
	Provider ProviderRef // the provider that owns the model
	Key      string      // the config key
	Want     string      // the id the config asked for: "as", else the config key
	Call     string      // the id the router advertises, unique across the config
	Upstream string      // the id sent to the provider
	Model    *Model
	// Group is the unification name when set; then the entry stands for
	// every provider that serves the same group, listed in Backends, and the
	// provider/key fields above describe the representative only.
	Group    string
	Backends []backendRef
}

// backendRef is one provider's slice of a group: the provider serving it,
// the config key, the upstream id and the model settings.
type backendRef struct {
	Provider ProviderRef
	Key      string
	Upstream string
	Model    *Model
}

// Backend is one endpoint a unified model can be served by.
type Backend struct {
	Provider *Provider
	Name     string // the provider's config name
	Upstream string // the id sent to this endpoint
}

// ModelTable lists every configured model in a stable order (Providers by name,
// models by key) and decides the id each one is advertised under.
//
// That id is the model's "as" when it sets one and the config key otherwise -
// but never twice. A /v1/models list with two entries of the same id is a list a
// client cannot act on, and a request for that id is ambiguous, so when several
// models claim one, the ones that did not explicitly ask for it fall back to
// the "<provider>/<key>" namespace form, which resolves like any other id.
func (c *Config) ModelTable() []modelRef {
	return append([]modelRef(nil), c.modelTable()...)
}

// modelTable returns the cached table built by ModelTable's first call.
// Internal callers that only read should prefer this so they do not pay for
// the defensive copy.
func (c *Config) modelTable() []modelRef {
	c.tableOnce.Do(func() {
		c.table = buildModelTable(c)
	})
	return c.table
}

func (c *Config) AnyMediaCapable() bool {
	c.mediaOnce.Do(func() {
		for _, ref := range c.Providers() {
			if ref.Provider.MediaCapable() {
				c.mediaAny = true
				return
			}
		}
	})
	return c.mediaAny
}

func buildModelTable(c *Config) []modelRef {
	var entries []modelRef
	for _, ref := range c.Providers() {
		if ref.Provider.Hidden {
			continue
		}
		keys := make([]string, 0, len(ref.Provider.Models))
		for id := range ref.Provider.Models {
			keys = append(keys, id)
		}
		sort.Strings(keys)
		for _, id := range keys {
			m := ref.Provider.Models[id]
			if m == nil || m.Hidden {
				continue
			}
			want := id
			if m.As != "" {
				want = m.As
			}
			if m.Group != "" {
				want = m.Group
			}
			entries = append(entries, modelRef{
				Provider: ref,
				Key:      id,
				Want:     want,
				Upstream: upstreamModel(ref.Provider, id),
				Model:    m,
				Group:    m.Group,
			})
		}
	}
	if len(entries) == 0 {
		return nil
	}

	// Collapse every model that joined a group into one merged entry. The
	// group name is the id clients call; the representative keeps the
	// table's shape (first member by provider name) while Backends records
	// all of them for routing. Members no longer appear as their own rows:
	// the group entry is their only advertisement.
	byGroup := make(map[string][]int)
	for i := range entries {
		if entries[i].Group != "" {
			byGroup[entries[i].Group] = append(byGroup[entries[i].Group], i)
		}
	}
	if len(byGroup) > 0 {
		rows := []modelRef{}
		for i := range entries {
			if entries[i].Group != "" {
				continue
			}
			rows = append(rows, entries[i])
		}
		// Ranging the map would append the group rows in a fresh order every
		// run, which is not the stable order this table promises and which
		// decides who wins an id clash further down. Sort the names and append
		// in that order; SliceStable keeps same-provider members in key order.
		groupNames := make([]string, 0, len(byGroup))
		for name := range byGroup {
			groupNames = append(groupNames, name)
		}
		sort.Strings(groupNames)
		for _, name := range groupNames {
			idx := byGroup[name]
			sort.SliceStable(idx, func(a, b int) bool {
				return entries[idx[a]].Provider.Name < entries[idx[b]].Provider.Name
			})
			rep := entries[idx[0]]
			merged := modelRef{
				Provider: rep.Provider, Key: name, Want: name,
				Upstream: rep.Upstream, Model: rep.Model, Group: name,
			}
			for _, i := range idx {
				merged.Backends = append(merged.Backends, backendRef{
					Provider: entries[i].Provider, Key: entries[i].Key,
					Upstream: entries[i].Upstream, Model: entries[i].Model,
				})
			}
			rows = append(rows, merged)
		}
		entries = rows
	}

	// Group by the id each model wants, so a group of more than one is a clash.
	// An explicit "as" or a group name outranks a bare config key: the model
	// that named the id gets to keep it and the bare keys step aside.
	wants := make(map[string][]int, len(entries))
	for i, e := range entries {
		wants[e.Want] = append(wants[e.Want], i)
	}
	keep := make(map[int]bool, len(entries))
	for _, idx := range wants {
		if len(idx) == 1 {
			keep[idx[0]] = true
			continue
		}
		var named []int
		for _, i := range idx {
			if entries[i].Model.As != "" || entries[i].Group != "" {
				named = append(named, i)
			}
		}
		if len(named) == 1 {
			keep[named[0]] = true
		}
	}

	used := make(map[string]bool, len(entries))
	for i := range entries {
		if keep[i] && !used[entries[i].Want] {
			entries[i].Call = entries[i].Want
		} else {
			entries[i].Call = namespacedID(entries[i], used)
		}
		used[entries[i].Call] = true
	}
	return entries
}

// namespacedID picks a fallback call id for a model that cannot have the one it
// asked for: "<provider-alias>/<key>" first, then the provider's config name,
// then a numbered variant. It never returns an id some other model already has.
func namespacedID(e modelRef, used map[string]bool) string {
	alias := e.Provider.Provider.ID
	if alias == "" {
		alias = e.Provider.Name
	}
	candidates := []string{alias + "/" + e.Key}
	if e.Provider.Name != alias {
		candidates = append(candidates, e.Provider.Name+"/"+e.Key)
	}
	for _, c := range candidates {
		if !used[c] {
			return c
		}
	}
	for n := 2; ; n++ {
		if c := fmt.Sprintf("%s/%s-%d", alias, e.Key, n); !used[c] {
			return c
		}
	}
}

// ModelIDNotes describes every id the table had to rename, so startup can say
// what it did and how to keep it.
func (c *Config) ModelIDNotes() []string {
	var notes []string
	for _, e := range c.modelTable() {
		if e.Call == e.Want {
			continue
		}
		if e.Model.As != "" {
			notes = append(notes, fmt.Sprintf(
				"provider %q model %q asked for id %q, which another model already claims; advertised as %q instead",
				e.Provider.Name, e.Key, e.Model.As, e.Call))
			continue
		}
		notes = append(notes, fmt.Sprintf(
			"provider %q model %q is advertised as %q because more than one provider claims that id; set \"as\" to choose the id yourself",
			e.Provider.Name, e.Key, e.Call))
	}
	return notes
}

// ResolveBackends maps a requested model id onto the providers that can
// serve it. Most ids resolve to exactly one backend; a unified group id
// resolves to every provider serving the group, in provider-name order.
// Rules, in order:
//
//  1. an explicit entry in "routes"
//  2. an advertised id: a model's "as", or the id ModelTable settled on for it
//     (its config key, or the "<provider>/<key>" form when the key clashes).
//     Advertised ids are unique, so this is also how a renamed model is reached
//     when its key belongs to another provider too.
//  3. an exact key in a provider's models (the key is what goes upstream,
//     unless the model sets "upstreamName")
//  4. "<provider-alias>/<model>" namespace form, e.g. "llm/minimax-m3" for the
//     provider "LLM Povider" when it exposes "minimax-m3"
//  5. routing.defaultProvider, or the only provider, when nothing matched
func (c *Config) ResolveBackends(id string) ([]Backend, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("no model in request")
	}
	refs := c.Providers()
	if len(refs) == 0 {
		return nil, errors.New("no Providers configured")
	}

	if name, ok := c.Routes[id]; ok {
		if ref := c.ProviderByName(name); ref != nil {
			return []Backend{{Provider: ref.Provider, Name: name, Upstream: upstreamModel(ref.Provider, id)}}, nil
		}
		return nil, fmt.Errorf("route %q points at unknown provider %q", id, name)
	}

	table := c.modelTable()

	// An advertised id is exact and unique by construction, so it settles the
	// request before the looser rules below get a chance to be ambiguous. A
	// group entry expands to every backend it unifies.
	for _, e := range table {
		if e.Call == id {
			if len(e.Backends) > 0 {
				out := make([]Backend, 0, len(e.Backends))
				for _, b := range e.Backends {
					out = append(out, Backend{Provider: b.Provider.Provider, Name: b.Provider.Name, Upstream: b.Upstream})
				}
				return out, nil
			}
			return []Backend{{Provider: e.Provider.Provider, Name: e.Provider.Name, Upstream: e.Upstream}}, nil
		}
	}

	var hits []ProviderRef
	for _, ref := range refs {
		if _, ok := ref.Provider.Models[id]; ok {
			hits = append(hits, ref)
		}
	}
	single := func(ref ProviderRef) ([]Backend, error) {
		return []Backend{{Provider: ref.Provider, Name: ref.Name, Upstream: upstreamModel(ref.Provider, id)}}, nil
	}
	switch len(hits) {
	case 1:
		return single(hits[0])
	case 0:
		// Namespace form: the prefix picks the provider, the rest is the model.
		if i := strings.Index(id, "/"); i > 0 {
			prefix, rest := id[:i], id[i+1:]
			ref, found, err := c.resolvePrefix(prefix)
			if err != nil {
				return nil, fmt.Errorf("model %q: %w", id, err)
			}
			if found {
				if _, ok := ref.Provider.Models[rest]; ok {
					return []Backend{{Provider: ref.Provider, Name: ref.Name, Upstream: upstreamModel(ref.Provider, rest)}}, nil
				}
				return []Backend{{Provider: ref.Provider, Name: ref.Name, Upstream: id}}, nil
			}
		}
		// Unknown model: let the upstream answer with its own 404, as long as
		// there is an obvious provider to send it to.
		if c.Routing.DefaultProvider != "" {
			if ref := c.ProviderByName(c.Routing.DefaultProvider); ref != nil {
				return []Backend{{Provider: ref.Provider, Name: ref.Name, Upstream: upstreamModel(ref.Provider, id)}}, nil
			}
		}
		if len(refs) == 1 {
			return []Backend{{Provider: refs[0].Provider, Name: refs[0].Name, Upstream: upstreamModel(refs[0].Provider, id)}}, nil
		}
		return nil, fmt.Errorf("unknown model %q; add it under a provider or set routing.defaultProvider (known: %s)",
			id, strings.Join(c.ModelIDs(), ", "))
	default:
		// The bare key is shared, so name the ids that do work.
		names := make([]string, 0, len(hits))
		alts := make([]string, 0, len(hits))
		for _, h := range hits {
			names = append(names, h.Name)
			for _, e := range table {
				if e.Provider.Provider == h.Provider && (e.Key == id || groupMentions(e, id)) {
					dup := false
					for _, a := range alts {
						if a == e.Call {
							dup = true
							break
						}
					}
					if !dup {
						alts = append(alts, e.Call)
					}
				}
			}
		}
		return nil, fmt.Errorf("ambiguous model %q, offered by %s; call one of %s, or add a routes entry",
			id, strings.Join(names, ", "), strings.Join(alts, ", "))
	}
}

// groupMentions reports whether the entry's group contains a member under
// the given config key, so the ambiguous-key error can name the group id
// a client should call instead.
func groupMentions(e modelRef, key string) bool {
	for _, b := range e.Backends {
		if b.Key == key {
			return true
		}
	}
	return false
}

// ResolveModel maps a requested model id onto a provider plus the model id to
// send upstream. It is ResolveBackends' first answer; the router walks the
// whole list for a unified model instead.
func (c *Config) ResolveModel(id string) (*Provider, string, error) {
	backends, err := c.ResolveBackends(id)
	if err != nil {
		return nil, "", err
	}
	return backends[0].Provider, backends[0].Upstream, nil
}

// ModelCard resolves the single-model request path. It accepts anything that
// reaches a configured model - an advertised id, a config key, or the
// "<provider-alias>/<model>" namespace form - and returns the owning provider
// and the model itself, so the caller can read both the provider's name and
// its Dialect.
func (c *Config) ModelCard(id string) (ProviderRef, *Model, error) {
	table := c.modelTable()
	for i := range table {
		if table[i].Call == id {
			return table[i].Provider, table[i].Model, nil
		}
	}
	var hits []*modelRef
	for i := range table {
		if table[i].Key == id {
			hits = append(hits, &table[i])
		}
	}
	switch len(hits) {
	case 1:
		return hits[0].Provider, hits[0].Model, nil
	case 0:
	default:
		alts := make([]string, 0, len(hits))
		for _, h := range hits {
			alts = append(alts, h.Call)
		}
		return ProviderRef{}, nil, fmt.Errorf("ambiguous model %q; call one of %s", id, strings.Join(alts, ", "))
	}

	// The advertised table skips hidden providers and hidden models. They
	// still resolve by their exact key, because Model.Hidden and
	// Provider.Hidden promise that an unlisted entry can serve a request. The
	// media body ceiling depends on this: a hidden media model must resolve
	// here or its requests are refused at the size gate.
	var hidden []modelRef
	for _, ref := range c.Providers() {
		if m, ok := ref.Provider.Models[id]; ok && m != nil {
			hidden = append(hidden, modelRef{Provider: ref, Key: id, Model: m,
				Upstream: upstreamModel(ref.Provider, id)})
		}
	}
	switch len(hidden) {
	case 1:
		return hidden[0].Provider, hidden[0].Model, nil
	case 0:
	default:
		alts := make([]string, 0, len(hidden))
		for i := range hidden {
			alts = append(alts, hidden[i].Key)
		}
		return ProviderRef{}, nil, fmt.Errorf("ambiguous model %q; call one of %s", id, strings.Join(alts, ", "))
	}

	if i := strings.Index(id, "/"); i > 0 {
		prefix, rest := id[:i], id[i+1:]
		ref, found, err := c.resolvePrefix(prefix)
		if err != nil {
			return ProviderRef{}, nil, fmt.Errorf("model %q: %w", id, err)
		}
		if found {
			for k := range table {
				if table[k].Provider.Provider == ref.Provider && table[k].Key == rest {
					return table[k].Provider, table[k].Model, nil
				}
			}
			if m, ok := ref.Provider.Models[rest]; ok && m != nil {
				return ref, m, nil
			}
		}
	}
	return ProviderRef{}, nil, fmt.Errorf("unknown model %q", id)
}

// ModelBackends lists the provider names a group id resolves to, or nil
// for an ordinary single-provider model, so the model card can say which
// endpoints stand behind the one id.
func (c *Config) ModelBackends(id string) []string {
	for _, e := range c.modelTable() {
		if e.Call != id {
			continue
		}
		if len(e.Backends) == 0 {
			return nil
		}
		names := make([]string, 0, len(e.Backends))
		for _, b := range e.Backends {
			names = append(names, b.Provider.Name)
		}
		return names
	}
	return nil
}

// DefaultProvider returns the provider to use for a request that names no
// model at all (a plain GET passthrough, for example). It prefers
// routing.defaultProvider and otherwise only works when a single provider is
// configured, so a model-less request is never sent somewhere surprising.
func (c *Config) DefaultProvider() (*Provider, error) {
	refs := c.Providers()
	if len(refs) == 0 {
		return nil, errors.New("no Providers configured")
	}
	if c.Routing.DefaultProvider != "" {
		if ref := c.ProviderByName(c.Routing.DefaultProvider); ref != nil {
			return ref.Provider, nil
		}
		return nil, fmt.Errorf("routing.defaultProvider %q is not a configured provider", c.Routing.DefaultProvider)
	}
	if len(refs) == 1 {
		return refs[0].Provider, nil
	}
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Name)
	}
	return nil, fmt.Errorf("request has no model and %d Providers are configured (%s); set routing.defaultProvider",
		len(refs), strings.Join(names, ", "))
}

// upstreamModel returns the id to send to the provider for a config key,
// honouring a model's "upstreamName" override.
func upstreamModel(p *Provider, key string) string {
	if m, ok := p.Models[key]; ok && m != nil && m.Upstream != "" {
		return m.Upstream
	}
	return key
}

// ModelIDs lists every model id a client can ask for, which is what the router
// advertises: a model's "as", or the id ModelTable settled on for it.
func (c *Config) ModelIDs() []string {
	table := c.modelTable()
	ids := make([]string, 0, len(table))
	for _, e := range table {
		ids = append(ids, e.Call)
	}
	sort.Strings(ids)
	return ids
}

// APIKeyFor picks one key from a provider's pool, honouring the pool's
// strategy. An entry written "file:<path>" is read from disk, and reread
// whenever that file changes, so rotating it needs no restart; a key file may
// hold several keys, one per line, each of which joins the pool. An unreadable
// or empty file contributes no keys rather than failing the request, so a
// provider left without keys still works keyless.
//
// i is the request's sequence number, which is what makes "rotate" advance one
// step per request instead of handing the same key to a burst.
func (p *Provider) APIKeyFor(i int) string {
	pool := p.keyPool()
	if len(pool) == 0 {
		return ""
	}
	switch p.Options.KeyStrategy {
	case keyStrategyFirst:
		return pool[0]
	case keyStrategyRandom:
		return pool[rand.Intn(len(pool))]
	default: // rotate
		return pool[i%len(pool)]
	}
}

// EndpointPool flattens the configured endpoint entries into the URLs they
// stand for: a literal is itself, a "file:" path is every endpoint in that
// file, one per line. Blank lines and "#" comments are ignored, like in the
// key and proxy lists.
//
// The pool is options.baseURLs when it set any, else the single
// options.baseURL. A configured pool that resolves to nothing - a missing or
// empty file - falls back to baseURL rather than taking the provider down:
// no endpoint at all is the one failure a router cannot route around.
func (p *Provider) EndpointPool() []string {
	entries := p.Options.BaseURLs
	var pool []string
	for _, e := range entries {
		pool = append(pool, readKeyEntry(e)...)
	}
	if len(pool) == 0 && p.Options.BaseURL != "" {
		return readKeyEntry(p.Options.BaseURL)
	}
	return pool
}

// BaseURLFor picks the upstream endpoint for one attempt, honouring
// endpointStrategy. seq is the request's sequence number and attempt is the
// try within it; the strategy decides how the two are used (see PickEndpoint).
//
// An empty answer means the provider is not reachable at all (no baseURL and
// no pool, or a "file:" pool that has nothing in it); the caller refuses the
// request rather than forwarding it nowhere.
func (p *Provider) BaseURLFor(seq, attempt int) string {
	return p.Options.PickEndpoint(p.EndpointPool(), seq, attempt)
}

// PickEndpoint chooses one endpoint from pool for one attempt:
//
//	rotate   the next endpoint per request, wrapping; a retry walks on from there
//	primary  always the first endpoint, and a retry the one after it - the
//	         router only reaches the next attempt when the previous one failed
//	first    the first endpoint, always, retries included: the spares stay
//	         untouched until a hand changes the config
//	random   a fresh pick per attempt
//
// An empty pool answers "" for the caller to refuse on.
func (o ProviderOptions) PickEndpoint(pool []string, seq, attempt int) string {
	if len(pool) == 0 {
		return ""
	}
	switch NormalizeEndpointStrategy(o.EndpointStrategy) {
	case endpointStrategyFirst:
		return pool[0]
	case endpointStrategyRandom:
		return pool[rand.Intn(len(pool))]
	case endpointStrategyPrimary:
		return pool[attempt%len(pool)]
	default: // rotate
		return pool[(seq+attempt)%len(pool)]
	}
}

// keyPool flattens the configured entries into the keys they stand for: a
// literal is itself, a "file:" path is every key in that file. Blank lines and
// "#" comments in the file are ignored, like in the proxy list.
func (p *Provider) keyPool() []string {
	entries := p.Options.APIKeys
	if len(entries) == 0 {
		entries = KeyList{p.Options.APIKey}
	}
	var pool []string
	for _, e := range entries {
		pool = append(pool, readKeyEntry(e)...)
	}
	return pool
}

// ExpandPath resolves a leading ~ and any environment variables.
func ExpandPath(path string) string {
	if path == "" {
		return path
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path[1:], "/"))
		}
	}
	return os.ExpandEnv(path)
}

func BoolPtr(b bool) *bool { return &b }

// DefaultTestURLs are the https-only endpoints a proxy is checked against.
// They are https on purpose: the router's upstreams are https, so a proxy
// must support CONNECT tunneling to be of any use, and a check that accepted
// plain-http answers would keep http-only forwarders that fail every real
// request.
func DefaultTestURLs() []string {
	return []string{
		"https://api.ipify.org?format=json",
		"https://icanhazip.com",
	}
}

// MetaPathFor is the sidecar path that stores per-proxy latency and exit IP.
func MetaPathFor(listPath string) string {
	ext := filepath.Ext(listPath)
	return strings.TrimSuffix(listPath, ext) + ".meta" + ext
}

// ProviderName is the config name of the provider a pointer belongs to, or
// "?" when it is not part of the config.
func ProviderName(cfg *Config, want *Provider) string {
	for _, ref := range cfg.Providers() {
		if ref.Provider == want {
			return ref.Name
		}
	}
	return "?"
}

// Exported kind names, for callers outside the package (the tests, and any
// future integration).
const (
	KindChat       = kindChat
	KindImage      = kindImage
	KindImageEdit  = kindImageEdit
	KindSystemOne  = kindSystemOne
	KindEmbedding  = kindEmbedding
	KindAudio      = kindAudio
	KindTTS        = kindTTS
	KindSTT        = kindSTT
	KindOCR        = kindOCR
	KindModeration = kindModeration
	KindVideo      = kindVideo
)

// Exported key-pool strategy names, for callers outside the package.
const (
	KeyStrategyRotate = keyStrategyRotate
	KeyStrategyFirst  = keyStrategyFirst
	KeyStrategyRandom = keyStrategyRandom
)

// Exported endpoint-pool strategy names, for callers outside the package.
const (
	EndpointStrategyRotate  = endpointStrategyRotate
	EndpointStrategyFirst   = endpointStrategyFirst
	EndpointStrategyRandom  = endpointStrategyRandom
	EndpointStrategyPrimary = endpointStrategyPrimary
)

// SetPath records where the config was loaded from. applyDefaults uses it to
// decide whether to expand the proxy-list path, so tests set it to exercise
// path handling without touching the real ~/.noderouter.
func (c *Config) SetPath(p string) { c.path = p }
