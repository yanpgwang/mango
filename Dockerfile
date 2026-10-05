# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26.6-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
COPY sdk/go/go.mod ./sdk/go/go.mod
ARG GOPROXY=https://proxy.golang.org,direct
RUN --mount=type=cache,target=/go/pkg/mod GOPROXY=$GOPROXY go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY sdk/go ./sdk/go

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG REVISION=unknown
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X github.com/yanpgwang/mango/internal/buildinfo.Version=${VERSION} -X github.com/yanpgwang/mango/internal/buildinfo.Revision=${REVISION}" \
        -o /out/mango ./cmd/mango

FROM alpine:3.23
ARG VERSION=dev
ARG REVISION=unknown

LABEL org.opencontainers.image.title="Mango" \
      org.opencontainers.image.description="Independent self-hosted runtime for durable AI agents" \
      org.opencontainers.image.source="https://github.com/yanpgwang/mango" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$REVISION \
      org.opencontainers.image.licenses="Apache-2.0"

RUN apk add --no-cache ca-certificates
COPY --from=build /out/mango /usr/local/bin/mango

RUN addgroup -S -g 65532 mango && adduser -S -u 65532 -G mango mango
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/mango"]
