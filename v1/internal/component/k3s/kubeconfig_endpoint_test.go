package k3s

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/catalystcommunity/foundry/v1/internal/ssh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Synthetic addresses used throughout. They mirror the shape of a real
// deployment — LAN node address, CGNAT/Tailscale management address, and an
// API VIP on a third network — without being any real host.
const (
	testLANAddress       = "192.168.1.185"
	testTailscaleAddress = "100.81.89.62"
	testVIP              = "10.0.0.11"
)

// syntheticKubeconfig renders a minimal but structurally realistic kubeconfig
// whose single cluster points at server.
func syntheticKubeconfig(server string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: QUJD
    server: %s
  name: default
contexts:
- context:
    cluster: default
    user: default
  name: default
current-context: default
users:
- name: default
  user:
    token: synthetic-token
`, server)
}

func TestClientEndpoint(t *testing.T) {
	const testNodeIP = "192.168.1.185"

	tests := []struct {
		name             string
		tailscaleAddress string
		vip              string
		nodeIP           string
		want             string
	}{
		{
			name:             "tailscale address wins over VIP",
			tailscaleAddress: testTailscaleAddress,
			vip:              testVIP,
			nodeIP:           testNodeIP,
			want:             testTailscaleAddress,
		},
		{
			name:             "falls back to VIP when no tailscale address",
			tailscaleAddress: "",
			vip:              testVIP,
			nodeIP:           testNodeIP,
			want:             testVIP,
		},
		{
			name:             "tailscale address used even with no VIP",
			tailscaleAddress: testTailscaleAddress,
			vip:              "",
			nodeIP:           testNodeIP,
			want:             testTailscaleAddress,
		},
		{
			name:             "falls back to the node address for a VIP-less cluster",
			tailscaleAddress: "",
			vip:              "",
			nodeIP:           testNodeIP,
			want:             testNodeIP,
		},
		{
			name:             "empty when nothing is configured",
			tailscaleAddress: "",
			vip:              "",
			nodeIP:           "",
			want:             "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ClientEndpoint(tt.tailscaleAddress, tt.vip, tt.nodeIP))
		})
	}
}

func TestKubeconfigServerURL(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want string
	}{
		{"IPv4 LAN address", testLANAddress, "https://192.168.1.185:6443"},
		{"CGNAT tailscale address", testTailscaleAddress, "https://100.81.89.62:6443"},
		{"hostname", "k8s.example.local", "https://k8s.example.local:6443"},
		{"IPv6 literal is bracketed", "fd7a:115c:a1e0::1", "https://[fd7a:115c:a1e0::1]:6443"},
		{"already-bracketed IPv6 is not double-bracketed", "[fd7a:115c:a1e0::1]", "https://[fd7a:115c:a1e0::1]:6443"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, KubeconfigServerURL(tt.addr))
		})
	}
}

// TestModifyKubeconfigServerRederivesEndpoint covers rewriting endpoints that
// are not K3s's 127.0.0.1 default. TestModifyKubeconfigServer in
// kubeconfig_test.go covers the default-substitution cases.
func TestModifyKubeconfigServerRederivesEndpoint(t *testing.T) {
	t.Run("rewrites the K3s 127.0.0.1 default", func(t *testing.T) {
		got := ModifyKubeconfigServer(syntheticKubeconfig("https://127.0.0.1:6443"), testTailscaleAddress)
		assert.Contains(t, got, "server: https://100.81.89.62:6443")
		assert.NotContains(t, got, "127.0.0.1")
	})

	// The regression this whole change exists for: the previous implementation
	// substituted only the literal 127.0.0.1 default, so a kubeconfig already
	// rewritten to a LAN address silently survived a re-fetch unchanged.
	t.Run("rewrites an already-rewritten LAN endpoint", func(t *testing.T) {
		got := ModifyKubeconfigServer(syntheticKubeconfig("https://192.168.1.185:6443"), testTailscaleAddress)
		assert.Contains(t, got, "server: https://100.81.89.62:6443")
		assert.NotContains(t, got, testLANAddress)
	})

	t.Run("rewrites a VIP endpoint", func(t *testing.T) {
		got := ModifyKubeconfigServer(syntheticKubeconfig("https://10.0.0.11:6443"), testTailscaleAddress)
		assert.Contains(t, got, "server: https://100.81.89.62:6443")
		assert.NotContains(t, got, testVIP)
	})

	t.Run("preserves surrounding structure and indentation", func(t *testing.T) {
		got := ModifyKubeconfigServer(syntheticKubeconfig("https://127.0.0.1:6443"), testTailscaleAddress)
		assert.Contains(t, got, "    server: https://100.81.89.62:6443")
		assert.Contains(t, got, "certificate-authority-data: QUJD")
		assert.Contains(t, got, "current-context: default")
		assert.Contains(t, got, "token: synthetic-token")
	})

	t.Run("rewrites every cluster entry", func(t *testing.T) {
		multi := `apiVersion: v1
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: a
- cluster:
    server: https://192.168.1.185:6443
  name: b
`
		got := ModifyKubeconfigServer(multi, testTailscaleAddress)
		assert.Equal(t, 2, strings.Count(got, "server: https://100.81.89.62:6443"))
	})

	t.Run("empty address leaves kubeconfig untouched", func(t *testing.T) {
		original := syntheticKubeconfig("https://127.0.0.1:6443")
		assert.Equal(t, original, ModifyKubeconfigServer(original, ""))
	})

	t.Run("is idempotent", func(t *testing.T) {
		once := ModifyKubeconfigServer(syntheticKubeconfig("https://127.0.0.1:6443"), testTailscaleAddress)
		twice := ModifyKubeconfigServer(once, testTailscaleAddress)
		assert.Equal(t, once, twice)
	})
}

func TestKubeconfigServerAddresses(t *testing.T) {
	t.Run("extracts a single server", func(t *testing.T) {
		got := KubeconfigServerAddresses(syntheticKubeconfig("https://10.0.0.11:6443"))
		assert.Equal(t, []string{"https://10.0.0.11:6443"}, got)
	})

	t.Run("extracts every server", func(t *testing.T) {
		multi := "clusters:\n- cluster:\n    server: https://a:6443\n- cluster:\n    server: https://b:6443\n"
		assert.Equal(t, []string{"https://a:6443", "https://b:6443"}, KubeconfigServerAddresses(multi))
	})

	t.Run("returns empty for a kubeconfig with no server entry", func(t *testing.T) {
		assert.Empty(t, KubeconfigServerAddresses("apiVersion: v1\nkind: Config\n"))
	})
}

func TestKubeconfigTargets(t *testing.T) {
	tests := []struct {
		name       string
		kubeconfig string
		addr       string
		want       bool
	}{
		{
			name:       "already targets the address",
			kubeconfig: syntheticKubeconfig("https://100.81.89.62:6443"),
			addr:       testTailscaleAddress,
			want:       true,
		},
		{
			name:       "points at the LAN address instead",
			kubeconfig: syntheticKubeconfig("https://192.168.1.185:6443"),
			addr:       testTailscaleAddress,
			want:       false,
		},
		{
			name:       "points at the VIP instead",
			kubeconfig: syntheticKubeconfig("https://10.0.0.11:6443"),
			addr:       testTailscaleAddress,
			want:       false,
		},
		{
			name:       "no server entry is not converged",
			kubeconfig: "apiVersion: v1\nkind: Config\n",
			addr:       testTailscaleAddress,
			want:       false,
		},
		{
			name:       "mixed servers are not converged",
			kubeconfig: "clusters:\n- cluster:\n    server: https://100.81.89.62:6443\n- cluster:\n    server: https://192.168.1.185:6443\n",
			addr:       testTailscaleAddress,
			want:       false,
		},
		{
			name:       "all entries converged",
			kubeconfig: "clusters:\n- cluster:\n    server: https://100.81.89.62:6443\n- cluster:\n    server: https://100.81.89.62:6443\n",
			addr:       testTailscaleAddress,
			want:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, KubeconfigTargets(tt.kubeconfig, tt.addr))
		})
	}
}

func TestRefreshStoredKubeconfig(t *testing.T) {
	// The headline repair case: a kubeconfig written before the control plane
	// had a Tailscale address, still pointing at the LAN.
	t.Run("repairs a stale LAN endpoint", func(t *testing.T) {
		var stored string
		client := &mockKubeconfigClient{
			readFunc: func(ctx context.Context, mount, path string) (map[string]interface{}, error) {
				return map[string]interface{}{"kubeconfig": syntheticKubeconfig("https://192.168.1.185:6443")}, nil
			},
			writeFunc: func(ctx context.Context, mount, path string, data map[string]interface{}) error {
				stored = data["kubeconfig"].(string)
				return nil
			},
		}

		changed, err := RefreshStoredKubeconfig(context.Background(), client, testTailscaleAddress)
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Contains(t, stored, "server: https://100.81.89.62:6443")
		assert.NotContains(t, stored, testLANAddress)
	})

	t.Run("no-op when already converged", func(t *testing.T) {
		wrote := false
		client := &mockKubeconfigClient{
			readFunc: func(ctx context.Context, mount, path string) (map[string]interface{}, error) {
				return map[string]interface{}{"kubeconfig": syntheticKubeconfig("https://100.81.89.62:6443")}, nil
			},
			writeFunc: func(ctx context.Context, mount, path string, data map[string]interface{}) error {
				wrote = true
				return nil
			},
		}

		changed, err := RefreshStoredKubeconfig(context.Background(), client, testTailscaleAddress)
		require.NoError(t, err)
		assert.False(t, changed, "converged kubeconfig should not be rewritten")
		assert.False(t, wrote, "no write should be issued when nothing changes")
	})

	// Running the repair twice must converge: the second run reports no change.
	t.Run("is idempotent across runs", func(t *testing.T) {
		stored := syntheticKubeconfig("https://192.168.1.185:6443")
		writes := 0
		client := &mockKubeconfigClient{
			readFunc: func(ctx context.Context, mount, path string) (map[string]interface{}, error) {
				return map[string]interface{}{"kubeconfig": stored}, nil
			},
			writeFunc: func(ctx context.Context, mount, path string, data map[string]interface{}) error {
				stored = data["kubeconfig"].(string)
				writes++
				return nil
			},
		}

		first, err := RefreshStoredKubeconfig(context.Background(), client, testTailscaleAddress)
		require.NoError(t, err)
		assert.True(t, first)

		second, err := RefreshStoredKubeconfig(context.Background(), client, testTailscaleAddress)
		require.NoError(t, err)
		assert.False(t, second, "second run must be a no-op")
		assert.Equal(t, 1, writes, "only the first run should write")
	})

	t.Run("errors on empty address", func(t *testing.T) {
		changed, err := RefreshStoredKubeconfig(context.Background(), &mockKubeconfigClient{}, "")
		require.Error(t, err)
		assert.False(t, changed)
		assert.Contains(t, err.Error(), "cannot be empty")
	})

	t.Run("propagates a load failure", func(t *testing.T) {
		client := &mockKubeconfigClient{
			readFunc: func(ctx context.Context, mount, path string) (map[string]interface{}, error) {
				return nil, fmt.Errorf("openbao unreachable")
			},
		}
		changed, err := RefreshStoredKubeconfig(context.Background(), client, testTailscaleAddress)
		require.Error(t, err)
		assert.False(t, changed)
		assert.Contains(t, err.Error(), "openbao unreachable")
	})

	t.Run("errors when kubeconfig has no server entry", func(t *testing.T) {
		client := &mockKubeconfigClient{
			readFunc: func(ctx context.Context, mount, path string) (map[string]interface{}, error) {
				return map[string]interface{}{"kubeconfig": "apiVersion: v1\nkind: Config\n"}, nil
			},
		}
		changed, err := RefreshStoredKubeconfig(context.Background(), client, testTailscaleAddress)
		require.Error(t, err)
		assert.False(t, changed)
		assert.Contains(t, err.Error(), "no server entry")
	})

	t.Run("propagates a store failure", func(t *testing.T) {
		client := &mockKubeconfigClient{
			readFunc: func(ctx context.Context, mount, path string) (map[string]interface{}, error) {
				return map[string]interface{}{"kubeconfig": syntheticKubeconfig("https://192.168.1.185:6443")}, nil
			},
			writeFunc: func(ctx context.Context, mount, path string, data map[string]interface{}) error {
				return fmt.Errorf("openbao read-only")
			},
		}
		changed, err := RefreshStoredKubeconfig(context.Background(), client, testTailscaleAddress)
		require.Error(t, err)
		assert.False(t, changed)
		assert.Contains(t, err.Error(), "openbao read-only")
	})
}

// TestRetrieveAndStoreKubeconfigUsesClientEndpoint covers the provisioning path
// end-to-end over synthetic SSH + OpenBAO doubles, asserting that the endpoint
// selection rule and the rewrite compose correctly.
func TestRetrieveAndStoreKubeconfigUsesClientEndpoint(t *testing.T) {
	tests := []struct {
		name             string
		tailscaleAddress string
		wantServer       string
	}{
		{
			name:             "control plane with tailscale address",
			tailscaleAddress: testTailscaleAddress,
			wantServer:       "server: https://100.81.89.62:6443",
		},
		{
			name:             "control plane without tailscale address falls back to VIP",
			tailscaleAddress: "",
			wantServer:       "server: https://10.0.0.11:6443",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := &mockKubeconfigSSHExecutor{
				execFunc: func(command string) (*ssh.ExecResult, error) {
					return &ssh.ExecResult{
						Stdout:   syntheticKubeconfig("https://127.0.0.1:6443"),
						ExitCode: 0,
					}, nil
				},
			}

			var stored string
			client := &mockKubeconfigClient{
				writeFunc: func(ctx context.Context, mount, path string, data map[string]interface{}) error {
					stored = data["kubeconfig"].(string)
					return nil
				},
			}

			endpoint := ClientEndpoint(tt.tailscaleAddress, testVIP, "")
			require.NoError(t, RetrieveAndStoreKubeconfig(context.Background(), executor, client, endpoint))
			assert.Contains(t, stored, tt.wantServer)
			assert.NotContains(t, stored, "127.0.0.1")
		})
	}
}
