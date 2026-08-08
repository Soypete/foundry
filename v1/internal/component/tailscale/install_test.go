package tailscale

import (
	"context"
	"fmt"
	"testing"

	"github.com/catalystcommunity/foundry/v1/internal/helm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSecretWriter records the secrets written by the installer.
type fakeSecretWriter struct {
	err     error
	written []writtenSecret
}

type writtenSecret struct {
	namespace string
	name      string
	data      map[string]string
}

func (f *fakeSecretWriter) EnsureSecret(ctx context.Context, namespace, name string, data map[string]string) error {
	if f.err != nil {
		return f.err
	}
	f.written = append(f.written, writtenSecret{namespace: namespace, name: name, data: data})
	return nil
}

func strPtr(s string) *string { return &s }

// validConfig returns a config with synthetic OAuth credentials.
func validConfig() *Config {
	return &Config{
		OAuthClientID:     strPtr("synthetic-client-id"),
		OAuthClientSecret: strPtr("synthetic-client-secret"),
	}
}

// newTestInstaller wires an installer over fakes, returning them for assertions.
func newTestInstaller(t *testing.T, cfg *Config, helmClient *mockHelmClient) (*Installer, *mockKubernetesClient, *fakeSecretWriter) {
	t.Helper()
	helmInstaller, err := NewHelmInstaller(helmClient, cfg)
	require.NoError(t, err)
	k8sClient := &mockKubernetesClient{}
	crdInstaller, err := NewCRDInstaller(k8sClient, cfg)
	require.NoError(t, err)
	secrets := &fakeSecretWriter{}

	installer, err := NewInstaller(helmInstaller, crdInstaller, secrets, cfg)
	require.NoError(t, err)
	return installer, k8sClient, secrets
}

func TestNewInstaller(t *testing.T) {
	cfg := validConfig()
	helmInstaller, err := NewHelmInstaller(&mockHelmClient{}, cfg)
	require.NoError(t, err)
	crdInstaller, err := NewCRDInstaller(&mockKubernetesClient{}, cfg)
	require.NoError(t, err)

	tests := []struct {
		name    string
		helm    *HelmInstaller
		crds    *CRDInstaller
		secrets SecretWriter
		config  *Config
		wantErr string
	}{
		{name: "valid", helm: helmInstaller, crds: crdInstaller, secrets: &fakeSecretWriter{}, config: validConfig()},
		{name: "nil helm", crds: crdInstaller, secrets: &fakeSecretWriter{}, config: validConfig(), wantErr: "helm installer cannot be nil"},
		{name: "nil crds", helm: helmInstaller, secrets: &fakeSecretWriter{}, config: validConfig(), wantErr: "crd installer cannot be nil"},
		{name: "nil secrets", helm: helmInstaller, crds: crdInstaller, config: validConfig(), wantErr: "secret writer cannot be nil"},
		{name: "nil config", helm: helmInstaller, crds: crdInstaller, secrets: &fakeSecretWriter{}, wantErr: "config cannot be nil"},
		{
			name: "missing OAuth credentials", helm: helmInstaller, crds: crdInstaller,
			secrets: &fakeSecretWriter{}, config: &Config{},
			wantErr: "oauth_client_id is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installer, err := NewInstaller(tt.helm, tt.crds, tt.secrets, tt.config)
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

func TestNewInstallerAppliesDefaults(t *testing.T) {
	cfg := validConfig()
	_, _, _ = newTestInstaller(t, cfg, &mockHelmClient{})

	require.NotNil(t, cfg.OperatorImage)
	assert.Equal(t, "tailscale/operator:latest", *cfg.OperatorImage)
	assert.Equal(t, []string{DefaultTag}, cfg.Tags)
}

func TestInstall(t *testing.T) {
	t.Run("writes OAuth secret before installing the operator", func(t *testing.T) {
		helmClient := &mockHelmClient{}
		installer, _, secrets := newTestInstaller(t, validConfig(), helmClient)
		require.NoError(t, installer.Install(context.Background()))

		require.Len(t, secrets.written, 1)
		assert.Equal(t, DefaultNamespace, secrets.written[0].namespace)
		assert.Equal(t, OAuthSecretName, secrets.written[0].name)
		assert.Equal(t, "synthetic-client-id", secrets.written[0].data["client_id"])
		assert.Equal(t, "synthetic-client-secret", secrets.written[0].data["client_secret"])
	})

	// Credentials in Helm values would be readable via `helm get values` and
	// persisted in the release secret.
	t.Run("never passes OAuth credentials as Helm values", func(t *testing.T) {
		helmClient := &mockHelmClient{}
		installer, _, _ := newTestInstaller(t, validConfig(), helmClient)
		require.NoError(t, installer.Install(context.Background()))

		require.Len(t, helmClient.installCalls, 1)
		rendered := fmt.Sprintf("%v", helmClient.installCalls[0].Values)
		assert.NotContains(t, rendered, "synthetic-client-secret")
		assert.NotContains(t, rendered, "synthetic-client-id")
	})

	t.Run("installs the operator and applies DNSConfig", func(t *testing.T) {
		helmClient := &mockHelmClient{}
		installer, k8sClient, _ := newTestInstaller(t, validConfig(), helmClient)
		require.NoError(t, installer.Install(context.Background()))

		assert.Len(t, helmClient.repoAddCalls, 1)
		assert.Len(t, helmClient.installCalls, 1)
		require.Len(t, k8sClient.manifestsApplied, 1)
		assert.Equal(t, "DNSConfig", k8sClient.manifestsApplied[0]["kind"])
	})

	// Default posture: no advertise_routes, so no Connector and nothing from
	// the cluster's own networks reaches the tailnet.
	t.Run("creates no Connector without configured routes", func(t *testing.T) {
		helmClient := &mockHelmClient{}
		installer, k8sClient, _ := newTestInstaller(t, validConfig(), helmClient)
		require.NoError(t, installer.Install(context.Background()))

		for _, m := range k8sClient.manifestsApplied {
			assert.NotEqual(t, "Connector", m["kind"])
		}
	})

	t.Run("creates a Connector when routes are configured", func(t *testing.T) {
		cfg := validConfig()
		cfg.AdvertiseRoutes = []string{testExtraRoute}
		helmClient := &mockHelmClient{}
		installer, k8sClient, _ := newTestInstaller(t, cfg, helmClient)
		require.NoError(t, installer.Install(context.Background()))

		kinds := make([]string, 0, len(k8sClient.manifestsApplied))
		for _, m := range k8sClient.manifestsApplied {
			kinds = append(kinds, m["kind"].(string))
		}
		assert.Contains(t, kinds, "Connector")
		assert.Contains(t, kinds, "DNSConfig")
	})

	// The live operator was installed outside Foundry, so the command must
	// converge onto an existing release rather than failing.
	t.Run("adopts an operator installed outside foundry", func(t *testing.T) {
		helmClient := &mockHelmClient{
			releases: []helm.Release{{Name: OperatorReleaseName, Namespace: DefaultNamespace, Status: "deployed"}},
		}
		installer, _, _ := newTestInstaller(t, validConfig(), helmClient)
		require.NoError(t, installer.Install(context.Background()))

		assert.Empty(t, helmClient.installCalls, "must not attempt a fresh install")
		require.Len(t, helmClient.upgradeCalls, 1)
		assert.Equal(t, OperatorReleaseName, helmClient.upgradeCalls[0].ReleaseName)
	})

	// A second run must converge with no error and no duplicate resources.
	t.Run("is idempotent", func(t *testing.T) {
		helmClient := &mockHelmClient{}
		installer, k8sClient, secrets := newTestInstaller(t, validConfig(), helmClient)

		require.NoError(t, installer.Install(context.Background()))
		// Simulate the release now existing, as it would on a real second run.
		helmClient.releases = []helm.Release{{Name: OperatorReleaseName, Status: "deployed"}}
		firstManifests := len(k8sClient.manifestsApplied)

		require.NoError(t, installer.Install(context.Background()))

		assert.Len(t, helmClient.installCalls, 1, "second run must upgrade, not install")
		assert.Len(t, helmClient.upgradeCalls, 1)
		assert.Len(t, secrets.written, 2, "secret is rewritten with identical content")
		assert.Equal(t, secrets.written[0].data, secrets.written[1].data)
		assert.Equal(t, firstManifests*2, len(k8sClient.manifestsApplied),
			"manifests are re-applied, not duplicated in kind")
	})

	t.Run("secret failure aborts before touching Helm", func(t *testing.T) {
		helmClient := &mockHelmClient{}
		helmInstaller, err := NewHelmInstaller(helmClient, validConfig())
		require.NoError(t, err)
		crdInstaller, err := NewCRDInstaller(&mockKubernetesClient{}, validConfig())
		require.NoError(t, err)
		installer, err := NewInstaller(helmInstaller, crdInstaller,
			&fakeSecretWriter{err: fmt.Errorf("forbidden")}, validConfig())
		require.NoError(t, err)

		err = installer.Install(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "forbidden")
		assert.Empty(t, helmClient.installCalls, "operator must not start without credentials")
	})

	t.Run("propagates repo failure", func(t *testing.T) {
		helmClient := &mockHelmClient{addRepoErr: fmt.Errorf("repo unreachable")}
		installer, _, _ := newTestInstaller(t, validConfig(), helmClient)

		err := installer.Install(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "repo unreachable")
	})

	t.Run("propagates operator install failure", func(t *testing.T) {
		helmClient := &mockHelmClient{installErr: fmt.Errorf("chart missing")}
		installer, _, _ := newTestInstaller(t, validConfig(), helmClient)

		err := installer.Install(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "chart missing")
	})
}

func TestUninstall(t *testing.T) {
	t.Run("uninstalls the operator release", func(t *testing.T) {
		helmClient := &mockHelmClient{}
		installer, _, _ := newTestInstaller(t, validConfig(), helmClient)
		require.NoError(t, installer.Uninstall(context.Background()))
		assert.Len(t, helmClient.uninstallCalls, 1)
	})

	t.Run("propagates failure", func(t *testing.T) {
		helmClient := &mockHelmClient{uninstallErr: fmt.Errorf("release not found")}
		installer, _, _ := newTestInstaller(t, validConfig(), helmClient)

		err := installer.Uninstall(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "release not found")
	})
}
