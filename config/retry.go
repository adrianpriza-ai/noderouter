package config

import (
	"context"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The retry policy. A request that has not been answered yet is free to try
// again: nothing has been written to the client, so a second attempt costs
// latency and nothing else. Once a response exists, whether it is a success or
// the provider's own error, it goes straight back - a retry there would mean
// discarding output the client may already be reading.

// RetryableStatus reports whether a provider answer is worth another try.
// Transport failures always are and never reach here; this is about statuses.
//
// The default set is the ones that mean "not now". A 400 (malformed), 401
// (rejected key), 403, 404 (no such model) or 422 (unprocessable) means "not
// ever": the same answer comes back nine more times, only slower, and every
// one of them spends the caller's timeout budget.
func (c *RetryConfig) RetryableStatus(code int) bool {
	if containsInt(c.SkipStatuses, code) {
		return false
	}
	if containsInt(c.Statuses, code) {
		return true
	}
	return DefaultRetryStatuses[code]
}

// containsInt reports whether code is in list. The lists are one or two entries
// in practice, so a linear scan beats building a set per request.
func containsInt(list []int, code int) bool {
	for _, v := range list {
		if v == code {
			return true
		}
	}
	return false
}

// Backoff is how long to wait before attempt n+1, given that attempt n came
// back with a retryable answer. attempt is 0-based, so the first retry is the
// smallest wait.
//
// A provider's Retry-After wins when it sends one - that is the whole point of
// the header, and it is how a rate limit is meant to be respected - but it is
// clamped to maxBackoff so a provider (or a misconfigured gateway) asking for
// an hour cannot pin a request open that long.
func (c *RetryConfig) Backoff(attempt int, header http.Header) time.Duration {
	maxWait := time.Duration(c.MaxBackoffMS) * time.Millisecond
	if maxWait <= 0 {
		return 0
	}
	if wait, ok := ParseRetryAfter(header.Get("Retry-After"), time.Now()); ok {
		if wait > maxWait {
			wait = maxWait
		}
		return wait
	}
	base := time.Duration(c.BackoffMS) * time.Millisecond
	if base <= 0 {
		return 0
	}
	// Double per attempt, capped. The shift is done in int64 space so a big
	// attempt count cannot wrap a time.Duration negative.
	wait := maxWait
	if attempt < 62 {
		if grown := base << uint(attempt); grown > 0 && grown < wait {
			wait = grown
		}
	}
	// Half-jitter: keep half the wait so a rate limit is actually respected,
	// and randomise the rest so a burst of clients does not come back in
	// lockstep and trip the limit all over again.
	half := wait / 2
	return half + time.Duration(rand.Int63n(int64(wait-half)+1))
}

// ParseRetryAfter reads the header in either form RFC 9110 allows: a number of
// seconds, or an HTTP date. A date in the past means "now", and nonsense
// means "no opinion".
func ParseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(value); err == nil {
		if secs <= 0 {
			return 0, true
		}
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(value); err == nil {
		wait := when.Sub(now)
		if wait < 0 {
			wait = 0
		}
		return wait, true
	}
	return 0, false
}

// SleepCtx waits for d, but gives up the moment the client does. A caller that
// hung up must not leave the router holding a request open, and a cancelled
// context is the only signal that arrives when the other end goes away.
// Reports whether the full wait happened.
func SleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// retryDeadline caps the whole retry sequence when retry.totalSecs is set.
// Without it a sequence of hanging attempts is bounded only by each attempt's
// own timeout, which is a long time multiplied by ten.
//
// It is derived from the request's context rather than replacing it, so the
// client's own Deadline still wins and cancellation still propagates.
func (c *RetryConfig) Deadline(parent context.Context) (context.Context, context.CancelFunc) {
	if c.TotalSecs <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, time.Duration(c.TotalSecs)*time.Second)
}

// TimeLeft reports whether the retry sequence still has enough budget to be
// worth another try. A try started with no time left does not fail fast, it
// hangs until the per-attempt timeout, so it would burn whatever was left.
func (c *RetryConfig) TimeLeft(ctx context.Context) bool {
	if c.TotalSecs <= 0 {
		return true
	}
	Deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return time.Until(Deadline) > 0
}
