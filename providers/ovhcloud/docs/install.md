---
title: Install & deploy
description: Deploy the BigFleet OVHcloud Public Cloud provider — build the image, create the OpenStack user, and install the Helm chart one release per region.
sidebar:
  order: 1
  label: Install & deploy
---

The provider deploys as a container image plus a Helm chart, **one release per OVH
region**. This page is the end-to-end path; for every flag and the offerings
schema see [Configuration](/providers/ovhcloud/configuration/), and for the
credentials see [Credentials](/providers/ovhcloud/credentials/).

## Prerequisites

- A Kubernetes cluster to run the provider in (next to BigFleet), and `helm`.
- An OVH Public Cloud project, and the region(s) you want capacity in.
- A base image (UUID) that joins your cluster and ships the bootstrap hook (see
  [Configuration → the image hook contract](/providers/ovhcloud/configuration/#the-image-hook-contract)).

## 1. Build & push the image

The `providers/ovhcloud` module resolves the shared `providerkit` module from the
repo root via `replace ... => ../..`, so build from the **repository root**:

```sh
docker build -f providers/ovhcloud/deploy/Dockerfile -t ghcr.io/intunderflow/bigfleet-ovhcloud:0.1.0 .
docker push ghcr.io/intunderflow/bigfleet-ovhcloud:0.1.0
```

The multi-stage build ships a `distroless/static:nonroot` image (uid 65532, no
shell) exposing the gRPC port (`9000`) and the metrics/health port (`9090`).

## 2. Create the OpenStack user + keypair

```sh
# authenticated as a project admin (openrc sourced):
providers/ovhcloud/deploy/openstack/create-scoped-user.sh bigfleet-gra <PROJECT_ID> GRA
```

This creates a project-scoped user (project `member` role), an ed25519 keypair
(public half registered in OpenStack, private half kept for the Secret), and
prints the `kubectl create secret` commands. Create both Secrets:

```sh
kubectl -n bigfleet create secret generic bigfleet-ovh-gra-os \
  --from-literal=OS_AUTH_URL=https://auth.cloud.ovh.net/v3 \
  --from-literal=OS_IDENTITY_API_VERSION=3 \
  --from-literal=OS_USERNAME=... --from-literal=OS_PASSWORD=... \
  --from-literal=OS_PROJECT_ID=... \
  --from-literal=OS_USER_DOMAIN_NAME=Default --from-literal=OS_PROJECT_DOMAIN_NAME=Default \
  --from-literal=OS_REGION_NAME=GRA

kubectl -n bigfleet create secret generic bigfleet-ovh-ssh \
  --from-file=id_ed25519=./bigfleet-ovh
```

Full guidance — scoping, rotation, never-logged — is on
[Credentials](/providers/ovhcloud/credentials/).

## 3. Write your offerings

An offerings file is the quota of capacity the provider may provision. See
[Configuration → Offerings](/providers/ovhcloud/configuration/#offerings) for the
schema. A minimal `offerings.gra.json`:

```json
[
  { "flavor": "b2-7",  "region": "GRA", "count": 8,  "resources": { "cpu": "1", "memory": "2Gi" } },
  { "flavor": "c2-15", "region": "GRA", "count": 16, "resources": { "cpu": "2", "memory": "4Gi" } }
]
```

## 4. Install the chart (one release per region)

```sh
helm install ovh-gra providers/ovhcloud/deploy/helm \
  --namespace bigfleet --create-namespace \
  --set image.repository=ghcr.io/intunderflow/bigfleet-ovhcloud \
  --set image.tag=0.1.0 \
  --set region=GRA \
  --set regionB=SBG \
  --set provider=ovh-public-GRA \
  --set ovh.image=<BASE_IMAGE_UUID> \
  --set ovh.keyName=bigfleet-ovh \
  --set ovh.eurToUSD=1.08 \
  --set openstack.secretName=bigfleet-ovh-gra-os \
  --set ssh.secretName=bigfleet-ovh-ssh \
  --set-file offerings.content=offerings.gra.json \
  --set state.enabled=true --set state.persistence.enabled=true --set state.persistence.size=1Gi
```

Repeat with a distinct release name, `region`, `provider`, OS_* Secret, and
offerings file for each region. **Never scale a release past `replicas: 1`** — one
process owns one region's state.

Register the resulting `Service` (the `grpc` port) as a coordinator entry in
BigFleet — one entry per (implementation × region), e.g. `ovh-public-GRA`,
`ovh-public-SBG`.

## 5. Verify

```sh
kubectl -n bigfleet get pods -l app.kubernetes.io/instance=ovh-gra
kubectl -n bigfleet port-forward deploy/ovh-gra-bigfleet-ovhcloud 9090:9090 &
curl -s localhost:9090/readyz        # -> ready
curl -s localhost:9090/metrics | grep bigfleet_ovh
```

See [Observability](/providers/ovhcloud/observability/) for the metrics and probes,
and [Troubleshooting](/providers/ovhcloud/troubleshooting/) if a machine sticks in
`CREATING`/`CONFIGURING` or lands in `FAILED`.

## TLS / mTLS

The gRPC listener is plaintext by default (fine for trusted in-cluster traffic).
For exposed production, enable mTLS with a Secret holding `tls.crt`, `tls.key`,
`ca.crt`:

```sh
--set tls.enabled=true --set tls.mtls=true --set tls.secretName=bigfleet-ovh-tls
```

See [Security](/providers/ovhcloud/security/).
