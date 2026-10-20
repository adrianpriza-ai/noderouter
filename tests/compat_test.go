package tests

// The router is split into config, proxy and router packages. Rather than
// qualify every reference inside the tests, this file aliases the types and
// wraps the few helpers, so the test bodies stay about behaviour rather than
// package plumbing. It also installs the site filesystem the page-split tests
// need (the binary uses the embedded copies; here we read them from disk).

import (
	"context"
	"io/fs"
	"net/url"
	"os"
	"time"

	"noderouter/config"
	"noderouter/logx"
	"noderouter/proxy"
	"noderouter/router"
)

type (
	Config               = config.Config
	ServerConfig         = config.ServerConfig
	AuthConfig           = config.AuthConfig
	RoutingConfig        = config.RoutingConfig
	ProxyConfig          = config.ProxyConfig
	ProxyTest            = config.ProxyTest
	RetryConfig          = config.RetryConfig
	EndpointHealthConfig = config.EndpointHealthConfig
	Provider             = config.Provider
	ProviderOptions      = config.ProviderOptions
	ProviderRef          = config.ProviderRef
	Model                = config.Model
	ModelLimit           = config.ModelLimit
	Modalities           = config.Modalities
	keyList              = config.KeyList

	Pool          = proxy.Pool
	PoolStats     = proxy.PoolStats
	ProxyEntry    = proxy.ProxyEntry
	HarvestResult = proxy.HarvestResult
	Schedule      = proxy.Schedule

	RevalidateResult = proxy.RevalidateResult

	Server     = router.Server
	BodyBudget = router.BodyBudget
	Logger     = logx.Logger
)

const (
	defaultRetryAttempts     = config.DefaultRetryAttempts
	defaultRetryBackoffMS    = config.DefaultRetryBackoffMS
	defaultRetryMaxBackoffMS = config.DefaultRetryMaxBackoffMS
	defaultBodyBudgetMinMB   = config.DefaultBodyBudgetMinMB
)

var (
	NewServer = router.NewServer
	NewPool   = proxy.NewPool

	NormalizeProxy   = proxy.NormalizeProxy
	SplitStale       = proxy.SplitStale
	MergeRevalidated = proxy.MergeRevalidated
	TestProxies      = proxy.TestProxies
	SaveProxyList    = proxy.SaveProxyList
	LoadProxyList    = proxy.LoadProxyList
	Harvest          = proxy.Harvest
	CheckProxy       = proxy.CheckProxy
	Revalidate       = proxy.Revalidate
	FetchRawList     = proxy.FetchRawList
	LoadProxiesFile  = proxy.LoadProxiesFile
	ParseSchedule    = proxy.ParseSchedule
	NewTransport     = proxy.NewTransport
	ParseProxyURL    = proxy.ParseProxyURL

	DefaultTestURLs = config.DefaultTestURLs
	ExpandPath      = config.ExpandPath
	MetaPathFor     = config.MetaPathFor

	NewBodyBudget = router.NewBodyBudget
)

func newLogger(verbose bool) *logx.Logger                { return logx.New(verbose) }
func boolPtr(b bool) *bool                               { return config.BoolPtr(b) }
func expandPath(p string) string                         { return config.ExpandPath(p) }
func metaPathFor(p string) string                        { return config.MetaPathFor(p) }
func isLoopbackHost(host string) bool                    { return router.IsLoopbackHost(host) }
func sleepCtx(ctx context.Context, d time.Duration) bool { return config.SleepCtx(ctx, d) }
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	return config.ParseRetryAfter(v, now)
}
func providerName(cfg *Config, want *Provider) string { return config.ProviderName(cfg, want) }
func resolveUpstream(base, path, raw string) (*url.URL, error) {
	return router.ResolveUpstream(base, path, raw)
}
func replaceModel(body []byte, model string) ([]byte, bool) {
	return router.ReplaceModel(body, "application/json", model)
}
func replaceModelCT(body []byte, contentType, model string) ([]byte, bool) {
	return router.ReplaceModel(body, contentType, model)
}

func init() {
	root := os.DirFS("..")
	site, err := fs.Sub(root, "website")
	if err != nil {
		panic("test site fs: " + err.Error())
	}
	docs, err := fs.Sub(root, "docs")
	if err != nil {
		panic("test docs fs: " + err.Error())
	}
	router.SetSiteFS(site, docs)
}

// Kind names, matching the config package's categories.
const (
	kindChat       = config.KindChat
	kindImage      = config.KindImage
	kindImageEdit  = config.KindImageEdit
	kindSystemOne  = config.KindSystemOne
	kindEmbedding  = config.KindEmbedding
	kindAudio      = config.KindAudio
	kindTTS        = config.KindTTS
	kindSTT        = config.KindSTT
	kindOCR        = config.KindOCR
	kindModeration = config.KindModeration
	kindVideo      = config.KindVideo
)

// Key-pool strategy names.
const (
	keyStrategyRotate = config.KeyStrategyRotate
	keyStrategyFirst  = config.KeyStrategyFirst
	keyStrategyRandom = config.KeyStrategyRandom
)

// Endpoint-pool strategy names.
const (
	endpointStrategyRotate  = config.EndpointStrategyRotate
	endpointStrategyFirst   = config.EndpointStrategyFirst
	endpointStrategyRandom  = config.EndpointStrategyRandom
	endpointStrategyPrimary = config.EndpointStrategyPrimary
)

// Proxy re-validation window default.
const defaultReValidateAfter = config.DefaultReValidateAfter

// Exported helpers the tests reach for directly.
var LoadConfig = config.LoadConfig

const maxProxyFails = proxy.MaxProxyFails

var hostPortKey = proxy.HostPortKey

var (
	normalizeKeys        = config.NormalizeKeys
	normalizeKeyStrategy = config.NormalizeKeyStrategy
	newProxyClient       = proxy.NewProxyClient
	bareScheme           = proxy.BareScheme
	sourceURLs           = proxy.SourceURLs
)

var (
	sourceEndpoint = proxy.SourceEndpoint
	schemeAllowed  = proxy.SchemeAllowed
)

const (
	defaultListenHost     = config.DefaultListenHost
	defaultPortsPath      = config.DefaultPortsPath
	defaultStatusPath     = config.DefaultStatusPath
	defaultStatusSaveSecs = config.DefaultStatusSaveSecs
)
