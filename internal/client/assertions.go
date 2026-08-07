package client

import (
	"context"
	"net/http"
)

// Assertion is one output assertion attached to a monitor: a condition the ping
// body of a successful run must satisfy. When one fails, the success ping opens
// an incident with cause `assertion` instead of being recorded as a healthy
// check-in.
//
// The omitempty tags mirror the API's own DTO (api/api_assertions.go). Value,
// Path and Op are each meaningful for only some kinds, and the API stores the
// empty string for the rest — so omitting them on the wire and receiving them
// absent are the same state.
type Assertion struct {
	// ID is server-assigned and read-only. The endpoint replaces the whole set
	// on every write, so an id sent back is ignored rather than honoured — a
	// write is never an update of a particular row.
	ID string `json:"id,omitempty"`

	// Name is required and appears in the incident and the alert.
	Name string `json:"name"`

	// Kind is one of contains, not_contains, matches, json_path.
	Kind string `json:"kind"`

	// Value is the substring (contains/not_contains), the RE2 pattern
	// (matches), or the expected value (json_path).
	Value string `json:"value,omitempty"`

	// Path is a dotted path into the body parsed as JSON ("a.b.c"), json_path
	// only. Query syntax ('[', '*', '$') is rejected by the API.
	Path string `json:"path,omitempty"`

	// Op is the comparison operator for json_path: eq, ne, gt, gte, lt, lte.
	Op string `json:"op,omitempty"`
}

// assertionsBody is the wire shape of both GET and PUT
// /api/v1/checks/{id}/assertions.
type assertionsBody struct {
	Assertions []Assertion `json:"assertions"`
}

// GetAssertions returns a monitor's output assertions, in the server's order.
// The slice is empty (never nil) when none are set; a monitor outside the
// caller's project returns 404.
func (c *Client) GetAssertions(ctx context.Context, monitorID string) ([]Assertion, error) {
	var out assertionsBody
	if err := c.Do(ctx, http.MethodGet, "/api/v1/checks/"+monitorID+"/assertions", nil, &out); err != nil {
		return nil, err
	}
	if out.Assertions == nil {
		out.Assertions = []Assertion{}
	}
	return out.Assertions, nil
}

// PutAssertions replaces a monitor's whole assertion set and returns the result.
//
// Unlike PutTemplates, the semantics here are uniform and simple: the array
// sent IS the monitor's complete set afterwards. Sending an empty array removes
// every assertion; there is no per-entry "leave it alone" encoding, and no
// endpoint that edits a single assertion.
//
// The API validates every entry with the same evaluator the processor runs on
// each success ping, and rejects the whole request naming the first bad entry —
// so a write is all-or-nothing and a malformed pattern can never be stored. The
// cap is 20 per monitor.
func (c *Client) PutAssertions(ctx context.Context, monitorID string, assertions []Assertion) ([]Assertion, error) {
	body := assertionsBody{Assertions: assertions}
	if body.Assertions == nil {
		// Send [] rather than null: the two are equivalent to this API, but an
		// explicit empty array is what "the set is now empty" looks like.
		body.Assertions = []Assertion{}
	}
	var out assertionsBody
	if err := c.Do(ctx, http.MethodPut, "/api/v1/checks/"+monitorID+"/assertions", body, &out); err != nil {
		return nil, err
	}
	if out.Assertions == nil {
		out.Assertions = []Assertion{}
	}
	return out.Assertions, nil
}
