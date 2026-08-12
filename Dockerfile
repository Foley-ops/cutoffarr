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
FROM golang:1.23.6-alpine AS build

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
# dump survive).
#
# The tests are run in CI, not here: a build stage that runs them makes every
# image build slower and every test failure look like a Docker problem.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w" \
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
# sweep. `docker run ... --once` appends to this and gets a single pass instead,
# and `--dry-run` forces dry-run on regardless of the config.
ENTRYPOINT ["/cutoffarr"]
CMD ["--config", "/config/config.yml"]
