package tailscale

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockKubernetesClient is a mock implementation of KubernetesClient for testing.
type mockKubernetesClient struct {
	applyErr         error
	manifestsApplied []map[string]interface{}
}

func (m *mockKubernetesClient) Apply(ctx context.Context, manifest map[string]interface{}) error {
	if m.applyErr != nil {
		return m.applyErr
	}
	m.manifestsApplied = append(m.manifestsApplied, manifest)
	return nil
}

// Synthetic cluster-internal addresses that must never reach the tailnet.
const (
	testAPIVIP     = "10.0.0.11"
	testPodCIDR    = "10.42.0.0/16"
	testSvcCIDR    = "10.43.0.0/16"
	testLANRoute   = "192.168.1.0/24"
	testExtraRoute = "172.16.0.0/16"
)

func TestNewCRDInstaller(t *testing.T) {
	tests := []struct {
		name    string
		client  KubernetesClient
		config  *Config
		wantErr string
	}{
		{
			name:   "valid",
			client: &mockKubernetesClient{},
			config: &Config{},
		},
		{
			name:    "nil client",
			client:  nil,
			config:  &Config{},
			wantErr: "kubernetes client cannot be nil",
		},
		{
			name:    "nil config",
			client:  &mockKubernetesClient{},
			config:  nil,
			wantErr: "config cannot be nil",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installer, err := NewCRDInstaller(tt.client, tt.config)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, installer)
		})
	}
}

// TestDeployConnectorNeverAdvertisesClusterInternalNetworks is the guard for the
// maintainer's requirement that the API VIP stay internal to the cluster.
//
// The Connector previously advertised the VIP as a /32 subnet route, which put
// cluster traffic on the tailnet. Nothing derived from the cluster's own
// networks may appear in advertiseRoutes — only routes the operator explicitly
// configured.
func TestDeployConnectorNeverAdvertisesClusterInternalNetworks(t *testing.T) {
	client := &mockKubernetesClient{}
	installer, err := NewCRDInstaller(client, &Config{
		AdvertiseRoutes: []string{testExtraRoute},
	})
	require.NoError(t, err)
	require.NoError(t, installer.DeployConnector(context.Background()))

	require.Len(t, client.manifestsApplied, 1)
	routes := connectorRoutes(t, client.manifestsApplied[0])

	assert.Equal(t, []string{testExtraRoute}, routes)

	// Assert on the rendered manifest as a whole so no field reintroduces the
	// VIP by another route.
	rendered := fmt.Sprintf("%v", client.manifestsApplied[0])
	for _, internal := range []string{testAPIVIP, testPodCIDR, testSvcCIDR} {
		assert.NotContains(t, rendered, internal,
			"cluster-internal network %s must never appear in the Connector", internal)
	}
}

// TestDeployConnectorSkippedWithoutRoutes covers the default posture: with no
// advertise_routes configured, no Connector is created at all. A Connector
// exists only to advertise routes, so an empty one would be meaningless.
func TestDeployConnectorSkippedWithoutRoutes(t *testing.T) {
	for _, tt := range []struct {
		name   string
		routes []string
	}{
		{"nil routes", nil},
		{"empty routes", []string{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &mockKubernetesClient{}
			installer, err := NewCRDInstaller(client, &Config{AdvertiseRoutes: tt.routes})
			require.NoError(t, err)

			require.NoError(t, installer.DeployConnector(context.Background()))
			assert.Empty(t, client.manifestsApplied, "no Connector should be applied")
		})
	}
}

func TestDeployConnector(t *testing.T) {
	t.Run("advertises exactly the configured routes", func(t *testing.T) {
		client := &mockKubernetesClient{}
		installer, err := NewCRDInstaller(client, &Config{
			AdvertiseRoutes: []string{testLANRoute, testExtraRoute},
		})
		require.NoError(t, err)
		require.NoError(t, installer.DeployConnector(context.Background()))

		require.Len(t, client.manifestsApplied, 1)
		assert.Equal(t, []string{testLANRoute, testExtraRoute},
			connectorRoutes(t, client.manifestsApplied[0]))
	})

	t.Run("uses the default tag when none configured", func(t *testing.T) {
		client := &mockKubernetesClient{}
		installer, err := NewCRDInstaller(client, &Config{
			AdvertiseRoutes: []string{testExtraRoute},
		})
		require.NoError(t, err)
		require.NoError(t, installer.DeployConnector(context.Background()))

		assert.Equal(t, []string{DefaultTag}, connectorTags(t, client.manifestsApplied[0]))
	})

	// Configured tags previously had the default appended, so a caller asking
	// for exactly one tag got two.
	t.Run("configured tags replace the default rather than appending", func(t *testing.T) {
		client := &mockKubernetesClient{}
		installer, err := NewCRDInstaller(client, &Config{
			Tags:            []string{"tag:production"},
			AdvertiseRoutes: []string{testExtraRoute},
		})
		require.NoError(t, err)
		require.NoError(t, installer.DeployConnector(context.Background()))

		assert.Equal(t, []string{"tag:production"}, connectorTags(t, client.manifestsApplied[0]))
	})

	t.Run("does not mutate the configured route slice", func(t *testing.T) {
		routes := []string{testExtraRoute}
		client := &mockKubernetesClient{}
		installer, err := NewCRDInstaller(client, &Config{AdvertiseRoutes: routes})
		require.NoError(t, err)
		require.NoError(t, installer.DeployConnector(context.Background()))

		assert.Equal(t, []string{testExtraRoute}, routes, "config slice must not be modified")
	})

	t.Run("propagates apply failure", func(t *testing.T) {
		client := &mockKubernetesClient{applyErr: fmt.Errorf("apply rejected")}
		installer, err := NewCRDInstaller(client, &Config{AdvertiseRoutes: []string{testExtraRoute}})
		require.NoError(t, err)

		err = installer.DeployConnector(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "apply rejected")
	})

	t.Run("targets the tailscale namespace", func(t *testing.T) {
		client := &mockKubernetesClient{}
		installer, err := NewCRDInstaller(client, &Config{AdvertiseRoutes: []string{testExtraRoute}})
		require.NoError(t, err)
		require.NoError(t, installer.DeployConnector(context.Background()))

		metadata := client.manifestsApplied[0]["metadata"].(map[string]interface{})
		assert.Equal(t, DefaultNamespace, metadata["namespace"])
		assert.Equal(t, "tailscale.com/v1alpha1", client.manifestsApplied[0]["apiVersion"])
		assert.Equal(t, "Connector", client.manifestsApplied[0]["kind"])
	})
}

func TestDeployDNSConfig(t *testing.T) {
	t.Run("applies a DNSConfig", func(t *testing.T) {
		client := &mockKubernetesClient{}
		installer, err := NewCRDInstaller(client, &Config{})
		require.NoError(t, err)
		require.NoError(t, installer.DeployDNSConfig(context.Background()))

		require.Len(t, client.manifestsApplied, 1)
		assert.Equal(t, "DNSConfig", client.manifestsApplied[0]["kind"])
		assert.Equal(t, "tailscale.com/v1alpha1", client.manifestsApplied[0]["apiVersion"])
	})

	// DNSConfig makes tailnet names resolvable from inside the cluster; it does
	// not advertise anything outward, so it is safe without routes.
	t.Run("applied even with no advertise routes", func(t *testing.T) {
		client := &mockKubernetesClient{}
		installer, err := NewCRDInstaller(client, &Config{})
		require.NoError(t, err)
		require.NoError(t, installer.DeployDNSConfig(context.Background()))
		assert.Len(t, client.manifestsApplied, 1)
	})

	t.Run("propagates apply failure", func(t *testing.T) {
		client := &mockKubernetesClient{applyErr: fmt.Errorf("apply rejected")}
		installer, err := NewCRDInstaller(client, &Config{})
		require.NoError(t, err)

		err = installer.DeployDNSConfig(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "apply rejected")
	})
}

// connectorRoutes extracts spec.subnetRouter.advertiseRoutes as strings.
func connectorRoutes(t *testing.T, manifest map[string]interface{}) []string {
	t.Helper()
	spec, ok := manifest["spec"].(map[string]interface{})
	require.True(t, ok, "manifest has no spec")
	router, ok := spec["subnetRouter"].(map[string]interface{})
	require.True(t, ok, "spec has no subnetRouter")
	raw, ok := router["advertiseRoutes"].([]string)
	require.True(t, ok, "subnetRouter has no advertiseRoutes")
	return raw
}

// connectorTags extracts spec.tags as strings.
func connectorTags(t *testing.T, manifest map[string]interface{}) []string {
	t.Helper()
	spec, ok := manifest["spec"].(map[string]interface{})
	require.True(t, ok, "manifest has no spec")
	tags, ok := spec["tags"].([]string)
	require.True(t, ok, "spec has no tags")
	return tags
}

// TestConnectorManifestHasNoVIPField is a belt-and-braces check that no key or
// value anywhere in the rendered Connector mentions a VIP.
func TestConnectorManifestHasNoVIPField(t *testing.T) {
	client := &mockKubernetesClient{}
	installer, err := NewCRDInstaller(client, &Config{AdvertiseRoutes: []string{testExtraRoute}})
	require.NoError(t, err)
	require.NoError(t, installer.DeployConnector(context.Background()))

	rendered := strings.ToLower(fmt.Sprintf("%v", client.manifestsApplied[0]))
	assert.NotContains(t, rendered, "vip")
}
