# Terraform for the BigFleet AWS provider IAM. Creates the provider's
# least-privilege role + policy and wires the trust policy as either:
#   * IRSA (EKS, default) — STS web-identity to a Kubernetes ServiceAccount, or
#   * an EC2 instance profile — when the provider runs on a plain EC2 host.
#
# The policy mirrors deploy/iam/policy.json and the actions the code calls:
#   ec2:RunInstances/TerminateInstances/DescribeInstances/DescribeInstanceTypes/
#   DescribeSpotPriceHistory, ec2:CreateTags/DeleteTags,
#   ssm:SendCommand/GetCommandInvocation,
#   iam:PassRole (only when --iam-instance-profile is set; scoped to the node
#   role + iam:PassedToService=ec2.amazonaws.com), and sqs:ReceiveMessage/
#   DeleteMessage (only with --spot-interruption-queue).
#
# One role per region is recommended (one provider process per region).

terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
  }
}

variable "name" {
  description = "Name prefix for the role and policy (e.g. bigfleet-aws-us-east-1)."
  type        = string
  default     = "bigfleet-aws"
}

variable "trust_mode" {
  description = "How the provider obtains credentials: 'irsa' (EKS) or 'instance_profile' (plain EC2)."
  type        = string
  default     = "irsa"
  validation {
    condition     = contains(["irsa", "instance_profile"], var.trust_mode)
    error_message = "trust_mode must be 'irsa' or 'instance_profile'."
  }
}

# --- IRSA inputs (trust_mode = irsa) ----------------------------------------
variable "oidc_provider_arn" {
  description = "EKS cluster OIDC provider ARN (IRSA). Required when trust_mode = irsa."
  type        = string
  default     = ""
}

variable "oidc_provider_url" {
  description = "EKS OIDC issuer URL without the https:// prefix (IRSA). Required when trust_mode = irsa."
  type        = string
  default     = ""
}

variable "service_account_namespace" {
  description = "Namespace of the provider ServiceAccount (IRSA)."
  type        = string
  default     = "bigfleet"
}

variable "service_account_name" {
  description = "Name of the provider ServiceAccount (IRSA)."
  type        = string
  default     = "bigfleet-aws"
}

# --- policy scoping inputs --------------------------------------------------
variable "node_role_arn" {
  description = "ARN of the node instance-profile role passed to EC2 (iam:PassRole). Empty disables the PassRole statement (only needed when --iam-instance-profile is set)."
  type        = string
  default     = ""
}

variable "spot_interruption_queue_arn" {
  description = "ARN of the SQS spot-interruption queue. Empty disables the SQS statement (only needed with --spot-interruption-queue)."
  type        = string
  default     = ""
}

# ---------------------------------------------------------------------------
# Provider policy
# ---------------------------------------------------------------------------
data "aws_iam_policy_document" "provider" {
  statement {
    sid    = "EC2LifecycleAndInventory"
    effect = "Allow"
    actions = [
      "ec2:RunInstances",
      "ec2:TerminateInstances",
      "ec2:DescribeInstances",
      "ec2:DescribeInstanceTypes",
      "ec2:DescribeSpotPriceHistory",
      "ec2:CreateTags",
      "ec2:DeleteTags",
    ]
    resources = ["*"]
  }

  statement {
    sid    = "SSMBootstrapAndDrain"
    effect = "Allow"
    actions = [
      "ssm:SendCommand",
      "ssm:GetCommandInvocation",
    ]
    resources = ["*"]
  }

  # iam:PassRole — only when a node instance profile is configured.
  dynamic "statement" {
    for_each = var.node_role_arn == "" ? [] : [var.node_role_arn]
    content {
      sid       = "PassNodeRoleToEC2"
      effect    = "Allow"
      actions   = ["iam:PassRole"]
      resources = [statement.value]
      condition {
        test     = "StringEquals"
        variable = "iam:PassedToService"
        values   = ["ec2.amazonaws.com"]
      }
    }
  }

  # sqs:ReceiveMessage / DeleteMessage — only when a queue is configured.
  dynamic "statement" {
    for_each = var.spot_interruption_queue_arn == "" ? [] : [var.spot_interruption_queue_arn]
    content {
      sid    = "SpotInterruptionQueue"
      effect = "Allow"
      actions = [
        "sqs:ReceiveMessage",
        "sqs:DeleteMessage",
      ]
      resources = [statement.value]
    }
  }
}

resource "aws_iam_policy" "provider" {
  name        = "${var.name}-policy"
  description = "Least-privilege policy for the BigFleet AWS EC2 capacity provider."
  policy      = data.aws_iam_policy_document.provider.json
}

# ---------------------------------------------------------------------------
# Trust policy
# ---------------------------------------------------------------------------
data "aws_iam_policy_document" "trust" {
  dynamic "statement" {
    for_each = var.trust_mode == "irsa" ? [1] : []
    content {
      effect  = "Allow"
      actions = ["sts:AssumeRoleWithWebIdentity"]
      principals {
        type        = "Federated"
        identifiers = [var.oidc_provider_arn]
      }
      condition {
        test     = "StringEquals"
        variable = "${var.oidc_provider_url}:sub"
        values   = ["system:serviceaccount:${var.service_account_namespace}:${var.service_account_name}"]
      }
      condition {
        test     = "StringEquals"
        variable = "${var.oidc_provider_url}:aud"
        values   = ["sts.amazonaws.com"]
      }
    }
  }

  dynamic "statement" {
    for_each = var.trust_mode == "instance_profile" ? [1] : []
    content {
      effect  = "Allow"
      actions = ["sts:AssumeRole"]
      principals {
        type        = "Service"
        identifiers = ["ec2.amazonaws.com"]
      }
    }
  }
}

resource "aws_iam_role" "provider" {
  name               = "${var.name}-role"
  assume_role_policy = data.aws_iam_policy_document.trust.json
}

resource "aws_iam_role_policy_attachment" "provider" {
  role       = aws_iam_role.provider.name
  policy_arn = aws_iam_policy.provider.arn
}

# Instance profile only in instance_profile mode.
resource "aws_iam_instance_profile" "provider" {
  count = var.trust_mode == "instance_profile" ? 1 : 0
  name  = "${var.name}-profile"
  role  = aws_iam_role.provider.name
}

# ---------------------------------------------------------------------------
# Outputs
# ---------------------------------------------------------------------------
output "role_arn" {
  description = "ARN of the provider role. For IRSA, set this on the ServiceAccount via serviceAccount.roleArn in the Helm values."
  value       = aws_iam_role.provider.arn
}

output "instance_profile_name" {
  description = "Instance profile name (instance_profile mode only)."
  value       = var.trust_mode == "instance_profile" ? aws_iam_instance_profile.provider[0].name : null
}
