# The end-to-end runner: a Go toolchain to build the binary under test, and
# tmux to give it a real terminal. Nothing else — the suite seeds and inspects
# MinIO through the project's own model package.
#
# The base image is pinned by digest, so a run is reproducible and works
# offline once the image is present. It matches go.mod's `go 1.23` directive;
# move the two together.
ARG GO_IMAGE=golang:1.23-alpine@sha256:383395b794dffa5b53012a212365d40c8e37109a626ca30d6151c8348d380b5f
FROM ${GO_IMAGE}

RUN apk add --no-cache tmux

# Modules are fetched at image build time, so the run itself needs no network
# beyond the compose network MinIO is on. Only go.mod / go.sum are in the
# build context (see Dockerfile.dockerignore): source changes do not
# invalidate this layer.
WORKDIR /deps
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Never let a newer go.mod line trigger a toolchain download mid-run.
ENV GOTOOLCHAIN=local

# The tree is mounted read-only here by docker-compose.yml; nothing is copied
# in, so a run always tests the working tree rather than a stale layer.
WORKDIR /src
CMD ["go", "test", "-tags", "e2e", "-count=1", "-v", "./e2e/"]
