# libvirt host-side least-privilege setup

This is the libvirt analogue of a cloud provider's IAM module. There is no role
service to provision; instead, on **each libvirt host** the provider manages, you
create a least-privilege identity it connects as and scope that identity to the
managed pool. Apply the model that matches your `--connect` URIs.

The goal is the same as least-privilege IAM: the provider can define / start /
configure / drain / destroy domains **in its own storage pool and network**, and
nothing more — no access to other tenants' domains, no host root.

## Model A — `qemu+ssh://` (SSH transport)

On each host:

1. **Create a dedicated, unprivileged libvirt user** (not root) and put it in the
   `libvirt` group so it can reach the system libvirt socket:

   ```sh
   sudo useradd --create-home --shell /bin/bash bigfleet
   sudo usermod -aG libvirt bigfleet
   ```

2. **Authorise the provider's SSH public key**, restricted to what it needs. Add
   the provider's public key (the one whose private half is in the
   `bigfleet-libvirt-ssh` Secret) to `~bigfleet/.ssh/authorized_keys`. Optionally
   pin it to the host's management address with a `from=` restriction:

   ```
   from="10.0.0.0/8",no-agent-forwarding,no-X11-forwarding ssh-ed25519 AAAA... bigfleet-libvirt
   ```

3. **Scope libvirt access with a polkit rule** (`polkit.rules` in this directory):
   copy it to `/etc/polkit-1/rules.d/` so the `bigfleet` user may manage domains
   via the system libvirtd without a password, but only through libvirt's API (no
   shell-out, no other users' sessions).

   ```sh
   sudo cp polkit.rules /etc/polkit-1/rules.d/49-bigfleet-libvirt.rules
   sudo systemctl restart polkit
   ```

4. **Pre-create the storage pool and network** the provider uses
   (`--storage-pool`, `--network`) and put the golden base image (`--image`) in
   that pool. The provider only ever touches its configured pool/network.

`ssh-keyscan host-a host-b > known_hosts` and ship the result in the SSH Secret so
the SSH transport verifies host identity (no trust-on-first-use).

## Model B — `qemu+tls://` (libvirt native TLS)

On each host, configure libvirtd's TLS listener and allow only the provider's
client certificate DN:

1. Issue a libvirt **CA**, a **server** cert/key for each host, and one **client**
   cert/key for the provider (the `bigfleet-libvirt-tls` Secret). The libvirt
   manual's "Generating TLS certificates" section covers the `certtool` steps.

2. In `/etc/libvirt/libvirtd.conf` on each host, enable the TLS socket and pin the
   allow-list to the provider's client DN:

   ```
   listen_tls = 1
   tls_allowed_dn_list = ["C=,O=BigFleet,CN=bigfleet-libvirt-client"]
   ```

   Then open the firewall to port `16514` only from the provider's network and
   `systemctl restart libvirtd` (or `virtproxyd` on split-daemon hosts).

3. As in Model A, pre-create the `--storage-pool` and `--network` and stage the
   golden base image.

The client DN allow-list is the least-privilege boundary: only a client
presenting that exact certificate may connect, and polkit (Model A's rule applies
to the resulting session too) scopes what it can do.

## Model C — `qemu:///system` (local socket)

Single-host, in-cluster deployments can bind-mount the host's libvirt socket into
the pod (`credentials.hostSocket.enabled=true`). The pod's uid must map to a user
in the host `libvirt` group, and the polkit rule above still scopes API access.
This is the simplest model but couples the provider pod to one host; prefer SSH or
TLS for multi-host.
