#!/usr/bin/env bash
#
# Create a LEAST-PRIVILEGE Proxmox API token for the BigFleet Proxmox provider.
#
# Run this ON A PROXMOX CLUSTER NODE as root (pveum talks to the local cluster).
# It creates:
#   * a dedicated user                (bigfleet@pve)
#   * a dedicated resource pool       (bigfleet)
#   * a custom role with only the privileges the provider needs
#   * an API token for the user
#   * an ACL binding the role to the pool (and to /nodes for capacity reads)
#
# The provider then connects with:
#   --proxmox-api-url https://<node>:8006/api2/json
#   --proxmox-token-id 'bigfleet@pve!autoscaler'
#   --proxmox-token-file <file with the printed secret>
#   --proxmox-ca-file /etc/pve/pve-root-ca.pem   (or --proxmox-tls-fingerprint)
#   --proxmox-pool bigfleet
#
# Privilege names are valid for Proxmox VE 8.x. Confirm them against your PVE
# version (`pveum role list`, `pvesh get /access/roles`) before relying on this.
set -euo pipefail

USER="${BIGFLEET_USER:-bigfleet@pve}"
ROLE="${BIGFLEET_ROLE:-BigFleetProvider}"
POOL="${BIGFLEET_POOL:-bigfleet}"
TOKEN="${BIGFLEET_TOKEN:-autoscaler}"

# The least-privilege role. The provider clones a template, sizes + tags the
# clone, powers it on/off, reads run state, talks to the guest agent, allocates
# disk space, and reads node/VM capacity. It needs nothing else.
PRIVS="VM.Allocate,VM.Clone,VM.Config.Disk,VM.Config.CPU,VM.Config.Memory,VM.Config.Network,VM.Config.Options,VM.PowerMgmt,VM.Monitor,VM.GuestAgent.Audit,VM.GuestAgent.FileSystemWrite,VM.GuestAgent.Unrestricted,VM.Audit,Datastore.AllocateSpace,Datastore.Audit,Pool.Audit,Sys.Audit"

echo ">> creating role $ROLE"
if pveum role list | awk '{print $2}' | grep -qx "$ROLE"; then
  pveum role modify "$ROLE" --privs "$PRIVS"
else
  pveum role add "$ROLE" --privs "$PRIVS"
fi

echo ">> creating user $USER"
pveum user add "$USER" --comment "BigFleet capacity provider" 2>/dev/null || true

echo ">> creating resource pool $POOL"
pveum pool add "$POOL" --comment "BigFleet-managed VMs" 2>/dev/null || true

echo ">> binding $ROLE to pool $POOL and to /nodes (capacity reads)"
pveum acl modify "/pool/$POOL" --users "$USER" --roles "$ROLE"
# Node-capacity reads (Cluster.Resources / node status) are scoped at /nodes;
# a least-privilege audit-only binding is enough.
pveum acl modify "/nodes" --users "$USER" --roles PVEAuditor

echo ">> creating API token $USER!$TOKEN (privilege separation OFF so it inherits the user's ACL)"
pveum user token add "$USER" "$TOKEN" --privsep 0

cat <<EOF

Done. Note the token 'value' printed above — that is the SECRET half. Store it in
the Kubernetes Secret (deploy/secret/proxmox-credentials.example.yaml) and set:

  --proxmox-token-id '$USER!$TOKEN'

Also copy the cluster CA for TLS verification:

  cp /etc/pve/pve-root-ca.pem  (mount it and pass --proxmox-ca-file)

Place your prepared VM template (qemu-guest-agent + kubelet installed, converted
to a template) in the '$POOL' pool, and point --template-vmid at it.
EOF
