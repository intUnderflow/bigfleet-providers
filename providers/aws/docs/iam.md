---
title: IAM
description: The exact least-privilege IAM policy the AWS provider needs, the node instance profile, and the IRSA setup on EKS.
sidebar:
  order: 3
  label: IAM
---

The AWS provider talks to EC2, SSM, and (optionally) SQS. There is no hardcoded
credential anywhere — it uses the standard AWS credential chain, so on EKS you
give it a role via **IRSA**, on EC2 an **instance profile**, and locally the
`AWS_*` env vars. Two distinct identities are involved, and conflating them is
the most common setup mistake:

- The **provider role** — the identity the provider process runs as. It calls
  `RunInstances`, `TerminateInstances`, the SSM commands, and so on.
- The **node instance profile** — the identity the *instances it launches* run
  as (set with [`--iam-instance-profile`](/providers/aws/configuration/)). It
  needs **SSM** so `Configure`/`Drain` can reach the node.

The provider role passes the node profile's role to EC2 at launch, which is the
*only* reason it needs `iam:PassRole`.

## The provider role policy

This is the complete least-privilege policy. Every action below is one the code
actually calls; nothing is padding. Drop the `sqs:*` statement if you are not
using [`--spot-interruption-queue`](/providers/aws/pricing-and-interruption/),
and the `iam:PassRole` statement if you are not setting `--iam-instance-profile`.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "EC2Lifecycle",
      "Effect": "Allow",
      "Action": [
        "ec2:RunInstances",
        "ec2:TerminateInstances",
        "ec2:DescribeInstances",
        "ec2:DescribeSpotPriceHistory",
        "ec2:CreateTags",
        "ec2:DeleteTags"
      ],
      "Resource": "*"
    },
    {
      "Sid": "SSMBootstrapAndDrain",
      "Effect": "Allow",
      "Action": [
        "ssm:SendCommand",
        "ssm:GetCommandInvocation"
      ],
      "Resource": "*"
    },
    {
      "Sid": "PassNodeRoleOnlyWithInstanceProfile",
      "Effect": "Allow",
      "Action": "iam:PassRole",
      "Resource": "arn:aws:iam::<account-id>:role/<node-role>",
      "Condition": {
        "StringEquals": { "iam:PassedToService": "ec2.amazonaws.com" }
      }
    },
    {
      "Sid": "ObservedSpotInterruptions",
      "Effect": "Allow",
      "Action": [
        "sqs:ReceiveMessage",
        "sqs:DeleteMessage"
      ],
      "Resource": "arn:aws:sqs:<region>:<account-id>:<queue-name>"
    }
  ]
}
```

### What each action is for

| Action | Lifecycle call | Why |
|---|---|---|
| `ec2:RunInstances` | `Create` | Launches the instance (base AMI + `--base-user-data`, with an idempotency `ClientToken`). |
| `ec2:TerminateInstances` | `Delete` | Tears the instance down. |
| `ec2:DescribeInstances` | List / reconcile | Recovers inventory and bindings from the `bigfleet:managed` tag; also the "running" waiter `Create` blocks on. |
| `ec2:DescribeSpotPriceHistory` | spot refresh | The live spot price, fetched on the `--spot-refresh` loop — never on the List hot path. |
| `ec2:CreateTags` | `Create` / `Configure` | Stamps `bigfleet:managed`, `machine-id`, `capacity` at launch and `bigfleet:cluster` on bind. |
| `ec2:DeleteTags` | `Drain` | Removes the `bigfleet:cluster` binding tag. |
| `ssm:SendCommand` | `Configure` / `Drain` | Delivers the opaque `bootstrap_blob` to the node's `--bootstrap-hook`, and runs cordon/drain. |
| `ssm:GetCommandInvocation` | `Configure` / `Drain` | Polls the command to `Success` — a failed bootstrap or drain becomes `FAILED`, never a false Configured/Idle. |
| `iam:PassRole` | `Create` | EC2 rejects `RunInstances` with an instance profile unless the caller may pass that role. **Conditional** — see below. |
| `sqs:ReceiveMessage` / `sqs:DeleteMessage` | interruption poller | Long-polls the EventBridge-fed queue to raise observed spot interruption probability. **Optional** — see below. |

`Resource: "*"` on the EC2 and SSM statements is the simplest correct starting
point. In production, scope them down with tag conditions (e.g. require
`aws:ResourceTag/bigfleet:managed` on `TerminateInstances`/`CreateTags`/
`DeleteTags`/SSM) so the role can only touch instances this provider owns. See
[Security](/providers/aws/security/) for the tightened variant.

### `iam:PassRole` — only with `--iam-instance-profile`

When you set `--iam-instance-profile`, `RunInstances` attaches that profile to
each instance, which means the provider role is *passing* the node role to EC2.
AWS rejects the launch without `iam:PassRole` on it. Keep this statement
narrow:

- Scope `Resource` to the **node role ARN** (the role behind the instance
  profile), not `*`.
- Keep the `iam:PassedToService` = `ec2.amazonaws.com` condition so the role
  can only ever be handed to EC2.

If you do **not** set `--iam-instance-profile`, omit this statement entirely —
the provider never calls `RunInstances` with a profile, so it never needs
`PassRole`.

### `sqs:*` — only with `--spot-interruption-queue`

The observed-interruption feed is optional. When you pass
`--spot-interruption-queue <url>`, the provider long-polls that queue for the
EventBridge "EC2 Spot Instance Interruption Warning" / "EC2 Instance Rebalance
Recommendation" events and deletes each message after handling it — exactly
`sqs:ReceiveMessage` + `sqs:DeleteMessage`, nothing more. Scope `Resource` to
the one queue's ARN. Without this flag the poller never starts and the
statement is dead weight; drop it. (Wiring the EventBridge rule → SQS itself is
covered in [Observability](/providers/aws/observability/).)

## The node instance profile (SSM)

The instances the provider launches need to be reachable by SSM, because
`Configure` and `Drain` are delivered as `AWS-RunShellScript` commands. The
role behind `--iam-instance-profile` must therefore carry the managed-instance
SSM permissions — the AWS managed policy `AmazonSSMManagedInstanceCore` is
exactly this:

```sh
aws iam create-role --role-name bigfleet-node \
  --assume-role-policy-document '{
    "Version": "2012-10-17",
    "Statement": [{
      "Effect": "Allow",
      "Principal": { "Service": "ec2.amazonaws.com" },
      "Action": "sts:AssumeRole"
    }]
  }'

aws iam attach-role-policy --role-name bigfleet-node \
  --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore

aws iam create-instance-profile --instance-profile-name bigfleet-node
aws iam add-role-to-instance-profile \
  --instance-profile-name bigfleet-node --role-name bigfleet-node
```

Then launch the provider with `--iam-instance-profile bigfleet-node`. The
`arn:aws:iam::<account-id>:role/bigfleet-node` ARN is what goes in the provider
role's `iam:PassRole` `Resource` above.

The AMI must also run the SSM Agent (the EKS-optimized and Amazon Linux AMIs
ship it pre-installed and enabled). If the agent is not running, the node never
registers as a managed instance and every `Configure`/`Drain` times out to
`FAILED`.

## IRSA on EKS

On EKS, give the **provider** its role through IAM Roles for Service Accounts
(IRSA) rather than node credentials — the pod assumes the role directly via its
ServiceAccount, with no static keys.

Create the role, attach the provider policy from above (saved as
`provider-policy.json`), and bind it to the ServiceAccount the provider Deployment
uses:

```sh
eksctl create iamserviceaccount \
  --cluster <cluster> \
  --namespace bigfleet \
  --name bigfleet-aws \
  --attach-policy-arn arn:aws:iam::<account-id>:policy/bigfleet-aws-provider \
  --approve
```

That stamps the `eks.amazonaws.com/role-arn` annotation onto the ServiceAccount:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: bigfleet-aws
  namespace: bigfleet
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::<account-id>:role/bigfleet-aws-provider
```

Reference that ServiceAccount from the provider Deployment
(`spec.template.spec.serviceAccountName: bigfleet-aws`) and the SDK's default
credential chain picks up the web-identity token automatically — no `AWS_*`
env vars, no mounted keys. See [Install & deploy](/providers/aws/install/) for
the full Deployment.

Note this is the **provider** role. The **node** role from the section above is
a normal EC2 instance role, not an IRSA role — those instances are not pods, so
they take their identity from the instance profile, not a ServiceAccount.

## Quick verification

Once deployed, a `Create → Configure → Drain → Delete` cycle exercises every
permission. If the role is short a permission you will see it surface as a
failed transition rather than a silent skip — watch
`bigfleet_aws_ec2_api_calls_total{outcome="error"}` and the provider logs (see
[Troubleshooting](/providers/aws/troubleshooting/)). A `PassRole`/`AccessDenied`
on the very first `RunInstances` almost always means the conditional statement
is missing or its `Resource` does not match your node role ARN.
