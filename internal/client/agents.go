package client

import (
	"context"
	"fmt"
	"net/http"
)

// Agent mirrors the Agent schema in the LastPing OpenAPI spec: a registry entry
// for an autonomous worker that owns monitors.
//
// Only Name and Description are ever sent. Slug is derived server-side from
// Name at creation (api/agents_api.go: slugFromName) and is immutable
// afterwards, and everything below the Computed comment is rolled up live from
// the monitors the agent owns — none of it is storable, so none of it is part
// of a request body.
type Agent struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`

	// Computed — server-supplied, never part of a request payload.
	//
	// Slug is derived from Name, not accepted on create and ignored on patch.
	// Status/MonitorCount/LastSeen are not stored at all: the API recomputes
	// them on every response from the agent's monitors (core/agent.RollUp), so
	// they change without anything in Terraform changing.
	Slug         string  `json:"slug,omitempty"`
	Status       string  `json:"status,omitempty"`
	MonitorCount int64   `json:"monitor_count"`
	LastSeen     *string `json:"last_seen,omitempty"`
	CreatedAt    string  `json:"created_at,omitempty"`
}

// createAgentRequest is the POST /api/v1/agents body. It is a distinct type
// from Agent so the rolled-up response fields can never leak into a request,
// and so `slug` — which the endpoint does not accept — cannot be sent by
// accident.
//
// description is omitempty because an absent key and an empty string mean the
// same thing on create (the column is NOT NULL with an empty-string default).
// That equivalence does NOT hold on update; see AgentPatch.
type createAgentRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// AgentPatch is the body of PATCH /api/v1/agents/{id}: a sparse JSON Merge
// Patch (RFC 7396) document.
//
//   - an absent key leaves the stored value alone;
//   - a nil value serialises as JSON `null`. The API honours that for
//     `description` only, clearing it to the empty string (the column is NOT
//     NULL with an empty-string default). A null `name` is read as an omission
//     and leaves the stored name in place — a name has no meaningful "unset"
//     state, so it can never be blanked;
//   - any other value replaces the stored one.
//
// `slug` is immutable after creation and is ignored if present.
//
// This is a map and not a struct for the same reason MonitorPatch is: presence
// is the whole point of a merge patch, and a struct with `omitempty` makes
// "cleared" and "unset" the same wire bytes — which is precisely how removing
// an optional attribute from a configuration becomes a silent no-op.
type AgentPatch map[string]any

// IsConflict reports whether err is a 409, which CreateAgent uses to detect a
// collision on the slug the server derives from the agent's name.
//
// It is deliberately not IsSlugConflict (statuspages.go), despite testing the
// same status code. That predicate documents the status page slug namespace,
// which is GLOBAL across every account, so the conflicting page may belong to a
// stranger and the advice has to be "pick a name nobody else has taken". An
// agent slug is project-scoped: a conflict here is always the caller's own
// agent, and the right advice is to import it.
func IsConflict(err error) bool { return statusIs(err, http.StatusConflict) }

// CreateAgent registers an agent.
//
// It needs no If-None-Match header, unlike CreateMonitor: POST /api/v1/agents
// is genuinely create-only server-side. A name whose derived slug is already
// taken in this project is a 409 and nothing is written — the endpoint has no
// upsert branch that could silently adopt an existing agent.
func (c *Client) CreateAgent(ctx context.Context, a Agent) (*Agent, error) {
	body := createAgentRequest{Name: a.Name, Description: a.Description}
	var out Agent
	if err := c.Do(ctx, http.MethodPost, "/api/v1/agents", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAgent fetches an agent by its UUID. The endpoint is project-scoped: an
// agent owned by another project is a plain 404, never a signal that the id
// exists elsewhere.
func (c *Client) GetAgent(ctx context.Context, id string) (*Agent, error) {
	var out Agent
	if err := c.Do(ctx, http.MethodGet, "/api/v1/agents/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAgents returns every agent in the caller's project.
func (c *Client) ListAgents(ctx context.Context) ([]Agent, error) {
	var out []Agent
	if err := c.Do(ctx, http.MethodGet, "/api/v1/agents", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetAgentBySlug supports import by slug, which is far easier to write by hand
// than a UUID. Agent slugs are project-scoped, so a match against the caller's
// own list is unambiguous.
//
// There is no GET /api/v1/agents/{slug}: the path parameter is parsed as a UUID
// and a non-UUID is a 400, so resolution has to go through the list.
func (c *Client) GetAgentBySlug(ctx context.Context, slug string) (*Agent, error) {
	list, err := c.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Slug == slug {
			return &list[i], nil
		}
	}
	return nil, &Problem{Status: http.StatusNotFound, Detail: fmt.Sprintf("no agent with slug %q", slug)}
}

// UpdateAgent applies a merge patch to an agent. See AgentPatch for how an
// absent key, an explicit null and a value differ.
func (c *Client) UpdateAgent(ctx context.Context, id string, patch AgentPatch) (*Agent, error) {
	var out Agent
	if err := c.Do(ctx, http.MethodPatch, "/api/v1/agents/"+id, patch, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteAgent deletes an agent.
//
// It does NOT delete the agent's monitors: checks.agent_id is ON DELETE SET
// NULL, so every monitor the agent owned survives with its ping history and
// incidents intact and simply becomes unowned.
func (c *Client) DeleteAgent(ctx context.Context, id string) error {
	return c.Do(ctx, http.MethodDelete, "/api/v1/agents/"+id, nil, nil)
}
