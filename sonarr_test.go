package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// This file reuses newDecisionTestLogger (decision_test.go) for the same
// buffered slog.Logger every other *_test.go in this project builds against.

// --- fetchSeriesLibrary ----------------------------------------------------

const sonarrSeriesJSON = `[
	{"id": 1, "title": "Show One", "monitored": true, "qualityProfileId": 1, "tags": [1, 2],
	 "seasons": [
		{"seasonNumber": 1, "monitored": true, "statistics": {"episodeFileCount": 10, "totalEpisodeCount": 10}},
		{"seasonNumber": 2, "monitored": false, "statistics": {"episodeFileCount": 0, "totalEpisodeCount": 10}}
	 ]},
	{"id": 2, "title": "Show Two", "monitored": false, "qualityProfileId": 2, "tags": [],
	 "seasons": [
		{"seasonNumber": 1, "monitored": true, "statistics": {"episodeFileCount": 5, "totalEpisodeCount": 5}}
	 ]}
]`

func TestFetchSeriesLibrary_HappyPath_ReturnsDecodedElements(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sonarrSeriesJSON))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelDebug)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	series, ok := fetchSeriesLibrary(context.Background(), logger, client, inst)
	if !ok {
		t.Fatalf("fetchSeriesLibrary returned ok=false, want true:\n%s", buf.String())
	}
	if len(series) != 2 {
		t.Fatalf("got %d series, want 2", len(series))
	}
	s := series[0]
	if s.ID == nil || *s.ID != 1 || s.Title == nil || *s.Title != "Show One" {
		t.Errorf("series[0] id/title = %v/%v, want 1/Show One", derefOrAbsent(s.ID), derefOrAbsent(s.Title))
	}
	if s.Seasons == nil {
		t.Fatalf("series[0].Seasons is nil, want a non-nil slice of 2 elements")
	}
	if len(*s.Seasons) != 2 {
		t.Fatalf("series[0] has %d seasons, want 2", len(*s.Seasons))
	}
	season1 := (*s.Seasons)[0]
	if season1.SeasonNumber == nil || *season1.SeasonNumber != 1 || season1.Monitored == nil || !*season1.Monitored {
		t.Errorf("season[0] = %+v, want seasonNumber=1 monitored=true", season1)
	}
	if season1.Statistics == nil || season1.Statistics.EpisodeFileCount == nil || *season1.Statistics.EpisodeFileCount != 10 {
		t.Errorf("season[0].Statistics = %+v, want episodeFileCount=10", season1.Statistics)
	}
	if !strings.Contains(buf.String(), "total=2") || !strings.Contains(buf.String(), "monitored=1") {
		t.Errorf("expected a series library summary log line with total=2 monitored=1:\n%s", buf.String())
	}
}

func TestFetchSeriesLibrary_RequestFailure_ReturnsNotOK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-broken", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	_, ok := fetchSeriesLibrary(context.Background(), logger, client, inst)
	if ok {
		t.Error("fetchSeriesLibrary returned ok=true, want false when the request fails")
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "sonarr-broken") {
		t.Errorf("expected a warning naming the instance:\n%s", buf.String())
	}
}

func TestFetchSeriesLibrary_MalformedJSON_ReturnsNotOK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-malformed", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	_, ok := fetchSeriesLibrary(context.Background(), logger, client, inst)
	if ok {
		t.Error("fetchSeriesLibrary returned ok=true, want false for malformed JSON")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

func TestFetchSeriesLibrary_NotAJSONArray_ReturnsNotOK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"unexpected": "object"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-broken", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	_, ok := fetchSeriesLibrary(context.Background(), logger, client, inst)
	if ok {
		t.Error("fetchSeriesLibrary returned ok=true, want false for a non-array response")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

func TestFetchSeriesLibrary_TruncatedBetweenElements_ReturnsNotOK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, r *http.Request) {
		// Missing the closing bracket: a body cut exactly on an element
		// boundary, which json.Decoder's More() alone would not treat as an
		// error (mirrors radarr.go's fetchMovies closing-bracket check).
		w.Write([]byte(`[{"id": 1, "title": "Show One", "monitored": true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-broken", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	_, ok := fetchSeriesLibrary(context.Background(), logger, client, inst)
	if ok {
		t.Error("fetchSeriesLibrary returned ok=true, want false for a truncated array")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

// TestFetchSeriesLibrary_TagsArrayContainsNullElement_NormalizedToAbsent
// mirrors TestInspectRadarrLibrary_TagsArrayContainsNullElement_NormalizedToAbsent
// (radarr_test.go): the same null-array-element corner applies to
// seriesElement.Tags, and the fix (radarr.go's rawObjectField+decodeTagIDs
// pattern) is reused rather than forked.
func TestFetchSeriesLibrary_TagsArrayContainsNullElement_NormalizedToAbsent(t *testing.T) {
	seriesJSON := `[{"id": 1, "title": "Corrupted Tags Show", "monitored": true, "qualityProfileId": 1, "tags": [3, null, 9], "seasons": []}]`
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(seriesJSON))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	series, ok := fetchSeriesLibrary(context.Background(), logger, client, inst)
	if !ok {
		t.Fatalf("fetchSeriesLibrary returned ok=false, want true (a corrupted tags array is per-series, not instance-fatal):\n%s", buf.String())
	}
	if len(series) != 1 {
		t.Fatalf("expected 1 series, got %d", len(series))
	}
	if series[0].Tags != nil {
		t.Errorf("expected Tags to be normalized to nil after a null array element was detected, got %v", *series[0].Tags)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "Corrupted Tags Show") {
		t.Errorf("expected a warning naming the series whose tags array contained a null element:\n%s", out)
	}
}

// --- fetchEpisodes -----------------------------------------------------

func TestFetchEpisodes_HappyPath_PassesSeriesIdAsQueryParam(t *testing.T) {
	var gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`[{"id": 100, "seasonNumber": 1, "episodeNumber": 1, "monitored": true, "hasFile": true, "airDateUtc": "2020-01-01T00:00:00Z", "episodeFileId": 500}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	episodes, ok := fetchEpisodes(context.Background(), logger, client, inst, 7)
	if !ok {
		t.Fatalf("fetchEpisodes returned ok=false, want true:\n%s", buf.String())
	}
	if gotQuery != "seriesId=7" {
		t.Errorf("query = %q, want seriesId=7", gotQuery)
	}
	if len(episodes) != 1 {
		t.Fatalf("got %d episodes, want 1", len(episodes))
	}
	e := episodes[0]
	if e.ID == nil || *e.ID != 100 || e.SeasonNumber == nil || *e.SeasonNumber != 1 {
		t.Errorf("episode = %+v, want id=100 seasonNumber=1", e)
	}
	if e.AirDateUtc == nil || *e.AirDateUtc != "2020-01-01T00:00:00Z" {
		t.Errorf("episode.AirDateUtc = %v, want 2020-01-01T00:00:00Z", derefOrAbsent(e.AirDateUtc))
	}
	if e.EpisodeFileID == nil || *e.EpisodeFileID != 500 {
		t.Errorf("episode.EpisodeFileID = %v, want 500", derefOrAbsent(e.EpisodeFileID))
	}
}

func TestFetchEpisodes_RequestFailure_ReturnsNotOK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-broken", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	_, ok := fetchEpisodes(context.Background(), logger, client, inst, 7)
	if ok {
		t.Error("fetchEpisodes returned ok=true, want false when the request fails")
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "seriesId=7") {
		t.Errorf("expected a warning naming the seriesId:\n%s", buf.String())
	}
}

func TestFetchEpisodes_MalformedJSON_ReturnsNotOK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-malformed", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	_, ok := fetchEpisodes(context.Background(), logger, client, inst, 7)
	if ok {
		t.Error("fetchEpisodes returned ok=true, want false for malformed JSON")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

// TestFetchEpisodes_ResponseLargerThan4MB_StillSucceeds pins F2 (Phase 6
// final review round): fetchEpisodes must route through the 512 MB
// large-body path (fetchLargeBody), not connectivity.go's fetchBody and its
// 4 MB cap — the cap every OTHER bulk Sonarr/Radarr read already avoids
// (/series here, /movie and /wanted/cutoff in radarr.go). A large real-world
// library's /episode response (the live deploy this project targets is an
// anime library with 1000+ episode series) can plausibly exceed 4 MB; before
// this fix such a series would be skipped on every cycle forever, reported
// as a transport failure rather than a size ceiling. The "padding" field
// below is not part of episodeElement, so json.Unmarshal silently ignores
// it — it inflates the body past the 4 MB cap without changing what
// actually gets decoded.
func TestFetchEpisodes_ResponseLargerThan4MB_StillSucceeds(t *testing.T) {
	padding := strings.Repeat("a", maxResponseBodyBytes+1024)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"id": 100, "seasonNumber": 1, "episodeNumber": 1, "monitored": true, "hasFile": true, "airDateUtc": "2020-01-01T00:00:00Z", "episodeFileId": 500, "padding": %q}]`, padding)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	episodes, ok := fetchEpisodes(context.Background(), logger, client, inst, 7)
	if !ok {
		t.Fatalf("fetchEpisodes returned ok=false for a response over the 4MB connectivity cap but well under the 512MB large-body limit; want true (fetchEpisodes must use fetchLargeBody):\n%s", buf.String())
	}
	if len(episodes) != 1 || episodes[0].ID == nil || *episodes[0].ID != 100 {
		t.Errorf("episodes = %+v, want one element with id=100", episodes)
	}
}

// --- fetchEpisodeFiles ---------------------------------------------------

func TestFetchEpisodeFiles_HappyPath_PassesSeriesIdAsQueryParam(t *testing.T) {
	var gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`[{"id": 500, "seasonNumber": 1, "seriesId": 7, "customFormatScore": 2000, "qualityCutoffNotMet": false}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	files, ok := fetchEpisodeFiles(context.Background(), logger, client, inst, 7)
	if !ok {
		t.Fatalf("fetchEpisodeFiles returned ok=false, want true:\n%s", buf.String())
	}
	if gotQuery != "seriesId=7" {
		t.Errorf("query = %q, want seriesId=7", gotQuery)
	}
	if len(files) != 1 || files[0].ID == nil || *files[0].ID != 500 || files[0].CustomFormatScore == nil || *files[0].CustomFormatScore != 2000 {
		t.Errorf("files = %+v, want one element {id:500 customFormatScore:2000}", files)
	}
	if files[0].QualityCutoffNotMet == nil || *files[0].QualityCutoffNotMet != false {
		t.Errorf("files[0].QualityCutoffNotMet = %v, want false", derefOrAbsent(files[0].QualityCutoffNotMet))
	}
}

func TestFetchEpisodeFiles_RequestFailure_ReturnsNotOK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-broken", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	_, ok := fetchEpisodeFiles(context.Background(), logger, client, inst, 7)
	if ok {
		t.Error("fetchEpisodeFiles returned ok=true, want false when the request fails")
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "seriesId=7") {
		t.Errorf("expected a warning naming the seriesId:\n%s", buf.String())
	}
}

func TestFetchEpisodeFiles_MalformedJSON_ReturnsNotOK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-malformed", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	_, ok := fetchEpisodeFiles(context.Background(), logger, client, inst, 7)
	if ok {
		t.Error("fetchEpisodeFiles returned ok=true, want false for malformed JSON")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

// TestFetchEpisodeFiles_ResponseLargerThan4MB_StillSucceeds is
// TestFetchEpisodes_ResponseLargerThan4MB_StillSucceeds's sibling for
// fetchEpisodeFiles — the second of the two endpoints F2 flags (sonarr.go's
// /episodefile fetch has the identical bug for the identical reason: a
// large series's episode-file list is at least as likely to cross 4 MB as
// its episode list).
func TestFetchEpisodeFiles_ResponseLargerThan4MB_StillSucceeds(t *testing.T) {
	padding := strings.Repeat("a", maxResponseBodyBytes+1024)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"id": 500, "seasonNumber": 1, "customFormatScore": 2000, "qualityCutoffNotMet": false, "padding": %q}]`, padding)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	files, ok := fetchEpisodeFiles(context.Background(), logger, client, inst, 7)
	if !ok {
		t.Fatalf("fetchEpisodeFiles returned ok=false for a response over the 4MB connectivity cap but well under the 512MB large-body limit; want true (fetchEpisodeFiles must use fetchLargeBody):\n%s", buf.String())
	}
	if len(files) != 1 || files[0].ID == nil || *files[0].ID != 500 {
		t.Errorf("files = %+v, want one element with id=500", files)
	}
}

// --- fetchSonarrWantedCutoff ----------------------------------------------

func sonarrWantedCutoffHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}
}

func TestFetchSonarrWantedCutoff_HappyPath_BuildsEpisodeIDsAndSeasonLookup(t *testing.T) {
	body := `{"page": 1, "pageSize": 100, "totalRecords": 2, "records": [
		{"id": 900, "seriesId": 1, "seasonNumber": 2, "episodeNumber": 3, "title": "Ep", "airDateUtc": "2020-01-01T00:00:00Z", "monitored": true, "hasFile": false},
		{"id": 901, "seriesId": 3, "seasonNumber": 1, "episodeNumber": 1, "title": "Ep2", "airDateUtc": "2020-01-01T00:00:00Z", "monitored": true, "hasFile": false}
	]}`
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/wanted/cutoff", sonarrWantedCutoffHandler(http.StatusOK, body))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	episodeIDs, seasons, ok := fetchSonarrWantedCutoff(context.Background(), logger, client, inst, nil)
	if !ok {
		t.Fatalf("fetchSonarrWantedCutoff returned ok=false, want true:\n%s", buf.String())
	}
	if !episodeIDs[900] || !episodeIDs[901] {
		t.Errorf("episodeIDs = %v, want 900 and 901 present", episodeIDs)
	}
	if len(episodeIDs) != 2 {
		t.Errorf("len(episodeIDs) = %d, want 2", len(episodeIDs))
	}
	if !seasons[seasonKey{seriesID: 1, seasonNumber: 2}] {
		t.Errorf("seasons missing key {seriesID:1 seasonNumber:2}: %v", seasons)
	}
	if !seasons[seasonKey{seriesID: 3, seasonNumber: 1}] {
		t.Errorf("seasons missing key {seriesID:3 seasonNumber:1}: %v", seasons)
	}
	if seasons[seasonKey{seriesID: 1, seasonNumber: 1}] {
		t.Errorf("seasons must not contain a key that was never in the wanted set")
	}
	if !strings.Contains(buf.String(), "totalRecords=2") || !strings.Contains(buf.String(), "fetched=2") {
		t.Errorf("expected the wanted/cutoff summary log line:\n%s", buf.String())
	}
}

func TestFetchSonarrWantedCutoff_RecordMissingSeriesId_SkipsInstance(t *testing.T) {
	body := `{"totalRecords": 1, "records": [{"id": 900, "seasonNumber": 2}]}` // seriesId absent
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/wanted/cutoff", sonarrWantedCutoffHandler(http.StatusOK, body))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	_, _, ok := fetchSonarrWantedCutoff(context.Background(), logger, client, inst, nil)
	if ok {
		t.Fatal("fetchSonarrWantedCutoff returned ok=true, want false: a record missing seriesId makes the whole result untrustworthy")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

func TestFetchSonarrWantedCutoff_RecordMissingSeasonNumber_SkipsInstance(t *testing.T) {
	body := `{"totalRecords": 1, "records": [{"id": 900, "seriesId": 1}]}` // seasonNumber absent
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/wanted/cutoff", sonarrWantedCutoffHandler(http.StatusOK, body))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	_, _, ok := fetchSonarrWantedCutoff(context.Background(), logger, client, inst, nil)
	if ok {
		t.Fatal("fetchSonarrWantedCutoff returned ok=true, want false: a record missing seasonNumber makes the whole result untrustworthy")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

func TestFetchSonarrWantedCutoff_RecordMissingId_SkipsInstance(t *testing.T) {
	body := `{"totalRecords": 1, "records": [{"seriesId": 1, "seasonNumber": 2}]}` // id absent
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/wanted/cutoff", sonarrWantedCutoffHandler(http.StatusOK, body))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	_, _, ok := fetchSonarrWantedCutoff(context.Background(), logger, client, inst, nil)
	if ok {
		t.Fatal("fetchSonarrWantedCutoff returned ok=true, want false: a record missing id makes the whole result untrustworthy")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

// TestFetchSonarrWantedCutoff_PagingUsesSharedMachinery is a light sanity
// check that fetchSonarrWantedCutoff really does route through
// fetchWantedCutoffPages (radarr.go): multiple pages accumulate correctly
// with the expected page/pageSize query params. The paging edge cases
// themselves (empty-page-before-total, page-cap, missing totalRecords) are
// already exhaustively covered against fetchWantedCutoffPages via Radarr's
// existing suite and are not re-proven per engine.
func TestFetchSonarrWantedCutoff_PagingUsesSharedMachinery(t *testing.T) {
	var gotQueries []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/wanted/cutoff", func(w http.ResponseWriter, r *http.Request) {
		gotQueries = append(gotQueries, r.URL.RawQuery)
		page := r.URL.Query().Get("page")
		if page == "1" {
			var recs []string
			for i := 0; i < 100; i++ {
				recs = append(recs, fmt.Sprintf(`{"id": %d, "seriesId": 1, "seasonNumber": 1}`, i))
			}
			fmt.Fprintf(w, `{"page":1,"pageSize":100,"totalRecords":101,"records":[%s]}`, strings.Join(recs, ","))
			return
		}
		w.Write([]byte(`{"page":2,"pageSize":100,"totalRecords":101,"records":[{"id":200,"seriesId":2,"seasonNumber":3}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	episodeIDs, seasons, ok := fetchSonarrWantedCutoff(context.Background(), logger, client, inst, nil)
	if !ok {
		t.Fatalf("fetchSonarrWantedCutoff returned ok=false, want true:\n%s", buf.String())
	}
	if len(gotQueries) != 2 {
		t.Fatalf("expected 2 page requests, got %d: %v", len(gotQueries), gotQueries)
	}
	if len(episodeIDs) != 101 {
		t.Errorf("len(episodeIDs) = %d, want 101", len(episodeIDs))
	}
	if !seasons[seasonKey{seriesID: 2, seasonNumber: 3}] {
		t.Errorf("expected the page-2 season to be present in the lookup")
	}
}

// --- inspectSonarrLibrary --------------------------------------------------

func TestInspectSonarrLibrary_HappyPath_ReturnsSeriesAndWantedData(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sonarrSeriesJSON))
	})
	mux.HandleFunc("/api/v3/wanted/cutoff", sonarrWantedCutoffHandler(http.StatusOK, `{"page":1,"pageSize":100,"totalRecords":0,"records":[]}`))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}

	series, episodeIDs, seasons, ok := inspectSonarrLibrary(context.Background(), logger, inst)
	if !ok {
		t.Fatalf("inspectSonarrLibrary returned ok=false, want true:\n%s", buf.String())
	}
	if len(series) != 2 {
		t.Errorf("got %d series, want 2", len(series))
	}
	if episodeIDs == nil || seasons == nil {
		t.Error("expected non-nil (possibly empty) episodeIDs/seasons maps")
	}
}

func TestInspectSonarrLibrary_SeriesFetchFailure_ReturnsNotOK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	var wantedHit bool
	mux.HandleFunc("/api/v3/wanted/cutoff", func(w http.ResponseWriter, r *http.Request) {
		wantedHit = true
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-broken", Type: "sonarr", URL: srv.URL, APIKey: "key"}

	_, _, _, ok := inspectSonarrLibrary(context.Background(), logger, inst)
	if ok {
		t.Error("inspectSonarrLibrary returned ok=true, want false when /series fails")
	}
	if wantedHit {
		t.Error("wanted/cutoff must not be fetched when /series already failed")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a skipped instance must say so (§2.6: warn and skip):\n%s", buf.String())
	}
}

func TestInspectSonarrLibrary_WantedCutoffFailure_ReturnsNotOK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sonarrSeriesJSON))
	})
	mux.HandleFunc("/api/v3/wanted/cutoff", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}

	series, episodeIDs, seasons, ok := inspectSonarrLibrary(context.Background(), logger, inst)
	if ok {
		t.Error("inspectSonarrLibrary returned ok=true, want false when /wanted/cutoff fails")
	}
	if series != nil || episodeIDs != nil || seasons != nil {
		t.Error("expected all return values nil on failure")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a skipped instance must say so (§2.6: warn and skip):\n%s", buf.String())
	}
}

// --- Phase 6: decision engine wiring into main.go's run() -------------------

// fullSonarrPipelineMux wires a mux serving every endpoint a full
// connectivity + library inspection + season decision engine pass touches:
// system/status, qualityprofile (hit twice: once by checkInstanceConnectivity,
// once by the decision engine's own fetchQualityProfiles), series,
// wanted/cutoff, tag, episode, and episodefile.
func fullSonarrPipelineMux(seriesJSON, wantedCutoffJSON, tagsJSON, episodeJSON, episodefileJSON string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/system/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"appName": "Sonarr", "version": "4.0.19.2979"}`))
	})
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id": 1, "name": "HD-1080p", "upgradeAllowed": true, "cutoff": 7, "cutoffFormatScore": 100}]`))
	})
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(seriesJSON))
	})
	mux.HandleFunc("/api/v3/wanted/cutoff", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(wantedCutoffJSON))
	})
	mux.HandleFunc("/api/v3/tag", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(tagsJSON))
	})
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(episodeJSON))
	})
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(episodefileJSON))
	})
	return mux
}

// TestRun_SonarrInstance_DecisionEngineProducesReportLines proves the full
// pipeline is wired end to end through run(): connectivity succeeds,
// inspectSonarrLibrary's returned series/wantedEpisodeIDs/wantedSeasons are
// handed to runSonarrDecisionEngine, which fetches profiles/tag/episode/
// episodefile and emits would-unmonitor/skip report lines per season.
func TestRun_SonarrInstance_DecisionEngineProducesReportLines(t *testing.T) {
	seriesJSON := `[
		{"id": 1, "title": "Would Unmonitor Show", "monitored": true, "qualityProfileId": 1, "tags": [],
		 "seasons": [{"seasonNumber": 1, "monitored": true, "statistics": {"episodeFileCount": 1, "totalEpisodeCount": 1}}]},
		{"id": 2, "title": "Incomplete Show", "monitored": true, "qualityProfileId": 1, "tags": [],
		 "seasons": [{"seasonNumber": 1, "monitored": true, "statistics": {"episodeFileCount": 0, "totalEpisodeCount": 5}}]}
	]`
	episodeJSON := `[{"id": 100, "seasonNumber": 1, "episodeNumber": 1, "monitored": true, "hasFile": true, "airDateUtc": "2015-01-01T00:00:00Z", "episodeFileId": 500}]`
	episodefileJSON := `[{"id": 500, "seasonNumber": 1, "customFormatScore": 200, "qualityCutoffNotMet": false}]`
	mux := fullSonarrPipelineMux(seriesJSON, `{"page":1,"pageSize":100,"totalRecords":0,"records":[]}`, `[]`, episodeJSON, episodefileJSON)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := `
instances:
  - name: sonarr-main
    type: sonarr
    url: ` + srv.URL + `
    api_key: key1
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "msg=would-unmonitor") || !strings.Contains(out, `series="Would Unmonitor Show"`) {
		t.Errorf("expected a would-unmonitor line for the passing season:\n%s", out)
	}
	if !strings.Contains(out, "msg=skip") || !strings.Contains(out, `series="Incomplete Show"`) || !strings.Contains(out, `reason="season incomplete on disk"`) {
		t.Errorf("expected a skip line for the incomplete season:\n%s", out)
	}
	if !strings.Contains(out, `msg="sonarr decision summary"`) {
		t.Errorf("expected the end-of-instance decision summary:\n%s", out)
	}
}

// TestRun_SonarrInstance_IncompleteWantedCutoff_DecisionEngineNeverRuns
// mirrors TestRun_RadarrInstance_IncompleteWantedCutoff_DecisionEngineNeverRuns:
// a partial wanted/cutoff result must prevent the season decision engine
// from running at all — no report lines, and the decision engine's own
// endpoints (qualityprofile's SECOND hit, tag, episode, episodefile) never
// reached.
func TestRun_SonarrInstance_IncompleteWantedCutoff_DecisionEngineNeverRuns(t *testing.T) {
	seriesJSON := `[{"id": 1, "title": "Some Show", "monitored": true, "qualityProfileId": 1, "tags": [], "seasons": [{"seasonNumber": 1, "monitored": true, "statistics": {"episodeFileCount": 1, "totalEpisodeCount": 1}}]}]`
	incompleteWantedCutoffJSON := `{"page": 1, "pageSize": 100, "totalRecords": 50, "records": []}`

	var gotTagRequests, gotEpisodeRequests, gotProfileRequests []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/system/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"appName": "Sonarr", "version": "4.0.19.2979"}`))
	})
	mux.HandleFunc("/api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		gotProfileRequests = append(gotProfileRequests, r.URL.Path)
		w.Write([]byte(`[{"id": 1, "name": "HD-1080p", "upgradeAllowed": true, "cutoff": 7, "cutoffFormatScore": 100}]`))
	})
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(seriesJSON))
	})
	mux.HandleFunc("/api/v3/wanted/cutoff", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(incompleteWantedCutoffJSON))
	})
	mux.HandleFunc("/api/v3/tag", func(w http.ResponseWriter, r *http.Request) {
		gotTagRequests = append(gotTagRequests, r.URL.Path)
		w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, r *http.Request) {
		gotEpisodeRequests = append(gotEpisodeRequests, r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := `
instances:
  - name: sonarr-main
    type: sonarr
    url: ` + srv.URL + `
    api_key: key1
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	if strings.Contains(out, "msg=would-unmonitor") || strings.Contains(out, "msg=skip") {
		t.Errorf("a partial wanted/cutoff result must prevent the season decision engine from running at all:\n%s", out)
	}
	if len(gotProfileRequests) != 1 {
		t.Errorf("expected qualityprofile hit only once (by connectivity), got %d", len(gotProfileRequests))
	}
	if len(gotTagRequests) != 0 {
		t.Errorf("expected zero /tag requests, got %d", len(gotTagRequests))
	}
	if len(gotEpisodeRequests) != 0 {
		t.Errorf("expected zero /episode requests, got %d", len(gotEpisodeRequests))
	}
}

// --- statefulSonarrFake: a whole-pipeline recording, mutable-state fake ----
//
// Built stateful from day one (binding controller notes item 6), the same
// way statefulRadarrFake (writer_test.go) was for Radarr's write pass: even
// though Phase 6 has no Sonarr write path at all, the per-episode monitored
// state modeled here — reflected consistently across /series, /episode,
// /episodefile, and /wanted/cutoff — is what Phase 7's write pass will need
// to prove a PUT actually changed something. Building that model now, and
// exercising it read-only in this phase, means Phase 7 extends this fake
// with a PUT handler rather than retrofitting the whole thing (the
// retrofit item 4's notes call out as Phase 4's hardest cycle).
//
// The catch-all handler and the writes() accessor (every non-GET request,
// never a PUT-only filter) are what make "this phase never writes anything"
// a checkable fact about requests nobody thought to make, not merely about
// the ones this fake happens to answer.

type statefulSonarrEpisode struct {
	id            int
	seriesID      int
	seasonNumber  int
	episodeNumber int
	monitored     bool
	hasFile       bool
	airDateUtc    string // RFC3339, or "" for JSON null (unaired/undated)
	episodeFileID int
	inWantedSet   bool
}

type statefulSonarrEpisodeFile struct {
	id                  int
	seasonNumber        int
	customFormatScore   int
	qualityCutoffNotMet bool
}

type statefulSonarrSeason struct {
	number            int
	monitored         bool
	episodeFileCount  int
	totalEpisodeCount int
}

type statefulSonarrSeries struct {
	id        int
	title     string
	monitored bool
	profileID int
	tags      []int
	seasons   []statefulSonarrSeason
}

type statefulSonarrFake struct {
	srv *httptest.Server
	t   *testing.T

	mu           sync.Mutex
	series       map[int]*statefulSonarrSeries
	seriesOrder  []int
	episodes     map[int]*statefulSonarrEpisode     // keyed by episode id
	episodeOrder []int                              // REVIEW FIX (F6): construction order, see episodesJSON
	files        map[int]*statefulSonarrEpisodeFile // keyed by file id
	fileOrder    []int                              // REVIEW FIX (F6): construction order, see episodeFilesJSON
	requests     []recordedRequest

	profilesJSON string
	tagsJSON     string

	// Phase 7 write-path response overrides, by series id (seriesPutStatus)
	// and globally (episodeMonitorStatus). Unset means "behave like Sonarr":
	// apply the write and echo the updated resource.
	seriesPutStatus      map[int]int
	episodeMonitorStatus int

	// onRequest, when set, is called with every request's method and path
	// BEFORE the fake answers it, from the serving goroutine and outside the
	// fake's own lock. It exists for the Phase 8 shutdown tests, which have to
	// deliver a cancellation at an exact point inside a multi-season write pass
	// — between two items, or between a season's two write calls — and no other
	// mechanism can name those instants.
	onRequest func(method, path string)
}

// newStatefulSonarrFake starts a fake Sonarr backed by series/episodes/
// files. Order is preserved for /series, /episode, and /episodefile alike
// (iterating seriesOrder/episodeOrder/fileOrder, REVIEW FIX F6), matching
// statefulRadarrFake's determinism rationale for report-line assertions:
// production code is itself order-sensitive (rule 7 stops at the first
// disqualifying episode file it sees; the rule-3 airing loop stops at the
// first not-yet-aired episode), so a fake that answered /episode or
// /episodefile in random per-call order (a bare `range` over the backing
// map) would make any future multi-item-per-season test flake
// intermittently.
func newStatefulSonarrFake(t *testing.T, series []*statefulSonarrSeries, episodes []*statefulSonarrEpisode, files []*statefulSonarrEpisodeFile) *statefulSonarrFake {
	t.Helper()
	f := &statefulSonarrFake{
		t:                    t,
		series:               make(map[int]*statefulSonarrSeries, len(series)),
		episodes:             make(map[int]*statefulSonarrEpisode, len(episodes)),
		files:                make(map[int]*statefulSonarrEpisodeFile, len(files)),
		profilesJSON:         `[{"id": 1, "name": "HD-1080p", "upgradeAllowed": true, "cutoff": 7, "cutoffFormatScore": 100}]`,
		tagsJSON:             `[]`,
		seriesPutStatus:      map[int]int{},
		episodeMonitorStatus: http.StatusOK,
	}
	for _, s := range series {
		f.series[s.id] = s
		f.seriesOrder = append(f.seriesOrder, s.id)
	}
	for _, e := range episodes {
		f.episodes[e.id] = e
		f.episodeOrder = append(f.episodeOrder, e.id)
	}
	for _, ef := range files {
		f.files[ef.id] = ef
		f.fileOrder = append(f.fileOrder, ef.id)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/system/status", f.handle(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"appName": "Sonarr", "version": "4.0.19.2979"}`))
	}))
	mux.HandleFunc("/api/v3/qualityprofile", f.handle(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(f.profilesJSON))
	}))
	mux.HandleFunc("/api/v3/tag", f.handle(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(f.tagsJSON))
	}))
	mux.HandleFunc("/api/v3/series", f.handle(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(f.seriesJSON()))
	}))
	mux.HandleFunc("/api/v3/episode", f.handle(func(w http.ResponseWriter, r *http.Request) {
		seriesID, _ := strconv.Atoi(r.URL.Query().Get("seriesId"))
		w.Write([]byte(f.episodesJSON(seriesID)))
	}))
	// Phase 7: the two write endpoints, and the per-series GET the write path
	// re-reads before either of them. Both PUTs MUTATE this fake's state, so a
	// later GET (library, detail, episode, or wanted/cutoff) really does
	// reflect what was written — which is what makes a second-run no-op test
	// a proof rather than a hope.
	mux.HandleFunc("/api/v3/episode/monitor", f.handle(f.serveEpisodeMonitor))
	mux.HandleFunc("/api/v3/series/", f.handle(f.serveSeriesDetail))
	mux.HandleFunc("/api/v3/episodefile", f.handle(func(w http.ResponseWriter, r *http.Request) {
		seriesID, _ := strconv.Atoi(r.URL.Query().Get("seriesId"))
		w.Write([]byte(f.episodeFilesJSON(seriesID)))
	}))
	mux.HandleFunc("/api/v3/wanted/cutoff", f.handle(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(f.wantedCutoffJSON(r.URL.Query().Get("monitored"))))
	}))
	// Catch-all: same rationale as radarrFake's (writer_test.go) — a request
	// to any path this fake does not stub (a future Sonarr write verb, an
	// unexpected endpoint) is recorded before the 404 a real Sonarr would
	// answer with, so "zero write requests" is a claim about requests
	// nobody thought to make, not just the ones this fake happens to serve.
	mux.HandleFunc("/", f.handle(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *statefulSonarrFake) handle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{method: r.Method, path: r.URL.Path, body: body, contentType: r.Header.Get("Content-Type")})
		hook := f.onRequest
		f.mu.Unlock()
		if hook != nil {
			hook(r.Method, r.URL.Path)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next(w, r)
	}
}

// statefulSonarrExtraField is a key this codebase has no knowledge of at all,
// carried by the per-series detail body so §2.4's "send the object back
// otherwise unmodified" is testable against something the write path could
// not possibly know to preserve deliberately.
const statefulSonarrExtraField = `"someFutureField":{"nested":["a","b"],"flag":true}`

// seriesDetailJSON renders GET /api/v3/series/{id} — the object the write
// path re-fetches, mutates one value inside, and hands straight back. It
// carries more than the library element does (a path, a large sizeOnDisk, and
// an unknown nested object) precisely so the byte-preservation mandate has
// something to preserve.
//
// Every SEASON object carries an images[].remoteUrl holding "&" and "<", the
// characters encoding/json escapes by default. §2.4's surgery re-encodes the
// seasons array through its own encoder (encodeRawArray), so escapable bytes
// at the top level of this body prove nothing about that second encoder; a
// season without them lets an escaping regression one level down pass every
// byte-preservation assertion in the suite. Real seasons carry exactly this
// (poster URLs with query strings, statistics.releaseGroups).
func (f *statefulSonarrFake) seriesDetailJSON(id int) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, found := f.series[id]
	if !found {
		return "", false
	}
	var seasons []string
	for _, season := range s.seasons {
		seasons = append(seasons, fmt.Sprintf(`{"seasonNumber":%d,"monitored":%t,"images":[{"coverType":"poster","remoteUrl":"https://img/s%d?a=1&b=2&c=<d>"}],"statistics":{"episodeFileCount":%d,"totalEpisodeCount":%d}}`,
			season.number, season.monitored, season.number, season.episodeFileCount, season.totalEpisodeCount))
	}
	tagsJSON, _ := json.Marshal(s.tags)
	return fmt.Sprintf(`{"id":%d,"title":%q,"monitored":%t,"qualityProfileId":%d,"tags":%s,"path":"/tv/%s","sizeOnDisk":9876543210123,"seasons":[%s],%s}`,
		s.id, s.title, s.monitored, s.profileID, tagsJSON, s.title, strings.Join(seasons, ","), statefulSonarrExtraField), true
}

// serveSeriesDetail is GET/PUT /api/v3/series/{id}. The PUT handler applies
// what it was SENT — including the series-level monitored flag — rather than
// only the fields this project is supposed to change: a fake that ignored the
// series-level flag would make "the write never touches series monitoring" a
// property of the fake instead of a property of the code under test.
func (f *statefulSonarrFake) serveSeriesDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/v3/series/"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		body, found := f.seriesDetailJSON(id)
		if !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(body))
	case http.MethodPut:
		if status, overridden := f.seriesPutStatus[id]; overridden && status >= 400 {
			w.WriteHeader(status)
			w.Write([]byte(`{"message":"write rejected by fake"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Monitored *bool `json:"monitored"`
			Seasons   []struct {
				SeasonNumber *int  `json:"seasonNumber"`
				Monitored    *bool `json:"monitored"`
			} `json:"seasons"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			// Never silently discard a malformed write body (the flaw
			// statefulRadarrFake's PUT handler carried): a body the server
			// cannot read is a defect in the code under test, and a fake that
			// shrugs at it hides the very bug it exists to catch.
			f.t.Errorf("statefulSonarrFake: PUT /api/v3/series/%d body is not valid JSON: %v\n%s", id, err, body)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		if s, found := f.series[id]; found {
			if payload.Monitored != nil {
				s.monitored = *payload.Monitored
			}
			for _, sent := range payload.Seasons {
				if sent.SeasonNumber == nil || sent.Monitored == nil {
					continue
				}
				for i := range s.seasons {
					if s.seasons[i].number == *sent.SeasonNumber {
						s.seasons[i].monitored = *sent.Monitored
					}
				}
			}
		}
		f.mu.Unlock()
		echo, found := f.seriesDetailJSON(id)
		if !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(echo))
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// serveEpisodeMonitor is PUT /api/v3/episode/monitor: it applies the sent
// monitored value to every named episode and echoes those episodes back, as
// Sonarr does. Because wantedCutoffJSON is DERIVED from per-episode monitored
// state, a write here visibly shrinks the wanted set on the next read.
func (f *statefulSonarrFake) serveEpisodeMonitor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if f.episodeMonitorStatus >= 400 {
		w.WriteHeader(f.episodeMonitorStatus)
		w.Write([]byte(`{"message":"episode monitor write rejected by fake"}`))
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		EpisodeIDs []int `json:"episodeIds"`
		Monitored  *bool `json:"monitored"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		f.t.Errorf("statefulSonarrFake: PUT /api/v3/episode/monitor body is not valid JSON: %v\n%s", err, body)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if req.Monitored == nil {
		f.t.Errorf("statefulSonarrFake: PUT /api/v3/episode/monitor body has no monitored field: %s", body)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	var elems []string
	for _, id := range req.EpisodeIDs {
		if e, found := f.episodes[id]; found {
			e.monitored = *req.Monitored
		}
		elems = append(elems, fmt.Sprintf(`{"id":%d,"monitored":%t}`, id, *req.Monitored))
	}
	f.mu.Unlock()
	w.Write([]byte("[" + strings.Join(elems, ",") + "]"))
}

func (f *statefulSonarrFake) seriesJSON() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var elems []string
	for _, id := range f.seriesOrder {
		s := f.series[id]
		var seasons []string
		for _, season := range s.seasons {
			seasons = append(seasons, fmt.Sprintf(`{"seasonNumber":%d,"monitored":%t,"statistics":{"episodeFileCount":%d,"totalEpisodeCount":%d}}`,
				season.number, season.monitored, season.episodeFileCount, season.totalEpisodeCount))
		}
		tagsJSON, _ := json.Marshal(s.tags)
		elems = append(elems, fmt.Sprintf(`{"id":%d,"title":%q,"monitored":%t,"qualityProfileId":%d,"tags":%s,"seasons":[%s]}`,
			s.id, s.title, s.monitored, s.profileID, tagsJSON, strings.Join(seasons, ",")))
	}
	return "[" + strings.Join(elems, ",") + "]"
}

func (f *statefulSonarrFake) episodesJSON(seriesID int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var elems []string
	for _, id := range f.episodeOrder {
		e := f.episodes[id]
		if e.seriesID != seriesID {
			continue
		}
		airDateJSON := "null"
		if e.airDateUtc != "" {
			airDateJSON = strconv.Quote(e.airDateUtc)
		}
		// seriesId is emitted because a real Sonarr always emits it, and since
		// Phase 8 the WRITE path requires it: an episode of the target season
		// that does not state its provenance cannot be named in PUT
		// /episode/monitor (see episodesOfSeason). A fake that omitted it made
		// every write-mode end-to-end test pass through a code path no real
		// instance would take.
		elems = append(elems, fmt.Sprintf(`{"id":%d,"seriesId":%d,"seasonNumber":%d,"episodeNumber":%d,"monitored":%t,"hasFile":%t,"airDateUtc":%s,"episodeFileId":%d}`,
			e.id, e.seriesID, e.seasonNumber, e.episodeNumber, e.monitored, e.hasFile, airDateJSON, e.episodeFileID))
	}
	return "[" + strings.Join(elems, ",") + "]"
}

func (f *statefulSonarrFake) episodeFilesJSON(seriesID int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var elems []string
	for _, id := range f.fileOrder {
		ef := f.files[id]
		// Only include files belonging to episodes of this series — the
		// fake models the join the same way real Sonarr's /episodefile
		// endpoint does (scoped by seriesId), via each episode's own
		// episodeFileId pointing at one of these files.
		belongs := false
		for _, e := range f.episodes {
			if e.seriesID == seriesID && e.episodeFileID == ef.id {
				belongs = true
				break
			}
		}
		if !belongs {
			continue
		}
		elems = append(elems, fmt.Sprintf(`{"id":%d,"seasonNumber":%d,"customFormatScore":%d,"qualityCutoffNotMet":%t}`,
			ef.id, ef.seasonNumber, ef.customFormatScore, ef.qualityCutoffNotMet))
	}
	return "[" + strings.Join(elems, ",") + "]"
}

// wantedCutoffJSON is DERIVED from live per-episode state (binding controller
// note): an episode appears in the wanted/cutoff set only while it is still
// MONITORED, because Sonarr's own /wanted/cutoff is filtered to monitored
// episodes of monitored series. That is what makes a write visible on the
// next read — unmonitoring a season's episodes really does shrink the wanted
// set — instead of the set being a hand-set constant that no write can ever
// affect. Iteration follows episodeOrder for the same determinism reason
// episodesJSON does.
// The monitored parameter (Phase 10) selects WHICH half of the library is asked
// about, exactly as the live endpoint does: monitored=false — the only value
// this project ever sends — answers with the UNMONITORED below-cutoff episodes
// (1213 of them on the live instance, against 2405 for the default), and
// anything else with the monitored ones. Deriving both from the same per-episode
// state is what makes a write visible in both directions on the next read.
func (f *statefulSonarrFake) wantedCutoffJSON(monitored string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	wantUnmonitored := monitored == "false"
	var recs []string
	for _, id := range f.episodeOrder {
		e := f.episodes[id]
		if e.inWantedSet && e.monitored != wantUnmonitored {
			recs = append(recs, fmt.Sprintf(`{"id":%d,"seriesId":%d,"seasonNumber":%d,"episodeNumber":%d,"title":"Ep","airDateUtc":"2015-01-01T00:00:00Z","monitored":%t,"hasFile":%t}`,
				e.id, e.seriesID, e.seasonNumber, e.episodeNumber, e.monitored, e.hasFile))
		}
	}
	return fmt.Sprintf(`{"page":1,"pageSize":100,"totalRecords":%d,"records":[%s]}`, len(recs), strings.Join(recs, ","))
}

// countRequests reports how many requests this fake received for a path,
// whatever their method or query.
func (f *statefulSonarrFake) countRequests(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.path == path {
			n++
		}
	}
	return n
}

// setEpisodeWanted moves an episode in or out of the fake's /wanted/cutoff set
// (and its episode file's qualityCutoffNotMet, which the cross-check compares
// against it) while the fake is serving. It models what a Download webhook
// actually announces — a file landed and this episode is no longer below its
// cutoff — so a daemon test can observe a write the STARTUP scan would not
// already have made. Mirrors statefulRadarrFake.setWanted.
func (f *statefulSonarrFake) setEpisodeWanted(episodeID int, wanted bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, found := f.episodes[episodeID]
	if !found {
		return
	}
	e.inWantedSet = wanted
	if file, ok := f.files[e.episodeFileID]; ok {
		file.qualityCutoffNotMet = wanted
	}
}

// all returns a copy of every request the fake received, under the mutex the
// handler goroutines append with. Reading f.requests directly from a test is a
// data race — one the suite really carried, in the allowlist test that bounds
// this project's entire Sonarr API surface.
func (f *statefulSonarrFake) all() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

// writes returns every non-GET request the fake received. Anything that is
// not a GET counts — the same "all non-GET, never a PUT-only filter" rule
// statefulRadarrFake's writes() applies.
func (f *statefulSonarrFake) writes() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedRequest
	for _, r := range f.requests {
		if r.method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

func (f *statefulSonarrFake) instance() Instance {
	return Instance{Name: "sonarr-main", Type: "sonarr", URL: f.srv.URL, APIKey: "key"}
}

// seriesMonitored, seasonMonitored and episodeMonitored read the fake's LIVE
// state, so a write test asserts on what the fake actually holds rather than
// on what the log claims. seriesMonitored is the one this phase cares most
// about: it must never change, in any run, ever.
func (f *statefulSonarrFake) seriesMonitored(id int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, found := f.series[id]
	return found && s.monitored
}

func (f *statefulSonarrFake) seasonMonitored(seriesID, seasonNumber int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, found := f.series[seriesID]
	if !found {
		return false
	}
	for _, season := range s.seasons {
		if season.number == seasonNumber {
			return season.monitored
		}
	}
	return false
}

func (f *statefulSonarrFake) episodeMonitored(id int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, found := f.episodes[id]
	return found && e.monitored
}

// TestStatefulSonarrFake_EpisodesAndEpisodeFilesJSON_OrderIsInsertionStable
// pins F6 (Phase 6 final review round): episodesJSON and episodeFilesJSON
// both used to iterate a Go map directly, so their element order was random
// per call — unlike seriesJSON, which deliberately iterates seriesOrder for
// exactly this determinism reason (see its own doc comment). Production
// code IS order-sensitive (rule 7 stops at the first episode file that is
// missing its score or below threshold, so which reason a season gets can
// depend on which file is examined first; the rule-3 airing loop similarly
// stops at the first not-yet-aired episode it finds), so the first future
// test with two files or two problem episodes in one season would flake
// intermittently against the unfixed fake. Calling each JSON builder many
// times and requiring byte-identical output is what actually catches a
// regression back to ranging the map: Go's map iteration order is
// randomized per range statement, not merely per process, so a large enough
// repeat count reliably disagrees with itself once the fix regresses.
func TestStatefulSonarrFake_EpisodesAndEpisodeFilesJSON_OrderIsInsertionStable(t *testing.T) {
	fake := newStatefulSonarrFake(t,
		[]*statefulSonarrSeries{
			{id: 1, title: "Order Show", monitored: true, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{{number: 1, monitored: true, episodeFileCount: 5, totalEpisodeCount: 5}}},
		},
		[]*statefulSonarrEpisode{
			// Deliberately not id-ascending, so "stable" cannot be confused
			// with "coincidentally sorted".
			{id: 105, seriesID: 1, seasonNumber: 1, episodeNumber: 5, monitored: true, hasFile: true, airDateUtc: pastAirDate, episodeFileID: 605},
			{id: 101, seriesID: 1, seasonNumber: 1, episodeNumber: 1, monitored: true, hasFile: true, airDateUtc: pastAirDate, episodeFileID: 601},
			{id: 104, seriesID: 1, seasonNumber: 1, episodeNumber: 4, monitored: true, hasFile: true, airDateUtc: pastAirDate, episodeFileID: 604},
			{id: 102, seriesID: 1, seasonNumber: 1, episodeNumber: 2, monitored: true, hasFile: true, airDateUtc: pastAirDate, episodeFileID: 602},
			{id: 103, seriesID: 1, seasonNumber: 1, episodeNumber: 3, monitored: true, hasFile: true, airDateUtc: pastAirDate, episodeFileID: 603},
		},
		[]*statefulSonarrEpisodeFile{
			{id: 605, seasonNumber: 1, customFormatScore: 200, qualityCutoffNotMet: false},
			{id: 601, seasonNumber: 1, customFormatScore: 200, qualityCutoffNotMet: false},
			{id: 604, seasonNumber: 1, customFormatScore: 200, qualityCutoffNotMet: false},
			{id: 602, seasonNumber: 1, customFormatScore: 200, qualityCutoffNotMet: false},
			{id: 603, seasonNumber: 1, customFormatScore: 200, qualityCutoffNotMet: false},
		},
	)

	wantEpisodes := fake.episodesJSON(1)
	wantFiles := fake.episodeFilesJSON(1)
	for i := 0; i < 50; i++ {
		if got := fake.episodesJSON(1); got != wantEpisodes {
			t.Fatalf("episodesJSON order is not stable across calls (iteration %d):\nfirst: %s\ngot:   %s", i, wantEpisodes, got)
		}
		if got := fake.episodeFilesJSON(1); got != wantFiles {
			t.Fatalf("episodeFilesJSON order is not stable across calls (iteration %d):\nfirst: %s\ngot:   %s", i, wantFiles, got)
		}
	}

	// Order must also match construction order, not merely be self-
	// consistent — otherwise a fix that stabilized on some other, equally
	// arbitrary order (e.g. always sorted by id) would pass the loop above
	// without actually matching seriesOrder's own "preserve insertion
	// order" contract.
	var gotEpisodes []episodeElement
	if err := json.Unmarshal([]byte(wantEpisodes), &gotEpisodes); err != nil {
		t.Fatalf("episodesJSON did not produce valid JSON: %v\n%s", err, wantEpisodes)
	}
	var gotEpisodeIDs []int
	for _, e := range gotEpisodes {
		gotEpisodeIDs = append(gotEpisodeIDs, *e.ID)
	}
	if want := fmt.Sprint([]int{105, 101, 104, 102, 103}); fmt.Sprint(gotEpisodeIDs) != want {
		t.Errorf("episodesJSON element order = %v, want construction order %s", gotEpisodeIDs, want)
	}

	var gotFiles []episodeFileElement
	if err := json.Unmarshal([]byte(wantFiles), &gotFiles); err != nil {
		t.Fatalf("episodeFilesJSON did not produce valid JSON: %v\n%s", err, wantFiles)
	}
	var gotFileIDs []int
	for _, ef := range gotFiles {
		gotFileIDs = append(gotFileIDs, *ef.ID)
	}
	if want := fmt.Sprint([]int{605, 601, 604, 602, 603}); fmt.Sprint(gotFileIDs) != want {
		t.Errorf("episodeFilesJSON element order = %v, want construction order %s", gotFileIDs, want)
	}
}

// TestRun_SonarrInstance_FullPipelineNeverMakesAWriteRequest is §2.1's
// guarantee for Sonarr at run() level: across a complete DRY-RUN pass that
// really does produce a would-unmonitor decision — through connectivity,
// library inspection, the season decision engine, the cross-check, and the
// write pass's own rehearsal — not one non-GET request of any kind reaches the
// fake, to any path, stubbed or not.
//
// It was the zero-write-verb pin for Phase 6, when Sonarr had no write path at
// all. Phase 7 gives it one, and the fixture now also carries a
// currently-airing season, so the same run proves the phase's other accept
// criterion in the mode a human will actually check it in first.
func TestRun_SonarrInstance_FullPipelineNeverMakesAWriteRequest(t *testing.T) {
	fake := newStatefulSonarrFake(t,
		[]*statefulSonarrSeries{
			{id: 1, title: "Would Unmonitor Show", monitored: true, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{{number: 1, monitored: true, episodeFileCount: 1, totalEpisodeCount: 1}}},
			// A currently-airing series alongside it (carried-forward test
			// debt): the phase's own accept criterion is that no airing season
			// is ever written, and until this fixture existed that criterion
			// had no run()-level test at all — every airing test stopped at
			// evaluateSeries' return value.
			{id: 2, title: "Airing Show", monitored: true, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{{number: 1, monitored: true, episodeFileCount: 2, totalEpisodeCount: 2}}},
		},
		[]*statefulSonarrEpisode{
			{id: 100, seriesID: 1, seasonNumber: 1, episodeNumber: 1, monitored: true, hasFile: true, airDateUtc: "2015-01-01T00:00:00Z", episodeFileID: 500, inWantedSet: false},
			{id: 200, seriesID: 2, seasonNumber: 1, episodeNumber: 1, monitored: true, hasFile: true, airDateUtc: pastAirDate, episodeFileID: 600, inWantedSet: false},
			{id: 201, seriesID: 2, seasonNumber: 1, episodeNumber: 2, monitored: true, hasFile: true, airDateUtc: futureAirDate, episodeFileID: 601, inWantedSet: false},
		},
		[]*statefulSonarrEpisodeFile{
			{id: 500, seasonNumber: 1, customFormatScore: 200, qualityCutoffNotMet: false},
			{id: 600, seasonNumber: 1, customFormatScore: 200, qualityCutoffNotMet: false},
			{id: 601, seasonNumber: 1, customFormatScore: 200, qualityCutoffNotMet: false},
		},
	)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := `
instances:
  - name: sonarr-main
    type: sonarr
    url: ` + fake.srv.URL + `
    api_key: key1
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "msg=would-unmonitor") {
		t.Fatalf("test setup did not actually produce a would-unmonitor decision:\n%s", out)
	}
	if !strings.Contains(out, `reason="unaired or undated episodes"`) {
		t.Fatalf("test setup did not actually produce an airing skip:\n%s", out)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("a dry-run sonarr run must make ZERO write requests, got %+v", writes)
	}
	// The rehearsal is real: dry-run runs the write pass all the way to each
	// gate, so the write path's own fresh read happens and its withholding is
	// what the summary counts.
	if !strings.Contains(out, "withheldWrites=1") {
		t.Errorf("expected the withheld write to be counted:\n%s", out)
	}
}

// TestRun_SonarrInstance_CrossCheckAgreement_SummaryStatesPassed exercises
// the cross-check's honest-agreement path through the full run() pipeline
// against the stateful fake, proving wanted-set membership and
// episodeFile.qualityCutoffNotMet genuinely agreeing (both false, for a
// season that becomes a would-unmonitor candidate) renders "passed" with a
// nonzero verified count — not a default or a guess.
func TestRun_SonarrInstance_CrossCheckAgreement_SummaryStatesPassed(t *testing.T) {
	fake := newStatefulSonarrFake(t,
		[]*statefulSonarrSeries{
			{id: 1, title: "Agreeing Show", monitored: true, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{{number: 1, monitored: true, episodeFileCount: 1, totalEpisodeCount: 1}}},
		},
		[]*statefulSonarrEpisode{
			{id: 100, seriesID: 1, seasonNumber: 1, episodeNumber: 1, monitored: true, hasFile: true, airDateUtc: "2015-01-01T00:00:00Z", episodeFileID: 500, inWantedSet: false},
		},
		[]*statefulSonarrEpisodeFile{
			{id: 500, seasonNumber: 1, customFormatScore: 200, qualityCutoffNotMet: false},
		},
	)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := `
instances:
  - name: sonarr-main
    type: sonarr
    url: ` + fake.srv.URL + `
    api_key: key1
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "crossCheck=\"passed (1 verified, 0 unverifiable)\"") {
		t.Errorf("expected crossCheck=\"passed (1 verified, 0 unverifiable)\" in the summary:\n%s", out)
	}
}

// TestRun_SonarrInstance_WantedSeasonLookup_PerSeasonNotPerSeries is the
// IMPORTANT review fix's discriminating coverage for rule 4: one series
// (id 7) with TWO monitored seasons, only one of which (season 2) has an
// episode in the wanted set. Driven end to end through run() so the
// producer (statefulSonarrFake's own inWantedSet field, via
// wantedCutoffJSON) and the consumer (fetchSonarrWantedCutoff's
// (seriesId, seasonNumber) lookup, via evaluateSeries' rule 4) are
// exercised together, in the same pass, for the first time: every prior
// test either used an empty wantedSeasons map or the single fixed key
// {seriesID: 1, seasonNumber: 1}, so the seriesId/seasonNumber fields of
// the key were indistinguishable and a swapped or per-SERIES-instead-of-
// per-SEASON lookup would have passed the whole suite undetected.
func TestRun_SonarrInstance_WantedSeasonLookup_PerSeasonNotPerSeries(t *testing.T) {
	fake := newStatefulSonarrFake(t,
		[]*statefulSonarrSeries{
			{id: 7, title: "Mixed Wanted Show", monitored: true, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{
					{number: 2, monitored: true, episodeFileCount: 1, totalEpisodeCount: 1},
					{number: 3, monitored: true, episodeFileCount: 1, totalEpisodeCount: 1},
				}},
		},
		[]*statefulSonarrEpisode{
			// Season 2's only episode IS in the wanted set: rule 4 must skip season 2.
			{id: 200, seriesID: 7, seasonNumber: 2, episodeNumber: 1, monitored: true, hasFile: true, airDateUtc: "2015-01-01T00:00:00Z", episodeFileID: 700, inWantedSet: true},
			// Season 3's only episode is NOT in the wanted set: rule 4 must let season 3 through.
			{id: 300, seriesID: 7, seasonNumber: 3, episodeNumber: 1, monitored: true, hasFile: true, airDateUtc: "2015-01-01T00:00:00Z", episodeFileID: 800, inWantedSet: false},
		},
		[]*statefulSonarrEpisodeFile{
			{id: 700, seasonNumber: 2, customFormatScore: 200, qualityCutoffNotMet: true},
			{id: 800, seasonNumber: 3, customFormatScore: 200, qualityCutoffNotMet: false},
		},
	)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := `
instances:
  - name: sonarr-main
    type: sonarr
    url: ` + fake.srv.URL + `
    api_key: key1
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()

	// IMPORTANT REVIEW FIX: track whether the season-2 and season-3 report
	// lines were actually found, and Fatalf if either was not — without
	// this, a report line silently missing its "season" attr (e.g. an
	// accidental deletion of "season", d.season from the logger.Info calls)
	// would make BOTH `if strings.Contains(line, "season=N")` guards below
	// never fire, so the whole per-season branch is skipped and the test
	// passes vacuously instead of catching the regression.
	season2LineFound := false
	season3LineFound := false
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "msg=skip") && !strings.Contains(line, "msg=would-unmonitor") {
			continue // report lines only — the cross-check's own "cross-check" lines also carry seriesId/season attrs.
		}
		if strings.Contains(line, "seriesId=7") && strings.Contains(line, "season=2") {
			season2LineFound = true
			if !strings.Contains(line, "msg=skip") || !strings.Contains(line, `reason="quality cutoff not met"`) {
				t.Errorf("season 2 (in the wanted set) line = %q, want msg=skip reason=\"quality cutoff not met\"", line)
			}
		}
		if strings.Contains(line, "seriesId=7") && strings.Contains(line, "season=3") {
			season3LineFound = true
			if !strings.Contains(line, "msg=would-unmonitor") || !strings.Contains(line, `reason="cutoff met"`) {
				t.Errorf("season 3 (NOT in the wanted set) line = %q, want msg=would-unmonitor reason=\"cutoff met\"", line)
			}
		}
	}
	if !season2LineFound {
		t.Fatalf("expected a report line for seriesId=7 season=2 (msg=skip):\n%s", out)
	}
	if !season3LineFound {
		t.Fatalf("expected a report line for seriesId=7 season=3 (msg=would-unmonitor):\n%s", out)
	}
	if !strings.Contains(out, `reason="quality cutoff not met"`) {
		t.Errorf("expected season 2's skip line:\n%s", out)
	}
	if !strings.Contains(out, "msg=would-unmonitor") {
		t.Errorf("expected season 3's would-unmonitor line:\n%s", out)
	}
}

// TestRun_SonarrInstance_AiringSeason_ReportLineCarriesMandatedReasonString_NeverWouldUnmonitor
// is the IMPORTANT review fix: every prior airing-guard test (evaluateSeries'
// TestEvaluateSeries_AiringSeason_NeverWouldUnmonitor_EvenWhenCompleteAndCutoffMet
// and its siblings) compared d.reason against the ReasonSeasonNotFullyAired
// CONSTANT, never the brief-mandated LITERAL string "unaired or undated
// episodes" — so editing the constant's value, or having the report line
// emit the airing skip under a different reason/msg, would keep the whole
// suite green while breaking the one greppable output the brief's accept
// criterion actually asks a human to spot-check ("no currently-airing season
// appears as would-unmonitor"). Every prior airing test also stopped at
// evaluateSeries' return value; none drove a future-dated episode through
// runSonarrDecisionEngine/run() far enough to see the REPORT LINE itself.
// This test closes both gaps at once: driven end to end through run()
// against statefulSonarrFake (which models airDateUtc), asserting the
// isolated report line (via reportLineWithMsg, sonarr_decision_test.go)
// contains the literal reason string, and that no would-unmonitor line ever
// appears for this season.
func TestRun_SonarrInstance_AiringSeason_ReportLineCarriesMandatedReasonString_NeverWouldUnmonitor(t *testing.T) {
	fake := newStatefulSonarrFake(t,
		[]*statefulSonarrSeries{
			{id: 1, title: "Airing Show", monitored: true, profileID: 1, tags: []int{},
				seasons: []statefulSonarrSeason{{number: 1, monitored: true, episodeFileCount: 2, totalEpisodeCount: 2}}},
		},
		[]*statefulSonarrEpisode{
			// Otherwise-passing: complete on disk (2/2), CF score comfortably
			// above threshold, not in the wanted set. Only episode 101's
			// future airDateUtc should stop this season from would-unmonitor.
			{id: 100, seriesID: 1, seasonNumber: 1, episodeNumber: 1, monitored: true, hasFile: true, airDateUtc: pastAirDate, episodeFileID: 500, inWantedSet: false},
			{id: 101, seriesID: 1, seasonNumber: 1, episodeNumber: 2, monitored: true, hasFile: true, airDateUtc: futureAirDate, episodeFileID: 501, inWantedSet: false},
		},
		[]*statefulSonarrEpisodeFile{
			{id: 500, seasonNumber: 1, customFormatScore: 200, qualityCutoffNotMet: false},
			{id: 501, seasonNumber: 1, customFormatScore: 200, qualityCutoffNotMet: false},
		},
	)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := `
instances:
  - name: sonarr-main
    type: sonarr
    url: ` + fake.srv.URL + `
    api_key: key1
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", path, "--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()

	skipLine := reportLineWithMsg(t, out, "skip")
	if !strings.Contains(skipLine, `reason="unaired or undated episodes"`) {
		t.Errorf("airing skip report line = %q, want the LITERAL brief-mandated reason string %q — not merely whatever ReasonSeasonNotFullyAired happens to equal today", skipLine, "unaired or undated episodes")
	}
	if strings.Contains(out, "msg=would-unmonitor") {
		t.Errorf("a currently-airing season must NEVER appear as would-unmonitor, per the phase's accept criterion:\n%s", out)
	}
}

// ============================================================================
// Phase 7: Phase 6 branch-review carry-over minors (binding)
// ============================================================================

// TestFetchSeriesLibrary_TagsKeyDifferentlyCased_NullElementStillCaught pins
// the case-sensitivity hole the null-tag-element fix left open: the struct
// decode that populates Tags matches JSON keys case-INsensitively (that is
// encoding/json's documented behavior), while rawObjectField's re-check did
// an exact, case-sensitive map lookup. A payload spelling the key "Tags"
// therefore decoded [3, null, 9] into [3, 0, 9], rawObjectField reported
// found=false, the re-check was skipped entirely, and the corrupted array —
// the one shape this whole fix exists to catch — was trusted.
func TestFetchSeriesLibrary_TagsKeyDifferentlyCased_NullElementStillCaught(t *testing.T) {
	seriesJSON := `[{"id": 1, "title": "Oddly Cased Tags Show", "monitored": true, "qualityProfileId": 1, "Tags": [3, null, 9], "seasons": []}]`
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(seriesJSON))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	series, ok := fetchSeriesLibrary(context.Background(), logger, client, inst)
	if !ok {
		t.Fatalf("fetchSeriesLibrary returned ok=false, want true:\n%s", buf.String())
	}
	if len(series) != 1 {
		t.Fatalf("expected 1 series, got %d", len(series))
	}
	if series[0].Tags != nil {
		t.Errorf("a corrupted tags array must be normalized to nil regardless of how the JSON key is cased, got %v", *series[0].Tags)
	}
}

// TestFetchEpisodes_ForeignSeriesIdRecord_WarnedAndExcluded pins the
// seriesId echo-check: episodeElement did not decode seriesId at all, so the
// code trusted the server to have honoured ?seriesId=X. A proxy or routing
// mistake would feed rules 3 and 7 another series' episodes with only the
// incidental count comparisons as backstop.
func TestFetchEpisodes_ForeignSeriesIdRecord_WarnedAndExcluded(t *testing.T) {
	episodesJSON := `[
		{"id": 100, "seriesId": 1, "seasonNumber": 1, "episodeNumber": 1, "monitored": true, "hasFile": true, "airDateUtc": "2015-01-01T00:00:00Z", "episodeFileId": 500},
		{"id": 999, "seriesId": 2, "seasonNumber": 1, "episodeNumber": 2, "monitored": true, "hasFile": true, "airDateUtc": "2015-01-01T00:00:00Z", "episodeFileId": 999}
	]`
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(episodesJSON))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	episodes, ok := fetchEpisodes(context.Background(), logger, client, inst, 1)
	if !ok {
		t.Fatalf("fetchEpisodes returned ok=false, want true:\n%s", buf.String())
	}
	if len(episodes) != 1 || episodes[0].ID == nil || *episodes[0].ID != 100 {
		t.Fatalf("expected only the requested series' episode to survive, got %d: %+v", len(episodes), episodes)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "episodeId=999") {
		t.Errorf("expected a WARN naming the foreign episode record:\n%s", out)
	}
}

// TestFetchEpisodeFiles_ForeignSeriesIdRecord_WarnedAndExcluded is the same
// guard on the endpoint that feeds rule 7's custom-format comparison.
func TestFetchEpisodeFiles_ForeignSeriesIdRecord_WarnedAndExcluded(t *testing.T) {
	filesJSON := `[
		{"id": 500, "seriesId": 1, "seasonNumber": 1, "customFormatScore": 200, "qualityCutoffNotMet": false},
		{"id": 999, "seriesId": 2, "seasonNumber": 1, "customFormatScore": 0, "qualityCutoffNotMet": true}
	]`
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(filesJSON))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	files, ok := fetchEpisodeFiles(context.Background(), logger, client, inst, 1)
	if !ok {
		t.Fatalf("fetchEpisodeFiles returned ok=false, want true:\n%s", buf.String())
	}
	if len(files) != 1 || files[0].ID == nil || *files[0].ID != 500 {
		t.Fatalf("expected only the requested series' file to survive, got %d: %+v", len(files), files)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "fileId=999") {
		t.Errorf("expected a WARN naming the foreign episode file record:\n%s", out)
	}
}

// TestFetchSeriesLibrary_TagsKeyPresentTwiceInDifferentCases_TreatedAsUnverifiable
// closes the last hole in the tags re-check. encoding/json decodes an object
// key by key, so when BOTH "tags" and "Tags" are present the LAST one wins for
// the struct field — while a re-check that stopped at the first match would
// validate the clean array and pass the corrupted one straight through, which
// is the very outcome the re-check exists to prevent. Two keys that could each
// have populated the field means the field's provenance is ambiguous, and
// ambiguous provenance on the exclusion tag's own input is untrusted input.
func TestFetchSeriesLibrary_TagsKeyPresentTwiceInDifferentCases_TreatedAsUnverifiable(t *testing.T) {
	seriesJSON := `[{"id": 1, "title": "Two Tags Keys Show", "monitored": true, "qualityProfileId": 1, "tags": [1, 2], "Tags": [3, null], "seasons": []}]`
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(seriesJSON))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	series, ok := fetchSeriesLibrary(context.Background(), logger, client, inst)
	if !ok {
		t.Fatalf("fetchSeriesLibrary returned ok=false, want true:\n%s", buf.String())
	}
	if len(series) != 1 {
		t.Fatalf("expected 1 series, got %d", len(series))
	}
	if series[0].Tags != nil {
		t.Errorf("two keys could each have populated tags; the field's provenance is ambiguous and must be treated as unverifiable, got %v", *series[0].Tags)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning:\n%s", buf.String())
	}
}

// TestFetchSonarrWantedCutoff_UnmonitoredFilter_SendsMonitoredFalseAndBuildsBothLookups
// is the Sonarr half of the reverse scan's quality-cutoff signal. The record
// shape is unchanged — id, seriesId, seasonNumber, verified live against the
// monitored=false set itself (1213 records, every one monitored=false) — so the
// reverse scan reuses this fetch whole rather than growing a second decoder for
// the same records.
func TestFetchSonarrWantedCutoff_UnmonitoredFilter_SendsMonitoredFalseAndBuildsBothLookups(t *testing.T) {
	var gotQueries []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/wanted/cutoff", func(w http.ResponseWriter, r *http.Request) {
		gotQueries = append(gotQueries, r.URL.RawQuery)
		w.Write([]byte(`{"page":1,"pageSize":100,"totalRecords":2,"records":[
			{"id": 57093, "seriesId": 416, "seasonNumber": 2},
			{"id": 17131, "seriesId": 226, "seasonNumber": 5}
		]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger, buf := newDecisionTestLogger(slog.LevelInfo)
	inst := Instance{Name: "sonarr-main", Type: "sonarr", URL: srv.URL, APIKey: "key"}
	client := NewAPIClient(inst.URL, inst.APIKey)

	episodeIDs, seasons, ok := fetchSonarrWantedCutoff(context.Background(), logger, client, inst, unmonitoredWantedFilter())
	if !ok {
		t.Fatalf("fetchSonarrWantedCutoff returned ok=false, want true:\n%s", buf.String())
	}
	if !episodeIDs[57093] || !episodeIDs[17131] || len(episodeIDs) != 2 {
		t.Errorf("episode id set = %v, want exactly the two records' ids", episodeIDs)
	}
	if !seasons[seasonKey{seriesID: 416, seasonNumber: 2}] || !seasons[seasonKey{seriesID: 226, seasonNumber: 5}] {
		t.Errorf("season lookup = %v, want both (seriesId, seasonNumber) pairs", seasons)
	}
	if len(gotQueries) != 1 || !strings.Contains(gotQueries[0], "monitored=false") {
		t.Errorf("query = %v, want the monitored=false filter", gotQueries)
	}
}
