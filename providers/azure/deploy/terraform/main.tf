# Terraform for the BigFleet Azure provider identity. Creates:
#
#   * a user-assigned MANAGED IDENTITY for the provider,
#   * a ROLE ASSIGNMENT scoped to the target resource group — either the
#     built-in "Contributor" role or a tighter CUSTOM role granting only the
#     compute/network actions the code calls (var.use_custom_role), and
#   * a FEDERATED IDENTITY CREDENTIAL binding the identity to the provider's
#     Kubernetes ServiceAccount on AKS (Workload Identity), the production path.
#
# The custom role mirrors the actions the provider actually calls:
#   Microsoft.Compute/virtualMachines: read/write/delete + the
#   virtualMachines/extensions it uses for Configure/Drain,
#   Microsoft.Network/networkInterfaces: read/write/delete,
#   Microsoft.Compute/locations/* and Microsoft.Compute/skus/read for the
#   Resource SKUs lookup, and the disks the VM creates.
#
# One identity per region is recommended (one provider process per region).

terraform {
  required_version = ">= 1.5"
  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = ">= 3.80"
    }
  }
}

provider "azurerm" {
  features {}
}

variable "name" {
  description = "Name prefix for the identity and role (e.g. bigfleet-azure-eastus)."
  type        = string
  default     = "bigfleet-azure"
}

variable "location" {
  description = "Azure region for the managed identity (e.g. eastus)."
  type        = string
}

variable "resource_group_name" {
  description = "Resource group the provider creates VMs in; the role is scoped here."
  type        = string
}

variable "use_custom_role" {
  description = "Assign a least-privilege custom role instead of the built-in Contributor."
  type        = bool
  default     = true
}

# --- Workload Identity federation inputs (AKS) ------------------------------
variable "oidc_issuer_url" {
  description = "AKS cluster OIDC issuer URL (az aks show --query oidcIssuerProfile.issuerUrl)."
  type        = string
}

variable "service_account_namespace" {
  description = "Namespace of the provider ServiceAccount."
  type        = string
  default     = "bigfleet"
}

variable "service_account_name" {
  description = "Name of the provider ServiceAccount (matches the Helm release's SA)."
  type        = string
  default     = "bigfleet-azure"
}

# ---------------------------------------------------------------------------
data "azurerm_resource_group" "target" {
  name = var.resource_group_name
}

# The provider's user-assigned managed identity.
resource "azurerm_user_assigned_identity" "provider" {
  name                = "${var.name}-identity"
  location            = var.location
  resource_group_name = var.resource_group_name
}

# ---------------------------------------------------------------------------
# Role: built-in Contributor (default) OR a least-privilege custom role.
# ---------------------------------------------------------------------------
resource "azurerm_role_definition" "provider" {
  count       = var.use_custom_role ? 1 : 0
  name        = "${var.name}-role"
  scope       = data.azurerm_resource_group.target.id
  description = "Least-privilege role for the BigFleet Azure capacity provider."

  permissions {
    actions = [
      "Microsoft.Compute/virtualMachines/read",
      "Microsoft.Compute/virtualMachines/write",
      "Microsoft.Compute/virtualMachines/delete",
      "Microsoft.Compute/virtualMachines/extensions/read",
      "Microsoft.Compute/virtualMachines/extensions/write",
      "Microsoft.Compute/virtualMachines/extensions/delete",
      "Microsoft.Compute/disks/read",
      "Microsoft.Compute/disks/write",
      "Microsoft.Compute/disks/delete",
      "Microsoft.Compute/locations/vmSizes/read",
      "Microsoft.Compute/skus/read",
      "Microsoft.Network/networkInterfaces/read",
      "Microsoft.Network/networkInterfaces/write",
      "Microsoft.Network/networkInterfaces/delete",
      # Join the configured subnet when attaching a NIC.
      "Microsoft.Network/virtualNetworks/subnets/join/action",
      "Microsoft.Network/virtualNetworks/subnets/read",
    ]
    not_actions = []
  }

  assignable_scopes = [data.azurerm_resource_group.target.id]
}

resource "azurerm_role_assignment" "custom" {
  count              = var.use_custom_role ? 1 : 0
  scope              = data.azurerm_resource_group.target.id
  role_definition_id = azurerm_role_definition.provider[0].role_definition_resource_id
  principal_id       = azurerm_user_assigned_identity.provider.principal_id
}

resource "azurerm_role_assignment" "contributor" {
  count                = var.use_custom_role ? 0 : 1
  scope                = data.azurerm_resource_group.target.id
  role_definition_name = "Contributor"
  principal_id         = azurerm_user_assigned_identity.provider.principal_id
}

# ---------------------------------------------------------------------------
# Workload Identity: federate the managed identity to the Kubernetes SA.
# ---------------------------------------------------------------------------
resource "azurerm_federated_identity_credential" "provider" {
  name                = "${var.name}-federated"
  resource_group_name = var.resource_group_name
  parent_id           = azurerm_user_assigned_identity.provider.id
  audience            = ["api://AzureADTokenExchange"]
  issuer              = var.oidc_issuer_url
  subject             = "system:serviceaccount:${var.service_account_namespace}:${var.service_account_name}"
}

# ---------------------------------------------------------------------------
# Outputs — wire these into the Helm values.
# ---------------------------------------------------------------------------
output "client_id" {
  description = "Managed identity client id. Set serviceAccount.clientId in the Helm values."
  value       = azurerm_user_assigned_identity.provider.client_id
}

output "tenant_id" {
  description = "Tenant id. Set serviceAccount.tenantId in the Helm values."
  value       = azurerm_user_assigned_identity.provider.tenant_id
}

output "principal_id" {
  description = "Managed identity principal (object) id."
  value       = azurerm_user_assigned_identity.provider.principal_id
}
