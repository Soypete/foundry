package network

import (
	"fmt"

	"github.com/catalystcommunity/foundry/v1/internal/host"
)

const (
	// SubstrateLAN carries Flannel's VXLAN endpoints on the physical LAN. This
	// is the default, and what an empty network_substrate means.
	SubstrateLAN = "lan"

	// SubstrateTailscale carries them on the tailnet instead, using each host's
	// tailscale_address and the tailscale0 interface.
	SubstrateTailscale = "tailscale"

	// TailscaleInterface is the interface the Tailscale daemon creates.
	TailscaleInterface = "tailscale0"

	// TailscaleMTU is the MTU Tailscale gives its interface.
	TailscaleMTU = 1280

	// VXLANOverhead is the header size Flannel's VXLAN encapsulation adds, so
	// the pod MTU is the underlying interface's MTU minus this.
	VXLANOverhead = 50
)

// Substrate decides which network carries pod traffic between nodes.
//
// Flannel needs an address it can encapsulate to, and both planes can provide
// one. The LAN is the default because it is fast and its failure domain is a
// single switch. The tailnet is available for clusters whose nodes do not share
// a layer 2 segment, at the cost of double encapsulation (VXLAN inside
// WireGuard), a lower MTU, and a dependency on the tailnet being up before pod
// networking works.
type Substrate struct {
	Name string
}

// NewSubstrate returns the substrate named by the configuration. An empty name
// is the LAN, so an existing config keeps its behavior exactly.
func NewSubstrate(name string) (Substrate, error) {
	switch name {
	case "", SubstrateLAN:
		return Substrate{Name: SubstrateLAN}, nil
	case SubstrateTailscale:
		return Substrate{Name: SubstrateTailscale}, nil
	default:
		return Substrate{}, fmt.Errorf("network_substrate %q is not valid (expected %q or %q)",
			name, SubstrateLAN, SubstrateTailscale)
	}
}

// IsTailscale reports whether pod traffic rides the tailnet.
func (s Substrate) IsTailscale() bool {
	return s.Name == SubstrateTailscale
}

// NodeAddress returns the address Flannel should use as this host's VXLAN
// endpoint.
func (s Substrate) NodeAddress(h *host.Host) (string, error) {
	if h == nil {
		return "", fmt.Errorf("host is nil")
	}
	if !s.IsTailscale() {
		// K3sNodeIP carries the LAN rules, including the refusal to adopt a
		// CGNAT management address implicitly.
		return h.K3sNodeIP()
	}
	if h.TailscaleAddress == "" {
		return "", fmt.Errorf("host %s: network_substrate is %q but tailscale_address is not set; Flannel has no tailnet address to use as its endpoint",
			h.Hostname, SubstrateTailscale)
	}
	return h.TailscaleAddress, nil
}

// DefaultInterface returns the interface Flannel binds to when a host does not
// name one. The LAN is discovered per host from the address that owns it, so it
// has no fixed default.
func (s Substrate) DefaultInterface() string {
	if s.IsTailscale() {
		return TailscaleInterface
	}
	return ""
}

// PodMTU returns the MTU Flannel should use given the underlying interface's
// MTU, or zero to let Flannel derive it.
//
// On the LAN, Flannel's own detection is correct and Foundry stays out of it.
// On the tailnet it is not: tailscale0 is 1280 rather than 1500, and Flannel
// sizing for the wrong underlay produces the classic silent failure where small
// packets pass and large transfers stall.
func (s Substrate) PodMTU(interfaceMTU int) int {
	if !s.IsTailscale() {
		return 0
	}
	if interfaceMTU <= 0 {
		interfaceMTU = TailscaleMTU
	}
	return interfaceMTU - VXLANOverhead
}

// Validate checks that a host can serve as a node on this substrate.
//
// The tailscale mode does not relax the LAN guards, it swaps them: an address
// that must not be CGNAT on the LAN must be CGNAT here, and must agree with an
// explicitly configured node_ip.
func (s Substrate) Validate(h *host.Host, vip string) error {
	addr, err := s.NodeAddress(h)
	if err != nil {
		return err
	}

	if vip != "" && addr == vip {
		return fmt.Errorf("host %s node address %s is the API VIP; Flannel's endpoint must be an address the node exclusively owns, not a floating address that moves between control plane nodes",
			h.Hostname, addr)
	}

	if !s.IsTailscale() {
		if isCGNAT(addr) {
			return fmt.Errorf("host %s node IP %s is in the Tailscale/CGNAT range 100.64.0.0/10; Flannel's endpoint must be a physical address, not an overlay one (set cluster.network_substrate: %q to use the tailnet deliberately)",
				h.Hostname, addr, SubstrateTailscale)
		}
		return nil
	}

	if !isCGNAT(addr) {
		return fmt.Errorf("host %s tailscale_address %s is not in the Tailscale CGNAT range 100.64.0.0/10",
			h.Hostname, addr)
	}
	if h.NodeIP != "" && h.NodeIP != h.TailscaleAddress {
		return fmt.Errorf("host %s has node_ip %s but network_substrate is %q; node_ip must equal tailscale_address %s or be omitted",
			h.Hostname, h.NodeIP, SubstrateTailscale, h.TailscaleAddress)
	}
	return nil
}
