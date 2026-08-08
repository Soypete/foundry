# Backups

Foundry uses Velero for cluster backup and restore operations, with SeaweedFS providing S3-compatible storage.

## Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│  Velero                                                              │
│  - Cluster state backups                                            │
│  - Scheduled backups                                                │
│  - Disaster recovery                                                │
└─────────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────────┐
│  SeaweedFS S3 API                                                    │
│  - Backup storage bucket                                            │
│  - Replicated across nodes via Longhorn                             │
└─────────────────────────────────────────────────────────────────────┘
```

## What Gets Backed Up

Velero backs up:
- Kubernetes resources (Deployments, Services, ConfigMaps, Secrets, etc.)
- Persistent Volume data (via Longhorn snapshots or restic)
- Custom Resource Definitions and instances

> **⚠️ Important**: Backups stored inside the cluster they protect are NOT true backups. If the cluster fails (etcd loss, total hardware failure), you lose access to the backup metadata. For production, store backups in external S3-compatible storage outside the cluster.

## Configuration

```yaml
velero:
  provider: s3
  s3_endpoint: http://seaweedfs-s3.seaweedfs.svc.cluster.local:8333
  s3_bucket: velero
  s3_region: us-east-1
  backup_retention_days: 30
  schedule_cron: "0 2 * * *"  # Daily at 2 AM
  schedule_excluded_namespaces:
    - kube-system
    - velero
```

## Commands

### Create Backup

```bash
# Create a manual backup
foundry backup create

# Create a named backup
foundry backup create --name pre-upgrade-backup

# Backup specific namespaces
foundry backup create --namespaces default,production
```

### List Backups

```bash
foundry backup list
```

Output:
```
NAME                          STATUS      CREATED                  EXPIRES
daily-backup-20241201020000   Completed   2024-12-01 02:00:00     2024-12-31
daily-backup-20241130020000   Completed   2024-11-30 02:00:00     2024-12-30
pre-upgrade-backup            Completed   2024-11-29 14:30:00     2024-12-29
```

### Restore from Backup

```bash
# Restore entire backup
foundry backup restore daily-backup-20241201020000

# Restore specific namespaces
foundry backup restore daily-backup-20241201020000 --namespaces default

# Restore to different namespace
foundry backup restore daily-backup-20241201020000 --namespace-mappings old-ns:new-ns
```

### Schedule Backups

```bash
# Create a daily backup schedule
foundry backup schedule --cron "0 2 * * *"

# Create weekly backup schedule
foundry backup schedule --cron "0 3 * * 0" --name weekly-backup

# List schedules
foundry backup schedule list
```

### Delete Backup

```bash
foundry backup delete pre-upgrade-backup
```

## Scheduled Backups

By default, Foundry creates a daily backup schedule that runs at 2 AM.

**Default Schedule:**
- Name: `daily-backup`
- Cron: `0 2 * * *` (daily at 2 AM)
- Retention: 30 days
- Excluded namespaces: `kube-system`, `velero`

### Customize Schedule

Edit the Velero configuration in your stack.yaml:

```yaml
velero:
  schedule_name: daily-backup
  schedule_cron: "0 2 * * *"
  backup_retention_days: 30
  schedule_excluded_namespaces:
    - kube-system
    - velero
    - monitoring  # Add more exclusions
```

## Disaster Recovery

### Complete Cluster Restore

If you need to restore to a new cluster:

1. Install Foundry on new cluster with same SeaweedFS storage configuration
2. Wait for Velero to connect to backup storage
3. List available backups:
   ```bash
   foundry backup list
   ```
4. Restore the most recent backup:
   ```bash
   foundry backup restore <backup-name>
   ```

### Partial Restore

Restore only specific resources:

```bash
# Restore only deployments
velero restore create --from-backup daily-backup --include-resources deployments

# Restore specific namespace
velero restore create --from-backup daily-backup --include-namespaces production

# Restore with label selector
velero restore create --from-backup daily-backup --selector app=critical
```

## Backup Storage

Backups are stored in SeaweedFS, which runs on Longhorn PVCs:

- **Bucket**: `velero` (created automatically)
- **Path style**: Enabled (required for S3-compatible storage)
- **Region**: `us-east-1` (default)

### Verify Storage

```bash
# Check SeaweedFS S3 connectivity
kubectl -n seaweedfs port-forward svc/seaweedfs-s3 8333:8333

# List buckets
aws --endpoint-url http://localhost:8333 s3 ls

# List backup contents
aws --endpoint-url http://localhost:8333 s3 ls s3://velero/
```

## Volume Snapshots

For persistent volume data, Velero can use:

1. **Longhorn snapshots** (recommended): Native volume snapshots via CSI
2. **Restic/Kopia**: File-level backups for any storage class

### Enable CSI Snapshots

Longhorn includes VolumeSnapshotClass support. Velero uses this automatically when available:

```yaml
velero:
  snapshots_enabled: true
  csi_snapshot_timeout: 10m
```

## Reinstalling the Cluster

### CRITICAL: Do NOT Reinstall VM-based Components

When recovering from a power outage or cluster failure, **do NOT reinstall** the following components that live on VMs:

| Component | Host | Why Not Reinstall |
|-----------|------|-------------------|
| openbao | refurb | Contains all secrets, tokens, kubeconfig |
| PowerDNS (dns) | blue1 | Contains zone data |
| zot | refurb | OCI registry with images |

Reinstalling these will **delete your data**:
- Reinstalling OpenBAO = loss of all secrets, tokens, credentials
- Reinstalling PowerDNS = loss of DNS zones
- Reinstalling zot = loss of container images

### CRITICAL: Longhorn Holds All k8s Data

Longhorn is the foundation for all Kubernetes storage. **Do NOT reinstall Longhorn** if you want to preserve:

- SeaweedFS data (including Velero backups in the `velero` bucket)
- Prometheus/Grafana/Loki metrics data
- Any PVC-based application data

**The dependency chain:**
```
Velero backups → SeaweedFS S3 → Longhorn PVCs → Longhorn
```

If Longhorn is reinstalled, all PVCs are wiped, and you lose:
- SeaweedFS data
- Velero backup files
- All persistent application data

### CRITICAL: etcd Loss Orphans All Data

**Reinstalling k3s wipes etcd**, which destroys all Kubernetes Custom Resource Definitions and instances, including:
- Longhorn volume CRs (even if Longhorn itself is not reinstalled)
- Velero backup CRs
- All Deployments, Services, ConfigMaps, etc.

This means **backups stored inside the cluster they protect aren't backups** — if you lose the cluster, you lose access to the backup metadata. Off-cluster backup storage (external S3, cold storage) is required for true disaster recovery.

### What Can Be Reinstalled (k8s-based, but be careful)

These components are **safe to reinstall** if Longhorn is preserved:
- k3s (cluster) - preserves Longhorn data
- contour (ingress)
- prometheus, grafana, loki (monitoring) - may lose historical data
- external-dns

**Use with caution**:
- seaweedfs - if Longhorn PVCs exist, it will reuse them
- longhorn - **never reinstall** if you want to keep data
- velero - safe to reinstall if SeaweedFS bucket exists

### Safe Recovery Procedure

1. **Check VM services first**:
   ```bash
   ssh refurb "sudo systemctl status openbao"
   ssh blue1 "sudo systemctl status powerdns"
   ```

2. **If VMs are OK, skip their installation** in stack.yaml:
   ```yaml
   setup_state:
     openbao_installed: true
     openbao_initialized: true
     dns_installed: true
     dns_zones_created: true
     zot_installed: true
     k8s_installed: false  # Can reinstall k3s
   ```

3. **Install only k8s components**:
   ```bash
   ./bin/foundry stack install --config ~/.foundry/stack.yaml
   ```

4. **Restore Velero backups** (after cluster is healthy):
   ```bash
   velero backup get
   velero restore create --from-backup <backup-name>
   ```

### If Backups Are Lost

If SeaweedFS was reinstalled and the `velero` bucket no longer exists:
- Check if volume data still exists on the underlying storage
- Longhorn snapshots may still exist if Longhorn itself wasn't fully wiped
- Contact support if physical storage is accessible

---

When reinstalling the cluster (e.g., after a failure or hardware reset), the backup data in SeaweedFS persists independently:

1. **Backup data is safe**: Backups are stored in the SeaweedFS S3 bucket (`velero`), which runs outside the k3s cluster on VMs or bare metal
2. **Reinstall Velero**: Once the new cluster is running, reinstall Velero with the same S3 configuration
3. **Remount the bucket**: Velero will automatically discover existing backups in the SeaweedFS bucket

### Restoring After Reinstall

```bash
# After Velero is reinstalled with same S3 config
# List existing backups (they're read from SeaweedFS directly)
velero backup get

# Restore from any existing backup
velero restore create --from-backup <backup-name>
```

The backup files remain in SeaweedFS at:
- `http://seaweedfs-s3.seaweedfs.svc.cluster.local:8333/velero/`

**Note**: If Longhorn was not installed on the original cluster, PVC-based data was not backed up. Only Kubernetes resources (Deployments, Services, ConfigMaps, etc.) stored in Velero can be restored.

## Troubleshooting

### Check Velero Status

```bash
kubectl -n velero get pods
kubectl -n velero logs deployment/velero
```

### Check Backup Status

```bash
# Get backup details
velero backup describe <backup-name>

# Get backup logs
velero backup logs <backup-name>
```

### Check Restore Status

```bash
# Get restore details
velero restore describe <restore-name>

# Get restore logs
velero restore logs <restore-name>
```

### Common Issues

**Backup stuck in "InProgress":**
1. Check Velero logs: `kubectl -n velero logs deployment/velero`
2. Verify S3 connectivity
3. Check for resource errors in backup describe output

**Restore fails:**
1. Check restore logs for specific errors
2. Verify target namespace doesn't have conflicting resources
3. Check RBAC permissions

**S3 connection errors:**
1. Verify SeaweedFS is running: `kubectl -n seaweedfs get pods`
2. Check credentials in velero secret: `kubectl -n velero get secret cloud-credentials -o yaml`
3. Test S3 connectivity from velero pod

### Verify Backup Integrity

```bash
# List backup contents
velero backup describe <backup-name> --details

# Check for warnings/errors
velero backup describe <backup-name> | grep -E "(Warning|Error)"
```

## Best Practices

1. **Test restores regularly**: Don't wait for a disaster to test your backups
2. **Monitor backup jobs**: Set up alerts for failed backups
3. **Exclude unnecessary namespaces**: Don't backup system namespaces that will be recreated
4. **Document restore procedures**: Keep runbooks updated
5. **Offsite backups**: Consider replicating SeaweedFS to external storage for true disaster recovery
