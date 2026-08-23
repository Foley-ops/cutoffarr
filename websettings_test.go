package main

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// runPageFunctionUnderNode, pageFunctionSource, and pageFunctionBody are
// defined in statecache_test.go/scanprogress_test.go and reused here rather
// than reimplemented.

// websettings_test.go covers v0.3.0: the wide fluid layout + instance grid,
// the light/dark token migration, the settings panel (gear button + native
// <dialog>), and the REHEARSING/live badge tooltips. It follows the same
// structural-pin conventions as webui_test.go/scanprogress_test.go — no
// browser, so most assertions read the embedded page's own HTML/CSS/JS
// source rather than rendering it; jsBracedBlockAfter and pageFunctionBody
// (scanprogress_test.go) are reused rather than re-implemented.

// --- 1. wide layout ----------------------------------------------------

// TestWebUIPage_WideLayoutFluidContainerAndInstanceGrid pins the brief's own
// two numbers: the fluid container (min(1400px, 94vw)) and the ≥1100px
// instance grid, while requiring the narrow (360px-and-up) flex stack to
// stay the unconditional base rule the grid only ever overrides.
func TestWebUIPage_WideLayoutFluidContainerAndInstanceGrid(t *testing.T) {
	page := string(webUIPage)

	wrapRule := jsBracedBlockAfter(t, page, ".wrap {")
	if !strings.Contains(wrapRule, "min(1400px, 94vw)") {
		t.Errorf(".wrap does not use the mandated fluid container width, got: %s", wrapRule)
	}

	baseStart := strings.Index(page, "#instances {")
	gridMarkerStart := strings.Index(page, "@media (min-width: 1100px)")
	if baseStart == -1 {
		t.Fatal("page has no base #instances rule")
	}
	if gridMarkerStart == -1 {
		t.Fatal("page has no @media (min-width: 1100px) wide-layout block")
	}
	if baseStart > gridMarkerStart {
		t.Fatal("the base #instances rule does not come before the wide-layout override")
	}
	baseRule := jsBracedBlockAfter(t, page, "#instances {")
	if !strings.Contains(baseRule, "display: flex") {
		t.Error("the base #instances rule is no longer a flex column; narrow-viewport (360px) stacking must stay unconditional")
	}

	gridBlock := jsBracedBlockAfter(t, page, "@media (min-width: 1100px)")
	if !strings.Contains(gridBlock, "#instances") {
		t.Error("the wide-layout media block never targets #instances")
	}
	if !strings.Contains(gridBlock, "display: grid") {
		t.Error("the wide-layout media block does not switch #instances to a grid")
	}
	if !strings.Contains(gridBlock, "auto-fit") {
		t.Error("the wide-layout grid does not use auto-fit, so a third instance could not wrap onto its own row")
	}
	if !strings.Contains(gridBlock, "grid-template-columns") {
		t.Error("the wide-layout media block never sets grid-template-columns")
	}
}

// TestWebUIPage_WidthSensitivePinsSurviveTheWideLayout re-verifies the three
// width-sensitive rules the brief calls out by name after the container
// change: the path column's own width priority over the action cell, the
// UNREACHABLE badge's wrap behavior, and the shelf-count hero clamp
// mechanism — none of which the wide layout is allowed to have touched.
func TestWebUIPage_WidthSensitivePinsSurviveTheWideLayout(t *testing.T) {
	page := string(webUIPage)

	pathRule := jsBracedBlockAfter(t, page, "table.findings td.path {")
	if !strings.Contains(pathRule, "min-width") {
		t.Error("table.findings td.path lost its min-width floor")
	}
	actionRule := jsBracedBlockAfter(t, page, "table.findings td.action-cell {")
	if !strings.Contains(actionRule, "max-width") {
		t.Error("table.findings td.action-cell lost its max-width cap")
	}

	shelfHeadRule := jsBracedBlockAfter(t, page, ".shelf-head {")
	if !strings.Contains(shelfHeadRule, "flex-wrap: wrap") {
		t.Error(".shelf-head lost its flex-wrap: wrap, so the UNREACHABLE badge has nowhere to go but overflow")
	}
	unreachableRule := jsBracedBlockAfter(t, page, ".shelf-unreachable {")
	if !strings.Contains(unreachableRule, "white-space: normal") || !strings.Contains(unreachableRule, "text-transform: none") {
		t.Error(".shelf-unreachable lost its wrap-enabling override")
	}

	body := pageFunctionBody(t, "updateShelfCard")
	if !strings.Contains(body, "offsetWidth") || !strings.Contains(body, "Math.max") || !strings.Contains(body, "Math.min") {
		t.Error("updateShelfCard's hero-number edge clamp is no longer present")
	}
}

// --- 2. cutoff-marker divider (now binding) -----------------------------

// TestWebUIPage_CutoffMarkerDividerIsBindingAndPresent pins the brief's own
// escalation: the 2px sage/amber divider is no longer decoration, it is the
// pair's only secondary encoding and now a binding design rule.
func TestWebUIPage_CutoffMarkerDividerIsBindingAndPresent(t *testing.T) {
	page := string(webUIPage)
	rule := jsBracedBlockAfter(t, page, ".shelf-marker {")
	if !strings.Contains(rule, "width: 2px") {
		t.Error(".shelf-marker is no longer a 2px divider — this is now a BINDING design rule (the sage/amber pair's only secondary encoding) and must not shrink to decoration")
	}
}

// --- 3. light mode: token migration + validation record -----------------

// TestWebUIPage_TokenMigrationNoRawPaletteColorsOutsideTokenBlocks is the
// brief's own grep pin: every hex (and, more strictly, every rgb(...))
// palette literal in the stylesheet must live inside the contiguous token
// region — the bare :root block through the last :root[data-theme="dark"]
// override — never introduced fresh in a component rule.
func TestWebUIPage_TokenMigrationNoRawPaletteColorsOutsideTokenBlocks(t *testing.T) {
	page := string(webUIPage)
	styleStart := strings.Index(page, "<style>")
	styleEnd := strings.Index(page, "</style>")
	if styleStart == -1 || styleEnd == -1 || styleEnd < styleStart {
		t.Fatal("page has no <style>...</style> block")
	}
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(page[styleStart+len("<style>"):styleEnd], "")

	tokenStart := strings.Index(css, ":root {")
	if tokenStart == -1 {
		t.Fatal("css has no bare :root token block")
	}
	lastDarkMarker := `:root[data-theme="dark"]`
	lastDark := strings.LastIndex(css, lastDarkMarker)
	if lastDark == -1 {
		t.Fatalf("css has no %s override block", lastDarkMarker)
	}
	tokenEndRule := jsBracedBlockAfter(t, css[lastDark:], lastDarkMarker)
	tokenEnd := lastDark + len(tokenEndRule)

	hexRe := regexp.MustCompile(`#[0-9A-Fa-f]{3,8}\b`)
	for _, region := range []struct {
		name string
		text string
	}{
		{"before the token region", css[:tokenStart]},
		{"after the token region", css[tokenEnd:]},
	} {
		if loc := hexRe.FindString(region.text); loc != "" {
			t.Errorf("found a raw hex color %q %s — every color outside the token blocks must be a var(--...) reference", loc, region.name)
		}
		if strings.Contains(region.text, "rgb(") {
			t.Errorf("found a raw rgb(...) color %s — every derived shade (dim/border/hover) must be a var(--...) reference, not a literal recomputed outside the token blocks", region.name)
		}
	}
}

// TestWebUIPage_ThemeAttributeSwitchDefinesBothPalettes pins the mechanism:
// a bare :root DARK default, a prefers-color-scheme:light override guarded
// against an explicit data-theme="dark", and both explicit
// data-theme="light"/"dark" overrides — plus that JS actually flips the
// attribute rather than leaving unused CSS behind.
func TestWebUIPage_ThemeAttributeSwitchDefinesBothPalettes(t *testing.T) {
	page := string(webUIPage)
	for _, want := range []string{
		`@media (prefers-color-scheme: light)`,
		`:root:not([data-theme="dark"])`,
		`:root[data-theme="light"]`,
		`:root[data-theme="dark"]`,
		"#5FAE8C", "#D4923F", "#C4604E", // DARK rest/hunt/alert
		"#22795D", "#A96F10", "#A63A2C", // LIGHT rest/hunt/alert
		"#F4F6F5", "#FDFDFC", "#202B28", // LIGHT bg/panel/ink
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}

	if !strings.Contains(page, `document.documentElement.setAttribute("data-theme"`) {
		t.Error("no JS ever sets the data-theme attribute")
	}
	if !strings.Contains(page, `document.documentElement.removeAttribute("data-theme")`) {
		t.Error(`no JS ever clears data-theme for the "System" choice`)
	}
}

// TestWebUIPage_ValidationRecordDocumentedInTokenBlock pins the brief's own
// binding requirement: since the dataviz validator is not wired into CI,
// the CVD validation record and its two judged exemptions (including the
// amber/clay adjacency rule) must be documented as a comment beside the
// tokens they describe.
func TestWebUIPage_ValidationRecordDocumentedInTokenBlock(t *testing.T) {
	page := string(webUIPage)
	tokenStart := strings.Index(page, ":root {")
	styleStart := strings.Index(page, "<style>")
	if tokenStart == -1 || styleStart == -1 || tokenStart < styleStart {
		t.Fatal("could not locate the token block's own leading comment")
	}
	comment := page[styleStart:tokenStart]
	for _, want := range []string{
		"protan",
		"9.3",
		"8.4",
		"BINDING design rule",
		"chroma floor",
		"sub-floor",
		"MUST NEVER become adjacent",
	} {
		if !strings.Contains(comment, want) {
			t.Errorf("the token block's leading comment is missing %q — the validation record and its judged exemptions must be documented since the dataviz validator is not wired into CI", want)
		}
	}
}

// TestWebUIPage_MutedInkTextMeetsWCAGAA pins the brief's binding "text at
// WCAG AA via the ink tokens (spot-check the muted inks)" clause.
// --ink-faint measures ~3.0:1 in light mode (~2.9:1 dark) — below AA's
// 4.5:1 for normal text — so it must never color text a reader needs in
// order to use the page. [v0.3.0 review fix, round 3] Three call sites that
// DO carry primary/required text used --ink-faint: a findings row's own
// path (table.findings td.path — the findings tables' real payload), the
// write-safety switches' own required "config-file-only" explanation
// (.switch-why — brief item 2's "one sentence each stating they are
// config-file-only and why"), and each settings group's own label
// (.settings-group legend/h3). All three now use --ink-dim instead, which
// clears AA (4.5:1+) against both --panel and --bg in both modes. The token
// block's own leading comment documents this AA-scope split beside the CVD
// exemptions already there.
func TestWebUIPage_MutedInkTextMeetsWCAGAA(t *testing.T) {
	page := string(webUIPage)

	pathRule := jsBracedBlockAfter(t, page, "table.findings td.path {")
	if strings.Contains(pathRule, "var(--ink-faint)") {
		t.Errorf("table.findings td.path still colors a finding's own path (the findings tables' real payload) with --ink-faint, which fails WCAG AA (~3.0:1 light / ~2.9:1 dark):\n%s", pathRule)
	}
	if !strings.Contains(pathRule, "var(--ink-dim)") {
		t.Errorf("table.findings td.path never falls back to --ink-dim (AA-compliant in both modes):\n%s", pathRule)
	}

	switchWhyRule := jsBracedBlockAfter(t, page, ".switch-why {")
	if strings.Contains(switchWhyRule, "var(--ink-faint)") {
		t.Errorf(".switch-why (the write-safety switches' own required config-file-only explanation) still uses --ink-faint, which fails WCAG AA:\n%s", switchWhyRule)
	}
	if !strings.Contains(switchWhyRule, "var(--ink-dim)") {
		t.Errorf(".switch-why never falls back to --ink-dim (AA-compliant in both modes):\n%s", switchWhyRule)
	}

	legendRule := jsBracedBlockAfter(t, page, ".settings-group legend, .settings-group h3 {")
	if strings.Contains(legendRule, "var(--ink-faint)") {
		t.Errorf(".settings-group legend/h3 still uses --ink-faint, which fails WCAG AA:\n%s", legendRule)
	}
	if !strings.Contains(legendRule, "var(--ink-dim)") {
		t.Errorf(".settings-group legend/h3 never falls back to --ink-dim (AA-compliant in both modes):\n%s", legendRule)
	}

	tokenStart := strings.Index(page, ":root {")
	styleStart := strings.Index(page, "<style>")
	if tokenStart == -1 || styleStart == -1 || tokenStart < styleStart {
		t.Fatal("could not locate the token block's own leading comment")
	}
	comment := page[styleStart:tokenStart]
	if !strings.Contains(comment, "MUTED-INK TEXT AA SCOPE") {
		t.Error("the token block's leading comment never documents the muted-ink AA scope split (--ink-dim for required text, --ink-faint reserved for de-emphasized/inactive states)")
	}
}

// TestWebUIPage_MarkVsSurfaceContrastTokensAreDistinctPerMode is a
// lightweight sanity pin that the light and dark mark tokens are actually
// DIFFERENT values (not an accidental duplicate/inversion bug) — the real
// contrast validation is the hand-run record above, this just guards
// against a copy-paste mistake between the two blocks.
func TestWebUIPage_MarkVsSurfaceContrastTokensAreDistinctPerMode(t *testing.T) {
	page := string(webUIPage)
	darkBlock := jsBracedBlockAfter(t, page, `:root[data-theme="dark"] {`)
	lightBlock := jsBracedBlockAfter(t, page, `:root[data-theme="light"] {`)
	for _, tok := range []string{"--bg:", "--panel:", "--ink:", "--rest:", "--hunt:", "--alert:"} {
		dv := valueAfterToken(t, darkBlock, tok)
		lv := valueAfterToken(t, lightBlock, tok)
		if dv == "" || lv == "" {
			t.Fatalf("could not read %s from both theme blocks (dark=%q light=%q)", tok, dv, lv)
		}
		if dv == lv {
			t.Errorf("%s is identical (%s) between the dark and light explicit theme blocks; light mode is supposed to be designed, not inverted or copy-pasted", tok, dv)
		}
	}
}

// TestWebUIPage_DuplicatedThemeTokenBlocksStayInSyncTokenForToken pins the
// two places each theme's token set is written out in full. LIGHT lives
// twice — once inside `@media (prefers-color-scheme: light)
// { :root:not([data-theme="dark"]) { ... } }` (the system-default path) and
// once in the explicit `:root[data-theme="light"] { ... }` override — and
// DARK likewise lives twice (the bare `:root { ... }` block and the explicit
// `:root[data-theme="dark"] { ... }` override). Only the explicit blocks were
// previously pinned (TestWebUIPage_MarkVsSurfaceContrastTokensAreDistinctPerMode,
// TestWebUIPage_ThemeAttributeSwitchDefinesBothPalettes's "hex appears
// somewhere" check): editing ONE copy of a pair and not the other — e.g.
// reverting --rest in the media-query block while leaving the explicit block
// alone — left the suite green while every reader on the DEFAULT theme
// setting ("System" + an OS light/dark preference, i.e. the media-query
// path) got un-validated colors nobody exercising only the explicit
// data-theme paths would ever notice.
func TestWebUIPage_DuplicatedThemeTokenBlocksStayInSyncTokenForToken(t *testing.T) {
	page := string(webUIPage)

	// assertOverrideMatchesBase checks every token the (smaller) override
	// block declares against the same key's value in the (larger) base
	// block. It is deliberately NOT a check that the two blocks declare an
	// identical SET: the bare :root block also carries mode-agnostic tokens
	// (--mono, --sans, --backdrop, --shelf-count-size) that no per-theme
	// override needs to restate, so subset containment — rather than exact
	// set equality — is the real invariant for the dark pair. For the light
	// pair the two sets happen to be equal in practice, which this still
	// verifies (every key in one exists with the same value in the other).
	assertOverrideMatchesBase := func(t *testing.T, label, overrideHeader, baseHeader string) {
		t.Helper()
		overrideBlock := jsBracedBlockAfter(t, page, overrideHeader)
		baseBlock := jsBracedBlockAfter(t, page, baseHeader)
		override := cssTokenMap(overrideBlock)
		base := cssTokenMap(baseBlock)
		if len(override) < 10 {
			t.Fatalf("%s: found only %d --token: value; declarations in %q; expected at least 10", label, len(override), overrideHeader)
		}
		for name, ov := range override {
			bv, ok := base[name]
			if !ok {
				t.Errorf("%s: %s declares %s but %s does not define it at all", label, overrideHeader, name, baseHeader)
				continue
			}
			if ov != bv {
				t.Errorf("%s: %s = %q in %s but %q in %s; these two blocks must be edited together or they will silently drift apart", label, name, ov, overrideHeader, bv, baseHeader)
			}
		}
	}

	assertOverrideMatchesBase(t, "LIGHT", `:root:not([data-theme="dark"]) {`, `:root[data-theme="light"] {`)
	assertOverrideMatchesBase(t, "DARK", `:root[data-theme="dark"] {`, ":root {")
}

// cssTokenMap returns every `--name: value;` custom-property declaration in
// block as name -> normalized value (trimmed, no surrounding whitespace), so
// two blocks with different indentation or declaration order but identical
// tokens compare equal.
func cssTokenMap(block string) map[string]string {
	re := regexp.MustCompile(`(?m)^\s*(--[\w-]+)\s*:\s*([^;]+);`)
	matches := re.FindAllStringSubmatch(block, -1)
	out := make(map[string]string, len(matches))
	for _, m := range matches {
		out[m[1]] = strings.TrimSpace(m[2])
	}
	return out
}

// valueAfterToken returns the first line's value following `tok` inside src
// (up to the next `;`), trimmed.
func valueAfterToken(t *testing.T, src, tok string) string {
	t.Helper()
	at := strings.Index(src, tok)
	if at == -1 {
		return ""
	}
	rest := src[at+len(tok):]
	end := strings.Index(rest, ";")
	if end == -1 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// --- 4. the settings panel ------------------------------------------------

// TestWebUIPage_SettingsGearButtonInHeaderQuietInkFocusable pins the
// header-right, quiet-ink, accessible-name half of the gear button.
func TestWebUIPage_SettingsGearButtonInHeaderQuietInkFocusable(t *testing.T) {
	page := string(webUIPage)
	header := jsBracedBlockAfterTag(t, page, `<header class="topbar">`, "</header>")
	if !strings.Contains(header, `id="settingsBtn"`) {
		t.Error("the settings gear button is not inside the header")
	}
	if !strings.Contains(header, `aria-label="Settings"`) {
		t.Error("the settings button has no aria-label")
	}
	if !strings.Contains(header, `class="icon-btn"`) {
		t.Error("the settings button is not styled with the quiet-ink icon-btn treatment")
	}

	rule := jsBracedBlockAfter(t, page, "button.icon-btn {")
	if !strings.Contains(rule, "var(--ink-dim)") {
		t.Error("the settings button's resting color is not the quiet ink-dim token")
	}
	if strings.Contains(rule, "background: var(--rest)") {
		t.Error("the settings button uses the page's one primary/filled treatment, not a quiet one")
	}
}

// jsBracedBlockAfterTag returns the text from the first occurrence of open to
// the first occurrence of close after it — a simpler, tag-shaped sibling of
// jsBracedBlockAfter for HTML elements that are not one braced CSS/JS block.
func jsBracedBlockAfterTag(t *testing.T, page, open, close string) string {
	t.Helper()
	start := strings.Index(page, open)
	if start == -1 {
		t.Fatalf("no %q found in page", open)
	}
	end := strings.Index(page[start:], close)
	if end == -1 {
		t.Fatalf("no %q found after %q", close, open)
	}
	return page[start : start+end]
}

// TestWebUIPage_SettingsDialogIsANativeDialogWithFocusHandling pins the
// "native <dialog>, no framework" requirement plus its focus behavior:
// opened with showModal() (traps focus, blocks the page behind it), and
// closing it explicitly returns focus to the button that opened it.
func TestWebUIPage_SettingsDialogIsANativeDialogWithFocusHandling(t *testing.T) {
	page := string(webUIPage)
	if !strings.Contains(page, `<dialog id="settingsDialog"`) {
		t.Fatal(`page has no <dialog id="settingsDialog"> element`)
	}
	if !strings.Contains(page, "settingsDialog.showModal()") {
		t.Error("the settings dialog is never opened via showModal(), so it would not trap focus or block the page behind it")
	}
	if !strings.Contains(page, `settingsDialog.addEventListener("close"`) {
		t.Error("the settings dialog never listens for its own close event")
	}
	closeBlock := jsBracedBlockAfter(t, page, `settingsDialog.addEventListener("close"`)
	if !strings.Contains(closeBlock, "settingsBtn.focus()") {
		t.Error("closing the dialog never returns focus to the gear button that opened it")
	}
	if !strings.Contains(page, `settingsDialog.close()`) {
		t.Error("the dialog's own Close button never calls dialog.close()")
	}
}

// TestWebUIPage_SettingsDialogReloadsFromStorageBeforeRenderingOnOpen pins a
// cross-tab correctness requirement: `settings` in memory is only ever
// mutated by THIS tab's own onSettingsChange, so the gear button's click
// handler must check localStorage for a GENUINE change immediately before
// reflecting it onto the form — otherwise a second tab open on the same
// origin shows its own stale copy forever, and changing just one radio there
// would silently overwrite whatever a DIFFERENT tab had written to the other
// four settings keys in between.
//
// [v0.3.0 review fix, round 3] This used to call the ALWAYS-succeeds
// loadSettings() unconditionally, which cannot tell "storage is blocked"
// from "nothing chosen yet" apart — both return the same all-defaults
// object. With storage blocked, that discarded every choice made this
// session (e.g. Light) the moment the panel was reopened, even though the
// choice was still visibly in force on the page — see readStoredSettings'
// own comment in webui.html for the full repro. The fix must (1) use
// readStoredSettings(), which returns null rather than an all-defaults
// object when nothing genuine was read, (2) only adopt its result — and only
// re-push it onto the PAGE via applySettings() — inside a guard over that
// result, and (3) never unconditionally reassign `settings` from storage.
func TestWebUIPage_SettingsDialogReloadsFromStorageBeforeRenderingOnOpen(t *testing.T) {
	page := string(webUIPage)
	clickBlock := jsBracedBlockAfter(t, page, `settingsBtn.addEventListener("click", function () {`)

	if strings.Contains(clickBlock, "settings = loadSettings()") {
		t.Errorf(`the click handler still unconditionally assigns settings = loadSettings() — on a browser with storage blocked, loadSettings() returns the same all-defaults object "nothing chosen yet" would also return, silently discarding every choice made this session the moment the panel reopens:\n%s`, clickBlock)
	}
	if !strings.Contains(clickBlock, "readStoredSettings()") {
		t.Fatalf("the gear button's click handler never calls readStoredSettings(), so it cannot tell a genuine cross-tab change from storage merely failing to read:\n%s", clickBlock)
	}

	ifBlock := jsBracedBlockAfter(t, clickBlock, "if (stored) {")
	if !strings.Contains(ifBlock, "settings = stored") {
		t.Errorf(`readStoredSettings()'s result is never adopted into settings inside its own "if (stored)" guard, so it either overwrites settings unconditionally (reintroducing the storage-blocked bug) or is never adopted at all:\n%s`, clickBlock)
	}
	if !strings.Contains(ifBlock, "applySettings(settings)") {
		t.Errorf(`a genuine cross-tab settings change is adopted into the in-memory settings object but never re-applied to the PAGE via applySettings() — the dialog would show, say, "Light" while the page itself keeps rendering dark until some unrelated radio here happens to get clicked:\n%s`, ifBlock)
	}

	readAt := strings.Index(clickBlock, "readStoredSettings()")
	renderAt := strings.Index(clickBlock, "renderSettingsForm()")
	if readAt == -1 || renderAt == -1 || readAt > renderAt {
		t.Errorf("settings must be checked against storage BEFORE renderSettingsForm() reflects it onto the dialog's radios:\n%s", clickBlock)
	}
}

// TestWebUIPage_ApplySettingsBundlesEveryLoadTimeEffect pins the shared
// helper the gear button's click handler (see the test above) uses to
// re-push a genuinely cross-tab-changed settings object onto the PAGE, not
// just the dialog's own radios — the same four effects the initial load
// applies inline (kept inline there rather than routed through this
// function, for the variable-declaration-ordering reason applySettings'
// own comment in webui.html explains).
//
// [v0.3.0 review fix, round 4] It used to be enough for POLL_MS and
// DEFAULT_PAGE_SIZE to merely be REASSIGNED here — but DEFAULT_PAGE_SIZE is
// only ever read by the pager constructors (once, at construction, long
// before any cross-tab adoption can run) and by onSettingsChange's own
// gated block, so reassigning it alone never reached reversePager/filePager,
// and the operator could not even correct it by hand afterwards (the radio
// is already checked, so clicking it again fires no change event). This now
// pins the SAME propagation onSettingsChange's own blocks are pinned for —
// pushed onto whichever pager is untouched, both panels repainted, and the
// poll timer re-armed — not merely that the right identifiers appear
// somewhere in the function body.
func TestWebUIPage_ApplySettingsBundlesEveryLoadTimeEffect(t *testing.T) {
	page := string(webUIPage)
	body := jsBracedBlockAfter(t, page, "function applySettings(")
	for _, want := range []string{
		"applyTheme(",
		"applyMotion(",
		"VALID_POLL_MS.indexOf(",
		"POLL_MS = ",
		"VALID_PAGE_SIZES.indexOf(",
		"DEFAULT_PAGE_SIZE = ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("applySettings never contains %q — a genuine cross-tab settings change adopted by the gear button's click handler would not fully take effect on the page:\n%s", want, body)
		}
	}

	// POLL_MS's own guard block must re-arm the poll timer, exactly as
	// onSettingsChange's matching block does
	// (TestWebUIPage_PollCadenceSettingWiredWithoutAffectingScanningTighten
	// below) — otherwise an adopted cross-tab cadence change would not take
	// effect until whatever timeout is already outstanding happens to fire
	// on its own.
	pollBlock := jsBracedBlockAfter(t, body, "if (VALID_POLL_MS.indexOf(s.pollMs) !== -1")
	if !strings.Contains(pollBlock, "schedulePoll()") {
		t.Errorf("applySettings' POLL_MS guard block never calls schedulePoll() to re-arm the outstanding timer:\n%s", pollBlock)
	}

	// DEFAULT_PAGE_SIZE's own guard block must reach the pagers themselves,
	// not just the module-level default: pushed onto whichever of
	// reversePager/filePager is untouched, and both panels repainted so the
	// change is visible without a reload — the same propagation
	// TestWebUIPage_PageSizeSettingFeedsPagerDefaultsButPerSectionOverridesPersist
	// pins for onSettingsChange's own matching block.
	pageSizeBlock := jsBracedBlockAfter(t, body, "if (VALID_PAGE_SIZES.indexOf(s.pageSize) !== -1")
	for _, want := range []string{
		"!reversePager.touched",
		"!filePager.touched",
		"renderReverse(lastInstances)",
		"renderFileReport(lastInstances)",
	} {
		if !strings.Contains(pageSizeBlock, want) {
			t.Errorf("applySettings' DEFAULT_PAGE_SIZE guard block never contains %q — an adopted cross-tab page-size change would be recorded in the module-level default but never reach the pagers or repaint the panels, leaving the dialog stating a value the page is not using:\n%s", want, pageSizeBlock)
		}
	}
}

// TestWebUIPage_SettingsChangeListenerCallsOnSettingsChange pins the ONE
// call site that makes the whole settings panel real. Every other settings
// assertion in this file is scoped to onSettingsChange's own function BODY
// (pageFunctionBody cuts at the next top-level statement) — none of them can
// see whether anything actually CALLS it. Deleting
// `settingsDialog.addEventListener("change", ...)` turns all five settings
// into permanent no-ops (every radio click updates the DOM's own checked
// state and nothing else) while every body-scoped test stays green, because
// they assert what the dead function would do if it ran, not that anything
// ever runs it. This mirrors the existing "close" listener pin
// (TestWebUIPage_SettingsDialogIsANativeDialogWithFocusHandling) for the
// "change" listener specifically.
func TestWebUIPage_SettingsChangeListenerCallsOnSettingsChange(t *testing.T) {
	page := string(webUIPage)
	if !strings.Contains(page, `settingsDialog.addEventListener("change"`) {
		t.Fatal(`the settings dialog never listens for its own "change" event, so no radio click could ever apply or persist a setting`)
	}
	changeBlock := jsBracedBlockAfter(t, page, `settingsDialog.addEventListener("change"`)
	if !strings.Contains(changeBlock, "onSettingsChange()") {
		t.Errorf(`the settings dialog's "change" listener never calls onSettingsChange() — every setting would silently become a no-op:\n%s`, changeBlock)
	}
}

// TestWebUIPage_LoadTimeAppliesEveryPersistedSetting pins the four call
// sites that make "persisted in localStorage, applied on load" — the
// panel's core promise — actually true, rather than only what
// onSettingsChange (the WRITE path) does. loadSettings itself is referenced
// by no other test at all; deleting any one of applyMotion(settings.motion),
// the POLL_MS load-time assignment, or the DEFAULT_PAGE_SIZE load-time
// assignment makes that setting silently reset to its default on every
// reload while the rest of the suite (all scoped to onSettingsChange or to
// static markup) stays green.
func TestWebUIPage_LoadTimeAppliesEveryPersistedSetting(t *testing.T) {
	page := string(webUIPage)
	start := strings.Index(page, "var settings = loadSettings();")
	if start == -1 {
		t.Fatal("page never assigns var settings = loadSettings()")
	}
	end := strings.Index(page[start:], "function text(el, s)")
	if end == -1 {
		t.Fatal("could not find the end of the load-time settings-application region (function text)")
	}
	region := page[start : start+end]

	for _, want := range []string{
		"applyTheme(settings.theme)",
		"applyMotion(settings.motion)",
		"POLL_MS = settings.pollMs",
		"DEFAULT_PAGE_SIZE = settings.pageSize",
	} {
		if !strings.Contains(region, want) {
			t.Errorf("the load-time region (between `var settings = loadSettings();` and the first helper function) never contains %q — this setting would silently reset to its default on every reload:\n%s", want, region)
		}
	}
}

// TestWebUIPage_SettingsDialogHasAllFiveSettingsControls pins the exact
// five setting groups the brief specifies, and every option each of them
// must offer — a typo or a dropped radio value here would silently remove
// or rename an option nobody would otherwise notice.
func TestWebUIPage_SettingsDialogHasAllFiveSettingsControls(t *testing.T) {
	page := string(webUIPage)
	dialog := jsBracedBlockAfterTag(t, page, `<dialog id="settingsDialog"`, "</dialog>")

	if !strings.Contains(dialog, ">Settings<") {
		t.Error(`the dialog is not titled "Settings"`)
	}

	groups := []struct {
		label  string
		radio  string
		values []string
	}{
		{"theme", "settingTheme", []string{"system", "dark", "light"}},
		{"refresh cadence", "settingPollMs", []string{"10000", "30000", "60000"}},
		{"findings page size default", "settingPageSize", []string{"10", "25", "50", "all"}},
		{"timestamps", "settingTimestamps", []string{"relative", "absolute"}},
		{"motion", "settingMotion", []string{"system", "reduced"}},
	}
	for _, g := range groups {
		for _, v := range g.values {
			want := `name="` + g.radio + `" value="` + v + `"`
			if !strings.Contains(dialog, want) {
				t.Errorf("%s setting is missing the %q radio option (%q)", g.label, v, want)
			}
		}
	}

	if !strings.Contains(dialog, "2s") {
		t.Error("the refresh-cadence copy never states that the 2s scanning tighten is unaffected")
	}
	if !strings.Contains(dialog, "34s ago") {
		t.Error(`the Timestamps setting never shows the "relative" option's own example copy ("34s ago")`)
	}
}

// TestWebUIPage_FmtTimestampAbsoluteSettingActuallyRendersAbsoluteTime runs
// fmtTimestamp under Node with a real settings global, rather than only
// grepping its source for the string "absolute". Every OTHER reference to
// fmtTimestamp in this suite either greps for the identifier or forces
// settings.timestamps = "relative" (statecache_test.go's renderStaleBanner
// case), so before this test, replacing fmtTimestamp's entire body with
// `return fmtRelative(iso);` kept the whole suite green — one of the
// brief's five mandated settings had zero discriminating coverage. Both
// directions are pinned so an inverted condition fails just as loudly as a
// dropped one.
func TestWebUIPage_FmtTimestampAbsoluteSettingActuallyRendersAbsoluteTime(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping the behavioural fmtTimestamp check")
	}

	fixed := time.Date(2023, 6, 15, 12, 34, 56, 0, time.UTC)
	iso := fixed.Format(time.RFC3339)

	t.Run(`"absolute" renders an absolute time, not the relative form`, func(t *testing.T) {
		out := runPageFunctionUnderNode(t, nodePath, `
var settings = { timestamps: "absolute" };
`+pageFunctionSource(t, "fmtRelative")+pageFunctionSource(t, "fmtTimestamp")+`
console.log(JSON.stringify(fmtTimestamp("`+iso+`")));
`)
		var got string
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("could not read what fmtTimestamp rendered (%v):\n%s", err, out)
		}
		if strings.Contains(got, "ago") || got == "just now" {
			t.Errorf(`fmtTimestamp("%s") with settings.timestamps="absolute" returned %q, which looks like the RELATIVE form — the absolute branch may not be executing at all`, iso, got)
		}
		if !strings.Contains(got, "2023") {
			t.Errorf(`fmtTimestamp("%s") with settings.timestamps="absolute" returned %q, which does not name the year — doesn't look like an absolute rendering`, iso, got)
		}
	})

	t.Run(`"relative" (the default) keeps rendering the relative form`, func(t *testing.T) {
		recentISO := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
		out := runPageFunctionUnderNode(t, nodePath, `
var settings = { timestamps: "relative" };
`+pageFunctionSource(t, "fmtRelative")+pageFunctionSource(t, "fmtTimestamp")+`
console.log(JSON.stringify(fmtTimestamp("`+recentISO+`")));
`)
		var got string
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("could not read what fmtTimestamp rendered (%v):\n%s", err, out)
		}
		if !strings.Contains(got, "ago") {
			t.Errorf(`fmtTimestamp("%s") with settings.timestamps="relative" returned %q, want the relative form ("Nm ago") — an inverted condition would silently swap this to the absolute rendering`, recentISO, got)
		}
	})
}

// TestWebUIPage_ReadOnlySwitchesCardRendersThreeStatesWithExplanations pins
// the read-only server-switches card: all three booleans present as
// distinct elements, each with a sentence explaining it is config-file-only
// and why, and renderSwitchesCard actually wired to the stats payload's own
// field spellings.
func TestWebUIPage_ReadOnlySwitchesCardRendersThreeStatesWithExplanations(t *testing.T) {
	page := string(webUIPage)
	dialog := jsBracedBlockAfterTag(t, page, `<dialog id="settingsDialog"`, "</dialog>")

	for _, id := range []string{"swDryRun", "swGuiActions", "swReverseScanRemonitor"} {
		if !strings.Contains(dialog, `id="`+id+`"`) {
			t.Errorf("the read-only switches card has no %s element", id)
		}
	}
	for _, want := range []string{"dry_run", "gui_actions", "reverse_scan_remonitor"} {
		if !strings.Contains(dialog, want) {
			t.Errorf("the switches card never names %q", want)
		}
	}
	lower := strings.ToLower(dialog)
	if !strings.Contains(lower, "config-file-only") {
		t.Error("the switches card never states these are config-file-only")
	}
	if !strings.Contains(lower, "must not") {
		t.Error("the switches card never explains WHY these are read-only (an unauthenticated LAN page must not arm its own writes)")
	}

	body := pageFunctionBody(t, "renderSwitchesCard")
	for _, want := range []string{"data.dryRun", "data.guiActions", "data.reverseScanRemonitor"} {
		if !strings.Contains(body, want) {
			t.Errorf("renderSwitchesCard never reads %q from the stats payload", want)
		}
	}
	if !strings.Contains(pageFunctionBody(t, "render"), "renderSwitchesCard(data)") {
		t.Error("render() never calls renderSwitchesCard, so the card would never update")
	}
}

// TestWebUIPage_ReadOnlySwitchesCardMapsEachBooleanToItsOwnElement runs
// renderSwitchesCard under Node against a stub document, rather than only
// grepping its source for the three field names. The substring check above
// cannot tell WHICH element receives WHICH boolean: swapping the
// swGuiActions/swReverseScanRemonitor targets, or hardcoding "on" for every
// element, would both pass it unchanged. That matters more here than for a
// cosmetic card: these are the write-safety switches, and a swap would
// actively misinform an operator about whether click-to-act or re-monitor
// writes are armed on the server.
func TestWebUIPage_ReadOnlySwitchesCardMapsEachBooleanToItsOwnElement(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; the structural half of this pin ran, the behavioural half needs an interpreter")
	}

	runCase := func(t *testing.T, dryRun, guiActions, reverseScanRemonitor bool) map[string]string {
		out := runPageFunctionUnderNode(t, nodePath, `
var records = {};
var document = {
  getElementById: function (id) {
    if (!records[id]) records[id] = { textContent: "" };
    return records[id];
  }
};
`+pageFunctionSource(t, "text")+pageFunctionSource(t, "renderSwitchesCard")+`
renderSwitchesCard({ dryRun: `+boolLit(dryRun)+`, guiActions: `+boolLit(guiActions)+`, reverseScanRemonitor: `+boolLit(reverseScanRemonitor)+` });
console.log(JSON.stringify({
  swDryRun: records.swDryRun && records.swDryRun.textContent,
  swGuiActions: records.swGuiActions && records.swGuiActions.textContent,
  swReverseScanRemonitor: records.swReverseScanRemonitor && records.swReverseScanRemonitor.textContent
}));
`)
		var got map[string]string
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("could not read what renderSwitchesCard rendered (%v):\n%s", err, out)
		}
		return got
	}

	t.Run("mixed booleans map to the correct element each", func(t *testing.T) {
		got := runCase(t, true, false, true)
		want := map[string]string{"swDryRun": "on", "swGuiActions": "off", "swReverseScanRemonitor": "on"}
		for id, w := range want {
			if got[id] != w {
				t.Errorf("%s = %q, want %q (input dryRun=true guiActions=false reverseScanRemonitor=true) — this catches a swapped target as well as a hardcoded value", id, got[id], w)
			}
		}
	})

	t.Run("all false renders all off", func(t *testing.T) {
		got := runCase(t, false, false, false)
		want := map[string]string{"swDryRun": "off", "swGuiActions": "off", "swReverseScanRemonitor": "off"}
		for id, w := range want {
			if got[id] != w {
				t.Errorf("%s = %q, want %q", id, got[id], w)
			}
		}
	})
}

func boolLit(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestWebUIPage_LocalStorageNamespaceIsOneKeyReadBeforePaint pins the
// no-flash requirement structurally: a small guarded script BEFORE <style>
// reads the SAME namespaced localStorage key the main script (at the bottom
// of the page) defines as SETTINGS_KEY, and applies data-theme from it.
func TestWebUIPage_LocalStorageNamespaceIsOneKeyReadBeforePaint(t *testing.T) {
	page := string(webUIPage)
	styleStart := strings.Index(page, "<style>")
	if styleStart == -1 {
		t.Fatal("page has no <style> tag")
	}

	firstScriptStart := strings.Index(page, "<script>")
	if firstScriptStart == -1 || firstScriptStart >= styleStart {
		t.Fatal("page has no <script> block before <style> — the theme cannot be applied before first paint")
	}
	firstScriptEnd := strings.Index(page, "</script>")
	if firstScriptEnd == -1 || firstScriptEnd > styleStart {
		t.Fatal("the pre-style script block is never closed before <style>")
	}
	headScript := page[firstScriptStart:firstScriptEnd]

	const key = `"cutoffarr.settings.v1"`
	if !strings.Contains(headScript, key) {
		t.Errorf("the pre-paint head script does not read the namespaced settings key %s", key)
	}
	if !strings.Contains(headScript, "localStorage") {
		t.Error("the pre-paint head script never reads localStorage")
	}
	if !strings.Contains(headScript, "try") || !strings.Contains(headScript, "catch") {
		t.Error("the pre-paint head script does not guard localStorage/JSON access with try/catch")
	}
	if !strings.Contains(headScript, `setAttribute("data-theme"`) {
		t.Error("the pre-paint head script never sets the data-theme attribute")
	}

	mainScriptStart := strings.LastIndex(page, "<script>")
	if mainScriptStart == firstScriptStart {
		t.Fatal("page has only one <script> block; the main script must be separate from the pre-paint head script")
	}
	mainScript := page[mainScriptStart:]
	if !strings.Contains(mainScript, "SETTINGS_KEY = "+key) {
		t.Errorf("the main script does not define SETTINGS_KEY = %s — the same literal the head script reads", key)
	}
}

// TestWebUIPage_ThemeSettingAppliesDataThemeAttributeOnChange pins the
// write side: changing a setting persists it and re-applies the theme
// immediately, and applyTheme's own two branches set/clear the attribute
// correctly for an explicit choice vs "System".
func TestWebUIPage_ThemeSettingAppliesDataThemeAttributeOnChange(t *testing.T) {
	body := pageFunctionBody(t, "onSettingsChange")
	if !strings.Contains(body, "applyTheme(settings.theme)") {
		t.Error("changing a setting never re-applies the theme")
	}
	if !strings.Contains(body, "saveSettings(settings)") {
		t.Error("changing a setting is never persisted")
	}

	applyBody := pageFunctionBody(t, "applyTheme")
	if !strings.Contains(applyBody, `setAttribute("data-theme", theme)`) {
		t.Error("applyTheme never sets the data-theme attribute for an explicit choice")
	}
	if !strings.Contains(applyBody, `removeAttribute("data-theme")`) {
		t.Error(`applyTheme never clears data-theme for "System"`)
	}
}

// TestWebUIPage_SaveSettingsReportsStorageFailure pins the WRITE side of the
// same honesty rule loadSettings already gets for reads: loadSettings' own
// catch has an honest fallback (the System default it would have rendered
// anyway), but a failed localStorage.setItem has no honest silent
// equivalent — the setting applies for this session only and is gone on
// reload, and previously the operator was never told why. saveSettings must
// report success/failure rather than swallow it.
func TestWebUIPage_SaveSettingsReportsStorageFailure(t *testing.T) {
	body := pageFunctionBody(t, "saveSettings")
	if !strings.Contains(body, "return true") {
		t.Error("saveSettings never reports a successful write")
	}
	if !strings.Contains(body, "return false") {
		t.Error("saveSettings never reports a failed write; a storage failure is silently swallowed with no way for the caller to know")
	}

	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; the structural half of this pin ran, the behavioural half needs an interpreter")
	}

	okOut := runPageFunctionUnderNode(t, nodePath, `
var localStorage = { setItem: function () {} };
var SETTINGS_KEY = "cutoffarr.settings.v1";
`+pageFunctionSource(t, "saveSettings")+`
console.log(JSON.stringify(saveSettings({theme:"dark"})));
`)
	if strings.TrimSpace(okOut) != "true" {
		t.Errorf("saveSettings with a working localStorage.setItem returned %q, want true", okOut)
	}

	blockedOut := runPageFunctionUnderNode(t, nodePath, `
var localStorage = { setItem: function () { throw new Error("blocked"); } };
var SETTINGS_KEY = "cutoffarr.settings.v1";
`+pageFunctionSource(t, "saveSettings")+`
console.log(JSON.stringify(saveSettings({theme:"dark"})));
`)
	if strings.TrimSpace(blockedOut) != "false" {
		t.Errorf("saveSettings with a throwing localStorage.setItem returned %q, want false", blockedOut)
	}
}

// TestWebUIPage_SettingsDialogWarnsWhenStorageFails pins the surfacing half:
// a false return from saveSettings must show the dialog's existing
// .settings-note treatment naming the reason, and a successful save must
// hide it again (storage being blocked is not necessarily permanent — e.g.
// a privacy-mode toggle mid-session).
func TestWebUIPage_SettingsDialogWarnsWhenStorageFails(t *testing.T) {
	page := string(webUIPage)
	dialog := jsBracedBlockAfterTag(t, page, `<dialog id="settingsDialog"`, "</dialog>")
	if !strings.Contains(dialog, `id="settingsStorageWarning"`) {
		t.Fatal("the settings dialog has no settingsStorageWarning element")
	}
	if !strings.Contains(dialog, `class="settings-note" id="settingsStorageWarning" hidden`) {
		t.Error("settingsStorageWarning is not hidden by default, or does not use the dialog's existing .settings-note treatment")
	}
	if !strings.Contains(dialog, "will not survive a reload") {
		t.Error("settingsStorageWarning never states the concrete consequence (the setting will not survive a reload)")
	}

	body := pageFunctionBody(t, "onSettingsChange")
	if !strings.Contains(body, "saveSettings(settings)") {
		t.Fatal("onSettingsChange no longer calls saveSettings")
	}
	if !strings.Contains(body, "settingsStorageWarning.hidden = ") {
		t.Error("onSettingsChange never sets settingsStorageWarning.hidden from saveSettings' return value, so a storage failure would never be surfaced to the operator")
	}
	if strings.Contains(body, "settingsStorageWarning.hidden = true;") && !strings.Contains(body, "settingsStorageWarning.hidden = persisted") {
		// A hardcoded `= true` (always hidden) would defeat the whole point;
		// it must be driven by saveSettings' actual return value.
		t.Error("settingsStorageWarning.hidden looks hardcoded rather than driven by saveSettings' return value")
	}
}

// TestWebUIPage_PollCadenceSettingWiredWithoutAffectingScanningTighten pins
// the brief's explicit "unaffected" clause structurally: the setting can
// only ever reassign POLL_MS, and there is no statement anywhere that
// reassigns POLL_SCANNING_MS from a setting.
func TestWebUIPage_PollCadenceSettingWiredWithoutAffectingScanningTighten(t *testing.T) {
	page := string(webUIPage)
	if !strings.Contains(page, "POLL_MS = 30000") {
		t.Error("the idle poll default is no longer pinned at 30s")
	}
	if !strings.Contains(page, "POLL_SCANNING_MS = 2000") {
		t.Error("the 2s scanning-tighten interval is no longer pinned")
	}
	if !strings.Contains(page, "VALID_POLL_MS") {
		t.Error("page never validates a pollMs setting against the three allowed cadences")
	}
	for _, v := range []string{"10000", "30000", "60000"} {
		if !strings.Contains(page, v) {
			t.Errorf("page never mentions the %s cadence option", v)
		}
	}

	body := pageFunctionBody(t, "onSettingsChange")
	if !strings.Contains(body, "POLL_MS = settings.pollMs") {
		t.Error("onSettingsChange never applies the cadence setting to POLL_MS")
	}
	if strings.Contains(page, "POLL_SCANNING_MS = settings") {
		t.Error("a setting must never be able to override the 2s scanning-tighten interval")
	}

	// [v0.3.0 review fix] POLL_MS is only READ by schedulePoll at the moment it
	// arms a new setTimeout; assigning it alone changes nothing until whatever
	// timer is already outstanding happens to fire (up to the OLD cadence's
	// full duration later). onSettingsChange must re-arm by calling
	// schedulePoll() itself, in the same statement/block as the assignment —
	// this is the identical defect already fixed once for the Scan-now button.
	pollAssign := strings.Index(body, "POLL_MS = settings.pollMs")
	if pollAssign == -1 {
		t.Fatal("POLL_MS = settings.pollMs not found in onSettingsChange body")
	}
	afterAssign := body[pollAssign:]
	closeBrace := strings.Index(afterAssign, "}")
	if closeBrace == -1 {
		t.Fatal("could not find the end of the POLL_MS assignment's guarding block")
	}
	if !strings.Contains(afterAssign[:closeBrace], "schedulePoll()") {
		t.Errorf("onSettingsChange assigns POLL_MS but never calls schedulePoll() to re-arm the outstanding timer — a tightened cadence would not take effect until the timer armed at the OLD interval happens to fire on its own:\n%s", afterAssign[:closeBrace])
	}
}

// TestWebUIPage_PageSizeSettingFeedsPagerDefaultsButPerSectionOverridesPersist
// pins the brief's own "feeds the existing pagers' default; per-section
// overrides still work per session" sentence as an actual mechanism: a
// manual per-panel size click marks that pager touched, and a later
// settings-driven default change only reaches an UNTOUCHED pager.
//
// [v0.3.0 review fix, round 3] Extended with the assertion that the reset
// itself is gated on the default having ACTUALLY changed
// (settings.pageSize !== previousPageSize), not merely on
// VALID_PAGE_SIZES.indexOf(settings.pageSize) being valid — which is true
// for every settings object the radios can ever produce, so on its own it
// used to run (and reset any untouched pager to page 1) on EVERY settings
// change, including Theme/Motion/Refresh-cadence changes that have nothing
// to do with page size.
func TestWebUIPage_PageSizeSettingFeedsPagerDefaultsButPerSectionOverridesPersist(t *testing.T) {
	page := string(webUIPage)
	if !strings.Contains(page, "DEFAULT_PAGE_SIZE = 25") {
		t.Error("the built-in findings page-size default is no longer pinned at 25")
	}
	if !strings.Contains(page, `PAGE_SIZES = [10, 25, 50, "all"]`) {
		t.Error("page no longer defines the four page-size tokens")
	}
	if !strings.Contains(page, "size: DEFAULT_PAGE_SIZE") {
		t.Error("a pager's initial state no longer defaults to DEFAULT_PAGE_SIZE")
	}

	body := pageFunctionBody(t, "onSettingsChange")
	if !strings.Contains(body, "DEFAULT_PAGE_SIZE = settings.pageSize") {
		t.Error("onSettingsChange never feeds the settings default into DEFAULT_PAGE_SIZE")
	}
	if !strings.Contains(body, "!reversePager.touched") || !strings.Contains(body, "!filePager.touched") {
		t.Error("onSettingsChange does not gate the new default on each pager's own touched flag, so a per-section override would be clobbered by a later settings change")
	}

	sizeHandler := pageFunctionBody(t, "makeSizeClickHandler")
	if !strings.Contains(sizeHandler, "pager.touched = true") {
		t.Error("a manual per-section page-size click never marks that pager as touched, so a later settings change could still clobber it")
	}

	// The OLD value must be captured BEFORE `settings = next` overwrites it —
	// otherwise there is nothing left to compare against to tell "the
	// default actually changed" from "some other setting changed instead".
	assignAt := strings.Index(body, "settings = next;")
	if assignAt == -1 {
		t.Fatal("onSettingsChange no longer assigns settings = next; — cannot locate where the prior pageSize must be captured")
	}
	before := body[:assignAt]
	if !regexp.MustCompile(`\bprev\w*\s*=\s*settings\.pageSize\b`).MatchString(before) {
		t.Errorf("onSettingsChange never captures the OLD settings.pageSize before overwriting `settings` — the page-size reset block cannot tell an actual default change from an unrelated settings change (e.g. Theme):\n%s", before)
	}

	// The DEFAULT_PAGE_SIZE assignment's own guard must compare against that
	// captured previous value.
	sizeAssignAt := strings.Index(body, "DEFAULT_PAGE_SIZE = settings.pageSize")
	if sizeAssignAt == -1 {
		t.Fatal("DEFAULT_PAGE_SIZE = settings.pageSize not found in onSettingsChange body")
	}
	ifAt := strings.LastIndex(body[:sizeAssignAt], "if (")
	if ifAt == -1 {
		t.Fatal("could not find the if-guard preceding the DEFAULT_PAGE_SIZE assignment")
	}
	closeParen := strings.Index(body[ifAt:], ") {")
	if closeParen == -1 {
		t.Fatal("could not find the end of the DEFAULT_PAGE_SIZE guard's condition")
	}
	cond := body[ifAt : ifAt+closeParen]
	if !regexp.MustCompile(`\bprev\w*`).MatchString(cond) {
		t.Errorf("the page-size block's guard never compares against a captured previous value, so it runs — and resets any untouched pager to page 1 — on EVERY settings change, not only when the page-size default actually changed:\n%s", cond)
	}
}

// TestWebUIPage_TimestampsSettingChangeTriggersFullRepaint pins that
// onSettingsChange actually repaints the page when the Timestamps setting
// changes, the way it already does for theme, motion and (as of the fix
// above) page size. [v0.3.0 review fix, round 3] fmtTimestamp is read LIVE
// by six separate call sites (last-swept, each shelf card's "as of", both
// renderStaleBanner branches, the reverse panel's staleness notice, and
// showDisconnected's "showing data from") that onSettingsChange otherwise
// never touches, so a relative<->absolute change used to sit invisible on
// screen until the next successful poll — up to the operator's own cadence
// setting later, or never while the backend stayed unreachable.
func TestWebUIPage_TimestampsSettingChangeTriggersFullRepaint(t *testing.T) {
	body := pageFunctionBody(t, "onSettingsChange")

	assignAt := strings.Index(body, "settings = next;")
	if assignAt == -1 {
		t.Fatal("onSettingsChange no longer assigns settings = next; — cannot locate where the prior timestamps setting must be captured")
	}
	before := body[:assignAt]
	if !regexp.MustCompile(`\bprev\w*\s*=\s*settings\.timestamps\b`).MatchString(before) {
		t.Errorf("onSettingsChange never captures the OLD settings.timestamps before overwriting `settings` — a repaint triggered on every settings change (not just a timestamps change) would cost an extra network round-trip for nothing:\n%s", before)
	}

	refreshAt := strings.Index(body, "refresh();")
	if refreshAt == -1 {
		t.Fatalf("onSettingsChange never calls refresh() — a Timestamps change would not repaint last-swept, the shelf cards' \"as of\", the warm-start banner, or the disconnected badge's \"showing data from\" until the next poll on its own schedule:\n%s", body)
	}
	ifAt := strings.LastIndex(body[:refreshAt], "if (")
	if ifAt == -1 {
		t.Fatalf("refresh() in onSettingsChange is not guarded by an if — it would run on EVERY settings change, not only a timestamps change:\n%s", body)
	}
	closeParen := strings.Index(body[ifAt:], ") {")
	if closeParen == -1 {
		t.Fatal("could not find the end of the refresh() guard's condition")
	}
	cond := body[ifAt : ifAt+closeParen]
	if !strings.Contains(cond, "timestamps") || !regexp.MustCompile(`\bprev\w*`).MatchString(cond) {
		t.Errorf("the refresh() guard never compares settings.timestamps against a captured previous value:\n%s", cond)
	}
}

// TestWebUIPage_MotionReducedSettingForcesMotionOffIndependentOfSystemPreference
// pins the forced-motion-off mechanism: a plain attribute selector (not
// another media query, which could never see a DOM attribute) mirroring
// every animated selector the OS-level reduced-motion block covers, and no
// data-motion value anywhere that could turn motion back ON.
func TestWebUIPage_MotionReducedSettingForcesMotionOffIndependentOfSystemPreference(t *testing.T) {
	page := string(webUIPage)

	block := jsBracedBlockAfter(t, page, `[data-motion="reduce"] .shelf-rest`)
	for _, sel := range []string{".shelf-rest", ".shelf-marker", "details.panel summary::before", ".scan-strip-fill"} {
		if !strings.Contains(block, sel) {
			t.Errorf("the forced-motion-off rule never names %q — every animated selector the OS-level reduced-motion block covers must also be covered here", sel)
		}
	}
	if !strings.Contains(block, "transition: none") {
		t.Error("the forced-motion-off rule never sets transition: none")
	}

	pulseBlock := jsBracedBlockAfter(t, page, `[data-motion="reduce"] .scan-strip-indeterminate .scan-strip-fill`)
	if !strings.Contains(pulseBlock, "animation: none") {
		t.Error("the forced-motion-off rule never disables the indeterminate pulse's keyframe animation")
	}

	if regexp.MustCompile(`\[data-motion="(?:system|on|off)"\]`).MatchString(page) {
		t.Error(`page defines a data-motion value other than "reduce" — the setting must never be able to force motion ON`)
	}

	applyBody := pageFunctionBody(t, "applyMotion")
	if !strings.Contains(applyBody, `setAttribute("data-motion", "reduce")`) {
		t.Error(`applyMotion never sets data-motion="reduce" for the Reduced choice`)
	}
	if !strings.Contains(applyBody, `removeAttribute("data-motion")`) {
		t.Error(`applyMotion never clears data-motion for "System" (which must defer entirely to the OS's own prefers-reduced-motion query)`)
	}
}

// --- 5. the REHEARSING/live badge tooltip -------------------------------

// TestWebUIPage_DryRunBadgeCarriesTooltipCopy pins item 4's exact copy, on
// BOTH the title attribute (hover) and aria-label (screen reader / anyone
// who never hovers) — one sentence each, exact wording, per state.
func TestWebUIPage_DryRunBadgeCarriesTooltipCopy(t *testing.T) {
	const rehearsing = "dry_run is on: cutoffarr computes and shows everything it would do, and writes nothing"
	const live = "dry_run is off: sweeps and enabled actions perform real writes"

	body := pageFunctionBody(t, "render")
	if !strings.Contains(body, rehearsing) {
		t.Errorf("render() never sets the rehearsing tooltip copy %q", rehearsing)
	}
	if !strings.Contains(body, live) {
		t.Errorf("render() never sets the live tooltip copy %q", live)
	}
	if !strings.Contains(body, "badge.title") {
		t.Error("render() never sets the badge's title attribute")
	}
	if !strings.Contains(body, `badge.setAttribute("aria-label"`) {
		t.Error("render() never sets the badge's aria-label")
	}
}

// TestWebUIPage_DisconnectedStateClearsTheStaleTooltip is the badge
// tooltip's own honesty rule: once the page can no longer say whether
// dry_run is on or off (the backend is unreachable), the badge must not go
// on asserting the last answer it heard.
func TestWebUIPage_DisconnectedStateClearsTheStaleTooltip(t *testing.T) {
	body := pageFunctionBody(t, "showDisconnected")
	if !strings.Contains(body, `badge.removeAttribute("title")`) {
		t.Error("showDisconnected never clears the badge's stale dry-run tooltip")
	}
	if !strings.Contains(body, `badge.removeAttribute("aria-label")`) {
		t.Error("showDisconnected never clears the badge's stale aria-label")
	}
}

// TestWebUIPage_DisconnectedStateClearsTheStaleSwitchesCard is the badge
// tooltip's honesty rule applied to the read-only server-switches card the
// same commit added: renderSwitchesCard only ever runs from render() (i.e.
// only on a SUCCESSFUL poll), so without an explicit reset here a
// disconnected page would go on asserting the last-known dry_run/gui_actions/
// reverse_scan_remonitor values as current fact — worse than the tooltip
// case, since these are the write-safety switches and the only way any of
// them changes is a config edit plus a daemon restart, exactly the event
// that produces a disconnect.
func TestWebUIPage_DisconnectedStateClearsTheStaleSwitchesCard(t *testing.T) {
	body := pageFunctionBody(t, "showDisconnected")
	for _, id := range []string{"swDryRun", "swGuiActions", "swReverseScanRemonitor"} {
		if !strings.Contains(body, id) {
			t.Errorf("showDisconnected never touches %s — the read-only switches card would keep asserting pre-outage values indefinitely on a disconnected page", id)
		}
	}
	if !strings.Contains(body, `"—"`) {
		t.Error(`showDisconnected never resets the switches card to the markup's own honest "—" placeholder`)
	}
}
