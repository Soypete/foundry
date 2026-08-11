# K3s network planes

Foundry treats the following networks as separate planes. An address may be
routable in more than one plane, but it must not silently change roles.

| Plane | Purpose | K3s setting |
|---|---|---|
| Node underlay | Node-to-node addresses carrying Flannel VXLAN endpoints | `node-ip`, `flannel-iface` |
| Pod network | CNI-assigned pod addresses carried over Flannel | K3s cluster CIDR |
| Service network | Virtual ClusterIP addresses, including DNS | K3s service CIDR |
| API VIP | Stable Kubernetes API endpoint managed by kube-vip | TLS SAN and client/server URL only |
| Tailscale | Remote administration and API access; the node underlay when `network_substrate: tailscale` | TLS SAN/client endpoint, or `node-ip`/`flannel-iface` |

## What Flannel's endpoint must be

Flannel encapsulates pod traffic to a *node* address, so that address must be:

- **exclusively the node's own**, so peers reach that node and no other;
- **not floating**, so it cannot move to a different machine; and
- **not an overlay address**, unless the overlay is the chosen substrate.

Subnet membership is deliberately *not* the test. It was only ever a proxy for
exclusive ownership, and it rejects legitimate topologies such as a routed `/32`
or a second subnet.

The API VIP fails the second requirement: kube-vip moves it between control
plane nodes, so a peer sending pod traffic there follows the API server role
rather than the node. Foundry refuses to converge when the interface Flannel is
pinned to also carries the VIP, because pinning by interface *name* is not
enough — K3s resolves that name to whichever address it picks.

## Substrate selection

`cluster.network_substrate` chooses which network carries pod traffic. It
defaults to `lan`, and an absent value behaves identically.

| | `lan` (default) | `tailscale` |
|---|---|---|
| `node_ip` source | `node_ip`, else `address` | `tailscale_address` |
| CGNAT addresses | refused | required |
| `flannel_interface` | interface owning `node_ip` | `tailscale0` |
| Pod MTU | Flannel derives it (1450) | 1280 − 50 = 1230 |
| Failure domain | the switch | the tailnet |

### LAN (default)

Each host's `node_ip` is its LAN address and `flannel_interface` is the
interface that owns it. If omitted, `node_ip` defaults to the host `address`,
and Foundry discovers the interface by looking up that exact address. A CGNAT
`address` is refused rather than adopted implicitly, because that would put the
data plane on an overlay by accident.

```yaml
hosts:
  - hostname: blue1
    address: 192.168.1.185
    node_ip: 192.168.1.185
    flannel_interface: enp1s0
    tailscale_address: 100.80.1.10   # remote access only
    roles: [cluster-control-plane]
cluster:
  vip: 10.0.0.11
```

### Tailscale

For clusters whose nodes do not share a layer 2 segment. Flannel still runs;
only its endpoints move to the tailnet. Every cluster host needs a
`tailscale_address`, and `node_ip` must either match it or be omitted.

```yaml
hosts:
  - hostname: blue1
    address: 192.168.1.185           # SSH/management
    tailscale_address: 100.81.89.62  # Flannel endpoint
    roles: [cluster-control-plane]
cluster:
  network_substrate: tailscale
```

This costs real performance, so prefer `lan` when the nodes share a switch:

- **Double encapsulation.** Pod packets are VXLAN-wrapped inside WireGuard.
- **Lower MTU.** `tailscale0` is 1280 and VXLAN takes 50, leaving 1230 for pods
  rather than 1450. Foundry sets `flannel-mtu` explicitly here, because letting
  Flannel assume a 1500-byte underlay produces the classic silent failure where
  small packets pass and large transfers stall.
- **A new dependency.** Pod networking stops working if the tailnet does. K3s is
  ordered after `tailscaled` via a systemd drop-in, and installation fails early
  if `tailscale0` is not carrying the node's address.

The API VIP is unrelated to this choice: it is never advertised on the tailnet
in either mode.

## When the API VIP is deployed

A VIP is a floating address kube-vip moves between control plane nodes, so it
only means anything when there is more than one. With a single control plane
Foundry does not deploy kube-vip at all — the VIP would provide no failover
while remaining selectable as the Flannel endpoint. `cluster.vip` is therefore
required only for a multi-control-plane cluster.

A `cluster.vip` left in the config for a single-control-plane cluster is not an
error: Foundry warns, keeps it as a certificate SAN so adding a second control
plane later needs no new certificate, and does not deploy kube-vip for it.

Foundry writes `/etc/rancher/k3s/config.yaml.d/10-foundry-network.yaml`
before starting K3s. It pins the node address, Flannel interface, and API
advertise address without putting join credentials in that file. After
provisioning, Foundry verifies every
`flannel.alpha.coreos.com/public-ip` annotation and checks that another node
can reach it. Provisioning fails if an annotation differs from `node_ip`,
equals the API VIP, or fails the peer reachability check.

## Existing cluster remediation

1. Back up the K3s datastore and the current server token together.
2. Add `node_ip` and, preferably, `flannel_interface` to every cluster host.
3. Run `foundry component install k3s --all-nodes`. Foundry writes the network
   drop-in and restarts changed server or agent services.
4. Confirm each node's InternalIP and Flannel public-IP annotation equal its
   configured LAN address, then test cross-node pod traffic and DNS.
5. If credentials were exposed, rotate them as described below before taking
   a new datastore backup.

### Removing a kube-vip Foundry no longer manages

A cluster provisioned before single-control-plane VIPs were dropped still has
kube-vip running, and it keeps the VIP on the Flannel interface. Foundry reports
this during install but does not remove it, because deleting a DaemonSet as a
side effect of an install is not reviewable at the point of use.

Remove it on the control plane:

```
sudo k3s kubectl delete daemonset -n kube-system kube-vip
ip addr show enp1s0          # the VIP should be gone; kube-vip releases it
sudo systemctl restart k3s   # Flannel re-selects the node address
```

Then confirm every node advertises its own address:

```
sudo k3s kubectl get nodes -o custom-columns=\
NAME:.metadata.name,PUBIP:.metadata.annotations.flannel\.alpha\.coreos\.com/public-ip
```

Check for anything still pointing at the VIP before withdrawing it — the DNS
`k8s` record, and any Service carrying it in `externalIPs`.

The K3s server token protects node joins and also encrypts bootstrap data, so
rotation must be coordinated. Run `sudo k3s token rotate` on a server without
capturing its output in shared logs, update Foundry's OpenBAO K3s token secrets
through a secure input path, update the K3s service configuration on every
server and agent, and restart servers first and agents second. Preserve the old
token with any pre-rotation snapshot: those snapshots require it for restore.
Do not paste tokens, kubeconfigs, service environment, or full service command
lines into tickets or terminal transcripts.
