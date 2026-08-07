package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// apiClientTimeout bounds every request made by APIClient. There is no
// retry or backoff behavior by design: the plan forbids automatic write
// retries, and reads are kept equally simple.
const apiClientTimeout = 15 * time.Second

// APIClient is a minimal wrapper around net/http for talking to a single
// Sonarr or Radarr instance. It is introduced in this phase but not called
// anywhere yet; Phase 1 is the first phase that issues real *arr API calls.
type APIClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewAPIClient builds a client for a single instance's base URL and API key.
func NewAPIClient(baseURL, apiKey string) *APIClient {
	return &APIClient{
		baseURL:    baseURL,
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: apiClientTimeout},
	}
}

// Do issues an HTTP request against path (joined onto the client's base
// URL), setting the X-Api-Key header on every call. Any non-2xx response is
// treated as an error for that call.
func (c *APIClient) Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	reqURL, err := url.JoinPath(c.baseURL, path)
	if err != nil {
		return nil, fmt.Errorf("client: building request url from base %q and path %q: %w", c.baseURL, path, err)
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, fmt.Errorf("client: building request: %w", err)
	}
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("client: %s %s: %w", method, reqURL, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, fmt.Errorf("client: %s %s: unexpected status %d", method, reqURL, resp.StatusCode)
	}

	return resp, nil
}
