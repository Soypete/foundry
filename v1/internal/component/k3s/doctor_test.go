package k3s

import (
	"strings"
	"testing"

	"github.com/catalystcommunity/foundry/v1/internal/ssh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiagnoseNodeNetwork(t *testing.T) {
	t.Run("reports the VIP sharing the Flannel interface", func(t *testing.T) {
		exec := &mockInstallSSHExecutor{execFunc: func(command string) (*ssh.ExecResult, error) {
			// blue1's state: node address and the floating VIP on one NIC.
			return &ssh.ExecResult{Stdout: "192.168.1.185/24\n10.0.0.11/32\n", ExitCode: 0}, nil
		}}

		findings := DiagnoseNodeNetwork(exec, NodeDiagnosis{
			Name: "blue1", NodeIP: "192.168.1.185", FlannelIface: "enp1s0",
		}, "10.0.0.11")

		require.Len(t, findings, 1)
		assert.Equal(t, "vip-on-flannel-interface", findings[0].Check)
		assert.Equal(t, "blue1", findings[0].Node)
		assert.True(t, findings[0].Fixable)
		assert.Contains(t, findings[0].Summary, "enp1s0")
		assert.Contains(t, findings[0].Summary, "10.0.0.11")
	})

	t.Run("is quiet when the interface carries only the node address", func(t *testing.T) {
		exec := &mockInstallSSHExecutor{execFunc: func(command string) (*ssh.ExecResult, error) {
			return &ssh.ExecResult{Stdout: "192.168.1.185/24\n", ExitCode: 0}, nil
		}}

		findings := DiagnoseNodeNetwork(exec, NodeDiagnosis{
			Name: "blue1", NodeIP: "192.168.1.185", FlannelIface: "enp1s0",
		}, "10.0.0.11")

		assert.Empty(t, findings)
	})

	t.Run("reports a node_ip that is the VIP", func(t *testing.T) {
		exec := &mockInstallSSHExecutor{execFunc: func(command string) (*ssh.ExecResult, error) {
			return &ssh.ExecResult{Stdout: "10.0.0.11/32\n", ExitCode: 0}, nil
		}}

		findings := DiagnoseNodeNetwork(exec, NodeDiagnosis{
			Name: "blue1", NodeIP: "10.0.0.11", FlannelIface: "enp1s0",
		}, "10.0.0.11")

		require.NotEmpty(t, findings)
		assert.Equal(t, "flannel-endpoint", findings[0].Check)
		assert.Contains(t, findings[0].Summary, "the API VIP")
	})
}

func TestDiagnoseFlannelEndpoints(t *testing.T) {
	t.Run("reports a node advertising the VIP", func(t *testing.T) {
		exec := &mockInstallSSHExecutor{execFunc: func(command string) (*ssh.ExecResult, error) {
			return &ssh.ExecResult{Stdout: "10.0.0.11", ExitCode: 0}, nil
		}}

		findings := DiagnoseFlannelEndpoints(exec,
			[]FlannelNode{{Name: "blue1", NodeIP: "192.168.1.185"}}, "10.0.0.11")

		require.Len(t, findings, 1)
		assert.True(t, findings[0].Fixable)
		assert.Contains(t, findings[0].Summary, "advertises the API VIP")
	})

	t.Run("reports drift from the configured address", func(t *testing.T) {
		exec := &mockInstallSSHExecutor{execFunc: func(command string) (*ssh.ExecResult, error) {
			return &ssh.ExecResult{Stdout: "192.168.1.99", ExitCode: 0}, nil
		}}

		findings := DiagnoseFlannelEndpoints(exec,
			[]FlannelNode{{Name: "blue1", NodeIP: "192.168.1.185"}}, "10.0.0.11")

		require.Len(t, findings, 1)
		assert.Contains(t, findings[0].Summary, "192.168.1.99")
		assert.Contains(t, findings[0].Summary, "192.168.1.185")
	})

	t.Run("is quiet on a healthy cluster", func(t *testing.T) {
		// The state blue1 was restored to.
		exec := &mockInstallSSHExecutor{execFunc: func(command string) (*ssh.ExecResult, error) {
			switch {
			case strings.Contains(command, "blue1"):
				return &ssh.ExecResult{Stdout: "192.168.1.185", ExitCode: 0}, nil
			case strings.Contains(command, "blue2"):
				return &ssh.ExecResult{Stdout: "192.168.1.97", ExitCode: 0}, nil
			}
			return &ssh.ExecResult{Stdout: "192.168.1.253", ExitCode: 0}, nil
		}}

		findings := DiagnoseFlannelEndpoints(exec, []FlannelNode{
			{Name: "blue1", NodeIP: "192.168.1.185"},
			{Name: "blue2", NodeIP: "192.168.1.97"},
			{Name: "refurb", NodeIP: "192.168.1.253"},
		}, "10.0.0.11")

		assert.Empty(t, findings)
	})
}

func TestDiagnoseStaleKubeVIP(t *testing.T) {
	installed := &mockInstallSSHExecutor{execFunc: func(command string) (*ssh.ExecResult, error) {
		return &ssh.ExecResult{Stdout: "kube-vip", ExitCode: 0}, nil
	}}
	absent := &mockInstallSSHExecutor{execFunc: func(command string) (*ssh.ExecResult, error) {
		return &ssh.ExecResult{ExitCode: 1}, nil
	}}

	t.Run("reports kube-vip on a single control plane cluster", func(t *testing.T) {
		findings := DiagnoseStaleKubeVIP(installed, false, "10.0.0.11")
		require.Len(t, findings, 1)
		assert.Equal(t, "kube-vip", findings[0].Check)
		assert.True(t, findings[0].Fixable)
		assert.Contains(t, findings[0].Remedy, "delete daemonset")
	})

	t.Run("is quiet when the VIP is legitimately deployed", func(t *testing.T) {
		assert.Empty(t, DiagnoseStaleKubeVIP(installed, true, "10.0.0.11"))
	})

	t.Run("is quiet when kube-vip is already gone", func(t *testing.T) {
		assert.Empty(t, DiagnoseStaleKubeVIP(absent, false, "10.0.0.11"))
	})
}

func TestDiagnoseStaleVIPReferences(t *testing.T) {
	t.Run("reports a Service still publishing the VIP", func(t *testing.T) {
		exec := &mockInstallSSHExecutor{execFunc: func(command string) (*ssh.ExecResult, error) {
			return &ssh.ExecResult{Stdout: "projectcontour/contour-envoy [\"10.0.0.11\"]\n", ExitCode: 0}, nil
		}}

		findings := DiagnoseStaleVIPReferences(exec, "10.0.0.11")
		require.Len(t, findings, 1)
		assert.Contains(t, findings[0].Summary, "projectcontour/contour-envoy")
		assert.False(t, findings[0].Fixable, "editing someone's Service needs an operator decision")
	})

	t.Run("is quiet with no VIP configured", func(t *testing.T) {
		assert.Empty(t, DiagnoseStaleVIPReferences(&mockInstallSSHExecutor{}, ""))
	})
}

func TestRemoveKubeVIP(t *testing.T) {
	var commands []string
	exec := &mockInstallSSHExecutor{execFunc: func(command string) (*ssh.ExecResult, error) {
		commands = append(commands, command)
		return &ssh.ExecResult{ExitCode: 0}, nil
	}}

	require.NoError(t, RemoveKubeVIP(exec))

	joined := strings.Join(commands, "\n")
	assert.Contains(t, joined, "delete daemonset -n kube-system kube-vip")
	// The address belongs to kube-vip, which releases it on pod teardown.
	assert.NotContains(t, joined, "ip addr del",
		"Foundry must never edit the interface directly")
}

func TestFindingString(t *testing.T) {
	f := Finding{
		Check:   "kube-vip",
		Node:    "blue1",
		Summary: "kube-vip is deployed",
		Detail:  "It provides no failover here.",
		Remedy:  "kubectl delete daemonset kube-vip",
	}
	rendered := f.String()
	assert.Contains(t, rendered, "blue1")
	assert.Contains(t, rendered, "kube-vip is deployed")
	assert.Contains(t, rendered, "fix: kubectl delete")
}
