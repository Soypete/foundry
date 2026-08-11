# Tailscale Integration

Foundry uses Tailscale for **external access to the cluster** — remote `kubectl`,
ingress, and administration — while all internal cluster traffic stays on the
physical LAN.

Treat the cluster as though there is no LAN to reach it from: everything coming
from outside arrives over the tailnet.

## The rule

**Cluster-internal networks never go on the tailnet.**

Pod addresses, Service ClusterIPs, and the API VIP are internal to the cluster
data plane. Advertising any of them as a Tailscale subnet route puts cluster
traffic on the overlay, which is the failure mode this integration exists to
avoid. Foundry will not do it, and you should not configure it by hand.

See [K3s network planes](./network-planes.md) for the full model. In short:

| Plane | Purpose | Carried by |
|---|---|---|
| Physical LAN | Node identity, Flannel VXLAN underlay | `node_ip`, `flannel_interface` |
| Pod network | Pod-to-pod traffic | Flannel over the LAN |
| Service network | ClusterIPs, including in-cluster DNS | kube-proxy |
| API VIP | Stable API endpoint for in-cluster clients, HA failover | kube-vip, **LAN only** |
| Tailscale | Remote kubectl, ingress, administration | Tailscale operator |

### Why the API VIP stays internal

kube-vip assigns the VIP as a secondary address on a node's LAN interface. If
that node's K3s configuration does not pin `node_ip` and `flannel_interface`,
Flannel can select the VIP as the node's VXLAN endpoint — an address the other
nodes cannot reach coherently. Cross-node pod traffic involving that node then
fails while the remaining nodes stay healthy.

Foundry guards against this at three layers, so a misconfiguration fails loudly
rather than silently breaking the overlay:

- `Host.K3sNodeIP()` refuses to infer a node IP from a CGNAT (`100.64.0.0/10`)
  address and requires an explicit `node_ip`.
- `ResolveNodeNetwork` refuses an empty `node_ip`, refuses `node_ip == VIP`, and
  derives the Flannel interface from the node IP rather than the interface's
  address list.
- `ValidateFlannelPublicIPs` reads each node's
  `flannel.alpha.coreos.com/public-ip` after installation, rejects it if it
  equals the VIP or differs from the configured node IP, and verifies another
  node can reach it.

## Host configuration

Each cluster host carries three distinct addresses. They are not
interchangeable.

```yaml
hosts:
  - hostname: blue1
    address: 192.168.1.185          # SSH/management address Foundry connects to
    node_ip: 192.168.1.185          # LAN address K3s and Flannel use
    flannel_interface: enp1s0       # interface that owns node_ip
    tailscale_address: 100.81.89.62 # tailnet address for remote access
    roles: [cluster-control-plane]

  - hostname: blue2
    address: 192.168.1.97
    node_ip: 192.168.1.97
    flannel_interface: enp1s0
    tailscale_address: 100.125.196.1
    roles: [cluster-worker]

cluster:
  vip: 10.0.0.11                    # internal only; never advertised on Tailscale
```

`tailscale_address` is used for two things and nothing else:

1. It is added to the API server's TLS SANs, so a remote client can present it.
2. It becomes the server URL in the generated kubeconfig.

It is never used as a node address, an advertise address, or a Flannel endpoint.

## Remote kubectl

With `tailscale_address` set on a control plane host, Foundry points the
kubeconfig at it:

```
server: https://100.81.89.62:6443
```

Without one, the kubeconfig falls back to the API VIP, which is only reachable
from the cluster LAN. `foundry stack validate` warns when a control plane host
has no `tailscale_address`.

### Repairing an existing cluster

Adding `tailscale_address` to `stack.yaml` does not by itself update a
kubeconfig that was generated earlier. Converge it:

```bash
foundry component install k3s --all-nodes
```

This re-points the stored kubeconfig and exports it to `~/.foundry/kubeconfig`.
It is idempotent — a kubeconfig already using the right endpoint is reported
unchanged. Verify:

```bash
grep server: ~/.foundry/kubeconfig
kubectl --kubeconfig ~/.foundry/kubeconfig get nodes
```

## The Tailscale operator

The operator exposes selected services on the tailnet. It runs as a pod; it does
**not** put the cluster's nodes or pods on Tailscale.

### Prerequisites

An OAuth client with the `devices:write` and `auth_keys` scopes. See the
[Tailscale Kubernetes operator documentation](https://tailscale.com/kb/1236/kubernetes-operator#prerequisites).

Foundry requires OpenBAO and K3s to be installed first.

### Configuration

```yaml
components:
  tailscale:
    # Literal values are written to OpenBAO on first install, and this file is
    # rewritten to reference them so plaintext does not persist here.
    oauth_client_id: ${secret:tailscale:client_id}
    oauth_client_secret: ${secret:tailscale:client_secret}

    # Optional: pin the operator image.
    # operator_image: tailscale/operator:v1.98.9

    # Optional: ACL tags for operator-managed devices.
    # Defaults to [tag:k8s-foundry].
    # tags:
    #   - tag:k8s-foundry

    # Optional: subnet routes to advertise on the tailnet.
    #
    # Only set this to reach a non-cluster network through the cluster. Never
    # list pod, Service, or VIP ranges here. With no routes configured, no
    # Connector is created at all -- which is the correct default.
    # advertise_routes:
    #   - 172.16.5.0/24
```

Credentials are stored in OpenBAO at `foundry-core/tailscale` with keys
`client_id` and `client_secret`. If they are absent from both the config and
OpenBAO, installation fails with a link to the documentation above.

### Installing

```bash
foundry component install tailscale
```

Idempotent, and it converges onto an operator that was installed outside
Foundry rather than failing on the existing Helm release.

### Checking state

```bash
foundry component tailscale health
```

```
Tailscale integration

  Operator:  deployed
  Address:   operator.tail-scale.ts.net

  Ingress:
    ✓ kei/kei-web                              kei-web.tail-scale.ts.net
    ✗ kei/kei-oidc                             no address assigned
```

Exits non-zero when the operator is not deployed or an ingress is not serving,
so it can be used as a check. An ingress with no assigned address means the
operator has not finished provisioning its proxy, or the proxy is failing.

### Exposing a service

Set the ingress class to `tailscale`:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: my-app
spec:
  ingressClassName: tailscale
  defaultBackend:
    service:
      name: my-app
      port:
        number: 80
```

The operator provisions a proxy and publishes its tailnet hostname on the
ingress status, which is what `tailscale health` reports.

## Troubleshooting

### Remote kubectl times out

Check what the kubeconfig points at:

```bash
grep server: ~/.foundry/kubeconfig
```

A `192.168.x` or VIP address means the endpoint was never converged — set
`tailscale_address` and run `foundry component install k3s --all-nodes`.

### Cross-node pod traffic fails for one node

Compare each node's Flannel endpoint against its configured LAN address:

```bash
kubectl get nodes -o custom-columns=\
NAME:.metadata.name,\
FLANNEL:.metadata.annotations.flannel\\.alpha\\.coreos\\.com/public-ip
```

Every value must be that node's `node_ip`. A node advertising the VIP or a
`100.x` address has lost its network identity; add `node_ip` and
`flannel_interface` to `stack.yaml` and run
`foundry component install k3s --all-nodes`.

### An ingress shows no address

The operator has not provisioned its proxy. Check the operator's own logs and
the proxy pods in the `tailscale` namespace, and confirm the OAuth client still
has the required scopes and its ACL tags are authorized.

### Worker nodes cannot join

Node joins use the LAN, not Tailscale. Confirm `node_ip` is set for every host
and that the nodes can reach each other on the LAN. A join failure against a
`100.x` address means an address was configured in the wrong plane.

## Migrating from a VIP-on-Tailscale setup

Earlier guidance advertised the API VIP as a Tailscale subnet route and used
`100.x` node addresses with `allow_cgnat_vip: true`. That puts cluster traffic
on the overlay and can break Flannel. Foundry no longer generates that topology,
and the guards described above now reject the node-addressing part of it.

To migrate:

1. Give every cluster host an explicit `node_ip` (its LAN address) and
   `flannel_interface`.
2. Move each host's `100.x` address from `address`/`node_ip` to
   `tailscale_address`.
3. Withdraw any VIP subnet route advertisement
   (`tailscale up --advertise-routes=` without the VIP) and remove the
   corresponding route approval in the Tailscale admin console.
4. Run `foundry component install k3s --all-nodes`.
5. Confirm every node's Flannel public IP equals its `node_ip`, then test
   cross-node pod traffic and remote kubectl.

`allow_cgnat_vip` remains a supported flag: it relaxes VIP *format* validation
so a `100.64.0.0/10` address is accepted. It is only needed when the VIP itself
is a CGNAT address, which the topology above avoids. A VIP on the LAN does not
need it.

Back up the K3s datastore before step 4. If a node loses connectivity, restore
that node's previous configuration and revalidate before continuing to the next.

## References

- [K3s network planes](./network-planes.md)
- [Tailscale Kubernetes operator](https://tailscale.com/kb/1236/kubernetes-operator)
- [Tailscale subnet routers](https://tailscale.com/kb/1019/subnets)
- [RFC 6598 — Shared Address Space (CGNAT)](https://www.rfc-editor.org/rfc/rfc6598)
