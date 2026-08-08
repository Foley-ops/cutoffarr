package main

import (
	"context"
	"net/http"
	"net/http/httptest"
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
