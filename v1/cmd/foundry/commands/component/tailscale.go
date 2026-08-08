package component

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/catalystcommunity/foundry/v1/internal/component/tailscale"
	"github.com/catalystcommunity/foundry/v1/internal/config"
	"github.com/catalystcommunity/foundry/v1/internal/helm"
	"github.com/catalystcommunity/foundry/v1/internal/k8s"
	"github.com/urfave/cli/v3"
)

// TailscaleCommand groups the Tailscale-specific subcommands.
var TailscaleCommand = &cli.Command{
	Name:  "tailscale",
	Usage: "Inspect the Tailscale integration",
	Commands: []*cli.Command{
		tailscaleHealthCommand,
	},
}

var tailscaleHealthCommand = &cli.Command{
	Name:  "health",
	Usage: "Report the Tailscale operator address and available ingress",
	Description: `Reports whether the Tailscale operator is installed, the address it is
reachable at on the tailnet, and every Tailscale-backed ingress with its
readiness.

Exits non-zero when the operator is not deployed or any ingress is not
serving, so it can be used as a check.`,
	Action: runTailscaleHealth,
}

func runTailscaleHealth(ctx context.Context, cmd *cli.Command) error {
	configPath, err := config.FindConfig(cmd.String("config"))
	if err != nil {
		return fmt.Errorf("failed to find config: %w", err)
	}
	stackConfig, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load stack config: %w", err)
	}

	configDir, err := config.GetConfigDir()
	if err != nil {
		return fmt.Errorf("failed to get config directory: %w", err)
	}
	kubeconfigPath := filepath.Join(configDir, "kubeconfig")
	kubeconfigBytes, err := os.ReadFile(kubeconfigPath)
	if err != nil {
		return fmt.Errorf("kubeconfig not found at %s: %w\n\nHint: install K3s first", kubeconfigPath, err)
	}

	helmClient, err := helm.NewClient(kubeconfigBytes, tailscale.DefaultNamespace)
	if err != nil {
		return fmt.Errorf("failed to create helm client: %w", err)
	}
	defer helmClient.Close()

	k8sClient, err := k8s.NewClientFromKubeconfig(kubeconfigBytes)
	if err != nil {
		return fmt.Errorf("failed to create k8s client: %w", err)
	}

	health, err := checkTailscaleHealth(ctx, stackConfig, helmClient, k8sClient)
	if err != nil {
		return err
	}

	printTailscaleHealth(health)

	if !health.Healthy() {
		return fmt.Errorf("Tailscale integration is not healthy")
	}
	return nil
}

// checkTailscaleHealth gathers Tailscale health using the given clients.
func checkTailscaleHealth(ctx context.Context, stackConfig *config.Config, helmClient tailscale.HelmClient, k8sClient *k8s.Client) (*tailscale.Health, error) {
	// Health only reads state, so a placeholder config is enough — the real
	// OAuth credentials are not needed to ask what is deployed.
	cfg := &tailscale.Config{}
	helmInstaller, err := tailscale.NewHelmInstaller(helmClient, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create Helm installer: %w", err)
	}

	adapter, err := tailscale.NewKubeAdapter(k8sClient.Clientset(), k8sClient.DynamicClient())
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes adapter: %w", err)
	}

	checker, err := tailscale.NewHealthChecker(helmInstaller, adapter)
	if err != nil {
		return nil, fmt.Errorf("failed to create health checker: %w", err)
	}

	health, err := checker.Check(ctx)
	if err != nil {
		return nil, fmt.Errorf("health check failed: %w", err)
	}
	return health, nil
}

// printTailscaleHealth renders a health report.
func printTailscaleHealth(health *tailscale.Health) {
	fmt.Println("Tailscale integration")
	fmt.Println()

	if !health.Installed {
		fmt.Println("  Operator:  not installed")
		fmt.Println()
		fmt.Println("Run 'foundry component install tailscale' to deploy it.")
		return
	}

	fmt.Printf("  Operator:  %s\n", health.ReleaseStatus)
	fmt.Printf("  Address:   %s\n", health.AddressDescription())

	// An empty address has two very different causes; say what to do about each.
	switch health.AddressState {
	case tailscale.AddressServiceMissing:
		fmt.Printf("             (no operator Deployment or Service in namespace %q)\n",
			tailscale.DefaultNamespace)
	case tailscale.AddressNotAssigned:
		fmt.Println("             (the operator is deployed but its tailnet hostname could not")
		fmt.Println("              be read - check `tailscale status` for its device identity)")
	}

	fmt.Println()
	if len(health.Ingresses) == 0 {
		fmt.Println("  Ingress:   none exposed over Tailscale")
		return
	}

	fmt.Println("  Ingress:")
	for _, ing := range health.Ingresses {
		mark := "✗"
		hostname := ing.Hostname
		if ing.Ready {
			mark = "✓"
		}
		if hostname == "" {
			hostname = "no address assigned"
		}
		fmt.Printf("    %s %-40s %s\n", mark, ing.Name, hostname)
	}
}
