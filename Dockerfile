# syntax=docker/dockerfile:1

# ARGs used in a FROM must live in the global scope (before the first FROM).
# GO_VERSION is supplied by the release workflow from mise, the single source
# of truth for the toolchain (see .mise/config.toml).
ARG GO_VERSION

# ---- Go build -------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG REVISION=dev

WORKDIR /workspace
# Cache module downloads before copying source.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

# Static, stripped, reproducible binary. GOARCH is left to the platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${REVISION}" \
    -o kritik ./cmd/kritik

# ---- Runtime --------------------------------------------------------------
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/kritik /kritik
EXPOSE 8080 8081
ENTRYPOINT ["/kritik"]
