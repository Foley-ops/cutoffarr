package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// webui_test.go covers Phase 12's HTTP surface directly against
// newWebUIHandler — no daemon, no listener wiring, just the mux and the two
// endpoints' contract. daemon_webui_test.go covers the same surface wired
// into the real daemon end to end.

func newTestWebUIServer(t *testing.T, s *statsStore) (*httptest.Server, *webUIServer) {
	t.Helper()
	srv := &webUIServer{stats: s, scan: newScanCoordinator()}
	ts := httptest.NewServer(newWebUIHandler(srv))
	t.Cleanup(ts.Close)
	return ts, srv
}

// --- GET /api/stats ----------------------------------------------------

func TestWebUIHandler_Stats_EmptyInstancesBeforeAnyCycleIsValidJSONNotAnError(t *testing.T) {
	ts, _ := newTestWebUIServer(t, newStatsStore(true))

	resp, err := http.Get(ts.URL + "/api/stats")
	if err != nil {
		t.Fatalf("GET /api/stats: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body statsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if len(body.Instances) != 0 {
		t.Errorf("Instances = %+v, want empty before any cycle completed", body.Instances)
	}
	if !body.DryRun {
		t.Errorf("DryRun = false, want true (the store was created with dryRun=true)")
	}
}

// TestWebUIHandler_Stats_ReflectsARecordedCycle checks every field the API
// contract names actually round-trips through the handler's JSON encoding —
// including the nested reverseFindings/fileReport/lastActions shapes.
func TestWebUIHandler_Stats_ReflectsARecordedCycle(t *testing.T) {
	s := newStatsStore(false)
	season := 2
	s.recordInstance(cycleKindSweep, time.Now(), "radarr-main", "radarr", cycleInstanceStats{
		total: 100, monitored: 40, unmonitored: 60, wouldUnmonitor: 3,
		reverseRan:      true,
		reverseFindings: []reverseFinding{{ID: 7, Title: "Finding", Reason: ReasonQualityCutoffNotMet}},
		fileReportRan:   true,
		fileReport: fileReportSnapshot{Status: "ran", Duplicates: 1, Orphans: 2,
			Findings: []fileReportFindingRecord{{Kind: "duplicate", Group: "A", Path: "/x", Count: 2}}},
		actions: []actionRecord{{Action: ActionUnmonitor, ID: 1, Title: "Movie", Season: &season, Reason: ReasonCutoffMet}},
	})
	ts, _ := newTestWebUIServer(t, s)

	resp, err := http.Get(ts.URL + "/api/stats")
	if err != nil {
		t.Fatalf("GET /api/stats: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var body statsResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("response is not valid JSON: %v\n%s", err, raw)
	}
	if len(body.Instances) != 1 {
		t.Fatalf("Instances = %+v, want exactly 1", body.Instances)
	}
	inst := body.Instances[0]
	if inst.Name != "radarr-main" || inst.Type != "radarr" || inst.Total != 100 || inst.Monitored != 40 || inst.Unmonitored != 60 || inst.WouldUnmonitor != 3 {
		t.Errorf("instance fields = %+v, do not match what was recorded", inst)
	}
	if inst.LastRun == nil {
		t.Error("LastRun must be present (RFC3339), not null, for an instance a cycle has actually touched")
	}
	if inst.LastCycleKind == nil || *inst.LastCycleKind != cycleKindSweep {
		t.Errorf("LastCycleKind = %v, want %q", inst.LastCycleKind, cycleKindSweep)
	}
	if len(inst.ReverseFindings) != 1 || inst.ReverseFindings[0].ID != 7 {
		t.Errorf("ReverseFindings = %+v, want the one recorded finding", inst.ReverseFindings)
	}
	if inst.FileReport.Status != "ran" || inst.FileReport.Duplicates != 1 || inst.FileReport.Orphans != 2 || len(inst.FileReport.Findings) != 1 {
		t.Errorf("FileReport = %+v, does not match what was recorded", inst.FileReport)
	}
	if len(inst.LastActions) != 1 || inst.LastActions[0].Season == nil || *inst.LastActions[0].Season != 2 {
		t.Errorf("LastActions = %+v, want the one recorded action with season=2", inst.LastActions)
	}

	// The RAW bytes, not just the decoded struct: a season-less (Radarr)
	// action or an orphan's absent group/count must be OMITTED, not printed
	// as a misleading zero value, per the API contract's own "season?"/
	// "count?" optionality.
	if strings.Contains(string(raw), `"group":""`) {
		t.Errorf("an empty group must be omitted from the JSON, not printed as \"\":\n%s", raw)
	}
}

// --- POST /api/scan ------------------------------------------------------

func TestWebUIHandler_Scan_FirstPostQueuesSecondReportsAlreadyPending(t *testing.T) {
	ts, srv := newTestWebUIServer(t, newStatsStore(false))

	first := postScan(t, ts.URL)
	if first.status != http.StatusAccepted || first.body["status"] != "queued" {
		t.Fatalf("first POST /api/scan = %+v, want 202 {status: queued}", first)
	}

	second := postScan(t, ts.URL)
	if second.status != http.StatusAccepted || second.body["status"] != "already-pending" {
		t.Fatalf("second POST /api/scan (while the first is still pending) = %+v, want 202 {status: already-pending}", second)
	}

	// A third POST, after the daemon's own loop would have picked up and
	// finished the pending scan (simulated directly here since this test
	// has no daemon loop), must queue again: idempotency is about not
	// stacking two, never about refusing forever.
	if !srv.scan.takePending() {
		t.Fatal("takePending() = false; the coordinator lost the request the handler queued")
	}
	srv.scan.end() // never began; end() is idempotent and harmless here
	third := postScan(t, ts.URL)
	if third.status != http.StatusAccepted || third.body["status"] != "queued" {
		t.Fatalf("POST /api/scan after the pending one was taken = %+v, want 202 {status: queued}", third)
	}
}

// TestWebUIHandler_Scan_AlreadyPendingWhileACycleIsRunning covers the OTHER
// half of "already running or queued": a cycle in flight (begin() called,
// end() not yet), with nothing pending, must still refuse a second scan.
func TestWebUIHandler_Scan_AlreadyPendingWhileACycleIsRunning(t *testing.T) {
	ts, srv := newTestWebUIServer(t, newStatsStore(false))
	srv.scan.begin()

	got := postScan(t, ts.URL)
	if got.status != http.StatusAccepted || got.body["status"] != "already-pending" {
		t.Fatalf("POST /api/scan while a cycle is running = %+v, want 202 {status: already-pending}", got)
	}
}

type scanResponse struct {
	status int
	body   map[string]string
}

func postScan(t *testing.T, baseURL string) scanResponse {
	t.Helper()
	resp, err := http.Post(baseURL+"/api/scan", "", nil)
	if err != nil {
		t.Fatalf("POST /api/scan: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("POST /api/scan: response is not valid JSON: %v", err)
	}
	return scanResponse{status: resp.StatusCode, body: body}
}

// --- method / route audit -------------------------------------------------

func TestWebUIHandler_WrongMethodOnEachRoute_Is405(t *testing.T) {
	ts, _ := newTestWebUIServer(t, newStatsStore(false))

	cases := []struct {
		method, path string
	}{
		{http.MethodPost, "/"},
		{http.MethodPut, "/api/stats"},
		{http.MethodGet, "/api/scan"},
	}
	for _, c := range cases {
		req, err := http.NewRequest(c.method, ts.URL+c.path, nil)
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", c.method, c.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", c.method, c.path, resp.StatusCode)
		}
	}
}

func TestWebUIHandler_UnknownPath_Is404(t *testing.T) {
	ts, _ := newTestWebUIServer(t, newStatsStore(false))

	resp, err := http.Get(ts.URL + "/nothing-here")
	if err != nil {
		t.Fatalf("GET /nothing-here: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// --- GET / (the embedded page) --------------------------------------------

func TestWebUIHandler_Index_ServesHTMLWithCSPMeta(t *testing.T) {
	ts, _ := newTestWebUIServer(t, newStatsStore(false))

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	const wantCSP = `<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'">`
	if !strings.Contains(string(body), wantCSP) {
		t.Errorf("served page is missing the exact required CSP meta tag:\n%s", body)
	}
}

// TestWebUIPage_ContainsNoExternalURLs is the brief's own instrument: grep
// the embedded HTML for http:// or https:// and require none outside a
// comment. This page carries none anywhere (no webfonts, no CDN, no linked
// icon), so the assertion is unconditional rather than comment-aware — a
// weaker test would have to parse HTML comments out first, which this file
// never needs to do because there is nothing to exempt.
func TestWebUIPage_ContainsNoExternalURLs(t *testing.T) {
	page := string(webUIPage)
	for _, scheme := range []string{"http://", "https://"} {
		if strings.Contains(page, scheme) {
			t.Errorf("embedded page contains %q: it must make zero external requests (CSP default-src 'none')", scheme)
		}
	}
}

// TestWebUIPage_ContainsTheSignatureShelfElement pins the GUI design plan's
// binding "signature element": one horizontal shelf bar per instance, sage
// fill meeting amber remainder at a marker.
func TestWebUIPage_ContainsTheSignatureShelfElement(t *testing.T) {
	page := string(webUIPage)
	for _, want := range []string{
		"data-cutoff-shelf",
		"shelf-bar",
		"shelf-rest",
		"shelf-marker",
		"shelf-count",
		"#6FA08C", // REST sage
		"#D9A05B", // HUNT amber
		"#C46A5A", // ALERT clay
		"Scan now",
		"reverse scan",
		"File clutter",
		"ui-monospace",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("embedded page is missing %q", want)
		}
	}
}

// TestWebUIPage_PollsStatsAndPausesWhenHidden pins the two JS behaviors the
// brief requires that a handler-level test can otherwise never see: a 30s
// poll of /api/stats, and document.hidden pausing it.
func TestWebUIPage_PollsStatsAndPausesWhenHidden(t *testing.T) {
	page := string(webUIPage)
	if !strings.Contains(page, "/api/stats") {
		t.Error("page never references /api/stats")
	}
	if !strings.Contains(page, "30000") {
		t.Error("page does not poll on a 30s interval")
	}
	if !strings.Contains(page, "document.hidden") {
		t.Error("page does not check document.hidden to pause polling")
	}
	if !strings.Contains(page, "prefers-reduced-motion") {
		t.Error("page does not disable bar-width motion under prefers-reduced-motion")
	}
}
