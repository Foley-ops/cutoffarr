package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
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
		total: 100, monitored: 40, unmonitored: 60, wouldUnmonitor: 3, decisionsRan: true,
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

// TestWebUIHandler_Stats_FileReportThreeStatesSurviveDistinctlyToTheClient
// is the binding Phase 11 carry-forward, exercised end to end through the
// REAL HTTP handler's JSON encoding rather than only statsStore's own Go
// struct: an "off" instance (media_root_map never configured — the
// fileReport pass never ran at all, so fileReportRan stays false), a
// "skipped" one (the pass ran but a tracked root could not be trusted this
// sweep), and a "ran" one (a clean, trustworthy pass) must all reach the
// client with THEIR OWN status, not collapse into looking alike. Before
// this test, "off" and "ran" were each covered separately at the
// statsStore level and "ran" alone through this handler, but nothing
// exercised "skipped" through the handler, and nothing asserted the three
// are pairwise distinct on one response the way a real dashboard poll
// would see them together.
func TestWebUIHandler_Stats_FileReportThreeStatesSurviveDistinctlyToTheClient(t *testing.T) {
	s := newStatsStore(false)
	s.recordInstance(cycleKindStartup, time.Now(), "radarr-ran", "radarr", cycleInstanceStats{
		total: 5, decisionsRan: true, fileReportRan: true,
		fileReport: fileReportSnapshot{Status: "ran", Duplicates: 1, Findings: []fileReportFindingRecord{{Kind: "duplicate", Path: "/x"}}},
	})
	s.recordInstance(cycleKindStartup, time.Now(), "radarr-skipped", "radarr", cycleInstanceStats{
		total: 5, decisionsRan: true, fileReportRan: true,
		fileReport: fileReportSnapshot{Status: "skipped", Findings: []fileReportFindingRecord{}},
	})
	// radarr-off records NOTHING about the file report at all
	// (fileReportRan left at its zero value, false) — exactly how a real
	// instance with no media_root_map behaves, since fileReport.enabled is
	// never set for it and runRadarrFileReport is never even called.
	s.recordInstance(cycleKindStartup, time.Now(), "radarr-off", "radarr", cycleInstanceStats{
		total: 5, decisionsRan: true,
	})

	ts, _ := newTestWebUIServer(t, s)
	resp, err := http.Get(ts.URL + "/api/stats")
	if err != nil {
		t.Fatalf("GET /api/stats: %v", err)
	}
	defer resp.Body.Close()
	var body statsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}

	byName := map[string]instanceStatsView{}
	for _, inst := range body.Instances {
		byName[inst.Name] = inst
	}
	if got := byName["radarr-ran"].FileReport.Status; got != "ran" {
		t.Errorf("radarr-ran FileReport.Status = %q, want %q", got, "ran")
	}
	if got := byName["radarr-skipped"].FileReport.Status; got != "skipped" {
		t.Errorf("radarr-skipped FileReport.Status = %q, want %q", got, "skipped")
	}
	if got := byName["radarr-off"].FileReport.Status; got != "off" {
		t.Errorf("radarr-off FileReport.Status = %q, want %q", got, "off")
	}
	statuses := map[string]bool{
		byName["radarr-ran"].FileReport.Status:     true,
		byName["radarr-skipped"].FileReport.Status: true,
		byName["radarr-off"].FileReport.Status:     true,
	}
	if len(statuses) != 3 {
		t.Errorf("expected 3 PAIRWISE DISTINCT FileReport.Status values across ran/skipped/off on one response, got %v", statuses)
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
// end() not yet), with nothing pending, must still refuse a second scan —
// but, REVIEW FIX, the refusal must be truthful: the request must actually
// survive as pending for the daemon's own loop to pick up once the running
// cycle ends, not be silently dropped while still being reported as
// "already-pending" (the bug: the old requestScan returned false without
// ever setting s.pending when only s.running was true, so nothing was ever
// queued behind a running startup scan, sweep, or webhook cycle).
func TestWebUIHandler_Scan_AlreadyPendingWhileACycleIsRunning(t *testing.T) {
	ts, srv := newTestWebUIServer(t, newStatsStore(false))
	srv.scan.begin()

	got := postScan(t, ts.URL)
	if got.status != http.StatusAccepted || got.body["status"] != "already-pending" {
		t.Fatalf("POST /api/scan while a cycle is running = %+v, want 202 {status: already-pending}", got)
	}

	// The request must have actually been queued, not dropped: once the
	// running cycle ends, takePending() must report a manual scan waiting.
	srv.scan.end()
	if !srv.scan.takePending() {
		t.Fatal("takePending() = false after the running cycle ended; the POST that landed while it was running was silently dropped instead of queued behind it")
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

// TestWebUIPage_ShelfCountHeroSizeIsInTheMandatedBand pins the GUI design
// plan's type spec: "hero numbers large (28-36px) mono with small unit
// words". The shelf count ("456 / 996 at rest") is the page's one hero
// number; the small-unit-word half is covered by
// TestWebUIPage_ContainsTheSignatureShelfElement's "shelf-count" /
// ".unit" markers already existing, so this test covers only the size half.
func TestWebUIPage_ShelfCountHeroSizeIsInTheMandatedBand(t *testing.T) {
	page := string(webUIPage)
	re := regexp.MustCompile(`--shelf-count-size:\s*(\d+)px`)
	m := re.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("page does not define a --shelf-count-size custom property in px")
	}
	size, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("could not parse --shelf-count-size value %q: %v", m[1], err)
	}
	if size < 28 || size > 36 {
		t.Errorf("--shelf-count-size = %dpx, want 28-36px per the GUI design plan's hero-number type spec", size)
	}
	if !strings.Contains(page, "font-size: var(--shelf-count-size)") {
		t.Error(".shelf-count does not actually use the --shelf-count-size token for its font-size")
	}
}

// TestWebUIPage_ReusesShelfCardsAcrossRefreshesForTransitions pins the
// mandated "bar widths transition 300ms ease-out on refresh" motion rule
// structurally: a CSS transition needs a prior COMPUTED value on an
// already-rendered element to animate from, so the per-instance shelf card
// must be built ONCE and have only its text/style properties updated on
// each later refresh — never torn down and rebuilt from scratch (a fresh
// node with its final width set inline has nothing to transition from).
func TestWebUIPage_ReusesShelfCardsAcrossRefreshesForTransitions(t *testing.T) {
	page := string(webUIPage)
	if strings.Contains(page, "function renderShelf(") {
		t.Error("renderInstances still rebuilds every shelf card from scratch via a renderShelf(inst) function; the .shelf-rest/.shelf-marker CSS transitions can only fire on an element whose style properties change IN PLACE across refreshes")
	}
	for _, want := range []string{"shelfCards", "buildShelfCard", "updateShelfCard"} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q: the per-instance card must be built once (keyed so a later refresh finds the SAME element) and updated in place, not recreated, for the bar-width transition to ever have a prior value to animate from", want)
		}
	}
}

// TestWebUIPage_FileReportRendersThreeStateStatus is the page-side half of
// the binding Phase 11 carry-forward ("a skipped instance must never render
// as clean … an off instance shows the feature as not configured"): the
// page's JS must actually read fileReport.status and distinguish all three
// of the API contract's own vocabulary words, with copy that does not
// conflate any of them.
func TestWebUIPage_FileReportRendersThreeStateStatus(t *testing.T) {
	page := string(webUIPage)
	if !strings.Contains(page, "fr.status") && !strings.Contains(page, "fileReport.status") {
		t.Error("renderFileReport never reads fileReport.status")
	}
	for _, want := range []string{`"ran"`, `"skipped"`, `"off"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page's JS never references the fileReport.status value %s (the API contract's ran|skipped|off vocabulary)", want)
		}
	}
	for _, want := range []string{"not configured", "could not be checked this sweep"} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing the status-aware copy %q", want)
		}
	}
}

// TestWebUIPage_RefreshSurfacesAFailedPollExplicitly pins the "a dead
// dashboard must never be pixel-identical to a healthy one" fix: refresh()
// must check the response's ok status (a 5xx, or an HTML error page from a
// proxy, both decode-fail or must not be treated as success) and must
// surface an explicit disconnected state to the operator rather than an
// empty catch that keeps stale numbers on screen forever with no signal.
func TestWebUIPage_RefreshSurfacesAFailedPollExplicitly(t *testing.T) {
	page := string(webUIPage)
	if !strings.Contains(page, "r.ok") {
		t.Error("refresh() never checks the response's ok status; a 5xx or non-JSON response could be swallowed without any signal to the operator")
	}
	if !strings.Contains(page, "badge-alert") {
		t.Error("page never defines/uses the badge-alert (ALERT clay) token for a disconnected poll")
	}
	if !strings.Contains(page, "disconnected") {
		t.Error("a failed poll never surfaces an explicit \"disconnected\" state")
	}
}
