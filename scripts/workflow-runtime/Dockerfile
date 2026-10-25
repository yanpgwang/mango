# Research images only: neither target is a Mango runtime or release artifact.
FROM debian:bookworm-slim@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8 AS build
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl gcc make libc6-dev xz-utils && rm -rf /var/lib/apt/lists/*
WORKDIR /build
RUN curl --fail --location --silent --show-error https://bellard.org/quickjs/quickjs-2026-06-04.tar.xz -o source.tar.xz \
    && echo 'b376e839b322978313d929fd20663b11ba58b75df5a46c126dd19ea2fa70ad2a  source.tar.xz' | sha256sum -c - \
    && tar -xJf source.tar.xz
COPY engines/quickjs.c /build/quickjs-2026-06-04/study.c
WORKDIR /build/quickjs-2026-06-04
RUN make -j2 libquickjs.a && gcc -O2 -I. study.c libquickjs.a -lm -ldl -lpthread -o /build/evaluator

FROM debian:bookworm-slim@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8 AS quickjs
COPY --from=build /build/evaluator /evaluator
COPY --from=build /build/quickjs-2026-06-04/LICENSE /usr/share/licenses/quickjs/LICENSE
COPY realm.js /realm.js
USER 65532:65532
ENTRYPOINT ["/evaluator"]

FROM node:22.17.0-bookworm-slim@sha256:b04ce4ae4e95b522112c2e5c52f781471a5cbc3b594527bcddedee9bc48c03a0 AS node
COPY realm.js node.cjs /
USER 65532:65532
ENTRYPOINT ["node", "--max-old-space-size=48", "--stack-size=8192", "/node.cjs"]
