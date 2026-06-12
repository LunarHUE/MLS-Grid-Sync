# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git ca-certificates

ENV GOPRIVATE=github.com/lunarhue/* \
    CGO_ENABLED=0 \
    GOOS=linux

WORKDIR /src

COPY go.mod go.sum ./

# The gh_token secret grants read access to the private
# github.com/lunarhue/libs-go module. Mounted, never an ARG/ENV, so the
# credential cannot persist in any image layer.
RUN --mount=type=secret,id=gh_token \
    git config --global url."https://x-access-token:$(cat /run/secrets/gh_token)@github.com/".insteadOf "https://github.com/" && \
    go mod download && \
    git config --global --unset url."https://x-access-token:$(cat /run/secrets/gh_token)@github.com/".insteadOf

COPY . .

RUN go build -trimpath -ldflags="-s -w" -o /out/mls-cli .

FROM gcr.io/distroless/static:nonroot

COPY --from=builder /out/mls-cli /usr/local/bin/mls-cli

USER nonroot:nonroot

# GraphQL API port used by the `serve` subcommand (MLS_SYNC_SERVER_ADDR).
EXPOSE 8080

ENTRYPOINT ["mls-cli"]
CMD ["serve"]
