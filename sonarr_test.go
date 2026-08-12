package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	if len(s.Seasons) != 2 {
		t.Fatalf("series[0] has %d seasons, want 2", len(s.Seasons))
	}
	season1 := s.Seasons[0]
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
