# syntax=docker/dockerfile:1

# Multi-stage build for the BigFleet Latitude.sh capacity provider.
#
# The providers/latitude module is a Go submodule whose go.mod carries
#   replace github.com/intUnderflow/bigfleet-providers => ../..
# so it can only be built with the WHOLE repository in context. Build this image
# from the repository ROOT:
#
#   docker build -f providers/latitude/deploy/Dockerfile -t bigfleet-latitude:latest .

# ---- build stage -----------------------------------------------------------
FROM golang:1.26 AS build

WORKDIR /src

# Copy the whole repo so the ../.. replace directive resolves the providerkit
# (root) module from the local checkout.
COPY . .

# Warm the module cache for the latitude submodule, then build it. CGO off + a
# trimmed, stripped static binary for distroless/static.
ENV CGO_ENABLED=0 GOFLAGS=-mod=mod
RUN go -C providers/latitude mod download
RUN go -C providers/latitude build -trimpath -ldflags="-s -w" -o /out/latitude .

# ---- runtime stage ---------------------------------------------------------
# distroless/static:nonroot — no shell, no package manager, runs as uid 65532.
FROM gcr.io/distroless/static:nonroot

# gRPC CapacityProvider (--addr, default :9000) and observability (--metrics-addr,
# default :9090: /metrics, /healthz, /readyz).
EXPOSE 9000 9090

COPY --from=build /out/latitude /usr/local/bin/latitude

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/latitude"]
