// Package client provides an authenticated HTTP client for the LastPing
// management API, including mapping of RFC 7807 problem responses (extended
// with agent-oriented code/fix fields) into actionable Go errors.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is an authenticated HTTP client for the LastPing management API.
type Client struct {
	BaseURL string
	apiKey  string
	version string
	HTTP    *http.Client

	// RetryMaxAttempts and RetryMaxWait bound the 429 retry loop (see retry.go).
	// Zero selects the default.
	RetryMaxAttempts int
	RetryMaxWait     time.Duration

	// sleep is the backoff wait, swapped out in tests so the retry loop can be
	// exercised without real multi-second sleeps.
	sleep func(ctx context.Context, d time.Duration) error
}

// New creates a Client. Any trailing slash on baseURL is trimmed.
func New(baseURL, apiKey, version string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		version: version,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		sleep:   sleepCtx,
	}
}

// Problem is an RFC 7807 problem detail. LastPing adds Code and Fix, which are
// written for machine consumers — surfacing them turns an opaque 400 into an
// actionable Terraform diagnostic.
type Problem struct {
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Code   string `json:"code"`
	Fix    string `json:"fix"`
}

func (p *Problem) Error() string {
	var b strings.Builder
	switch {
	case p.Detail != "":
		b.WriteString(p.Detail)
	case p.Title != "":
		b.WriteString(p.Title)
	default:
		fmt.Fprintf(&b, "HTTP %d", p.Status)
	}
	if p.Code != "" {
		fmt.Fprintf(&b, " [%s]", p.Code)
	}
	if p.Fix != "" {
		fmt.Fprintf(&b, "\n\nSuggested fix: %s", p.Fix)
	}
	return b.String()
}

// IsNotFound reports whether err is a 404, so Read can remove the resource from
// state instead of failing the whole plan.
func IsNotFound(err error) bool { return statusIs(err, http.StatusNotFound) }

// IsPreconditionFailed reports whether err is a 412, which Create uses to detect
// a slug collision.
func IsPreconditionFailed(err error) bool { return statusIs(err, http.StatusPreconditionFailed) }

func statusIs(err error, code int) bool {
	var p *Problem
	return errors.As(err, &p) && p.Status == code
}

// ReqOpt customises a single request.
type ReqOpt func(*http.Request)

// WithHeader sets an extra request header.
func WithHeader(k, v string) ReqOpt {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

// Do performs an authenticated request. body is JSON-encoded when non-nil; the
// response is decoded into out when non-nil. Non-2xx responses return *Problem.
//
// A 429 is retried under the policy in retry.go; every other status is
// terminal. The encoded body is kept so each attempt gets a fresh reader.
func (c *Client) Do(ctx context.Context, method, path string, body, out any, opts ...ReqOpt) error {
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			return fmt.Errorf("encode request body: %w", err)
		}
	}

	var waited time.Duration
	for attempt := 1; ; attempt++ {
		res, err := c.attempt(ctx, method, path, encoded, body != nil, out, opts)
		switch {
		case err != nil:
			return err
		case res.problem == nil:
			return nil
		case res.status != http.StatusTooManyRequests:
			return res.problem
		}

		// Rate-limited. Give up once the attempt budget is spent or the next
		// wait would push past the total-wait ceiling, and surface the 429 as
		// the ordinary *Problem callers already know how to interpret.
		delay := retryDelay(res.retryAfter, attempt, time.Now())
		if attempt >= c.retryMaxAttempts() || waited+delay > c.retryMaxWait() {
			return res.problem
		}
		waited += delay
		if err := c.sleepFn()(ctx, delay); err != nil {
			return err
		}
	}
}

// attemptResult is one response, reduced to what the retry loop needs. status
// is the wire status code, deliberately separate from Problem.Status — the
// latter is whatever the response body claimed, and a retry decision must not
// be driven by a body an intermediary wrote.
type attemptResult struct {
	status     int
	retryAfter string
	problem    *Problem // non-nil iff the response was not 2xx
}

// attempt performs one request, decoding into out on success.
func (c *Client) attempt(
	ctx context.Context,
	method, path string,
	encoded []byte,
	hasBody bool,
	out any,
	opts []ReqOpt,
) (attemptResult, error) {
	var rdr io.Reader
	if hasBody {
		rdr = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return attemptResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "terraform-provider-lastping/"+c.version)
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return attemptResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	res := attemptResult{status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After")}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		p := &Problem{Status: resp.StatusCode}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = json.Unmarshal(raw, p) // a non-JSON body leaves Status set, which is enough
		if p.Status == 0 {
			p.Status = resp.StatusCode
		}
		res.problem = p
		return res, nil
	}

	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return res, nil
	}

	// A *[]byte out means "hand me the body verbatim". Only /api/v1/metrics
	// needs it — that endpoint answers in Prometheus text exposition format,
	// not JSON — and routing it through Do rather than a bespoke request keeps
	// it under the same auth headers, problem decoding and 429 retry policy as
	// every other call.
	if raw, ok := out.(*[]byte); ok {
		b, err := io.ReadAll(resp.Body)
		*raw = b
		return res, err
	}
	return res, json.NewDecoder(resp.Body).Decode(out)
}

// sleepFn returns the backoff wait, defaulting to a real timer so a
// zero-value Client still behaves.
func (c *Client) sleepFn() func(context.Context, time.Duration) error {
	if c.sleep != nil {
		return c.sleep
	}
	return sleepCtx
}
