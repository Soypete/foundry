package network

import (
	"fmt"
	"net"
	"strings"

	"github.com/catalystcommunity/foundry/v1/internal/config"
)

// ValidateIPs validates IP addresses in the network configuration
func ValidateIPs(fullCfg *config.Config) error {
	if fullCfg == nil || fullCfg.Network == nil {
		return fmt.Errorf("network config is nil")
	}

	cfg := fullCfg.Network

	// Gateway and netmask are already validated by config.Validate()
	// But we can do additional checks here

	// Validate that the gateway and netmask describe a usable network. The
	// result is not used to constrain host addresses; see
	// ValidateFlannelEndpoints for what actually governs them.
	if _, err := GetNetworkCIDR(cfg.Gateway, cfg.Netmask); err != nil {
		return fmt.Errorf("failed to calculate network CIDR: %w", err)
	}

	return ValidateFlannelEndpoints(fullCfg)
}

// ValidateFlannelEndpoints checks that every host has an address Flannel can
// safely use as its VXLAN endpoint.
//
// The requirement is not that the address belongs to the LAN subnet. Subnet
// membership was only ever a proxy for the real property, and it rejects
// legitimate topologies such as a routed /32 or a second subnet. What Flannel
// actually needs is an address that:
//
//   - the node exclusively owns, so peers reach that node and no other;
//   - does not float, so it cannot move to a different machine; and
//   - is not an overlay address, so pod traffic is not encapsulated twice and
//     does not depend on the overlay being up.
//
// The API VIP fails the second test: kube-vip moves it between control plane
// nodes, so a peer sending pod traffic there follows the API server role rather
// than the node. A CGNAT address fails the third. See docs/network-planes.md.
func ValidateFlannelEndpoints(fullCfg *config.Config) error {
	if fullCfg == nil {
		return fmt.Errorf("config is nil")
	}

	vip := fullCfg.Cluster.VIP
	seen := make(map[string]string, len(fullCfg.Hosts))

	for _, h := range fullCfg.Hosts {
		// K3sNodeIP applies the CGNAT guard and reports why an address is not
		// usable, so the rule lives in one place.
		nodeIP, err := h.K3sNodeIP()
		if err != nil {
			// A host with no address at all is not necessarily a cluster
			// member; only a stated-but-unusable address is an error here.
			if h.Address == "" && h.NodeIP == "" {
				continue
			}
			return fmt.Errorf("host %s: %w", h.Hostname, err)
		}
		if nodeIP == "" {
			continue
		}

		if net.ParseIP(nodeIP) == nil {
			return fmt.Errorf("invalid IP address for host %s: %s", h.Hostname, nodeIP)
		}
		if vip != "" && nodeIP == vip {
			return fmt.Errorf("host %s node IP %s is the API VIP; Flannel's endpoint must be an address the node exclusively owns, not a floating address that moves between control plane nodes",
				h.Hostname, nodeIP)
		}
		if isCGNAT(nodeIP) {
			return fmt.Errorf("host %s node IP %s is in the Tailscale/CGNAT range 100.64.0.0/10; Flannel's endpoint must be a physical address, not an overlay one",
				h.Hostname, nodeIP)
		}
		if other, dup := seen[nodeIP]; dup {
			return fmt.Errorf("hosts %s and %s share node IP %s; each node's Flannel endpoint must be exclusively its own",
				other, h.Hostname, nodeIP)
		}
		seen[nodeIP] = h.Hostname
	}

	return nil
}

// isCGNAT reports whether an address is in the RFC 6598 shared address space
// (100.64.0.0/10) that Tailscale and other overlays use.
func isCGNAT(addr string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	_, cgnat, err := net.ParseCIDR("100.64.0.0/10")
	if err != nil {
		return false
	}
	return cgnat.Contains(ip)
}

// CheckReachability checks if the given IPs are reachable via ping
func CheckReachability(conn SSHExecutor, ips []string) error {
	if len(ips) == 0 {
		return nil
	}

	unreachable := []string{}
	for _, ip := range ips {
		// Use ping with count=1 and timeout=2 seconds
		cmd := fmt.Sprintf("ping -c 1 -W 2 %s > /dev/null 2>&1", ip)
		result, err := conn.Exec(cmd)
		if err != nil {
			return fmt.Errorf("failed to ping %s: %w", ip, err)
		}

		if result.ExitCode != 0 {
			unreachable = append(unreachable, ip)
		}
	}

	if len(unreachable) > 0 {
		return fmt.Errorf("unreachable IPs: %s", strings.Join(unreachable, ", "))
	}

	return nil
}

// CheckDHCPConflicts checks if any infrastructure IPs fall within the DHCP range
func CheckDHCPConflicts(fullCfg *config.Config) error {
	if fullCfg == nil || fullCfg.Network == nil {
		return fmt.Errorf("network config is nil")
	}

	cfg := fullCfg.Network

	// If no DHCP range is configured, no conflicts possible
	if cfg.DHCPRange == nil {
		return nil
	}

	dhcpStart := net.ParseIP(cfg.DHCPRange.Start)
	dhcpEnd := net.ParseIP(cfg.DHCPRange.End)

	if dhcpStart == nil || dhcpEnd == nil {
		return fmt.Errorf("invalid DHCP range")
	}

	// Collect all infrastructure IPs (from hosts and VIP)
	var allIPs []string
	if fullCfg.Cluster.VIP != "" {
		allIPs = append(allIPs, fullCfg.Cluster.VIP)
	}
	for _, h := range fullCfg.Hosts {
		if h.Address != "" {
			allIPs = append(allIPs, h.Address)
		}
	}

	conflicts := []string{}
	for _, ipStr := range allIPs {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}

		if isIPInRange(ip, dhcpStart, dhcpEnd) {
			conflicts = append(conflicts, ipStr)
		}
	}

	if len(conflicts) > 0 {
		return fmt.Errorf("infrastructure IPs within DHCP range (%s - %s): %s",
			cfg.DHCPRange.Start, cfg.DHCPRange.End, strings.Join(conflicts, ", "))
	}

	return nil
}

// ValidateDNSResolution validates that a hostname resolves to the expected IP
// This is used after PowerDNS is installed to verify DNS configuration
func ValidateDNSResolution(conn SSHExecutor, hostname string, expectedIP string) error {
	if hostname == "" {
		return fmt.Errorf("hostname is required")
	}

	if expectedIP == "" {
		return fmt.Errorf("expected IP is required")
	}

	// Use dig to query DNS (more reliable than nslookup)
	// If dig is not available, fall back to getent hosts
	cmd := fmt.Sprintf("dig +short %s || getent hosts %s | awk '{print $1}'", hostname, hostname)
	result, err := conn.Exec(cmd)
	if err != nil {
		return fmt.Errorf("failed to resolve %s: %w", hostname, err)
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to resolve %s: %s", hostname, result.Stderr)
	}

	resolvedIP := strings.TrimSpace(result.Stdout)
	if resolvedIP == "" {
		return fmt.Errorf("hostname %s did not resolve to any IP", hostname)
	}

	// Handle multiple IPs (take the first one)
	if strings.Contains(resolvedIP, "\n") {
		resolvedIP = strings.Split(resolvedIP, "\n")[0]
	}

	if resolvedIP != expectedIP {
		return fmt.Errorf("hostname %s resolved to %s, expected %s", hostname, resolvedIP, expectedIP)
	}

	return nil
}

// GetNetworkCIDR calculates the network CIDR from gateway and netmask
func GetNetworkCIDR(gateway, netmask string) (*net.IPNet, error) {
	gatewayIP := net.ParseIP(gateway)
	if gatewayIP == nil {
		return nil, fmt.Errorf("invalid gateway IP: %s", gateway)
	}

	netmaskIP := net.ParseIP(netmask)
	if netmaskIP == nil {
		return nil, fmt.Errorf("invalid netmask: %s", netmask)
	}

	// Convert netmask to IPMask
	mask := net.IPMask(netmaskIP.To4())
	if mask == nil {
		return nil, fmt.Errorf("invalid netmask format: %s", netmask)
	}

	// Create IPNet from gateway and mask
	return &net.IPNet{
		IP:   gatewayIP.Mask(mask),
		Mask: mask,
	}, nil
}

// isIPInRange checks if an IP is within the range [start, end]
func isIPInRange(ip, start, end net.IP) bool {
	// Convert to 4-byte representation for comparison
	ip4 := ip.To4()
	start4 := start.To4()
	end4 := end.To4()

	if ip4 == nil || start4 == nil || end4 == nil {
		return false
	}

	// Convert to uint32 for comparison
	ipInt := ipToUint32(ip4)
	startInt := ipToUint32(start4)
	endInt := ipToUint32(end4)

	return ipInt >= startInt && ipInt <= endInt
}

// ipToUint32 converts an IPv4 address to uint32
func ipToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	if ip == nil {
		return 0
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}
