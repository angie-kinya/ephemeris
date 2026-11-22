# Egress proxy image for network.mode allowlist on macOS, Windows or a remote
# engine, where the host binary cannot be bind-mounted into a Linux container.
#   export EPHEMERIS_PROXY_IMAGE=ghcr.io/angie-kinya/ephemeris:latest
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /ephemeris ./cmd/ephemeris

FROM scratch
COPY --from=build /ephemeris /ephemeris
USER 65534:65534
ENTRYPOINT ["/ephemeris"]