FROM --platform=$BUILDPLATFORM golang:1.26.6-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
COPY sdk/go/go.mod ./sdk/go/go.mod
COPY . .
ARG VERSION=dev
ARG REVISION=unknown
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/yanpgwang/mango/internal/buildinfo.Version=${VERSION} -X github.com/yanpgwang/mango/internal/buildinfo.Revision=${REVISION}" \
    -o /out/mango-worker ./cmd/mango-worker

FROM debian:12-slim
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="Mango self-hosted worker" \
      org.opencontainers.image.description="Operator-run reference worker for Mango Environment Work" \
      org.opencontainers.image.source="https://github.com/yanpgwang/mango" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$REVISION \
      org.opencontainers.image.licenses="Apache-2.0"
RUN apt-get update && apt-get install -y --no-install-recommends \
      bash ca-certificates coreutils findutils git grep python3 \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 65532 mango \
    && useradd --uid 65532 --gid 65532 --home-dir /workspace --no-create-home mango \
    && mkdir -p /workspace /mnt/memory \
    && chown 65532:65532 /workspace /mnt/memory

COPY --from=build /out/mango-worker /usr/local/bin/mango-worker
WORKDIR /workspace
USER 65532:65532
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/mango-worker", "run"]
