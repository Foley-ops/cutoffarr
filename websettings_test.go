package main

import (
	"regexp"
	"strings"
	"testing"
)

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
}

// TestWebUIPage_PageSizeSettingFeedsPagerDefaultsButPerSectionOverridesPersist
// pins the brief's own "feeds the existing pagers' default; per-section
// overrides still work per session" sentence as an actual mechanism: a
// manual per-panel size click marks that pager touched, and a later
// settings-driven default change only reaches an UNTOUCHED pager.
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
