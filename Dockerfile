# cutoffarr — multi-stage build to a distroless, non-root, single-binary image.
#
# The image contains the binary and nothing else: no shell, no package manager,
# no interpreter. That is not minimalism for its own sake — this program holds
# API keys for every *arr on the network and is the one thing in the stack
# allowed to write to them, so the smaller its surface, the less there is to
# reason about.

# --- build stage -------------------------------------------------------------
#
# Pinned to the repo's own Go version (go.mod says 1.23.6). Bump both together.
#
# --platform=$BUILDPLATFORM pins THIS STAGE to whatever platform is actually
# running the build (BUILDPLATFORM, buildx's other automatic per-platform
# build arg), for every leg of release.yml's
# `docker buildx build --platform linux/amd64,linux/arm64 .`. Without it,
# BuildKit resolves this stage's own base image per TARGET platform too: the
# arm64 leg would pull an arm64 golang:alpine and run every RUN line below —
# including `go mod download` and the whole compile — under QEMU emulation.
# release.yml carries no QEMU setup step at all, precisely because this pin
# (plus GOARCH=$TARGETARCH below) makes that emulation path unreachable —
# see release.yml's own header comment for the full reasoning.
# Go's own GOARCH already defaults to the host it finds itself running on, so
# an emulated-arm64 stage would still produce a correctly-arched arm64
# binary even with no GOARCH set at all — just by compiling the entire
# module, slowly, under full emulation, for no reason: the actual bottleneck
# in every future release. With the stage pinned to BUILDPLATFORM instead,
# both legs run natively on the build machine (amd64, on GitHub-hosted
# runners), and GOARCH=$TARGETARCH below becomes what actually selects the
# output arch — turning the arm64 leg into a genuine native cross-compile.
FROM --platform=$BUILDPLATFORM golang:1.23.6-alpine AS build

# TARGETARCH is buildx's other automatic per-platform build arg. With the
# stage above pinned to BUILDPLATFORM, this is now the only thing telling
# `go build` which architecture to actually target — but only if a stage
# ARGs it in.
ARG TARGETARCH

# VERSION is the release tag (release.yml passes --build-arg
# VERSION=${{ github.ref_name }}, a strict vX.Y.Z per that workflow's own tag
# filter). Defaulted to "dev" — the SAME default stats.go's own buildVersion
# var already carries — so a plain `docker build .` with no build-arg (a
# local/dev build) still produces a binary that honestly reports "dev"
# rather than an empty or stale string.
ARG VERSION=dev

WORKDIR /src

# Dependency layer first, so a source-only change does not re-download modules.
# This project has exactly one dependency (gopkg.in/yaml.v3), which is the whole
# reason `go mod download` is cheap enough to be worth a layer of its own.
COPY go.mod go.sum ./
RUN go mod download

# What this copies is the build context AFTER .dockerignore has filtered it,
# and that filter is load-bearing rather than cosmetic: a working tree of this
# project holds the operator's live config.yml (a real api_key per instance), a
# .env, and .git. The final image would never carry them — only /out/cutoffarr
# is copied forward — but THIS stage would, and a build stage is readable from
# the BuildKit cache, from `--target build`, and from `docker history`. See
# .dockerignore, whose contents container_test.go pins.
COPY . .

# CGO_ENABLED=0 is what makes the binary runnable on `distroless/static`: with
# cgo on, Go's net package links the system resolver and the binary needs libc.
# The *arr URLs are hostnames, so the resolver matters — the pure-Go one reads
# /etc/resolv.conf, which the distroless image provides.
#
# -trimpath strips local filesystem paths out of the binary; -s -w drop the
# symbol table and DWARF, which is most of the size difference and costs only
# stack-trace symbol names in a panic (the panic's own message and goroutine
# dump survive). -X main.buildVersion=${VERSION} stamps the ARG above into
# the exact package-qualified var GET /api/stats reports (stats.go's own
# buildVersion, "var buildVersion = \"dev\"") — the same -ldflags "-X
# main.buildVersion=vX.Y.Z" that var's own doc comment already anticipated,
# before anything in the release pipeline actually set it. A local `go build`
# with no ldflags at all is untouched by any of this: it still links
# buildVersion's own "dev" default, exactly as before.
#
# The tests are run in CI, not here: a build stage that runs them makes every
# image build slower and every test failure look like a Docker problem.
# GOARCH=$TARGETARCH is what makes this stage actually cross-compile: because
# the FROM line above pins the stage to --platform=$BUILDPLATFORM, `go build`
# would otherwise target the BUILD machine's own arch on every leg (amd64, on
# GitHub-hosted runners), regardless of which platform buildx is producing
# this leg for. GOOS stays hardcoded to linux — this image never targets
# anything else, and TARGETOS would only add a second unused build arg.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build \
        -trimpath \
        -ldflags "-s -w -X main.buildVersion=${VERSION}" \
        -o /out/cutoffarr .

# --- final stage -------------------------------------------------------------
#
# distroless/static:nonroot carries CA certificates (needed for any https:// *arr
# URL), /etc/passwd with the nonroot user, and tzdata — and nothing executable
# but what we copy in.
FROM gcr.io/distroless/static:nonroot

COPY --from=build /out/cutoffarr /cutoffarr

# Runs as uid/gid 65532 (distroless "nonroot"). On Unraid the compose file
# overrides this with user: "99:100" so the mounted /config is readable by the
# usual nobody/users pair; either way the process is never root. The config file
# only needs to be READABLE — cutoffarr never writes to disk.
USER nonroot:nonroot

# The webhook listener's default port. Documentation only: publishing it is the
# compose file's job.
EXPOSE 9898

# Default run mode is the daemon: startup scan, webhook listener, reconciliation
# sweep.
#
# `docker run <image> --once` REPLACES this CMD — Docker does not append to it —
# so that container runs `/cutoffarr --once` with no --config at all. It still
# reads the right file only because --config's flag default is this same path
# (defaultConfigPath in main.go, pinned equal to the CMD below by
# TestDockerfile_DefaultsMatchTheProgramsOwnDefaults). Change one without the
# other and every `docker run ... --once` silently looks for its config
# somewhere else. `--dry-run` is passed the same way, and forces dry-run on
# regardless of the config.
ENTRYPOINT ["/cutoffarr"]
CMD ["--config", "/config/config.yml"]
