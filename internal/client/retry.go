package client

import (
	"context"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// Retry policy for HTTP 429.
//
// The API rate-limits per key — LP_API_RATE_MAX requests per LP_API_RATE_WINDOW_S,
// 60 per minute by default, over a fixed window (api/ratelimit.go). Terraform
// walks the graph with a default parallelism of 10, so any configuration of
// more than a few dozen resources will cross that limit partway through an
// apply. Treating the 429 as terminal loses the worst way possible: the
// resources created before it exist on the server but never reach state, and
// the next apply tries to create them again.
//
// Only 429 is retried. It is the one status the API returns *before* the
// handler runs, so the request provably had no effect and replaying it is free.
// A 5xx is deliberately NOT retried: POST /api/v1/channels and POST
// /api/v1/checks are not idempotent, and a 502 from an intermediary carries no
// information about whether the write landed. Replaying one would silently
// create a duplicate resource that Terraform does not know about and will never
// clean up — strictly worse than a failed apply the operator can re-run.
const (
	// defaultRetryMaxAttempts counts the first try, so this is three retries.
	defaultRetryMaxAttempts = 4

	// defaultRetryMaxWait bounds the total time spent sleeping across all
	// retries of a single request, so a rate-limited plan fails in bounded time
	// instead of hanging. One full 60s window plus slack.
	defaultRetryMaxWait = 90 * time.Second

	// retryPerAttemptCap clamps a server-supplied Retry-After. The API's own
	// value never exceeds one window, but a proxy in between can say anything.
	retryPerAttemptCap = 65 * time.Second

	// retryBaseDelay and retryBackoffCap bound the fallback backoff used when no
	// usable Retry-After is present.
	retryBaseDelay   = 500 * time.Millisecond
	retryBackoffCap  = 8 * time.Second
	retryJitterFloor = 2 // backoff/retryJitterFloor is the low end of the jitter window
)

func (c *Client) retryMaxAttempts() int {
	if c.RetryMaxAttempts > 0 {
		return c.RetryMaxAttempts
	}
	return defaultRetryMaxAttempts
}

func (c *Client) retryMaxWait() time.Duration {
	if c.RetryMaxWait > 0 {
		return c.RetryMaxWait
	}
	return defaultRetryMaxWait
}

// retryDelay returns how long to wait before retrying a 429.
//
// A parseable Retry-After wins, because the API knows exactly when its fixed
// window rolls over and guessing shorter just burns another request. Otherwise
// the delay is exponential in the attempt number with jitter: without jitter,
// ten parallel Terraform workers rate-limited by the same window would retry in
// lockstep and rate-limit each other all over again.
func retryDelay(retryAfter string, attempt int, now time.Time) time.Duration {
	if d, ok := parseRetryAfter(retryAfter, now); ok {
		return min(max(d, 0), retryPerAttemptCap)
	}

	backoff := retryBaseDelay
	for i := 1; i < attempt && backoff < retryBackoffCap; i++ {
		backoff *= 2
	}
	backoff = min(backoff, retryBackoffCap)

	// Uniform in [backoff/2, backoff].
	low := backoff / retryJitterFloor
	return low + rand.N(backoff-low+1) //nolint:gosec // jitter, not a security decision
}

// parseRetryAfter reads an RFC 9110 Retry-After value, which is either a count
// of seconds or an HTTP-date. It reports false when the header is absent or
// unparseable, which is the caller's signal to fall back to backoff.
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return t.Sub(now), true
	}
	return 0, false
}

// sleepCtx waits for d, returning early with the context's error if it is
// cancelled first. A user pressing Ctrl-C must not have to wait out a backoff.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
