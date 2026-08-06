package component

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/catalystcommunity/foundry/v1/cmd/foundry/registry"
	"github.com/catalystcommunity/foundry/v1/internal/component"
	"github.com/catalystcommunity/foundry/v1/internal/component/k3s"
	"github.com/catalystcommunity/foundry/v1/internal/config"
	"github.com/catalystcommunity/foundry/v1/internal/host"
	"github.com/catalystcommunity/foundry/v1/internal/setup"
	"github.com/catalystcommunity/foundry/v1/internal/ssh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

func TestInstallCommand_DryRun(t *testing.T) {
	// Create a temporary directory for test isolation
	tmpDir := t.TempDir()

	// Set HOME environment variable to use temp directory
	// This ensures no real config file interferes with the test
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	// Create a fresh registry for testing
	testRegistry := component.NewRegistry()

	// Temporarily replace the default registry
	oldRegistry := component.DefaultRegistry
	component.DefaultRegistry = testRegistry
	defer func() { component.DefaultRegistry = oldRegistry }()

	// Initialize component registry
	err := registry.InitComponents()
	require.NoError(t, err)

	// Initialize host registry (needed for install command)
	err = registry.InitHostRegistry()
	require.NoError(t, err)

	tests := []struct {
		name          string
		args          []string
		expectError   bool
		expectOutput  []string
		errorContains string
	}{
		{
			name:          "no arguments",
			args:          []string{"test", "install"},
			expectError:   true,
			errorContains: "component name required",
		},
		{
			name:          "non-existent component",
			args:          []string{"test", "install", "nonexistent"},
			expectError:   true,
			errorContains: "not found",
		},
		{
			name:          "dry-run openbao (requires config)",
			args:          []string{"test", "install", "openbao", "--dry-run"},
			expectError:   true, // Will fail without stack config
			errorContains: "config file not found",
		},
		{
			name:          "dry-run with version (requires config)",
			args:          []string{"test", "install", "openbao", "--dry-run", "--version", "2.0.0"},
			expectError:   true, // Will fail without stack config
			errorContains: "config file not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Capture stdout
			oldStdout := os.Stdout
			r, w, _ := os.Pipe()
			os.Stdout = w

			// Create CLI command
			cmd := &cli.Command{
				Commands: []*cli.Command{
					InstallCommand,
				},
			}

			// Run the install command
			err := cmd.Run(context.Background(), tt.args)

			// Restore stdout and read captured output
			w.Close()
			os.Stdout = oldStdout
			var buf bytes.Buffer
			io.Copy(&buf, r)
			output := buf.String()

			// Check error expectation
			if tt.expectError {
				assert.Error(t, err)
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains)
				}
			} else {
				assert.NoError(t, err)
			}

			// Check output contains expected strings
			for _, expected := range tt.expectOutput {
				assert.Contains(t, output, expected)
			}
		})
	}
}

func TestInstallCommand_DependencyCheck(t *testing.T) {
	// Create a temporary directory for the test config
	tmpDir := t.TempDir()

	// Set HOME environment variable to use temp directory
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	// Create .foundry directory
	foundryDir := filepath.Join(tmpDir, ".foundry")
	err := os.MkdirAll(foundryDir, 0755)
	require.NoError(t, err)

	// Create a minimal stack config
	configPath := filepath.Join(foundryDir, "stack.yaml")
	configContent := `cluster:
  name: test-cluster
  domain: test.local
  vip: 192.168.1.100
network:
  gateway: 192.168.1.1
  netmask: 255.255.255.0
hosts:
  - hostname: test-host
    address: 192.168.1.10
    port: 22
    user: root
    roles:
      - openbao
      - dns
      - zot
dns:
  backend: gsqlite3
  api_key: test-api-key
  infrastructure_zones:
    - name: infra.local
  kubernetes_zones:
    - name: k8s.local
components:
  openbao: {}
  dns: {}
  zot: {}
`
	err = os.WriteFile(configPath, []byte(configContent), 0644)
	require.NoError(t, err)

	// Create a fresh registry for testing
	testRegistry := component.NewRegistry()

	// Temporarily replace the default registry
	oldRegistry := component.DefaultRegistry
	component.DefaultRegistry = testRegistry
	defer func() { component.DefaultRegistry = oldRegistry }()

	// Initialize component registry
	err = registry.InitComponents()
	require.NoError(t, err)

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	// Create CLI command
	cmd := &cli.Command{
		Commands: []*cli.Command{
			InstallCommand,
		},
	}

	// Try to install k3s with --dry-run (should show dependencies)
	err = cmd.Run(context.Background(), []string{"test", "install", "k3s", "--dry-run"})

	// Restore stdout and read captured output
	w.Close()
	os.Stdout = oldStdout
	var buf bytes.Buffer
	io.Copy(&buf, r)
	output := buf.String()

	// K3s depends on openbao, dns, and zot
	// Since they're not installed, the command should fail
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not installed")
	// Check that dependencies are shown in output
	assert.Contains(t, output, "depends on")
}

func TestInstallCommand_K3sDryRun(t *testing.T) {
	tmpDir := t.TempDir()

	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	foundryDir := filepath.Join(tmpDir, ".foundry")
	err := os.MkdirAll(foundryDir, 0755)
	require.NoError(t, err)

	configPath := filepath.Join(foundryDir, "stack.yaml")
	configContent := `cluster:
  name: test-cluster
  primary_domain: test.local
  vip: 192.168.1.100
network:
  gateway: 192.168.1.1
  netmask: 255.255.255.0
hosts:
  - hostname: test-cp
    address: 192.168.1.10
    port: 22
    user: root
    roles:
      - cluster-control-plane
      - openbao
      - dns
      - zot
  - hostname: test-worker
    address: 192.168.1.11
    port: 22
    user: root
    roles:
      - cluster-worker
dns:
  backend: gsqlite3
  api_key: test-api-key
  infrastructure_zones:
    - name: infra.local
  kubernetes_zones:
    - name: k8s.local
components:
  openbao:
    installed: true
  dns:
    installed: true
  zot:
    installed: true
  k3s:
    installed: true
setup_state:
  openbao_installed: true
  dns_installed: true
  zot_installed: true
  k8s_installed: true
`
	err = os.WriteFile(configPath, []byte(configContent), 0644)
	require.NoError(t, err)

	testRegistry := component.NewRegistry()
	oldRegistry := component.DefaultRegistry
	component.DefaultRegistry = testRegistry
	defer func() { component.DefaultRegistry = oldRegistry }()

	err = registry.InitComponents()
	require.NoError(t, err)

	tests := []struct {
		name         string
		args         []string
		expectOutput []string
	}{
		{
			name:         "k3s dry-run single node",
			args:         []string{"test", "--config", configPath, "install", "k3s", "--dry-run"},
			expectOutput: []string{"Would reconcile node", "test-cp"},
		},
		{
			name:         "k3s dry-run all nodes",
			args:         []string{"test", "--config", configPath, "install", "k3s", "--dry-run", "--all-nodes"},
			expectOutput: []string{"Would reconcile registries.yaml on 2 cluster nodes", "test-cp", "test-worker"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldStdout := os.Stdout
			r, w, _ := os.Pipe()
			os.Stdout = w

			cmd := &cli.Command{
				Name: "test",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:    "config",
						Aliases: []string{"c"},
						Usage:   "path to config file",
					},
				},
				Commands: []*cli.Command{
					InstallCommand,
				},
			}

			err := cmd.Run(context.Background(), tt.args)

			w.Close()
			os.Stdout = oldStdout
			var buf bytes.Buffer
			io.Copy(&buf, r)
			output := buf.String()

			assert.NoError(t, err)
			for _, expected := range tt.expectOutput {
				assert.Contains(t, output, expected, "expected output to contain: %s", expected)
			}
		})
	}
}

func TestInstallCommand_K3sAllNodesFlag(t *testing.T) {
	tmpDir := t.TempDir()

	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", oldHome)

	foundryDir := filepath.Join(tmpDir, ".foundry")
	err := os.MkdirAll(foundryDir, 0755)
	require.NoError(t, err)

	configPath := filepath.Join(foundryDir, "stack.yaml")
	configContent := `cluster:
  name: test-cluster
  primary_domain: test.local
  vip: 192.168.1.100
hosts:
  - hostname: cp1
    address: 192.168.1.10
    port: 22
    user: root
    roles:
      - cluster-control-plane
      - openbao
      - dns
      - zot
  - hostname: worker1
    address: 192.168.1.11
    port: 22
    user: root
    roles:
      - cluster-worker
  - hostname: worker2
    address: 192.168.1.12
    port: 22
    user: root
    roles:
      - cluster-worker
dns:
  backend: gsqlite3
  api_key: test-api-key
components:
  openbao:
    installed: true
  dns:
    installed: true
  zot:
    installed: true
  k3s:
    installed: true
setup_state:
  openbao_installed: true
  dns_installed: true
  zot_installed: true
  k8s_installed: true
`
	err = os.WriteFile(configPath, []byte(configContent), 0644)
	require.NoError(t, err)

	testRegistry := component.NewRegistry()
	oldRegistry := component.DefaultRegistry
	component.DefaultRegistry = testRegistry
	defer func() { component.DefaultRegistry = oldRegistry }()

	err = registry.InitComponents()
	require.NoError(t, err)

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	cmd := &cli.Command{
		Name: "test",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Usage:   "path to config file",
			},
		},
		Commands: []*cli.Command{
			InstallCommand,
		},
	}

	err = cmd.Run(context.Background(), []string{"test", "--config", configPath, "install", "k3s", "--dry-run", "--all-nodes"})

	w.Close()
	os.Stdout = oldStdout
	var buf bytes.Buffer
	io.Copy(&buf, r)
	output := buf.String()

	assert.NoError(t, err)
	assert.Contains(t, output, "3 cluster nodes")
	assert.Contains(t, output, "cp1")
	assert.Contains(t, output, "worker1")
	assert.Contains(t, output, "worker2")
}

// fakeK3sExecutor records the commands issued to a single node.
type fakeK3sExecutor struct {
	hostname string
	commands []string
	// existingRegistries is returned for `cat .../registries.yaml`.
	existingRegistries string
	existingNetwork    string
	// k3sActive controls whether k3s reports as installed.
	k3sActive bool
	// execErr, when set, is returned for any command matching failOn.
	failOn  string
	execErr error
}

func (f *fakeK3sExecutor) Exec(command string) (*ssh.ExecResult, error) {
	f.commands = append(f.commands, command)

	if f.failOn != "" && strings.Contains(command, f.failOn) {
		if f.execErr != nil {
			return nil, f.execErr
		}
		return &ssh.ExecResult{ExitCode: 1, Stderr: "command failed"}, nil
	}

	switch {
	case strings.Contains(command, "systemctl is-active k3s"):
		if !f.k3sActive {
			return &ssh.ExecResult{ExitCode: 3, Stdout: "inactive"}, nil
		}
		return &ssh.ExecResult{ExitCode: 0, Stdout: "active"}, nil
	case strings.Contains(command, "cat /etc/rancher/k3s/registries.yaml"):
		return &ssh.ExecResult{ExitCode: 0, Stdout: f.existingRegistries}, nil
	case strings.Contains(command, k3s.NetworkConfigPath):
		return &ssh.ExecResult{ExitCode: 0, Stdout: f.existingNetwork}, nil
	case strings.Contains(command, "ip -o -4 addr show"):
		return &ssh.ExecResult{ExitCode: 0, Stdout: "eth0\n"}, nil
	case strings.Contains(command, "k3s kubectl get nodes"):
		return &ssh.ExecResult{ExitCode: 0, Stdout: "node Ready"}, nil
	}
	return &ssh.ExecResult{ExitCode: 0}, nil
}

// wroteRegistriesConfig reports whether registries.yaml was written.
func (f *fakeK3sExecutor) wroteRegistriesConfig() bool {
	for _, c := range f.commands {
		if strings.Contains(c, "/etc/rancher/k3s/registries.yaml") && !strings.HasPrefix(strings.TrimSpace(c), "cat ") {
			return true
		}
	}
	return false
}

func (f *fakeK3sExecutor) restartedK3s() bool {
	for _, c := range f.commands {
		if strings.Contains(c, "systemctl restart k3s") {
			return true
		}
	}
	return false
}

// k3sTestHarness builds a connector that hands out one fake executor per host
// and records the order in which hosts were dialed.
type k3sTestHarness struct {
	executors map[string]*fakeK3sExecutor
	dialed    []string
	closed    int
	// connectErrOn makes the connector fail for a given hostname.
	connectErrOn string
	// template configures each newly created executor.
	template func(*fakeK3sExecutor)
}

func newK3sTestHarness() *k3sTestHarness {
	return &k3sTestHarness{executors: map[string]*fakeK3sExecutor{}}
}

func (h *k3sTestHarness) connector() k3sNodeConnector {
	return func(target *host.Host) (k3s.SSHExecutor, func(), error) {
		h.dialed = append(h.dialed, target.Hostname)
		if h.connectErrOn != "" && target.Hostname == h.connectErrOn {
			return nil, nil, fmt.Errorf("dial failed for %s", target.Hostname)
		}
		exec := &fakeK3sExecutor{hostname: target.Hostname, k3sActive: true}
		if h.template != nil {
			h.template(exec)
		}
		h.executors[target.Hostname] = exec
		return exec, func() { h.closed++ }, nil
	}
}

// k3sTestConfig builds a stack config with one control plane and two workers.
func k3sTestConfig() *config.Config {
	return &config.Config{
		Cluster: config.ClusterConfig{
			Name: "test-cluster",
			VIP:  "192.168.1.100",
		},
		Hosts: []*host.Host{
			{Hostname: "cp1", Address: "192.168.1.10", Port: 22, User: "root",
				Roles: []string{host.RoleClusterControlPlane, host.RoleZot}},
			{Hostname: "worker1", Address: "192.168.1.11", Port: 22, User: "root",
				Roles: []string{host.RoleClusterWorker}},
			{Hostname: "worker2", Address: "192.168.1.12", Port: 22, User: "root",
				Roles: []string{host.RoleClusterWorker}},
		},
		SetupState: &setup.SetupState{ZotInstalled: true},
	}
}

// This is the regression test for the bug where --all-nodes reused a single
// control-plane connection for every host, so workers were never touched.
func TestInstallK3sComponent_AllNodesConnectsToEachHost(t *testing.T) {
	harness := newK3sTestHarness()
	cfg := k3sTestConfig()

	err := installK3sComponent(context.Background(), cfg, harness.connector(), false, true)
	require.NoError(t, err)

	assert.Equal(t, []string{"cp1", "worker1", "worker2"}, harness.dialed,
		"--all-nodes must open a connection to every cluster node")
	assert.Equal(t, 3, harness.closed, "every connection must be closed")

	for _, name := range []string{"cp1", "worker1", "worker2"} {
		exec, ok := harness.executors[name]
		require.True(t, ok, "expected an executor for %s", name)
		assert.True(t, exec.wroteRegistriesConfig(),
			"registries.yaml should be written on %s", name)
	}
}

func TestInstallK3sComponent_DefaultTargetsOnlyControlPlane(t *testing.T) {
	harness := newK3sTestHarness()
	cfg := k3sTestConfig()

	err := installK3sComponent(context.Background(), cfg, harness.connector(), false, false)
	require.NoError(t, err)

	assert.Equal(t, []string{"cp1"}, harness.dialed,
		"without --all-nodes only the first control plane node is reconciled")
	assert.NotContains(t, harness.executors, "worker1")
}

func TestInstallK3sComponent_DryRunOpensNoConnections(t *testing.T) {
	harness := newK3sTestHarness()
	cfg := k3sTestConfig()

	err := installK3sComponent(context.Background(), cfg, harness.connector(), true, true)
	require.NoError(t, err)

	assert.Empty(t, harness.dialed, "dry-run must not open SSH connections")
}

func TestInstallK3sComponent_NoClusterHosts(t *testing.T) {
	harness := newK3sTestHarness()
	cfg := &config.Config{
		Cluster: config.ClusterConfig{Name: "empty"},
		Hosts:   []*host.Host{},
	}

	err := installK3sComponent(context.Background(), cfg, harness.connector(), false, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no cluster hosts configured")
}

func TestInstallK3sComponent_ConnectFailureStopsWithHostContext(t *testing.T) {
	harness := newK3sTestHarness()
	harness.connectErrOn = "worker1"
	cfg := k3sTestConfig()

	err := installK3sComponent(context.Background(), cfg, harness.connector(), false, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker1", "error should name the failing node")

	// worker2 must not be dialed after worker1 fails.
	assert.Equal(t, []string{"cp1", "worker1"}, harness.dialed)
}

func TestInstallK3sComponent_ErrorsWhenK3sNotInstalledOnNode(t *testing.T) {
	harness := newK3sTestHarness()
	harness.template = func(e *fakeK3sExecutor) { e.k3sActive = false }
	cfg := k3sTestConfig()

	err := installK3sComponent(context.Background(), cfg, harness.connector(), false, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "k3s is not installed")
	assert.Contains(t, err.Error(), "cp1")
}

// Idempotency at the command layer: a node already holding the desired
// registries.yaml must not be restarted.
func TestInstallK3sComponent_SkipsRestartWhenConfigUnchanged(t *testing.T) {
	cfg := k3sTestConfig()

	// First pass: capture the config that gets written.
	first := newK3sTestHarness()
	require.NoError(t, installK3sComponent(context.Background(), cfg, first.connector(), false, false))
	require.True(t, first.executors["cp1"].restartedK3s(),
		"a node with no matching config should be restarted")

	// Derive the desired registries.yaml the same way production does.
	desired := &k3s.Config{}
	k3s.PopulateRegistryConfig(desired, "192.168.1.10")
	require.NotEmpty(t, desired.RegistryConfig)

	// Second pass: node already has that exact config.
	second := newK3sTestHarness()
	second.template = func(e *fakeK3sExecutor) {
		e.existingRegistries = desired.RegistryConfig
		e.existingNetwork = k3s.GenerateNetworkConfigYAML(&k3s.Config{NodeIP: "192.168.1.10", FlannelIface: "eth0", AdvertiseAddress: "192.168.1.10"}, true)
	}
	require.NoError(t, installK3sComponent(context.Background(), cfg, second.connector(), false, false))

	assert.False(t, second.executors["cp1"].restartedK3s(),
		"k3s must not restart when registries.yaml is unchanged")
}

func TestBuildK3sNodeConfigSeparatesVIPAndNodeIP(t *testing.T) {
	cfg := k3sTestConfig()

	k3sConfig := buildK3sNodeConfig(cfg, cfg.Hosts[0])

	assert.Empty(t, k3sConfig.Interface,
		"legacy kube-vip Interface must not be populated from an address")
	assert.Equal(t, "192.168.1.10", k3sConfig.NodeIP)
	assert.Equal(t, "192.168.1.10", k3sConfig.AdvertiseAddress)
	assert.Equal(t, "192.168.1.100", k3sConfig.VIP)
	assert.NotEmpty(t, k3sConfig.RegistryConfig,
		"a reachable Zot host should populate registries.yaml")
}

func TestReconcileK3sNode_WarnsWhenZotAddressUnresolvable(t *testing.T) {
	cfg := k3sTestConfig()
	// Zot marked installed, but no host carries the zot role.
	cfg.Hosts[0].Roles = []string{host.RoleClusterControlPlane}

	harness := newK3sTestHarness()

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := installK3sComponent(context.Background(), cfg, harness.connector(), false, false)

	w.Close()
	os.Stdout = oldStdout
	var buf bytes.Buffer
	io.Copy(&buf, r)
	output := buf.String()

	require.NoError(t, err)
	assert.Contains(t, output, "Zot is installed but its address could not be resolved")
	assert.False(t, harness.executors["cp1"].wroteRegistriesConfig(),
		"no registries.yaml should be written without a Zot address")
}

func TestK3sClusterHosts_DoesNotMutateControlPlaneSlice(t *testing.T) {
	cfg := k3sTestConfig()

	cpHosts := cfg.GetClusterControlPlaneHosts()
	require.Len(t, cpHosts, 1)
	before := cpHosts[0].Hostname

	all := k3sClusterHosts(cfg)
	require.Len(t, all, 3)

	assert.Equal(t, before, cfg.GetClusterControlPlaneHosts()[0].Hostname,
		"building the full host list must not clobber the control plane slice")
}
