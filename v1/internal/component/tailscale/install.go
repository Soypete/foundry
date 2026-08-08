package tailscale

import (
	"context"
	"fmt"
	"strings"
)

const (
	// DefaultNamespace is the namespace where Tailscale operator will be installed
	DefaultNamespace = "tailscale"

	// DefaultTag is the ACL tag applied to operator-managed devices when the
	// config does not specify any.
	DefaultTag = "tag:k8s-foundry"

	// OAuthSecretName is the Kubernetes secret holding the operator's OAuth
	// client credentials.
	OAuthSecretName = "operator-oauth"
)

// Installer deploys the Tailscale operator and its custom resources.
//
// It owns the ordering of the install steps; the Helm and CRD mechanics live in
// HelmInstaller and CRDInstaller. Each step converges rather than recreating, so
// a second run makes no changes.
type Installer struct {
	helm    *HelmInstaller
	crds    *CRDInstaller
	secrets SecretWriter
	config  *Config
}

// SecretWriter creates or updates the Kubernetes secret carrying the operator's
// OAuth credentials. The credentials are passed this way rather than as Helm
// values so they are not persisted in the Helm release.
type SecretWriter interface {
	EnsureSecret(ctx context.Context, namespace, name string, data map[string]string) error
}

// NewInstaller creates a Tailscale installer.
//
// The API VIP is deliberately not a parameter: nothing in the Tailscale
// installation may reference it. The VIP is internal to the cluster data plane,
// and advertising it on the tailnet is what previously placed cluster traffic
// on Tailscale.
func NewInstaller(helmInstaller *HelmInstaller, crdInstaller *CRDInstaller, secrets SecretWriter, cfg *Config) (*Installer, error) {
	if helmInstaller == nil {
		return nil, fmt.Errorf("helm installer cannot be nil")
	}
	if crdInstaller == nil {
		return nil, fmt.Errorf("crd installer cannot be nil")
	}
	if secrets == nil {
		return nil, fmt.Errorf("secret writer cannot be nil")
	}
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	cfg.SetDefaults()

	return &Installer{
		helm:    helmInstaller,
		crds:    crdInstaller,
		secrets: secrets,
		config:  cfg,
	}, nil
}

// Install deploys the operator and reconciles its custom resources.
//
// Idempotent: the OAuth secret is written with the same content, the Helm
// release is upgraded in place when it already exists (including one installed
// outside Foundry), and the custom resources are applied rather than created.
func (i *Installer) Install(ctx context.Context) error {
	// Step 1: OAuth credentials must exist before the operator starts, or it
	// crash-loops waiting for them.
	data, err := GenerateSecretData(i.config)
	if err != nil {
		return fmt.Errorf("failed to build OAuth secret: %w", err)
	}
	if err := i.secrets.EnsureSecret(ctx, DefaultNamespace, OAuthSecretName, data); err != nil {
		return fmt.Errorf("failed to write OAuth secret: %w", err)
	}

	// Step 2: Chart repository.
	if err := i.helm.AddRepository(ctx); err != nil {
		return fmt.Errorf("failed to add Helm repository: %w", err)
	}

	// Step 3: Operator, adopting any existing release.
	if err := i.helm.InstallOperator(ctx); err != nil {
		return fmt.Errorf("failed to install Tailscale operator: %w", err)
	}

	// Step 4: DNSConfig, so tailnet names resolve inside the cluster.
	if err := i.crds.DeployDNSConfig(ctx); err != nil {
		return fmt.Errorf("failed to deploy DNSConfig: %w", err)
	}

	// Step 5: Connector, only when subnet routes are configured. Never
	// advertises cluster-internal networks.
	if err := i.crds.DeployConnector(ctx); err != nil {
		return fmt.Errorf("failed to deploy Connector: %w", err)
	}

	return nil
}

// Uninstall removes the Tailscale operator Helm release.
//
// The custom resources it owns are removed by the operator's own cleanup; the
// OAuth secret is left in place so a reinstall does not require re-entering
// credentials.
func (i *Installer) Uninstall(ctx context.Context) error {
	if err := i.helm.UninstallOperator(ctx); err != nil {
		return fmt.Errorf("failed to uninstall Tailscale operator: %w", err)
	}
	return nil
}

// Health describes the observable state of the Tailscale integration.
type Health struct {
	// Installed reports whether the operator Helm release is present.
	Installed bool

	// ReleaseStatus is the Helm release status (e.g. "deployed").
	ReleaseStatus string

	// OperatorAddress is the operator's tailnet address, empty when unknown.
	OperatorAddress string

	// AddressState explains an empty OperatorAddress.
	AddressState AddressState

	// Ingresses are the Tailscale-backed ingress endpoints and their state.
	Ingresses []Ingress
}

// Ingress is one Tailscale-exposed service.
type Ingress struct {
	Name     string
	Hostname string
	Ready    bool
}

// Healthy reports whether the operator is deployed and every discovered
// ingress is ready.
func (h Health) Healthy() bool {
	if !h.Installed || !strings.EqualFold(h.ReleaseStatus, "deployed") {
		return false
	}
	for _, ing := range h.Ingresses {
		if !ing.Ready {
			return false
		}
	}
	return true
}

// Summary renders a one-line description of the integration's state.
func (h Health) Summary() string {
	if !h.Installed {
		return "Tailscale operator is not installed"
	}
	ready := 0
	for _, ing := range h.Ingresses {
		if ing.Ready {
			ready++
		}
	}
	return fmt.Sprintf("operator %s at %s; %d/%d ingress ready",
		h.ReleaseStatus, h.AddressDescription(), ready, len(h.Ingresses))
}

// AddressDescription renders the operator's tailnet address, or why there is
// none. The two empty cases mean different things, so they read differently.
func (h Health) AddressDescription() string {
	if h.OperatorAddress != "" {
		return h.OperatorAddress
	}
	switch h.AddressState {
	case AddressServiceMissing:
		return "no operator service found"
	case AddressNotAssigned:
		return "not yet registered on the tailnet"
	default:
		return "unknown"
	}
}
