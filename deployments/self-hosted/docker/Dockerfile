FROM golang:1.26.6-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
COPY sdk/go/go.mod ./sdk/go/go.mod
COPY . .
ARG VERSION=dev
ARG REVISION=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.revision=${REVISION}" \
    -o /out/mango-worker ./cmd/mango-worker

FROM debian:12-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      bash ca-certificates coreutils findutils git grep python3 \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 65532 mango \
    && useradd --uid 65532 --gid 65532 --home-dir /workspace --no-create-home mango \
    && mkdir -p /workspace \
    && chown 65532:65532 /workspace

COPY --from=build /out/mango-worker /usr/local/bin/mango-worker
WORKDIR /workspace
USER 65532:65532
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/mango-worker", "run"]
