package main

import (
	"bytes"
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

// errorBodySnippetLimit caps how much of a non-2xx response body is read
// into the returned error, so a large or unexpected response (e.g. an HTML
// error page from a reverse proxy) can't bloat error output. It is enough
// to distinguish, for example, a JSON "invalid API key" body from a wrong
// URL returning an HTML 404 page.
const errorBodySnippetLimit = 200

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
// treated as an error for that call. It is a thin wrapper around DoQuery
// with no query parameters attached.
func (c *APIClient) Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	return c.DoQuery(ctx, method, path, nil, body)
}

// DoQuery is Do plus a query url.Values, attached to the request URL
// correctly. It exists as a sibling to Do rather than an added parameter to
// it because url.JoinPath (used to join path onto the client's base URL)
// treats "?" as a literal path character and percent-encodes it (confirmed
// empirically): embedding a query string directly in the path argument
// would corrupt it. DoQuery instead joins only the path, then attaches the
// query via url.Values.Encode() on the parsed result. A nil or empty query
// attaches no "?" at all, matching Do's existing behavior exactly.
func (c *APIClient) DoQuery(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Response, error) {
	return c.do(ctx, method, path, query, body, "")
}

// DoJSON issues a request carrying a JSON request body, setting
// Content-Type: application/json alongside the usual X-Api-Key. It exists
// as the single entry point for the project's only write request (PUT
// /api/v3/movie/{id}, writer.go): Do and DoQuery deliberately set no
// Content-Type at all, which is correct for every bodyless read but not for
// a request with a body — Radarr's ASP.NET Core stack rejects a body whose
// content type it does not recognize (415 Unsupported Media Type). Putting
// the header here rather than at the call site means a future write path
// cannot forget it.
//
// The body is taken as []byte rather than an io.Reader because every caller
// has already marshaled the payload in full (and must, to compare it
// against what was fetched), and because a []byte lets net/http set an
// exact Content-Length instead of falling back to chunked encoding.
func (c *APIClient) DoJSON(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	return c.do(ctx, method, path, nil, bytes.NewReader(body), "application/json")
}

// do is the single implementation behind Do, DoQuery, and DoJSON: it joins
// path onto the base URL, attaches query (if any), sets X-Api-Key plus
// contentType (when non-empty), issues the request, and treats any non-2xx
// response as an error carrying a bounded snippet of the response body.
func (c *APIClient) do(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	joined, err := url.JoinPath(c.baseURL, path)
	if err != nil {
		return nil, fmt.Errorf("client: building request url from base %q and path %q: %w", c.baseURL, path, err)
	}

	reqURL := joined
	if len(query) > 0 {
		parsed, err := url.Parse(joined)
		if err != nil {
			return nil, fmt.Errorf("client: parsing joined url %q: %w", joined, err)
		}
		parsed.RawQuery = query.Encode()
		reqURL = parsed.String()
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, fmt.Errorf("client: building request: %w", err)
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("client: %s %s: %w", method, reqURL, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodySnippetLimit))
		return nil, fmt.Errorf("client: %s %s: unexpected status %d: %s", method, reqURL, resp.StatusCode, snippet)
	}

	return resp, nil
}
