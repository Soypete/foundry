package network

import (
	"testing"

	"github.com/catalystcommunity/foundry/v1/internal/host"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewSubstrate(t *testing.T) {
	t.Run("an empty name is the LAN", func(t *testing.T) {
		s, err := NewSubstrate("")
		require.NoError(t, err)
		assert.Equal(t, SubstrateLAN, s.Name)
		assert.False(t, s.IsTailscale())
	})

	t.Run("an explicit lan matches an empty name", func(t *testing.T) {
		empty, err := NewSubstrate("")
		require.NoError(t, err)
		explicit, err := NewSubstrate(SubstrateLAN)
		require.NoError(t, err)
		assert.Equal(t, empty, explicit)
	})

	t.Run("tailscale is recognised", func(t *testing.T) {
		s, err := NewSubstrate(SubstrateTailscale)
		require.NoError(t, err)
		assert.True(t, s.IsTailscale())
	})

	t.Run("an unknown substrate lists the valid ones", func(t *testing.T) {
		_, err := NewSubstrate("wireguard")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wireguard")
		assert.Contains(t, err.Error(), "lan")
		assert.Contains(t, err.Error(), "tailscale")
	})
}

func TestSubstrateNodeAddress(t *testing.T) {
	lan, err := NewSubstrate(SubstrateLAN)
	require.NoError(t, err)
	ts, err := NewSubstrate(SubstrateTailscale)
	require.NoError(t, err)

	t.Run("lan uses node_ip", func(t *testing.T) {
		addr, err := lan.NodeAddress(&host.Host{
			Hostname: "blue1", Address: "192.168.1.185", NodeIP: "192.168.1.185",
		})
		require.NoError(t, err)
		assert.Equal(t, "192.168.1.185", addr)
	})

	t.Run("lan keeps refusing an implicit CGNAT address", func(t *testing.T) {
		_, err := lan.NodeAddress(&host.Host{Hostname: "blue1", Address: "100.81.89.62"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "100.64.0.0/10")
	})

	t.Run("tailscale uses tailscale_address", func(t *testing.T) {
		addr, err := ts.NodeAddress(&host.Host{
			Hostname: "blue1", Address: "192.168.1.185", TailscaleAddress: "100.81.89.62",
		})
		require.NoError(t, err)
		assert.Equal(t, "100.81.89.62", addr)
	})

	t.Run("tailscale requires tailscale_address", func(t *testing.T) {
		_, err := ts.NodeAddress(&host.Host{Hostname: "blue1", Address: "192.168.1.185"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "blue1")
		assert.Contains(t, err.Error(), "tailscale_address")
	})
}

func TestSubstrateValidate(t *testing.T) {
	lan, err := NewSubstrate(SubstrateLAN)
	require.NoError(t, err)
	ts, err := NewSubstrate(SubstrateTailscale)
	require.NoError(t, err)

	t.Run("rejects the API VIP on either substrate", func(t *testing.T) {
		err := lan.Validate(&host.Host{
			Hostname: "blue1", Address: "192.168.1.185", NodeIP: "10.0.0.11",
		}, "10.0.0.11")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is the API VIP")
	})

	t.Run("lan points at the substrate switch when it sees CGNAT", func(t *testing.T) {
		err := lan.Validate(&host.Host{
			Hostname: "blue1", Address: "192.168.1.185", NodeIP: "100.81.89.62",
		}, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "100.64.0.0/10")
		assert.Contains(t, err.Error(), "network_substrate")
	})

	t.Run("tailscale accepts a tailnet address", func(t *testing.T) {
		require.NoError(t, ts.Validate(&host.Host{
			Hostname: "blue1", Address: "192.168.1.185", TailscaleAddress: "100.81.89.62",
		}, "10.0.0.11"))
	})

	t.Run("tailscale requires the address to actually be on the tailnet", func(t *testing.T) {
		err := ts.Validate(&host.Host{
			Hostname: "blue1", Address: "192.168.1.185", TailscaleAddress: "192.168.1.185",
		}, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not in the Tailscale CGNAT range")
	})

	t.Run("tailscale rejects a node_ip that disagrees", func(t *testing.T) {
		err := ts.Validate(&host.Host{
			Hostname: "blue1", Address: "192.168.1.185",
			TailscaleAddress: "100.81.89.62", NodeIP: "192.168.1.185",
		}, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must equal tailscale_address")
	})

	t.Run("tailscale accepts a node_ip that agrees", func(t *testing.T) {
		require.NoError(t, ts.Validate(&host.Host{
			Hostname: "blue1", Address: "192.168.1.185",
			TailscaleAddress: "100.81.89.62", NodeIP: "100.81.89.62",
		}, ""))
	})
}

func TestSubstrateDefaultInterfaceAndMTU(t *testing.T) {
	lan, err := NewSubstrate(SubstrateLAN)
	require.NoError(t, err)
	ts, err := NewSubstrate(SubstrateTailscale)
	require.NoError(t, err)

	// The LAN interface is discovered per host from the address that owns it,
	// and Flannel's own MTU detection is correct there.
	assert.Empty(t, lan.DefaultInterface())
	assert.Zero(t, lan.PodMTU(1500))

	assert.Equal(t, TailscaleInterface, ts.DefaultInterface())
	assert.Equal(t, 1230, ts.PodMTU(TailscaleMTU))
	assert.Equal(t, 1230, ts.PodMTU(0), "an undetectable MTU falls back to Tailscale's 1280")
	assert.Equal(t, 1450, ts.PodMTU(1500), "the MTU is computed, not assumed")
}
