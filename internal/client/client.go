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
}

// New creates a Client. Any trailing slash on baseURL is trimmed.
func New(baseURL, apiKey, version string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		version: version,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
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
func (c *Client) Do(ctx context.Context, method, path string, body, out any, opts ...ReqOpt) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request body: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "terraform-provider-lastping/"+c.version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		p := &Problem{Status: resp.StatusCode}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = json.Unmarshal(raw, p) // a non-JSON body leaves Status set, which is enough
		if p.Status == 0 {
			p.Status = resp.StatusCode
		}
		return p
	}

	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
