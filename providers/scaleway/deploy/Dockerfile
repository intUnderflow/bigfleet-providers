# syntax=docker/dockerfile:1

# Multi-stage build for the BigFleet Scaleway capacity provider.
#
# The providers/scaleway module is a Go submodule whose go.mod carries
#   replace github.com/intUnderflow/bigfleet-providers => ../..
# so it can only be built with the WHOLE repository in context. Build this
# image from the repository ROOT:
#
#   docker build -f providers/scaleway/deploy/Dockerfile -t bigfleet-scaleway:latest .

# ---- build stage -----------------------------------------------------------
FROM golang:1.26 AS build

WORKDIR /src

# Copy the whole repo so the ../.. replace directive resolves the providerkit
# (root) module from the local checkout.
COPY . .

# Warm the module cache for the scaleway submodule, then build it. CGO off + a
# trimmed, stripped static binary for distroless/static.
ENV CGO_ENABLED=0 GOFLAGS=-mod=mod
RUN go -C providers/scaleway mod download
RUN go -C providers/scaleway build -trimpath -ldflags="-s -w" -o /out/scaleway .

# ---- runtime stage ---------------------------------------------------------
# distroless/static:nonroot — no shell, no package manager, runs as uid 65532.
FROM gcr.io/distroless/static:nonroot

# gRPC CapacityProvider (--addr, default :9000) and observability (--metrics-addr,
# default :9090: /metrics, /healthz, /readyz).
EXPOSE 9000 9090

COPY --from=build /out/scaleway /usr/local/bin/scaleway

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/scaleway"]
