# syntax=docker/dockerfile:1

# Multi-stage build for the BigFleet DigitalOcean capacity provider.
#
# The providers/digitalocean module is a Go submodule whose go.mod carries
#   replace github.com/intUnderflow/bigfleet-providers => ../..
# so it can only be built with the WHOLE repository in context. Build this
# image from the repository ROOT:
#
#   docker build -f providers/digitalocean/deploy/Dockerfile -t bigfleet-digitalocean:latest .

# ---- build stage -----------------------------------------------------------
FROM golang:1.26 AS build

WORKDIR /src

# Copy the whole repo so the ../.. replace directive resolves the providerkit
# (root) module from the local checkout.
COPY . .

# Warm the module cache for the digitalocean submodule, then build it. CGO off +
# a trimmed, stripped static binary for distroless/static.
ENV CGO_ENABLED=0 GOFLAGS=-mod=mod
RUN go -C providers/digitalocean mod download
RUN go -C providers/digitalocean build -trimpath -ldflags="-s -w" -o /out/digitalocean .

# ---- runtime stage ---------------------------------------------------------
# distroless/static:nonroot — no shell, no package manager, runs as uid 65532.
FROM gcr.io/distroless/static:nonroot

# gRPC CapacityProvider (--addr, default :9000), observability (--metrics-addr,
# default :9090: /metrics, /healthz, /readyz), and the on-host agent bootstrap
# channel (--bootstrap-addr, e.g. :9443) used only by the real backend.
EXPOSE 9000 9090 9443

COPY --from=build /out/digitalocean /usr/local/bin/digitalocean

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/digitalocean"]
