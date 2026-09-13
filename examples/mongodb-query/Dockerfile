FROM golang:1.26.6-bookworm AS build
WORKDIR /src
COPY sdk/go/ ./sdk/go/
COPY examples/mongodb-query/*.go ./example/
RUN cd /src/example && go mod init mango-mongodb-example \
    && go mod edit -require=github.com/yanpgwang/mango/sdk/go@v0.0.0 \
    -replace=github.com/yanpgwang/mango/sdk/go=/src/sdk/go \
    && go mod tidy \
    && CGO_ENABLED=0 go build -trimpath -o /out/mongodb-query .

FROM debian:12-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates coreutils findutils grep python3 python3-pymongo python3-dnspython \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /workspace && chown 65532:65532 /workspace
COPY --from=build /out/mongodb-query /usr/local/bin/mongodb-query
WORKDIR /workspace
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/mongodb-query"]
