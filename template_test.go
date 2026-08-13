package main

import (
	"encoding/xml"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// template_test.go is the container_test.go-style pin for
// templates/cutoffarr.xml (binding controller resolution 5): the Unraid
// Community Applications template is, like the Dockerfile and the compose
// example, documentation nothing in the Go build reads — and exactly the
// kind of artifact that silently drifts from the deployment shape it claims
// to describe unless something fails the moment it does. These tests check
// templates/cutoffarr.xml against docker-compose.example.yml field by field,
// the same way container_test.go checks the Dockerfile against the Go code
// it packages.

// unraidTemplate is the subset of the Unraid CA template schema these tests
// need to assert against, parsed structurally rather than by substring
// match wherever the schema makes that possible (Repository, Icon,
// ExtraParams, and the Config entries). The two purely explanatory
// requirements — the distroless no-shell caveat and the WebUI 405
// rationale — exist only as comment prose, which encoding/xml does not
// surface by default, so those two are checked against the raw file text
// instead (see TestTemplate_HasDistrolessShellCaveat and
// TestTemplate_CarriesTheWebUI405Rationale below).
type unraidTemplate struct {
	XMLName     xml.Name       `xml:"Container"`
	Repository  string         `xml:"Repository"`
	Network     string         `xml:"Network"`
	Shell       string         `xml:"Shell"`
	Icon        string         `xml:"Icon"`
	ExtraParams string         `xml:"ExtraParams"`
	Configs     []unraidConfig `xml:"Config"`

	// WebUI is a pointer purely so its presence is distinguishable from its
	// absence: encoding/xml leaves an unmatched pointer field nil rather
	// than erroring, which is exactly what TestTemplate_NoWebUIElement needs
	// to check for an ACTUAL <WebUI> element structurally — immune to the
	// XML comment elsewhere in this file that mentions "<WebUI>" in prose
	// (a raw substring search over the file text would false-positive on
	// that comment; the parsed struct cannot, since a comment is never part
	// of an unmarshaled element).
	WebUI *string `xml:"WebUI"`
}

type unraidConfig struct {
	Name        string `xml:"Name,attr"`
	Target      string `xml:"Target,attr"`
	Default     string `xml:"Default,attr"`
	Mode        string `xml:"Mode,attr"`
	Type        string `xml:"Type,attr"`
	Required    string `xml:"Required,attr"`
	Mask        string `xml:"Mask,attr"`
	Description string `xml:"Description,attr"`
}

func parseUnraidTemplate(t *testing.T) unraidTemplate {
	t.Helper()
	raw := readRepoFile(t, "templates/cutoffarr.xml")
	var tmpl unraidTemplate
	if err := xml.Unmarshal([]byte(raw), &tmpl); err != nil {
		t.Fatalf("templates/cutoffarr.xml is not well-formed XML: %v", err)
	}
	return tmpl
}

// TestTemplate_IsWellFormedXML is the standalone well-formedness check the
// task's own verification section calls for, pinned as a Go test (not just a
// one-off `python3 -c` invocation) so a future edit that breaks the XML fails
// the same `go test ./...` everything else in this project fails under.
func TestTemplate_IsWellFormedXML(t *testing.T) {
	parseUnraidTemplate(t)
}

// TestTemplate_RepositoryPointsAtTheGHCRImage pins the one field the binding
// resolution names outright: the published GHCR image release.yml pushes on
// a tag push.
func TestTemplate_RepositoryPointsAtTheGHCRImage(t *testing.T) {
	tmpl := parseUnraidTemplate(t)
	const want = "ghcr.io/foley-ops/cutoffarr"
	if tmpl.Repository != want {
		t.Errorf("Repository = %q, want %q", tmpl.Repository, want)
	}
}

// TestTemplate_AgreesWithComposeExample_Image closes the gap the Phase 9
// branch review found: docker-compose.example.yml's own `image:` line used
// to be the bare local tag `cutoffarr:latest`, which Docker resolves to
// docker.io/library/cutoffarr — nowhere — and fails "pull access denied /
// repository does not exist" for anyone who follows the README's compose
// quick start verbatim, even though this file's Repository already named
// the real published image. Now that docker-compose.example.yml points at
// the same GHCR image, this pins the two from drifting apart again: the
// compose repository (everything before the `:tag`) must equal the
// template's Repository. Each file is still free to pin its own tag
// independently (`latest` vs. a specific `vX.Y.Z`).
func TestTemplate_AgreesWithComposeExample_Image(t *testing.T) {
	tmpl := parseUnraidTemplate(t)

	compose := readRepoFile(t, "docker-compose.example.yml")
	m := regexp.MustCompile(`(?m)^\s*image:\s*(\S+)\s*$`).FindStringSubmatch(uncommented(compose))
	if m == nil {
		t.Fatalf("docker-compose.example.yml must set an image: line:\n%s", compose)
	}
	composeImage := m[1]
	composeRepo, _, ok := strings.Cut(composeImage, ":")
	if !ok {
		t.Fatalf("docker-compose.example.yml's image %q has no :tag", composeImage)
	}
	if composeRepo != tmpl.Repository {
		t.Errorf("docker-compose.example.yml's image repository is %q but the template's Repository is %q; they must name the same published image (a bare local tag like cutoffarr:latest resolves to docker.io/library/cutoffarr and does not exist)", composeRepo, tmpl.Repository)
	}
}

// TestTemplate_HasWebUIElement pins Phase 12 (v2c)'s reversal of the
// pre-v2c omission: cutoffarr now serves a small read-only dashboard on the
// SAME port as the webhook endpoint (webui.go), so a <WebUI> element no
// longer sends a human to a 405 — it must be present, and must use Unraid's
// own [IP]/[PORT:n] placeholder syntax naming defaultWebhookPort (config.go)
// rather than a literal, so this can never silently drift from the actual
// webhook_port default the same way
// TestTemplate_AgreesWithComposeExample_WebhookPort already guards the Port
// Config against.
func TestTemplate_HasWebUIElement(t *testing.T) {
	tmpl := parseUnraidTemplate(t)
	want := "http://[IP]:[PORT:" + strconv.Itoa(defaultWebhookPort) + "]/"
	if tmpl.WebUI == nil {
		t.Fatalf("templates/cutoffarr.xml must contain a <WebUI>%s</WebUI> element: cutoffarr now serves a dashboard on this port", want)
	}
	if *tmpl.WebUI != want {
		t.Errorf("<WebUI> = %q, want %q", *tmpl.WebUI, want)
	}
}

// TestTemplate_AgreesWithComposeExample_WebUI is the WebUI element's own
// field-for-field agreement check, in the same shape as this file's other
// Agrees-tests (e.g. TestTemplate_AgreesWithComposeExample_IconURL): the
// template's <WebUI> and docker-compose.example.yml's own
// net.unraid.docker.webui label must name the exact same URL, or a human
// copying one deployment shape to the other gets a silently different
// button.
func TestTemplate_AgreesWithComposeExample_WebUI(t *testing.T) {
	tmpl := parseUnraidTemplate(t)
	if tmpl.WebUI == nil {
		t.Fatal("templates/cutoffarr.xml has no <WebUI> element to compare")
	}

	compose := readRepoFile(t, "docker-compose.example.yml")
	m := regexp.MustCompile(`net\.unraid\.docker\.webui:\s*"([^"]+)"`).FindStringSubmatch(uncommented(compose))
	if m == nil {
		t.Fatalf("docker-compose.example.yml must set the net.unraid.docker.webui label:\n%s", compose)
	}
	composeWebUI := m[1]

	if *tmpl.WebUI != composeWebUI {
		t.Errorf("template WebUI = %q, docker-compose.example.yml's net.unraid.docker.webui = %q; they must name the same URL", *tmpl.WebUI, composeWebUI)
	}
}

// TestTemplate_HasDistrolessShellCaveat pins the Shell setting's own
// required caveat: the final image is distroless and has no shell at all,
// so Unraid's Console button fails regardless of what this field says. Mirrors
// docker-compose.example.yml's net.unraid.docker.shell label, which carries
// the identical warning.
func TestTemplate_HasDistrolessShellCaveat(t *testing.T) {
	raw := readRepoFile(t, "templates/cutoffarr.xml")
	if !strings.Contains(raw, "distroless") || !strings.Contains(raw, "no shell") {
		t.Errorf("templates/cutoffarr.xml must carry the distroless/no-shell caveat on the Shell setting, mirroring docker-compose.example.yml's net.unraid.docker.shell label:\n%s", raw)
	}
}

// --- field-for-field agreement with docker-compose.example.yml -------------

// TestTemplate_AgreesWithComposeExample_WebhookPort pins the template's
// published port against the SAME source of truth container_test.go's own
// compose checks use: defaultWebhookPort (config.go), not a literal repeated
// a third time. Checked on both Target (the container-side port) and Default
// (the host-side port Unraid pre-fills), since docker-compose.example.yml
// publishes it as "9898:9898" — both sides equal.
func TestTemplate_AgreesWithComposeExample_WebhookPort(t *testing.T) {
	tmpl := parseUnraidTemplate(t)
	want := strconv.Itoa(defaultWebhookPort)

	cfg := findConfigByType(t, tmpl, "Port")
	if cfg.Target != want {
		t.Errorf("the Port Config's Target = %q, want %q (defaultWebhookPort)", cfg.Target, want)
	}
	if cfg.Default != want {
		t.Errorf("the Port Config's Default = %q, want %q (defaultWebhookPort)", cfg.Default, want)
	}

	compose := readRepoFile(t, "docker-compose.example.yml")
	if !strings.Contains(uncommented(compose), want+":"+want) {
		t.Errorf("docker-compose.example.yml no longer publishes %s:%s; the template's port Config must be updated to match whatever it publishes instead", want, want)
	}
}

// TestTemplate_AgreesWithComposeExample_ConfigPath pins the mounted config
// directory's container-side path and read-only mode against
// docker-compose.example.yml's own `:/config:ro` volume line.
func TestTemplate_AgreesWithComposeExample_ConfigPath(t *testing.T) {
	tmpl := parseUnraidTemplate(t)
	cfg := findConfigByType(t, tmpl, "Path")
	if cfg.Target != "/config" {
		t.Errorf("the Path Config's Target = %q, want %q", cfg.Target, "/config")
	}
	if cfg.Mode != "ro" {
		t.Errorf("the Path Config's Mode = %q, want %q (docker-compose.example.yml mounts /config read-only)", cfg.Mode, "ro")
	}

	compose := readRepoFile(t, "docker-compose.example.yml")
	if !strings.Contains(uncommented(compose), ":/config:ro") {
		t.Errorf("docker-compose.example.yml no longer mounts /config read-only; the template's Path Config Mode must be updated to match")
	}
}

// TestTemplate_AgreesWithComposeExample_UnraidUIDGID pins the "99:100"
// (Unraid's usual nobody:users pair) that docker-compose.example.yml sets via
// `user: "99:100"` and the template sets via ExtraParams' `--user 99:100`
// (Unraid CA templates have no dedicated "user" Config field).
func TestTemplate_AgreesWithComposeExample_UnraidUIDGID(t *testing.T) {
	tmpl := parseUnraidTemplate(t)
	if !strings.Contains(tmpl.ExtraParams, "99:100") {
		t.Errorf("ExtraParams = %q, must set the Unraid uid:gid pair 99:100", tmpl.ExtraParams)
	}

	compose := readRepoFile(t, "docker-compose.example.yml")
	if !strings.Contains(uncommented(compose), `user: "99:100"`) {
		t.Errorf(`docker-compose.example.yml no longer sets user: "99:100"; the template's ExtraParams must be updated to match`)
	}
}

// TestTemplate_AgreesWithComposeExample_RestartPolicy pins "unless-stopped"
// on both sides.
func TestTemplate_AgreesWithComposeExample_RestartPolicy(t *testing.T) {
	tmpl := parseUnraidTemplate(t)
	if !strings.Contains(tmpl.ExtraParams, "--restart unless-stopped") {
		t.Errorf("ExtraParams = %q, must set --restart unless-stopped", tmpl.ExtraParams)
	}

	compose := readRepoFile(t, "docker-compose.example.yml")
	if !strings.Contains(uncommented(compose), "restart: unless-stopped") {
		t.Errorf("docker-compose.example.yml no longer sets restart: unless-stopped; the template's ExtraParams must be updated to match")
	}
}

// TestTemplate_AgreesWithComposeExample_StopTimeoutMatchesStopGracePeriod is
// the template's half of binding controller note 4 (see
// TestComposeExample_StopGracePeriodOutlastsASeasonWrite for the full
// rationale): `docker run --stop-timeout` is the flag-form of compose's
// `stop_grace_period`, and the two MUST name the same number of seconds, or
// an Unraid deployment gets a different — and unexamined — shutdown grace
// window than the one docker-compose.example.yml's own long comment
// justifies against apiClientTimeout and webhookShutdownGrace.
func TestTemplate_AgreesWithComposeExample_StopTimeoutMatchesStopGracePeriod(t *testing.T) {
	tmpl := parseUnraidTemplate(t)

	m := regexp.MustCompile(`--stop-timeout (\d+)`).FindStringSubmatch(tmpl.ExtraParams)
	if m == nil {
		t.Fatalf("ExtraParams = %q, must set --stop-timeout <seconds>", tmpl.ExtraParams)
	}
	templateTimeout, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parsing --stop-timeout value %q: %v", m[1], err)
	}

	compose := readRepoFile(t, "docker-compose.example.yml")
	cm := regexp.MustCompile(`(?m)^\s*stop_grace_period:\s*(\d+)s`).FindStringSubmatch(uncommented(compose))
	if cm == nil {
		t.Fatalf("docker-compose.example.yml must set stop_grace_period as a plain number of seconds:\n%s", compose)
	}
	composeGrace, err := strconv.Atoi(cm[1])
	if err != nil {
		t.Fatalf("parsing stop_grace_period %q: %v", cm[1], err)
	}

	if templateTimeout != composeGrace {
		t.Errorf("template --stop-timeout is %ds but docker-compose.example.yml's stop_grace_period is %ds; an Unraid deployment must get the same shutdown grace window a compose deployment does",
			templateTimeout, composeGrace)
	}
}

// TestTemplate_AgreesWithComposeExample_IconURL pins the two files' icon URLs
// character-for-character: this is the exact 404 the Phase 8 branch review
// flagged (the compose file's own net.unraid.docker.icon label pointed at a
// file that did not exist in the repo), so the template naming a THIRD,
// silently different URL would reintroduce the same failure for Unraid users
// specifically.
func TestTemplate_AgreesWithComposeExample_IconURL(t *testing.T) {
	tmpl := parseUnraidTemplate(t)

	compose := readRepoFile(t, "docker-compose.example.yml")
	m := regexp.MustCompile(`net\.unraid\.docker\.icon:\s*"([^"]+)"`).FindStringSubmatch(compose)
	if m == nil {
		t.Fatalf("docker-compose.example.yml must set the net.unraid.docker.icon label:\n%s", compose)
	}
	composeIcon := m[1]

	if tmpl.Icon != composeIcon {
		t.Errorf("template Icon = %q, docker-compose.example.yml's net.unraid.docker.icon = %q; they must name the same URL", tmpl.Icon, composeIcon)
	}
}

// TestTemplate_IconURLPointsAtACommittedFile is the other half of the same
// Phase 8 finding: not just "the two files agree with each other" but "the
// URL they agree on actually resolves to something in this repo" — the
// committed icon.png, checked without any network access (no external link
// fetching is required or performed; the file simply has to exist at the
// path the URL's own final segment names).
func TestTemplate_IconURLPointsAtACommittedFile(t *testing.T) {
	tmpl := parseUnraidTemplate(t)
	if !strings.HasSuffix(tmpl.Icon, "/icon.png") {
		t.Fatalf("template Icon = %q, expected it to end in /icon.png", tmpl.Icon)
	}
	// readRepoFile itself fails the test if icon.png is not present.
	readRepoFile(t, "icon.png")
}

// TestTemplate_AgreesWithComposeExample_Network pins that the template
// points at the same "media-net" PLACEHOLDER docker-compose.example.yml
// does, carrying the identical "this is a placeholder, not a promise"
// caveat docker-compose.example.yml's own external-network comment makes —
// not a literal network-name match requirement (any real deployment's
// network is named whatever that user's *arr stack actually uses), but both
// files must at least start from and explain the same placeholder so a
// human copying one setup to the other is not surprised by a silent
// difference.
func TestTemplate_AgreesWithComposeExample_Network(t *testing.T) {
	tmpl := parseUnraidTemplate(t)
	if tmpl.Network != "media-net" {
		t.Errorf("template Network = %q, want the same placeholder docker-compose.example.yml uses, %q", tmpl.Network, "media-net")
	}

	compose := readRepoFile(t, "docker-compose.example.yml")
	if !strings.Contains(uncommented(compose), "media-net") {
		t.Errorf("docker-compose.example.yml no longer uses the media-net placeholder; the template's Network must be updated to match")
	}
}

// TestTemplate_AgreesWithComposeExample_Variables closes the gap the Phase 9
// branch review found: docker-compose.example.yml's `environment:` block
// sets TZ plus the two `${..._API_KEY}` references, but the template used to
// carry no `<Config Type="Variable">` element at all. A CA user who follows
// this template's own Overview ("Mount your config.yml ... API keys ... are
// read from the container's own environment") gets a config.yml whose
// `${RADARR_MAIN_API_KEY}` reference has nowhere to come from on Unraid,
// and config.go's own fatal "environment variable(s) referenced in config
// but not set" is what greets them at container start — the Overview's
// parenthetical about "additional Variable entries" was the only thing
// standing between the template and that outcome, and it was prose rather
// than a pre-filled field.
//
// This pins full agreement, not just "at least one": every environment key
// docker-compose.example.yml sets must have a matching Type="Variable"
// Config (matched on Target), and the template must not carry a Variable
// Config naming a key compose does not — the same "cannot drift apart"
// standard the rest of this file already holds every other field to.
func TestTemplate_AgreesWithComposeExample_Variables(t *testing.T) {
	tmpl := parseUnraidTemplate(t)

	compose := readRepoFile(t, "docker-compose.example.yml")
	envKey := regexp.MustCompile(`(?m)^\s*-\s*([A-Z0-9_]+)=`)
	composeKeys := map[string]bool{}
	for _, m := range envKey.FindAllStringSubmatch(uncommented(compose), -1) {
		composeKeys[m[1]] = true
	}
	if len(composeKeys) == 0 {
		t.Fatalf("docker-compose.example.yml's environment: block must set at least one key (TZ, at minimum):\n%s", compose)
	}

	templateKeys := map[string]bool{}
	for _, c := range tmpl.Configs {
		if c.Type != "Variable" {
			continue
		}
		if templateKeys[c.Target] {
			t.Errorf("templates/cutoffarr.xml has more than one Variable Config with Target=%q", c.Target)
		}
		templateKeys[c.Target] = true
	}

	for k := range composeKeys {
		if !templateKeys[k] {
			t.Errorf("docker-compose.example.yml's environment: sets %s, but templates/cutoffarr.xml has no matching <Config Type=\"Variable\" Target=%q>", k, k)
		}
	}
	for k := range templateKeys {
		if !composeKeys[k] {
			t.Errorf("templates/cutoffarr.xml has a Variable Config Target=%q that docker-compose.example.yml's environment: block does not set; they must name the same keys", k)
		}
	}

	// TZ specifically must not be Masked (it's not a secret) and every
	// *_API_KEY example must be — both Required=false, since a fresh
	// deployment with dry_run left at its true default writes nothing and a
	// human is expected to fill these in (or add more) before turning it off.
	for _, c := range tmpl.Configs {
		if c.Type != "Variable" {
			continue
		}
		switch {
		case c.Target == "TZ":
			if c.Required != "false" {
				t.Errorf("TZ Variable Config Required=%q, want %q", c.Required, "false")
			}
		case strings.HasSuffix(c.Target, "_API_KEY"):
			if c.Mask != "true" {
				t.Errorf("%s Variable Config Mask=%q, want %q (an API key must not be shown in plaintext)", c.Target, c.Mask, "true")
			}
			if c.Required != "false" {
				t.Errorf("%s Variable Config Required=%q, want %q (a fresh deployment has dry_run=true and no writes to authorize yet)", c.Target, c.Required, "false")
			}
			if !strings.Contains(c.Description, "${"+c.Target+"}") {
				t.Errorf("%s Variable Config Description must name the ${%s} convention config.yml expects, got %q", c.Target, c.Target, c.Description)
			}
		}
	}
}

// findConfigByType returns the single <Config Type="..."> element of the
// given type, failing the test if there is not exactly one — the same
// "count it, don't just grep for a substring" discipline
// TestTree_HasExactlyThreeWriteVerbCallSites applies to the write-verb
// audit, applied here so a future second Path or Port Config (which would
// make "the" Config of that type ambiguous) is caught rather than silently
// matching whichever one happens to be first.
func findConfigByType(t *testing.T, tmpl unraidTemplate, configType string) unraidConfig {
	t.Helper()
	var matches []unraidConfig
	for _, c := range tmpl.Configs {
		if c.Type == configType {
			matches = append(matches, c)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("templates/cutoffarr.xml has %d Config element(s) of Type=%q, want exactly 1", len(matches), configType)
	}
	return matches[0]
}
