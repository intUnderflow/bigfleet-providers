#!/usr/bin/env bash
#
# Create a least-privilege OpenStack user for the BigFleet OVHcloud provider, and
# the SSH keypair it injects at instance create. This is the OVH Public Cloud
# analogue of the AWS least-privilege IAM Terraform: it provisions the *minimum*
# authorisation surface the provider needs, scoped to ONE Public Cloud project.
#
# Prerequisites: the `openstack` CLI (python-openstackclient), authenticated as a
# project ADMIN with an openrc sourced for the target project/region. OVH's
# Public Cloud roles map to the OpenStack `member` role on the project; OVH does
# not expose fine-grained Keystone policy editing, so "least privilege" here means
# a DEDICATED user scoped to a SINGLE project (blast radius = that project), with
# nothing else attached. Treat its password as a secret (see ../secret/).
#
# Usage:
#   ./create-scoped-user.sh <username> <project-id> [region]
#
set -euo pipefail

USER_NAME="${1:?usage: create-scoped-user.sh <username> <project-id> [region]}"
PROJECT_ID="${2:?project id required}"
REGION="${3:-GRA}"
KEYPAIR_NAME="${KEYPAIR_NAME:-bigfleet-ovh}"
PASSWORD="${OS_NEW_USER_PASSWORD:-$(openssl rand -base64 24)}"

echo ">> creating user '$USER_NAME' scoped to project $PROJECT_ID"
openstack user create --project "$PROJECT_ID" --password "$PASSWORD" --enable "$USER_NAME"

# Grant ONLY the project member role (Compute create/delete + Network attach).
# Do not grant admin or any other project — the user must touch nothing else.
echo ">> granting the 'member' role on the project (Compute + Network)"
openstack role add --user "$USER_NAME" --project "$PROJECT_ID" member || \
  openstack role add --user "$USER_NAME" --project "$PROJECT_ID" _member_

# The SSH keypair injected into every instance at create (ovh.keyName). Generate a
# fresh ed25519 key; keep the PRIVATE half for the provider Secret, register the
# PUBLIC half here.
if [[ ! -f "./${KEYPAIR_NAME}" ]]; then
  echo ">> generating SSH keypair ./${KEYPAIR_NAME} (private) + .pub (public)"
  ssh-keygen -t ed25519 -N "" -C "$KEYPAIR_NAME" -f "./${KEYPAIR_NAME}"
fi
echo ">> registering keypair '$KEYPAIR_NAME' in OpenStack"
openstack keypair create --public-key "./${KEYPAIR_NAME}.pub" "$KEYPAIR_NAME" || \
  echo "   (keypair may already exist — continuing)"

cat <<SUMMARY

================================================================================
Done. Wire these into the provider:

  OS_AUTH_URL=https://auth.cloud.ovh.net/v3
  OS_IDENTITY_API_VERSION=3
  OS_USERNAME=$USER_NAME
  OS_PASSWORD=$PASSWORD          # store in the OpenStack-credentials Secret
  OS_PROJECT_ID=$PROJECT_ID
  OS_USER_DOMAIN_NAME=Default
  OS_PROJECT_DOMAIN_NAME=Default
  OS_REGION_NAME=$REGION

  --key-name=$KEYPAIR_NAME       # ovh.keyName in values.yaml
  SSH private key: ./${KEYPAIR_NAME}   # the bigfleet-ovh-ssh Secret (id_ed25519)

Create the Kubernetes Secrets (do NOT commit the password / private key):

  kubectl -n bigfleet create secret generic bigfleet-ovh-${REGION,,}-os \\
    --from-literal=OS_AUTH_URL=https://auth.cloud.ovh.net/v3 \\
    --from-literal=OS_IDENTITY_API_VERSION=3 \\
    --from-literal=OS_USERNAME=$USER_NAME \\
    --from-literal=OS_PASSWORD='$PASSWORD' \\
    --from-literal=OS_PROJECT_ID=$PROJECT_ID \\
    --from-literal=OS_USER_DOMAIN_NAME=Default \\
    --from-literal=OS_PROJECT_DOMAIN_NAME=Default \\
    --from-literal=OS_REGION_NAME=$REGION

  kubectl -n bigfleet create secret generic bigfleet-ovh-ssh \\
    --from-file=id_ed25519=./${KEYPAIR_NAME}
================================================================================
SUMMARY
