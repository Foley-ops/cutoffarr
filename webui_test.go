package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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

// TestWebUIHandler_Stats_CaseCollisionWireFieldsRawJSONSpelling is the
// [final review round] regression test for a gap every other case-collision
// test shared: every one of them decodes the response back into the SAME Go
// structs (fileReportSnapshot/fileReportFindingRecord/
// caseCollisionNameRecord) it was encoded from, so a typo'd or accidentally
// `,omitempty`'d struct tag on CaseCollisions/EntryType/Names would break
// webui.html's literal r.f.entryType/r.f.names/n.tracked reads AND README's
// "caseCollisions is always present (including 0)" contract with a fully
// green suite — decode/re-encode round-trips through Go field names, not
// wire spelling. This asserts the RAW bytes instead, the same pattern
// TestWebUIHandler_Stats_ReflectsARecordedCycle already uses for `"group":
// ""`: caseCollisions is present at the top level even though it is 1 (a
// non-zero case is not what would silently break under omitempty — this
// pins the KEY spelling, which the existing count-based tests never do),
// and entryType/names (with name/tracked) are present on the collision
// finding's own JSON object and ABSENT on the duplicate's, exactly as the
// API contract promises (`entryType`/`names` are case-collision-only).
func TestWebUIHandler_Stats_CaseCollisionWireFieldsRawJSONSpelling(t *testing.T) {
	s := newStatsStore(false)
	s.recordInstance(cycleKindSweep, time.Now(), "sonarr-main", "sonarr", cycleInstanceStats{
		total: 10, decisionsRan: true, fileReportRan: true,
		fileReport: fileReportSnapshot{
			Status: "ran", Duplicates: 1, Orphans: 0, CaseCollisions: 1,
			Findings: []fileReportFindingRecord{
				{Kind: "duplicate", Group: "S01E01", Path: "/tv/Show/dup.mkv", Display: "Show/dup.mkv", Count: 2},
				{Kind: "case-collision", Path: "/tv/Show", Display: "Show", EntryType: "dir",
					Names: []caseCollisionNameRecord{{Name: "Show", Tracked: true}, {Name: "show", Tracked: false}}},
			},
		},
	})
	ts, _ := newTestWebUIServer(t, s)

	resp, err := http.Get(ts.URL + "/api/stats")
	if err != nil {
		t.Fatalf("GET /api/stats: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)

	if !strings.Contains(body, `"caseCollisions":1`) {
		t.Errorf(`raw JSON missing "caseCollisions":1 (wire-spelled key) — a typo'd or omitempty'd struct tag would still round-trip through the Go struct decode every other test uses:\n%s`, raw)
	}

	// Pull each finding's OWN raw JSON object apart so presence/absence can
	// be checked per-object, not just "somewhere in the response" (the
	// collision finding's own entryType/names would otherwise make a
	// substring search pass even if the duplicate wrongly carried them too,
	// or mask a duplicate that wrongly omitted display/path).
	var generic struct {
		Instances []struct {
			FileReport struct {
				Findings []json.RawMessage `json:"findings"`
			} `json:"fileReport"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("response is not valid JSON: %v\n%s", err, raw)
	}
	if len(generic.Instances) != 1 || len(generic.Instances[0].FileReport.Findings) != 2 {
		t.Fatalf("expected exactly 1 instance with 2 findings, got %+v", generic)
	}
	dup := string(generic.Instances[0].FileReport.Findings[0])
	collision := string(generic.Instances[0].FileReport.Findings[1])

	if strings.Contains(dup, `"entryType"`) || strings.Contains(dup, `"names"`) {
		t.Errorf("the duplicate finding's own JSON object must never carry entryType/names (case-collision-only per the API contract):\n%s", dup)
	}
	if !strings.Contains(collision, `"entryType":"dir"`) {
		t.Errorf(`collision finding missing "entryType":"dir":%s`, collision)
	}
	if !strings.Contains(collision, `"names":[{"name":"Show","tracked":true},{"name":"show","tracked":false}]`) {
		t.Errorf(`collision finding's names array does not wire-spell {"name","tracked"} as promised:%s`, collision)
	}
	if strings.Contains(collision, `"group"`) || strings.Contains(collision, `"count"`) {
		t.Errorf("the case-collision finding's own JSON object must never carry group/count (duplicate-only per the API contract):\n%s", collision)
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

// TestWebUIHandler_Stats_ReverseStatusThreeStatesSurviveDistinctlyToTheClient
// is round-3's reverse-scan twin of the file-report test above: ran/skipped/
// off must all reach the client as pairwise distinct ReverseStatus values,
// through the real HTTP handler's JSON encoding.
func TestWebUIHandler_Stats_ReverseStatusThreeStatesSurviveDistinctlyToTheClient(t *testing.T) {
	s := newStatsStore(false)
	s.recordInstance(cycleKindStartup, time.Now(), "radarr-ran", "radarr", cycleInstanceStats{
		total: 5, decisionsRan: true, reverseRan: true,
		reverseFindings: []reverseFinding{{ID: 1, Title: "Finding", Reason: ReasonQualityCutoffNotMet}},
	})
	s.recordInstance(cycleKindStartup, time.Now(), "radarr-skipped", "radarr", cycleInstanceStats{
		total: 5, decisionsRan: true, reverseSkipped: true,
	})
	// radarr-off records neither reverseRan nor reverseSkipped — exactly how
	// a webhook cycle (or the reverse scan globally disabled) behaves.
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
	if got := byName["radarr-ran"].ReverseStatus; got != "ran" {
		t.Errorf("radarr-ran ReverseStatus = %q, want %q", got, "ran")
	}
	if got := byName["radarr-skipped"].ReverseStatus; got != "skipped" {
		t.Errorf("radarr-skipped ReverseStatus = %q, want %q", got, "skipped")
	}
	if got := byName["radarr-off"].ReverseStatus; got != "off" {
		t.Errorf("radarr-off ReverseStatus = %q, want %q", got, "off")
	}
	statuses := map[string]bool{
		byName["radarr-ran"].ReverseStatus:     true,
		byName["radarr-skipped"].ReverseStatus: true,
		byName["radarr-off"].ReverseStatus:     true,
	}
	if len(statuses) != 3 {
		t.Errorf("expected 3 PAIRWISE DISTINCT ReverseStatus values across ran/skipped/off on one response, got %v", statuses)
	}
}

// TestWebUIHandler_Stats_ReverseAsOf_SurvivesToTheClientAcrossASkippedCycle
// is the controller ruling's handler-level pin (Phase 12 final round: "the
// skipped-reverse-pass overwrite is the highest-severity item — last-known-
// good preservation + a staleness indicator on the page"): reverseAsOf must
// reach the client holding the LAST TRUSTWORTHY pass's own timestamp, not
// the most recent cycle's, and must be null when no pass has ever run.
func TestWebUIHandler_Stats_ReverseAsOf_SurvivesToTheClientAcrossASkippedCycle(t *testing.T) {
	s := newStatsStore(false)
	goodAt := time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC)

	s.recordInstance(cycleKindStartup, goodAt, "radarr-main", "radarr", cycleInstanceStats{
		total: 5, decisionsRan: true, reverseRan: true,
		reverseFindings: []reverseFinding{{ID: 1, Title: "Finding", Reason: ReasonQualityCutoffNotMet}},
	})
	s.recordInstance(cycleKindSweep, goodAt.Add(6*time.Hour), "radarr-main", "radarr", cycleInstanceStats{
		total: 5, decisionsRan: true, reverseSkipped: true,
	})
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
	main := byName["radarr-main"]
	if main.ReverseStatus != "skipped" {
		t.Fatalf("radarr-main ReverseStatus = %q, want %q", main.ReverseStatus, "skipped")
	}
	if main.ReverseAsOf == nil || !main.ReverseAsOf.Equal(goodAt) {
		t.Errorf("radarr-main ReverseAsOf = %v, want %v (the last cycle that actually completed the pass, not the skipped cycle 6h later)", main.ReverseAsOf, goodAt)
	}
	if len(main.ReverseFindings) != 1 {
		t.Errorf("radarr-main ReverseFindings = %+v, want the preserved finding from the last trustworthy pass", main.ReverseFindings)
	}
	if got := byName["radarr-off"].ReverseAsOf; got != nil {
		t.Errorf("radarr-off ReverseAsOf = %v, want nil: no pass has ever run for this instance", got)
	}
}

// TestWebUIHandler_Stats_UnreachableInstance_VisibleWithSkippedStatusAndNoTotals
// is controller addition N2's handler-level pin: an instance that has NEVER
// been reached at all must still appear in `instances` (never simply
// absent), carrying LastCycleStatus={status: skipped, reason: ...} and
// zero-value totals — through the real HTTP handler's JSON encoding.
func TestWebUIHandler_Stats_UnreachableInstance_VisibleWithSkippedStatusAndNoTotals(t *testing.T) {
	s := newStatsStore(false)
	s.recordUnreachable("radarr-main", "radarr", unreachableReasonConnectivity)

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
	if len(body.Instances) != 1 {
		t.Fatalf("Instances = %+v, want exactly 1: a never-reached instance must be VISIBLE, not absent", body.Instances)
	}
	inst := body.Instances[0]
	if inst.LastCycleStatus.Status != cycleStatusSkipped || inst.LastCycleStatus.Reason != unreachableReasonConnectivity {
		t.Errorf("LastCycleStatus = %+v, want {status: %q, reason: %q}", inst.LastCycleStatus, cycleStatusSkipped, unreachableReasonConnectivity)
	}
	if inst.Total != 0 || inst.LastRun != nil {
		t.Errorf("a never-reached instance must show zero-value totals and a nil LastRun, got Total=%d LastRun=%v", inst.Total, inst.LastRun)
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

// TestWebUIPage_ReducedMotionDisablesEveryAnimatedProperty is round-3's
// regression test for a gap TestWebUIPage_PollsStatsAndPausesWhenHidden's
// bare substring check couldn't see: the design plan says "No other
// animation" under prefers-reduced-motion, but the disclosure caret
// (details.panel summary::before) had its OWN 0.15s rotate transition that
// the reduced-motion media query never touched.
//
// This test parses the <style> block into individual CSS rules by hand
// (splitting on brace nesting depth — a real selector like "details.panel
// summary::before" contains a space and two colons, which is exactly why an
// earlier version of this test using a selector-charset regex silently
// matched nothing for that rule and could not have caught the bug it was
// written for; verified by reverting the fix and confirming THIS version
// fails while a charset-based regex version did not), finds every rule
// whose body sets a real `transition:` (not `transition: none`), and
// asserts every one of those selectors appears in the
// prefers-reduced-motion block's own selector list — so a THIRD animated
// property added later without updating that block fails here too, not
// only the two/three known today.
func TestWebUIPage_ReducedMotionDisablesEveryAnimatedProperty(t *testing.T) {
	page := string(webUIPage)

	styleStart := strings.Index(page, "<style>")
	styleEnd := strings.Index(page, "</style>")
	if styleStart == -1 || styleEnd == -1 || styleEnd < styleStart {
		t.Fatal("page has no <style>...</style> block")
	}
	// Comments are stripped BEFORE parsing: this test's own doc comment
	// above (and the source CSS's) mentions the literal selector text
	// "details.panel summary::before" in prose, which — left in — would
	// make the @media rule's parsed body contain that substring merely
	// because a COMMENT says it, regardless of whether the actual selector
	// list does. That false-positive is exactly what let an earlier version
	// of this test pass against the unfixed CSS; verified by reproducing it.
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(page[styleStart+len("<style>"):styleEnd], "")

	type rule struct{ selector, body string }
	var rules []rule
	depth := 0
	var selBuf, bodyBuf strings.Builder
	for _, r := range css {
		switch {
		case r == '{':
			depth++
			if depth == 1 {
				bodyBuf.Reset()
				continue
			}
		case r == '}':
			depth--
			if depth == 0 {
				rules = append(rules, rule{selector: strings.TrimSpace(selBuf.String()), body: bodyBuf.String()})
				selBuf.Reset()
				continue
			}
		}
		if depth == 0 {
			selBuf.WriteRune(r)
		} else if depth >= 1 {
			bodyBuf.WriteRune(r)
		}
	}
	if len(rules) == 0 {
		t.Fatal("test instrument parsed zero CSS rules; the page's <style> shape changed enough that this parser needs updating")
	}

	var reducedMotionBody string
	for _, ru := range rules {
		if strings.Contains(ru.selector, "prefers-reduced-motion: reduce") {
			reducedMotionBody = ru.body
		}
	}
	if reducedMotionBody == "" {
		t.Fatal("page has no @media (prefers-reduced-motion: reduce) rule")
	}

	animatedSelectors := 0
	for _, ru := range rules {
		if strings.HasPrefix(strings.TrimSpace(ru.selector), "@media") {
			continue
		}
		if !strings.Contains(ru.body, "transition:") || strings.Contains(ru.body, "transition: none") {
			continue
		}
		animatedSelectors++
		for _, one := range strings.Split(ru.selector, ",") {
			one = strings.TrimSpace(one)
			if one == "" {
				continue
			}
			if !strings.Contains(reducedMotionBody, one) {
				t.Errorf("selector %q sets a real transition (%q) but is not listed in the prefers-reduced-motion block %q",
					one, strings.TrimSpace(ru.body), reducedMotionBody)
			}
		}
	}
	if animatedSelectors == 0 {
		t.Fatal("test instrument found zero animated (transition, not none) rules; the page's CSS shape changed enough that this parser needs updating")
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

// TestWebUIPage_ShelfCountLabelClampsAwayFromCardEdges is round-4's final
// finding: .shelf-count is `position: absolute` with `left:<pct>%` (set from
// JS) and `transform: translateX(-50%)`, so at pct=0% (the common first-run
// "nothing at rest yet" state) or pct=100% the label's own center sits
// exactly on the card's edge and roughly HALF of the 28-36px mono hero
// number's rendered width hangs off past it — there is no clamp anywhere in
// the chain. A CSS-only `clamp()` on `.shelf-count`'s `left` cannot fix this:
// updateShelfCard sets `style.left` directly (an inline style), which always
// wins over a stylesheet rule for the same property regardless of
// specificity, so the clamp has to happen in JS, using the label's own
// measured width (not a guessed constant — the string's length varies with
// the library's size, "5 / 12" vs "996 / 1042").
func TestWebUIPage_ShelfCountLabelClampsAwayFromCardEdges(t *testing.T) {
	page := string(webUIPage)

	start := strings.Index(page, "function updateShelfCard(")
	if start == -1 {
		t.Fatal("page has no updateShelfCard function")
	}
	end := strings.Index(page[start:], "\n  function renderInstances(")
	if end == -1 {
		t.Fatal("could not find the end of updateShelfCard (renderInstances must follow it)")
	}
	body := page[start : start+end]

	if !strings.Contains(body, "offsetWidth") {
		t.Error("updateShelfCard never reads the label's own rendered offsetWidth; a clamp cannot know how much room the label actually needs without measuring it")
	}
	if !strings.Contains(body, "Math.max") || !strings.Contains(body, "Math.min") {
		t.Error("updateShelfCard has no Math.max/Math.min clamp on the label's pixel position")
	}
	// The unclamped pct must not reach the label's own left directly outside
	// the (rare, unlaid-out) fallback branch — it must instead flow through
	// a pixel computation that Math.max/Math.min above bound to the wrap's
	// own measured width.
	countLeftAssignments := strings.Count(body, "els.count.style.left =")
	if countLeftAssignments < 2 {
		t.Errorf("expected updateShelfCard to set els.count.style.left from at least two branches (the measured pixel clamp, and an unlaid-out fallback), got %d assignment(s) — the common case must not be the raw unclamped percentage", countLeftAssignments)
	}
	if !strings.Contains(body, `els.count.style.left = pxLeft + "px";`) {
		t.Error("updateShelfCard never sets the label's left to a clamped pixel value")
	}
	// The fill and marker are the truthful signal and must stay at the
	// EXACT, unclamped percentage — only the text label's position may be
	// nudged inward.
	if !strings.Contains(body, "els.rest.style.width = pctStr;") {
		t.Error("the shelf-rest fill must stay at the exact, unclamped percentage")
	}
	if !strings.Contains(body, "els.marker.style.left = pctStr;") {
		t.Error("the shelf-marker line must stay at the exact, unclamped percentage")
	}
}

// TestWebUIPage_InitialShelfLabelPositioningRoutesThroughSharedUpdateFn pins
// the fix for the "hero count overhangs the card's left edge on the FIRST
// render" bug: buildShelfCard's freshly-created card isn't laid out yet at
// the moment renderInstances appends it, so calling updateShelfCard's
// offsetWidth-based clamp (pinned above) right there measures 0 and skips
// the clamp — the label hangs off the card's edge until the next 30s poll
// re-measures it correctly. The fix must not solve this by forking a
// second, first-render-only copy of the positioning logic: both the
// first-render path and the refresh path must call the SAME
// updateShelfCard, with only the first-render call deferred (e.g. via
// requestAnimationFrame) so it runs after the browser has actually laid the
// just-appended card out.
func TestWebUIPage_InitialShelfLabelPositioningRoutesThroughSharedUpdateFn(t *testing.T) {
	page := string(webUIPage)

	// Exactly one updateShelfCard definition: no forked, first-render-only
	// copy of the positioning logic.
	if n := strings.Count(page, "function updateShelfCard("); n != 1 {
		t.Fatalf("expected exactly one updateShelfCard definition, found %d — the initial-render path must reuse it, not fork a second copy of the label-positioning logic", n)
	}

	// Something must defer the first-render call until after layout.
	// requestAnimationFrame is the standard mechanism for "run this after
	// the browser has laid the DOM out"; a synchronous call at the moment
	// of append is exactly the bug being fixed.
	if !strings.Contains(page, "requestAnimationFrame") {
		t.Error("page never uses requestAnimationFrame; the first-render label positioning must be deferred until after the browser has laid the freshly-appended card out, or it will measure a width of 0 like the original bug")
	}

	// updateShelfCard(card, inst) must be the call both paths make — once
	// from inside the deferred callback, once directly on refresh. A
	// single call site would mean one of the two paths isn't actually
	// routed through the shared function.
	if calls := strings.Count(page, "updateShelfCard(card, inst)"); calls < 2 {
		t.Errorf("expected updateShelfCard(card, inst) to be invoked from at least two call sites (a requestAnimationFrame-deferred first-render call, and a direct refresh call), found %d", calls)
	}

	// renderInstances' own new-card branch must be the one doing the
	// deferring, not some unrelated part of the script.
	start := strings.Index(page, "function renderInstances(")
	if start == -1 {
		t.Fatal("page has no renderInstances function")
	}
	end := strings.Index(page[start:], "\n  function findingsTable(")
	if end == -1 {
		t.Fatal("could not find the end of renderInstances (findingsTable must follow it)")
	}
	body := page[start : start+end]
	if !strings.Contains(body, "isNewCard") {
		t.Error("renderInstances no longer distinguishes a brand-new card from a reused one; that distinction is what routes only the unlaid-out first render through the deferred path")
	}
	if !strings.Contains(body, "scheduleInitialShelfUpdate(card, inst)") {
		t.Error("renderInstances' new-card branch does not call scheduleInitialShelfUpdate — the deferred, layout-safe path to updateShelfCard")
	}
	if !strings.Contains(body, "updateShelfCard(card, inst)") {
		t.Error("renderInstances' reused-card branch no longer calls updateShelfCard directly")
	}
}

// TestWebUIPage_ScriptIsSyntacticallyValidJS runs `node --check` over the
// embedded page's inline <script> block — the one live-code check available
// under this project's "no browser" testing constraint. It would have caught
// a stray syntax error in the shelf-count clamp fix (round-4 final round)
// long before a human ever opened the page.
func TestWebUIPage_ScriptIsSyntacticallyValidJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS syntax check")
	}

	page := string(webUIPage)
	start := strings.Index(page, "<script>")
	end := strings.Index(page, "</script>")
	if start == -1 || end == -1 || end < start {
		t.Fatal("page has no <script>...</script> block to check")
	}
	script := page[start+len("<script>") : end]

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "webui.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatalf("could not write extracted script: %v", err)
	}

	cmd := exec.Command(nodePath, "--check", scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("node --check reports a syntax error in the embedded page's script:\n%s", out)
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

// TestWebUIPage_FindingsPaginationDefaultsAndSizeTokens pins the reverse-scan
// / file-clutter pagination's basic contract: a default page size of 25, and
// the exact four size tokens (10 · 25 · 50 · all) an operator can switch
// between — a typo or dropped token here would silently remove a size
// option or change what a fresh page load shows before anyone clicks
// anything.
func TestWebUIPage_FindingsPaginationDefaultsAndSizeTokens(t *testing.T) {
	page := string(webUIPage)

	if !strings.Contains(page, "DEFAULT_PAGE_SIZE = 25") {
		t.Error("page's default findings page size is not pinned to 25")
	}
	if !strings.Contains(page, `PAGE_SIZES = [10, 25, 50, "all"]`) {
		t.Error(`page does not define the four page-size tokens as [10, 25, 50, "all"]`)
	}
	if !strings.Contains(page, "size: DEFAULT_PAGE_SIZE") {
		t.Error("a pager's initial state does not default to DEFAULT_PAGE_SIZE")
	}
}

// TestWebUIPage_FindingsPaginationSlicingIsOneSharedImplementation pins that
// renderReverse and renderFileReport slice their rows through the SAME
// paginateRows function — a second, drifted copy of the arithmetic in one
// section would be exactly the kind of bug that only shows up once a panel
// has enough rows to page past page 1.
func TestWebUIPage_FindingsPaginationSlicingIsOneSharedImplementation(t *testing.T) {
	page := string(webUIPage)

	if n := strings.Count(page, "function paginateRows("); n != 1 {
		t.Fatalf("expected exactly one paginateRows definition, found %d — the reverse-scan and file-clutter panels must share one slicing implementation, not each carry their own", n)
	}
	if n := strings.Count(page, "= paginateRows(rows,"); n != 2 {
		t.Errorf("expected paginateRows to be called (assigned to a result) from exactly two call sites (renderReverse and renderFileReport), found %d", n)
	}

	start := strings.Index(page, "function renderReverse(")
	if start == -1 {
		t.Fatal("page has no renderReverse function")
	}
	end := strings.Index(page[start:], "\n  function renderFileReport(")
	if end == -1 {
		t.Fatal("could not find the end of renderReverse (renderFileReport must follow it)")
	}
	reverseBody := page[start : start+end]
	if !strings.Contains(reverseBody, "paginateRows(rows, reversePager)") {
		t.Error("renderReverse does not call paginateRows against its own reversePager state")
	}

	fileStart := start + end
	fileEnd := strings.Index(page[fileStart:], "\n  function render(data)")
	if fileEnd == -1 {
		t.Fatal("could not find the end of renderFileReport (render(data) must follow it)")
	}
	fileBody := page[fileStart : fileStart+fileEnd]
	if !strings.Contains(fileBody, "paginateRows(rows, filePager)") {
		t.Error("renderFileReport does not call paginateRows against its own filePager state")
	}
}

// TestWebUIPage_FindingsPaginationPositionLineFormat pins the "a–b of n"
// position-line format (an en dash, matching the design plan's exact
// wording) that buildPagerControls renders next to each section's page-size
// and prev/next controls.
func TestWebUIPage_FindingsPaginationPositionLineFormat(t *testing.T) {
	page := string(webUIPage)
	if !strings.Contains(page, `(page.start + 1) + "–" + page.end + " of " + page.total`) {
		t.Error(`buildPagerControls does not render the "a–b of n" position line in the expected format`)
	}
	if !strings.Contains(page, "pager-position") {
		t.Error("page has no pager-position element for the position line")
	}
}

// TestWebUIPage_FindingsPaginationPreservesPositionAcrossPollsWithSameCount
// pins the "do not yank the operator back to page 1 every 30s" requirement:
// a poll refresh must only reset a section's page back to 1 when that
// section's row COUNT has actually changed since the last render — never
// unconditionally on every refresh, which would make paging past page 1
// useless on a page that re-polls every 30 seconds.
func TestWebUIPage_FindingsPaginationPreservesPositionAcrossPollsWithSameCount(t *testing.T) {
	page := string(webUIPage)

	if !strings.Contains(page, "function resetPagerIfCountChanged(") {
		t.Fatal("page has no resetPagerIfCountChanged function guarding page resets by row-count change")
	}
	if !strings.Contains(page, "resetPagerIfCountChanged(reversePager, rows.length)") {
		t.Error("renderReverse never calls resetPagerIfCountChanged — its page could reset on every poll regardless of whether the row count changed")
	}
	if !strings.Contains(page, "resetPagerIfCountChanged(filePager, rows.length)") {
		t.Error("renderFileReport never calls resetPagerIfCountChanged — its page could reset on every poll regardless of whether the row count changed")
	}

	start := strings.Index(page, "function resetPagerIfCountChanged(")
	if start == -1 {
		t.Fatal("page has no resetPagerIfCountChanged function")
	}
	end := strings.Index(page[start:], "\n  }")
	if end == -1 {
		t.Fatal("could not find the end of resetPagerIfCountChanged")
	}
	body := page[start : start+end]
	if !strings.Contains(body, "pager.lastCount !== count") {
		t.Error("resetPagerIfCountChanged does not compare against a persisted lastCount before resetting the page — a reset on every call would reintroduce the yank-to-page-1 bug")
	}
	if !strings.Contains(body, "pager.page = 1") {
		t.Error("resetPagerIfCountChanged never resets the page to 1 when the count actually changed")
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

// TestWebUIPage_FileReportShowsRootRelativeDisplayPathWithFullPathAsTitle
// pins the "full cutoffarr-side path overhangs the file-clutter row" fix: the
// path cell's visible text must come from the finding's root-relative
// `display` field (falling back to `path` for a daemon that predates it,
// never the reverse — `display` is always the shorter, screen-sized one),
// and the cell's `title` attribute must carry the untruncated `path` so the
// full path is still available on hover.
func TestWebUIPage_FileReportShowsRootRelativeDisplayPathWithFullPathAsTitle(t *testing.T) {
	page := string(webUIPage)

	start := strings.Index(page, "function renderFileReport(")
	if start == -1 {
		t.Fatal("page has no renderFileReport function")
	}
	end := strings.Index(page[start:], "\n  function render(data)")
	if end == -1 {
		t.Fatal("could not find the end of renderFileReport (render(data) must follow it)")
	}
	body := page[start : start+end]

	if !strings.Contains(body, `text(td, r.f.display || r.f.path || "")`) {
		t.Error("renderFileReport's path cell does not render r.f.display (falling back to r.f.path) as its visible text")
	}
	if !strings.Contains(body, `td.title = r.f.path || "";`) {
		t.Error("renderFileReport's path cell does not set its title attribute to the full r.f.path for a hover")
	}
}

// TestWebUIPage_FileReportKindCellDoesNotWrapMidWord pins the "duplicate
// breaks mid-word (dupli-cate) in its narrow column" fix: the kind cell must
// opt out of table.findings td's page-wide word-break: break-word (below)
// via its own class, and that class must reserve enough width to hold
// "duplicate" — [ROUND, inline-display fix] once again the longest kind
// value this column ever renders, now that case-collision findings are
// rendered exclusively by buildCaseCollisionBlock (see
// TestWebUIPage_FileReportTableExcludesCaseCollisionRowsButHeaderCount
// IncludesThem) and never reach table.findings at all — deliberately
// shrunk back down from [v2.1]'s 14ch (sized for "case-collision", which
// briefly passed through this same cell) rather than left oversized for a
// value that can no longer appear here.
func TestWebUIPage_FileReportKindCellDoesNotWrapMidWord(t *testing.T) {
	page := string(webUIPage)

	if !strings.Contains(page, `td = el("td", "kind");`) {
		t.Error("renderFileReport's kind cell is not built with the \"kind\" class")
	}

	start := strings.Index(page, "table.findings td.kind {")
	if start == -1 {
		t.Fatal("page has no table.findings td.kind CSS rule")
	}
	end := strings.Index(page[start:], "}")
	if end == -1 {
		t.Fatal("could not find the end of the table.findings td.kind rule")
	}
	rule := page[start : start+end]
	if !strings.Contains(rule, "white-space: nowrap") {
		t.Error("table.findings td.kind does not set white-space: nowrap — \"duplicate\" can still wrap (and break) mid-word")
	}
	if !strings.Contains(rule, "min-width: 9ch") {
		t.Error("table.findings td.kind does not reserve 9ch — the auto-sized column can still come out narrower than \"duplicate\" itself")
	}
}

// TestWebUIPage_FileReportKindCellUsesClayChipStyling pins [v2.1]'s "renders
// the kind with the existing chip styling (clay)" requirement: the kind
// cell's text is wrapped in the page's OWN existing badge/badge-hunt chip
// (the same clay-toned pill .shelf-unreachable and the dry-run/live badge
// already use), not a bespoke new visual element — see the CSS token
// comment's own "never introduced fresh" rule.
func TestWebUIPage_FileReportKindCellUsesClayChipStyling(t *testing.T) {
	page := string(webUIPage)

	start := strings.Index(page, "function renderFileReport(")
	if start == -1 {
		t.Fatal("page has no renderFileReport function")
	}
	end := strings.Index(page[start:], "\n  function render(data)")
	if end == -1 {
		t.Fatal("could not find the end of renderFileReport (render(data) must follow it)")
	}
	body := page[start : start+end]

	if !strings.Contains(body, `"badge badge-hunt"`) {
		t.Error("renderFileReport's kind cell does not reuse the existing badge/badge-hunt (clay) chip styling")
	}
	// [final review round fix] The prior version of this test was relaxed to
	// pin only the chip's creation/class, losing the assertion that the
	// chip's own TEXT is the finding's kind — dropping
	// `text(kindChip, r.f.kind || "")` (webui.html) would ship an empty clay
	// pill in every findings row with the rest of this test still green.
	if !strings.Contains(body, `text(kindChip, r.f.kind`) {
		t.Error("renderFileReport's kind chip never sets its own text to r.f.kind — the chip would render empty")
	}
}

// TestWebUIPage_FileReportCaseCollisionNoticeExplainsActionability pins
// [ROUND, inline-display fix]'s shortened notice copy: the user-reported bug
// was that the OLD notice ("N case-twin name collisions found — two names
// differing only by case; ...") pointed at rows buried in a multi-thousand-
// row table, so the collisions themselves (buildCaseCollisionBlock, rendered
// immediately below this notice — see
// TestWebUIPage_CaseCollisionBlockRendersEveryCollisionsNamesAndTracked) now
// speak for themselves; the notice's own copy shortens to actionability
// alone ("merge or rename"), still in the panel's plain, second-person-free
// operator voice, still PLURALIZED to the collision count the same way the
// reverse-scan panel's own notice pluralizes "item"/"items".
func TestWebUIPage_FileReportCaseCollisionNoticeExplainsActionability(t *testing.T) {
	page := string(webUIPage)

	start := strings.Index(page, "function renderFileReport(")
	if start == -1 {
		t.Fatal("page has no renderFileReport function")
	}
	end := strings.Index(page[start:], "\n  function render(data)")
	if end == -1 {
		t.Fatal("could not find the end of renderFileReport (render(data) must follow it)")
	}
	body := page[start : start+end]

	if !strings.Contains(body, "case-twin name") {
		t.Error(`renderFileReport's case-collision notice no longer identifies the finding as a "case-twin name[s]"`)
	}
	if !strings.Contains(body, "merge or rename") {
		t.Error("renderFileReport never explains the actionability of a case-collision finding (\"merge or rename\")")
	}
	if !strings.Contains(body, `=== 1 ? "" : "s"`) && !strings.Contains(body, `== 1 ? "" : "s"`) {
		t.Error("renderFileReport's case-collision notice does not pluralize its copy to the count, unlike the rest of this page's own notices")
	}
	if !strings.Contains(body, "buildCaseCollisionBlock(collisions)") {
		t.Error("renderFileReport's case-collision notice is no longer followed by buildCaseCollisionBlock — the collisions it announces are not actually rendered")
	}
}

// TestWebUIPage_FileReportTableExcludesCaseCollisionRowsButHeaderCount
// IncludesThem pins [ROUND, inline-display fix]'s controller ruling head-on:
// a case-twin collision used to be just as easy to lose inside a
// multi-thousand-row paginated table as any other row — the user-reported
// bug this whole round exists to fix — so case-collision findings are now
// routed to `collisions` (buildCaseCollisionBlock's own display, never
// table.findings) instead of `rows` (table.findings' data), UNCONDITIONALLY
// by kind, deliberately reversing the PRIOR round's
// "rows.push is never kind-filtered" pin now that collisions have
// their own, better home. The header count is a DIFFERENT promise, made
// separately and explicitly kept: "3412 stays 3412" — fileCountText must
// still total every finding of every kind (totalFindingCount), not just the
// rows the table ends up paginating, so dropping collisions from the table
// must never also drop them from what the collapsed panel's own summary
// count claims to have found.
func TestWebUIPage_FileReportTableExcludesCaseCollisionRowsButHeaderCountIncludesThem(t *testing.T) {
	page := string(webUIPage)

	start := strings.Index(page, "function renderFileReport(")
	if start == -1 {
		t.Fatal("page has no renderFileReport function")
	}
	end := strings.Index(page[start:], "\n  function render(data)")
	if end == -1 {
		t.Fatal("could not find the end of renderFileReport (render(data) must follow it)")
	}
	body := page[start : start+end]

	// totalFindingCount is incremented unconditionally, before the
	// kind-based routing branch — every finding counts toward the header
	// regardless of which of the two homes (table row vs. collision block)
	// it ends up in.
	if !strings.Contains(body, "j++) {\n          totalFindingCount++;") {
		t.Error("totalFindingCount++ is not the row-building loop's own first, unconditional statement — the header count could silently drop a kind")
	}
	// The routing itself: case-collision goes to collisions, everything
	// else still goes to rows — table.findings' own data source.
	if !strings.Contains(body, `if (findings[j].kind === "case-collision") {`) {
		t.Fatal("renderFileReport's finding loop has no case-collision routing branch")
	}
	if !strings.Contains(body, "collisions.push({ instance: inst.name, f: findings[j] });") {
		t.Error("case-collision findings are never pushed into collisions — buildCaseCollisionBlock would have nothing to render")
	}
	if !strings.Contains(body, "rows.push({ instance: inst.name, f: findings[j] });") {
		t.Error("non-case-collision findings are never pushed into rows — table.findings would have nothing to page")
	}
	// The header must total every finding (totalFindingCount), never just
	// the ones that made it into the now-filtered rows.
	if !strings.Contains(body, "var fileCountText = String(totalFindingCount);") {
		t.Error("fileCountText is not derived from totalFindingCount — the header count would undercount once case-collision rows are excluded from `rows`")
	}
	if strings.Contains(body, "var fileCountText = String(rows.length);") {
		t.Error("fileCountText is still derived from rows.length — case-collision findings would silently vanish from the header count now that they are excluded from rows")
	}
}

// TestWebUIPage_CaseCollisionBlockRendersEveryCollisionsNamesAndTracked pins
// [ROUND, inline-display fix]'s whole point: the collisions themselves must
// be the obvious thing on screen, not a fact buried in a 3000+ row table.
// buildCaseCollisionBlock must loop over EVERY collision passed to it (not
// just the first — a single hardcoded item would satisfy a test that only
// ever checks for one) and, within each, over EVERY colliding name (same
// reasoning) — the two-loop shape is the structural stand-in this project's
// "no browser" test suite uses for "renders 2+ collisions, and 2+ names
// within one, not just index 0" (mirrors how
// TestWebUIPage_ShelfCountLabelClampsAwayFromCardEdges and friends pin
// per-item loops rather than executing the DOM). It must also read
// f.entryType to call out the "mixed" case (a folder colliding with a file
// — the one case genuinely worth a label, unlike a plain dir/dir or
// file/file twin) and mark each tracked name with the clay [tracked]
// marker.
func TestWebUIPage_CaseCollisionBlockRendersEveryCollisionsNamesAndTracked(t *testing.T) {
	page := string(webUIPage)

	start := strings.Index(page, "function buildCaseCollisionBlock(")
	if start == -1 {
		t.Fatal("page has no buildCaseCollisionBlock function")
	}
	end := strings.Index(page[start:], "\n  function renderFileReport(")
	if end == -1 {
		t.Fatal("could not find the end of buildCaseCollisionBlock (renderFileReport must follow it)")
	}
	body := page[start : start+end]

	if !strings.Contains(body, "for (var i = 0; i < collisions.length; i++)") {
		t.Error("buildCaseCollisionBlock does not loop over every collision (collisions.length) — a 2nd+ collision would never render")
	}
	if !strings.Contains(body, "for (var j = 0; j < names.length; j++)") {
		t.Error("buildCaseCollisionBlock does not loop over every colliding name (names.length) — a 2nd+ name in one collision would never render")
	}
	if !strings.Contains(body, "f.display || f.path") {
		t.Error("buildCaseCollisionBlock never renders the collision's own root-relative display path")
	}
	if !strings.Contains(body, `f.entryType === "mixed"`) {
		t.Error(`buildCaseCollisionBlock never checks for the "mixed" entryType (a folder colliding with a file)`)
	}
	if !strings.Contains(body, "names[j].tracked") {
		t.Error("buildCaseCollisionBlock never reads a colliding name's own tracked correlation")
	}
	if !strings.Contains(body, `"case-collision-tracked"`) {
		t.Error("buildCaseCollisionBlock never applies the clay case-collision-tracked marker class")
	}
	if !strings.Contains(body, "[tracked]") {
		t.Error(`buildCaseCollisionBlock's tracked marker text is not "[tracked]"`)
	}

	// The CSS backing the block's prominence/readability split: the notice
	// and each collision's own directory line stay amber (hunt), the
	// colliding names render in the page's normal, readable ink, and the
	// tracked marker is clay (alert) — never a color introduced fresh.
	for _, want := range []string{
		".case-collision-dir {",
		".case-collision-name {",
		".case-collision-tracked {",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing the %q CSS rule the case-collision block depends on", want)
		}
	}
	if !strings.Contains(page, "color: var(--alert);") {
		t.Error("no case-collision styling uses the clay (--alert) token for the [tracked] marker")
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

// TestWebUIPage_DisconnectedStateNeverClaimsASweepIsRunning is round-3's
// regression test for controller addition N1: a page that has NEVER once
// heard back from a daemon (a stale tab, a crashed container, a proxy
// serving the static page without the backend behind it) must say so
// explicitly, and must never leave the static "a sweep is in progress"
// placeholder on screen as though it were still true.
func TestWebUIPage_DisconnectedStateNeverClaimsASweepIsRunning(t *testing.T) {
	page := string(webUIPage)
	const cantReach = "Can't reach cutoffarr. The page is being served without its daemon — start cutoffarr and reload."
	const firstSweep = "First sweep in progress — the shelves fill in when it finishes."

	if !strings.Contains(page, cantReach) {
		t.Errorf("page is missing the unreachable-daemon copy %q", cantReach)
	}
	if !strings.Contains(page, firstSweep) {
		t.Errorf("page is missing the daemon-up-no-cycle-yet copy %q", firstSweep)
	}
	if strings.Contains(page, "still working") {
		t.Error("page still contains the old ambiguous placeholder copy, which could be read as \"a sweep is in progress\" even when the daemon cannot be reached at all")
	}

	// The unreachable copy must be reachable ONLY from a path gated on
	// lastSuccessAt being unset — i.e. from inside showDisconnected, not
	// from render()'s own empty-instances branch (which is state (b), a
	// live daemon that simply has no cycle yet, and must keep the OTHER
	// copy). Extract showDisconnected's own function body and check both
	// properties hold there.
	start := strings.Index(page, "function showDisconnected()")
	if start == -1 {
		t.Fatal("page has no showDisconnected function")
	}
	end := strings.Index(page[start:], "\n  function refresh()")
	if end == -1 {
		t.Fatal("could not find the end of showDisconnected (refresh() must follow it)")
	}
	body := page[start : start+end]
	if !strings.Contains(body, "lastSuccessAt") {
		t.Error("showDisconnected never checks lastSuccessAt: it cannot tell \"never once connected\" apart from \"was connected, now isn't\"")
	}
	if !strings.Contains(body, cantReach) {
		t.Error("showDisconnected's own body never contains the unreachable-daemon copy")
	}
}

// TestWebUIPage_FileClutterHeaderDistinguishesCleanFromNotChecked is
// round-3's regression test: the <summary> row's own fileCount text — what
// a COLLAPSED panel actually shows the glancing operator — used to be a
// bare row count, making "(0)" mean either "every root was clean" or
// "nothing was ever looked at" identically. The fix must qualify the count
// right there in the summary, not only in the (collapsed, unseen) body.
func TestWebUIPage_FileClutterHeaderDistinguishesCleanFromNotChecked(t *testing.T) {
	page := string(webUIPage)
	if strings.Contains(page, `text(document.getElementById("fileCount"), String(rows.length));`) {
		t.Error("fileCount summary is still a bare row count; an all-off/all-skipped deployment renders the exact same \"(0)\" as an all-clean one")
	}
	if !strings.Contains(page, "not configured") || !strings.Contains(page, "not checked") {
		t.Error("fileCount summary text never distinguishes not-configured/not-checked instances from a genuine 0")
	}
}

// TestWebUIPage_FileClutterHeaderNamesEveryFindingKind is round-3's [v2.1
// review] regression test: the panel's own collapsed <summary> — the exact
// text a glancing operator sees without expanding anything — still read
// "File clutter — duplicates & orphans (N)" even after case-collision
// findings became a THIRD kind this same panel renders. fileCountText (the
// number inside that summary) is `totalFindingCount` — [ROUND, inline-
// display fix] renamed from the original `rows.length` once case-collision
// findings were routed OUT of `rows` and into their own `collisions` array
// (buildCaseCollisionBlock's data, never table.findings') — but the
// controller ruling was explicit that the header must still total every
// finding of every kind regardless of which of the two homes it ends up in:
// an instance with 0 duplicates, 0 orphans and 3 case-twins must still
// render "duplicates & orphans (3)", a count that would otherwise
// contradict its own label. This is the same "the number means something
// other than what the label says" failure TestWebUIPage_FileClutterHeader
// DistinguishesCleanFromNotChecked (above) exists to prevent for the
// off/skipped qualifier — here pinning the label itself so it can never
// again omit a kind renderFileReport actually counts.
func TestWebUIPage_FileClutterHeaderNamesEveryFindingKind(t *testing.T) {
	page := string(webUIPage)
	start := strings.Index(page, "<summary>File clutter")
	if start == -1 {
		t.Fatal("page has no File clutter panel <summary>")
	}
	end := strings.Index(page[start:], "</summary>")
	if end == -1 {
		t.Fatal("File clutter panel <summary> has no closing tag")
	}
	summary := page[start : start+end]
	// duplicate, orphan, and case-collision are the only three kinds
	// renderFileReport's finding loop ever counts into totalFindingCount
	// (see fileKindDuplicate/fileKindOrphan/fileKindCaseCollision in
	// filereport.go) — every one of them must be named in the heading that
	// counts them, in the panel's own established copy for each ("case-twin"
	// is the operator-facing term used everywhere else on this panel, e.g.
	// the actionability notice below).
	for _, want := range []string{"duplicates", "orphans", "case-twin"} {
		if !strings.Contains(summary, want) {
			t.Errorf("File clutter panel <summary> %q is missing %q; the heading must name every finding kind the panel can display, not just the first two", summary, want)
		}
	}
}

// TestWebUIPage_ScanNowChecksResponseOkAndSurfacesStatus is round-3's
// regression test for the Scan-now click handler's sibling bug to the one
// round 2 fixed on refresh(): the POST's response was never checked for
// r.ok NOR decoded, so a refused/failed scan (404/405/500, or a network
// failure) was indistinguishable from a queued one — refresh() still ran
// unconditionally, still repainted a healthy badge, and the ONLY thing that
// says whether the click did anything (the response body) was discarded.
func TestWebUIPage_ScanNowChecksResponseOkAndSurfacesStatus(t *testing.T) {
	page := string(webUIPage)
	start := strings.Index(page, `scanBtn.addEventListener("click"`)
	if start == -1 {
		t.Fatal("page has no scanBtn click handler")
	}
	handler := page[start:]
	if idx := strings.Index(handler, "\n  refresh().then(schedulePoll);"); idx != -1 {
		handler = handler[:idx]
	}

	if !strings.Contains(handler, "r.ok") {
		t.Error("Scan-now click handler never checks the POST response's ok status")
	}
	if !strings.Contains(handler, "already-pending") {
		t.Error("Scan-now click handler never reads the returned {status} body")
	}
	if !strings.Contains(handler, "showDisconnected()") {
		t.Error("Scan-now click handler's failure path never routes through showDisconnected(); a refused/failed scan is left as a silent, unsignaled button reset")
	}
}

// TestWebUIPage_ReverseStatusThreeStateFidelity is renderFileReport's
// three-state test (TestWebUIPage_FileReportRendersThreeStateStatus),
// mirrored for the reverse scan: the page's JS must read reverseStatus and
// gate the "Nothing wrongly unmonitored" invitation on at least one
// instance actually having a TRUSTED pass, never printing it as a stand-in
// for "never ran" or "ran but could not be trusted".
func TestWebUIPage_ReverseStatusThreeStateFidelity(t *testing.T) {
	page := string(webUIPage)
	if !strings.Contains(page, "inst.reverseStatus") && !strings.Contains(page, ".reverseStatus") {
		t.Error("renderReverse never reads reverseStatus")
	}
	if !strings.Contains(page, "anyRan") {
		t.Error(`renderReverse never gates its empty-state invitation on "at least one instance actually ran a trustworthy pass" (anyRan)`)
	}
	if !strings.Contains(page, "could not be trusted this sweep") {
		t.Error("page is missing skipped-reverse-scan notice copy")
	}
}

// TestWebUIPage_ReverseCountHeaderDistinguishesCleanFromNotChecked is
// renderFileReport's own fix (TestWebUIPage_FileClutterHeaderDistinguishes
// CleanFromNotChecked), mirrored for the reverse scan: round-4 review
// finding — the "Needs your eyes — reverse scan (N)" summary is what a
// COLLAPSED <details> (the state the glancing operator actually sees)
// shows, and a bare row count made "(0)" mean either "every instance's
// reverse pass was clean" or "no instance has ever produced a trustworthy
// pass at all", indistinguishably — exactly the ambiguity round 3 already
// removed one function down, for file clutter.
func TestWebUIPage_ReverseCountHeaderDistinguishesCleanFromNotChecked(t *testing.T) {
	page := string(webUIPage)
	if strings.Contains(page, `text(document.getElementById("reverseCount"), String(rows.length));`) {
		t.Error("reverseCount summary is still a bare row count; a deployment where every instance is off/skipped renders the exact same \"(0)\" as an all-clean one")
	}
	if !strings.Contains(page, "not checked yet") || !strings.Contains(page, "not trusted this sweep") {
		t.Error("reverseCount summary text never distinguishes not-checked/not-trusted instances from a genuine 0")
	}
}

// TestWebUIPage_ReverseOffInstancesGetANoticeNotABlankBody is round-4
// review's other reverse-scan finding: an instance whose reverseStatus is
// "off" (never configured to run reverse at all, OR — made MORE reachable
// by round 3's own recordUnreachable fix — an instance recordUnreachable
// just created, or a cycle that aborted before the reverse pass) used to
// contribute NOTHING to renderReverse: no row, no notice, unlike
// renderFileReport's off-instance handling one function down. When every
// instance is "off", the panel body was left with literally nothing
// appended — no empty-state invitation either, since that is gated on
// anyRan||instances.length===0 and neither held — so expanding the panel
// showed a blank box with zero explanation, directly under a collapsed
// summary already reading the ambiguous bare "(0)".
func TestWebUIPage_ReverseOffInstancesGetANoticeNotABlankBody(t *testing.T) {
	page := string(webUIPage)
	const offNotice = "the reverse scan has not completed a trustworthy pass for this instance yet"
	if !strings.Contains(page, offNotice) {
		t.Errorf("page is missing the off/absent reverseStatus notice copy %q; an all-off deployment renders a blank panel body with no explanation", offNotice)
	}

	start := strings.Index(page, "function renderReverse(")
	if start == -1 {
		t.Fatal("page has no renderReverse function")
	}
	end := strings.Index(page[start:], "\n  function renderFileReport(")
	if end == -1 {
		t.Fatal("could not find the end of renderReverse (renderFileReport must follow it)")
	}
	body := page[start : start+end]
	// Structural proof renderReverse's "off" branch actually pushes a
	// notice (mirroring its own "skipped" branch and renderFileReport's own
	// off/skipped branches) rather than silently no-op'ing with a
	// comment-only else, which is exactly what the pre-fix code did.
	if strings.Count(body, "notices.push") < 2 {
		t.Errorf("renderReverse must push a notice for BOTH the skipped case and the off/absent case, so the panel body is never left blank; got function body:\n%s", body)
	}
}

// TestWebUIPage_ReverseSkippedNoticeShowsStalenessTimestamp is the
// controller ruling's page-level pin (Phase 12 final round): "the
// skipped-reverse-pass overwrite is the highest-severity item —
// last-known-good preservation + a staleness indicator on the page
// ('showing last complete sweep from <time>')". Preservation on the DATA
// side (statsStore never clearing ReverseFindings on a skipped cycle) is
// not enough if the GUI never surfaces WHEN those findings are from — this
// pins that renderReverse reads reverseAsOf and renders the controller's own
// wording, using the same fmtRelative helper every other timestamp on the
// page already uses.
func TestWebUIPage_ReverseSkippedNoticeShowsStalenessTimestamp(t *testing.T) {
	page := string(webUIPage)

	start := strings.Index(page, "function renderReverse(")
	if start == -1 {
		t.Fatal("page has no renderReverse function")
	}
	end := strings.Index(page[start:], "\n  function renderFileReport(")
	if end == -1 {
		t.Fatal("could not find the end of renderReverse (renderFileReport must follow it)")
	}
	body := page[start : start+end]

	if !strings.Contains(body, "reverseAsOf") {
		t.Error("renderReverse never reads reverseAsOf; a skipped pass's preserved findings have no way to say when they are actually from")
	}
	if !strings.Contains(body, "showing last complete sweep from") {
		t.Error("renderReverse is missing the controller-mandated staleness copy \"showing last complete sweep from <time>\"")
	}
	if !strings.Contains(body, "fmtRelative(notice.asOf)") && !strings.Contains(body, "fmtRelative(") {
		t.Error("the staleness notice does not render the timestamp through fmtRelative, the same relative-time helper every other timestamp on the page uses")
	}
}

// TestWebUIPage_UnreachableInstanceBadge pins controller addition N2's GUI
// half: an instance whose most recent cycle could not even reach the
// decision engine must carry a visible, distinctly-colored (ALERT clay)
// badge on its shelf card — never silently show stale numbers with no
// marker that anything is wrong.
func TestWebUIPage_UnreachableInstanceBadge(t *testing.T) {
	page := string(webUIPage)
	if !strings.Contains(page, "lastCycleStatus") {
		t.Error("page never reads lastCycleStatus")
	}
	if !strings.Contains(page, "shelf-unreachable") {
		t.Error("page never defines a shelf-unreachable badge element/class")
	}
	// Round-4 review fix: the badge copy must not claim "couldn't reach"
	// specifically — LastCycleStatus.Status can now also be "skipped" for a
	// cycle that DID reach the instance but aborted INSIDE the engine before
	// completing an evaluation (a quality-profile fetch failure, an
	// exclusion-tag resolution failure), for which "couldn't reach" would be
	// a false statement. "last sweep incomplete" is accurate for all of
	// LastCycleStatus's skipped reasons (connectivity, library read, AND an
	// aborted evaluation).
	if !strings.Contains(page, "last sweep incomplete") {
		t.Error("page is missing the unreachable/incomplete-cycle badge copy")
	}
	if strings.Contains(page, "couldn't reach ") {
		t.Error("page still contains the old badge copy, which falsely claims connectivity failure even for a cycle that reached the instance but aborted mid-evaluation")
	}
	// The badge must use the ALERT clay token, not invent a new color.
	badgeRuleStart := strings.Index(page, ".shelf-unreachable")
	if badgeRuleStart == -1 {
		t.Fatal("no .shelf-unreachable CSS rule found")
	}
	if !strings.Contains(page, `el("span", "badge badge-alert shelf-unreachable")`) {
		t.Error("the unreachable badge element is not built with the badge-alert (ALERT clay) class")
	}
}

// TestWebUIPage_UnreachableBadgeWrapsInsteadOfOverflowing is round-4
// review's fix for the badge's overflow at the mandated 360px-wide minimum:
// the badge text is up to ~95 characters ("couldn't reach " + a long
// instance name + " last sweep — " + a full-sentence reason), rendered
// inside .badge (white-space: nowrap) inside .shelf-head (display: flex,
// no flex-wrap and no overflow anywhere in the chain) — a flex item's
// default min-width:auto resolves to its min-content, which for nowrap text
// is the WHOLE string, so the badge could neither shrink nor wrap and
// overflowed the shelf card (roughly 2x the 360px viewport). An *arr
// mid-restart is the ordinary real-world cause of this state, i.e. exactly
// when the page must stay readable.
func TestWebUIPage_UnreachableBadgeWrapsInsteadOfOverflowing(t *testing.T) {
	page := string(webUIPage)

	shelfHeadStart := strings.Index(page, ".shelf-head {")
	if shelfHeadStart == -1 {
		t.Fatal("no .shelf-head CSS rule found")
	}
	shelfHeadEnd := strings.Index(page[shelfHeadStart:], "}")
	if shelfHeadEnd == -1 {
		t.Fatal("could not find the end of the .shelf-head rule")
	}
	shelfHeadRule := page[shelfHeadStart : shelfHeadStart+shelfHeadEnd]
	if !strings.Contains(shelfHeadRule, "flex-wrap: wrap") {
		t.Error(".shelf-head does not allow its children to wrap onto a new line, so a long badge has nowhere to go but overflow")
	}

	unreachableStart := strings.Index(page, ".shelf-unreachable {")
	if unreachableStart == -1 {
		t.Fatal("no .shelf-unreachable CSS rule found")
	}
	unreachableEnd := strings.Index(page[unreachableStart:], "}")
	if unreachableEnd == -1 {
		t.Fatal("could not find the end of the .shelf-unreachable rule")
	}
	unreachableRule := page[unreachableStart : unreachableStart+unreachableEnd]
	if !strings.Contains(unreachableRule, "white-space: normal") {
		t.Error(".shelf-unreachable never overrides .badge's white-space: nowrap, so its text cannot wrap and will overflow at 360px")
	}
	if !strings.Contains(unreachableRule, "text-transform: none") {
		t.Error(".shelf-unreachable never overrides .badge's uppercase text-transform; a full sentence in 11px caps is unreadable")
	}
}

// --- [v2.2] the action switches on the wire ---------------------------------

// TestWebUIHandler_Stats_CarriesTheActionSwitches is what lets the page render
// a button DISABLED WITH ITS REASON rather than either hiding it (which reads
// as "this finding is not actionable") or showing it live and letting the
// click 403 (which reads as "cutoffarr is broken"). The page cannot know which
// switch is missing without being told, and rule 7 is explicit that it must
// never lie about which one it is.
func TestWebUIHandler_Stats_CarriesTheActionSwitches(t *testing.T) {
	store := newStatsStore(true)
	srv := &webUIServer{stats: store, scan: newScanCoordinator(),
		actions: newActionRunner(Config{GUIActions: true, ReverseScanRemonitor: false}, nil, store)}
	ts := httptest.NewServer(newWebUIHandler(srv))
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/stats")
	if err != nil {
		t.Fatalf("GET /api/stats: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if raw["guiActions"] != true {
		t.Errorf("guiActions = %v, want true", raw["guiActions"])
	}
	if raw["reverseScanRemonitor"] != false {
		t.Errorf("reverseScanRemonitor = %v, want false", raw["reverseScanRemonitor"])
	}
}

// TestWebUIHandler_Stats_NoActionRunnerReportsBothSwitchesOff keeps the
// pre-v2.2 shape honest: a server with no action system wired must report the
// switches as off rather than omitting them, or the page would fall back to
// its own default and could render a live-looking button on a deployment where
// every click is a 403.
func TestWebUIHandler_Stats_NoActionRunnerReportsBothSwitchesOff(t *testing.T) {
	ts, _ := newTestWebUIServer(t, newStatsStore(true))
	resp, err := http.Get(ts.URL + "/api/stats")
	if err != nil {
		t.Fatalf("GET /api/stats: %v", err)
	}
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	for _, key := range []string{"guiActions", "reverseScanRemonitor"} {
		v, present := raw[key]
		if !present {
			t.Errorf("%s is absent from the stats payload; the page must never have to guess a switch state", key)
		}
		if v != false {
			t.Errorf("%s = %v, want false when no action system is wired", key, v)
		}
	}
}

// TestActionEndpoint_TheTwoExitsBeforeTheRunnerAreLogged closes the one hole in
// rule 9's coverage of this endpoint: handleAction has two exits that answer
// WITHOUT reaching the runner, and neither used to write a log line at any
// level, while every exit the runner owns produces a full msg=action line.
//
// On the one unauthenticated endpoint in this program that can move files, that
// silence hid two very different things at once — a burst of malformed probes
// against the port, and an operator's "I click the button and nothing happens"
// on a deployment where no action system is wired at all. Both are now one line
// each, at WARN, carrying the status that was actually returned.
func TestActionEndpoint_TheTwoExitsBeforeTheRunnerAreLogged(t *testing.T) {
	const body = `{"kind":"trash","confirm":true,"instance":"radarr-main","path":"/x","finding":"duplicate","size":10}`

	t.Run("no action system wired", func(t *testing.T) {
		logger, buf := newActionTestLogger()
		store := newStatsStore(true)
		ts := httptest.NewServer(newWebUIHandler(&webUIServer{
			stats: store, scan: newScanCoordinator(), logger: logger,
		}))
		t.Cleanup(ts.Close)

		status, out := postAction(t, ts, body)
		if status != http.StatusForbidden || out.Outcome != actionOutcomeDisabled {
			t.Fatalf("status=%d outcome=%q, want 403/disabled", status, out.Outcome)
		}
		log := buf.String()
		for _, want := range []string{"level=WARN", "msg=action", "source=gui", "outcome=" + actionOutcomeDisabled, "status=403"} {
			if !strings.Contains(log, want) {
				t.Errorf("the no-runner exit's log line is missing %q; an operator whose buttons do nothing must be able to find out why from the log. Log:\n%s", want, log)
			}
		}
	})

	t.Run("a body that cannot be read", func(t *testing.T) {
		logger, buf := newActionTestLogger()
		store := newStatsStore(true)
		ts := httptest.NewServer(newWebUIHandler(&webUIServer{
			stats: store, scan: newScanCoordinator(), logger: logger,
			actions: newActionRunner(Config{GUIActions: true}, logger, store),
		}))
		t.Cleanup(ts.Close)

		status, out := postAction(t, ts, `{not json`)
		if status != http.StatusBadRequest || out.Outcome != actionOutcomeRefused {
			t.Fatalf("status=%d outcome=%q, want 400/refused", status, out.Outcome)
		}
		log := buf.String()
		for _, want := range []string{"level=WARN", "source=gui", "status=400", "error="} {
			if !strings.Contains(log, want) {
				t.Errorf("the undecodable-body exit's log line is missing %q; a burst of malformed probes at this endpoint must not be invisible. Log:\n%s", want, log)
			}
		}
	})
}

// TestWebUIServer_ADeadlineThatCannotBeSetIsNotADebugLine is the second half of
// the same finding. If SetWriteDeadline fails in production the action silently
// reverts to the server's 30s WriteTimeout — the exact mid-move connection
// teardown actionDeadline exists to prevent, on a request that may be renaming
// files — and at Debug that reverting is invisible on every deployment that
// does not run with debug logging, which is all of them.
func TestWebUIServer_ADeadlineThatCannotBeSetIsNotADebugLine(t *testing.T) {
	src := webUIGoSource(t)
	call := strings.Index(src, "SetWriteDeadline(")
	if call == -1 {
		t.Fatal("webui.go no longer sets a write deadline on the action response")
	}
	// The if-block the failure is handled in, whatever length its comment
	// grows to: from the call to the first line that closes it at one tab.
	block := src[call:]
	if end := strings.Index(block, "\n\t}\n"); end != -1 {
		block = block[:end]
	}
	if strings.Contains(block, ".Debug(") || strings.Contains(block, "logDebug(") {
		t.Errorf("the deadline failure is still logged at Debug, where a production deployment never sees it:\n%s", block)
	}
	if !strings.Contains(block, "logWarn(") {
		t.Errorf("the deadline failure must be logged at Warn; got:\n%s", block)
	}
}

// webUIGoSource reads webui.go itself. The two properties above are about the
// LEVEL a line is emitted at on a path that needs a real ResponseWriter failure
// to reach, which no httptest recorder can be made to produce reliably; the
// source is where the level is decided and is therefore what is pinned.
func webUIGoSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("webui.go")
	if err != nil {
		t.Fatalf("reading webui.go: %v", err)
	}
	return string(b)
}

// --- [v2.2] GUI action structural pins --------------------------------------

// TestWebUIPage_ActionButtonsStateTheirExactOperation is rule 2, pinned
// against the page's own source. A button labelled "Fix" or "Clean up" is the
// single failure this whole design is built to prevent: the operator has to be
// able to read what will happen BEFORE they click, in enough detail to catch
// cutoffarr having identified the wrong thing.
func TestWebUIPage_ActionButtonsStateTheirExactOperation(t *testing.T) {
	page := string(webUIPage)
	// The three operation phrasings, each matching the server's own
	// operation text word for word (actions.go's trashOperationText,
	// twinOperationText, remonitorOperationText).
	for _, want := range []string{`"Move to trash — "`, `"Re-monitor — "`, `Merge \"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page never builds the operation label %s; a button that does not state its exact operation is exactly what rule 2 forbids", want)
		}
	}
	// The trash button must state the SIZE, which is what tells a stray
	// sample from the real episode.
	if !strings.Contains(page, "humanSize(r.f.size)") {
		t.Error("the trash button's label never includes the file's size")
	}
	for _, banned := range []string{`text(btn, "Fix")`, `text(btn, "Clean up")`, `text(btn, "Delete")`} {
		if strings.Contains(page, banned) {
			t.Errorf("the page contains a bare %s button", banned)
		}
	}
}

// TestWebUIPage_EveryActionGoesThroughAConfirmGate is the second half of rule
// 2: the confirm step re-shows the full operation before anything fires. It is
// pinned structurally because the failure mode — a click that acts
// immediately — is invisible in review and irreversible in use.
func TestWebUIPage_EveryActionGoesThroughAConfirmGate(t *testing.T) {
	page := string(webUIPage)
	start := strings.Index(page, "function buildActionButton(")
	if start == -1 {
		t.Fatal("page has no buildActionButton")
	}
	end := strings.Index(page[start:], "\n  function reverseActionLabel(")
	if end == -1 {
		t.Fatal("could not find the end of buildActionButton")
	}
	fn := page[start : start+end]

	confirmAt := strings.Index(fn, "window.confirm(")
	postAt := strings.Index(fn, "postAction(")
	if confirmAt == -1 {
		t.Fatal("buildActionButton never calls window.confirm: an action must never fire on a bare click")
	}
	if postAt == -1 {
		t.Fatal("buildActionButton never posts the action")
	}
	if confirmAt > postAt {
		t.Error("buildActionButton posts the action before confirming it")
	}
	if !strings.Contains(fn, "opts.label + ") {
		t.Error("the confirm dialog must re-show the FULL operation (opts.label), not a generic 'are you sure'")
	}
	// The bulk action has its own confirm, listing every item.
	bulk := page[strings.Index(page, "function buildBulkRemonitor("):]
	if !strings.Contains(bulk[:strings.Index(bulk, "\n  function ")], "window.confirm(") {
		t.Error("the bulk re-monitor never confirms")
	}
}

// TestWebUIPage_DisabledButtonsCarryTheirReasonTwice is rule 7's GUI half: a
// switched-off button is disabled with its reason, never hidden — and the
// reason travels in both the title attribute and an inline note, because a
// title alone is invisible on a touch screen.
func TestWebUIPage_DisabledButtonsCarryTheirReasonTwice(t *testing.T) {
	page := string(webUIPage)
	if !strings.Contains(page, "function actionDisabledReason(") {
		t.Fatal("page has no actionDisabledReason")
	}
	for _, want := range []string{"gui_actions is false", "reverse_scan_remonitor is false"} {
		if !strings.Contains(page, want) {
			t.Errorf("the disabled reason never names the switch %q; rule 7 requires the page never lie about which switch is missing", want)
		}
	}
	start := strings.Index(page, "function buildActionButton(")
	fn := page[start : start+strings.Index(page[start:], "\n  function reverseActionLabel(")]
	if !strings.Contains(fn, "btn.disabled = true") || !strings.Contains(fn, "btn.title = reason") || !strings.Contains(fn, "text(note, reason)") {
		t.Errorf("a disabled button must carry its reason as BOTH title and inline text; got:\n%s", fn)
	}
}

// TestWebUIPage_RowsUpdateFromTheResponseNotFromTheNextSweep pins that a row
// reflects what actually happened, from the answer, immediately.
func TestWebUIPage_RowsUpdateFromTheResponseNotFromTheNextSweep(t *testing.T) {
	page := string(webUIPage)
	if !strings.Contains(page, "function renderActionOutcome(") {
		t.Fatal("page has no renderActionOutcome")
	}
	for _, want := range []string{"action-performed", "action-note-refused", "action-note-rehearsed", "resp.items"} {
		if !strings.Contains(page, want) {
			t.Errorf("renderActionOutcome never handles %q: performed, refused, rehearsed and the merge's itemized collisions must each be visible on the row", want)
		}
	}
	if !strings.Contains(page, "tr.action-performed td") {
		t.Error("a performed row is never struck through; the operator has to see that the row they clicked is the one that changed")
	}
}

// TestWebUIPage_BulkActionIsRemonitorOnly is the phase's own scope boundary,
// pinned so a later round cannot quietly add a bulk file action: moving many
// files on one click is a different decision from moving one, and it has not
// been ruled on.
func TestWebUIPage_BulkActionIsRemonitorOnly(t *testing.T) {
	page := string(webUIPage)
	if !strings.Contains(page, "Re-monitor all ") {
		t.Error("the page has no bulk re-monitor")
	}
	for _, banned := range []string{"Trash all ", "Merge all ", "Move all "} {
		if strings.Contains(page, banned) {
			t.Errorf("the page offers a bulk file action (%q): file actions stay per-item in this phase", banned)
		}
	}
}

// TestWebUIPage_ActionButtonsUseTheEstablishedTokens keeps the buttons inside
// the page's own five-token palette — clay for anything that moves a file,
// sage for the re-monitor, which only flips a flag.
func TestWebUIPage_ActionButtonsUseTheEstablishedTokens(t *testing.T) {
	page := string(webUIPage)
	if !strings.Contains(page, ".action-clay { color: var(--alert)") {
		t.Error("the destructive action tone is not the page's own clay/--alert token")
	}
	if !strings.Contains(page, ".action-sage { color: var(--rest)") {
		t.Error("the re-monitor action tone is not the page's own sage/--rest token")
	}
	if !strings.Contains(page, `tone: "clay"`) || !strings.Contains(page, `tone: "sage"`) {
		t.Error("the two tones are never actually used by the button builders")
	}
	// The re-monitor must not be clay: it moves nothing and undoes nothing
	// the next sweep could not redo.
	start := strings.Index(page, "kind: \"remonitor\",")
	if start == -1 {
		t.Fatal("no re-monitor button is built")
	}
	if !strings.Contains(page[start:start+80], `tone: "sage"`) {
		t.Error("the re-monitor button is not sage; clay is reserved for operations that move a file")
	}
}

// TestWebUIPage_TrashButtonSendsTheFindingsIdentifyingFields pins that the
// request carries what the server needs to re-derive the finding and to check
// the promise the button made — in particular the size, without which a file
// REPLACED at the same path passes every check there is.
func TestWebUIPage_TrashButtonSendsTheFindingsIdentifyingFields(t *testing.T) {
	page := string(webUIPage)
	start := strings.Index(page, `kind: "trash", confirm: true,`)
	if start == -1 {
		t.Fatal("the trash button builds no request body")
	}
	body := page[start : start+240]
	for _, want := range []string{"instance:", "path:", "finding:", "size:"} {
		if !strings.Contains(body, want) {
			t.Errorf("the trash request body is missing %s; got %q", want, body)
		}
	}
}

// TestWebUIPage_ActionRequestsAreSentAsSameOriginJSON pins the CLIENT half of
// the cross-site guard, which was a one-sided contract until this round.
//
// crossSiteRefusal (actions.go) makes `Content-Type: application/json` a hard
// precondition for every action: a request without it is a 400 before any gate
// runs, because a cross-site form post cannot set that header. The server-side
// test proves the server ACCEPTS that shape; nothing proved the page PRODUCES
// it. Deleting or editing the page's one Content-Type line therefore made every
// action button on every surface answer 400 while `go test ./...`, `-race` and
// `node --check` all stayed green — a guard that costs the real client nothing
// only as long as the real client keeps sending it.
//
// The assertion is scoped to postAction's body rather than to the page, so it
// is about the fetch that actually runs (see jsFunctionBody).
func TestWebUIPage_ActionRequestsAreSentAsSameOriginJSON(t *testing.T) {
	page := string(webUIPage)
	post := jsFunctionBody(t, page, "postAction", "renderActionOutcome")
	for _, want := range []string{`"/api/action"`, `method: "POST"`, `"Content-Type": "application/json"`} {
		if !strings.Contains(post, want) {
			t.Errorf("postAction() no longer sends %s, so every action button answers 400 at crossSiteRefusal; the function is:\n%s", want, post)
		}
	}
}

// TestWebUIPage_ActionOutcomesSurviveARepaint is the round-2 IMPORTANT on the
// front end: renderFileReport and renderReverse each begin by clearing their
// table (body.textContent = ""), and refresh() calls render(data) on a 30s
// timer. Every trace of an action's result — the strike-through, the clay
// refusal note, the merge's itemized collisions — was therefore wiped within
// one poll, and the row came back from the still-stale snapshot with a fully
// live "Move to trash — …" button for a file that is already in the trash.
//
// The brief requires the row to update from the response "without waiting for
// the next sweep"; holding that state for at most one poll does not satisfy it,
// and a re-armed button reads to the operator as "the action did not work".
// Each assertion below is scoped to the function that has to make the CALL,
// which is the round-3 review fix: the previous version looked for
// "rememberedActionOutcome(" anywhere in the page, and the DECLARATION satisfied
// that. Deleting the one call inside buildActionButton — re-introducing the
// exact defect this test is named for — left it green, and `node --check` does
// not notice an uncalled function either.
func TestWebUIPage_ActionOutcomesSurviveARepaint(t *testing.T) {
	page := string(webUIPage)
	if !strings.Contains(page, "var actionOutcomes") {
		t.Fatal("the page keeps no memory of what a click answered, so every repaint erases it")
	}
	if !strings.Contains(page, "function actionKey(") {
		t.Error("there is no per-finding identity to key an outcome by; instance+kind+path / instance+id+season is what a rebuilt row has to match on")
	}

	// The read side: a rebuilt row consults the memory, keyed off the very body
	// it would POST, BEFORE it decides anything about the button.
	build := jsFunctionBody(t, page, "buildActionButton", "attachActionClick")
	if !strings.Contains(build, "rememberedActionOutcome(opts.body)") {
		t.Error("buildActionButton never consults the remembered outcome, so a rebuilt row re-arms its button and loses its note — the poll erases the result of the click")
	}
	if !strings.Contains(build, "renderActionOutcome(note, opts.container, remembered)") {
		t.Error("buildActionButton looks the outcome up and then does not render it")
	}
	rememberedAt := strings.Index(build, "rememberedActionOutcome(opts.body)")
	reasonAt := strings.Index(build, "var reason = opts.ineligible")
	if rememberedAt == -1 || reasonAt == -1 || rememberedAt > reasonAt {
		t.Error("the remembered outcome must be applied before the disabled-reason path returns, or a performed action on an ineligible-for-other-reasons row is never shown")
	}

	// The write side: the memory is written from the answer, not from optimism.
	click := jsFunctionBody(t, page, "attachActionClick", "reverseActionLabel")
	if !strings.Contains(click, "rememberActionOutcome(") {
		t.Error("a click's answer is never remembered, so the next 30s poll erases it")
	}

	// The expiry side: pruning is driven by the snapshot, from render().
	render := jsFunctionBody(t, page, "render", "")
	if !strings.Contains(render, "pruneActionOutcomes(instances)") {
		t.Error("render never prunes remembered outcomes, so a row keeps its strike-through long after the finding stopped appearing in the snapshot")
	}
	pruneAt := strings.Index(render, "pruneActionOutcomes(instances)")
	renderReverseAt := strings.Index(render, "renderReverse(instances)")
	if renderReverseAt == -1 || pruneAt > renderReverseAt {
		t.Error("pruning must happen before the tables are rebuilt, or the rows are rebuilt from a memory this snapshot has already contradicted")
	}
}

// TestWebUIPage_BulkRemonitorSummarySurvivesItsOwnRerender is the same rule for
// the one bulk action: buildBulkRemonitor wrote its summary into `note` and
// then called rerender(), whose first act is body.textContent = "" — destroying
// the summary in the same tick, so it was never visible at all.
//
// Scoped to the call sites for the same reason as the test above: the previous
// version searched the whole page for "bulkRemonitorSummary", which the
// `var bulkRemonitorSummary = null;` declaration alone satisfied.
func TestWebUIPage_BulkRemonitorSummarySurvivesItsOwnRerender(t *testing.T) {
	page := string(webUIPage)
	if !strings.Contains(page, "var bulkRemonitorSummary = null;") {
		t.Fatal("the bulk summary is not kept anywhere, so the rerender that follows it destroys it in the same tick")
	}
	bulk := jsFunctionBody(t, page, "buildBulkRemonitor", "buildCaseCollisionBlock")

	applyAt := strings.Index(bulk, "text(note, bulkRemonitorSummary.text)")
	if applyAt == -1 {
		t.Fatal("the rebuilt bulk control never re-applies the summary it stored, so the rerender it triggers still destroys it")
	}
	// It has to be re-applied BEFORE the disabled-reason early return, or the
	// summary vanishes the moment a switch is off — which is precisely when an
	// operator most needs to read what just happened.
	returnAt := strings.Index(bulk, `var reason = actionDisabledReason("remonitor")`)
	if returnAt == -1 {
		t.Fatal("buildBulkRemonitor no longer has its disabled-reason path")
	}
	if applyAt > returnAt {
		t.Error("the summary is re-applied after the disabled-reason early return, so it is never shown when a switch is off")
	}
	if !strings.Contains(bulk, "bulkRemonitorSummary = {") {
		t.Error("the run never stores a summary, so there is nothing for the rerender to bring back")
	}
	storeAt := strings.Index(bulk, "bulkRemonitorSummary = {")
	rerenderAt := strings.Index(bulk[storeAt:], "rerender()")
	if rerenderAt == -1 {
		t.Error("the bulk run never triggers the rerender that brings the per-row outcomes back")
	}
}

// TestWebUIPage_BulkRemonitorNeverCallsAFailureARefusal is the honesty half of
// the same finding: the loop counted a 502 `failed` and a network error
// identically to a refusal ("N answered yes, M were refused or rehearsed"),
// asserting a decision where the outcome is genuinely unknown.
//
// The round-3 fix is that this now checks the COUNTER, not the sentence: the
// old version only proved the string "failed or unknown" existed somewhere,
// which stayed true with `unknown` stuck at zero forever and every failure
// silently counted as a refusal again.
func TestWebUIPage_BulkRemonitorNeverCallsAFailureARefusal(t *testing.T) {
	page := string(webUIPage)
	bulk := jsFunctionBody(t, page, "buildBulkRemonitor", "buildCaseCollisionBlock")
	if !strings.Contains(bulk, "failed or unknown") {
		t.Error("a failed or unreachable bulk item is still reported as refused; those are different outcomes and one of them means go and look at the server")
	}
	if !strings.Contains(bulk, "var performed = 0, declined = 0, unknown = 0;") {
		t.Fatal("the bulk loop no longer keeps three separate counters")
	}
	// The two paths that must reach `unknown`: an answer whose outcome is
	// neither performed nor one of the three decided ones (a 502 failed), and a
	// request that never answered at all.
	if !strings.Contains(bulk, "else unknown++;") {
		t.Error("an answer that is not performed/refused/rehearsed/disabled is not counted as unknown, so a 502 `failed` is being reported as a refusal — a decision cutoffarr did not make")
	}
	catchAt := strings.Index(bulk, ".catch(function ()")
	if catchAt == -1 {
		t.Fatal("the bulk loop has no catch, so one network error stops it mid-run")
	}
	if !strings.Contains(bulk[catchAt:], "unknown++") {
		t.Error("a request that never answered is not counted as unknown; a network error is the definition of an outcome nobody knows")
	}
}

// jsFunctionBody slices one function out of webui.html: from `function name(`
// to the declaration of `until` (or to the next top-level function, when until
// is empty). Assertions made against a slice are assertions about the code that
// has to RUN — a whole-page substring search is satisfied by a declaration, and
// an uncalled function is exactly the defect these tests exist to catch.
func jsFunctionBody(t *testing.T, page, name, until string) string {
	t.Helper()
	start := strings.Index(page, "function "+name+"(")
	if start == -1 {
		t.Fatalf("webui.html no longer declares %s(), so this pin is looking at nothing", name)
	}
	rest := page[start:]
	end := -1
	if until != "" {
		end = strings.Index(rest, "\n  function "+until+"(")
		if end == -1 {
			t.Fatalf("could not find the end of %s(): %s() no longer follows it", name, until)
		}
	} else {
		end = strings.Index(rest, "\n  function ")
		if end == -1 {
			t.Fatalf("could not find the end of %s()", name)
		}
	}
	return rest[:end]
}
