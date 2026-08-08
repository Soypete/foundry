package tailscale

import (
	"context"
	"fmt"
)

// KubernetesClient defines the interface for Kubernetes operations needed by Tailscale installer.
// This interface allows for easier testing with mock implementations.
type KubernetesClient interface {
	Apply(ctx context.Context, manifest map[string]interface{}) error
}

// CRDInstaller handles CRD deployment for Tailscale operator.
type CRDInstaller struct {
	client KubernetesClient
	config *Config
}

// NewCRDInstaller creates a new CRD installer for Tailscale.
func NewCRDInstaller(client KubernetesClient, config *Config) (*CRDInstaller, error) {
	if client == nil {
		return nil, fmt.Errorf("kubernetes client cannot be nil")
	}
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}

	return &CRDInstaller{
		client: client,
		config: config,
	}, nil
}

// DeployConnector deploys a Tailscale Connector advertising the subnet routes
// from the component config.
//
// No-op when no routes are configured: a Connector exists to advertise routes,
// and the cluster's own networks must not be among them. Pod, Service, and API
// VIP addresses stay internal to the cluster — external access is provided by
// per-service proxies and the API server proxy, not by putting cluster
// networks on the tailnet.
func (c *CRDInstaller) DeployConnector(ctx context.Context) error {
	if len(c.config.AdvertiseRoutes) == 0 {
		return nil
	}

	connector, err := c.generateConnectorManifest()
	if err != nil {
		return fmt.Errorf("failed to generate Connector manifest: %w", err)
	}

	if err := c.client.Apply(ctx, connector); err != nil {
		return fmt.Errorf("failed to apply Connector CRD: %w", err)
	}

	return nil
}

// DeployDNSConfig deploys the Tailscale DNSConfig CRD for Magic DNS.
func (c *CRDInstaller) DeployDNSConfig(ctx context.Context) error {
	dnsConfig, err := c.generateDNSConfigManifest()
	if err != nil {
		return fmt.Errorf("failed to generate DNSConfig manifest: %w", err)
	}

	if err := c.client.Apply(ctx, dnsConfig); err != nil {
		return fmt.Errorf("failed to apply DNSConfig CRD: %w", err)
	}

	return nil
}

// generateConnectorManifest creates the Connector CRD manifest.
//
// Only the routes explicitly configured in advertise_routes are advertised.
// The API VIP is deliberately never included: it is internal to the cluster
// data plane, and advertising it as a subnet route is what previously placed
// cluster traffic on the tailnet.
func (c *CRDInstaller) generateConnectorManifest() (map[string]interface{}, error) {
	if len(c.config.AdvertiseRoutes) == 0 {
		return nil, fmt.Errorf("no advertise_routes configured")
	}
	routes := append([]string(nil), c.config.AdvertiseRoutes...)

	tags := c.config.Tags
	if len(tags) == 0 {
		tags = []string{DefaultTag}
	}

	connector := map[string]interface{}{
		"apiVersion": "tailscale.com/v1alpha1",
		"kind":       "Connector",
		"metadata": map[string]interface{}{
			"name":      "foundry-subnet-router",
			"namespace": DefaultNamespace,
		},
		"spec": map[string]interface{}{
			"tags": tags,
			"subnetRouter": map[string]interface{}{
				"advertiseRoutes": routes,
			},
		},
	}

	return connector, nil
}

// generateDNSConfigManifest creates the DNSConfig CRD manifest.
func (c *CRDInstaller) generateDNSConfigManifest() (map[string]interface{}, error) {
	dnsConfig := map[string]interface{}{
		"apiVersion": "tailscale.com/v1alpha1",
		"kind":       "DNSConfig",
		"metadata": map[string]interface{}{
			"name":      "ts-dns",
			"namespace": DefaultNamespace,
		},
		"spec": map[string]interface{}{
			"nameserver": map[string]interface{}{
				"image": map[string]interface{}{
					"repo": "tailscale/k8s-nameserver",
					"tag":  "unstable",
				},
			},
		},
	}

	return dnsConfig, nil
}
