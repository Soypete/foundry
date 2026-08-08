# Foundry Stack Debugging Guide

This document covers common issues encountered during Foundry stack installation and their solutions.

## CRITICAL: Data Loss Prevention

When recovering from a cluster failure or power outage:

### DO NOT REINSTALL (VM-based)
- **openbao** (refurb) - Contains all secrets, tokens, credentials
- **PowerDNS/dns** (blue1) - Contains DNS zones
- **zot** (refurb) - Contains OCI container images

### DO NOT REINSTALL (k8s-based, destroys data)
- **longhorn** - Wipes all PVCs including SeaweedFS and Velero backups
- **seaweedfs** - May delete PVC data if not configured to reuse

### SAFE TO REINSTALL
- **k3s** - Can reinstall cluster, preserves Longhorn PVCs
- **contour** - Ingress controller
- **prometheus/grafana/loki** - Monitoring (may lose historical data)

## K3s Installation

### Worker Node Fails to Join Cluster

**Symptom**: Worker node (e.g., blue2) fails to join with error:
```
K3s agent did not become ready after 30 retries
```

**Root Cause**: The worker node cannot reach the cluster VIP (e.g., `10.0.0.11`) because it's on a different network (Tailscale) without access to the internal 10.x network.

**Solutions**:

1. **Use Tailscale Operator**: Install the Tailscale operator to enable cross-network communication. The operator creates routes between Tailscale network and cluster services.

2. **Use Single-Node Cluster**: For testing, run k3s as a single-node cluster without worker nodes.

3. **Network Topology Fix**: Ensure worker nodes have access to the cluster VIP through either:
   - Direct network access to 10.x subnet
   - Tailscale subnet router configuration
   - Tailscale Operator Connector CRD

### K3s Control Plane Fails to Become Ready

**Symptom**: Installation fails at "K3s did not become ready after 30 retries" but service starts manually.

**Root Cause**: The readiness check uses `sudo k3s kubectl get nodes` which may fail due to timing or permissions.

**Solutions**:

1. **Manual Service Start**: After failed install, manually start the service:
   ```bash
   sudo systemctl start k3s
   sudo systemctl status k3s
   ```

2. **Update State**: Mark k8s as installed in `~/.foundry/stack.yaml`:
   ```yaml
   setup_state:
     k8s_installed: true
   ```

### Kubeconfig Permissions

**Symptom**: Cannot retrieve kubeconfig from remote host - permission denied.

**Root Cause**: K3s creates kubeconfig with mode 600 (root-only) by default.

**Solution**: Install k3s with explicit permission flag:
```bash
curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="server --write-kubeconfig-mode 644" sh -
```

### TLS Certificate Invalid for New IP Address

**Symptom**: Error message like:
```
tls: failed to verify certificate: x509: certificate is valid for 10.0.0.11, not 100.81.89.62
```

**Root Cause**: K3s certificate was generated with specific IP addresses (VIP, localhost), but the Tailscale IP of the control plane node was not included in the TLS SANs.

**Solution**: Rotate the certificates to include the new IP:

```bash
# On control plane node
sudo k3s certificate rotate
sudo systemctl restart k3s
```

This is handled automatically by Foundry when reinstalling k3s with updated TLSSANs (control plane's Tailscale IP is now included in certificate SANs).

## PowerDNS

### API Key Mismatch After Config Update

**Symptom**: PowerDNS API returns 401 Unauthorized after updating config.

**Root Cause**: The running container uses the old API key from when it started.

**Solution**: Restart the PowerDNS container after config changes:
```bash
sudo systemctl restart foundry-powerdns
```

Or via container runtime:
```bash
sudo nerdctl rm -f powerdns
# Then let Foundry recreate it, or restart the service
```

## OpenBAO

### Credentials Not Available

**Symptom**: Installation fails with "OpenBAO credentials not found".

**Solution**: Ensure OpenBAO is installed and initialized before running stack install. The stack install requires OpenBAO to be running and unsealed.

## Tailscale Operator

### Network Isolation Between Nodes

When nodes are on different networks (e.g., some on Tailscale, some on internal LAN):

1. Install Tailscale operator component:
   ```yaml
   tailscale-operator:
     enabled: true
   ```

2. The operator creates `Connector` and `Service` resources that enable:
   - Route advertisement from k3s to Tailscale
   - Service exposure across Tailscale network

3. Verify operator is running:
   ```bash
   kubectl get pods -n tailscale
   ```

## General Debugging Commands

### Check Component Status
```bash
./bin/foundry stack status --config ~/.foundry/stack.yaml
```

### View Service Logs
```bash
# On the target host
sudo journalctl -u k3s -f
sudo journalctl -u openbao -f

# Or via container runtime
sudo nerdctl logs <container-name>
```

### Verify Network Connectivity
```bash
# From worker to control plane VIP
ssh <worker> "ping -c 3 10.0.0.11"

# Check Tailscale routes
ssh <host> "tailscale status"
```

### Check Cluster State
```bash
ssh <control-plane> "sudo k3s kubectl get nodes -o wide"
ssh <control-plane> "sudo k3s kubectl get pods -A"
```

## Longhorn Storage Configuration

### CRITICAL: Disk Space Requirements

Longhorn requires adequate disk space on nodes. Without proper allocation, pods will hang on initialization.

**Minimum disk requirements by component:**
- **SeaweedFS**: 50GB+ (filers, volumes)
- **Prometheus**: 200GB+ (metrics retention)
- **Loki**: 10GB+ (logs)
- **Grafana**: 5GB+ (dashboards)
- **Total for cluster**: Plan for 500GB+ minimum

### Configuring Storage on Specific Nodes

By default, Longhorn uses `/var/lib/longhorn` (root disk). For production, add a dedicated disk:

```bash
# Add a new disk to a Longhorn node (e.g., refurb with 2TB drive)
kubectl patch nodes.longhorn.io <node-name> -n longhorn-system \
  --type='merge' -p '{
    "spec":{
      "disks":{
        "data-disk-001":{
          "path":"/data/persistent-storage",
          "allowScheduling":true,
          "storageReserved":107374182400,
          "diskType":"filesystem"
        }
      }
    }
  }'
```

### Diagnosing Volume Attachment Issues

If pods hang on `Init:0/2` or `AttachVolume.Attach failed`:

```bash
# Check Longhorn volumes
kubectl get volumes.longhorn.io -n longhorn-system

# Look for faulted volumes
kubectl get volumes.longhorn.io -n longhorn-system | grep faulted

# Check volume details
kubectl get volumes.longhorn.io <volume-name> -n longhorn-system -o yaml

# Common issue: faulted volumes from previous cluster
# Solution: Delete the PVC to recreate fresh
kubectl delete pvc <pvc-name> -n <namespace>
```

### Verify Node Disk Configuration

```bash
# Check which disks Longhorn is using on each node
kubectl get nodes.longhorn.io -n longhorn-system -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{range .spec.disks:*}{$..path}{" "}{$..storageReserved}{"\n"}{end}'
```

### Storage Class for Large Volumes

Ensure PVCs use Longhorn storage class. Default is fine, but for large volumes (Prometheus 200GB), make sure the node has adequate space.

## Common State File Issues

The `~/.foundry/stack.yaml` contains a `setup_state` section that tracks installation progress. If installation fails partway:

1. Manually set `k8s_installed: true` to skip k3s reinstallation
2. Or reset the entire state:
   ```yaml
   setup_state:
     network_planned: false
     network_validated: false
     openbao_installed: false
     # ... etc
   ```