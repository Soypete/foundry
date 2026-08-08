package component

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/catalystcommunity/foundry/v1/internal/component/k3s"
	"github.com/catalystcommunity/foundry/v1/internal/config"
	"github.com/catalystcommunity/foundry/v1/internal/host"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Synthetic addresses: LAN underlay, CGNAT/Tailscale management, API VIP.
const (
	endpointLAN       = "192.168.1.185"
	endpointTailscale = "100.81.89.62"
	endpointVIP       = "10.0.0.11"
)

func cpHost(hostname, lanAddress, tailscaleAddress string) *host.Host {
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

func TestKubeconfigClientEndpoint(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{
			name: "prefers the control plane tailscale address",
			cfg: &config.Config{
				Cluster: config.ClusterConfig{VIP: endpointVIP},
				Hosts:   []*host.Host{cpHost("blue1", endpointLAN, endpointTailscale)},
			},
			want: endpointTailscale,
		},
		{
			name: "falls back to the VIP without a tailscale address",
			cfg: &config.Config{
				Cluster: config.ClusterConfig{VIP: endpointVIP},
				Hosts:   []*host.Host{cpHost("blue1", endpointLAN, "")},
			},
			want: endpointVIP,
		},
		{
			name: "uses the first control plane host",
			cfg: &config.Config{
				Cluster: config.ClusterConfig{VIP: endpointVIP},
				Hosts: []*host.Host{
					cpHost("blue1", endpointLAN, endpointTailscale),
					cpHost("blue2", "192.168.1.97", "100.125.196.1"),
				},
			},
			want: endpointTailscale,
		},
		{
			// Workers never define the client endpoint.
			name: "ignores worker hosts",
			cfg: &config.Config{
				Cluster: config.ClusterConfig{VIP: endpointVIP},
				Hosts: []*host.Host{
					{
						Hostname: "refurb", Address: "192.168.1.253", Port: 22, User: "root",
						TailscaleAddress: "100.70.90.12",
						Roles:            []string{host.RoleClusterWorker},
					},
					cpHost("blue1", endpointLAN, endpointTailscale),
				},
			},
			want: endpointTailscale,
		},
		{
			name: "empty when no control plane hosts",
			cfg: &config.Config{
				Cluster: config.ClusterConfig{VIP: endpointVIP},
				Hosts:   []*host.Host{},
			},
			want: "",
		},
		{
			name: "empty when neither tailscale address nor VIP configured",
			cfg: &config.Config{
				Cluster: config.ClusterConfig{VIP: ""},
				Hosts:   []*host.Host{cpHost("blue1", endpointLAN, "")},
			},
			want: "",
		},
		{
			name: "nil config is handled",
			cfg:  nil,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, kubeconfigClientEndpoint(tt.cfg))
		})
	}
}

// TestReconcileKubeconfigEndpointDryRun asserts the dry-run path performs no
// I/O: it must not need OpenBAO, and must not write a kubeconfig.
func TestReconcileKubeconfigEndpointDryRun(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("FOUNDRY_CONFIG_DIR", configDir)

	cfg := &config.Config{
		Cluster: config.ClusterConfig{Name: "synthetic", VIP: endpointVIP},
		Hosts:   []*host.Host{cpHost("blue1", endpointLAN, endpointTailscale)},
	}

	require.NoError(t, reconcileKubeconfigEndpoint(context.Background(), cfg, true))

	_, err := os.Stat(filepath.Join(configDir, "kubeconfig"))
	assert.True(t, os.IsNotExist(err), "dry-run must not write a kubeconfig")
}

// TestReconcileKubeconfigEndpointNoClusterHosts asserts the no-op path: a stack
// with no control plane is not an error and touches nothing.
func TestReconcileKubeconfigEndpointNoClusterHosts(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("FOUNDRY_CONFIG_DIR", configDir)

	cfg := &config.Config{
		Cluster: config.ClusterConfig{Name: "synthetic", VIP: endpointVIP},
		Hosts:   []*host.Host{},
	}

	require.NoError(t, reconcileKubeconfigEndpoint(context.Background(), cfg, false))

	_, err := os.Stat(filepath.Join(configDir, "kubeconfig"))
	assert.True(t, os.IsNotExist(err), "no control plane means nothing to reconcile")
}

// TestReconcileKubeconfigEndpointReportsOpenBAOFailure asserts the error path
// when OpenBAO credentials are unavailable. The caller treats this as a
// warning rather than a failed install, so the error must be descriptive.
func TestReconcileKubeconfigEndpointReportsOpenBAOFailure(t *testing.T) {
	t.Setenv("FOUNDRY_CONFIG_DIR", t.TempDir())

	cfg := &config.Config{
		Cluster: config.ClusterConfig{Name: "synthetic", VIP: endpointVIP},
		Hosts:   []*host.Host{cpHost("blue1", endpointLAN, endpointTailscale)},
	}

	// No openbao-keys file exists in the temp config dir.
	err := reconcileKubeconfigEndpoint(context.Background(), cfg, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OpenBAO")
}

// fakeSecretStore stands in for OpenBAO, the only third-party dependency in
// this path. It satisfies k3s.KubeconfigClient, which *openbao.Client also
// satisfies.
type fakeSecretStore struct {
	kubeconfig string
	readErr    error
	writeErr   error
	writes     int
}

func (f *fakeSecretStore) ReadSecretV2(ctx context.Context, mount, path string) (map[string]interface{}, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return map[string]interface{}{"kubeconfig": f.kubeconfig}, nil
}

func (f *fakeSecretStore) WriteSecretV2(ctx context.Context, mount, path string, data map[string]interface{}) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.kubeconfig = data["kubeconfig"].(string)
	f.writes++
	return nil
}

func syntheticKubeconfig(server string) string {
	return "apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    server: " + server + "\n  name: default\n"
}

func stackWithControlPlane(tailscaleAddress string) *config.Config {
	return &config.Config{
		Cluster: config.ClusterConfig{Name: "synthetic", VIP: endpointVIP},
		Hosts:   []*host.Host{cpHost("blue1", endpointLAN, tailscaleAddress)},
	}
}

// TestReconcileKubeconfigEndpointRepairsStaleEndpoint is the headline case: a
// kubeconfig left pointing at the LAN is re-pointed at the Tailscale address
// and mirrored to ~/.foundry/kubeconfig.
func TestReconcileKubeconfigEndpointRepairsStaleEndpoint(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("FOUNDRY_CONFIG_DIR", configDir)

	store := &fakeSecretStore{kubeconfig: syntheticKubeconfig("https://" + endpointLAN + ":6443")}

	require.NoError(t, reconcileKubeconfigEndpointWithClient(
		context.Background(), stackWithControlPlane(endpointTailscale),
		func() (k3s.KubeconfigClient, error) { return store, nil },
	))

	assert.Contains(t, store.kubeconfig, "server: https://"+endpointTailscale+":6443")
	assert.NotContains(t, store.kubeconfig, endpointLAN)

	exported, err := os.ReadFile(filepath.Join(configDir, "kubeconfig"))
	require.NoError(t, err, "repaired kubeconfig must be exported locally")
	assert.Contains(t, string(exported), "server: https://"+endpointTailscale+":6443")
}

// TestReconcileKubeconfigEndpointIsIdempotent asserts a second run rewrites
// neither store when both are already converged.
func TestReconcileKubeconfigEndpointIsIdempotent(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("FOUNDRY_CONFIG_DIR", configDir)

	store := &fakeSecretStore{kubeconfig: syntheticKubeconfig("https://" + endpointLAN + ":6443")}
	newClient := func() (k3s.KubeconfigClient, error) { return store, nil }
	cfg := stackWithControlPlane(endpointTailscale)

	require.NoError(t, reconcileKubeconfigEndpointWithClient(context.Background(), cfg, newClient))
	require.Equal(t, 1, store.writes)

	path := filepath.Join(configDir, "kubeconfig")
	before, err := os.Stat(path)
	require.NoError(t, err)

	require.NoError(t, reconcileKubeconfigEndpointWithClient(context.Background(), cfg, newClient))
	assert.Equal(t, 1, store.writes, "second run must not write to the secret store")

	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, before.ModTime(), after.ModTime(), "converged local file must not be rewritten")
}

// TestReconcileKubeconfigEndpointRepairsStaleLocalFileWhenStoreIsCurrent is the
// regression test for the Gate 3 validation failure (VALIDATION.md iteration 1).
//
// On the live cluster the OpenBAO copy already carried the Tailscale endpoint
// while ~/.foundry/kubeconfig was still on the LAN address. The reconcile
// reported "endpoint unchanged" and returned early, leaving the local file --
// the one kubectl actually reads -- stale. The two stores are independent and
// each must be reconciled against its own state.
func TestReconcileKubeconfigEndpointRepairsStaleLocalFileWhenStoreIsCurrent(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("FOUNDRY_CONFIG_DIR", configDir)

	// Secret store already correct; local file stale on the LAN endpoint.
	store := &fakeSecretStore{kubeconfig: syntheticKubeconfig("https://" + endpointTailscale + ":6443")}
	path := filepath.Join(configDir, "kubeconfig")
	require.NoError(t, os.WriteFile(path, []byte(syntheticKubeconfig("https://"+endpointLAN+":6443")), 0600))

	require.NoError(t, reconcileKubeconfigEndpointWithClient(
		context.Background(), stackWithControlPlane(endpointTailscale),
		func() (k3s.KubeconfigClient, error) { return store, nil },
	))

	assert.Equal(t, 0, store.writes, "an already-correct secret store must not be rewritten")

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(got), "server: https://"+endpointTailscale+":6443",
		"stale local kubeconfig must be repaired even when the store is current")
	assert.NotContains(t, string(got), endpointLAN)
}

// TestReconcileKubeconfigEndpointCreatesMissingLocalFile covers a local
// kubeconfig that does not exist yet while the store is already correct.
func TestReconcileKubeconfigEndpointCreatesMissingLocalFile(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("FOUNDRY_CONFIG_DIR", configDir)

	store := &fakeSecretStore{kubeconfig: syntheticKubeconfig("https://" + endpointTailscale + ":6443")}

	require.NoError(t, reconcileKubeconfigEndpointWithClient(
		context.Background(), stackWithControlPlane(endpointTailscale),
		func() (k3s.KubeconfigClient, error) { return store, nil },
	))

	got, err := os.ReadFile(filepath.Join(configDir, "kubeconfig"))
	require.NoError(t, err, "a missing local kubeconfig must be created")
	assert.Contains(t, string(got), "server: https://"+endpointTailscale+":6443")
}

func TestExportKubeconfigIfStale(t *testing.T) {
	store := func() *fakeSecretStore {
		return &fakeSecretStore{kubeconfig: syntheticKubeconfig("https://" + endpointTailscale + ":6443")}
	}

	t.Run("writes when the local file is stale", func(t *testing.T) {
		configDir := t.TempDir()
		t.Setenv("FOUNDRY_CONFIG_DIR", configDir)
		path := filepath.Join(configDir, "kubeconfig")
		require.NoError(t, os.WriteFile(path, []byte(syntheticKubeconfig("https://"+endpointLAN+":6443")), 0600))

		wrote, err := exportKubeconfigIfStale(context.Background(), store(), endpointTailscale)
		require.NoError(t, err)
		assert.True(t, wrote)
	})

	t.Run("skips when the local file already targets the endpoint", func(t *testing.T) {
		configDir := t.TempDir()
		t.Setenv("FOUNDRY_CONFIG_DIR", configDir)
		path := filepath.Join(configDir, "kubeconfig")
		require.NoError(t, os.WriteFile(path, []byte(syntheticKubeconfig("https://"+endpointTailscale+":6443")), 0600))

		wrote, err := exportKubeconfigIfStale(context.Background(), store(), endpointTailscale)
		require.NoError(t, err)
		assert.False(t, wrote)
	})

	t.Run("writes when the local file is missing", func(t *testing.T) {
		t.Setenv("FOUNDRY_CONFIG_DIR", t.TempDir())
		wrote, err := exportKubeconfigIfStale(context.Background(), store(), endpointTailscale)
		require.NoError(t, err)
		assert.True(t, wrote)
	})

	t.Run("rewrites an unparseable local file", func(t *testing.T) {
		configDir := t.TempDir()
		t.Setenv("FOUNDRY_CONFIG_DIR", configDir)
		require.NoError(t, os.WriteFile(filepath.Join(configDir, "kubeconfig"), []byte("garbage"), 0600))

		wrote, err := exportKubeconfigIfStale(context.Background(), store(), endpointTailscale)
		require.NoError(t, err)
		assert.True(t, wrote, "a file with no readable endpoint must be replaced")
	})

	t.Run("reports an unreadable local path", func(t *testing.T) {
		configDir := t.TempDir()
		t.Setenv("FOUNDRY_CONFIG_DIR", configDir)
		// A directory where the file belongs makes the read fail with a
		// non-IsNotExist error.
		require.NoError(t, os.Mkdir(filepath.Join(configDir, "kubeconfig"), 0700))

		_, err := exportKubeconfigIfStale(context.Background(), store(), endpointTailscale)
		require.Error(t, err)
	})
}

// TestReconcileKubeconfigEndpointFallsBackToVIP covers a control plane with no
// Tailscale address: the kubeconfig targets the VIP.
func TestReconcileKubeconfigEndpointFallsBackToVIP(t *testing.T) {
	t.Setenv("FOUNDRY_CONFIG_DIR", t.TempDir())

	store := &fakeSecretStore{kubeconfig: syntheticKubeconfig("https://127.0.0.1:6443")}

	require.NoError(t, reconcileKubeconfigEndpointWithClient(
		context.Background(), stackWithControlPlane(""),
		func() (k3s.KubeconfigClient, error) { return store, nil },
	))

	assert.Contains(t, store.kubeconfig, "server: https://"+endpointVIP+":6443")
}

func TestReconcileKubeconfigEndpointWithClientErrorPaths(t *testing.T) {
	t.Run("no control plane is a no-op and never opens the store", func(t *testing.T) {
		t.Setenv("FOUNDRY_CONFIG_DIR", t.TempDir())
		opened := false
		cfg := &config.Config{Cluster: config.ClusterConfig{VIP: endpointVIP}, Hosts: []*host.Host{}}

		require.NoError(t, reconcileKubeconfigEndpointWithClient(context.Background(), cfg,
			func() (k3s.KubeconfigClient, error) { opened = true; return nil, nil }))
		assert.False(t, opened)
	})

	t.Run("client construction failure is wrapped", func(t *testing.T) {
		t.Setenv("FOUNDRY_CONFIG_DIR", t.TempDir())
		err := reconcileKubeconfigEndpointWithClient(context.Background(), stackWithControlPlane(endpointTailscale),
			func() (k3s.KubeconfigClient, error) { return nil, errors.New("keys file missing") })
		require.Error(t, err)
		assert.Contains(t, err.Error(), "OpenBAO")
		assert.Contains(t, err.Error(), "keys file missing")
	})

	t.Run("refresh failure propagates", func(t *testing.T) {
		t.Setenv("FOUNDRY_CONFIG_DIR", t.TempDir())
		store := &fakeSecretStore{readErr: errors.New("openbao sealed")}
		err := reconcileKubeconfigEndpointWithClient(context.Background(), stackWithControlPlane(endpointTailscale),
			func() (k3s.KubeconfigClient, error) { return store, nil })
		require.Error(t, err)
		assert.Contains(t, err.Error(), "openbao sealed")
	})

	t.Run("export failure reports that the store was already updated", func(t *testing.T) {
		// A directory where the kubeconfig file belongs makes the write fail.
		configDir := t.TempDir()
		t.Setenv("FOUNDRY_CONFIG_DIR", configDir)
		require.NoError(t, os.Mkdir(filepath.Join(configDir, "kubeconfig"), 0700))

		store := &fakeSecretStore{kubeconfig: syntheticKubeconfig("https://" + endpointLAN + ":6443")}
		err := reconcileKubeconfigEndpointWithClient(context.Background(), stackWithControlPlane(endpointTailscale),
			func() (k3s.KubeconfigClient, error) { return store, nil })
		require.Error(t, err)
		assert.Contains(t, err.Error(), "stored in OpenBAO but local export failed")
	})
}

func TestExportKubeconfig(t *testing.T) {
	t.Run("writes the stored kubeconfig with owner-only permissions", func(t *testing.T) {
		configDir := t.TempDir()
		t.Setenv("FOUNDRY_CONFIG_DIR", configDir)

		want := syntheticKubeconfig("https://" + endpointTailscale + ":6443")
		require.NoError(t, exportKubeconfig(context.Background(), &fakeSecretStore{kubeconfig: want}))

		path := filepath.Join(configDir, "kubeconfig")
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, want, string(got))

		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm(),
			"a kubeconfig grants cluster access and must not be group/world readable")
	})

	t.Run("overwrites an existing kubeconfig", func(t *testing.T) {
		configDir := t.TempDir()
		t.Setenv("FOUNDRY_CONFIG_DIR", configDir)
		path := filepath.Join(configDir, "kubeconfig")
		require.NoError(t, os.WriteFile(path, []byte("stale"), 0600))

		want := syntheticKubeconfig("https://" + endpointTailscale + ":6443")
		require.NoError(t, exportKubeconfig(context.Background(), &fakeSecretStore{kubeconfig: want}))

		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, want, string(got))
	})

	t.Run("load failure propagates", func(t *testing.T) {
		t.Setenv("FOUNDRY_CONFIG_DIR", t.TempDir())
		err := exportKubeconfig(context.Background(), &fakeSecretStore{readErr: errors.New("openbao unreachable")})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "openbao unreachable")
	})
}
