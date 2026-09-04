# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26.6-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
COPY sdk/go/go.mod ./sdk/go/go.mod
ARG GOPROXY=https://proxy.golang.org,direct
RUN --mount=type=cache,target=/go/pkg/mod GOPROXY=$GOPROXY go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" \
        -o /out/mango ./cmd/mango

FROM alpine:3.23
ARG VERSION=dev
ARG REVISION=unknown

LABEL org.opencontainers.image.title="Mango" \
      org.opencontainers.image.description="Self-hosted Managed Agents-compatible runtime" \
      org.opencontainers.image.source="https://github.com/yanpgwang/mango" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$REVISION \
      org.opencontainers.image.licenses="Apache-2.0"

RUN apk add --no-cache ca-certificates
COPY --from=build /out/mango /usr/local/bin/mango

RUN addgroup -S mango && adduser -S -G mango mango
USER mango

ENTRYPOINT ["/usr/local/bin/mango"]
