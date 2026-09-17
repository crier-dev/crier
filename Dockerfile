# ---- Build Stage ----
FROM golang:1.26.6-alpine AS build

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build identity (DF-CRIER-171). The image is an artifact of this checkout, so
# it carries the SAME identity `make build` stamps — without these stamps the
# reference image reported the "dev" sentinel and could not be correlated to a
# commit. Override at build time:
#
#	docker build --build-arg VERSION=1.2.3 --build-arg COMMIT=<sha> .
#
# The defaults are derived HERE so a plain `docker build .` still carries a real
# identity: `git describe` yields the same version string the Makefile stamps
# (.git is in the build context), and a context with no git metadata falls back
# to `dev`, which internal/buildinfo renders as `dev-<commit>`. The build runs
# as root over a context owned by another uid, so git refuses to read the repo
# until /src is marked safe.
ARG VERSION=
ARG COMMIT=
ARG BUILD_TIME=
RUN set -eux; \
    git config --global --add safe.directory /src; \
    V="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"; \
    C="${COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}"; \
    T="${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"; \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/crier-dev/crier/internal/buildinfo.Version=${V} -X github.com/crier-dev/crier/internal/buildinfo.Commit=${C} -X github.com/crier-dev/crier/internal/buildinfo.BuildTime=${T}" -o /bin/crier ./cmd/server

# ---- Run Stage ----
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

COPY --from=build /bin/crier /usr/local/bin/crier

EXPOSE 8767
ENTRYPOINT ["crier"]
