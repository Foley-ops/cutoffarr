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
	Name    string `xml:"Name,attr"`
	Target  string `xml:"Target,attr"`
	Default string `xml:"Default,attr"`
	Mode    string `xml:"Mode,attr"`
	Type    string `xml:"Type,attr"`
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
// resolution names outright, independent of anything docker-compose.example.yml
// says (compose's own `image:` line is a local build tag, not this project's
// published image).
func TestTemplate_RepositoryPointsAtTheGHCRImage(t *testing.T) {
	tmpl := parseUnraidTemplate(t)
	const want = "ghcr.io/foley-ops/cutoffarr"
	if tmpl.Repository != want {
		t.Errorf("Repository = %q, want %q", tmpl.Repository, want)
	}
}

// TestTemplate_NoWebUIElement pins the deliberate omission: cutoffarr has no
// web UI, and a <WebUI> element would point Unraid's WebUI button at the
// webhook endpoint, which only ever answers with 405. Checked structurally
// (see unraidTemplate.WebUI's own comment) rather than by raw substring
// search, because the file's explanatory comment about this very omission
// legitimately contains the text "<WebUI>" in prose.
func TestTemplate_NoWebUIElement(t *testing.T) {
	tmpl := parseUnraidTemplate(t)
	if tmpl.WebUI != nil {
		t.Errorf("templates/cutoffarr.xml must not contain a <WebUI> element (got %q): cutoffarr has no web UI, and the button would only ever reach a 405", *tmpl.WebUI)
	}
}

// TestTemplate_CarriesTheWebUI405Rationale pins that the omission above is
// explained, not merely silent — the same "deliberate, not an oversight"
// standard container_test.go already holds docker-compose.example.yml's own
// webui omission to.
func TestTemplate_CarriesTheWebUI405Rationale(t *testing.T) {
	raw := readRepoFile(t, "templates/cutoffarr.xml")
	if !strings.Contains(raw, "405") {
		t.Errorf("templates/cutoffarr.xml must explain the WebUI omission with its 405 rationale (a POST-only endpoint answers a browser GET with 405):\n%s", raw)
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
	if !strings.Contains(compose, want+":"+want) {
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
	if !strings.Contains(compose, ":/config:ro") {
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
	if !strings.Contains(compose, `user: "99:100"`) {
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
	if !strings.Contains(compose, "restart: unless-stopped") {
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
	if !strings.Contains(compose, "media-net") {
		t.Errorf("docker-compose.example.yml no longer uses the media-net placeholder; the template's Network must be updated to match")
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
