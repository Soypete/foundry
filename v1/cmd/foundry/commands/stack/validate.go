package stack

import (
	"context"
	"fmt"
	"strings"

	"github.com/catalystcommunity/foundry/v1/internal/component"
	"github.com/catalystcommunity/foundry/v1/internal/component/tailscale"
	"github.com/catalystcommunity/foundry/v1/internal/config"
	"github.com/catalystcommunity/foundry/v1/internal/network"
	"github.com/urfave/cli/v3"
)

// ValidateCommand handles the 'foundry stack validate' command
var ValidateCommand = &cli.Command{
	Name:   "validate",
	Usage:  "Validate stack configuration without installing",
	Action: runStackValidate,
}

func runStackValidate(ctx context.Context, cmd *cli.Command) error {
	// Load configuration (--config flag inherited from root command)
	configPath, err := config.FindConfig(cmd.String("config"))
	if err != nil {
		return fmt.Errorf("failed to find config: %w", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	fmt.Println("Validating stack configuration...")
	fmt.Println()

	// Run all validation checks
	validations := []struct {
		name string
		fn   func(*config.Config) error
	}{
		{"Configuration structure", validateConfigStructure},
		{"Secret references syntax", validateSecretReferences},
		{"Network configuration", validateNetworkConfig},
		{"DNS configuration", validateDNSConfig},
		{"VIP configuration", validateVIPConfig},
		{"Cluster configuration", validateClusterConfig},
		{"Component dependencies", validateComponentDependencies},
		{"Tailscale configuration", validateTailscaleConfig},
	}

	for _, v := range validations {
		if err := v.fn(cfg); err != nil {
			fmt.Printf("✗ %s failed: %v\n", v.name, err)
			return err
		}
		fmt.Printf("✓ %s passed\n", v.name)
	}

	fmt.Println()
	fmt.Println("✓ All validation checks passed")

	// Advisory findings: valid configurations that will not behave as the
	// operator probably intends. These never fail validation.
	if warnings := collectConfigWarnings(cfg); len(warnings) > 0 {
		fmt.Println()
		for _, w := range warnings {
			fmt.Printf("⚠ %s\n", w)
		}
	}

	fmt.Println()
	fmt.Println("Your stack configuration is valid and ready for installation.")
	fmt.Println("Run 'foundry stack install' to deploy the stack.")

	return nil
}

// validateConfigStructure validates the basic config structure
func validateConfigStructure(cfg *config.Config) error {
	// Use the built-in Validate method which checks all struct validations
	if err := cfg.Validate(); err != nil {
		return err
	}
	return nil
}

// validateSecretReferences validates all secret reference syntax
func validateSecretReferences(cfg *config.Config) error {
	if err := config.ValidateSecretRefs(cfg); err != nil {
		return fmt.Errorf("invalid secret reference: %w", err)
	}
	return nil
}

// validateNetworkConfig performs network-specific validations
func validateNetworkConfig(cfg *config.Config) error {
	if cfg.Network == nil {
		return fmt.Errorf("network configuration is required")
	}

	// Validate IP addresses are on the same network
	if err := network.ValidateIPs(cfg); err != nil {
		return err
	}

	// Check for DHCP conflicts
	if err := network.CheckDHCPConflicts(cfg); err != nil {
		return err
	}

	return nil
}

// validateDNSConfig performs DNS-specific validations
func validateDNSConfig(cfg *config.Config) error {
	if cfg.DNS == nil {
		return fmt.Errorf("dns configuration is required")
	}

	// DNS.Validate() is already called by cfg.Validate(), but we can add
	// additional checks here if needed

	// Verify at least one infrastructure zone
	if len(cfg.DNS.InfrastructureZones) == 0 {
		return fmt.Errorf("at least one infrastructure zone is required")
	}

	// Verify at least one kubernetes zone
	if len(cfg.DNS.KubernetesZones) == 0 {
		return fmt.Errorf("at least one kubernetes zone is required")
	}

	// Check that public zones all have the same public_cname (if multiple)
	var publicCNAME string
	allZones := append(cfg.DNS.InfrastructureZones, cfg.DNS.KubernetesZones...)
	for _, zone := range allZones {
		if zone.Public && zone.PublicCNAME != nil {
			if publicCNAME == "" {
				publicCNAME = *zone.PublicCNAME
			} else if *zone.PublicCNAME != publicCNAME {
				return fmt.Errorf("all public zones must have the same public_cname (found %q and %q)",
					publicCNAME, *zone.PublicCNAME)
			}
		}
	}

	return nil
}

// validateVIPConfig validates VIP configuration
func validateVIPConfig(cfg *config.Config) error {
	if cfg.Network == nil {
		return fmt.Errorf("network configuration is required")
	}

	// VIP is already validated in Network.Validate() and validateK8sVIPUniqueness()
	// but we can add additional checks here

	// Ensure VIP is set
	if cfg.Cluster.VIP == "" {
		return fmt.Errorf("cluster.vip is required")
	}

	return nil
}

// validateClusterConfig validates cluster configuration
func validateClusterConfig(cfg *config.Config) error {
	// Cluster.Validate() is already called by cfg.Validate()
	// Additional checks can be added here

	// Ensure at least one host with cluster role is defined
	clusterHosts := cfg.GetClusterHosts()
	if len(clusterHosts) == 0 {
		return fmt.Errorf("at least one host with cluster role (cluster-control-plane or cluster-worker) is required")
	}

	// Verify at least one control-plane host
	controlPlaneHosts := cfg.GetHostsByRole("cluster-control-plane")
	if len(controlPlaneHosts) == 0 {
		return fmt.Errorf("at least one host with cluster-control-plane role is required")
	}

	return nil
}

// validateComponentDependencies validates component dependencies can be resolved
func validateComponentDependencies(cfg *config.Config) error {
	// Names must match what each component reports from Name(), which is how
	// the registry keys them — "cert-manager", not "certmanager".
	componentNames := []string{
		"openbao",
		"dns",
		"zot",
		"k3s",
		"contour",
		"cert-manager",
	}

	order, err := component.ResolveInstallationOrder(component.DefaultRegistry, componentNames)
	if err != nil {
		return fmt.Errorf("dependency resolution failed: %w", err)
	}

	// Verify every requested component made it into the order. The result may
	// legitimately be longer than the request — resolution pulls in transitive
	// dependencies, e.g. contour brings in gateway-api — so compare membership
	// rather than length.
	resolved := make(map[string]bool, len(order))
	for _, name := range order {
		resolved[name] = true
	}
	var missing []string
	for _, name := range componentNames {
		if !resolved[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("dependency resolution incomplete: missing %s",
			strings.Join(missing, ", "))
	}

	return nil
}

// collectConfigWarnings returns advisory messages about configurations that are
// valid but will not behave as intended. Unlike the validation checks, these
// never fail the command.
func collectConfigWarnings(cfg *config.Config) []string {
	var warnings []string
	warnings = append(warnings, warnMissingControlPlaneTailscaleAddress(cfg)...)
	warnings = append(warnings, warnMissingTailscaleCredentials(cfg)...)
	return warnings
}

// warnMissingControlPlaneTailscaleAddress flags control plane hosts with no
// tailscale_address.
//
// Without one, the generated kubeconfig falls back to the API VIP, which is
// internal to the cluster data plane — so remote kubectl works only from the
// LAN. The address is also what gets added to the API certificate SANs, so it
// must be set before provisioning for remote access to work at all.
func warnMissingControlPlaneTailscaleAddress(cfg *config.Config) []string {
	cpHosts := cfg.GetClusterControlPlaneHosts()
	if len(cpHosts) == 0 {
		return nil
	}

	var missing []string
	for _, h := range cpHosts {
		if h.TailscaleAddress == "" {
			missing = append(missing, h.Hostname)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	return []string{fmt.Sprintf(
		"Control plane host(s) %s have no tailscale_address. Remote kubectl will use "+
			"the API VIP %q, which is only reachable from the cluster LAN. Set "+
			"tailscale_address on each control plane host, then run "+
			"'foundry component install k3s' to update the kubeconfig endpoint.",
		strings.Join(missing, ", "), cfg.Cluster.VIP)}
}

// validateTailscaleConfig validates Tailscale configuration if enabled
func validateTailscaleConfig(cfg *config.Config) error {
	// Check if Tailscale is configured
	if cfg.Components == nil {
		return nil
	}

	tsCfg, exists := cfg.Components["tailscale"]
	if !exists {
		return nil
	}

	// Check if enabled (default to true if config exists but no enabled field)
	enabled := true
	if enabledVal, ok := tsCfg.Config["enabled"].(bool); ok {
		enabled = enabledVal
	}

	if !enabled {
		return nil
	}

	// Credentials may legitimately be absent from this file: OpenBAO is the
	// authoritative store, and a config that never carried them literally is
	// valid. Whether they actually resolve is decided at install time, which is
	// the only place with an OpenBAO client. Surface it as advice instead.
	return nil
}

// warnMissingTailscaleCredentials flags Tailscale enabled with no OAuth
// credentials in either the config or a secret reference.
func warnMissingTailscaleCredentials(cfg *config.Config) []string {
	if cfg.Components == nil {
		return nil
	}
	tsCfg, exists := cfg.Components["tailscale"]
	if !exists {
		return nil
	}
	if enabled, ok := tsCfg.Config["enabled"].(bool); ok && !enabled {
		return nil
	}

	clientID, _ := tsCfg.Config["oauth_client_id"].(string)
	clientSecret, _ := tsCfg.Config["oauth_client_secret"].(string)
	if clientID != "" && clientSecret != "" {
		return nil
	}

	return []string{fmt.Sprintf(
		"Tailscale is enabled but stack.yaml carries no OAuth credentials. They must "+
			"already be in OpenBAO at %s/%s, or 'foundry component install tailscale' "+
			"will fail. To create an OAuth client: %s",
		tailscale.SecretMount, tailscale.SecretPath, tailscaleDocsURL)}
}
