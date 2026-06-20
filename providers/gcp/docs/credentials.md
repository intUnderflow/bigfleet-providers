---
title: Credentials & auth
description: The GCP auth model — a least-privilege provider service account via Workload Identity (or a key-file Secret), and the separate identity the launched instances run as.
sidebar:
  order: 3
  label: Credentials & auth
---

The GCP provider authenticates to Compute Engine via **Application Default
Credentials (ADC)** — no token flag. On GKE that means **Workload Identity**
(the GKE analogue of AWS IRSA: bind the Kubernetes ServiceAccount to a Google
service account, no key files); off-GKE it means a key-file Secret via
`GOOGLE_APPLICATION_CREDENTIALS`.

There are **two identities**, and keeping them separate is the whole point:

| Identity | Who | What it does |
|---|---|---|
| **Provider service account** | the provider process | calls `instances.insert/delete/reset`, reads instances + machine types, sets metadata/labels |
| **Instance service account** | the VMs the provider launches | whatever your nodes need at runtime (`--instance-service-account`); **not** the provider's identity |

The provider's identity must never be the node identity. A node should not be
able to create or delete other nodes.

## 1. The provider service account & role

The provider needs exactly one predefined role on the target project:

| Role | Why | Lifecycle call |
|---|---|---|
| `roles/compute.instanceAdmin.v1` | create / delete / reset instances, set metadata + labels, read instances and machine types | `Create`, `Delete`, `Configure`, `Drain`, `Describe` |
| `roles/iam.serviceAccountUser` on the **instance** SA | lets the provider launch instances that *run as* `--instance-service-account` | `Create` (only when `--instance-service-account` is set) |

That's the least-privilege set. `instanceAdmin.v1` already covers
`compute.instances.{insert,delete,reset,setMetadata,setLabels,get,list}` and
`compute.machineTypes.get`. Map each grant to the call that needs it, and grant
nothing more. (If you later add live Cloud Billing pricing, add a billing viewer
role then — this provider uses a pinned price table, so it is not needed.)

The Terraform under
[`deploy/sa/`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/gcp/deploy/sa)
creates the provider service account, binds the role on the project, and wires
the Workload-Identity binding:

```sh
cd providers/gcp/deploy/sa
terraform init
terraform apply \
  -var 'project_id=my-gcp-project' \
  -var 'name=bigfleet-gcp-us-central1' \
  -var 'k8s_namespace=bigfleet' \
  -var 'k8s_service_account=bigfleet-gcp' \
  -var 'instance_service_account=bigfleet-node@my-gcp-project.iam.gserviceaccount.com'
# -> outputs provider_service_account_email
```

## 2. Workload Identity (GKE — preferred, no keys)

Workload Identity lets the Kubernetes ServiceAccount the pod runs as impersonate
the Google service account — so there is **no key file** anywhere. Two bindings,
both created by the Terraform above:

1. an IAM policy binding granting the Kubernetes SA the
   `roles/iam.workloadIdentityUser` role on the Google SA, for the member
   `serviceAccount:PROJECT.svc.id.goog[NAMESPACE/KSA_NAME]`;
2. the Kubernetes ServiceAccount annotated
   `iam.gke.io/gcp-service-account: <provider-sa-email>`.

The Helm chart writes that annotation when you set `serviceAccount.gcpServiceAccount`:

```yaml
serviceAccount:
  create: true
  name: bigfleet-gcp
  gcpServiceAccount: bigfleet-gcp-us-central1@my-gcp-project.iam.gserviceaccount.com
```

Ensure Workload Identity is enabled on the cluster and node pool. Once bound,
the provider picks up credentials from the metadata server automatically — ADC
needs no env var on GKE.

## 3. Key-file fallback (off-GKE)

Off-GKE (or on a cluster without Workload Identity), create a key for the
provider service account, store it as a Secret, and mount it as
`GOOGLE_APPLICATION_CREDENTIALS`:

```sh
gcloud iam service-accounts keys create key.json \
  --iam-account bigfleet-gcp-us-central1@my-gcp-project.iam.gserviceaccount.com
kubectl -n bigfleet create secret generic bigfleet-gcp-key --from-file=key.json
```

```yaml
credentials:
  secretName: bigfleet-gcp-key   # mounted at /var/secrets/google/key.json
                                 # GOOGLE_APPLICATION_CREDENTIALS is set for you
```

A ready-to-edit Secret manifest is in
[`deploy/secret/gcp-key.example.yaml`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/gcp/deploy/secret).
Prefer Workload Identity — key files are long-lived credentials you must rotate
and protect.

## 4. The instance service account

`--instance-service-account` is the identity your **nodes** run as — unrelated to
the provider's. Give it only what the workloads on the node need (often a
minimal SA, or the project default). The provider needs
`roles/iam.serviceAccountUser` on this SA to launch instances that run as it
(the Terraform grants it). If you omit `--instance-service-account`, instances
use the project default compute service account.

## 5. Rotate

- **Workload Identity** has no key to rotate — that's its main benefit. Rotating
  the *role binding* is a Terraform change; the running process picks up new
  permissions immediately (no restart needed for an additive grant).
- **Key files** are long-lived: create a new key, update the Secret, roll the
  Deployment (`kubectl -n bigfleet rollout restart deploy/…`) so the process
  re-reads it, then delete the old key. Because the persisted `--state` file is
  the restart path and transitions run on minute-scale timeouts, a rolling
  restart is safe.

## What the credentials are used for

Every GCE call the provider makes, and the role permission it needs:

| Call | Permission | When |
|---|---|---|
| `Instances.Insert` | `compute.instances.create` (+ `iam.serviceAccounts.actAs` for the node SA) | Create |
| `Instances.Delete` | `compute.instances.delete` | Delete |
| `Instances.AggregatedList` | `compute.instances.list` | Describe / reconcile |
| `Instances.SetMetadata` + `Reset` | `compute.instances.setMetadata`, `compute.instances.reset` | Configure / Drain |
| `Instances.SetLabels` | `compute.instances.setLabels` | Configure / Drain (binding label) |
| `MachineTypes.Get` | `compute.machineTypes.get` | `allocatable` resolution |

No credential is ever logged. See [Security](/providers/gcp/security/) for the
full trust model.
