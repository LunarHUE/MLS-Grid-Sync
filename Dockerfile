# syntax=docker/dockerfile:1

# Builder always runs natively on the build host; Go cross-compiles to the
# target platform, so no QEMU emulation is needed for multi-arch builds.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

# ca-certificates ships with golang:alpine, and all modules are public
# (fetched from proxy.golang.org), so no extra apk packages are required.

ENV CGO_ENABLED=0

WORKDIR /src

COPY go.mod go.sum ./

RUN go mod download

COPY . .

# Declared after `go mod download` so the module cache layer is shared
# across target platforms (ARGs in scope invalidate later RUN cache keys).
ARG TARGETOS TARGETARCH

# Build identity, injected via -X ldflags (the image has no .git, so Go's VCS
# stamping can't populate these — CI passes real values as --build-arg). The
# defaults keep a plain `docker build` (no build-args) producing sane output.
# NOTE: the -X paths must match go.mod's exact module-path casing
# (github.com/LunarHUE/MLS-Grid-Sync) or -X silently no-ops.
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

RUN GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags="-s -w \
        -X github.com/LunarHUE/MLS-Grid-Sync/version.Version=${VERSION} \
        -X github.com/LunarHUE/MLS-Grid-Sync/version.Commit=${COMMIT} \
        -X github.com/LunarHUE/MLS-Grid-Sync/version.BuildDate=${BUILD_DATE}" \
      -o /out/mls-cli .

FROM gcr.io/distroless/static:nonroot

COPY --from=builder /out/mls-cli /usr/local/bin/mls-cli

USER nonroot:nonroot

# GraphQL API port used by the `serve` subcommand (MLS_SYNC_SERVER_ADDR).
EXPOSE 8080

ENTRYPOINT ["mls-cli"]
CMD ["serve"]
