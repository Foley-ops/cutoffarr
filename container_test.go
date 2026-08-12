package main

import (
	"os"
	"path"
	"regexp"
	"sort"
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

// TestDockerfile_CrossBuildsForTARGETARCH pins Phase 9's release-workflow
// requirement: the build stage must be buildable for linux/amd64 AND
// linux/arm64 from a single `docker buildx build --platform
// linux/amd64,linux/arm64 .` invocation. Before this phase the build stage
// hardcoded GOOS=linux with no GOARCH at all, so every cross-arch build
// silently produced an amd64 binary (Go defaults GOARCH to the host
// toolchain's arch when unset) regardless of which platform buildx thought it
// was building — the image would report as arm64 in the manifest and fail
// immediately on any actual arm64 host. buildx auto-populates a TARGETARCH
// build arg per platform, but only a Dockerfile that ARGs it in and threads it
// into GOARCH ever sees it.
func TestDockerfile_CrossBuildsForTARGETARCH(t *testing.T) {
	dockerfile := readRepoFile(t, "Dockerfile")

	for _, want := range []string{
		"ARG TARGETARCH",
		"GOARCH=$TARGETARCH",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("Dockerfile must contain %q so buildx's per-platform build arg reaches `go build`:\n%s", want, dockerfile)
		}
	}

	// ARG TARGETARCH must be declared in the BUILD stage (before the RUN that
	// consumes it), not merely present anywhere in the file — a bare mention
	// in a comment or the final stage would satisfy the substring check above
	// without actually wiring anything.
	argIdx := strings.Index(dockerfile, "ARG TARGETARCH")
	buildIdx := strings.Index(dockerfile, "AS build")
	useIdx := strings.Index(dockerfile, "GOARCH=$TARGETARCH")
	if argIdx == -1 || buildIdx == -1 || useIdx == -1 || !(buildIdx < argIdx && argIdx < useIdx) {
		t.Errorf("ARG TARGETARCH must be declared inside the build stage, before the go build line that consumes it:\n%s", dockerfile)
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
//
// The CMD equality is load-bearing beyond tidiness, because `docker run <image>
// --once` REPLACES the CMD rather than appending to it: that container runs
// `/cutoffarr --once` with no --config argument at all, and finds its config
// only because the flag's own default is the same path. Let the two drift and
// the documented one-shot invocation quietly reads a file that is not there.
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

// --- the build context ------------------------------------------------------

// TestDockerignore_KeepsTheWorkingTreesSecretsOutOfTheBuildContext is the
// packaging invariant with the largest blast radius, and the one nothing else
// in this file can see.
//
// The FINAL image is clean by construction: it is `FROM scratch`-shaped and
// only /out/cutoffarr is copied forward. The BUILD STAGE is not. `COPY . .`
// copies whatever the build context holds, and this working tree holds the
// operator's live config.yml (real *arr API keys), a .env, .git, and agent
// scratch. Those land in the build-stage layer, and a build-stage layer is not
// a private thing: `docker build --target build`, `docker buildx -o
// type=local`, `docker history` on the cached stage, and the BuildKit cache on
// whatever host ran the build all still hold it. The deployment plan puts that
// host on the server, so without a .dockerignore, `docker build` ships the
// developer's credentials there as a side effect.
//
// TestComposeExample_CarriesNoRealCredentials guards a COMMITTED file and by
// construction cannot catch this: the danger here is precisely the files that
// are NOT committed.
func TestDockerignore_KeepsTheWorkingTreesSecretsOutOfTheBuildContext(t *testing.T) {
	patterns := ignorePatterns(readRepoFile(t, ".dockerignore"))

	// The named entries, each one a file that exists in a working tree of this
	// project and must never enter a build context.
	for _, want := range []string{
		".git",                             // the entire history, including any key ever committed by mistake
		".env*",                            // compose's own credential file
		"config.yml",                       // the live config: api_key per instance
		"config.yaml",                      // ... under its other legal name
		".superpowers/",                    // agent scratch
		"/cutoffarr",                       // a stale host-built binary, ~9 MB of nothing the build needs
		"cutoffarr-implementation-plan.md", // private planning doc
	} {
		if !patterns[want] {
			t.Errorf(".dockerignore must exclude %q from the build context; it has:\n%s", want, strings.Join(sortedSet(patterns), "\n"))
		}
	}

	// And the rule that keeps the list from rotting: .gitignore is this repo's
	// existing statement of "must never leave this machine", so anything added
	// there has to be added here too. A new secret file is otherwise protected
	// from git and handed straight to Docker.
	for _, p := range sortedSet(ignorePatterns(readRepoFile(t, ".gitignore"))) {
		if !patterns[p] {
			t.Errorf("%q is gitignored but not dockerignored: git refuses to track it, and `docker build` would copy it into the build stage anyway", p)
		}
	}
}

// TestDockerignore_DoesNotExcludeWhatTheBuildNeeds is the other half: an
// exclusion list broad enough to drop the sources builds an image that is
// wrong rather than one that fails to build, if the failure is subtle enough
// (a missing _test.go file is invisible; a missing .go file is not). This is
// cheap insurance against a future `*` or `**` that someone adds meaning "then
// re-include what I need" and gets slightly wrong.
func TestDockerignore_DoesNotExcludeWhatTheBuildNeeds(t *testing.T) {
	needed := []string{"go.mod", "go.sum", "main.go", "daemon.go", "webhook.go"}
	for pattern := range ignorePatterns(readRepoFile(t, ".dockerignore")) {
		clean := strings.TrimSuffix(strings.TrimPrefix(pattern, "/"), "/")
		for _, name := range needed {
			if ok, err := path.Match(clean, name); err == nil && ok {
				t.Errorf(".dockerignore pattern %q excludes %s, which the build stage needs", pattern, name)
			}
		}
	}
}

// ignorePatterns reads a .gitignore/.dockerignore into a set, dropping comments
// and blank lines. Both formats agree on those two rules, which is what lets
// one file be checked against the other.
func ignorePatterns(contents string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[line] = true
	}
	return out
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
