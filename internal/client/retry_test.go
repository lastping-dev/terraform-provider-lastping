package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeSleep replaces the backoff wait with a recorder, so the retry loop can be
// exercised in microseconds instead of minutes. It still honours ctx, because
// aborting on cancellation is one of the behaviours under test.
func fakeSleep(t *testing.T, c *Client) *[]time.Duration {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []time.Duration
	)
	c.sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		seen = append(seen, d)
		mu.Unlock()
		return ctx.Err()
	}
	return &seen
}

// statusServer replies with the given statuses in order, repeating the last one
// once the list is exhausted, and counts the requests it received.
func statusServer(t *testing.T, retryAfter string, statuses ...int) (*httptest.Server, *int) {
	t.Helper()
	var (
		mu       sync.Mutex
		attempts int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		i := attempts
		attempts++
		mu.Unlock()

		if i >= len(statuses) {
			i = len(statuses) - 1
		}
		code := statuses[i]
		if code == http.StatusTooManyRequests && retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		if code >= 200 && code <= 299 {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"id":"abc"}`))
			return
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"title":"Too Many Requests","status":429,
			"detail":"rate limit exceeded","code":"RATE_LIMITED","fix":"slow down"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &attempts
}

// TestDo_RetriesOn429 is the whole point of the retry loop. The API rate-limits
// per key — LP_API_RATE_MAX, 60/min by default, over a fixed window — and
// Terraform walks the graph with a parallelism of 10, so any realistic
// configuration hits 429 partway through an apply. Treating it as terminal
// leaves resources created on the server but absent from state.
func TestDo_RetriesOn429(t *testing.T) {
	srv, attempts := statusServer(t, "1", http.StatusTooManyRequests, http.StatusOK)

	var out struct {
		ID string `json:"id"`
	}
	c := New(srv.URL, "lp_x", "test")
	slept := fakeSleep(t, c)

	require.NoError(t, c.Do(context.Background(), http.MethodGet, "/api/v1/checks/abc", nil, &out))
	require.Equal(t, 2, *attempts, "the 429 must be retried")
	require.Equal(t, "abc", out.ID)
	require.Equal(t, []time.Duration{time.Second}, *slept)
}

// TestDo_HonoursRetryAfter — the API knows exactly when its fixed window rolls
// over and says so. Guessing a shorter delay just burns another request against
// the same window.
func TestDo_HonoursRetryAfter(t *testing.T) {
	srv, _ := statusServer(t, "17", http.StatusTooManyRequests, http.StatusOK)

	c := New(srv.URL, "lp_x", "test")
	slept := fakeSleep(t, c)

	require.NoError(t, c.Do(context.Background(), http.MethodGet, "/api/v1/checks", nil, nil))
	require.Equal(t, []time.Duration{17 * time.Second}, *slept)
}

// TestDo_RetryAfterHTTPDateIsHonoured — RFC 9110 allows an HTTP-date as well as
// a delay in seconds, and a proxy in front of the API may rewrite it as one.
func TestDo_RetryAfterHTTPDateIsHonoured(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	d := retryDelay(now.Add(30*time.Second).Format(http.TimeFormat), 1, now)
	require.Equal(t, 30*time.Second, d)
}

// TestDo_RetryAfterIsClamped — the API's own Retry-After never exceeds one
// window, but an intermediary can claim anything, and an hour-long sleep inside
// a plan is indistinguishable from a hang.
func TestDo_RetryAfterIsClamped(t *testing.T) {
	require.Equal(t, retryPerAttemptCap, retryDelay("3600", 1, time.Now()))
	require.Equal(t, time.Duration(0), retryDelay("-5", 1, time.Now()),
		"a Retry-After in the past means retry now, not travel back in time")
}

// TestRetryDelayBacksOffWithJitter — with no usable Retry-After the delay grows
// with the attempt number and carries jitter, so ten parallel Terraform workers
// rate-limited by the same window do not retry in lockstep and rate-limit each
// other all over again.
func TestRetryDelayBacksOffWithJitter(t *testing.T) {
	for attempt := 1; attempt <= 6; attempt++ {
		base := min(retryBaseDelay<<(attempt-1), retryBackoffCap)
		seen := map[time.Duration]bool{}
		for range 200 {
			d := retryDelay("", attempt, time.Now())
			require.GreaterOrEqual(t, d, base/2, "attempt %d below the jitter floor", attempt)
			require.LessOrEqual(t, d, base, "attempt %d above the backoff ceiling", attempt)
			seen[d] = true
		}
		require.Greater(t, len(seen), 1, "attempt %d produced no jitter", attempt)
	}
	require.Greater(t, retryDelay("", 3, time.Now()), retryDelay("", 1, time.Now())/2,
		"backoff must grow with the attempt number")
}

// TestDo_RetryAttemptsAreCapped — a plan must fail in bounded time, and the
// failure has to stay a *Problem so IsNotFound and the diagnostic text keep
// working.
func TestDo_RetryAttemptsAreCapped(t *testing.T) {
	srv, attempts := statusServer(t, "1", http.StatusTooManyRequests)

	c := New(srv.URL, "lp_x", "test")
	slept := fakeSleep(t, c)

	err := c.Do(context.Background(), http.MethodGet, "/api/v1/checks", nil, nil)
	require.Error(t, err)
	require.Equal(t, defaultRetryMaxAttempts, *attempts)
	require.Len(t, *slept, defaultRetryMaxAttempts-1, "no sleep after the final attempt")

	var p *Problem
	require.True(t, errors.As(err, &p), "the give-up error must still be a *Problem")
	require.Equal(t, http.StatusTooManyRequests, p.Status)
	require.Contains(t, err.Error(), "rate limit exceeded")
	require.Contains(t, err.Error(), "RATE_LIMITED")
	require.Contains(t, err.Error(), "slow down")
}

// TestDo_RetryTotalWaitIsCapped — the attempt cap alone is not enough: a
// Retry-After of a full window on every attempt could otherwise stall a plan
// for minutes. The total-wait ceiling bounds it independently.
func TestDo_RetryTotalWaitIsCapped(t *testing.T) {
	srv, attempts := statusServer(t, "60", http.StatusTooManyRequests)

	c := New(srv.URL, "lp_x", "test")
	c.RetryMaxWait = 90 * time.Second
	slept := fakeSleep(t, c)

	err := c.Do(context.Background(), http.MethodGet, "/api/v1/checks", nil, nil)
	require.Error(t, err)
	// 60s fits, a second 60s would exceed 90s, so it gives up before sleeping again.
	require.Equal(t, []time.Duration{60 * time.Second}, *slept)
	require.Equal(t, 2, *attempts)

	var total time.Duration
	for _, d := range *slept {
		total += d
	}
	require.LessOrEqual(t, total, c.RetryMaxWait)
}

// TestDo_CancelledContextAbortsBackoff — Ctrl-C during an apply must not wait
// out a 60-second backoff.
func TestDo_CancelledContextAbortsBackoff(t *testing.T) {
	srv, attempts := statusServer(t, "60", http.StatusTooManyRequests)

	c := New(srv.URL, "lp_x", "test")
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel as the loop enters its first backoff, then let the real sleep run:
	// it must return immediately rather than after 60 seconds.
	c.sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return sleepCtx(ctx, d)
	}

	start := time.Now()
	err := c.Do(ctx, http.MethodGet, "/api/v1/checks", nil, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(start), 5*time.Second, "backoff must abort on cancellation")
	require.Equal(t, 1, *attempts, "no request may be issued after cancellation")
}

// TestDo_DoesNotRetryOther4xx — a 400 is the operator's configuration being
// wrong; replaying it just delays the diagnostic. A 404 must reach Read
// unretried so a deleted resource leaves state promptly.
func TestDo_DoesNotRetryOther4xx(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized,
		http.StatusForbidden, http.StatusNotFound, http.StatusPreconditionFailed} {
		srv, attempts := statusServer(t, "", code)
		c := New(srv.URL, "lp_x", "test")
		slept := fakeSleep(t, c)

		require.Error(t, c.Do(context.Background(), http.MethodGet, "/api/v1/checks", nil, nil))
		require.Equal(t, 1, *attempts, "HTTP %d must not be retried", code)
		require.Empty(t, *slept)
	}
}

// TestDo_DoesNotRetry5xx is a deliberate choice, not an oversight. POST
// /api/v1/channels and POST /api/v1/checks are not idempotent, and a 5xx from
// an intermediary says nothing about whether the write landed. Replaying one
// would create a duplicate resource Terraform neither records nor cleans up —
// worse than a failed apply the operator can simply re-run.
func TestDo_DoesNotRetry5xx(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		srv, attempts := statusServer(t, "", code, http.StatusOK)
		c := New(srv.URL, "lp_x", "test")
		slept := fakeSleep(t, c)

		require.Error(t, c.Do(context.Background(), http.MethodPost, "/api/v1/channels",
			map[string]string{"name": "x"}, nil))
		require.Equal(t, 1, *attempts, "HTTP %d must not be retried", code)
		require.Empty(t, *slept)
	}
}

// TestDo_RetryReplaysRequestBody — the encoded body is read by the first
// attempt, so a retry that reused the same reader would POST an empty document
// and get a 400 that looks like a validation bug.
func TestDo_RetryReplaysRequestBody(t *testing.T) {
	var (
		mu       sync.Mutex
		bodies   []string
		attempts int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		attempts++
		bodies = append(bodies, string(raw))
		n := attempts
		mu.Unlock()

		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"abc"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "lp_x", "test")
	fakeSleep(t, c)

	require.NoError(t, c.Do(context.Background(), http.MethodPost, "/api/v1/channels",
		map[string]string{"name": "acc"}, nil))
	require.Equal(t, []string{`{"name":"acc"}`, `{"name":"acc"}`}, bodies)
}

// TestDo_RetryKeepsRequestOptions — headers added by a ReqOpt must survive onto
// the replayed request; a lost If-None-Match would turn a conditional write
// into an unconditional one.
func TestDo_RetryKeepsRequestOptions(t *testing.T) {
	var (
		mu       sync.Mutex
		headers  []string
		attempts int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		headers = append(headers, r.Header.Get("If-None-Match"))
		n := attempts
		mu.Unlock()

		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, "lp_x", "test")
	fakeSleep(t, c)

	require.NoError(t, c.Do(context.Background(), http.MethodPost, "/api/v1/checks", nil, nil,
		WithHeader("If-None-Match", "*")))
	require.Equal(t, []string{"*", "*"}, headers)
}

// TestSleepCtxReturnsPromptlyOnCancel guards the wait primitive itself.
func TestSleepCtxReturnsPromptlyOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	require.ErrorIs(t, sleepCtx(ctx, time.Hour), context.Canceled)
	require.Less(t, time.Since(start), time.Second)

	require.NoError(t, sleepCtx(context.Background(), 0), "a zero wait is not an error")
}
