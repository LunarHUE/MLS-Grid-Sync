# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git ca-certificates

ENV CGO_ENABLED=0 \
    GOOS=linux

WORKDIR /src

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN go build -trimpath -ldflags="-s -w" -o /out/mls-cli .

FROM gcr.io/distroless/static:nonroot

COPY --from=builder /out/mls-cli /usr/local/bin/mls-cli

USER nonroot:nonroot

# GraphQL API port used by the `serve` subcommand (MLS_SYNC_SERVER_ADDR).
EXPOSE 8080

ENTRYPOINT ["mls-cli"]
CMD ["serve"]
