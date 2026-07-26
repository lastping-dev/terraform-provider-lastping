package client

import (
	"context"
	"net/http"
)

// templatesBody is the wire shape of both GET and PUT
// /api/v1/checks/{id}/templates.
//
// Keys are either an event type on its own (`down`, `recovery`, `fail`,
// `every-run`) or `event/cause` for a per-cause override such as
// `down/silence`.
type templatesBody struct {
	Templates map[string]string `json:"templates"`
}

// GetTemplates returns a monitor's custom alert message bodies, keyed as
// described on templatesBody. The map is empty when none are set; a monitor
// outside the caller's project returns 404.
func (c *Client) GetTemplates(ctx context.Context, monitorID string) (map[string]string, error) {
	var out templatesBody
	if err := c.Do(ctx, http.MethodGet, "/api/v1/checks/"+monitorID+"/templates", nil, &out); err != nil {
		return nil, err
	}
	if out.Templates == nil {
		out.Templates = map[string]string{}
	}
	return out.Templates, nil
}

// PutTemplates writes a monitor's alert message bodies and returns the
// resulting set.
//
// The API's replace semantics are NOT uniform, and the difference is the whole
// reason callers must not simply send the desired map (api/api_templates.go:
// handleAPIPutTemplates):
//
//   - Event-wide keys (`down`, `fail`, …) are replace-the-set: every one is
//     deleted first, then the request's entries are applied. Dropping such a
//     key from the map does remove it.
//   - Per-cause keys (`down/silence`, …) are only touched when the request
//     mentions them. Dropping one from the map leaves it in place — it is
//     removed only by sending it explicitly with an empty value.
//
// So a caller replacing a whole set must send "" for every key it wants gone.
// The API validates each body against a sample render context and rejects the
// entire request on the first bad template, so a write is all-or-nothing.
func (c *Client) PutTemplates(ctx context.Context, monitorID string, templates map[string]string) (map[string]string, error) {
	body := templatesBody{Templates: templates}
	if body.Templates == nil {
		body.Templates = map[string]string{}
	}
	var out templatesBody
	if err := c.Do(ctx, http.MethodPut, "/api/v1/checks/"+monitorID+"/templates", body, &out); err != nil {
		return nil, err
	}
	if out.Templates == nil {
		out.Templates = map[string]string{}
	}
	return out.Templates, nil
}
