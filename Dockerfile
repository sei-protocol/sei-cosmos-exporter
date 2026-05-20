# syntax=docker/dockerfile:1.7

# ----- builder ------------------------------------------------------------
# Pinned to 1.20 because cosmos-sdk v0.45.4 (this repo's transitive dep) does
# not compile cleanly under 1.21+ stdlib changes. Bump in lock-step with any
# cosmos-sdk upgrade.
FROM golang:1.20-alpine@sha256:e47f121850f4e276b2b210c56df3fda9191278dd84a3a442bfe0b09934462a8f AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -buildvcs=false \
    -o /out/sei-cosmos-exporter ./

# ----- runtime ------------------------------------------------------------
# Provides /bin/bash for the wait-then-exec wrapper in the pod spec.
# UID/GID 65532 matches sei-k8s-controller's sidecarSecurityContext.
FROM docker.io/ubuntu:24.04@sha256:c4a8d5503dfb2a3eb8ab5f807da5bc69a85730fb49b5cfca2330194ebcc41c7b

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/* && \
    groupadd --system --gid 65532 nonroot && \
    useradd --system --uid 65532 --gid 65532 --shell /sbin/nologin --no-create-home nonroot

COPY --from=builder /out/sei-cosmos-exporter /usr/local/bin/sei-cosmos-exporter

EXPOSE 9300

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/sei-cosmos-exporter"]

# K8s pod spec overrides both ENTRYPOINT and CMD with a bash wait wrapper.
CMD ["--denom", "usei", \
     "--denom-coefficient", "1000000", \
     "--bech-prefix", "sei", \
     "--listen-address", ":9300"]
