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

RUN GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/mls-cli .

FROM gcr.io/distroless/static:nonroot

COPY --from=builder /out/mls-cli /usr/local/bin/mls-cli

USER nonroot:nonroot

# GraphQL API port used by the `serve` subcommand (MLS_SYNC_SERVER_ADDR).
EXPOSE 8080

ENTRYPOINT ["mls-cli"]
CMD ["serve"]
