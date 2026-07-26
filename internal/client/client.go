// Package client provides an authenticated HTTP client for the LastPing
// management API. This is a minimal stub; Task 2 fleshes out the request
// methods used by resources and data sources.
package client

import (
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
