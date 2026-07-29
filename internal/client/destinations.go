package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Destination mirrors the Channel schema in the LastPing OpenAPI spec — an
// alert delivery target (Slack, email, a webhook, …).
//
// SECURITY: the API stores Config but NEVER returns it. A response carries only
// Target, a deliberately non-secret hint (api/channels.go: channelDTO), which is
// the raw value for webhook/ntfy/email, "chat <id>" for telegram, and a fixed
// label such as "slack webhook" for the credential-bearing kinds. Config is
// therefore a request-only field: it is populated when building a payload and
// always empty on the way back.
type Destination struct {
	ID     string            `json:"id,omitempty"`
	Kind   string            `json:"kind,omitempty"`
	Name   string            `json:"name,omitempty"`
	Config map[string]string `json:"config,omitempty"`

	// Computed — server-supplied, never part of a request payload.
	Target        string `json:"target,omitempty"`
	Verified      bool   `json:"verified,omitempty"`
	Disabled      bool   `json:"disabled,omitempty"`
	DisableReason string `json:"disable_reason,omitempty"`
	CreatedAt     string `json:"created_at,omitempty"`
}

// createDestinationRequest is the POST /api/v1/channels body. It is a distinct
// type from Destination so response-only fields can never leak into a request.
type createDestinationRequest struct {
	Kind   string            `json:"kind"`
	Name   string            `json:"name"`
	Config map[string]string `json:"config"`
}

// updateDestinationRequest is the PATCH /api/v1/channels/{id} body. Both fields
// are pointers because the API treats an omitted field as "leave unchanged";
// kind is never sent because the API rejects any attempt to change it.
//
// Config is all-or-nothing server-side: when present it replaces the stored
// config wholesale and is re-validated against the kind's required fields, so a
// caller must send every field for the kind, secrets included.
type updateDestinationRequest struct {
	Name   *string            `json:"name,omitempty"`
	Config *map[string]string `json:"config,omitempty"`
}

// CreateDestination creates an alert destination.
func (c *Client) CreateDestination(ctx context.Context, d Destination) (*Destination, error) {
	body := createDestinationRequest{Kind: d.Kind, Name: d.Name, Config: d.Config}
	if body.Config == nil {
		body.Config = map[string]string{}
	}
	var out Destination
	if err := c.Do(ctx, http.MethodPost, "/api/v1/channels", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetDestination fetches a destination by its UUID.
func (c *Client) GetDestination(ctx context.Context, id string) (*Destination, error) {
	var out Destination
	if err := c.Do(ctx, http.MethodGet, "/api/v1/channels/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListDestinations returns every destination in the caller's project.
func (c *Client) ListDestinations(ctx context.Context) ([]Destination, error) {
	var out []Destination
	if err := c.Do(ctx, http.MethodGet, "/api/v1/channels", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DefaultEmailDestinationID returns the destination the API auto-routes every
// newly created monitor to, or "" when the project has none.
//
// It mirrors the server's own selection exactly (api/defaultdest.go:
// attachDefaultRoutes in the LastPing monorepo): the FIRST channel that is
// `kind = "email"` and verified, in the order the API returns them. Both sides
// read the same query — `SELECT * FROM channels WHERE project_id = $1 ORDER BY
// created_at` — and GET /api/v1/channels serialises those rows in order, so
// "first" means the same thing here as it does there.
//
// Two deliberate omissions, both of which keep this in step with the server:
//
//   - A disabled channel is still eligible, because attachDefaultRoutes tests
//     only verified_at. Skipping disabled channels here would make the provider
//     name a different channel than the one the auto-created route points at.
//
//   - No tie-break beyond created_at. That column is not unique, so two email
//     channels stamped in the same microsecond are ordered arbitrarily — by the
//     server and by this call alike, but not necessarily the same way. Reaching
//     that state takes two hand-made email channels created inside one clock
//     tick, because the auto-provisioning path creates at most one per project
//     (ensureDefaultEmailChannel is idempotent on kind). Losing that coin flip
//     costs a refusal to adopt, which is the safe direction, not an overwrite.
func (c *Client) DefaultEmailDestinationID(ctx context.Context) (string, error) {
	list, err := c.ListDestinations(ctx)
	if err != nil {
		return "", err
	}
	for i := range list {
		if list[i].Kind == "email" && list[i].Verified {
			return list[i].ID, nil
		}
	}
	return "", nil
}

// GetDestinationByName resolves a destination by its human-readable name.
//
// Destination names are NOT unique — unlike monitor slugs, the API imposes no
// uniqueness constraint — so an ambiguous name is an error naming every match
// rather than an arbitrary pick that would silently point a configuration at
// the wrong channel. The caller can then disambiguate by UUID.
//
// The ambiguity error is deliberately a plain error, not a *Problem: it is not
// something the server said, and a caller must not treat it as a 404 and
// conclude the destination does not exist.
func (c *Client) GetDestinationByName(ctx context.Context, name string) (*Destination, error) {
	list, err := c.ListDestinations(ctx)
	if err != nil {
		return nil, err
	}
	var matches []Destination
	for i := range list {
		if list[i].Name == name {
			matches = append(matches, list[i])
		}
	}
	switch len(matches) {
	case 1:
		return &matches[0], nil
	case 0:
		return nil, &Problem{
			Status: http.StatusNotFound,
			Detail: fmt.Sprintf("no destination named %q in this project", name),
		}
	default:
		ids := make([]string, 0, len(matches))
		for _, d := range matches {
			ids = append(ids, d.ID)
		}
		return nil, fmt.Errorf("%d destinations are named %q: %s; look it up by id instead",
			len(matches), name, strings.Join(ids, ", "))
	}
}

// UpdateDestination renames a destination and/or rotates its config in place.
// A nil name or config leaves that part untouched — which is what makes a
// rename a Terraform update rather than a destroy/create, and what keeps an
// email destination's verification intact when only the name changes.
func (c *Client) UpdateDestination(ctx context.Context, id string, name *string, config map[string]string) (*Destination, error) {
	body := updateDestinationRequest{Name: name}
	if config != nil {
		body.Config = &config
	}
	var out Destination
	if err := c.Do(ctx, http.MethodPatch, "/api/v1/channels/"+id, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteDestination deletes a destination.
func (c *Client) DeleteDestination(ctx context.Context, id string) error {
	return c.Do(ctx, http.MethodDelete, "/api/v1/channels/"+id, nil, nil)
}
