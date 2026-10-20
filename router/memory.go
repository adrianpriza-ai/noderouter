package router

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
)

// In-flight request-body accounting.
//
// The router buffers each request body in RAM so it can retry the same
// request across proxies. The copies are short-lived, but several coexist
// per request (the raw read, the model-patched copy, the per-attempt
// request), so the worst case is roughly bodySize * 3 * concurrentBigRequests.
// For chat-sized payloads that is nothing; for bodies near server.maxBodyMb
// it can spike RSS past what a small box wants to give.
//
// The budget tracks the largest buffered body per in-flight request and
// refuses (503) new big requests once server.bodyBudgetMB worth of them is
// already being processed. Small requests stay allowed: rejecting a 2KB chat
// to protect a 60MB upload is the wrong trade. This is the hard backstop;
// the soft layer is the Go GC limit set from server.memoryLimitMb.

type BodyBudget struct {
	LimitBytes int64 // <=0 disables the backstop
	inFlight   atomic.Int64
}

func NewBodyBudget(limitMB int) *BodyBudget {
	if limitMB <= 0 {
		return &BodyBudget{}
	}
	return &BodyBudget{LimitBytes: int64(limitMB) << 20}
}

// acquire reserves room for a body of size bytes. It reports whether the
// request may proceed; when denied the caller answers 503 and must not
// release (nothing was reserved).
func (b *BodyBudget) Acquire(size int64) bool {
	if b == nil || b.LimitBytes <= 0 || size <= 0 {
		return true
	}
	for {
		cur := b.inFlight.Load()
		if cur+size > b.LimitBytes {
			return false
		}
		if b.inFlight.CompareAndSwap(cur, cur+size) {
			return true
		}
	}
}

func (b *BodyBudget) Release(size int64) {
	if b == nil || b.LimitBytes <= 0 || size <= 0 {
		return
	}
	b.inFlight.Add(-size)
}

// Reservation is one in-flight request's claim on the budget, and the only
// way handleForward takes one.
//
// It exists because a claim has to start before the bytes are read and grow
// while they arrive: a chunked upload declares no length, so a claim taken
// only after the body was buffered accounted for bytes that were already in
// RAM, which is exactly the accounting that let N concurrent chunked uploads
// each buffer the whole ceiling with the budget showing zero.
//
// Locking: held is guarded by mu, nothing else. Every reader and every
// writer - reserve, grow, Held, release - takes the same lock, so the claim
// says one thing whether it is asked about mid-read from a status log, from
// another goroutine, or from a test. The transfer against the budget inside
// the lock is a lock-free CAS on BodyBudget, so holding mu here never waits
// on anything else. Callers are still normally the owning request's
// goroutine, but that is an implementation detail rather than the invariant.
type Reservation struct {
	budget *BodyBudget
	held   int64 // bytes currently claimed, guarded by mu
	mu     sync.Mutex
}

// NewReservation starts a claim against the budget. Nothing is held until
// reserve or grow is called.
func NewReservation(b *BodyBudget) *Reservation { return &Reservation{budget: b} }

// reserve takes a fresh claim of n bytes, on top of anything already held.
func (r *Reservation) reserve(n int64) bool {
	if r == nil || n <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reserveLocked(n)
}

// reserveLocked is reserve with the lock already held (by reserve, or by
// grow mid-update).
func (r *Reservation) reserveLocked(n int64) bool {
	if !r.budget.Acquire(n) {
		return false
	}
	r.held += n
	return true
}

// grow moves the claim to exactly n bytes, which is how the read keeps the
// budget honest: a chunked body has no declared length, so the claim tracks
// what has actually arrived. Growing is refused when the budget is full; the
// caller answers 503 and stops reading. Shrinking is always allowed and hands
// the difference straight back.
func (r *Reservation) grow(n int64) bool {
	if r == nil {
		return true
	}
	if n < 0 {
		n = 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	have := r.held
	switch {
	case n == have:
		return true
	case n < have:
		// A client that declared more than it sent: the difference was never
		// buffered, so give it back immediately rather than at the end.
		r.budget.Release(have - n)
		r.held = n
		return true
	default:
		return r.reserveLocked(n - have)
	}
}

// Held reports how many bytes the claim currently covers. It is safe to call
// while the claim is still being charged.
func (r *Reservation) Held() int64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.held
}

// release hands the whole claim back and is safe to call more than once, so
// the deferred call in handleForward cannot double-release a claim that an
// early exit already returned.
func (r *Reservation) release() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if held := r.held; held > 0 {
		r.held = 0
		r.budget.Release(held)
	}
}

func (b *BodyBudget) Status() (limitMB, inFlightMB int64, enabled bool) {
	if b == nil || b.LimitBytes <= 0 {
		return 0, 0, false
	}
	return b.LimitBytes >> 20, b.inFlight.Load() >> 20, true
}

var errBodyBudgetExceeded = func(limit int64) error {
	return fmt.Errorf("server busy: in-flight request bodies at the %dMB body budget; retry shortly", limit)
}

// handleBudgetExceeded writes the 503 the backstop answers with.
func (s *Server) handleBudgetExceeded(w http.ResponseWriter, budget *BodyBudget) {
	limit, inFlight, _ := budget.Status()
	s.errors.Add(1)
	writeJSONError(w, http.StatusServiceUnavailable,
		fmt.Sprintf("%s (in flight: %dMB)", errBodyBudgetExceeded(limit).Error(), inFlight))
}
