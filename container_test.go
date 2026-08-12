package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// container_test.go keeps the two packaging artifacts honest against the code
// they package.
//
// Both are documentation in the sense that nothing in the Go build reads them —
// which is exactly why they rot. A Dockerfile pinned to a Go version the module
// no longer builds under, a CMD pointing at a config path the flag default
// moved away from, a published port that stopped being the default: each is
// invisible until someone deploys. These tests are cheap and they fail at the
// moment the drift is introduced rather than at the moment it is deployed.

func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

// TestDockerfile_BuildsWithTheModulesOwnGoVersion: a build stage pinned to an
// older Go than go.mod requires fails at `go build` with a message about the
// toolchain, in a build nobody runs until deploy day.
func TestDockerfile_BuildsWithTheModulesOwnGoVersion(t *testing.T) {
	dockerfile := readRepoFile(t, "Dockerfile")
	gomod := readRepoFile(t, "go.mod")

	m := regexp.MustCompile(`(?m)^go (\d+\.\d+(?:\.\d+)?)`).FindStringSubmatch(gomod)
	if m == nil {
		t.Fatalf("go.mod has no go directive:\n%s", gomod)
	}
	want := "FROM golang:" + m[1]
	if !strings.Contains(dockerfile, want) {
		t.Errorf("the build stage must pin the module's own Go version (%q from go.mod); Dockerfile:\n%s", want, dockerfile)
	}
}

// TestDockerfile_FinalStageIsDistrolessNonRootAndStaticallyLinked pins the
// three properties that make the image what the plan asked for: nothing in it
// but the binary, never running as root, and no libc dependency (which is what
// would break a distroless/static base at runtime rather than at build time).
func TestDockerfile_FinalStageIsDistrolessNonRootAndStaticallyLinked(t *testing.T) {
	dockerfile := readRepoFile(t, "Dockerfile")

	for _, want := range []string{
		"FROM gcr.io/distroless/static:nonroot",
		"USER nonroot",
		"CGO_ENABLED=0",
		"-trimpath",
		`-ldflags "-s -w"`,
		`ENTRYPOINT ["/cutoffarr"]`,
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("Dockerfile must contain %q:\n%s", want, dockerfile)
		}
	}
	// A shell in the final image would defeat the point of distroless.
	if strings.Contains(dockerfile, "FROM alpine") || strings.Contains(dockerfile, "FROM debian") {
		t.Errorf("the FINAL stage must be distroless; a shell-bearing base defeats it:\n%s", dockerfile)
	}
}

// TestDockerfile_DefaultsMatchTheProgramsOwnDefaults: the image's CMD and its
// EXPOSE are claims about this program, and both have a single source of truth
// in the Go code.
func TestDockerfile_DefaultsMatchTheProgramsOwnDefaults(t *testing.T) {
	dockerfile := readRepoFile(t, "Dockerfile")

	wantCmd := `CMD ["--config", "` + defaultConfigPath + `"]`
	if !strings.Contains(dockerfile, wantCmd) {
		t.Errorf("CMD must point at the flag's own default (%s):\n%s", wantCmd, dockerfile)
	}
	wantExpose := "EXPOSE " + strconv.Itoa(defaultWebhookPort)
	if !strings.Contains(dockerfile, wantExpose) {
		t.Errorf("EXPOSE must name the default webhook port (%s):\n%s", wantExpose, dockerfile)
	}
}

// TestComposeExample_MatchesThePlansDeploymentShape pins the compose example
// against the plan's own list, item by item, plus the config path the image
// expects.
func TestComposeExample_MatchesThePlansDeploymentShape(t *testing.T) {
	compose := readRepoFile(t, "docker-compose.example.yml")

	port := strconv.Itoa(defaultWebhookPort)
	for _, want := range []struct{ what, substr string }{
		{"the config volume at /config", ":/config"},
		{"the Unraid uid/gid", `user: "99:100"`},
		{"a timezone", "TZ="},
		{"the external network", "external: true"},
		{"the shared network's name", "media-net"},
		{"the webhook port", port + ":" + port},
		{"a restart policy", "restart: unless-stopped"},
		{"the Unraid icon label", "net.unraid.docker.icon"},
		{"the Unraid shell label", "net.unraid.docker.shell"},
	} {
		if !strings.Contains(compose, want.substr) {
			t.Errorf("the compose example must carry %s (%q):\n%s", want.what, want.substr, compose)
		}
	}

	// And the one key the plan's list does not name but this program's own
	// shutdown design requires. See the dedicated test below.
	if !strings.Contains(uncommented(compose), "stop_grace_period:") {
		t.Errorf("the compose example must set stop_grace_period; Docker's 10s default SIGKILLs mid-season-write:\n%s", compose)
	}
}

// TestComposeExample_StopGracePeriodOutlastsASeasonWrite is the deployment
// artifact's half of binding controller note 4.
//
// The invariant that note protects is that a season write is ATOMIC with
// respect to shutdown: unmonitorSeason detaches its calls from cancellation
// (context.WithoutCancel) so the episode PUT and the series PUT either both
// happen or neither starts, because the state in between — episodes
// unmonitored, season still monitored — is the one the recovery path exists to
// mop up. The Go code cannot enforce that alone: if the supervisor SIGKILLs the
// process mid-pair, the invariant is broken from outside, and Docker's default
// stop timeout is 10 SECONDS.
//
// A season write is up to five sequential calls (GET /series, GET /episode,
// PUT /episode/monitor, the confirming re-GET, PUT /series), each bounded by
// apiClientTimeout — so against an *arr that is slow rather than dead, the
// worst case is five times that, plus the server's own drain. The grace period
// is asserted against the CODE's constant rather than a literal, so shortening
// one without the other fails here instead of on deploy day.
func TestComposeExample_StopGracePeriodOutlastsASeasonWrite(t *testing.T) {
	compose := readRepoFile(t, "docker-compose.example.yml")

	m := regexp.MustCompile(`(?m)^\s*stop_grace_period:\s*(\d+)s`).FindStringSubmatch(uncommented(compose))
	if m == nil {
		t.Fatalf("the compose example must set stop_grace_period as a plain number of seconds:\n%s", compose)
	}
	grace, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parsing stop_grace_period %q: %v", m[1], err)
	}

	// The five calls of the worst-case season write, plus the webhook server's
	// own drain: anything less and SIGKILL can land between the episode PUT and
	// the series PUT.
	const seasonWriteCalls = 5
	want := seasonWriteCalls*int(apiClientTimeout.Seconds()) + int(webhookShutdownGrace.Seconds())
	if grace < want {
		t.Errorf("stop_grace_period is %ds; a season write can take %d x %s plus a %s drain, so anything under %ds lets SIGKILL land between the episode PUT and the series PUT",
			grace, seasonWriteCalls, apiClientTimeout, webhookShutdownGrace, want)
	}

	// webui is deliberately omitted (plan: "webui omitted for now") — cutoffarr
	// has no web UI, and the label would point Unraid's button at a 405. The
	// check is against the UNCOMMENTED file, because saying so in a comment is
	// how the omission stays deliberate rather than looking like an oversight.
	if strings.Contains(uncommented(compose), "net.unraid.docker.webui") {
		t.Errorf("net.unraid.docker.webui must stay omitted: there is no web UI to point it at:\n%s", compose)
	}

	// The external network needs a warning, because the single most likely
	// deployment failure is a name that exists but is the wrong one: a network
	// compose created for another stack is "<project>_default", not the bare
	// name in that stack's file.
	if !strings.Contains(compose, "_default") {
		t.Errorf("the external network must carry the <project>_default caveat, which is the likeliest deployment failure:\n%s", compose)
	}
}

// TestComposeExample_CarriesNoRealCredentials is a guard on a committed file
// that names API keys. Every key must be an ${ENV} reference, never a value.
func TestComposeExample_CarriesNoRealCredentials(t *testing.T) {
	compose := readRepoFile(t, "docker-compose.example.yml")

	// Any "..._API_KEY=" assignment must be followed by a ${...} expansion.
	assignment := regexp.MustCompile(`(?m)([A-Z0-9_]*API_KEY)=(\S*)`)
	for _, m := range assignment.FindAllStringSubmatch(uncommented(compose), -1) {
		if !strings.HasPrefix(m[2], "${") {
			t.Errorf("%s is assigned the literal %q; committed files reference credentials, they never carry them", m[1], m[2])
		}
	}
	// And no api_key: key at all — that belongs in config.yml, which is
	// gitignored.
	if strings.Contains(compose, "api_key:") {
		t.Errorf("the compose example must not carry an api_key at all:\n%s", compose)
	}
}

// uncommented strips YAML comment text, so a check about what the file DOES can
// not be tripped by a comment explaining what it deliberately does not do.
func uncommented(yaml string) string {
	var b strings.Builder
	for _, line := range strings.Split(yaml, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
