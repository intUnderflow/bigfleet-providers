# Terraform / OpenTofu for the BigFleet OCI provider authorization. OCI has no
# AWS-style role+policy; instead a DYNAMIC GROUP matches the principal the
# provider runs as (an OCI instance, or an OKE pod via Workload Identity) and an
# IAM POLICY grants that group the least-privilege Compute permissions, scoped to
# ONE compartment.
#
# The policy mirrors the actions the code calls:
#   manage instance-family          — LaunchInstance / TerminateInstance / Get / List / UpdateInstance
#   use volume-family               — boot volumes created/attached at launch
#   use virtual-network-family      — the VNIC/subnet attached at launch
#   read instance-images            — resolve the base image
#   use instance-agent-command-family — Oracle Cloud Agent Run Command (Configure/Drain bootstrap delivery)
#
# One dynamic group + policy per region/compartment is recommended (one provider
# process per region).
#
# NOTE: authored against the AWS provider's iam/ module; verify with
# `tofu validate` / `terraform validate` in your environment (the OCI Terraform
# provider was not available in the build sandbox).

terraform {
  required_version = ">= 1.5"
  required_providers {
    oci = {
      source  = "oracle/oci"
      version = ">= 5.0"
    }
  }
}

variable "tenancy_ocid" {
  description = "Tenancy OCID (dynamic groups and policies live in the tenancy / root compartment by default)."
  type        = string
}

variable "compartment_ocid" {
  description = "Compartment OCID the provider operates in (Compute scoped here)."
  type        = string
}

variable "name" {
  description = "Name prefix for the dynamic group and policy (e.g. bigfleet-oci-eu-frankfurt-1)."
  type        = string
  default     = "bigfleet-oci"
}

variable "trust_mode" {
  description = "How the provider obtains its principal: 'instance_principal' (provider runs on an OCI instance) or 'workload_identity' (provider runs as an OKE pod)."
  type        = string
  default     = "instance_principal"
  validation {
    condition     = contains(["instance_principal", "workload_identity"], var.trust_mode)
    error_message = "trust_mode must be 'instance_principal' or 'workload_identity'."
  }
}

# --- instance_principal inputs ----------------------------------------------
variable "provider_instance_ocid" {
  description = "OCID of the instance the provider runs on (instance_principal mode). The dynamic group matches this instance."
  type        = string
  default     = ""
}

# --- workload_identity inputs -----------------------------------------------
variable "oke_cluster_ocid" {
  description = "OKE cluster OCID whose pods run the provider (workload_identity mode)."
  type        = string
  default     = ""
}

variable "workload_namespace" {
  description = "Kubernetes namespace of the provider pod (workload_identity mode)."
  type        = string
  default     = "bigfleet"
}

variable "workload_service_account" {
  description = "Kubernetes ServiceAccount of the provider pod (workload_identity mode)."
  type        = string
  default     = "bigfleet-oracle-cloud"
}

# ---------------------------------------------------------------------------
# Dynamic group — the principal the provider runs as.
# ---------------------------------------------------------------------------
locals {
  matching_rule = var.trust_mode == "instance_principal" ? (
    "ALL {instance.id = '${var.provider_instance_ocid}'}"
    ) : (
    # Workload Identity: match pods of the given OKE cluster running under the
    # named namespace + ServiceAccount.
    "ALL {resource.type = 'workload', resource.compartment.id = '${var.compartment_ocid}', resource.k8s.cluster.id = '${var.oke_cluster_ocid}', resource.k8s.namespace = '${var.workload_namespace}', resource.k8s.serviceaccount.name = '${var.workload_service_account}'}"
  )
}

resource "oci_identity_dynamic_group" "provider" {
  compartment_id = var.tenancy_ocid
  name           = "${var.name}-dg"
  description    = "BigFleet OCI capacity provider principal (${var.trust_mode})."
  matching_rule  = local.matching_rule
}

# ---------------------------------------------------------------------------
# Policy — least-privilege Compute permissions scoped to the compartment.
# ---------------------------------------------------------------------------
data "oci_identity_compartment" "target" {
  id = var.compartment_ocid
}

resource "oci_identity_policy" "provider" {
  compartment_id = var.tenancy_ocid
  name           = "${var.name}-policy"
  description    = "Least-privilege policy for the BigFleet OCI capacity provider."
  statements = [
    "Allow dynamic-group ${oci_identity_dynamic_group.provider.name} to manage instance-family in compartment ${data.oci_identity_compartment.target.name}",
    "Allow dynamic-group ${oci_identity_dynamic_group.provider.name} to use volume-family in compartment ${data.oci_identity_compartment.target.name}",
    "Allow dynamic-group ${oci_identity_dynamic_group.provider.name} to use virtual-network-family in compartment ${data.oci_identity_compartment.target.name}",
    "Allow dynamic-group ${oci_identity_dynamic_group.provider.name} to read instance-images in compartment ${data.oci_identity_compartment.target.name}",
    "Allow dynamic-group ${oci_identity_dynamic_group.provider.name} to use instance-agent-command-family in compartment ${data.oci_identity_compartment.target.name}",
  ]
}

# ---------------------------------------------------------------------------
# Outputs
# ---------------------------------------------------------------------------
output "dynamic_group_name" {
  description = "Name of the dynamic group matching the provider principal."
  value       = oci_identity_dynamic_group.provider.name
}

output "policy_name" {
  description = "Name of the provider's least-privilege policy."
  value       = oci_identity_policy.provider.name
}
