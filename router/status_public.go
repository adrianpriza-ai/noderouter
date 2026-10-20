package router

import (
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// StatusTracker keeps a rolling window of request outcomes. Global, per-model,
// per-provider and per model+provider hourly buckets are all retained for
// StatusRetention (7 days by default) and pruned on every write and read.
//
// The buckets live in RAM, and can also be kept in a file so a restart does not
// reset the window; see status_store.go, which owns everything file-shaped.
type StatusTracker struct {
	mu        sync.RWMutex
	retention time.Duration
	// file is where the history is kept between runs, or "" for memory only.
	file string
	// gen counts changes (records and loads) and savedGen counts what the
	// last successful save wrote; they differ exactly while there is
	// something new to write.
	gen, savedGen uint64
	// repaired is how many hours the last load had to correct; see
	// repairDoubleCountedGlobal.
	repaired int
	// global buckets cover every recorded request.
	buckets map[int64]*statusBucket
	// per-key buckets: model id / provider name -> hour -> bucket.
	models    map[string]map[int64]*statusBucket
	providers map[string]map[int64]*statusBucket
	// model -> provider -> hour, so a model's traffic can be split across the
	// providers that served it instead of inferred from two rollups that each
	// counted the requests.
	pairs map[string]map[string]map[int64]*statusBucket
}

type statusBucket struct {
	Total   int
	Success int
	Failed  int
}

// NewStatusTracker creates a tracker with the given retention window. A
// non-positive value falls back to 7 days, the storage-efficient default.
func NewStatusTracker(retention time.Duration) *StatusTracker {
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	return &StatusTracker{
		retention: retention,
		buckets:   make(map[int64]*statusBucket),
		models:    make(map[string]map[int64]*statusBucket),
		providers: make(map[string]map[int64]*statusBucket),
		pairs:     make(map[string]map[string]map[int64]*statusBucket),
	}
}

// Retention returns the configured retention window.
func (t *StatusTracker) Retention() time.Duration { return t.retention }

// retentionDays is the retention expressed in whole days.
func (t *StatusTracker) retentionDays() int {
	days := int(t.retention / (24 * time.Hour))
	if days < 1 {
		days = 1
	}
	return days
}

func bucketKey(at time.Time) int64 {
	return at.UTC().Truncate(time.Hour).Unix()
}

func (t *StatusTracker) pruneLocked(now time.Time) {
	cutoff := now.UTC().Add(-t.retention).Truncate(time.Hour).Unix()
	for k := range t.buckets {
		if k < cutoff {
			delete(t.buckets, k)
		}
	}
	for name, m := range t.models {
		for k := range m {
			if k < cutoff {
				delete(m, k)
			}
		}
		// A model with no hour left inside the window is no longer a model the
		// page should list: an empty entry would render as a row reporting
		// "no requests in the window" for traffic that aged out.
		if len(m) == 0 {
			delete(t.models, name)
		}
	}
	for name, p := range t.providers {
		for k := range p {
			if k < cutoff {
				delete(p, k)
			}
		}
		if len(p) == 0 {
			delete(t.providers, name)
		}
	}
	for model, byProvider := range t.pairs {
		for name, p := range byProvider {
			for k := range p {
				if k < cutoff {
					delete(p, k)
				}
			}
			// A pair with nothing left inside the window is gone for good:
			// keeping the empty map would grow the model list forever.
			if len(p) == 0 {
				delete(byProvider, name)
			}
		}
		if len(byProvider) == 0 {
			delete(t.pairs, model)
		}
	}
}

func (t *StatusTracker) ensureModelLocked(name string) map[int64]*statusBucket {
	if name == "" {
		return nil
	}
	if _, ok := t.models[name]; !ok {
		t.models[name] = make(map[int64]*statusBucket)
	}
	return t.models[name]
}

func (t *StatusTracker) ensureProviderLocked(name string) map[int64]*statusBucket {
	if name == "" {
		return nil
	}
	if _, ok := t.providers[name]; !ok {
		t.providers[name] = make(map[int64]*statusBucket)
	}
	return t.providers[name]
}

// bucketFor returns the hour's bucket in m, making it on the first request of
// that hour. The caller holds the lock.
func bucketFor(m map[int64]*statusBucket, hour int64) *statusBucket {
	b, ok := m[hour]
	if !ok {
		b = &statusBucket{}
		m[hour] = b
	}
	return b
}

func (b *statusBucket) add(success bool) {
	b.Total++
	if success {
		b.Success++
	} else {
		b.Failed++
	}
}

// Record adds one request outcome to the global *and* the per-model /
// per-provider / per model+provider buckets - the whole picture of one request,
// which is what a caller that saw the request from end to end has. model and
// provider are optional and may be passed positionally after the timestamp.
//
// Inside the server, a forwarded request is recorded in two halves instead:
// the middleware counts its global outcome, and the forward path counts the
// detail with RecordDetail. Calling Record from the forward path as well would
// count one forwarded request twice in the global rollup - see RecordDetail.
func (t *StatusTracker) Record(success bool, statusCode int, at time.Time, keys ...string) {
	model, provider := "", ""
	if len(keys) > 0 {
		model = keys[0]
	}
	if len(keys) > 1 {
		provider = keys[1]
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	bucketFor(t.buckets, bucketKey(at)).add(success)
	t.addDetailLocked(success, at, model, provider)

	t.pruneLocked(at)
	t.gen++
}

// RecordDetail adds one request outcome to the per-model, per-provider and per
// model+provider buckets only, leaving the global rollup alone.
//
// A request is counted once in each rollup, by whoever saw it best. The global
// rollup is the request as a whole - one entry per request, pass or fail, for
// paths that never resolved a model to a provider - so the middleware around
// every handler owns it. Which model was asked for and which provider answered
// is known only in the forward path, so that half is recorded here. Doing both
// there is what made a forwarded request read as two requests globally, and the
// global uptime the page leads with is computed from that total.
func (t *StatusTracker) RecordDetail(success bool, at time.Time, model, provider string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.addDetailLocked(success, at, model, provider)

	t.pruneLocked(at)
	t.gen++
}

// addDetailLocked adds one outcome to the model, provider and model+provider
// buckets. The caller holds the lock and prunes.
func (t *StatusTracker) addDetailLocked(success bool, at time.Time, model, provider string) {
	hour := bucketKey(at)

	if mb := t.ensureModelLocked(model); mb != nil {
		bucketFor(mb, hour).add(success)
	}
	if pb := t.ensureProviderLocked(provider); pb != nil {
		bucketFor(pb, hour).add(success)
	}

	// model + provider together, so the split the page shows is the traffic
	// that really went through each provider.
	if model == "" || provider == "" {
		return
	}
	byProvider, ok := t.pairs[model]
	if !ok {
		byProvider = make(map[string]map[int64]*statusBucket)
		t.pairs[model] = byProvider
	}
	pair, ok := byProvider[provider]
	if !ok {
		pair = make(map[int64]*statusBucket)
		byProvider[provider] = pair
	}
	bucketFor(pair, hour).add(success)
}

// RecordRequest is a convenience wrapper for callers that only know the
// boolean outcome.
func (t *StatusTracker) RecordRequest(success bool) {
	code := http.StatusOK
	if !success {
		code = http.StatusInternalServerError
	}
	t.Record(success, code, time.Now())
}

// Snapshot returns the current status as a JSON-friendly map, including
// per-model and per-provider breakdowns.
func (t *StatusTracker) Snapshot() map[string]any {
	t.mu.Lock()
	t.pruneLocked(time.Now())
	t.mu.Unlock()

	t.mu.RLock()
	defer t.mu.RUnlock()

	now := time.Now().UTC()
	cutoff := now.Add(-t.retention).Truncate(time.Hour)
	retentionDays := t.retentionDays()

	// global
	g := rangeStats(t.buckets, cutoff.Unix())
	gUptime := percent(g.Success, g.Total)
	gStatus := statusLabel(gUptime, g.Total)

	// models, each with the providers that served it.
	modelsMap := make(map[string]any)
	modelList := make([]map[string]any, 0, len(t.models))
	for _, name := range sortedKeys(t.models) {
		entry := t.statsEntry(name, t.models[name], cutoff.Unix(), now, retentionDays)
		providers := sortedKeys(t.pairs[name])
		split := make([]map[string]any, 0, len(providers))
		for _, providerName := range providers {
			split = append(split, t.statsEntry(providerName, t.pairs[name][providerName], cutoff.Unix(), now, retentionDays))
		}
		entry["providers"] = providers
		entry["providerStats"] = split
		modelsMap[name] = entry
		modelList = append(modelList, entry)
	}

	// providers, each with the models it served. A model's row under a provider
	// carries that provider's numbers rather than the model's totals from
	// everywhere, so a model on two providers is not counted twice under both.
	modelsByProvider := make(map[string][]string)
	for modelName, byProvider := range t.pairs {
		for providerName := range byProvider {
			modelsByProvider[providerName] = append(modelsByProvider[providerName], modelName)
		}
	}
	providersMap := make(map[string]any)
	providerList := make([]map[string]any, 0, len(t.providers))
	for _, name := range sortedKeys(t.providers) {
		entry := t.statsEntry(name, t.providers[name], cutoff.Unix(), now, retentionDays)
		served := modelsByProvider[name]
		sort.Strings(served)
		servedStats := make([]map[string]any, 0, len(served))
		for _, modelName := range served {
			servedStats = append(servedStats, t.statsEntry(modelName, t.pairs[modelName][name], cutoff.Unix(), now, retentionDays))
		}
		entry["models"] = served
		entry["modelStats"] = servedStats
		providersMap[name] = entry
		providerList = append(providerList, entry)
	}

	payload := map[string]any{
		"status":             gStatus,
		"uptime":             uptimeText(gUptime, g.Total),
		"uptimePercent":      gUptime,
		"uptimePercent7d":    gUptime,
		"window":             fmt.Sprintf("%dd", retentionDays),
		"retentionDays":      retentionDays,
		"retentionSeconds":   int(t.retention.Seconds()),
		"total":              g.Total,
		"totalRequests":      g.Total,
		"requests":           g.Total,
		"success":            g.Success,
		"successful":         g.Success,
		"successfulRequests": g.Success,
		"failed":             g.Failed,
		"failedRequests":     g.Failed,
		"from":               cutoff.Format(time.RFC3339),
		"to":                 now.Format(time.RFC3339),
		"generatedAt":        now.Format(time.RFC3339),
		"hours":              bucketList(t.buckets, g.Keys, cutoff.Unix()),
		"days":               aggregateDays(now, retentionDays, g.Keys, t.buckets),
		"models":             modelsMap,
		"byModel":            modelsMap,
		"modelStats":         modelList,
		"perModel":           modelList,
		"providers":          providersMap,
		"byProvider":         providersMap,
		"providerStats":      providerList,
		"perProvider":        providerList,
	}
	return payload
}

func sortBucketKeys(bk map[int64]*statusBucket, cutoff int64) []int64 {
	keys := make([]int64, 0, len(bk))
	for k := range bk {
		if k >= cutoff {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func bucketList(bk map[int64]*statusBucket, keys []int64, cutoff int64) []map[string]any {
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		b := bk[k]
		ts := time.Unix(k, 0).UTC()
		pct := percent(b.Success, b.Total)
		out = append(out, map[string]any{
			"hour":          ts.Format(time.RFC3339),
			"timestamp":     k,
			"total":         b.Total,
			"totalRequests": b.Total,
			"success":       b.Success,
			"successful":    b.Success,
			"failed":        b.Failed,
			"uptimePercent": pct,
		})
	}
	return out
}

func aggregateDays(now time.Time, retentionDays int, keys []int64, buckets map[int64]*statusBucket) []map[string]any {
	type agg struct {
		Total, Success, Failed int
	}
	byDay := make(map[string]*agg)
	for _, k := range keys {
		b := buckets[k]
		day := time.Unix(k, 0).UTC().Format("2006-01-02")
		a, ok := byDay[day]
		if !ok {
			a = &agg{}
			byDay[day] = a
		}
		a.Total += b.Total
		a.Success += b.Success
		a.Failed += b.Failed
	}

	days := make([]map[string]any, 0, retentionDays)
	for i := retentionDays - 1; i >= 0; i-- {
		day := now.AddDate(0, 0, -i).Format("2006-01-02")
		a := byDay[day]
		if a == nil {
			a = &agg{}
		}
		pct := percent(a.Success, a.Total)
		days = append(days, map[string]any{
			"date":               day,
			"total":              a.Total,
			"totalRequests":      a.Total,
			"success":            a.Success,
			"successful":         a.Success,
			"successfulRequests": a.Success,
			"failed":             a.Failed,
			"failedRequests":     a.Failed,
			"uptimePercent":      pct,
			"uptime":             uptimeText(pct, a.Total),
		})
	}
	return days
}

// windowStats is one set of buckets summed over the retention window, with the
// keys kept so the hourly and daily rollups are built from the same pass.
type windowStats struct {
	Keys                   []int64
	Total, Success, Failed int
}

func rangeStats(buckets map[int64]*statusBucket, cutoff int64) windowStats {
	keys := sortBucketKeys(buckets, cutoff)
	s := windowStats{Keys: keys}
	for _, k := range keys {
		b := buckets[k]
		s.Total += b.Total
		s.Success += b.Success
		s.Failed += b.Failed
	}
	return s
}

// statsEntry is the shape the status page reads for a model, a provider, or one
// model's traffic through one provider: the totals, the uptime they imply, and
// the hourly and daily rollups behind them.
func (t *StatusTracker) statsEntry(name string, buckets map[int64]*statusBucket, cutoff int64, now time.Time, retentionDays int) map[string]any {
	s := rangeStats(buckets, cutoff)
	u := percent(s.Success, s.Total)
	return map[string]any{
		"name":               name,
		"total":              s.Total,
		"totalRequests":      s.Total,
		"success":            s.Success,
		"successful":         s.Success,
		"successfulRequests": s.Success,
		"failed":             s.Failed,
		"failedRequests":     s.Failed,
		"uptime":             uptimeText(u, s.Total),
		"uptimePercent":      u,
		"days":               aggregateDays(now, retentionDays, s.Keys, buckets),
		"hours":              bucketList(buckets, s.Keys, cutoff),
	}
}

// sortedKeys returns a map's keys in order, so every list the page renders has
// a stable order of its own rather than the map's.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func percent(success, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(success) / float64(total) * 100
}

func uptimeText(pct float64, total int) string {
	if total <= 0 {
		return "no data"
	}
	return fmt.Sprintf("%.2f%%", pct)
}

func statusLabel(uptime float64, total int) string {
	if total == 0 {
		return "operational"
	}
	switch {
	case uptime >= 99:
		return "operational"
	case uptime >= 95:
		return "degraded"
	default:
		return "down"
	}
}

// withStatusTracking records API request outcomes for the public status page.
// It runs outside recovery so a panic that became a 500 is still counted as a
// failure.
func (s *Server) withStatusTracking(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusResponseRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		path := r.URL.Path
		if shouldSkipStatusRecording(path) {
			return
		}
		s.statusTracker().Record(rec.code < http.StatusBadRequest, rec.code, time.Now())
	})
}

func shouldSkipStatusRecording(path string) bool {
	// The public status page describes the LLM API, not website, health,
	// admin, or arbitrary passthrough traffic. Keep the boundary checks exact
	// so names such as /v1/models-custom remain ordinary API requests.
	if !strings.HasPrefix(path, "/v1/") {
		return true
	}
	return path == "/v1/models" || strings.HasPrefix(path, "/v1/models/") ||
		path == "/v1/status" || strings.HasPrefix(path, "/v1/status/")
}

type statusResponseRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusResponseRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusResponseRecorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusResponseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) handleStatusPage(w http.ResponseWriter, r *http.Request) {
	if SiteFS != nil {
		if data, err := fs.ReadFile(SiteFS, "status.html"); err == nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(data)
			return
		}
	}
	http.NotFound(w, r)
}

func (s *Server) handlePublicStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.statusTracker().Snapshot())
}

// statusTracker returns the server's public status tracker, building the
// fallback one on first use. s.Status is written in exactly one place - the
// Once body below - so every caller shares one synchronized view of it; the
// ad-hoc assignments that used to live in each helper were an unsynchronised
// field write racing the readers. NewServer always sets Status before the
// server is served, so the lazy branch is only reached by a Server built by
// hand (tests, embedders).
func (s *Server) statusTracker() *StatusTracker {
	s.statusOnce.Do(func() {
		if s.Status == nil {
			s.Status = NewStatusTracker(7 * 24 * time.Hour)
		}
	})
	return s.Status
}

// StatusSnapshot exposes the current public status for external callers/tests.
func (s *Server) StatusSnapshot() map[string]any {
	return s.statusTracker().Snapshot()
}

// RecordStatus lets external callers record an outcome directly.
func (s *Server) RecordStatus(success bool) {
	s.statusTracker().RecordRequest(success)
}

// RecordStatusDetail records which model was asked for and which provider
// answered. It is the detail half of a forwarded request: the global outcome is
// counted by the middleware that sees every request, so recording it here as
// well would count one forwarded request as two - see
// StatusTracker.RecordDetail.
func (s *Server) RecordStatusDetail(success bool, model, provider string) {
	s.statusTracker().RecordDetail(success, time.Now(), model, provider)
}

func (s *Server) recordForwardStatus(r *http.Request, success bool, model, provider string) {
	if shouldSkipStatusRecording(r.URL.Path) {
		return
	}
	s.RecordStatusDetail(success, model, provider)
}
