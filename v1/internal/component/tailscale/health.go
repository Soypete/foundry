package tailscale

import (
	"context"
	"fmt"
	"sort"
)

// IngressLister reports the Tailscale-backed ingresses in the cluster and the
// operator's own tailnet address.
//
// Kept narrow and Tailscale-specific so the health command does not depend on
// the whole Kubernetes client surface.
type IngressLister interface {
	// ListTailscaleIngresses returns every Ingress whose ingressClassName is
	// "tailscale", with the hostname the operator assigned.
	ListTailscaleIngresses(ctx context.Context) ([]Ingress, error)

	// OperatorAddress returns the operator's tailnet address, or "" if the
	// operator has not registered one yet.
	OperatorAddress(ctx context.Context) (string, error)
}

// HealthChecker reports the observable state of the Tailscale integration.
type HealthChecker struct {
	helm    *HelmInstaller
	ingress IngressLister
}

// NewHealthChecker creates a health checker.
func NewHealthChecker(helmInstaller *HelmInstaller, ingress IngressLister) (*HealthChecker, error) {
	if helmInstaller == nil {
		return nil, fmt.Errorf("helm installer cannot be nil")
	}
	if ingress == nil {
		return nil, fmt.Errorf("ingress lister cannot be nil")
	}
	return &HealthChecker{helm: helmInstaller, ingress: ingress}, nil
}

// Check gathers the current state of the Tailscale integration.
//
// When the operator is not installed it returns early: the address and ingress
// lookups would be meaningless, and reporting "not installed" is more useful
// than a list of lookup failures.
func (h *HealthChecker) Check(ctx context.Context) (*Health, error) {
	status, installed, err := h.helm.ReleaseStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read operator release status: %w", err)
	}

	health := &Health{Installed: installed, ReleaseStatus: status}
	if !installed {
		return health, nil
	}

	// A missing address is a reportable state, not a failure: the operator may
	// still be registering with the tailnet.
	addr, err := h.ingress.OperatorAddress(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read operator address: %w", err)
	}
	health.OperatorAddress = addr

	ingresses, err := h.ingress.ListTailscaleIngresses(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list Tailscale ingresses: %w", err)
	}
	sort.Slice(ingresses, func(i, j int) bool { return ingresses[i].Name < ingresses[j].Name })
	health.Ingresses = ingresses

	return health, nil
}
