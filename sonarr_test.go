package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

	episodeIDs, seasons, ok := fetchSonarrWantedCutoff(context.Background(), logger, client, inst)
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

	_, _, ok := fetchSonarrWantedCutoff(context.Background(), logger, client, inst)
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

	_, _, ok := fetchSonarrWantedCutoff(context.Background(), logger, client, inst)
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

	_, _, ok := fetchSonarrWantedCutoff(context.Background(), logger, client, inst)
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

	episodeIDs, seasons, ok := fetchSonarrWantedCutoff(context.Background(), logger, client, inst)
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
	_ = buf
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
	_ = buf
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

	mu          sync.Mutex
	series      map[int]*statefulSonarrSeries
	seriesOrder []int
	episodes    map[int]*statefulSonarrEpisode     // keyed by episode id
	files       map[int]*statefulSonarrEpisodeFile // keyed by file id
	requests    []recordedRequest

	profilesJSON string
	tagsJSON     string
}

// newStatefulSonarrFake starts a fake Sonarr backed by series/episodes/
// files. Order is preserved for /series (iterating seriesOrder), matching
// statefulRadarrFake's determinism rationale for report-line assertions.
func newStatefulSonarrFake(t *testing.T, series []*statefulSonarrSeries, episodes []*statefulSonarrEpisode, files []*statefulSonarrEpisodeFile) *statefulSonarrFake {
	t.Helper()
	f := &statefulSonarrFake{
		series:       make(map[int]*statefulSonarrSeries, len(series)),
		episodes:     make(map[int]*statefulSonarrEpisode, len(episodes)),
		files:        make(map[int]*statefulSonarrEpisodeFile, len(files)),
		profilesJSON: `[{"id": 1, "name": "HD-1080p", "upgradeAllowed": true, "cutoff": 7, "cutoffFormatScore": 100}]`,
		tagsJSON:     `[]`,
	}
	for _, s := range series {
		f.series[s.id] = s
		f.seriesOrder = append(f.seriesOrder, s.id)
	}
	for _, e := range episodes {
		f.episodes[e.id] = e
	}
	for _, ef := range files {
		f.files[ef.id] = ef
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
	mux.HandleFunc("/api/v3/episodefile", f.handle(func(w http.ResponseWriter, r *http.Request) {
		seriesID, _ := strconv.Atoi(r.URL.Query().Get("seriesId"))
		w.Write([]byte(f.episodeFilesJSON(seriesID)))
	}))
	mux.HandleFunc("/api/v3/wanted/cutoff", f.handle(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(f.wantedCutoffJSON()))
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
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{method: r.Method, path: r.URL.Path})
		f.mu.Unlock()
		next(w, r)
	}
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
	for _, e := range f.episodes {
		if e.seriesID != seriesID {
			continue
		}
		airDateJSON := "null"
		if e.airDateUtc != "" {
			airDateJSON = strconv.Quote(e.airDateUtc)
		}
		elems = append(elems, fmt.Sprintf(`{"id":%d,"seasonNumber":%d,"episodeNumber":%d,"monitored":%t,"hasFile":%t,"airDateUtc":%s,"episodeFileId":%d}`,
			e.id, e.seasonNumber, e.episodeNumber, e.monitored, e.hasFile, airDateJSON, e.episodeFileID))
	}
	return "[" + strings.Join(elems, ",") + "]"
}

func (f *statefulSonarrFake) episodeFilesJSON(seriesID int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var elems []string
	for _, ef := range f.files {
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

func (f *statefulSonarrFake) wantedCutoffJSON() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var recs []string
	for _, e := range f.episodes {
		if e.inWantedSet {
			recs = append(recs, fmt.Sprintf(`{"id":%d,"seriesId":%d,"seasonNumber":%d,"episodeNumber":%d,"title":"Ep","airDateUtc":"2015-01-01T00:00:00Z","monitored":true,"hasFile":%t}`,
				e.id, e.seriesID, e.seasonNumber, e.episodeNumber, e.hasFile))
		}
	}
	return fmt.Sprintf(`{"page":1,"pageSize":100,"totalRecords":%d,"records":[%s]}`, len(recs), strings.Join(recs, ","))
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

// TestRun_SonarrInstance_FullPipelineNeverMakesAWriteRequest is the run()-
// level zero-write-verb pin (item 1 of the carried-forward controller
// notes, "pin it the way Phases 4-5 pinned 'exactly one'" — here, zero):
// across a complete run that really does produce would-unmonitor decisions
// through the full connectivity + library inspection + season decision
// engine pipeline, not one non-GET request of any kind reaches the fake, to
// any path, stubbed or not.
func TestRun_SonarrInstance_FullPipelineNeverMakesAWriteRequest(t *testing.T) {
	fake := newStatefulSonarrFake(t,
		[]*statefulSonarrSeries{
			{id: 1, title: "Would Unmonitor Show", monitored: true, profileID: 1, tags: []int{},
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
	if !strings.Contains(stdout.String(), "msg=would-unmonitor") {
		t.Fatalf("test setup did not actually produce a would-unmonitor decision:\n%s", stdout.String())
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Errorf("a sonarr run must make ZERO write requests in this phase, got %+v", writes)
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
