package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// StatusPage mirrors the StatusPage schema in the LastPing OpenAPI spec: a
// public or private page rendering the current state of a set of monitors.
//
// Slug is GLOBALLY unique across every LastPing account, unlike every other
// identifier in this package — `/status/{slug}` is a public URL, so only one
// page anywhere can own it (db/migrations/000011_status_pages.up.sql creates
// UNIQUE (slug) with no project column). See IsSlugConflict.
type StatusPage struct {
	ID       string   `json:"id,omitempty"`
	Slug     string   `json:"slug,omitempty"`
	Title    string   `json:"title,omitempty"`
	CheckIDs []string `json:"check_ids"`

	Visibility string `json:"visibility,omitempty"`

	// Computed — server-supplied, never part of a request payload.
	PublicURL string `json:"public_url,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

// createStatusPageRequest is the POST /api/v1/status-pages body.
//
// Slug is omitempty because it is genuinely optional on create: the server
// generates a random 10-character slug when it is absent. Every other request
// field is always sent.
type createStatusPageRequest struct {
	Slug       string   `json:"slug,omitempty"`
	Title      string   `json:"title"`
	CheckIDs   []string `json:"check_ids"`
	Visibility string   `json:"visibility"`
}

// updateStatusPageRequest is the PATCH /api/v1/status-pages/{id} body. Unlike
// create it is a full replacement and slug is REQUIRED — the API validates it
// unconditionally (api/statuspages.go: handleUpdateStatusPage), so an update
// must resend the current slug rather than omit it.
type updateStatusPageRequest struct {
	Slug       string   `json:"slug"`
	Title      string   `json:"title"`
	CheckIDs   []string `json:"check_ids"`
	Visibility string   `json:"visibility"`
}

// IsSlugConflict reports whether err is the 409 the API returns when a status
// page slug is already registered.
//
// This is the only cross-tenant error in the LastPing API: the slug namespace
// is global, so the conflicting page may belong to a completely different
// account and the caller has no way to see it. Callers must say so rather than
// implying a mistake in the caller's own configuration.
func IsSlugConflict(err error) bool { return statusIs(err, http.StatusConflict) }

// IsPublicPageLimit reports whether err is the free-tier cap of one public
// status page per project (api/statuspages.go: handleCreateStatusPage).
//
// It is deliberately NOT folded into IsSlugConflict. Both are 4xx failures on
// create, but they mean opposite things — one is "someone else owns this
// name", the other is "your own plan allows one public page" — and a caller
// told the wrong one will debug entirely the wrong problem. The detail string
// is matched because 403 alone is not specific enough to justify the claim.
func IsPublicPageLimit(err error) bool {
	var p *Problem
	return errors.As(err, &p) &&
		p.Status == http.StatusForbidden &&
		strings.Contains(p.Detail, "public status page")
}

// CreateStatusPage creates a status page. An empty Slug asks the server to
// generate one.
func (c *Client) CreateStatusPage(ctx context.Context, sp StatusPage) (*StatusPage, error) {
	body := createStatusPageRequest{
		Slug:       sp.Slug,
		Title:      sp.Title,
		CheckIDs:   sp.CheckIDs,
		Visibility: sp.Visibility,
	}
	if body.CheckIDs == nil {
		body.CheckIDs = []string{}
	}
	var out StatusPage
	if err := c.Do(ctx, http.MethodPost, "/api/v1/status-pages", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetStatusPage fetches a status page by its UUID. The endpoint is
// project-scoped: a page owned by another account is a plain 404, never a
// signal that the id exists elsewhere.
func (c *Client) GetStatusPage(ctx context.Context, id string) (*StatusPage, error) {
	var out StatusPage
	if err := c.Do(ctx, http.MethodGet, "/api/v1/status-pages/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListStatusPages returns every status page in the caller's project.
func (c *Client) ListStatusPages(ctx context.Context) ([]StatusPage, error) {
	var out []StatusPage
	if err := c.Do(ctx, http.MethodGet, "/api/v1/status-pages", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetStatusPageBySlug resolves a slug WITHIN THE CALLER'S PROJECT ONLY, by
// filtering the project's own list.
//
// SECURITY: it must never fall back to a global lookup. The slug namespace is
// global, so a slug the caller does not own may well exist — reporting that
// would turn `terraform import` into an oracle for other accounts' page names.
// A slug outside the project is reported exactly like one that does not exist
// at all.
func (c *Client) GetStatusPageBySlug(ctx context.Context, slug string) (*StatusPage, error) {
	list, err := c.ListStatusPages(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Slug == slug {
			return &list[i], nil
		}
	}
	return nil, &Problem{
		Status: http.StatusNotFound,
		Detail: fmt.Sprintf("no status page with slug %q in this project", slug),
	}
}

// UpdateStatusPage replaces a status page's attributes. Every field is sent:
// PATCH is a full replacement here, and an omitted slug or visibility is a 400.
func (c *Client) UpdateStatusPage(ctx context.Context, id string, sp StatusPage) (*StatusPage, error) {
	body := updateStatusPageRequest{
		Slug:       sp.Slug,
		Title:      sp.Title,
		CheckIDs:   sp.CheckIDs,
		Visibility: sp.Visibility,
	}
	if body.CheckIDs == nil {
		body.CheckIDs = []string{}
	}
	var out StatusPage
	if err := c.Do(ctx, http.MethodPatch, "/api/v1/status-pages/"+id, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteStatusPage deletes a status page, releasing its slug back into the
// global namespace.
func (c *Client) DeleteStatusPage(ctx context.Context, id string) error {
	return c.Do(ctx, http.MethodDelete, "/api/v1/status-pages/"+id, nil, nil)
}
