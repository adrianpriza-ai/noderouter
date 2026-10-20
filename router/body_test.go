package router

// The body budget is the hard backstop on buffered request bytes (memory.go).
// The contract that is easy to break, and that a chunked request breaks first,
// is *when* a body is charged: charging it only after the whole body has been
// buffered accounts bytes that are already in RAM, so the backstop refuses
// nothing while the memory it exists to bound is being spent. A chunked upload
// declares no Content-Length, so it is the case with nothing to charge up
// front - every one of these tests drives it with Transfer-Encoding: chunked
// on purpose.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"noderouter/config"
	"noderouter/logx"
	"noderouter/proxy"
)

// ----------------------------------------------------------- Reservation ---

// A claim starts empty and grows to exactly the bytes handed to it. It must
// never drift: a claim that reports less than it holds under-counts the
// budget, and one that reports more strands capacity after the request ends.
func TestReservationGrowsToExactSize(t *testing.T) {
	b := NewBodyBudget(10)
	claim := NewReservation(b)
	if claim.Held() != 0 {
		t.Fatalf("a fresh claim holds %d, want 0", claim.Held())
	}

	if !claim.reserve(1 << 20) {
		t.Fatal("1MB refused on an empty 10MB budget")
	}
	if _, inFlight, _ := b.Status(); inFlight != 1 {
		t.Errorf("in flight = %dMB after reserving 1MB, want 1", inFlight)
	}

	// Growing to the same size twice is not double-charging.
	if !claim.grow(1 << 20) {
		t.Fatal("growing a claim to its current size was refused")
	}
	if _, inFlight, _ := b.Status(); inFlight != 1 {
		t.Errorf("in flight = %dMB after a no-op grow, want 1", inFlight)
	}

	if !claim.grow(4 << 20) {
		t.Fatal("growing the claim to 4MB was refused on a 10MB budget")
	}
	if got := claim.Held(); got != 4<<20 {
		t.Errorf("claim holds %d, want 4MB", got)
	}
	if _, inFlight, _ := b.Status(); inFlight != 4 {
		t.Errorf("in flight = %dMB after growing to 4MB, want 4", inFlight)
	}

	// Shrinking hands the difference back at once. This is the lying-client
	// case: a body that declared more than it sent must not sit on the
	// difference for the rest of the request.
	if !claim.grow(2 << 20) {
		t.Fatal("shrinking the claim was refused")
	}
	if _, inFlight, _ := b.Status(); inFlight != 2 {
		t.Errorf("in flight = %dMB after shrinking to 2MB, want 2", inFlight)
	}

	// Negative is meaningless and is clamped rather than becoming a phantom
	// release on the way out.
	if !claim.grow(-5) || claim.Held() != 0 {
		t.Errorf("claim holds %d after grow(-5), want 0", claim.Held())
	}
	if _, inFlight, _ := b.Status(); inFlight != 0 {
		t.Errorf("in flight = %dMB after the claim emptied, want 0", inFlight)
	}
}

// A refused growth must leave the claim exactly as it was. Charging the bytes
// it could not get would make the deferred release hand back more than was
// ever reserved, which drives the in-flight total negative and silently
// disables the backstop for every later request.
func TestReservationRefusedGrowthKeepsItsSize(t *testing.T) {
	b := NewBodyBudget(4)
	claim := NewReservation(b)
	if !claim.reserve(3 << 20) {
		t.Fatal("3MB refused on a 4MB budget")
	}

	if claim.grow(5 << 20) {
		t.Fatal("growing to 5MB succeeded on a 4MB budget")
	}
	if got := claim.Held(); got != 3<<20 {
		t.Errorf("claim holds %d after a refused growth, want the 3MB it had", got)
	}
	if _, inFlight, _ := b.Status(); inFlight != 3 {
		t.Errorf("in flight = %dMB after a refused growth, want 3", inFlight)
	}

	// And the release balances out exactly.
	claim.release()
	if _, inFlight, _ := b.Status(); inFlight != 0 {
		t.Errorf("in flight = %dMB after release, want 0", inFlight)
	}
}

// Releasing is idempotent, because handleForward defers it and also calls it
// explicitly at the streaming exits. A double release would drive inFlight
// negative and hand free capacity that was never taken.
func TestReservationReleaseIsIdempotent(t *testing.T) {
	b := NewBodyBudget(4)
	claim := NewReservation(b)
	if !claim.reserve(3 << 20) {
		t.Fatal("3MB refused on a 4MB budget")
	}
	claim.release()
	claim.release()
	claim.release()
	if _, inFlight, _ := b.Status(); inFlight != 0 {
		t.Errorf("in flight = %dMB after three releases of one claim, want 0", inFlight)
	}

	// The budget must still be the 4MB it was: a negative inFlight would let
	// the next request take more than the ceiling.
	if claim.reserve(4<<20) != true {
		t.Error("the budget no longer admits its full size after a double release")
	}
}

// A nil budget (bodyBudgetMb disabled) must not panic and must admit
// everything, at any size.
func TestReservationOnDisabledBudget(t *testing.T) {
	for name, b := range map[string]*BodyBudget{"nil": nil, "disabled": NewBodyBudget(0), "negative": NewBodyBudget(-1)} {
		claim := NewReservation(b)
		if !claim.reserve(500 << 20) {
			t.Errorf("%s budget refused a 500MB claim", name)
		}
		if !claim.grow(1 << 20) {
			t.Errorf("%s budget refused a shrink", name)
		}
		claim.release()
	}
}

// ------------------------------------------------------------- readBody ---

// A body with a declared Content-Length inside the ceiling is charged for its
// declared size up front (the caller does that) and read in one allocation.
func TestReadBodyDeclaredLength(t *testing.T) {
	s := &Server{}
	body := strings.Repeat("a", 1000)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))

	claim := NewReservation(NewBodyBudget(10))
	if !claim.reserve(int64(len(body))) {
		t.Fatal("claim refused")
	}
	got, err := s.readBody(req, 1, claim)
	if err != nil {
		t.Fatalf("readBody: %v", err)
	}
	if string(got) != body {
		t.Errorf("read %d bytes, want the %d sent", len(got), len(body))
	}
	if h := claim.Held(); h != int64(len(body)) {
		t.Errorf("claim holds %d after reading a %d byte body", h, len(body))
	}
}

// A chunked body is charged as it arrives. The test drives the read by hand
// through a pipe so the accounting can be observed *during* the transfer,
// which is the whole point: before the fix nothing was charged until the read
// had already finished.
func TestReadBodyChargesChunkedBodyAsItArrives(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", pr)
	req.ContentLength = -1 // chunked: no declared length to charge up front

	s := &Server{}
	claim := NewReservation(NewBodyBudget(10))

	done := make(chan struct {
		body []byte
		err  error
	}, 1)
	go func() {
		b, err := s.readBody(req, 8, claim)
		done <- struct {
			body []byte
			err  error
		}{b, err}
	}()

	// First MB: the claim must already be carrying it while the body is still
	// open, which is the assertion that used to fail (in flight was 0).
	writeChunk(t, pw, 'a', 1<<20)
	waitForHeld(t, claim, 1<<20)
	if h := claim.Held(); h != 1<<20 {
		t.Errorf("claim holds %d mid-transfer, want the 1MB already sent", h)
	}

	// Second MB: charged as it arrives, so the total tracks the body.
	writeChunk(t, pw, 'b', 1<<20)
	waitForHeld(t, claim, 2<<20)

	pw.Close()
	res := <-done
	if res.err != nil {
		t.Fatalf("readBody: %v", res.err)
	}
	if len(res.body) != 2<<20 {
		t.Errorf("read %d bytes, want 2MB", len(res.body))
	}
	if res.body[0] != 'a' || res.body[1<<20] != 'b' {
		t.Error("the buffered body is not the bytes that were sent")
	}
	if h := claim.Held(); h != 2<<20 {
		t.Errorf("claim holds %d after the read, want 2MB", h)
	}
}

// The budget running out mid-read is its own failure: the body is abandoned
// where it stands rather than grown to the ceiling, and the error says the
// budget rather than the client's size (which would be a 413 lie).
func TestReadBodyRefusedMidReadIsTheBudgetError(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", pr)
	req.ContentLength = -1

	s := &Server{}
	claim := NewReservation(NewBodyBudget(1)) // 1MB, ceiling far above it

	errc := make(chan error, 1)
	go func() {
		_, err := s.readBody(req, 64, claim)
		errc <- err
	}()

	writeChunk(t, pw, 'a', 900<<10)
	waitForHeld(t, claim, 900<<10)

	// Push past the budget: the read must refuse rather than keep buffering.
	writeChunk(t, pw, 'b', 900<<10)

	select {
	case err := <-errc:
		var full bodyBudgetFullError
		var tooLarge bodyTooLargeError
		if err == nil {
			t.Fatal("readBody succeeded past the budget")
		}
		if !errors.As(err, &full) {
			t.Fatalf("error = %v (%T), want bodyBudgetFullError", err, err)
		}
		if errors.As(err, &tooLarge) {
			t.Error("a full budget was reported as an oversized body")
		}
		// The refused read holds only what it managed to take.
		if h := claim.Held(); h > 1<<20 {
			t.Errorf("claim holds %d after a refused read, want at most the 1MB budget", h)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readBody kept reading past the budget")
	}
	pw.Close()
}

// A chunked body that crosses the *ceiling* is still a 413, not a budget
// failure: the two are different problems and are answered differently.
func TestReadBodyChunkedOverCeilingIsTooLarge(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", pr)
	req.ContentLength = -1

	s := &Server{}
	claim := NewReservation(NewBodyBudget(0)) // budget off, so only the ceiling can fire

	go func() {
		pw.Write(make([]byte, 1<<20))
		pw.Write(make([]byte, 64<<10)) // crosses the 1MB ceiling
		pw.Close()
	}()

	_, err := s.readBody(req, 1, claim)
	var tooLarge bodyTooLargeError
	if err == nil || !errors.As(err, &tooLarge) {
		t.Fatalf("error = %v (%T), want bodyTooLargeError", err, err)
	}
	if !strings.Contains(err.Error(), "1MB") {
		t.Errorf("error = %q, want it to name the ceiling", err)
	}
}

// A chunked body that is exactly the ceiling still gets through. Reading up to
// the limit and stopping at limit+1 is what keeps an honest client at the
// boundary from being told it sent too much.
func TestReadBodyChunkedExactlyAtCeilingIsAccepted(t *testing.T) {
	payload := make([]byte, 1<<20)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", io.NopCloser(strings.NewReader(string(payload))))
	req.ContentLength = -1

	s := &Server{}
	claim := NewReservation(NewBodyBudget(0))
	got, err := s.readBody(req, 1, claim)
	if err != nil {
		t.Fatalf("a body of exactly the ceiling was refused: %v", err)
	}
	if len(got) != len(payload) {
		t.Errorf("read %d bytes, want %d", len(got), len(payload))
	}
	if h := claim.Held(); h != int64(len(payload)) {
		t.Errorf("claim holds %d after reading a %d byte body", h, len(payload))
	}
}

// A nil body is not an error and charges nothing (a GET has no body).
func TestReadBodyNilBody(t *testing.T) {
	s := &Server{}
	claim := NewReservation(NewBodyBudget(10))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Body = nil
	got, err := s.readBody(req, 1, claim)
	if err != nil {
		t.Fatalf("readBody: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("read %d bytes from a nil body", len(got))
	}
	if h := claim.Held(); h != 0 {
		t.Errorf("claim holds %d for a body-less request", h)
	}
}

// A truncated upload is a read failure, not a size problem: the claim is
// handed back and the error is not bodyTooLargeError.
func TestReadBodyTruncatedUploadIsNotTooLarge(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		io.NopCloser(io.LimitReader(neverEnding{}, 100)))
	req.ContentLength = 1 << 20 // declared far more than arrives

	s := &Server{}
	claim := NewReservation(NewBodyBudget(10))
	if !claim.reserve(1 << 20) {
		t.Fatal("claim refused")
	}
	_, err := s.readBody(req, 4, claim)
	if err == nil {
		t.Fatal("a truncated upload read cleanly")
	}
	var tooLarge bodyTooLargeError
	if errors.As(err, &tooLarge) {
		t.Errorf("a truncated upload was reported as oversized: %v", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Logf("truncated upload error = %v (not ErrUnexpectedEOF, still a read failure)", err)
	}
}

// neverEnding supplies bytes forever; io.LimitReader cuts it off.
type neverEnding struct{}

func (neverEnding) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// ------------------------------------------------------------ end to end ---

// budgetTestServer wires a router to a fake upstream and returns it with the
// body budget sized for the test.
func budgetTestServer(t *testing.T, upstreamURL string, budgetMB, maxBodyMB int) *Server {
	t.Helper()
	cfg := &config.Config{}
	cfg.ApplyDefaults()
	cfg.Server.BodyBudgetMB = budgetMB
	cfg.Server.MaxBodyMB = maxBodyMB
	// The proxy pool is not what these tests are about, and an empty pool
	// would answer every request with "going direct" anyway.
	cfg.Proxy.Enabled = config.BoolPtr(false)
	cfg.Provider = map[string]*config.Provider{
		"P": {Options: config.ProviderOptions{BaseURL: upstreamURL}, Models: map[string]*config.Model{}},
	}
	srv := NewServer(cfg, proxy.NewPool(filepath.Join(t.TempDir(), "none.txt"), "", "rotate", false), logx.New(false))
	return srv
}

// The regression this whole change exists for: while a chunked upload is still
// arriving, the budget must already be carrying its bytes. Before the fix the
// reservation was taken after readBody returned, so the in-flight total sat at
// zero for the entire transfer and a second big upload was admitted on top of
// it.
func TestChunkedUploadIsBudgetedDuringTheTransfer(t *testing.T) {
	const chunk = 2 << 20 // 2MB per write, 4 writes = 8MB total

	gate := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-gate
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	// 6MB of budget, 8MB in flight: the second half must be refused.
	srv := budgetTestServer(t, upstream.URL, 6, 16)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	pr, pw := io.Pipe()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1 // force chunked
	respCh := make(chan int, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			respCh <- 0
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		respCh <- resp.StatusCode
	}()

	// Push 4MB through while the transfer is still open. The budget is 6MB, so
	// by the time 4MB has landed a second 4MB upload has to be refused.
	total := 0
	for i := 0; i < 2; i++ {
		if _, err := pw.Write(make([]byte, chunk)); err != nil {
			t.Fatalf("writing the body: %v", err)
		}
		total += chunk
		waitForInFlight(t, srv, int64(total)>>20)
		if _, held, _ := srv.BodiesStatus(); held == 0 {
			t.Fatalf("in flight = 0MB with %dMB of a chunked body already sent and "+
				"the request still open: nothing is being charged during the read", total>>20)
		}
	}

	// A big second upload has to be turned away while the first is still
	// arriving: this is the admission the old accounting allowed.
	code := postSize(t, ts.URL, 4<<20)
	if code != http.StatusServiceUnavailable {
		t.Errorf("second 4MB upload while 4MB is chunked-in-flight: status %d, want 503", code)
	}

	// Close the body first, so the upstream's read finishes, then let the
	// gated upstream answer. Waiting for the response before closing the gate
	// would deadlock: the handler cannot get to the gate until the body ends.
	pw.Close()
	close(gate)
	if code := <-respCh; code != http.StatusOK {
		t.Errorf("the chunked upload: status %d, want 200", code)
	}

	// And the claim is fully handed back, so the next upload sails through.
	waitForInFlight(t, srv, 0)
	if code := postSize(t, ts.URL, 4<<20); code != http.StatusOK {
		t.Errorf("upload after the chunked one finished: status %d, want 200", code)
	}
}

// A chunked upload that outgrows the budget is refused with 503 and, crucially,
// the refusal is not 413: the client did not send too much, the router was
// busy. Before the fix a chunked upload was buffered to the ceiling with
// nothing charged and then admitted outright.
func TestChunkedUploadOverBudgetIs503Not413(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	// A 1MB budget under a 64MB ceiling: the ceiling can never be the reason.
	srv := budgetTestServer(t, upstream.URL, 1, 64)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	code := postChunked(t, ts.URL, 4<<20)
	if code != http.StatusServiceUnavailable {
		t.Errorf("4MB chunked upload against a 1MB budget: status %d, want 503", code)
	}
	if _, inFlight, _ := srv.BodiesStatus(); inFlight != 0 {
		t.Errorf("in flight = %dMB after a refused chunked upload, want 0", inFlight)
	}
}

// A chunked upload that fits is admitted and fully released, so the ceiling
// and the budget are not confused for one another.
func TestChunkedUploadWithinBudgetSucceeds(t *testing.T) {
	var seen int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		seen = n
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	srv := budgetTestServer(t, upstream.URL, 8, 16)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	if code := postChunked(t, ts.URL, 3<<20); code != http.StatusOK {
		t.Fatalf("3MB chunked upload against an 8MB budget: status %d, want 200", code)
	}
	if seen != 3<<20 {
		t.Errorf("upstream saw %d bytes, want the 3MB sent", seen)
	}
	if _, inFlight, _ := srv.BodiesStatus(); inFlight != 0 {
		t.Errorf("in flight = %dMB after a completed chunked upload, want 0", inFlight)
	}
}

// A declared body is still charged before it is read, so a client that
// announces a big body and then stalls is refused rather than served. This is
// the pre-existing up-front behaviour; it must survive the rework.
func TestDeclaredBodyIsChargedBeforeTheRead(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	srv := budgetTestServer(t, upstream.URL, 4, 16)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	pr, pw := io.Pipe()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = 3 << 20 // declared, but nothing is sent yet
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	// The claim is taken on the declaration alone - no bytes have moved.
	waitForInFlight(t, srv, 3)
	if code := postSize(t, ts.URL, 3<<20); code != http.StatusServiceUnavailable {
		t.Errorf("second 3MB upload while a declared 3MB body is pending: status %d, want 503", code)
	}
	pw.Close()
}

// A small body is still admitted when the budget is all but full: refusing a
// 2KB chat to protect a 60MB upload is the wrong trade, and the budget is a
// backstop on the big bodies, not a gate on every request. What is refused is
// the big body that would not fit in what is left.
func TestSmallBodiesPassANearlyFullBudget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	srv := budgetTestServer(t, upstream.URL, 6, 16)
	// Hold the budget directly: a refused request holds nothing, so a
	// reservation is the only way to keep it occupied without an upstream
	// round trip. This mirrors a 5MB upload in flight against a 6MB budget.
	saturate := NewReservation(srv.bodies)
	if !saturate.reserve(5 << 20) {
		t.Fatal("could not occupy the test budget")
	}
	defer saturate.release()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	if code := postSize(t, ts.URL, 256); code != http.StatusOK {
		t.Errorf("small body against a nearly full budget: status %d, want 200", code)
	}
	if code := postSize(t, ts.URL, 5<<20); code != http.StatusServiceUnavailable {
		t.Errorf("big body against a nearly full budget: status %d, want 503", code)
	}
	// The small request released its own claim: the big one above was refused
	// for want of room, not because a chat had parked on the budget.
	if _, inFlight, _ := srv.BodiesStatus(); inFlight != 5 {
		t.Errorf("in flight = %dMB, want the 5MB the test is holding", inFlight)
	}
}

// A body that exactly fills what is left is admitted: the budget is a
// ceiling, not a queue, and refusing a body that fits would make the ceiling
// unreachable. Measured against the real payload length, not an approximation.
func TestBodyExactlyFillingTheBudgetIsAdmitted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	payload := chatPayload(1 << 20)
	total := int64(len(payload))

	// The budget is the payload and nothing more. Sized directly rather than
	// through MB-granularity config so "exactly fits" really means exactly.
	srv := budgetTestServer(t, upstream.URL, 8, 16)
	srv.bodies = NewBodyBudget(8)
	srv.bodies.LimitBytes = total

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	code := postPayload(t, ts.URL, payload)
	if code != http.StatusOK {
		t.Errorf("a body of exactly the budget: status %d, want 200", code)
	}
	if _, inFlight, _ := srv.BodiesStatus(); inFlight != 0 {
		t.Errorf("in flight = %d after the request completed, want 0", inFlight)
	}
	// One byte short of the budget and the same body is turned away, so the
	// first result is not the budget quietly having room to spare.
	blocked := NewReservation(srv.bodies)
	if !blocked.reserve(total - 1) {
		t.Fatal("could not occupy the budget")
	}
	if code := postPayload(t, ts.URL, payload); code != http.StatusServiceUnavailable {
		t.Errorf("a body one byte over the budget: status %d, want 503", code)
	}
	blocked.release()
}

// ------------------------------------------------------------- helpers ---

// writeChunk writes n bytes of one repeated character to the pipe from a
// goroutine, so a read that stops consuming (because the budget refused it)
// cannot deadlock the test.
func writeChunk(t *testing.T, pw *io.PipeWriter, c byte, n int) {
	t.Helper()
	go func() {
		_, err := pw.Write(bytesRepeat(c, n))
		// A closed pipe is the expected outcome once the router has answered.
		if err != nil && !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("writing the body: %v", err)
		}
	}()
}

// waitForHeld waits for the claim to carry at least want bytes, which it must
// do while the transfer is still open.
func waitForHeld(t *testing.T, claim *Reservation, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if claim.Held() >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the claim never reached %d bytes (held %d): nothing is charged during the read",
				want, claim.Held())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForInFlight waits for the server's in-flight budget to report at least
// want MB.
func waitForInFlight(t *testing.T, srv *Server, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, inFlight, _ := srv.BodiesStatus()
		if inFlight >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("in-flight bodies never reached %dMB (currently %dMB)", want, inFlight)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// chatPayload builds a request body whose content field is n bytes of filler,
// returned whole so a test can measure exactly what it is about to send.
func chatPayload(n int) []byte {
	return []byte(fmt.Sprintf(`{"model":"x","messages":[{"role":"user","content":%q}]}`,
		strings.Repeat("a", n)))
}

// postSize sends a declared body of about n bytes.
func postSize(t *testing.T, base string, n int) int {
	t.Helper()
	return postPayload(t, base, chatPayload(n))
}

// postPayload sends exactly this body, with its length declared.
func postPayload(t *testing.T, base string, payload []byte) int {
	t.Helper()
	resp, err := http.Post(base+"/v1/chat/completions", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("posting %d bytes: %v", len(payload), err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// postChunked sends n bytes with no declared length, in 64KB pieces so the
// read takes several turns.
func postChunked(t *testing.T, base string, n int) int {
	t.Helper()
	pr, pw := io.Pipe()
	req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1
	done := make(chan int, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- 0
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	// Keep writing until the router refuses; a write error means it answered.
	written := 0
	for written < n {
		if _, err := pw.Write(make([]byte, 64<<10)); err != nil {
			break
		}
		written += 64 << 10
	}
	pw.Close()
	return <-done
}

func bytesRepeat(c byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return b
}
