# Terraform for the BigFleet Scaleway provider credentials.
#
# Scaleway auth is API-key based (not IAM roles like AWS): the provider presents
# an access key + secret key belonging to an IAM application, scoped by an IAM
# policy. Scaleway IAM grants are PRODUCT-scoped permission sets (no per-call
# action lists), so this module grants the narrowest predefined sets that cover
# the code's calls, scoped to a single project. It creates:
#
#   * an IAM application (the machine identity the provider runs as),
#   * an IAM policy granting those permission sets for one project, and
#   * an API key for the application (access key + secret key outputs).
#
# Deliver the outputs to the cluster as a Kubernetes Secret (see
# ../secret/scaleway-creds.example.yaml and ../../docs/credentials.md). Rotate by
# tainting scaleway_iam_api_key.provider and re-applying.
#
# One application/key per region is recommended (one provider process per region).

terraform {
  required_version = ">= 1.5"
  required_providers {
    scaleway = {
      source  = "scaleway/scaleway"
      version = ">= 2.30"
    }
  }
}

variable "name" {
  description = "Name prefix for the application/policy/key (e.g. bigfleet-scaleway-fr-par)."
  type        = string
  default     = "bigfleet-scaleway"
}

variable "organization_id" {
  description = "Scaleway organization id the policy is scoped to."
  type        = string
}

variable "project_id" {
  description = "Scaleway project id the provider operates in (the policy is scoped to this project)."
  type        = string
}

variable "enable_elastic_metal" {
  description = "Also grant the Elastic Metal (bare-metal) permission set. NOTE: the real Elastic Metal backend is not yet built into the provider binary (it runs on the fake backend only), so leave this false for any real deployment; it exists for when the Elastic Metal backend ships."
  type        = bool
  default     = false
}

# ---------------------------------------------------------------------------
# Application (the machine identity)
# ---------------------------------------------------------------------------
resource "scaleway_iam_application" "provider" {
  name        = "${var.name}-app"
  description = "BigFleet Scaleway capacity provider (Instances + Block Storage access, scoped to one project)."
}

# ---------------------------------------------------------------------------
# Policy — scoped to one project, using Scaleway's predefined permission sets
# ---------------------------------------------------------------------------
# Scaleway IAM authorises by PRODUCT-scoped permission sets, not per-API-call
# rules (there is no AWS-style action list), so "least privilege" here means: the
# narrowest predefined sets that cover the calls the code makes, scoped to a
# single project. The provider needs:
#   - InstancesFullAccess   — create/get/list/delete servers, server actions,
#                             user-data, and server types/pricing.
#   - BlockStorageFullAccess — delete the boot Block Storage (sbs) volume on
#                             Delete (the instance API cannot delete sbs volumes),
#                             so this is required to avoid a storage leak.
# BareMetalFullAccess is added only when enable_elastic_metal is set (note: the
# real Elastic Metal backend is not yet built — see var.enable_elastic_metal).
locals {
  permission_sets = concat(
    ["InstancesFullAccess", "BlockStorageFullAccess"],
    var.enable_elastic_metal ? ["BareMetalFullAccess"] : [],
  )
}

resource "scaleway_iam_policy" "provider" {
  name           = "${var.name}-policy"
  description    = "Least-privilege policy for the BigFleet Scaleway capacity provider."
  application_id = scaleway_iam_application.provider.id

  rule {
    project_ids          = [var.project_id]
    permission_set_names = local.permission_sets
  }
}

# ---------------------------------------------------------------------------
# API key (access key + secret key)
# ---------------------------------------------------------------------------
resource "scaleway_iam_api_key" "provider" {
  application_id     = scaleway_iam_application.provider.id
  description        = "BigFleet Scaleway provider API key."
  default_project_id = var.project_id
}

# ---------------------------------------------------------------------------
# Outputs — wire these into the Kubernetes Secret (see ../secret).
# ---------------------------------------------------------------------------
output "access_key" {
  description = "SCW_ACCESS_KEY for the provider Secret."
  value       = scaleway_iam_api_key.provider.access_key
}

output "secret_key" {
  description = "SCW_SECRET_KEY for the provider Secret. Sensitive — write straight into the Secret, never to logs/VCS."
  value       = scaleway_iam_api_key.provider.secret_key
  sensitive   = true
}

output "project_id" {
  description = "SCW_DEFAULT_PROJECT_ID for the provider Secret."
  value       = var.project_id
}
