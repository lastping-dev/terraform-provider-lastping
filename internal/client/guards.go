package client

import (
	"context"
	"net/http"
)

// MetricGuard is one metric guard attached to a monitor: a ceiling on a number
// the job reports about itself. An assertion catches a run that did nothing; a
// guard catches the opposite. The guard reads a number out of the ping body at
// Path, rolls it up across the trailing WindowS seconds with Aggregation, and
// opens an incident with cause `runaway` when the result exceeds Ceiling.
//
// The field names mirror the API's own DTO (api/api_guards.go). Note that Path
// is `path` on the wire even though the column behind it is `json_path`: it is
// the identical dotted path an Assertion carries, read by the identical code,
// and the API deliberately gives one concept one name across the two sibling
// endpoints.
type MetricGuard struct {
	// ID is server-assigned and read-only. The endpoint replaces the whole set
	// on every write, so an id sent back is ignored rather than honoured.
	ID string `json:"id,omitempty"`

	// Name is required. It is written to the incident's detail when the guard
	// trips, and is the only thing that distinguishes a tripped guard from the
	// fixed pings-per-hour runaway ceiling in the alert — both carry cause
	// `runaway`.
	Name string `json:"name"`

	// Path is a dotted path into the ping body parsed as JSON ("cost.usd").
	// Query syntax ('[', '*', '$') is rejected by the API.
	Path string `json:"path"`

	// WindowS is the trailing window in seconds. The API caps it at 604800.
	WindowS int64 `json:"window_s"`

	// Ceiling is the value the aggregate must not exceed. Equal does not trip.
	Ceiling float64 `json:"ceiling"`

	// Aggregation is one of sum, max, avg.
	Aggregation string `json:"aggregation"`
}

// guardsBody is the wire shape of both GET and PUT
// /api/v1/checks/{id}/guards.
type guardsBody struct {
	Guards []MetricGuard `json:"guards"`
}

// GetMetricGuards returns a monitor's metric guards, in the server's order. The
// slice is empty (never nil) when none are set; a monitor outside the caller's
// project returns 404.
func (c *Client) GetMetricGuards(ctx context.Context, monitorID string) ([]MetricGuard, error) {
	var out guardsBody
	if err := c.Do(ctx, http.MethodGet, "/api/v1/checks/"+monitorID+"/guards", nil, &out); err != nil {
		return nil, err
	}
	if out.Guards == nil {
		out.Guards = []MetricGuard{}
	}
	return out.Guards, nil
}

// PutMetricGuards replaces a monitor's whole guard set and returns the result.
//
// Same semantics as PutAssertions: the array sent IS the monitor's complete set
// afterwards, an empty array removes every guard, and there is no endpoint that
// edits a single guard. The API validates every entry before writing anything
// and rejects the whole request naming the first bad guard, so a write is
// all-or-nothing.
//
// Two caps apply and both are cost: at most 5 guards per monitor, and window_s
// at most 604800 seconds. Each guard re-aggregates every ping body in its
// window on every ping, so the per-ping work is linear in both.
func (c *Client) PutMetricGuards(ctx context.Context, monitorID string, guards []MetricGuard) ([]MetricGuard, error) {
	body := guardsBody{Guards: guards}
	if body.Guards == nil {
		// Send [] rather than null: an explicit empty array is what "the set is
		// now empty" looks like.
		body.Guards = []MetricGuard{}
	}
	var out guardsBody
	if err := c.Do(ctx, http.MethodPut, "/api/v1/checks/"+monitorID+"/guards", body, &out); err != nil {
		return nil, err
	}
	if out.Guards == nil {
		out.Guards = []MetricGuard{}
	}
	return out.Guards, nil
}
