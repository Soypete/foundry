package tailscale

import (
	"context"
	"fmt"

	"github.com/catalystcommunity/foundry/v1/internal/component"
)

// Component is the registry entry for the Tailscale operator.
//
// It exists so dependency resolution and `foundry component list` know about
// Tailscale. Installation needs Helm, Kubernetes, and OpenBAO clients, which
// the ComponentConfig map cannot carry, so the install command builds an
// Installer directly rather than calling Install here.
type Component struct{}

// Name returns the component's registry name.
func (c *Component) Name() string { return "tailscale" }

// Dependencies returns the components Tailscale requires.
//
// K3s provides the cluster to install into; OpenBAO holds the OAuth
// credentials.
func (c *Component) Dependencies() []string { return []string{"k3s", "openbao"} }

// errNeedsClients reports that this entry point cannot perform the operation.
func errNeedsClients(op string) error {
	return fmt.Errorf("tailscale %s requires Helm and Kubernetes clients; run 'foundry component install tailscale'", op)
}

// Install is not usable through the registry; see the type comment.
func (c *Component) Install(ctx context.Context, cfg component.ComponentConfig) error {
	return errNeedsClients("install")
}

// Upgrade converges an existing installation, which Install already does.
func (c *Component) Upgrade(ctx context.Context, cfg component.ComponentConfig) error {
	return errNeedsClients("upgrade")
}

// Status is not available without cluster clients; use
// `foundry component tailscale health`.
func (c *Component) Status(ctx context.Context) (*component.ComponentStatus, error) {
	return nil, fmt.Errorf("tailscale status requires cluster access; run 'foundry component tailscale health'")
}

// Uninstall is not usable through the registry; see the type comment.
func (c *Component) Uninstall(ctx context.Context) error {
	return errNeedsClients("uninstall")
}
