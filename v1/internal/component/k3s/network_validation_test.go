package k3s

import (
	"strings"
	"testing"

	"github.com/catalystcommunity/foundry/v1/internal/ssh"
	"github.com/stretchr/testify/require"
)

func TestResolveNodeNetworkWithSecondaryVIP(t *testing.T) {
	exec := &mockInstallSSHExecutor{execFunc: func(command string) (*ssh.ExecResult, error) {
		require.Contains(t, command, "ip -o -4 addr show")
		return &ssh.ExecResult{ExitCode: 0, Stdout: "enp1s0\n"}, nil
	}}
	cfg := &Config{VIP: "10.0.0.11", NodeIP: "192.168.1.185"}
	require.NoError(t, ResolveNodeNetwork(exec, cfg))
	require.Equal(t, "enp1s0", cfg.FlannelIface)
	require.Equal(t, "192.168.1.185", cfg.AdvertiseAddress)
	content := GenerateNetworkConfigYAML(cfg, true)
	require.Contains(t, content, "node-ip: 192.168.1.185")
	require.Contains(t, content, "flannel-iface: enp1s0")
	require.Contains(t, content, "advertise-address: 192.168.1.185")
	require.NotContains(t, content, "10.0.0.11")
}

func TestValidateFlannelPublicIPs(t *testing.T) {
	control := &mockInstallSSHExecutor{execFunc: func(command string) (*ssh.ExecResult, error) {
		if strings.Contains(command, "blue1") {
			return &ssh.ExecResult{ExitCode: 0, Stdout: "192.168.1.185"}, nil
		}
		return &ssh.ExecResult{ExitCode: 0, Stdout: "192.168.1.253"}, nil
	}}
	peer := &mockInstallSSHExecutor{execFunc: func(command string) (*ssh.ExecResult, error) {
		require.True(t, strings.HasPrefix(command, "ping -c 1 -W 2 192.168.1."))
		return &ssh.ExecResult{ExitCode: 0}, nil
	}}
	nodes := []FlannelNode{{Name: "blue1", NodeIP: "192.168.1.185", Peer: peer}, {Name: "refurb", NodeIP: "192.168.1.253", Peer: peer}}
	require.NoError(t, ValidateFlannelPublicIPs(control, nodes, "10.0.0.11"))
}

func TestValidateFlannelRejectsVIP(t *testing.T) {
	control := &mockInstallSSHExecutor{execFunc: func(string) (*ssh.ExecResult, error) {
		return &ssh.ExecResult{ExitCode: 0, Stdout: "10.0.0.11"}, nil
	}}
	err := ValidateFlannelPublicIPs(control, []FlannelNode{{Name: "blue1", NodeIP: "192.168.1.185"}}, "10.0.0.11")
	require.ErrorContains(t, err, "advertises API VIP")
}

func TestValidateFlannelRejectsUnreachableEndpoint(t *testing.T) {
	control := &mockInstallSSHExecutor{execFunc: func(string) (*ssh.ExecResult, error) {
		return &ssh.ExecResult{ExitCode: 0, Stdout: "192.168.1.185"}, nil
	}}
	peer := &mockInstallSSHExecutor{execFunc: func(string) (*ssh.ExecResult, error) { return &ssh.ExecResult{ExitCode: 1}, nil }}
	err := ValidateFlannelPublicIPs(control, []FlannelNode{{Name: "blue1", NodeIP: "192.168.1.185", Peer: peer}, {Name: "blue2", NodeIP: "192.168.1.97", Peer: peer}}, "10.0.0.11")
	require.ErrorContains(t, err, "unreachable")
}
