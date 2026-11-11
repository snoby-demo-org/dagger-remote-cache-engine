# syntax=docker/dockerfile:1

# dcache imports Dagger's own dagql/transfer libraries, so the build must be
# against the exact pinned upstream checkout (see scripts/bootstrap.sh). Native
# macOS builds are unsupported — the imported code uses Linux syscalls.
FROM golang:1.27-bookworm AS build

ARG DAGGER_TAG=v1.0.0-beta.25
ARG DAGGER_PIN=a45241b20a5e07fbfc5954479ef0b61da843499c

RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Clone the pinned upstream FIRST: go.mod replace directives point at
# ./.upstream/dagger, so the module graph is unresolvable without it.
RUN git clone --depth 1 --branch "${DAGGER_TAG}" https://github.com/dagger/dagger /src/.upstream/dagger \
 && actual="$(git -C /src/.upstream/dagger rev-parse HEAD)" \
 && if [ "${actual}" != "${DAGGER_PIN}" ]; then \
      echo "Refusing incompatible upstream revision: ${actual}" >&2; exit 1; \
    fi

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# GOMAXPROCS/-p are capped deliberately: building the dagql graph in parallel
# can exhaust a CI runner's memory. Keep them matching the Makefile.
RUN CGO_ENABLED=0 GOMAXPROCS=4 go build -p 2 -trimpath \
      -ldflags='-s -w' -o /out/dcache ./cmd/dcache

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/dcache /dcache

# The SQLite graph database lives on a persistent volume, never in S3.
ENV DCACHE_LISTEN=:8080 \
    DCACHE_DB=/state/cache.db \
    DCACHE_POOL=shared

VOLUME ["/state"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/dcache"]
