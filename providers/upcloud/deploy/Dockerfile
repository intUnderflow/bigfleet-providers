# syntax=docker/dockerfile:1

# Multi-stage build for the BigFleet UpCloud capacity provider.
#
# The providers/upcloud module is a Go submodule whose go.mod carries
#   replace github.com/intUnderflow/bigfleet-providers => ../..
# so it can only be built with the WHOLE repository in context. Build this image
# from the repository ROOT:
#
#   docker build -f providers/upcloud/deploy/Dockerfile -t bigfleet-upcloud:latest .

# ---- build stage -----------------------------------------------------------
FROM golang:1.26 AS build

WORKDIR /src

# Copy the whole repo so the ../.. replace directive resolves the providerkit
# (root) module from the local checkout.
COPY . .

# Warm the module cache for the upcloud submodule, then build it. CGO off + a
# trimmed, stripped static binary for distroless/static.
ENV CGO_ENABLED=0 GOFLAGS=-mod=mod
RUN go -C providers/upcloud mod download
RUN go -C providers/upcloud build -trimpath -ldflags="-s -w" -o /out/upcloud .

# ---- runtime stage ---------------------------------------------------------
# distroless/static:nonroot — no shell, no package manager, runs as uid 65532.
FROM gcr.io/distroless/static:nonroot

# gRPC CapacityProvider (--addr, default :9000) and observability (--metrics-addr,
# default :9090: /metrics, /healthz, /readyz). The bootstrap blob is delivered
# OUTBOUND over SSH (port 22 to the server), so no extra inbound port is needed.
EXPOSE 9000 9090

COPY --from=build /out/upcloud /usr/local/bin/upcloud

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/upcloud"]
