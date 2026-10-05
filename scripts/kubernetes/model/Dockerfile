FROM --platform=$BUILDPLATFORM golang:1.26.6-alpine AS build
WORKDIR /src
COPY scripts/kubernetes/model/main.go .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /model main.go
FROM alpine:3.23
COPY --from=build /model /usr/local/bin/model
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/model"]
