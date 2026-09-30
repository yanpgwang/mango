# MinIO no longer distributes the community image used by this development stack.
# Build its last public release from an immutable Go module revision. Go's module
# checksum database verifies the source archive; no hosted credentials are used.
FROM golang:1.26.6-bookworm AS build

ENV MINIO_SOURCE_VERSION=v0.0.0-20251015172955-9e49d5e7a648
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go install -trimpath github.com/minio/minio@${MINIO_SOURCE_VERSION} \
    && mkdir -p /out \
    && cp /go/bin/minio /out/minio \
    && cp /go/pkg/mod/github.com/minio/minio@${MINIO_SOURCE_VERSION}/LICENSE /out/LICENSE

FROM debian:12-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
LABEL org.opencontainers.image.source="https://github.com/minio/minio" \
      org.opencontainers.image.revision="9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a" \
      org.opencontainers.image.licenses="AGPL-3.0-only"
COPY --from=build /out/minio /usr/local/bin/minio
COPY --from=build /out/LICENSE /usr/share/licenses/minio/LICENSE
ENTRYPOINT ["/usr/local/bin/minio"]
