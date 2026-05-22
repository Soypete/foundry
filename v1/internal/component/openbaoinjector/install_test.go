package openbaoinjector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/catalystcommunity/foundry/v1/internal/helm"
	"github.com/catalystcommunity/foundry/v1/internal/k8s"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// captureStdout redirects os.Stdout while fn runs and returns what was written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	defer func() { os.Stdout = old }()

	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()

	fn()
	w.Close()
	<-done
	return buf.String()
}

type mockHelmClient struct {
	addRepoCalls   []helm.RepoAddOptions
	installCalls   []helm.InstallOptions
	upgradeCalls   []helm.UpgradeOptions
	uninstallCalls []helm.UninstallOptions
	listResponse   []helm.Release
	listErr        error
	addRepoErr     error
	installErr     error
	upgradeErr     error
	uninstallErr   error
}

func (m *mockHelmClient) AddRepo(ctx context.Context, opts helm.RepoAddOptions) error {
	m.addRepoCalls = append(m.addRepoCalls, opts)
	if m.addRepoErr != nil {
		return m.addRepoErr
	}
	return nil
}

func (m *mockHelmClient) Install(ctx context.Context, opts helm.InstallOptions) error {
	m.installCalls = append(m.installCalls, opts)
	if m.installErr != nil {
		return m.installErr
	}
	return nil
}

func (m *mockHelmClient) Upgrade(ctx context.Context, opts helm.UpgradeOptions) error {
	m.upgradeCalls = append(m.upgradeCalls, opts)
	if m.upgradeErr != nil {
		return m.upgradeErr
	}
	return nil
}

func (m *mockHelmClient) Uninstall(ctx context.Context, opts helm.UninstallOptions) error {
	m.uninstallCalls = append(m.uninstallCalls, opts)
	if m.uninstallErr != nil {
		return m.uninstallErr
	}
	return nil
}

func (m *mockHelmClient) List(ctx context.Context, namespace string) ([]helm.Release, error) {
	return m.listResponse, m.listErr
}

func TestInstall_Success(t *testing.T) {
	mock := &mockHelmClient{}
	cfg := &Config{
		Version:           "0.26.2",
		Namespace:         "openbao",
		ExternalVaultAddr: "http://10.0.0.1:8200",
	}

	err := Install(context.Background(), mock, nil, nil, cfg, false)

	require.NoError(t, err)
	assert.Len(t, mock.addRepoCalls, 1)
	assert.Equal(t, "openbao", mock.addRepoCalls[0].Name)
	assert.Equal(t, "https://openbao.github.io/openbao-helm", mock.addRepoCalls[0].URL)
	assert.Len(t, mock.installCalls, 1)
	assert.Equal(t, "openbao-injector", mock.installCalls[0].ReleaseName)
}

func TestInstall_NilHelmClient(t *testing.T) {
	cfg := &Config{
		Version:           "0.26.2",
		Namespace:         "openbao",
		ExternalVaultAddr: "http://10.0.0.1:8200",
	}

	err := Install(context.Background(), nil, nil, nil, cfg, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "helm client cannot be nil")
}

func TestInstall_NilConfig(t *testing.T) {
	mock := &mockHelmClient{}

	err := Install(context.Background(), mock, nil, nil, nil, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "config cannot be nil")
}

func TestInstall_AddRepoFailure(t *testing.T) {
	mock := &mockHelmClient{
		addRepoErr: errors.New("failed to add repo"),
	}
	cfg := &Config{
		Version:           "0.26.2",
		Namespace:         "openbao",
		ExternalVaultAddr: "http://10.0.0.1:8200",
	}

	err := Install(context.Background(), mock, nil, nil, cfg, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to add openbao helm repo")
}

func TestInstall_UpgradeExisting(t *testing.T) {
	mock := &mockHelmClient{
		listResponse: []helm.Release{
			{
				Name:       "openbao-injector",
				Status:     "deployed",
				AppVersion: "0.26.2",
			},
		},
	}
	cfg := &Config{
		Version:           "0.26.3",
		Namespace:         "openbao",
		ExternalVaultAddr: "http://10.0.0.1:8200",
	}

	err := Install(context.Background(), mock, nil, nil, cfg, false)

	require.NoError(t, err)
	assert.Len(t, mock.upgradeCalls, 1)
	assert.Equal(t, "openbao-injector", mock.upgradeCalls[0].ReleaseName)
	assert.Equal(t, "0.26.3", mock.upgradeCalls[0].Version)
	assert.Len(t, mock.installCalls, 0)
}

func TestInstall_ReplaceFailed(t *testing.T) {
	mock := &mockHelmClient{
		listResponse: []helm.Release{
			{
				Name:       "openbao-injector",
				Status:     "failed",
				AppVersion: "0.26.2",
			},
		},
	}
	cfg := &Config{
		Version:           "0.26.3",
		Namespace:         "openbao",
		ExternalVaultAddr: "http://10.0.0.1:8200",
	}

	err := Install(context.Background(), mock, nil, nil, cfg, false)

	require.NoError(t, err)
	assert.Len(t, mock.uninstallCalls, 1)
	assert.Len(t, mock.installCalls, 1)
}

func TestInstall_ReplaceFailedUninstallError(t *testing.T) {
	mock := &mockHelmClient{
		listResponse: []helm.Release{
			{
				Name:       "openbao-injector",
				Status:     "failed",
				AppVersion: "0.26.2",
			},
		},
		uninstallErr: errors.New("failed to uninstall"),
	}
	cfg := &Config{
		Version:           "0.26.3",
		Namespace:         "openbao",
		ExternalVaultAddr: "http://10.0.0.1:8200",
	}

	err := Install(context.Background(), mock, nil, nil, cfg, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to remove existing release")
}

func TestInstall_InstallFailure(t *testing.T) {
	mock := &mockHelmClient{
		installErr: errors.New("failed to install"),
	}
	cfg := &Config{
		Version:           "0.26.2",
		Namespace:         "openbao",
		ExternalVaultAddr: "http://10.0.0.1:8200",
	}

	err := Install(context.Background(), mock, nil, nil, cfg, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to install openbao injector")
}

func TestBuildHelmValues(t *testing.T) {
	cfg := &Config{
		Version:           "0.26.2",
		Namespace:         "openbao",
		ExternalVaultAddr: "http://10.0.0.1:8200",
	}

	values := buildHelmValues(cfg)

	serverConfig, ok := values["server"].(map[string]interface{})
	require.True(t, ok, "server should be a map")
	assert.Equal(t, false, serverConfig["enabled"])

	injectorConfig, ok := values["injector"].(map[string]interface{})
	require.True(t, ok, "injector should be a map")
	assert.Equal(t, true, injectorConfig["enabled"])
	assert.Equal(t, "http://10.0.0.1:8200", injectorConfig["externalVaultAddr"])
}

func TestComponent_Install(t *testing.T) {
	tests := []struct {
		name        string
		cfg         map[string]interface{}
		setupMock   func(*mockHelmClient)
		wantErr     bool
		errContains string
	}{
		{
			name: "successful install",
			cfg: map[string]interface{}{
				"external_vault_addr": "http://10.0.0.1:8200",
			},
			setupMock: func(m *mockHelmClient) {
				m.listResponse = []helm.Release{}
			},
			wantErr: false,
		},
		{
			name:        "missing external_vault_addr",
			cfg:         map[string]interface{}{},
			setupMock:   func(m *mockHelmClient) {},
			wantErr:     true,
			errContains: "external_vault_addr is required",
		},
		{
			name: "helm client error",
			cfg: map[string]interface{}{
				"external_vault_addr": "http://10.0.0.1:8200",
			},
			setupMock: func(m *mockHelmClient) {
				m.addRepoErr = fmt.Errorf("repo error")
			},
			wantErr:     true,
			errContains: "failed to add openbao helm repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockHelmClient{}
			tt.setupMock(mock)

			comp := NewComponent(mock, nil, nil)
			err := comp.Install(context.Background(), tt.cfg)

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContains)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// driftK8sClient implements just enough of K8sClient to drive the drift +
// post-install warning paths. The methods used by configureKubernetesAuth
// panic — those should not be reached when configureK8sAuth=false.
type driftK8sClient struct {
	webhookExists bool
	webhookErr    error
	stuckPods     []k8s.PodRef
	stuckErr      error
}

func (d *driftK8sClient) MutatingWebhookExists(ctx context.Context, name string) (bool, error) {
	return d.webhookExists, d.webhookErr
}

func (d *driftK8sClient) ListPodsNeedingInjectorRestart(ctx context.Context) ([]k8s.PodRef, error) {
	return d.stuckPods, d.stuckErr
}

func (d *driftK8sClient) GetSecret(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	panic("not used in drift test")
}
func (d *driftK8sClient) CreateServiceAccount(ctx context.Context, name string, sa *corev1.ServiceAccount) error {
	panic("not used in drift test")
}
func (d *driftK8sClient) GetServiceAccountToken(ctx context.Context, namespace, name string) (string, error) {
	panic("not used in drift test")
}
func (d *driftK8sClient) ApplyClusterRoleBinding(ctx context.Context, manifest string) error {
	panic("not used in drift test")
}
func (d *driftK8sClient) GetClusterCACert(ctx context.Context) (string, error) {
	panic("not used in drift test")
}
func (d *driftK8sClient) GetKubernetesHost() string { panic("not used in drift test") }

func TestInstall_DriftDetected_ForcesUpgrade(t *testing.T) {
	mock := &mockHelmClient{
		listResponse: []helm.Release{
			{Name: "openbao-injector", Status: "deployed", AppVersion: "0.26.2"},
		},
	}
	k8s := &driftK8sClient{webhookExists: false}
	cfg := &Config{
		Version:           "0.26.2",
		Namespace:         "openbao",
		ExternalVaultAddr: "http://10.0.0.1:8200",
	}

	err := Install(context.Background(), mock, k8s, nil, cfg, false)

	require.NoError(t, err)
	require.Len(t, mock.upgradeCalls, 1, "drifted release should trigger an upgrade")
	assert.True(t, mock.upgradeCalls[0].Force, "drifted release must upgrade with Force=true so chart resources are re-applied")
	assert.Len(t, mock.installCalls, 0)
}

func TestInstall_WebhookPresent_UpgradeWithoutForce(t *testing.T) {
	mock := &mockHelmClient{
		listResponse: []helm.Release{
			{Name: "openbao-injector", Status: "deployed", AppVersion: "0.26.2"},
		},
	}
	k8s := &driftK8sClient{webhookExists: true}
	cfg := &Config{
		Version:           "0.26.3",
		Namespace:         "openbao",
		ExternalVaultAddr: "http://10.0.0.1:8200",
	}

	err := Install(context.Background(), mock, k8s, nil, cfg, false)

	require.NoError(t, err)
	require.Len(t, mock.upgradeCalls, 1)
	assert.False(t, mock.upgradeCalls[0].Force, "Force should stay off when in-cluster state matches helm")
}

func TestInstall_WarnsAboutStuckPods(t *testing.T) {
	mock := &mockHelmClient{
		listResponse: []helm.Release{
			{Name: "openbao-injector", Status: "deployed", AppVersion: "0.26.2"},
		},
	}
	k8sMock := &driftK8sClient{
		webhookExists: false, // triggers the drift reconcile
		stuckPods: []k8s.PodRef{
			{Namespace: "agents", Name: "redditwatch-suggest-abc"},
			{Namespace: "chatbot", Name: "pedro-discord-xyz"},
		},
	}
	cfg := &Config{
		Version:           "0.26.2",
		Namespace:         "openbao",
		ExternalVaultAddr: "http://10.0.0.1:8200",
	}

	out := captureStdout(t, func() {
		require.NoError(t, Install(context.Background(), mock, k8sMock, nil, cfg, false))
	})

	assert.Contains(t, out, "2 pod(s) carry inject annotations but have no sidecar")
	assert.Contains(t, out, "kubectl -n agents delete pod redditwatch-suggest-abc")
	assert.Contains(t, out, "kubectl -n chatbot delete pod pedro-discord-xyz")
}

func TestInstall_NoStuckPods_NoWarning(t *testing.T) {
	mock := &mockHelmClient{}
	k8sMock := &driftK8sClient{webhookExists: true /* stuckPods nil */}
	cfg := &Config{
		Version:           "0.26.2",
		Namespace:         "openbao",
		ExternalVaultAddr: "http://10.0.0.1:8200",
	}

	out := captureStdout(t, func() {
		require.NoError(t, Install(context.Background(), mock, k8sMock, nil, cfg, false))
	})

	assert.NotContains(t, out, "no sidecar")
}
