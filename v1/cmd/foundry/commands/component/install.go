package component

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/catalystcommunity/foundry/v1/internal/component"
	"github.com/catalystcommunity/foundry/v1/internal/component/certmanager"
	"github.com/catalystcommunity/foundry/v1/internal/component/contour"
	"github.com/catalystcommunity/foundry/v1/internal/component/dns"
	"github.com/catalystcommunity/foundry/v1/internal/component/externaldns"
	"github.com/catalystcommunity/foundry/v1/internal/component/gatewayapi"
	"github.com/catalystcommunity/foundry/v1/internal/component/gatewaycontroller"
	"github.com/catalystcommunity/foundry/v1/internal/component/grafana"
	"github.com/catalystcommunity/foundry/v1/internal/component/k3s"
	"github.com/catalystcommunity/foundry/v1/internal/component/loki"
	"github.com/catalystcommunity/foundry/v1/internal/component/openbao"
	"github.com/catalystcommunity/foundry/v1/internal/component/openbaoinjector"
	"github.com/catalystcommunity/foundry/v1/internal/component/prometheus"
	"github.com/catalystcommunity/foundry/v1/internal/component/seaweedfs"
	componentStorage "github.com/catalystcommunity/foundry/v1/internal/component/storage"
	"github.com/catalystcommunity/foundry/v1/internal/component/tailscale"
	"github.com/catalystcommunity/foundry/v1/internal/component/velero"
	"github.com/catalystcommunity/foundry/v1/internal/config"
	"github.com/catalystcommunity/foundry/v1/internal/dashboards"
	"github.com/catalystcommunity/foundry/v1/internal/helm"
	"github.com/catalystcommunity/foundry/v1/internal/host"
	"github.com/catalystcommunity/foundry/v1/internal/k8s"
	"github.com/catalystcommunity/foundry/v1/internal/secrets"
	"github.com/catalystcommunity/foundry/v1/internal/ssh"
	"github.com/urfave/cli/v3"
)

// sshExecutorAdapter adapts ssh.Connection to container.SSHExecutor interface
// by implementing the Execute(cmd string) (string, error) method
type sshExecutorAdapter struct {
	conn *ssh.Connection
}

func (a *sshExecutorAdapter) Execute(cmd string) (string, error) {
	result, err := a.conn.Exec(cmd)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return result.Stdout, fmt.Errorf("command failed with exit code %d: %s", result.ExitCode, result.Stderr)
	}
	return result.Stdout, nil
}

func (a *sshExecutorAdapter) Exec(cmd string) (*ssh.ExecResult, error) {
	return a.conn.Exec(cmd)
}

// InstallCommand installs a component
var InstallCommand = &cli.Command{
	Name:      "install",
	Usage:     "Install a component",
	ArgsUsage: "<name>",
	Description: `Installs a component with its dependencies.

The component will be installed according to the configuration in ~/.foundry/stack.yaml.

Examples:
  # Phase 2 (container-based) components:
  foundry component install openbao
  foundry component install dns
  foundry component install zot

  # Phase 3 (Kubernetes-based) components:
  foundry component install storage --backend local-path
  foundry component install seaweedfs
  foundry component install prometheus
  foundry component install loki
  foundry component install grafana
  foundry component install external-dns
  foundry component install velero`,
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "dry-run",
			Usage:   "Show what would be installed without actually installing",
			Aliases: []string{"n"},
		},
		&cli.StringFlag{
			Name:  "version",
			Usage: "Override the version to install",
		},
		// K3s-specific flags
		&cli.BoolFlag{
			Name:  "all-nodes",
			Usage: "Apply configuration to all cluster nodes (for k3s component)",
		},
		// Storage-specific flags
		&cli.StringFlag{
			Name:  "backend",
			Usage: "Storage backend: local-path, nfs, longhorn (for storage component)",
			Value: "local-path",
		},
		&cli.StringFlag{
			Name:  "nfs-server",
			Usage: "NFS server address (for nfs backend)",
		},
		&cli.StringFlag{
			Name:  "nfs-path",
			Usage: "NFS export path (for nfs backend)",
		},
	},
	Action: runInstall,
}

// k8sComponents lists all components that are installed via kubeconfig (Helm/K8s)
var k8sComponents = map[string]bool{
	"gateway-api":        true,
	"contour":            true,
	"gateway-controller": true,
	"cert-manager":       true,
	"storage":            true,
	"seaweedfs":          true,
	"prometheus":         true,
	"loki":               true,
	"grafana":            true,
	"external-dns":       true,
	"velero":             true,
	"openbao-injector":   true,
	"tailscale":          true,
}

func runInstall(ctx context.Context, cmd *cli.Command) error {
	// Get component name from arguments
	if cmd.Args().Len() == 0 {
		return fmt.Errorf("component name required\n\nUsage: foundry component install <name>")
	}

	name := cmd.Args().Get(0)
	dryRun := cmd.Bool("dry-run")
	version := cmd.String("version")

	// Get component from registry
	comp := component.Get(name)
	if comp == nil {
		return component.ErrComponentNotFound(name)
	}

	// Load stack configuration (needed for dependency checking)
	fmt.Println("Loading stack configuration...")
	configPath, err := config.FindConfig(cmd.String("config"))
	if err != nil {
		return fmt.Errorf("failed to find config: %w", err)
	}
	stackConfig, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load stack config: %w\n\nHint: Run 'foundry config init' to create a configuration", err)
	}

	// Check dependencies using setup state
	deps := comp.Dependencies()
	if len(deps) > 0 {
		fmt.Printf("Component %s depends on: %v\n", name, deps)
		fmt.Println("Checking dependencies...")

		for _, dep := range deps {
			installed := isDependencyInstalled(dep, stackConfig)
			if !installed {
				return fmt.Errorf("dependency %q is not installed\n\nPlease run: foundry component install %s", dep, dep)
			}

			fmt.Printf("  ✓ %s (installed)\n", dep)
		}
		fmt.Println()
	}

	// Check if this is a K8s-based component
	if k8sComponents[name] {
		return installK8sComponent(ctx, cmd, name, stackConfig, dryRun, version)
	}

	// SSH-based component installation (Phase 2 components)
	return installSSHComponent(ctx, cmd, name, stackConfig, dryRun, version)
}

// installK8sComponent installs a Kubernetes-based component using kubeconfig
func installK8sComponent(ctx context.Context, cmd *cli.Command, name string, stackConfig *config.Config, dryRun bool, version string) error {
	fmt.Printf("Installing Kubernetes component: %s\n", name)

	// Get kubeconfig path
	configDir, err := config.GetConfigDir()
	if err != nil {
		return fmt.Errorf("failed to get config directory: %w", err)
	}
	kubeconfigPath := filepath.Join(configDir, "kubeconfig")

	// Verify kubeconfig exists
	kubeconfigBytes, err := os.ReadFile(kubeconfigPath)
	if err != nil {
		return fmt.Errorf("kubeconfig not found at %s: %w\n\nHint: Install K3s first with 'foundry stack install' or 'foundry component install k3s'", kubeconfigPath, err)
	}

	if dryRun {
		fmt.Printf("\nWould install Kubernetes component: %s\n", name)
		if version != "" {
			fmt.Printf("Version: %s\n", version)
		}
		fmt.Println("\nNote: This is a dry-run. No changes will be made.")
		return nil
	}

	// Create Helm and K8s clients
	helmClient, err := helm.NewClient(kubeconfigBytes, "default")
	if err != nil {
		return fmt.Errorf("failed to create helm client: %w", err)
	}

	k8sClient, err := k8s.NewClientFromKubeconfig(kubeconfigBytes)
	if err != nil {
		return fmt.Errorf("failed to create k8s client: %w", err)
	}

	// Build component config
	cfg := component.ComponentConfig{}
	if version != "" {
		cfg["version"] = version
	}

	// Add cluster VIP for components that need it
	if stackConfig.Cluster.VIP != "" {
		cfg["cluster_vip"] = stackConfig.Cluster.VIP
	}

	// Tailscale is wired by hand rather than through the Component interface:
	// it needs OpenBAO for its OAuth credentials in addition to Helm and
	// Kubernetes, and it writes back to the stack config.
	if name == "tailscale" {
		return installTailscaleComponent(ctx, cmd, stackConfig, helmClient, k8sClient)
	}

	// Create component-specific instance with clients and install
	var componentWithClients component.Component
	switch name {
	case "gateway-api":
		componentWithClients = gatewayapi.NewComponent(k8sClient)
	case "contour":
		componentWithClients = contour.NewComponent(helmClient, k8sClient)
	case "gateway-controller":
		componentWithClients = gatewaycontroller.NewComponent(helmClient, k8sClient)
	case "cert-manager":
		componentWithClients = certmanager.NewComponent(nil)
	case "storage":
		// Handle storage-specific flags
		backend := cmd.String("backend")
		cfg["backend"] = backend
		if backend == "nfs" {
			nfsServer := cmd.String("nfs-server")
			nfsPath := cmd.String("nfs-path")
			if nfsServer == "" || nfsPath == "" {
				return fmt.Errorf("--nfs-server and --nfs-path are required for nfs backend")
			}
			cfg["nfs"] = map[string]interface{}{
				"server": nfsServer,
				"path":   nfsPath,
			}
		}
		componentWithClients = componentStorage.NewComponent(helmClient, k8sClient)
	case "seaweedfs":
		componentWithClients = seaweedfs.NewComponent(helmClient, k8sClient)
	case "prometheus":
		// Auto-populate external targets for infrastructure services
		externalTargets := buildExternalTargetsFromStackConfig(stackConfig)
		if len(externalTargets) > 0 {
			cfg["external_targets"] = externalTargets
		}
		componentWithClients = prometheus.NewComponent(helmClient, k8sClient)
	case "loki":
		// Get SeaweedFS credentials (from stack config, falling back to a k8s secret)
		seaweedfsKey, seaweedfsSecret, err := getSeaweedFSCredentials(stackConfig, k8sClient)
		if err != nil {
			return fmt.Errorf("failed to get SeaweedFS credentials: %w", err)
		}
		cfg["s3_endpoint"] = "http://seaweedfs-s3.seaweedfs.svc.cluster.local:8333"
		cfg["s3_access_key"] = seaweedfsKey
		cfg["s3_secret_key"] = seaweedfsSecret
		cfg["s3_bucket"] = "loki"
		cfg["s3_region"] = "us-east-1"
		componentWithClients = loki.NewComponent(helmClient, k8sClient)
	case "grafana":
		// Honor the grafana settings from the stack config (values, ingress,
		// alerting contact points, etc.) so this path doesn't fall back to bare
		// defaults.
		if comp, ok := stackConfig.Components["grafana"]; ok {
			for k, v := range comp.Config {
				if _, present := cfg[k]; !present {
					cfg[k] = v
				}
			}
		}
		// Default datasource endpoints only where the stack config hasn't set them.
		if !hasNonEmptyString(cfg, "prometheus_url") {
			cfg["prometheus_url"] = "http://kube-prometheus-stack-prometheus.monitoring.svc.cluster.local:9090"
		}
		if !hasNonEmptyString(cfg, "loki_url") {
			cfg["loki_url"] = "http://loki-gateway.monitoring.svc.cluster.local:80"
		}
		// Resolve ${secret:...} references in alerting contact points (e.g. an
		// Opsgenie API key stored in OpenBAO) so the real value reaches Grafana and
		// is never persisted in the stack config.
		if alerting, ok := cfg["alerting"]; ok {
			resolver, resCtx, rerr := buildSecretResolver(stackConfig)
			if rerr != nil {
				return fmt.Errorf("failed to set up secret resolver for alerting: %w", rerr)
			}
			if err := resolveSecretRefs(alerting, resolver, resCtx); err != nil {
				return fmt.Errorf("failed to resolve alerting secrets: %w", err)
			}
		}
		componentWithClients = grafana.NewComponent(helmClient, k8sClient)
	case "external-dns":
		// Get PowerDNS configuration if available
		if stackConfig.DNS != nil && stackConfig.SetupState != nil && stackConfig.SetupState.DNSInstalled {
			dnsAddr, err := stackConfig.GetPrimaryDNSAddress()
			if err == nil {
				cfg["provider"] = "pdns"
				// PowerDNS config must be a nested map
				pdnsConfig := map[string]interface{}{
					"api_url": fmt.Sprintf("http://%s:8081", dnsAddr),
				}
				// Try to get API key from OpenBAO
				apiKey, err := getDNSAPIKey(stackConfig)
				if err == nil {
					pdnsConfig["api_key"] = apiKey
				}
				cfg["powerdns"] = pdnsConfig
			}
		}
		componentWithClients = externaldns.NewComponent(helmClient, k8sClient)
	case "velero":
		// Honor the velero settings from the stack config (values block, schedule,
		// off-cluster S3 target, deploy_node_agent, etc.) so this path doesn't silently
		// fall back to bare defaults.
		if comp, ok := stackConfig.Components["velero"]; ok {
			for k, v := range comp.Config {
				if _, present := cfg[k]; !present {
					cfg[k] = v
				}
			}
		}
		// Default the S3 target to in-cluster SeaweedFS, but only where the stack config
		// hasn't specified one (e.g. an off-cluster local backup target).
		if !hasNonEmptyString(cfg, "s3_endpoint") {
			cfg["s3_endpoint"] = "http://seaweedfs-s3.seaweedfs.svc.cluster.local:8333"
		}
		if !hasNonEmptyString(cfg, "s3_bucket") {
			cfg["s3_bucket"] = "velero"
		}
		if !hasNonEmptyString(cfg, "s3_region") {
			cfg["s3_region"] = "us-east-1"
		}
		// Resolve credentials from SeaweedFS only when not explicitly provided.
		if !hasNonEmptyString(cfg, "s3_access_key") || !hasNonEmptyString(cfg, "s3_secret_key") {
			seaweedfsKey, seaweedfsSecret, err := getSeaweedFSCredentials(stackConfig, k8sClient)
			if err != nil {
				return fmt.Errorf("failed to get SeaweedFS credentials: %w", err)
			}
			cfg["s3_access_key"] = seaweedfsKey
			cfg["s3_secret_key"] = seaweedfsSecret
		}
		componentWithClients = velero.NewComponent(helmClient, k8sClient)
	case "openbao-injector":
		// Inject the OpenBao address so the webhook knows where to reach it
		url, err := stackConfig.GetPrimaryOpenBAOURL()
		if err != nil {
			return fmt.Errorf("failed to get OpenBao address for injector: %w", err)
		}
		cfg["external_vault_addr"] = url

		// Create OpenBAO client for configuring Kubernetes auth
		openbaoClient, err := createOpenBAOClient(stackConfig)
		if err != nil {
			return fmt.Errorf("failed to create OpenBAO client: %w", err)
		}
		componentWithClients = openbaoinjector.NewComponent(helmClient, k8sClient, openbaoClient)
	default:
		return fmt.Errorf("unknown kubernetes component: %s", name)
	}

	fmt.Printf("\nInstalling component: %s\n", name)

	// Install the component
	if err := componentWithClients.Install(ctx, cfg); err != nil {
		return fmt.Errorf("installation failed: %w", err)
	}

	fmt.Printf("\n✓ Component %s installed successfully\n", name)

	// After Grafana is installed/upgraded, (re)install the bundled default dashboards
	// plus any user dashboards, so they land on every stack update. Non-fatal.
	if name == "grafana" {
		if err := syncGrafanaDashboards(ctx, stackConfig, k8sClient, cfg); err != nil {
			fmt.Printf("⚠ Dashboards not synced (run 'foundry dashboard sync'): %v\n", err)
		}
	}

	// Update setup state
	if err := updateSetupState(cmd, stackConfig, name, cfg); err != nil {
		fmt.Printf("\n⚠ Warning: Failed to update setup state: %v\n", err)
	}

	return nil
}

// resolveSecretRefs walks a nested config value and replaces any string that is a
// ${secret:path:key} reference with its resolved value. Used to pull alerting
// credentials (API keys/tokens) from OpenBAO at install time.
func resolveSecretRefs(v interface{}, resolver *secrets.ChainResolver, resCtx *secrets.ResolutionContext) error {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			if s, ok := val.(string); ok {
				ref, err := secrets.ParseSecretRef(s)
				if err != nil {
					return fmt.Errorf("invalid secret reference %q: %w", s, err)
				}
				if ref == nil {
					continue // not a secret reference; leave as-is
				}
				resolved, rerr := resolver.Resolve(resCtx, *ref)
				if rerr != nil {
					return fmt.Errorf("resolve %s: %w", s, rerr)
				}
				t[k] = resolved
			} else if err := resolveSecretRefs(val, resolver, resCtx); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, item := range t {
			if err := resolveSecretRefs(item, resolver, resCtx); err != nil {
				return err
			}
		}
	}
	return nil
}

// syncGrafanaDashboards seeds the bundled default dashboards into the stack's
// dashboards directory and applies them (plus any user dashboards) as ConfigMaps,
// so they are installed/refreshed whenever Grafana is installed or upgraded.
func syncGrafanaDashboards(ctx context.Context, stackConfig *config.Config, k8sClient *k8s.Client, cfg component.ComponentConfig) error {
	if k8sClient == nil {
		return fmt.Errorf("kubernetes client unavailable")
	}
	configDir, err := config.GetConfigDir()
	if err != nil {
		return err
	}
	namespace := "monitoring"
	if ns, ok := cfg["namespace"].(string); ok && ns != "" {
		namespace = ns
	}
	seeded, res, err := dashboards.InstallForStack(ctx, k8sClient.Clientset(), configDir, stackConfig.Cluster.Name, namespace)
	if err != nil {
		return err
	}
	fmt.Printf("  ✓ dashboards: %d default seeded, %d created, %d updated (namespace %q)\n", seeded, res.Created, res.Updated, namespace)
	return nil
}

// hasNonEmptyString reports whether the config map holds a non-empty string at key.
func hasNonEmptyString(cfg component.ComponentConfig, key string) bool {
	v, ok := cfg[key]
	if !ok {
		return false
	}
	s, ok := v.(string)
	return ok && s != ""
}

// getSeaweedFSCredentials resolves the SeaweedFS S3 access/secret keys.
//
// The authoritative source is the stack config's seaweedfs component
// (components.seaweedfs.access_key / secret_key) — these are the keys that
// Loki and Velero are wired to use, and SeaweedFS S3 runs with auth disabled
// by default so there is no dedicated S3-credentials secret to read. For older
// or custom setups that do publish a "seaweedfs" secret (keys accessKey/secretKey),
// we fall back to it.
func getSeaweedFSCredentials(stackConfig *config.Config, k8sClient *k8s.Client) (string, string, error) {
	// Primary: stack config seaweedfs component.
	if stackConfig != nil {
		if swfs, ok := stackConfig.Components["seaweedfs"]; ok {
			accessKey, _ := swfs.Config["access_key"].(string)
			secretKey, _ := swfs.Config["secret_key"].(string)
			if accessKey != "" && secretKey != "" {
				return accessKey, secretKey, nil
			}
		}
	}

	// Fallback: legacy k8s secret named "seaweedfs" with accessKey/secretKey.
	if k8sClient != nil {
		secret, err := k8sClient.GetSecret(context.Background(), "seaweedfs", "seaweedfs")
		if err == nil {
			accessKey, okA := secret.Data["accessKey"]
			secretKey, okS := secret.Data["secretKey"]
			if okA && okS {
				return string(accessKey), string(secretKey), nil
			}
		}
	}

	return "", "", fmt.Errorf("could not resolve SeaweedFS S3 credentials: set components.seaweedfs.access_key and secret_key in the stack config")
}

// getDNSAPIKey retrieves the DNS API key from OpenBAO
func getDNSAPIKey(stackConfig *config.Config) (string, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}

	openBAOAddr, err := stackConfig.GetPrimaryOpenBAOURL()
	if err != nil {
		return "", err
	}

	keysPath := filepath.Join(configDir, "openbao-keys", stackConfig.Cluster.Name, "keys.json")
	keysData, err := os.ReadFile(keysPath)
	if err != nil {
		return "", err
	}

	var keys struct {
		RootToken string `json:"root_token"`
	}
	if err := json.Unmarshal(keysData, &keys); err != nil {
		return "", err
	}

	openBAOClient := openbao.NewClient(openBAOAddr, keys.RootToken)
	ctx := context.Background()
	data, err := openBAOClient.ReadSecretV2(ctx, "foundry-core", "dns")
	if err != nil {
		return "", err
	}
	if apiKey, ok := data["api_key"].(string); ok {
		return apiKey, nil
	}
	return "", fmt.Errorf("api_key not found in OpenBAO")
}

// createOpenBAOClient creates an OpenBAO client from stack config
func createOpenBAOClient(stackConfig *config.Config) (*openbao.Client, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return nil, err
	}

	openBAOAddr, err := stackConfig.GetPrimaryOpenBAOURL()
	if err != nil {
		return nil, err
	}

	keysPath := filepath.Join(configDir, "openbao-keys", stackConfig.Cluster.Name, "keys.json")
	keysData, err := os.ReadFile(keysPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read OpenBAO keys file: %w", err)
	}

	var keys struct {
		RootToken string `json:"root_token"`
	}
	if err := json.Unmarshal(keysData, &keys); err != nil {
		return nil, fmt.Errorf("failed to parse OpenBAO keys file: %w", err)
	}

	if keys.RootToken == "" {
		return nil, fmt.Errorf("root token not found in keys file")
	}

	return openbao.NewClient(openBAOAddr, keys.RootToken), nil
}

// installSSHComponent installs a container-based component via SSH
func installSSHComponent(ctx context.Context, cmd *cli.Command, name string, stackConfig *config.Config, dryRun bool, version string) error {
	// Handle k3s-specific configuration early (before dry-run check)
	if name == "k3s" || name == "kubernetes" {
		// Get component from registry to validate it exists
		comp := component.Get(name)
		if comp == nil {
			return component.ErrComponentNotFound(name)
		}

		// Validate a control plane node exists before doing any work
		targetHost, err := getTargetHostForComponent(name, stackConfig)
		if err != nil {
			return fmt.Errorf("failed to determine target host: %w", err)
		}

		fmt.Printf("Target host: %s (%s)\n", targetHost.Hostname, targetHost.Address)

		// Dry-run takes the same path so the printed plan reflects the hosts
		// that would really be contacted; the connector is never invoked.
		allNodes := cmd.Bool("all-nodes")
		if err := installK3sComponent(ctx, stackConfig, sshNodeConnector(stackConfig), dryRun, allNodes); err != nil {
			return err
		}
		if dryRun {
			fmt.Println("\nNote: This is a dry-run. No changes will be made.")
		}
		return nil
	}

	// Determine target host for this component
	targetHost, err := getTargetHostForComponent(name, stackConfig)
	if err != nil {
		return fmt.Errorf("failed to determine target host: %w", err)
	}

	fmt.Printf("Target host: %s (%s)\n", targetHost.Hostname, targetHost.Address)

	if dryRun {
		fmt.Printf("\nWould install component: %s\n", name)
		fmt.Printf("Target host: %s\n", targetHost.Hostname)
		if version != "" {
			fmt.Printf("Version: %s\n", version)
		}
		fmt.Println("\nNote: This is a dry-run. No changes will be made.")
		return nil
	}

	// Get component from registry
	comp := component.Get(name)
	if comp == nil {
		return component.ErrComponentNotFound(name)
	}

	// Establish SSH connection to target host
	fmt.Printf("Connecting to %s...\n", targetHost.Hostname)
	conn, err := connectToHost(targetHost, stackConfig)
	if err != nil {
		return fmt.Errorf("failed to connect to host: %w", err)
	}
	defer conn.Close()
	fmt.Println("✓ Connected")

	// Create adapter for components that need container.SSHExecutor
	executor := &sshExecutorAdapter{conn: conn}

	// Build component config
	cfg := component.ComponentConfig{}
	if version != "" {
		cfg["version"] = version
	}

	// Add SSH connection to config (components extract this)
	// We pass both the raw connection and the executor adapter
	cfg["host"] = executor
	cfg["ssh_conn"] = conn // Some components might need the raw connection

	// Add cluster name for OpenBAO key storage
	cfg["cluster_name"] = stackConfig.Cluster.Name

	// Add keys directory for OpenBAO
	keysDir, err := config.GetKeysDir()
	if err != nil {
		return fmt.Errorf("failed to get keys directory: %w", err)
	}
	// Use openbao-keys subdirectory
	cfg["keys_dir"] = filepath.Join(filepath.Dir(keysDir), "openbao-keys")

	// Add OpenBAO API URL (for OpenBAO component initialization)
	if name == "openbao" {
		url, err := stackConfig.GetPrimaryOpenBAOURL()
		if err == nil {
			cfg["api_url"] = url
		}
	}

	// Handle PowerDNS API key generation and storage
	if (name == "dns" || name == "powerdns") && stackConfig.SetupState.OpenBAOInitialized {
		apiKey, err := ensureDNSAPIKey(stackConfig)
		if err != nil {
			return fmt.Errorf("failed to setup DNS API key: %w", err)
		}
		cfg["api_key"] = apiKey

		// Pass all zones as local zones for Recursor forwarding
		// This includes primary_domain + kubernetes_zones + infrastructure_zones (deduplicated)
		localZones := []string{}
		seen := make(map[string]bool)
		if stackConfig.Cluster.PrimaryDomain != "" {
			localZones = append(localZones, stackConfig.Cluster.PrimaryDomain)
			seen[stackConfig.Cluster.PrimaryDomain] = true
		}
		if stackConfig.DNS != nil {
			for _, zone := range stackConfig.DNS.KubernetesZones {
				if !seen[zone.Name] {
					localZones = append(localZones, zone.Name)
					seen[zone.Name] = true
				}
			}
			for _, zone := range stackConfig.DNS.InfrastructureZones {
				if !seen[zone.Name] {
					localZones = append(localZones, zone.Name)
					seen[zone.Name] = true
				}
			}
		}
		if len(localZones) > 0 {
			cfg["local_zones"] = localZones
			fmt.Printf("  Local zones for DNS recursor: %v\n", localZones)
		}
	}

	fmt.Printf("\nInstalling component: %s\n", name)

	// Install component
	if err := comp.Install(ctx, cfg); err != nil {
		return fmt.Errorf("failed to install component %s: %w", name, err)
	}

	fmt.Printf("\n✓ Component %s installed successfully\n", name)

	// Update setup state and component-specific config
	if err := updateSetupState(cmd, stackConfig, name, cfg); err != nil {
		// Don't fail the whole installation if state update fails
		// Just warn the user
		fmt.Printf("\n⚠ Warning: Failed to update setup state: %v\n", err)
		fmt.Println("The component is installed and working, but state tracking may be incorrect.")
	}

	// Handle DNS record registration (bidirectional)
	if err := handleDNSRegistration(ctx, name, stackConfig); err != nil {
		// Don't fail the installation if DNS registration fails
		// Just warn the user
		fmt.Printf("\n⚠ Warning: Failed to register DNS records: %v\n", err)
		fmt.Println("The component is installed and working, but DNS records may need manual creation.")
	}

	return nil
}

// k3sNodeConnector opens an executor for a single cluster node. The returned
// close func releases the underlying connection. Injected so tests can drive
// installK3sComponent without real SSH.
type k3sNodeConnector func(h *host.Host) (k3s.SSHExecutor, func(), error)

// sshNodeConnector is the production connector: it dials each node over SSH.
func sshNodeConnector(stackConfig *config.Config) k3sNodeConnector {
	return func(h *host.Host) (k3s.SSHExecutor, func(), error) {
		conn, err := connectToHost(h, stackConfig)
		if err != nil {
			return nil, nil, err
		}
		return &sshExecutorAdapter{conn: conn}, func() { conn.Close() }, nil
	}
}

// k3sClusterHosts returns every cluster node (control plane first, then workers).
func k3sClusterHosts(stackConfig *config.Config) []*host.Host {
	cpHosts := stackConfig.GetClusterControlPlaneHosts()
	workerHosts := stackConfig.GetClusterWorkerHosts()

	// Copy rather than append onto cpHosts, whose backing array may have spare
	// capacity that append would overwrite.
	allHosts := make([]*host.Host, 0, len(cpHosts)+len(workerHosts))
	allHosts = append(allHosts, cpHosts...)
	allHosts = append(allHosts, workerHosts...)
	return allHosts
}

// installK3sComponent handles k3s component installation/repair via SSH
// It configures registries.yaml and other node-level settings.
// Each node is contacted over its own connection, so --all-nodes genuinely
// reconciles workers rather than repeating work on the control plane.
func installK3sComponent(ctx context.Context, stackConfig *config.Config, connect k3sNodeConnector, dryRun bool, allNodes bool) error {
	cpHosts := stackConfig.GetClusterControlPlaneHosts()
	allHosts := k3sClusterHosts(stackConfig)

	if len(allHosts) == 0 {
		return fmt.Errorf("no cluster hosts configured")
	}

	targets := allHosts
	if !allNodes {
		// Default: process first control plane node only
		if len(cpHosts) == 0 {
			return fmt.Errorf("no control plane host configured for k3s")
		}
		targets = cpHosts[:1]
	}

	if allNodes {
		verb := "Reconciling"
		if dryRun {
			verb = "Would reconcile"
		}
		fmt.Printf("%s registries.yaml on %d cluster nodes...\n", verb, len(targets))
	}

	for _, h := range targets {
		if dryRun {
			fmt.Printf("\nWould reconcile node: %s (%s)\n", h.Hostname, h.Address)
		} else {
			fmt.Printf("\nProcessing node: %s (%s)\n", h.Hostname, h.Address)
		}
		if err := reconcileK3sNodeWithConnector(ctx, stackConfig, h, connect, dryRun); err != nil {
			return fmt.Errorf("failed to reconcile node %s: %w", h.Hostname, err)
		}
	}

	if allNodes && !dryRun {
		fmt.Println("\n✓ All nodes reconciled")
	}

	if err := reconcileKubeconfigEndpoint(ctx, stackConfig, dryRun); err != nil {
		// The nodes are already converged; a stale client endpoint should not
		// fail the install, but the user must be told it was not repaired.
		fmt.Printf("\n⚠ Warning: kubeconfig endpoint not updated: %v\n", err)
	}

	return nil
}

// installTailscaleComponent installs or converges the Tailscale operator.
//
// The API VIP is deliberately not passed anywhere in this path: it is internal
// to the cluster data plane, and advertising it on the tailnet is the topology
// this work removed.
func installTailscaleComponent(ctx context.Context, cmd *cli.Command, stackConfig *config.Config, helmClient *helm.Client, k8sClient *k8s.Client) error {
	tsCfg, err := buildTailscaleConfig(ctx, cmd, stackConfig)
	if err != nil {
		return err
	}

	adapter, err := tailscale.NewKubeAdapter(k8sClient.Clientset(), k8sClient.DynamicClient())
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes adapter: %w", err)
	}

	helmInstaller, err := tailscale.NewHelmInstaller(helmClient, tsCfg)
	if err != nil {
		return fmt.Errorf("failed to create Helm installer: %w", err)
	}
	crdInstaller, err := tailscale.NewCRDInstaller(adapter, tsCfg)
	if err != nil {
		return fmt.Errorf("failed to create CRD installer: %w", err)
	}
	installer, err := tailscale.NewInstaller(helmInstaller, crdInstaller, adapter, tsCfg)
	if err != nil {
		return fmt.Errorf("failed to create Tailscale installer: %w", err)
	}

	fmt.Println("\nInstalling component: tailscale")
	if err := installer.Install(ctx); err != nil {
		return fmt.Errorf("installation failed: %w", err)
	}
	fmt.Println("\n✓ Component tailscale installed successfully")

	if len(tsCfg.AdvertiseRoutes) > 0 {
		fmt.Printf("  Advertising subnet routes: %v\n", tsCfg.AdvertiseRoutes)
	} else {
		fmt.Println("  No subnet routes advertised (cluster networks stay internal)")
	}

	// Report what the operator is exposing, so a successful install says
	// something useful rather than just "done".
	checker, err := tailscale.NewHealthChecker(helmInstaller, adapter)
	if err == nil {
		if health, herr := checker.Check(ctx); herr == nil {
			fmt.Printf("  %s\n", health.Summary())
		}
	}

	return nil
}

// buildTailscaleConfig assembles the Tailscale component config from stack.yaml,
// resolving OAuth credentials through OpenBAO.
//
// When credentials are supplied literally in stack.yaml they are written to
// OpenBAO and the config is rewritten to reference them, so plaintext
// credentials do not persist in the file.
func buildTailscaleConfig(ctx context.Context, cmd *cli.Command, stackConfig *config.Config) (*tailscale.Config, error) {
	compCfg := component.ComponentConfig{}
	if comp, ok := stackConfig.Components["tailscale"]; ok {
		for k, v := range comp.Config {
			compCfg[k] = v
		}
	}

	openbaoClient, err := createOpenBAOClient(stackConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create OpenBAO client: %w\n\nTailscale credentials are stored in OpenBAO; install it first", err)
	}

	// A ${secret:...} reference means "read it from OpenBAO", so it is not a
	// literal credential.
	configID, _ := compCfg.GetString("oauth_client_id")
	configSecret, _ := compCfg.GetString("oauth_client_secret")
	if isSecretRef(configID) || isSecretRef(configSecret) {
		configID, configSecret = "", ""
	}

	clientID, clientSecret, storedNew, err := tailscale.ResolveCredentials(ctx, openbaoClient, configID, configSecret)
	if err != nil {
		return nil, err
	}
	if storedNew {
		fmt.Println("  ✓ Tailscale OAuth credentials stored in OpenBAO")
		if err := replaceTailscaleCredentialRefs(cmd, stackConfig); err != nil {
			fmt.Printf("  ⚠ Warning: credentials stored, but stack.yaml still holds them in plaintext: %v\n", err)
		} else {
			fmt.Println("  ✓ stack.yaml now references the stored credentials")
		}
	}

	cfg := &tailscale.Config{
		OAuthClientID:     &clientID,
		OAuthClientSecret: &clientSecret,
	}
	if image, ok := compCfg.GetString("operator_image"); ok && image != "" {
		cfg.OperatorImage = &image
	}
	if tags, ok := compCfg.GetStringSlice("tags"); ok {
		cfg.Tags = tags
	}
	if routes, ok := compCfg.GetStringSlice("advertise_routes"); ok {
		cfg.AdvertiseRoutes = routes
	}

	return cfg, nil
}

// replaceTailscaleCredentialRefs rewrites literal OAuth credentials in the
// stack config as ${secret:...} references and saves it.
func replaceTailscaleCredentialRefs(cmd *cli.Command, stackConfig *config.Config) error {
	comp, ok := stackConfig.Components["tailscale"]
	if !ok {
		return fmt.Errorf("tailscale component not present in config")
	}
	if comp.Config == nil {
		comp.Config = map[string]interface{}{}
	}
	comp.Config["oauth_client_id"] = "${secret:tailscale:client_id}"
	comp.Config["oauth_client_secret"] = "${secret:tailscale:client_secret}"
	stackConfig.Components["tailscale"] = comp

	configPath, err := config.FindConfig(cmd.String("config"))
	if err != nil {
		return err
	}
	return config.Save(stackConfig, configPath)
}

// isSecretRef reports whether a config value is a ${secret:...} reference
// rather than a literal credential.
func isSecretRef(value string) bool {
	return strings.HasPrefix(value, "${secret:") && strings.HasSuffix(value, "}")
}

// kubeconfigClientEndpoint returns the address remote clients should use to
// reach the API server, derived from the first control plane host. Returns ""
// when there is no cluster to point at, or when the host has no Tailscale
// address, no VIP, and no usable node address.
func kubeconfigClientEndpoint(stackConfig *config.Config) string {
	if stackConfig == nil {
		return ""
	}
	cpHosts := stackConfig.GetClusterControlPlaneHosts()
	if len(cpHosts) == 0 {
		return ""
	}
	// A host with no usable data plane address is reported by validation; here
	// it simply contributes no fallback.
	nodeIP, _ := cpHosts[0].K3sNodeIP()
	return k3s.ClientEndpoint(cpHosts[0].TailscaleAddress, stackConfig.Cluster.VIP, nodeIP)
}

// reconcileKubeconfigEndpoint re-points the stored kubeconfig at the address
// remote clients should use — the control plane's Tailscale address when one is
// configured, otherwise the API VIP.
//
// This is the repair path for a kubeconfig written before its control plane had
// a Tailscale address: node reconciliation alone never touches the kubeconfig,
// so the endpoint would otherwise stay stale forever. Idempotent — a kubeconfig
// already pointing at the right endpoint is left untouched.
func reconcileKubeconfigEndpoint(ctx context.Context, stackConfig *config.Config, dryRun bool) error {
	if dryRun {
		endpoint := kubeconfigClientEndpoint(stackConfig)
		if endpoint == "" {
			return nil
		}
		fmt.Printf("\n[dry-run] Would ensure kubeconfig points at %s\n", k3s.KubeconfigServerURL(endpoint))
		return nil
	}

	return reconcileKubeconfigEndpointWithClient(ctx, stackConfig, func() (k3s.KubeconfigClient, error) {
		return createOpenBAOClient(stackConfig)
	})
}

// kubeconfigClientFactory opens the secret store holding the kubeconfig.
// Injected so the reconcile path can be tested without a live OpenBAO.
type kubeconfigClientFactory func() (k3s.KubeconfigClient, error)

// reconcileKubeconfigEndpointWithClient is the non-dry-run body of
// reconcileKubeconfigEndpoint, with the secret-store dependency injected.
func reconcileKubeconfigEndpointWithClient(ctx context.Context, stackConfig *config.Config, newClient kubeconfigClientFactory) error {
	endpoint := kubeconfigClientEndpoint(stackConfig)
	if endpoint == "" {
		return nil
	}

	client, err := newClient()
	if err != nil {
		return fmt.Errorf("failed to create OpenBAO client: %w", err)
	}

	// The OpenBAO copy and the local ~/.foundry/kubeconfig are independent
	// stores that can disagree — an endpoint fixed in one may still be stale in
	// the other. Reconcile each against its own current state rather than
	// gating the local mirror on whether OpenBAO happened to need a write.
	storedChanged, err := k3s.RefreshStoredKubeconfig(ctx, client, endpoint)
	if err != nil {
		return err
	}

	if storedChanged {
		fmt.Printf("\n✓ Kubeconfig endpoint updated to %s\n", k3s.KubeconfigServerURL(endpoint))
	} else {
		fmt.Printf("\n✓ Kubeconfig endpoint unchanged (%s)\n", k3s.KubeconfigServerURL(endpoint))
	}

	// Mirror to ~/.foundry/kubeconfig, which is what every other foundry command
	// and the user's kubectl actually read.
	exportedChanged, err := exportKubeconfigIfStale(ctx, client, endpoint)
	if err != nil {
		return fmt.Errorf("stored in OpenBAO but local export failed: %w", err)
	}

	configDir, cfgErr := config.GetConfigDir()
	kubeconfigPath := "~/.foundry/kubeconfig"
	if cfgErr == nil {
		kubeconfigPath = filepath.Join(configDir, "kubeconfig")
	}
	if exportedChanged {
		fmt.Printf("✓ Kubeconfig exported to %s\n", kubeconfigPath)
	} else {
		fmt.Printf("✓ Local kubeconfig already current (%s)\n", kubeconfigPath)
	}

	return nil
}

// exportKubeconfigIfStale writes the stored kubeconfig to ~/.foundry/kubeconfig
// unless the local file already targets endpoint, returning whether it wrote.
//
// The local file is checked on its own terms: it can be stale while the copy in
// OpenBAO is already correct, so its freshness cannot be inferred from whether
// OpenBAO needed updating.
func exportKubeconfigIfStale(ctx context.Context, client k3s.KubeconfigClient, endpoint string) (bool, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return false, fmt.Errorf("failed to get config directory: %w", err)
	}

	existing, err := os.ReadFile(filepath.Join(configDir, "kubeconfig"))
	if err == nil && k3s.KubeconfigTargets(string(existing), endpoint) {
		return false, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("failed to read local kubeconfig: %w", err)
	}

	if err := exportKubeconfig(ctx, client); err != nil {
		return false, err
	}
	return true, nil
}

// exportKubeconfig writes the kubeconfig stored in OpenBAO to ~/.foundry/kubeconfig.
func exportKubeconfig(ctx context.Context, client k3s.KubeconfigClient) error {
	kubeconfig, err := k3s.LoadKubeconfig(ctx, client)
	if err != nil {
		return fmt.Errorf("failed to load kubeconfig from OpenBAO: %w", err)
	}

	configDir, err := config.GetConfigDir()
	if err != nil {
		return fmt.Errorf("failed to get config directory: %w", err)
	}
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	kubeconfigPath := filepath.Join(configDir, "kubeconfig")
	if err := os.WriteFile(kubeconfigPath, []byte(kubeconfig), 0600); err != nil {
		return fmt.Errorf("failed to write kubeconfig: %w", err)
	}

	return nil
}

// reconcileK3sNodeWithConnector opens a per-node connection and reconciles it.
// Dry-run needs no connection, so none is opened.
func reconcileK3sNodeWithConnector(ctx context.Context, stackConfig *config.Config, h *host.Host, connect k3sNodeConnector, dryRun bool) error {
	if dryRun {
		return reconcileK3sNode(ctx, stackConfig, h, nil, true)
	}

	executor, closeConn, err := connect(h)
	if err != nil {
		return fmt.Errorf("failed to connect to host %s: %w", h.Hostname, err)
	}
	defer closeConn()

	return reconcileK3sNode(ctx, stackConfig, h, executor, false)
}

// buildK3sNodeConfig assembles the k3s.Config applied to a node.
// Interface is deliberately left unset: it names a NIC (e.g. eth0), not an
// address, and is detected on the node itself when kube-vip needs it.
func buildK3sNodeConfig(stackConfig *config.Config, h *host.Host) (*k3s.Config, error) {
	nodeIP, err := h.K3sNodeIP()
	if err != nil {
		return nil, err
	}
	// The VIP is only carried into the node config when kube-vip is actually
	// deployed for it. A configured-but-undeployed VIP stays out, so a single
	// control plane cluster does not recreate kube-vip on reconcile; it is still
	// added to the certificate SANs from stackConfig.Cluster.VIP below.
	k3sConfig := &k3s.Config{
		NodeIP: nodeIP,
	}
	if stackConfig.VIPEnabled() {
		k3sConfig.VIP = stackConfig.Cluster.VIP
	} else if stackConfig.Cluster.VIP != "" {
		k3sConfig.TLSSANs = append(k3sConfig.TLSSANs, stackConfig.Cluster.VIP)
	}
	k3sConfig.FlannelIface = h.FlannelInterface
	k3sConfig.AdvertiseAddress = k3sConfig.NodeIP

	// Parse additional registries from component config
	if k3sCompCfg, exists := stackConfig.Components["k3s"]; exists {
		k3sConfig.AdditionalRegistries = k3s.ParseAdditionalRegistries(k3sCompCfg.Config)
	}

	// Populate registry config if Zot is configured
	zotAddr, err := stackConfig.GetPrimaryZotAddress()
	if err == nil && zotAddr != "" {
		k3s.PopulateRegistryConfig(k3sConfig, zotAddr)
	} else if stackConfig.SetupState != nil && stackConfig.SetupState.ZotInstalled {
		// Zot is installed but its address is unresolvable, so registries.yaml
		// cannot be written for this node.
		fmt.Printf("  ⚠ Warning: Zot is installed but its address could not be resolved - registries.yaml will not be written\n")
	}

	return k3sConfig, nil
}

// reconcileK3sNode reconciles a single k3s node's configuration.
// executor must be connected to h; it may be nil when dryRun is true.
func reconcileK3sNode(ctx context.Context, stackConfig *config.Config, h *host.Host, executor k3s.SSHExecutor, dryRun bool) error {
	k3sConfig, err := buildK3sNodeConfig(stackConfig, h)
	if err != nil {
		return fmt.Errorf("unsafe k3s network identity for %s: %w", h.Hostname, err)
	}

	if dryRun {
		fmt.Println("  [dry-run] Would apply k3s configuration:")
		fmt.Print(k3s.IndentNetworkConfig(k3s.GenerateNetworkConfigYAML(k3sConfig, h.HasRole(host.RoleClusterControlPlane)), "    "))
		if k3sConfig.RegistryConfig != "" {
			fmt.Println("    - registries.yaml configuration present")
		} else {
			fmt.Println("    - no registries.yaml configuration")
		}
		return nil
	}

	isWorker := h.HasRole(host.RoleClusterWorker) && !h.HasRole(host.RoleClusterControlPlane)
	// Check the correct service for this node role.
	var isInstalled bool
	if isWorker {
		isInstalled, err = k3s.IsK3sAgentInstalled(executor)
	} else {
		isInstalled, err = k3s.IsK3sInstalled(executor)
	}
	if err != nil {
		return fmt.Errorf("failed to check k3s status: %w", err)
	}

	if !isInstalled {
		return fmt.Errorf("k3s is not installed on node %s - use 'foundry stack install' to install k3s", h.Hostname)
	}
	if err := k3s.ResolveNodeNetwork(executor, k3sConfig); err != nil {
		return fmt.Errorf("failed to resolve node network: %w", err)
	}

	// Use the idempotent update path
	fmt.Println("  Applying k3s configuration (idempotent update)...")
	if isWorker {
		err = k3s.UpdateK3sAgentConfig(ctx, executor, k3sConfig)
	} else {
		err = k3s.UpdateK3sConfig(ctx, executor, k3sConfig)
	}
	if err != nil {
		return fmt.Errorf("failed to update k3s config: %w", err)
	}

	fmt.Printf("  ✓ Configuration applied to %s\n", h.Hostname)
	return nil
}

// handleDNSRegistration handles bidirectional DNS record registration
// - If DNS is being installed: look backward at installed components and create their records
// - If another component is being installed: if DNS exists, self-register
func handleDNSRegistration(ctx context.Context, componentName string, stackConfig *config.Config) error {
	// Skip if we don't have network config or cluster domain or setup state
	if stackConfig == nil || stackConfig.Network == nil || stackConfig.Cluster.PrimaryDomain == "" || stackConfig.SetupState == nil {
		return nil
	}

	switch componentName {
	case "dns", "powerdns":
		// DNS is being installed - look backward at installed components
		return registerExistingComponents(ctx, stackConfig)
	case "openbao":
		// OpenBAO is being installed - self-register if DNS exists
		if stackConfig.SetupState.DNSInstalled {
			return registerComponentDNS(ctx, "openbao", stackConfig)
		}
	case "zot":
		// Zot is being installed - self-register if DNS exists
		if stackConfig.SetupState.DNSInstalled {
			return registerComponentDNS(ctx, "zot", stackConfig)
		}
	case "k3s", "kubernetes":
		// K3s is being installed - self-register VIP if DNS exists
		if stackConfig.SetupState.DNSInstalled {
			return registerComponentDNS(ctx, "k8s", stackConfig)
		}
	}

	return nil
}

// registerExistingComponents creates DNS records for components that were installed before DNS
func registerExistingComponents(ctx context.Context, stackConfig *config.Config) error {
	fmt.Println("\nRegistering DNS records for existing components...")

	// Get DNS client
	dnsClient, err := getDNSClient(stackConfig)
	if err != nil {
		return fmt.Errorf("failed to create DNS client: %w", err)
	}

	// Import dns package types
	dnsZone := stackConfig.Cluster.PrimaryDomain

	// Register each installed component
	registered := 0

	if stackConfig.SetupState.OpenBAOInstalled {
		addr, err := stackConfig.GetPrimaryOpenBAOAddress()
		if err == nil {
			if err := registerServiceRecord(dnsClient, dnsZone, "openbao", addr); err != nil {
				return fmt.Errorf("failed to register openbao DNS record: %w", err)
			}
			registered++
		}
	}

	// Always register DNS service (pointing to itself)
	addr, err := stackConfig.GetPrimaryDNSAddress()
	if err == nil {
		if err := registerServiceRecord(dnsClient, dnsZone, "dns", addr); err != nil {
			return fmt.Errorf("failed to register dns DNS record: %w", err)
		}
		registered++
	}

	if stackConfig.SetupState.ZotInstalled {
		addr, err := stackConfig.GetPrimaryZotAddress()
		if err == nil {
			if err := registerServiceRecord(dnsClient, dnsZone, "zot", addr); err != nil {
				return fmt.Errorf("failed to register zot DNS record: %w", err)
			}
			registered++
		}
	}

	if stackConfig.SetupState.K8sInstalled && stackConfig.Cluster.VIP != "" {
		if err := registerServiceRecord(dnsClient, dnsZone, "k8s", stackConfig.Cluster.VIP); err != nil {
			return fmt.Errorf("failed to register k8s DNS record: %w", err)
		}
		registered++
	}

	if registered > 0 {
		fmt.Printf("✓ Registered %d DNS record(s)\n", registered)
	} else {
		fmt.Println("  No existing components to register")
	}

	return nil
}

// registerComponentDNS registers a DNS record for a single component
func registerComponentDNS(ctx context.Context, componentName string, stackConfig *config.Config) error {
	fmt.Printf("\nRegistering DNS record for %s...\n", componentName)

	// Get DNS client
	dnsClient, err := getDNSClient(stackConfig)
	if err != nil {
		return fmt.Errorf("failed to create DNS client: %w", err)
	}

	dnsZone := stackConfig.Cluster.PrimaryDomain

	// Determine IP address for this component
	var ip string
	switch componentName {
	case "openbao":
		ip, err = stackConfig.GetPrimaryOpenBAOAddress()
		if err != nil {
			return fmt.Errorf("no OpenBAO host configured: %w", err)
		}
	case "zot":
		ip, err = stackConfig.GetPrimaryZotAddress()
		if err != nil {
			return fmt.Errorf("no Zot host configured: %w", err)
		}
	case "k8s":
		// The API is reached at the VIP when one is deployed, and at the control
		// plane node's own address otherwise.
		ip, err = stackConfig.APIEndpoint()
		if err != nil {
			return fmt.Errorf("no K8s API endpoint configured: %w", err)
		}
		if ip == "" {
			return fmt.Errorf("no K8s API endpoint configured: no control plane host is defined")
		}
	default:
		return fmt.Errorf("unknown component: %s", componentName)
	}

	if err := registerServiceRecord(dnsClient, dnsZone, componentName, ip); err != nil {
		return fmt.Errorf("failed to register DNS record: %w", err)
	}

	fmt.Printf("✓ DNS record registered: %s.%s -> %s\n", componentName, dnsZone, ip)
	return nil
}

// registerServiceRecord creates an A record for a service
// name should be a short hostname (e.g., "openbao"), not an FQDN
func registerServiceRecord(dnsClient *dns.Client, zone, name, ip string) error {
	fmt.Printf("  Creating A record: %s.%s -> %s\n", name, zone, ip)

	// Use the DNS package's AddARecord helper function
	// Pass short hostname (name) not FQDN to avoid double zone appending
	if err := dns.AddARecord(dnsClient, zone, name, ip); err != nil {
		return fmt.Errorf("failed to add A record: %w", err)
	}

	return nil
}

// getDNSClient creates a DNS client for managing records
func getDNSClient(stackConfig *config.Config) (*dns.Client, error) {
	// Get DNS server address
	dnsHost, err := stackConfig.GetPrimaryDNSAddress()
	if err != nil {
		return nil, fmt.Errorf("no DNS host configured: %w", err)
	}

	// Get DNS API key from config (it's a secret reference)
	// We'll need to resolve it from OpenBAO
	if stackConfig.DNS == nil || stackConfig.DNS.APIKey == "" {
		return nil, fmt.Errorf("DNS API key not configured")
	}

	// Resolve API key from secrets (including OpenBAO)
	resolver, resCtx, err := buildSecretResolver(stackConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to setup secret resolver: %w", err)
	}

	// Parse the API key secret reference
	secretRef, err := secrets.ParseSecretRef(stackConfig.DNS.APIKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse DNS API key secret reference: %w", err)
	}
	if secretRef == nil {
		// Not a secret reference, use as-is
		return dns.NewClient(fmt.Sprintf("http://%s:8081", dnsHost), stackConfig.DNS.APIKey), nil
	}

	// Resolve the API key
	apiKey, err := resolver.Resolve(resCtx, *secretRef)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve DNS API key: %w", err)
	}

	// Create PowerDNS client
	client := dns.NewClient(fmt.Sprintf("http://%s:8081", dnsHost), apiKey)

	return client, nil
}

// buildSecretResolver creates a secret resolver chain that includes OpenBAO if available
func buildSecretResolver(cfg *config.Config) (*secrets.ChainResolver, *secrets.ResolutionContext, error) {
	// Try to get OpenBAO address and token
	var openBAOAddr, openBAOToken string

	if addr, err := cfg.GetPrimaryOpenBAOURL(); err == nil {
		openBAOAddr = addr
	}

	// Try to read OpenBAO token from keys file
	configDir, errConfig := config.GetConfigDir()
	if errConfig == nil {
		keysPath := filepath.Join(configDir, "openbao-keys", cfg.Cluster.Name, "keys.json")
		if keysData, errRead := os.ReadFile(keysPath); errRead == nil {
			var keys struct {
				RootToken string `json:"root_token"`
			}
			if errUnmarshal := json.Unmarshal(keysData, &keys); errUnmarshal == nil {
				openBAOToken = keys.RootToken
			}
		}
	}

	// ResolutionContext with empty instance since we're using foundry-core as the mount
	// The mount is specified in the resolver, not in instance scoping
	resCtx := &secrets.ResolutionContext{
		Instance: "",
	}

	// If we have OpenBAO configured, add it to the resolver chain
	if openBAOAddr != "" && openBAOToken != "" {
		// Use foundry-core mount (where we enabled the KV v2 engine)
		openBAOResolver, err := secrets.NewOpenBAOResolverWithMount(openBAOAddr, openBAOToken, "foundry-core")
		if err != nil {
			// Fall back to env-only if OpenBAO setup fails
			resolver := secrets.NewChainResolver(
				secrets.NewEnvResolver(),
			)
			return resolver, resCtx, nil
		}

		resolver := secrets.NewChainResolver(
			secrets.NewEnvResolver(),
			openBAOResolver,
		)
		return resolver, resCtx, nil
	}

	// OpenBAO not available, use env resolver only
	resolver := secrets.NewChainResolver(
		secrets.NewEnvResolver(),
	)
	return resolver, resCtx, nil
}

// updateSetupState updates the setup_state and component-specific config after successful installation
func updateSetupState(cmd *cli.Command, stackConfig *config.Config, componentName string, componentConfig component.ComponentConfig) error {
	// Map component names to their setup_state fields
	switch componentName {
	case "openbao":
		stackConfig.SetupState.OpenBAOInstalled = true
		stackConfig.SetupState.OpenBAOInitialized = true // Initialized during install
	case "dns", "powerdns":
		stackConfig.SetupState.DNSInstalled = true

		// Initialize DNS config section if needed
		if stackConfig.DNS == nil {
			stackConfig.DNS = &config.DNSConfig{
				Backend:    "gsqlite3",
				Forwarders: []string{"8.8.8.8", "1.1.1.1"},
			}
		}

		// Store API key reference (points to OpenBAO secret)
		// Instance scoping will add "foundry-core/" prefix automatically
		if apiKey, ok := componentConfig["api_key"].(string); ok && apiKey != "" {
			stackConfig.DNS.APIKey = "${secret:dns:api_key}"
		}
	case "zot":
		stackConfig.SetupState.ZotInstalled = true
	case "k3s", "kubernetes":
		stackConfig.SetupState.K8sInstalled = true
	default:
		// Unknown component, don't update state
		return nil
	}

	// Save the updated config
	configPath, err := config.FindConfig(cmd.String("config"))
	if err != nil {
		return fmt.Errorf("failed to find config path: %w", err)
	}
	if err := config.Save(stackConfig, configPath); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("✓ Setup state updated\n")
	return nil
}

// getTargetHostForComponent determines which host a component should be installed on
func getTargetHostForComponent(componentName string, stackConfig *config.Config) (*host.Host, error) {
	// Map component names to their host using role-based discovery
	var targetHost *host.Host
	var err error

	switch componentName {
	case "openbao":
		targetHost, err = stackConfig.GetPrimaryOpenBAOHost()
		if err != nil {
			return nil, fmt.Errorf("no host with openbao role configured: %w", err)
		}
	case "dns", "powerdns":
		targetHost, err = stackConfig.GetPrimaryDNSHost()
		if err != nil {
			return nil, fmt.Errorf("no host with dns role configured: %w", err)
		}
	case "zot":
		targetHost, err = stackConfig.GetPrimaryZotHost()
		if err != nil {
			return nil, fmt.Errorf("no host with zot role configured: %w", err)
		}
	case "k3s", "kubernetes":
		// K3s is installed on nodes defined by cluster roles
		// Use the first control plane node for single-node install/ repair
		cpHosts := stackConfig.GetClusterControlPlaneHosts()
		if len(cpHosts) == 0 {
			return nil, fmt.Errorf("no control plane host configured for k3s")
		}
		targetHost = cpHosts[0]
	default:
		return nil, fmt.Errorf("unknown component %q - cannot determine target host", componentName)
	}

	return targetHost, nil
}

// connectToHost establishes an SSH connection to the given host
func connectToHost(h *host.Host, cfg *config.Config) (*ssh.Connection, error) {
	// Get SSH key from storage (prefers OpenBAO, falls back to filesystem)
	configDir, err := config.GetConfigDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get config directory: %w", err)
	}

	keyStorage, err := ssh.GetKeyStorage(configDir, cfg.Cluster.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to create key storage: %w", err)
	}

	keyPair, err := keyStorage.Load(h.Hostname)
	if err != nil {
		return nil, fmt.Errorf("SSH key not found for host %s: %w\n\nHint: Run 'foundry host sync-keys %s' to reinstall SSH keys", h.Hostname, err, h.Hostname)
	}

	// Create auth method from key pair
	authMethod, err := keyPair.AuthMethod()
	if err != nil {
		return nil, fmt.Errorf("failed to create auth method: %w", err)
	}

	// Connect to host
	connOpts := &ssh.ConnectionOptions{
		Host:       h.Address,
		Port:       h.Port,
		User:       h.User,
		AuthMethod: authMethod,
		Timeout:    30,
	}

	conn, err := ssh.Connect(connOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", h.Hostname, err)
	}

	return conn, nil
}

// ensureDNSAPIKey generates and stores a PowerDNS API key in OpenBAO if it doesn't exist
// Returns the API key (either existing or newly generated)
func ensureDNSAPIKey(stackConfig *config.Config) (string, error) {
	// Get config directory for OpenBAO client
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to get config directory: %w", err)
	}

	// Get OpenBAO address from config
	openBAOAddr, err := stackConfig.GetPrimaryOpenBAOURL()
	if err != nil {
		return "", fmt.Errorf("OpenBAO host not configured: %w", err)
	}

	// Get OpenBAO token from keys file
	keysPath := filepath.Join(configDir, "openbao-keys", stackConfig.Cluster.Name, "keys.json")
	keysData, err := os.ReadFile(keysPath)
	if err != nil {
		return "", fmt.Errorf("failed to read OpenBAO keys: %w", err)
	}

	var keys struct {
		RootToken string `json:"root_token"`
	}
	if err := json.Unmarshal(keysData, &keys); err != nil {
		return "", fmt.Errorf("failed to parse OpenBAO keys: %w", err)
	}

	// Create OpenBAO client directly
	openBAOClient := openbao.NewClient(openBAOAddr, keys.RootToken)

	// Check if API key already exists in OpenBAO at foundry-core/dns
	ctx := context.Background()
	existingData, err := openBAOClient.ReadSecretV2(ctx, "foundry-core", "dns")
	if err == nil && existingData != nil {
		if apiKeyValue, ok := existingData["api_key"].(string); ok {
			fmt.Println("  ℹ Using existing DNS API key from OpenBAO")
			return apiKeyValue, nil
		}
	}

	// Generate new API key
	fmt.Println("  Generating DNS API key...")
	apiKey, err := generateDNSAPIKey()
	if err != nil {
		return "", fmt.Errorf("failed to generate API key: %w", err)
	}

	// Store in OpenBAO at foundry-core/dns
	fmt.Println("  Storing DNS API key in OpenBAO...")
	secretData := map[string]interface{}{
		"api_key": apiKey,
	}

	if err := openBAOClient.WriteSecretV2(ctx, "foundry-core", "dns", secretData); err != nil {
		return "", fmt.Errorf("failed to store API key in OpenBAO: %w", err)
	}

	fmt.Println("  ✓ DNS API key stored in OpenBAO")
	return apiKey, nil
}

// generateDNSAPIKey generates a secure random API key for PowerDNS
func generateDNSAPIKey() (string, error) {
	bytes := make([]byte, 32) // 256 bits
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// isDependencyInstalled checks if a component dependency is installed using setup state
func isDependencyInstalled(dep string, cfg *config.Config) bool {
	// If setup state is nil, assume nothing is installed
	if cfg.SetupState == nil {
		return false
	}

	switch dep {
	case "openbao":
		return cfg.SetupState.OpenBAOInstalled
	case "dns", "powerdns":
		return cfg.SetupState.DNSInstalled
	case "zot":
		return cfg.SetupState.ZotInstalled
	case "k3s", "kubernetes":
		return cfg.SetupState.K8sInstalled
	// K8s-based components - check via Helm release status
	case "storage", "seaweedfs", "prometheus", "loki", "grafana", "external-dns", "velero",
		"gateway-api", "contour", "cert-manager":
		// First check if K3s is installed
		if !cfg.SetupState.K8sInstalled {
			return false
		}
		// Check via Helm releases directly
		return isHelmReleaseInstalled(dep, cfg)
	default:
		return false
	}
}

// isHelmReleaseInstalled checks if a Helm release is installed for a given component
func isHelmReleaseInstalled(componentName string, cfg *config.Config) bool {
	// Get kubeconfig
	configDir, err := config.GetConfigDir()
	if err != nil {
		return false
	}
	kubeconfigPath := filepath.Join(configDir, "kubeconfig")
	kubeconfigBytes, err := os.ReadFile(kubeconfigPath)
	if err != nil {
		return false
	}

	// Create Helm client
	helmClient, err := helm.NewClient(kubeconfigBytes, "default")
	if err != nil {
		return false
	}
	defer helmClient.Close()

	// Map component names to release names and namespaces
	releaseInfo := map[string]struct {
		name      string
		namespace string
	}{
		"storage":      {"local-path-provisioner", "kube-system"},
		"seaweedfs":    {"seaweedfs", "seaweedfs"},
		"prometheus":   {"kube-prometheus-stack", "monitoring"},
		"loki":         {"loki", "loki"},
		"grafana":      {"grafana", "grafana"},
		"external-dns": {"external-dns", "external-dns"},
		"velero":       {"velero", "velero"},
		"gateway-api":  {"gateway-api", "gateway-system"},
		"contour":      {"contour", "projectcontour"},
		"cert-manager": {"cert-manager", "cert-manager"},
	}

	info, ok := releaseInfo[componentName]
	if !ok {
		return false
	}

	// Special case for storage - check if StorageClass exists (K3s bundles local-path)
	if componentName == "storage" {
		// Storage is always available if K3s is running (bundled local-path-provisioner)
		return true
	}

	// Prefer the namespace from the stack config — components are often deployed in a
	// shared namespace (e.g. loki and grafana in "monitoring") rather than the
	// per-component default, and the hardcoded default would look in the wrong place.
	namespace := info.namespace
	if cfg != nil {
		if comp, ok := cfg.Components[componentName]; ok {
			if ns, ok := comp.Config["namespace"].(string); ok && ns != "" {
				namespace = ns
			}
		}
	}

	// Check for Helm release
	releases, err := helmClient.List(context.Background(), namespace)
	if err != nil {
		return false
	}

	for _, rel := range releases {
		if rel.Name == info.name && rel.Status == "deployed" {
			return true
		}
	}

	return false
}

// buildExternalTargetsFromStackConfig creates Prometheus external targets for
// installed infrastructure services (OpenBAO, Zot, PowerDNS) based on stack config
func buildExternalTargetsFromStackConfig(stackConfig *config.Config) []prometheus.ExternalTarget {
	var targets []prometheus.ExternalTarget

	// Check if stack config and setup state are available
	if stackConfig == nil || stackConfig.SetupState == nil {
		return targets
	}

	// OpenBAO metrics at /v1/sys/metrics?format=prometheus
	if stackConfig.SetupState.OpenBAOInstalled {
		if addr, err := stackConfig.GetOpenBAOClientAddr(); err == nil {
			targets = append(targets, prometheus.ExternalTarget{
				Name:        "openbao",
				Targets:     []string{addr},
				MetricsPath: "/v1/sys/metrics",
				Params: map[string][]string{
					"format": {"prometheus"},
				},
			})
		}
	}

	// Zot registry metrics at /metrics on port 5000
	if stackConfig.SetupState.ZotInstalled {
		if addr, err := stackConfig.GetPrimaryZotAddress(); err == nil {
			targets = append(targets, prometheus.ExternalTarget{
				Name:        "zot",
				Targets:     []string{fmt.Sprintf("%s:5000", addr)},
				MetricsPath: "/metrics",
			})
		}
	}

	// PowerDNS metrics (native Prometheus since v4.3.0+/v4.4.0+)
	// Auth server on port 8081, Recursor on port 8082
	if stackConfig.SetupState.DNSInstalled {
		if addr, err := stackConfig.GetPrimaryDNSAddress(); err == nil {
			// PowerDNS Authoritative Server
			targets = append(targets, prometheus.ExternalTarget{
				Name:        "powerdns-auth",
				Targets:     []string{fmt.Sprintf("%s:8081", addr)},
				MetricsPath: "/metrics",
			})
			// PowerDNS Recursor
			targets = append(targets, prometheus.ExternalTarget{
				Name:        "powerdns-recursor",
				Targets:     []string{fmt.Sprintf("%s:8082", addr)},
				MetricsPath: "/metrics",
			})
		}
	}

	return targets
}
