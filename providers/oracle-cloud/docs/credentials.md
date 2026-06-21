---
title: Credentials & auth
description: How the BigFleet OCI provider authenticates — Instance Principals, OKE Workload Identity, or config-file API keys — and the least-privilege dynamic group + IAM policy it needs.
sidebar:
  order: 3
  label: Credentials & auth
---

The provider authenticates with **standard OCI auth** — nothing is hardcoded.
Pick the mode that fits where it runs (`--auth`, default `auto`):

| Mode | When | How |
|---|---|---|
| `instance_principal` | The provider runs **on an OCI instance / OKE node**. | The instance's principal; no key files. **Preferred for production.** |
| `workload_identity` | The provider runs **as an OKE pod**. | The pod's OKE Workload Identity principal; no key files. |
| `config_file` | Non-OKE clusters that can't use the above. | A mounted `~/.oci/config` + API signing key. |
| `auto` | Default. | Tries Instance Principals, falls back to the config file. |

Unlike AWS there is **no per-pod IRSA**: the identity comes from the instance or
OKE principal, and authorization is granted to a **dynamic group** that matches
that principal.

## Production: dynamic group + IAM policy

For Instance Principals or Workload Identity, create a **dynamic group** that
matches the provider's principal and an **IAM policy** granting it least-privilege
Compute permissions **scoped to one compartment**. The Terraform in
[`deploy/iam`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/oracle-cloud/deploy/iam)
does both:

```sh
cd providers/oracle-cloud/deploy/iam
tofu init && tofu apply \
  -var tenancy_ocid=ocid1.tenancy.oc1..aaaa \
  -var compartment_ocid=ocid1.compartment.oc1..bbbb \
  -var name=bigfleet-oci-eu-frankfurt-1 \
  -var trust_mode=instance_principal \
  -var provider_instance_ocid=ocid1.instance.oc1..cccc
```

The policy statements (scoped to the compartment):

```
Allow dynamic-group <DG> to manage instance-family in compartment <C>
Allow dynamic-group <DG> to use volume-family in compartment <C>
Allow dynamic-group <DG> to use virtual-network-family in compartment <C>
Allow dynamic-group <DG> to read instance-images in compartment <C>
Allow dynamic-group <DG> to use instance-agent-command-family in compartment <C>
```

Each maps to what the code calls:

- `manage instance-family` — `LaunchInstance` / `TerminateInstance` / `GetInstance`
  / `ListInstances` / `UpdateInstance` (binding tags).
- `use volume-family` — the boot volume created/attached at launch.
- `use virtual-network-family` — the VNIC/subnet attached at launch.
- `read instance-images` — resolving the base image.
- `use instance-agent-command-family` — the Oracle Cloud Agent **Run Command**
  that delivers the `Configure` bootstrap and runs `Drain` (the OCI control-plane
  analogue of AWS SSM `SendCommand`).

For **Workload Identity**, set `trust_mode=workload_identity` and pass the OKE
cluster OCID, namespace, and ServiceAccount; the dynamic group matches the
provider pod. Run the pod under the chart's ServiceAccount (`--set oci.auth=workload_identity`).

## Non-OKE: config-file / API-key Secret

For clusters that can't use Instance Principals or Workload Identity, mount a
standard `~/.oci/config` plus its signing key as a Kubernetes Secret and set
`--auth=config_file`:

```sh
kubectl -n bigfleet create secret generic bigfleet-oci-config \
  --from-file=config=$HOME/.oci/config \
  --from-file=oci_api_key.pem=$HOME/.oci/oci_api_key.pem
```

```sh
helm install ... \
  --set oci.auth=config_file \
  --set credentials.useConfigFile=true \
  --set credentials.secretName=bigfleet-oci-config
```

The chart mounts it read-only at `/home/nonroot/.oci`. The `config`'s `key_file`
must reference the mounted key path. A ready-to-edit manifest is in
[`deploy/secret/oci-config.example.yaml`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/oracle-cloud/deploy/secret).

## Good hygiene

- **Scope to one compartment, one region per process** (one registry entry per
  provider impl × region → one OCI-provider process per region).
- **Prefer Instance Principals / Workload Identity** — no long-lived keys to
  rotate or leak.
- The provider **never logs** credentials, the config file, or the bootstrap blob.
- Rotate API keys (config-file mode) on the tenancy's schedule; the provider picks
  up a remounted Secret on restart.
