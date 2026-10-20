package router

// The status tracker keeps its history in RAM, which is the right place to read
// it from and the wrong place to keep the only copy: a restart is routine - an
// update, a config edit, a crash a supervisor picks up - and starting the
// window over every time is exactly what an operator does not want from a
// status page.
//
// This file is the file-backed half. The hourly buckets of all four rollups are
// written out as one JSON document and merged back at startup. Nothing here is
// new information: it is what the router observed itself, with no config, no
// keys and no request or response bodies in it. The only thing that has to be
// right is that a restart continues the same window rather than a fresh one.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// statusFileVersion is the layout of the document below, and it doubles as the
// counting version of what it holds. Version 1 is the document written by the
// build whose global rollup counted a forwarded request twice - the middleware's
// entry plus the forward path's, which a fixed build records only as detail (see
// RecordDetail) - so a version 1 document is repaired once on load and written
// back as version 2. A file from a newer router is refused rather than
// half-read: if its buckets came to mean something else, silently dropping or
// misreading them would be worse than saying so and starting clean.
const (
	statusFileVersion   = 2
	statusLegacyVersion = 1
)

// statusCounts is one hour's outcome for one rollup.
type statusCounts struct {
	Total   int `json:"total"`
	Success int `json:"success"`
	Failed  int `json:"failed"`
}

// statusDocument is the on-disk shape: the same four rollups the tracker holds
// in RAM, with hours as unix-second strings because a JSON object key cannot be
// a number.
type statusDocument struct {
	Version   int                                           `json:"version"`
	SavedAt   time.Time                                     `json:"savedAt"`
	Buckets   map[string]statusCounts                       `json:"buckets,omitempty"`
	Models    map[string]map[string]statusCounts            `json:"models,omitempty"`
	Providers map[string]map[string]statusCounts            `json:"providers,omitempty"`
	Pairs     map[string]map[string]map[string]statusCounts `json:"pairs,omitempty"`
}

// UseFile points the tracker at the file its history is kept in and restores
// what that file holds, so a restarted router continues from the last run
// instead of from zero. The returned count is how many hours the global window
// got back - the hours inside the retention window, not every hour the file
// happens to mention. A missing file is not an error - it is the first run -
// and turns nothing on. An empty path leaves the tracker in memory only.
//
// The file is marked dirty even when the load restored nothing, so the first
// flush writes a document out and the file's presence (and its format, for
// anyone who looks) does not depend on traffic having arrived yet.
func (t *StatusTracker) UseFile(path string) (int, error) {
	if path == "" {
		return 0, nil
	}
	t.mu.Lock()
	t.file = path
	t.repaired = 0
	t.gen++
	t.mu.Unlock()

	restored, err := t.LoadFrom(path)
	if err != nil {
		return 0, err
	}
	return restored, nil
}

// RepairedHours is how many hours the last load had to correct, because they
// were written by the build whose global rollup counted a forwarded request
// twice. It is zero for a document this build wrote, and it is the number to log
// when the history on the page moves for a reason other than traffic.
func (t *StatusTracker) RepairedHours() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.repaired
}

// FilePath is the file the history is kept in, or "" when the tracker is
// memory-only.
func (t *StatusTracker) FilePath() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.file
}

// Dirty reports whether anything has been recorded or restored since the last
// successful save, so a caller can flush only when it would write something new.
func (t *StatusTracker) Dirty() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.gen != t.savedGen
}

// Save writes the history to the tracker's file, if it has one.
func (t *StatusTracker) Save() error {
	path := t.FilePath()
	if path == "" {
		return nil
	}
	return t.SaveTo(path)
}

// SaveIfDirty writes only when something changed since the last save, so the
// periodic flush is a no-op on a quiet router instead of a rewrite of the same
// document every interval.
func (t *StatusTracker) SaveIfDirty() error {
	if !t.Dirty() {
		return nil
	}
	return t.Save()
}

// LoadFrom reads a history document and merges it into the tracker. Hours
// outside the retention window are dropped on the way in, so a file last
// written days ago cannot resurrect expired buckets - or, worse, keep a model
// listed with traffic no page should show. A missing file is not an error.
//
// Nothing is merged until the whole document has been understood, so a file
// this build cannot read leaves the tracker exactly as it was rather than
// half-merged.
func (t *StatusTracker) LoadFrom(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var doc statusDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	if doc.Version > statusFileVersion {
		return 0, fmt.Errorf("%s was written by a newer noderouter (format %d, this build reads %d)", path, doc.Version, statusFileVersion)
	}
	legacy := doc.Version < statusFileVersion

	// Parse first, lock second: a bad hour key is a reason to leave the live
	// history alone, and it is cheaper to find it before touching it.
	global, err := parseHours(doc.Buckets)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	models, err := parseNamedHours(doc.Models)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	providers, err := parseNamedHours(doc.Providers)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	pairs, err := parsePairs(doc.Pairs)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}

	// History from the older counting is corrected before it is merged, so the
	// repaired numbers are what the page shows and what the next save writes
	// out: a version 2 document never has to be repaired again.
	repaired := 0
	if legacy {
		repaired = repairDoubleCountedGlobal(global, models, providers, pairs)
	}

	now := time.Now()
	cutoff := now.UTC().Add(-t.retention).Truncate(time.Hour).Unix()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.repaired = repaired

	restored := 0
	for hour, counts := range global {
		if addHours(t.buckets, hour, cutoff, counts) {
			restored++
		}
	}
	for name, hours := range models {
		dst := t.ensureModelLocked(name)
		if dst == nil {
			continue
		}
		for hour, counts := range hours {
			addHours(dst, hour, cutoff, counts)
		}
	}
	for name, hours := range providers {
		dst := t.ensureProviderLocked(name)
		if dst == nil {
			continue
		}
		for hour, counts := range hours {
			addHours(dst, hour, cutoff, counts)
		}
	}
	for model, byProvider := range pairs {
		if model == "" {
			continue
		}
		for provider, hours := range byProvider {
			if provider == "" {
				continue
			}
			dst, ok := t.pairs[model]
			if !ok {
				dst = make(map[string]map[int64]*statusBucket)
				t.pairs[model] = dst
			}
			pair, ok := dst[provider]
			if !ok {
				pair = make(map[int64]*statusBucket)
				dst[provider] = pair
			}
			for hour, counts := range hours {
				addHours(pair, hour, cutoff, counts)
			}
		}
	}

	// What the window is worth now, not what it was worth when the file was
	// written: a load is a change like any other.
	t.pruneLocked(now)
	t.gen++
	return restored, nil
}

// SaveTo writes the history to path, or to the tracker's own file when path is
// empty. The document goes to a temporary file in the same directory and is
// renamed over the target, so a reader - or a crash mid-write - never sees a
// half-written history.
func (t *StatusTracker) SaveTo(path string) error {
	if path == "" {
		path = t.FilePath()
	}
	if path == "" {
		return nil
	}

	doc, gen := t.snapshotForSave()
	buf, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".noderouter-status-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}

	// Recorded traffic that landed while the document was being written is
	// not in it: leave the tracker dirty so the next flush picks it up.
	t.markSaved(gen)
	return nil
}

// snapshotForSave copies the buckets out of the tracker and returns the
// generation it copied. The copy is what gets marshalled, so the lock is held
// for the copy and not for the write; the generation is what keeps a record
// that raced the write from being forgotten - see markSaved.
func (t *StatusTracker) snapshotForSave() (statusDocument, uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	t.pruneLocked(now)

	doc := statusDocument{
		Version:   statusFileVersion,
		SavedAt:   now.UTC(),
		Buckets:   encodeHours(t.buckets),
		Models:    make(map[string]map[string]statusCounts, len(t.models)),
		Providers: make(map[string]map[string]statusCounts, len(t.providers)),
		Pairs:     make(map[string]map[string]map[string]statusCounts, len(t.pairs)),
	}
	for name, hours := range t.models {
		doc.Models[name] = encodeHours(hours)
	}
	for name, hours := range t.providers {
		doc.Providers[name] = encodeHours(hours)
	}
	for model, byProvider := range t.pairs {
		encoded := make(map[string]map[string]statusCounts, len(byProvider))
		for provider, hours := range byProvider {
			encoded[provider] = encodeHours(hours)
		}
		doc.Pairs[model] = encoded
	}
	return doc, t.gen
}

// markSaved clears the dirty flag, unless something was recorded while the
// document was being written - then the tracker stays dirty and the next flush
// writes those buckets too.
func (t *StatusTracker) markSaved(gen uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gen == gen {
		t.savedGen = gen
	}
}

func encodeHours(hours map[int64]*statusBucket) map[string]statusCounts {
	out := make(map[string]statusCounts, len(hours))
	for hour, b := range hours {
		out[strconv.FormatInt(hour, 10)] = statusCounts{Total: b.Total, Success: b.Success, Failed: b.Failed}
	}
	return out
}

// repairDoubleCountedGlobal removes the extra global entries left by the build
// whose global rollup counted a forwarded request twice, in place.
//
// The excess in an hour is exactly the number of detail records that hour, and
// that number is recoverable from the detail rollups themselves. Every detail
// record added one to its model bucket when it had a model, one to its provider
// bucket when it had a provider, and one to the pair bucket only when it had
// both - so the records are the three disjoint kinds (model only, provider only,
// both) and inclusion-exclusion over them gives models + providers - pairs. The
// same expression over successes and failures gives the outcomes to take off.
// What is left is the count of requests, which is what the global rollup was
// always meant to be.
//
// Counts are clamped at zero rather than trusted to go negative, and an hour
// whose repaired total is not positive is dropped: a document that has already
// been repaired, or one where the detail is the only survivor, must not be
// turned into a negative hour. Returns how many hours changed.
func repairDoubleCountedGlobal(global hourCounts, models, providers map[string]hourCounts, pairs map[string]map[string]hourCounts) int {
	detail := make(map[int64]statusCounts)
	byKind := func(hour int64, c statusCounts, sign int) {
		d := detail[hour]
		d.Total += sign * c.Total
		d.Success += sign * c.Success
		d.Failed += sign * c.Failed
		detail[hour] = d
	}
	for _, hours := range models {
		for hour, c := range hours {
			byKind(hour, c, 1)
		}
	}
	for _, hours := range providers {
		for hour, c := range hours {
			byKind(hour, c, 1)
		}
	}
	for _, byProvider := range pairs {
		for _, hours := range byProvider {
			for hour, c := range hours {
				byKind(hour, c, -1)
			}
		}
	}

	repaired := 0
	for hour, extra := range detail {
		old, ok := global[hour]
		if !ok || extra.Total == 0 {
			continue
		}
		fixed := statusCounts{
			Total:   clampZero(old.Total - extra.Total),
			Success: clampZero(old.Success - extra.Success),
			Failed:  clampZero(old.Failed - extra.Failed),
		}
		if fixed == old {
			continue
		}
		if fixed.Total == 0 {
			delete(global, hour)
		} else {
			global[hour] = fixed
		}
		repaired++
	}
	return repaired
}

func clampZero(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// addHours merges one hour's counts into dst and reports whether it was worth
// restoring: an hour outside the window is dropped here rather than merged and
// immediately pruned, so the caller's count is what the window actually got
// back. Counts that disagree are not something to guess at: every rollup is
// computed from success and failure, so the total is taken as their sum rather
// than trusted on its own.
func addHours(dst map[int64]*statusBucket, hour, cutoff int64, counts statusCounts) bool {
	if hour < cutoff {
		return false
	}
	success, failed := counts.Success, counts.Failed
	if success < 0 {
		success = 0
	}
	if failed < 0 {
		failed = 0
	}
	if success+failed == 0 {
		return false
	}
	if b, ok := dst[hour]; ok {
		b.Success += success
		b.Failed += failed
		b.Total = b.Success + b.Failed
		return true
	}
	dst[hour] = &statusBucket{Total: success + failed, Success: success, Failed: failed}
	return true
}

type hourCounts map[int64]statusCounts

func parseHours(m map[string]statusCounts) (hourCounts, error) {
	out := make(hourCounts, len(m))
	for key, counts := range m {
		hour, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad hour %q", key)
		}
		out[hour] = counts
	}
	return out, nil
}

func parseNamedHours(m map[string]map[string]statusCounts) (map[string]hourCounts, error) {
	out := make(map[string]hourCounts, len(m))
	for name, hours := range m {
		if name == "" {
			continue
		}
		parsed, err := parseHours(hours)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out[name] = parsed
	}
	return out, nil
}

func parsePairs(m map[string]map[string]map[string]statusCounts) (map[string]map[string]hourCounts, error) {
	out := make(map[string]map[string]hourCounts, len(m))
	for model, byProvider := range m {
		if model == "" {
			continue
		}
		entry := make(map[string]hourCounts, len(byProvider))
		for provider, hours := range byProvider {
			if provider == "" {
				continue
			}
			parsed, err := parseHours(hours)
			if err != nil {
				return nil, fmt.Errorf("%s via %s: %w", model, provider, err)
			}
			entry[provider] = parsed
		}
		out[model] = entry
	}
	return out, nil
}
