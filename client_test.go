package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNewAPIClient_SetsFifteenSecondTimeout(t *testing.T) {
	c := NewAPIClient("http://radarr:7878", "some-key")
	if c.httpClient.Timeout != 15*time.Second {
		t.Errorf("client timeout = %v, want 15s", c.httpClient.Timeout)
	}
}

func TestAPIClient_Do_SetsAPIKeyHeader(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "my-secret-key")
	resp, err := c.Do(context.Background(), http.MethodGet, "/api/v3/system/status", nil)
	if err != nil {
		t.Fatalf("Do returned error: %v", err)
	}
	defer resp.Body.Close()

	if gotHeader != "my-secret-key" {
		t.Errorf("X-Api-Key header = %q, want %q", gotHeader, "my-secret-key")
	}
}

func TestAPIClient_Do_JoinsBaseURLAndPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "key")
	resp, err := c.Do(context.Background(), http.MethodGet, "/api/v3/movie", nil)
	if err != nil {
		t.Fatalf("Do returned error: %v", err)
	}
	defer resp.Body.Close()

	if gotPath != "/api/v3/movie" {
		t.Errorf("request path = %q, want %q", gotPath, "/api/v3/movie")
	}
}

func TestAPIClient_Do_NonTwoxxStatusIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "key")
	_, err := c.Do(context.Background(), http.MethodGet, "/api/v3/movie", nil)
	if err == nil {
		t.Fatal("Do returned nil error, want error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not mention status 500", err.Error())
	}
}

func TestAPIClient_Do_ErrorIncludesResponseBodySnippet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Unauthorized"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "bad-key")
	_, err := c.Do(context.Background(), http.MethodGet, "/api/v3/system/status", nil)
	if err == nil {
		t.Fatal("Do returned nil error, want error for 401 response")
	}
	if !strings.Contains(err.Error(), `{"error":"Unauthorized"}`) {
		t.Errorf("error %q does not contain the response body snippet", err.Error())
	}
}

func TestAPIClient_Do_ErrorResponseBodySnippetIsTruncatedAt200Bytes(t *testing.T) {
	long := strings.Repeat("x", 300)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(long))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "key")
	_, err := c.Do(context.Background(), http.MethodGet, "/api/v3/movie", nil)
	if err == nil {
		t.Fatal("Do returned nil error, want error for 500 response")
	}
	if !strings.Contains(err.Error(), strings.Repeat("x", 200)) {
		t.Errorf("error does not contain the expected 200-byte snippet: %q", err.Error())
	}
	if strings.Contains(err.Error(), strings.Repeat("x", 201)) {
		t.Errorf("error contains more than 200 bytes of the body, want truncation: %q", err.Error())
	}
}

// --- DoQuery ------------------------------------------------------------

// TestAPIClient_DoQuery_EncodesQueryParameters pins the query-param
// encoding behavior migrated out of radarr.go's now-deleted doGet: joining
// a path containing a literal "?" via url.JoinPath percent-encodes it
// (confirmed empirically in the original doGet comment), so query
// parameters must be attached by parsing the joined URL and setting
// RawQuery, not by appending "?"+query.Encode() to the path string.
func TestAPIClient_DoQuery_EncodesQueryParameters(t *testing.T) {
	var gotPath, gotRawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "key")
	query := url.Values{"movieId": {"5"}, "page": {"2"}}
	resp, err := c.DoQuery(context.Background(), http.MethodGet, "/api/v3/moviefile", query, nil)
	if err != nil {
		t.Fatalf("DoQuery returned error: %v", err)
	}
	defer resp.Body.Close()

	if gotPath != "/api/v3/moviefile" {
		t.Errorf("request path = %q, want %q", gotPath, "/api/v3/moviefile")
	}
	gotValues, err := url.ParseQuery(gotRawQuery)
	if err != nil {
		t.Fatalf("parsing recorded raw query %q: %v", gotRawQuery, err)
	}
	if gotValues.Get("movieId") != "5" || gotValues.Get("page") != "2" {
		t.Errorf("query = %q, want movieId=5 and page=2", gotRawQuery)
	}
}

// TestAPIClient_DoQuery_NilQueryBehavesLikeDo pins that DoQuery with a nil
// (or empty) query attaches no "?" at all, matching APIClient.Do's existing
// no-query behavior exactly (Do is expected to become a thin wrapper around
// DoQuery).
func TestAPIClient_DoQuery_NilQueryBehavesLikeDo(t *testing.T) {
	var gotRawQuery string
	var sawRawQuery bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		sawRawQuery = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "key")
	resp, err := c.DoQuery(context.Background(), http.MethodGet, "/api/v3/movie", nil, nil)
	if err != nil {
		t.Fatalf("DoQuery returned error: %v", err)
	}
	defer resp.Body.Close()

	if !sawRawQuery {
		t.Fatal("request never reached the server")
	}
	if gotRawQuery != "" {
		t.Errorf("raw query = %q, want empty for a nil query", gotRawQuery)
	}
}

// TestAPIClient_DoQuery_SetsAPIKeyHeaderAndNonTwoxxIsError pins that
// DoQuery still goes through the same header-setting and non-2xx error
// handling as Do, since Do now delegates to it.
func TestAPIClient_DoQuery_SetsAPIKeyHeaderAndNonTwoxxIsError(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "my-secret-key")
	_, err := c.DoQuery(context.Background(), http.MethodGet, "/api/v3/moviefile", url.Values{"movieId": {"1"}}, nil)
	if err == nil {
		t.Fatal("DoQuery returned nil error, want error for 500 response")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q does not contain the response body snippet", err.Error())
	}
	if gotHeader != "my-secret-key" {
		t.Errorf("X-Api-Key header = %q, want %q", gotHeader, "my-secret-key")
	}
}

func TestAPIClient_Do_SuccessReturnsResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "key")
	resp, err := c.Do(context.Background(), http.MethodPost, "/api/v3/movie", nil)
	if err != nil {
		t.Fatalf("Do returned error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
}
