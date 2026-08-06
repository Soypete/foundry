# K3s network planes

Foundry treats the following networks as separate planes. An address may be
routable in more than one plane, but it must not silently change roles.

| Plane | Purpose | K3s setting |
|---|---|---|
| Physical LAN | Node-to-node underlay and Flannel VXLAN endpoints | `node-ip`, `flannel-iface` |
| Pod network | CNI-assigned pod addresses carried over Flannel | K3s cluster CIDR |
| Service network | Virtual ClusterIP addresses, including DNS | K3s service CIDR |
| API VIP | Stable Kubernetes API endpoint managed by kube-vip | TLS SAN and client/server URL only |
| Tailscale | Optional remote administration and API access | TLS SAN/client endpoint only |

When all nodes share a physical LAN, each host's `node_ip` must be its LAN
address and `flannel_interface` must be the interface that owns that address.
If omitted, `node_ip` defaults to the host `address`, while Foundry discovers
the interface by looking up that exact address. `tailscale_address` is optional
and is never used as a node or Flannel address.

```yaml
hosts:
  - hostname: blue1
    address: 192.168.1.185
    node_ip: 192.168.1.185
    flannel_interface: enp1s0
    tailscale_address: 100.80.1.10
    roles: [cluster-control-plane]
cluster:
  vip: 10.0.0.11
```

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

The K3s server token protects node joins and also encrypts bootstrap data, so
rotation must be coordinated. Run `sudo k3s token rotate` on a server without
capturing its output in shared logs, update Foundry's OpenBAO K3s token secrets
through a secure input path, update the K3s service configuration on every
server and agent, and restart servers first and agents second. Preserve the old
token with any pre-rotation snapshot: those snapshots require it for restore.
Do not paste tokens, kubeconfigs, service environment, or full service command
lines into tickets or terminal transcripts.
