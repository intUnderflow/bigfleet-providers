# Deploying the BigFleet Proxmox provider

This directory holds everything needed to run the provider against a real
Proxmox VE cluster: the container image, a Helm chart, the API-token + CA
Secrets, and the host-side least-privilege token setup.

## 1. Prepare a VM template

On the Proxmox cluster, build a golden VM and convert it to a template:

- install and **enable `qemu-guest-agent`** (the provider delivers the bootstrap
  and drains over the guest agent),
- preinstall **kubelet** + the in-image bootstrap hook the cluster operator
  ships (a "wait for bootstrap, then run it" unit is fine),
- do **not** bake any cluster-join secret into the template — the secret is
  delivered later over the guest agent, never via the image or cloud-init,
- convert it to a template and note its **VMID** (`--template-vmid`).

## 2. Create a least-privilege API token

On a cluster node, as root:

```sh
./host-setup/setup-token.sh
```

This creates a dedicated user, a custom least-privilege role, a resource pool,
and an API token bound to the pool. Note the printed token secret.

## 3. Create the Kubernetes Secrets

Put the token secret and the cluster CA (`/etc/pve/pve-root-ca.pem`) into
Secrets — see [`secret/proxmox-credentials.example.yaml`](secret/proxmox-credentials.example.yaml).
TLS verification is mandatory; mount the CA (`--proxmox-ca-file`) or pin the cert
fingerprint (`--proxmox-tls-fingerprint`). There is no skip-verify option.

## 4. Build the image (from the repo root)

```sh
docker build -f providers/proxmox/deploy/Dockerfile -t ghcr.io/intunderflow/bigfleet-proxmox:dev .
```

The build must run from the repository root so the multi-module
`replace github.com/intUnderflow/bigfleet-providers => ../..` resolves the
`providerkit` (root) module from the local checkout.

## 5. Install the chart (one release per cluster)

```sh
helm install proxmox-dc1 ./helm \
  --set provider=proxmox-dc1 \
  --set proxmox.apiURL=https://pve1.dc1.example:8006/api2/json \
  --set proxmox.tokenID='bigfleet@pve!autoscaler' \
  --set proxmox.nodes='pve-1,pve-2,pve-3' \
  --set proxmox.templateVMID=9000 \
  --set proxmox.pool=bigfleet \
  --set credentials.token.secretName=bigfleet-proxmox-token \
  --set credentials.ca.secretName=bigfleet-proxmox-ca
```

Point a BigFleet shard at the Service (`--provider-addr`). For production, enable
mTLS on the gRPC listener (`tls.enabled=true`, `tls.mtls=true`) so only
authorized shards connect, and enable the durable state PVC
(`state.enabled=true`, `state.persistence.enabled=true`) so fence marks,
bindings, and inventory survive restarts.

See [`../docs/`](../docs) for the full operator documentation.
