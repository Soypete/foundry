package stack

import (
	"strings"
	"testing"

	"github.com/catalystcommunity/foundry/v1/cmd/foundry/registry"
	"github.com/catalystcommunity/foundry/v1/internal/component"
	"github.com/catalystcommunity/foundry/v1/internal/component/tailscale"
	"github.com/catalystcommunity/foundry/v1/internal/config"
	"github.com/catalystcommunity/foundry/v1/internal/host"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Synthetic addresses: a LAN underlay, CGNAT/Tailscale management addresses,
// and an API VIP on a third network.
const (
	warnVIP        = "10.0.0.11"
	warnTailscale1 = "100.81.89.62"
	warnTailscale2 = "100.125.196.1"
)

// controlPlaneHost builds a synthetic control plane host.
func controlPlaneHost(hostname, lanAddress, tailscaleAddress string) *host.Host {
	return &host.Host{
		Hostname:         hostname,
		Address:          lanAddress,
		NodeIP:           lanAddress,
		TailscaleAddress: tailscaleAddress,
		Port:             22,
		User:             "root",
		Roles:            []string{host.RoleClusterControlPlane},
	}
}

// workerHost builds a synthetic worker host.
func workerHost(hostname, lanAddress, tailscaleAddress string) *host.Host {
	return &host.Host{
		Hostname:         hostname,
		Address:          lanAddress,
		NodeIP:           lanAddress,
		TailscaleAddress: tailscaleAddress,
		Port:             22,
		User:             "root",
		Roles:            []string{host.RoleClusterWorker},
	}
}

func TestWarnMissingControlPlaneTailscaleAddress(t *testing.T) {
	tests := []struct {
		name        string
		cfg         *config.Config
		wantWarning bool
		wantContain []string
	}{
		{
			name: "control plane without tailscale address warns",
			cfg: &config.Config{
				Cluster: config.ClusterConfig{VIP: warnVIP},
				Hosts:   []*host.Host{controlPlaneHost("blue1", "192.168.1.185", "")},
			},
			wantWarning: true,
			// The warning must name the host, name the VIP it will fall back
			// to, and state the command that repairs it.
			wantContain: []string{"blue1", warnVIP, "tailscale_address", "foundry component install k3s"},
		},
		{
			name: "control plane with tailscale address does not warn",
			cfg: &config.Config{
				Cluster: config.ClusterConfig{VIP: warnVIP},
				Hosts:   []*host.Host{controlPlaneHost("blue1", "192.168.1.185", warnTailscale1)},
			},
			wantWarning: false,
		},
		{
			name: "only the control plane missing an address is named",
			cfg: &config.Config{
				Cluster: config.ClusterConfig{VIP: warnVIP},
				Hosts: []*host.Host{
					controlPlaneHost("blue1", "192.168.1.185", warnTailscale1),
					controlPlaneHost("blue2", "192.168.1.97", ""),
				},
			},
			wantWarning: true,
			wantContain: []string{"blue2"},
		},
		{
			name: "every control plane missing an address is named",
			cfg: &config.Config{
				Cluster: config.ClusterConfig{VIP: warnVIP},
				Hosts: []*host.Host{
					controlPlaneHost("blue1", "192.168.1.185", ""),
					controlPlaneHost("blue2", "192.168.1.97", ""),
				},
			},
			wantWarning: true,
			wantContain: []string{"blue1", "blue2"},
		},
		{
			// Workers are reached over SSH, not via the kubeconfig endpoint,
			// so a missing address there is not this warning's concern.
			name: "worker without tailscale address does not warn",
			cfg: &config.Config{
				Cluster: config.ClusterConfig{VIP: warnVIP},
				Hosts: []*host.Host{
					controlPlaneHost("blue1", "192.168.1.185", warnTailscale1),
					workerHost("refurb", "192.168.1.253", ""),
				},
			},
			wantWarning: false,
		},
		{
			name: "no cluster hosts does not warn",
			cfg: &config.Config{
				Cluster: config.ClusterConfig{VIP: warnVIP},
				Hosts:   []*host.Host{},
			},
			wantWarning: false,
		},
		{
			name: "non-cluster hosts only does not warn",
			cfg: &config.Config{
				Cluster: config.ClusterConfig{VIP: warnVIP},
				Hosts: []*host.Host{
					{Hostname: "refurb", Address: "192.168.1.253", Port: 22, User: "root", Roles: []string{host.RoleOpenBAO}},
				},
			},
			wantWarning: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings := warnMissingControlPlaneTailscaleAddress(tt.cfg)

			if !tt.wantWarning {
				assert.Empty(t, warnings)
				return
			}

			require.Len(t, warnings, 1)
			for _, want := range tt.wantContain {
				assert.Contains(t, warnings[0], want)
			}
		})
	}
}

// TestWarnMissingControlPlaneTailscaleAddressExcludesHealthyHosts guards
// against the warning naming a host that is correctly configured.
func TestWarnMissingControlPlaneTailscaleAddressExcludesHealthyHosts(t *testing.T) {
	cfg := &config.Config{
		Cluster: config.ClusterConfig{VIP: warnVIP},
		Hosts: []*host.Host{
			controlPlaneHost("blue1", "192.168.1.185", warnTailscale1),
			controlPlaneHost("blue2", "192.168.1.97", warnTailscale2),
			controlPlaneHost("refurb", "192.168.1.253", ""),
		},
	}

	warnings := warnMissingControlPlaneTailscaleAddress(cfg)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "refurb")
	assert.NotContains(t, warnings[0], "blue1")
	assert.NotContains(t, warnings[0], "blue2")
}

func TestCollectConfigWarnings(t *testing.T) {
	t.Run("surfaces the missing tailscale address warning", func(t *testing.T) {
		cfg := &config.Config{
			Cluster: config.ClusterConfig{VIP: warnVIP},
			Hosts:   []*host.Host{controlPlaneHost("blue1", "192.168.1.185", "")},
		}
		warnings := collectConfigWarnings(cfg)
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "blue1")
	})

	t.Run("returns nothing for a fully configured stack", func(t *testing.T) {
		cfg := &config.Config{
			Cluster: config.ClusterConfig{VIP: warnVIP},
			Hosts:   []*host.Host{controlPlaneHost("blue1", "192.168.1.185", warnTailscale1)},
		}
		assert.Empty(t, collectConfigWarnings(cfg))
	})
}

// TestWarningsAreAdvisoryNotFatal documents the contract that these findings
// never fail validation: they are returned as strings, so there is no error
// path for a caller to treat as fatal.
func TestWarningsAreAdvisoryNotFatal(t *testing.T) {
	cfg := &config.Config{
		Cluster: config.ClusterConfig{VIP: warnVIP},
		Hosts:   []*host.Host{controlPlaneHost("blue1", "192.168.1.185", "")},
	}

	warnings := collectConfigWarnings(cfg)
	require.NotEmpty(t, warnings)

	// The same config must still pass the Tailscale validation check, which is
	// concerned with operator OAuth credentials rather than host addressing.
	assert.NoError(t, validateTailscaleConfig(cfg))

	for _, w := range warnings {
		assert.False(t, strings.HasPrefix(w, "✗"), "advisory text must not read as a failure")
	}
}

// TestValidateComponentDependenciesAgainstRealRegistry guards the component
// names in validateComponentDependencies against the real registry rather than
// mocks.
//
// The hardcoded list previously said "certmanager" while the component
// registers itself as "cert-manager", so `foundry stack validate` failed on
// every config. The existing test missed it because it registered a mock under
// the misspelled name -- it validated its own fixture, not reality.
func TestValidateComponentDependenciesAgainstRealRegistry(t *testing.T) {
	original := component.DefaultRegistry
	t.Cleanup(func() { component.DefaultRegistry = original })

	component.DefaultRegistry = component.NewRegistry()
	require.NoError(t, registry.InitComponents(), "production registry must initialize")

	err := validateComponentDependencies(&config.Config{
		Components: config.ComponentMap{"openbao": {}},
	})
	assert.NoError(t, err, "every name in validateComponentDependencies must exist in the real registry")
}

// TestWarnMissingTailscaleCredentials covers the advisory for Tailscale enabled
// without credentials in stack.yaml.
//
// This must not be a validation failure: OpenBAO is the authoritative store, so
// a config that never carried credentials literally is valid. Whether they
// resolve is decided at install time, which is the only place with a client.
func TestWarnMissingTailscaleCredentials(t *testing.T) {
	tsComponent := func(cfg map[string]interface{}) *config.Config {
		return &config.Config{
			Cluster:    config.ClusterConfig{VIP: warnVIP},
			Components: config.ComponentMap{"tailscale": {Config: cfg}},
		}
	}

	tests := []struct {
		name        string
		cfg         *config.Config
		wantWarning bool
	}{
		{
			name:        "enabled with no credentials warns",
			cfg:         tsComponent(map[string]interface{}{}),
			wantWarning: true,
		},
		{
			name: "secret references count as present",
			cfg: tsComponent(map[string]interface{}{
				"oauth_client_id":     "${secret:tailscale:client_id}",
				"oauth_client_secret": "${secret:tailscale:client_secret}",
			}),
			wantWarning: false,
		},
		{
			name: "literal credentials count as present",
			cfg: tsComponent(map[string]interface{}{
				"oauth_client_id":     "literal-id",
				"oauth_client_secret": "literal-secret",
			}),
			wantWarning: false,
		},
		{
			name: "only one credential still warns",
			cfg: tsComponent(map[string]interface{}{
				"oauth_client_id": "${secret:tailscale:client_id}",
			}),
			wantWarning: true,
		},
		{
			name:        "explicitly disabled does not warn",
			cfg:         tsComponent(map[string]interface{}{"enabled": false}),
			wantWarning: false,
		},
		{
			name:        "component absent does not warn",
			cfg:         &config.Config{Cluster: config.ClusterConfig{VIP: warnVIP}},
			wantWarning: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings := warnMissingTailscaleCredentials(tt.cfg)
			if !tt.wantWarning {
				assert.Empty(t, warnings)
				return
			}
			require.Len(t, warnings, 1)
			assert.Contains(t, warnings[0], "OpenBAO")
			assert.Contains(t, warnings[0], tailscale.DocsURL)
		})
	}
}

// TestValidateTailscaleConfigNeverFailsOnMissingCredentials guards the change
// from a hard failure to an advisory: a config whose credentials live only in
// OpenBAO must still validate.
func TestValidateTailscaleConfigNeverFailsOnMissingCredentials(t *testing.T) {
	cfg := &config.Config{
		Components: config.ComponentMap{"tailscale": {Config: map[string]interface{}{}}},
	}
	assert.NoError(t, validateTailscaleConfig(cfg))
}
