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
# distroless nonroot pins UID/GID 65532 — matches the controller's
# sidecarSecurityContext (RunAsNonRoot + RunAsUser 65532) so the
# pod's fsGroup propagation works without extra config.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:a9329520abc449e3b14d5bc3a6ffae065bdde0f02667fa10880c49b35c109fd1

COPY --from=builder /out/sei-cosmos-exporter /usr/local/bin/sei-cosmos-exporter

EXPOSE 9300

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/sei-cosmos-exporter"]

# Sei-flavored defaults. K8s pod spec passes explicit Args which overrides
# this — see sei-k8s-controller buildCosmosExporterContainer.
CMD ["--denom", "usei", \
     "--denom-coefficient", "1000000", \
     "--bech-prefix", "sei", \
     "--listen-address", ":9300"]
