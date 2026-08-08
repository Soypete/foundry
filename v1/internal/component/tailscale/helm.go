package tailscale

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/catalystcommunity/foundry/v1/internal/helm"
)

// HelmClient defines the interface for Helm operations needed by Tailscale installer.
// This interface allows for easier testing with mock implementations.
type HelmClient interface {
	AddRepo(ctx context.Context, opts helm.RepoAddOptions) error
	Install(ctx context.Context, opts helm.InstallOptions) error
	Upgrade(ctx context.Context, opts helm.UpgradeOptions) error
	Uninstall(ctx context.Context, opts helm.UninstallOptions) error
	List(ctx context.Context, namespace string) ([]helm.Release, error)
}

const (
	// Tailscale Helm repository constants
	TailscaleRepoName = "tailscale"
	TailscaleRepoURL  = "https://pkgs.tailscale.com/helmcharts"

	// Operator chart constants
	OperatorChartName   = "tailscale-operator"
	OperatorReleaseName = "tailscale-operator"

	// Installation timeouts
	DefaultInstallTimeout = 5 * time.Minute
)

// HelmInstaller handles Helm operations for Tailscale operator.
// This is separated from the main Installer to allow for easier testing.
type HelmInstaller struct {
	client HelmClient
	config *Config
}

// NewHelmInstaller creates a new Helm installer for Tailscale.
func NewHelmInstaller(client HelmClient, config *Config) (*HelmInstaller, error) {
	if client == nil {
		return nil, fmt.Errorf("helm client cannot be nil")
	}
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}

	return &HelmInstaller{
		client: client,
		config: config,
	}, nil
}

// AddRepository adds the Tailscale Helm repository.
func (h *HelmInstaller) AddRepository(ctx context.Context) error {
	opts := helm.RepoAddOptions{
		Name:        TailscaleRepoName,
		URL:         TailscaleRepoURL,
		ForceUpdate: true, // Update if already exists
	}

	if err := h.client.AddRepo(ctx, opts); err != nil {
		return fmt.Errorf("failed to add Tailscale repository: %w", err)
	}

	return nil
}

// InstallOperator installs or upgrades the Tailscale operator Helm chart.
//
// An existing release is upgraded in place rather than treated as an error, so
// the command converges onto an operator that was installed outside Foundry and
// a second run is a no-op.
func (h *HelmInstaller) InstallOperator(ctx context.Context) error {
	// Generate Helm values from config
	values, err := h.generateHelmValues()
	if err != nil {
		return fmt.Errorf("failed to generate Helm values: %w", err)
	}

	chart := fmt.Sprintf("%s/%s", TailscaleRepoName, OperatorChartName)

	if h.releaseExists(ctx) {
		return h.upgradeOperator(ctx, chart, values)
	}

	opts := helm.InstallOptions{
		ReleaseName:     OperatorReleaseName,
		Namespace:       DefaultNamespace,
		Chart:           chart,
		Values:          values,
		CreateNamespace: true,
		Wait:            true,
		Timeout:         DefaultInstallTimeout,
	}

	if err := h.client.Install(ctx, opts); err != nil {
		// List can fail transiently or lack permission, so a release may exist
		// even when releaseExists said otherwise. Helm tells us definitively.
		if strings.Contains(err.Error(), "cannot re-use a name") {
			return h.upgradeOperator(ctx, chart, values)
		}
		return fmt.Errorf("failed to install Tailscale operator: %w", err)
	}

	return nil
}

// upgradeOperator upgrades the existing operator release in place.
func (h *HelmInstaller) upgradeOperator(ctx context.Context, chart string, values map[string]interface{}) error {
	opts := helm.UpgradeOptions{
		ReleaseName: OperatorReleaseName,
		Namespace:   DefaultNamespace,
		Chart:       chart,
		Values:      values,
		Wait:        true,
		Timeout:     DefaultInstallTimeout,
	}

	if err := h.client.Upgrade(ctx, opts); err != nil {
		return fmt.Errorf("failed to upgrade Tailscale operator: %w", err)
	}

	return nil
}

// releaseExists reports whether the operator release is already present.
// A failed List is reported as "not present"; InstallOperator recovers from a
// wrong answer via Helm's own name-in-use error.
func (h *HelmInstaller) releaseExists(ctx context.Context) bool {
	releases, err := h.client.List(ctx, DefaultNamespace)
	if err != nil {
		return false
	}
	for _, rel := range releases {
		if rel.Name == OperatorReleaseName {
			return true
		}
	}
	return false
}

// ReleaseStatus returns the operator release's Helm status and whether it
// exists. Used by the health command.
func (h *HelmInstaller) ReleaseStatus(ctx context.Context) (string, bool, error) {
	releases, err := h.client.List(ctx, DefaultNamespace)
	if err != nil {
		return "", false, fmt.Errorf("failed to list Helm releases: %w", err)
	}
	for _, rel := range releases {
		if rel.Name == OperatorReleaseName {
			return rel.Status, true, nil
		}
	}
	return "", false, nil
}

// UninstallOperator uninstalls the Tailscale operator Helm chart.
func (h *HelmInstaller) UninstallOperator(ctx context.Context) error {
	opts := helm.UninstallOptions{
		ReleaseName: OperatorReleaseName,
		Namespace:   DefaultNamespace,
		Wait:        true,
		Timeout:     DefaultInstallTimeout,
	}

	if err := h.client.Uninstall(ctx, opts); err != nil {
		return fmt.Errorf("failed to uninstall Tailscale operator: %w", err)
	}

	return nil
}

// generateHelmValues creates the Helm values map from Tailscale config.
//
// OAuth credentials are deliberately absent. Passing them as Helm values would
// persist them in cleartext in the release secret and in `helm get values`
// output; instead they are written to the operator-oauth Kubernetes secret,
// which the chart reads via oauthSecretVolume.
func (h *HelmInstaller) generateHelmValues() (map[string]interface{}, error) {
	if h.config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}

	values := make(map[string]interface{})

	// Operator image (optional). Split on the last colon so a registry with a
	// port (registry:5000/tailscale/operator) is not mistaken for a tag.
	if h.config.OperatorImage != nil && *h.config.OperatorImage != "" {
		repository, tag := splitImageRef(*h.config.OperatorImage)
		image := map[string]interface{}{"repository": repository}
		if tag != "" {
			image["tag"] = tag
		}
		values["operatorConfig"] = map[string]interface{}{"image": image}
	}

	if len(h.config.Tags) > 0 {
		values["operatorConfig"] = mergeInto(values["operatorConfig"], map[string]interface{}{
			"defaultTags": append([]string(nil), h.config.Tags...),
		})
	}

	return values, nil
}

// splitImageRef splits an image reference into repository and tag. A colon that
// belongs to a registry port (no slash after it) is not treated as a tag
// separator.
func splitImageRef(image string) (repository, tag string) {
	idx := strings.LastIndex(image, ":")
	if idx == -1 || strings.Contains(image[idx+1:], "/") {
		return image, ""
	}
	return image[:idx], image[idx+1:]
}

// mergeInto merges src into an existing map value, tolerating a nil or
// non-map existing value.
func mergeInto(existing interface{}, src map[string]interface{}) map[string]interface{} {
	dst, ok := existing.(map[string]interface{})
	if !ok || dst == nil {
		dst = make(map[string]interface{}, len(src))
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// GenerateSecretData creates the secret data structure for OAuth credentials.
// This is used when creating a Kubernetes secret directly (alternative to Helm).
func GenerateSecretData(config *Config) (map[string]string, error) {
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if config.OAuthClientID == nil || *config.OAuthClientID == "" {
		return nil, fmt.Errorf("OAuth client ID is required")
	}
	if config.OAuthClientSecret == nil || *config.OAuthClientSecret == "" {
		return nil, fmt.Errorf("OAuth client secret is required")
	}

	return map[string]string{
		"client_id":     *config.OAuthClientID,
		"client_secret": *config.OAuthClientSecret,
	}, nil
}
