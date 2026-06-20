# Terraform for the BigFleet GCP provider identity. Creates the provider's
# least-privilege Google service account, binds the Compute role on the project,
# and wires Workload Identity so the Kubernetes ServiceAccount can impersonate it
# with no key file (the GKE analogue of AWS IRSA).
#
# The role mirrors the credentials doc page and the actions the code calls:
#   compute.instances.{insert,delete,reset,setMetadata,setLabels,get,list} and
#   compute.machineTypes.get  → all covered by roles/compute.instanceAdmin.v1
#   iam.serviceAccounts.actAs  → roles/iam.serviceAccountUser on the NODE SA
#     (only needed when --instance-service-account is set).
#
# One provider service account per region is recommended (one process per region).

terraform {
  required_version = ">= 1.5"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">= 5.0"
    }
  }
}

variable "project_id" {
  description = "GCP project the provider manages capacity in."
  type        = string
}

variable "name" {
  description = "Name prefix for the provider service account (e.g. bigfleet-gcp-us-central1). Becomes the SA account id."
  type        = string
  default     = "bigfleet-gcp"
}

variable "k8s_namespace" {
  description = "Namespace of the provider's Kubernetes ServiceAccount (Workload Identity)."
  type        = string
  default     = "bigfleet"
}

variable "k8s_service_account" {
  description = "Name of the provider's Kubernetes ServiceAccount (Workload Identity)."
  type        = string
  default     = "bigfleet-gcp"
}

variable "instance_service_account" {
  description = "Email of the service account the LAUNCHED INSTANCES run as. Empty disables the serviceAccountUser binding (only needed when --instance-service-account is set)."
  type        = string
  default     = ""
}

# ---------------------------------------------------------------------------
# Provider service account
# ---------------------------------------------------------------------------
resource "google_service_account" "provider" {
  project      = var.project_id
  account_id   = var.name
  display_name = "BigFleet GCP capacity provider (${var.name})"
  description  = "Least-privilege identity for the BigFleet GCE capacity provider."
}

# Least-privilege Compute role: create/delete/reset instances, set metadata +
# labels, read instances and machine types.
resource "google_project_iam_member" "instance_admin" {
  project = var.project_id
  role    = "roles/compute.instanceAdmin.v1"
  member  = "serviceAccount:${google_service_account.provider.email}"
}

# serviceAccountUser on the NODE service account — lets the provider launch
# instances that RUN AS --instance-service-account. Only created when set.
resource "google_service_account_iam_member" "act_as_node_sa" {
  count              = var.instance_service_account == "" ? 0 : 1
  service_account_id = "projects/${var.project_id}/serviceAccounts/${var.instance_service_account}"
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.provider.email}"
}

# ---------------------------------------------------------------------------
# Workload Identity: let the Kubernetes SA impersonate the Google SA.
# ---------------------------------------------------------------------------
resource "google_service_account_iam_member" "workload_identity" {
  service_account_id = google_service_account.provider.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.k8s_namespace}/${var.k8s_service_account}]"
}

# ---------------------------------------------------------------------------
# Outputs
# ---------------------------------------------------------------------------
output "provider_service_account_email" {
  description = "Email of the provider service account. Set this on the chart via serviceAccount.gcpServiceAccount (and annotate the Kubernetes SA for Workload Identity)."
  value       = google_service_account.provider.email
}

output "workload_identity_member" {
  description = "The Workload Identity member bound to the Google SA."
  value       = "serviceAccount:${var.project_id}.svc.id.goog[${var.k8s_namespace}/${var.k8s_service_account}]"
}
