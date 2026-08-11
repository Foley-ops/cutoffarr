package main

import (
	"context"
	"io"
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

// --- DoJSON -------------------------------------------------------------
//
// Phase 4 introduces the project's first and only write request (PUT
// /api/v3/movie/{id}). Do/DoQuery set only X-Api-Key, which is enough for
// every read endpoint but not for a request that carries a body: Radarr's
// ASP.NET Core stack rejects a body without a Content-Type it recognizes
// (415 Unsupported Media Type). DoJSON is the one entry point that sends a
// JSON body, so the header is set in exactly one place rather than being
// left to each caller to remember.

func TestAPIClient_DoJSON_SendsBodyMethodAndJSONContentType(t *testing.T) {
	var gotMethod, gotPath, gotContentType, gotAPIKey string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotAPIKey = r.Header.Get("X-Api-Key")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "my-secret-key")
	body := []byte(`{"id":7,"monitored":false}`)
	resp, err := c.DoJSON(context.Background(), http.MethodPut, "/api/v3/movie/7", body)
	if err != nil {
		t.Fatalf("DoJSON returned error: %v", err)
	}
	defer resp.Body.Close()

	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotPath != "/api/v3/movie/7" {
		t.Errorf("path = %q, want /api/v3/movie/7", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotAPIKey != "my-secret-key" {
		t.Errorf("X-Api-Key = %q, want my-secret-key", gotAPIKey)
	}
	if string(gotBody) != string(body) {
		t.Errorf("request body = %q, want %q", gotBody, body)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
}

func TestAPIClient_DoJSON_NonTwoxxIsErrorWithBodySnippet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"message":"movie not found"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "key")
	_, err := c.DoJSON(context.Background(), http.MethodPut, "/api/v3/movie/7", []byte(`{}`))
	if err == nil {
		t.Fatal("DoJSON returned nil error, want error for 400 response")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "movie not found") {
		t.Errorf("error %q does not carry the status and body snippet", err.Error())
	}
}

// TestAPIClient_Do_SetsNoContentTypeHeader pins that the read path is
// unchanged: only DoJSON sets Content-Type, so a bodyless GET never starts
// advertising a body content type it does not have.
func TestAPIClient_Do_SetsNoContentTypeHeader(t *testing.T) {
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "key")
	resp, err := c.Do(context.Background(), http.MethodGet, "/api/v3/movie", nil)
	if err != nil {
		t.Fatalf("Do returned error: %v", err)
	}
	defer resp.Body.Close()

	if gotContentType != "" {
		t.Errorf("Content-Type = %q, want empty for a bodyless GET", gotContentType)
	}
}
