package tailscale

import (
	"context"
	"fmt"

	"github.com/catalystcommunity/foundry/v1/internal/component"
)

type Component struct {
	helmClient HelmClient
}

func NewComponent(helmClient HelmClient) *Component {
	return &Component{
		helmClient: helmClient,
	}
}

func (c *Component) Name() string {
	return "tailscale"
}

func (c *Component) Install(ctx context.Context, cfg component.ComponentConfig) error {
	oauthClientID, _ := cfg["oauth_client_id"].(string)
	oauthClientSecret, _ := cfg["oauth_client_secret"].(string)
	operatorImage, _ := cfg["operator_image"].(string)

	tailscaleCfg := &Config{}
	if oauthClientID != "" {
		tailscaleCfg.OAuthClientID = &oauthClientID
	}
	if oauthClientSecret != "" {
		tailscaleCfg.OAuthClientSecret = &oauthClientSecret
	}
	if operatorImage != "" {
		tailscaleCfg.OperatorImage = &operatorImage
	}

	tailscaleCfg.SetDefaults()

	helmInstaller, err := NewHelmInstaller(c.helmClient, tailscaleCfg)
	if err != nil {
		return fmt.Errorf("failed to create helm installer: %w", err)
	}

	if err := helmInstaller.AddRepository(ctx); err != nil {
		return fmt.Errorf("failed to add helm repository: %w", err)
	}

	if err := helmInstaller.InstallOperator(ctx); err != nil {
		return fmt.Errorf("failed to install operator: %w", err)
	}

	return nil
}

func (c *Component) Upgrade(ctx context.Context, cfg component.ComponentConfig) error {
	return fmt.Errorf("upgrade not yet implemented")
}

func (c *Component) Status(ctx context.Context) (*component.ComponentStatus, error) {
	if c.helmClient == nil {
		return &component.ComponentStatus{
			Installed: false,
			Healthy:   false,
			Message:   "helm client not initialized",
		}, nil
	}

	releases, err := c.helmClient.List(ctx, DefaultNamespace)
	if err != nil {
		return &component.ComponentStatus{
			Installed: false,
			Healthy:   false,
			Message:   fmt.Sprintf("failed to list releases: %v", err),
		}, nil
	}

	for _, rel := range releases {
		if rel.Name == OperatorReleaseName {
			healthy := rel.Status == "deployed"
			return &component.ComponentStatus{
				Installed: true,
				Version:   rel.AppVersion,
				Healthy:   healthy,
				Message:   fmt.Sprintf("operator status: %s", rel.Status),
			}, nil
		}
	}

	return &component.ComponentStatus{
		Installed: false,
		Healthy:   false,
		Message:   "tailscale-operator release not found",
	}, nil
}

func (c *Component) Uninstall(ctx context.Context) error {
	if c.helmClient == nil {
		return fmt.Errorf("helm client not initialized")
	}

	helmInstaller, err := NewHelmInstaller(c.helmClient, &Config{})
	if err != nil {
		return fmt.Errorf("failed to create helm installer: %w", err)
	}

	return helmInstaller.UninstallOperator(ctx)
}

func (c *Component) Dependencies() []string {
	return []string{"k3s"}
}
