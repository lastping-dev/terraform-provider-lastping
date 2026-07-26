package client

import (
	"context"
	"net/http"
)

// Route is one monitor's alert routing for a single event type: the ordered
// list of destinations LastPing notifies when that event fires.
//
// ChannelIDs is a *list*, not a set. The API stores and returns the array in
// the order it was sent (api/routes.go: handleUpsertRoute stores the deduped
// slice verbatim), so order is observable and must round-trip.
type Route struct {
	EventType  string   `json:"event_type"`
	ChannelIDs []string `json:"channel_ids"`
}

// upsertRouteRequest is the PUT /api/v1/checks/{id}/routes/{event_type} body.
//
// channel_ids REPLACES the whole list — there is no add/remove endpoint — which
// is why a route is modelled as one resource per (monitor, event_type) owning
// the entire destination list.
type upsertRouteRequest struct {
	ChannelIDs []string `json:"channel_ids"`
}

// UpsertRoute replaces the destination list for one (monitor, event type) pair.
// It is used for both create and update: the endpoint is idempotent, so there
// is nothing for the provider to distinguish.
//
// A nil list is sent as [], which the API accepts and stores as "no
// destinations for this event" — distinct from deleting the route.
func (c *Client) UpsertRoute(ctx context.Context, monitorID, eventType string, destinationIDs []string) (*Route, error) {
	body := upsertRouteRequest{ChannelIDs: destinationIDs}
	if body.ChannelIDs == nil {
		body.ChannelIDs = []string{}
	}
	var out Route
	err := c.Do(ctx, http.MethodPut,
		"/api/v1/checks/"+monitorID+"/routes/"+eventType, body, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRoutes returns every route configured on a monitor. There is no
// single-route GET, so Read fetches the list and picks its event type out of
// it. A monitor that does not exist in the caller's project returns 404.
func (c *Client) GetRoutes(ctx context.Context, monitorID string) ([]Route, error) {
	var out []Route
	if err := c.Do(ctx, http.MethodGet, "/api/v1/checks/"+monitorID+"/routes", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetRoute returns one monitor's route for a single event type, or a 404
// *Problem when that event type has no route — which is what lets Read tell
// "deleted out of band" from "the whole monitor is gone".
func (c *Client) GetRoute(ctx context.Context, monitorID, eventType string) (*Route, error) {
	routes, err := c.GetRoutes(ctx, monitorID)
	if err != nil {
		return nil, err
	}
	for i := range routes {
		if routes[i].EventType == eventType {
			return &routes[i], nil
		}
	}
	return nil, &Problem{
		Status: http.StatusNotFound,
		Detail: "no " + eventType + " route on monitor " + monitorID,
	}
}

// DeleteRoute removes a monitor's route for one event type.
func (c *Client) DeleteRoute(ctx context.Context, monitorID, eventType string) error {
	return c.Do(ctx, http.MethodDelete,
		"/api/v1/checks/"+monitorID+"/routes/"+eventType, nil, nil)
}
