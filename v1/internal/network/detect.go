package network

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/catalystcommunity/foundry/v1/internal/ssh"
)

// DetectInterfaceForIP returns the interface that owns the exact configured
// address. This is stable when a secondary VIP exists, unlike taking the first
// address returned by the kernel.
func DetectInterfaceForIP(conn SSHExecutor, ip string) (string, error) {
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("invalid IP address: %s", ip)
	}
	result, err := conn.Exec(fmt.Sprintf("ip -o -4 addr show | awk '$4 ~ /^%s\\// {print $2; exit}'", regexp.QuoteMeta(ip)))
	if err != nil {
		return "", fmt.Errorf("failed to find interface for %s: %w", ip, err)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("failed to find interface for %s: %s", ip, result.Stderr)
	}
	iface := strings.TrimSpace(result.Stdout)
	if iface == "" {
		return "", fmt.Errorf("configured node IP %s is not assigned to this host", ip)
	}
	return iface, nil
}

// InterfaceAddresses returns the IPv4 addresses assigned to an interface,
// without their prefix lengths.
//
// An interface can legitimately carry more than one address, which is how a
// kube-vip VIP ends up beside the node's own address on the same NIC. Callers
// that pin something to an interface by name need to see all of them to know
// which address that name will actually resolve to.
func InterfaceAddresses(conn SSHExecutor, iface string) ([]string, error) {
	result, err := conn.Exec(fmt.Sprintf("ip -o -4 addr show dev %s | awk '{print $4}'", iface))
	if err != nil {
		return nil, fmt.Errorf("failed to list addresses on %s: %w", iface, err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("failed to list addresses on %s: %s", iface, result.Stderr)
	}

	var addrs []string
	for _, line := range strings.Split(strings.TrimSpace(result.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Each line is CIDR notation, e.g. "192.168.1.185/24".
		addr, _, found := strings.Cut(line, "/")
		if !found {
			addr = line
		}
		if net.ParseIP(addr) != nil {
			addrs = append(addrs, addr)
		}
	}
	return addrs, nil
}

// DetectInterfaceMTU returns the MTU of an interface.
//
// Flannel derives its own MTU from the underlay, which is right on a 1500-byte
// LAN and wrong on tailscale0, so the caller needs the real value to size the
// pod MTU from.
func DetectInterfaceMTU(conn SSHExecutor, iface string) (int, error) {
	if iface == "" {
		return 0, fmt.Errorf("interface name cannot be empty")
	}

	result, err := conn.Exec(fmt.Sprintf("cat /sys/class/net/%s/mtu", iface))
	if err != nil {
		return 0, fmt.Errorf("failed to detect MTU for %s: %w", iface, err)
	}
	if result.ExitCode != 0 {
		return 0, fmt.Errorf("failed to detect MTU for %s: %s", iface, result.Stderr)
	}

	mtu, err := strconv.Atoi(strings.TrimSpace(result.Stdout))
	if err != nil {
		return 0, fmt.Errorf("unreadable MTU for %s: %q", iface, strings.TrimSpace(result.Stdout))
	}
	if mtu <= 0 {
		return 0, fmt.Errorf("invalid MTU for %s: %d", iface, mtu)
	}
	return mtu, nil
}

// WaitForInterfaceAddress waits until an interface carries a specific address.
//
// K3s must not start before the interface Flannel is pinned to actually has its
// address, or Flannel binds to whatever exists at that moment. Polling
// DetectInterfaceForIP proves all three things at once: the address exists, it
// is up, and it is on the interface the caller expects.
func WaitForInterfaceAddress(conn SSHExecutor, iface, addr string, attempts int, delay time.Duration) error {
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		found, err := DetectInterfaceForIP(conn, addr)
		switch {
		case err != nil:
			lastErr = err
		case found == iface:
			return nil
		default:
			lastErr = fmt.Errorf("address %s is on %s, not %s", addr, found, iface)
		}
		if i < attempts-1 {
			time.Sleep(delay)
		}
	}

	return fmt.Errorf("%s does not carry %s: %w; bring the interface up before installing K3s", iface, addr, lastErr)
}

// SSHExecutor is an interface for executing SSH commands
// This allows for easier testing with mocks
type SSHExecutor interface {
	Exec(command string) (*ssh.ExecResult, error)
}

// InterfaceInfo contains information about a network interface
type InterfaceInfo struct {
	Name      string
	MAC       string
	IP        string
	IsDefault bool
}

// DetectPrimaryInterface detects the primary network interface on the remote host
// This is typically the interface with the default route
func DetectPrimaryInterface(conn SSHExecutor) (string, error) {
	// Try to get the default route interface
	result, err := conn.Exec("ip route show default | head -n1 | awk '{print $5}'")
	if err != nil {
		return "", fmt.Errorf("failed to detect primary interface: %w", err)
	}

	if result.ExitCode != 0 {
		return "", fmt.Errorf("failed to detect primary interface: %s", result.Stderr)
	}

	iface := strings.TrimSpace(result.Stdout)
	if iface == "" {
		return "", fmt.Errorf("no default route interface found")
	}

	return iface, nil
}

// DetectMAC detects the MAC address for the specified interface
func DetectMAC(conn SSHExecutor, iface string) (string, error) {
	if iface == "" {
		return "", fmt.Errorf("interface name cannot be empty")
	}

	// Get the MAC address from /sys/class/net/<iface>/address
	cmd := fmt.Sprintf("cat /sys/class/net/%s/address", iface)
	result, err := conn.Exec(cmd)
	if err != nil {
		return "", fmt.Errorf("failed to detect MAC address: %w", err)
	}

	if result.ExitCode != 0 {
		return "", fmt.Errorf("failed to detect MAC address for %s: %s", iface, result.Stderr)
	}

	mac := strings.TrimSpace(result.Stdout)
	if mac == "" {
		return "", fmt.Errorf("no MAC address found for interface %s", iface)
	}

	// Validate MAC address format (basic check)
	macRegex := regexp.MustCompile(`^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$`)
	if !macRegex.MatchString(mac) {
		return "", fmt.Errorf("invalid MAC address format: %s", mac)
	}

	return mac, nil
}

// DetectCurrentIP detects the current IP address for the specified interface
func DetectCurrentIP(conn SSHExecutor, iface string) (string, error) {
	if iface == "" {
		return "", fmt.Errorf("interface name cannot be empty")
	}

	// Get the IP address using ip addr show
	cmd := fmt.Sprintf("ip addr show %s | grep 'inet ' | head -n1 | awk '{print $2}' | cut -d'/' -f1", iface)
	result, err := conn.Exec(cmd)
	if err != nil {
		return "", fmt.Errorf("failed to detect IP address: %w", err)
	}

	if result.ExitCode != 0 {
		return "", fmt.Errorf("failed to detect IP address for %s: %s", iface, result.Stderr)
	}

	ip := strings.TrimSpace(result.Stdout)
	if ip == "" {
		return "", fmt.Errorf("no IP address found for interface %s", iface)
	}

	// Basic IPv4 validation
	ipRegex := regexp.MustCompile(`^(\d{1,3}\.){3}\d{1,3}$`)
	if !ipRegex.MatchString(ip) {
		return "", fmt.Errorf("invalid IP address format: %s", ip)
	}

	return ip, nil
}

// DetectInterface detects comprehensive interface information for the primary interface
func DetectInterface(conn SSHExecutor) (*InterfaceInfo, error) {
	// First, detect the primary interface
	iface, err := DetectPrimaryInterface(conn)
	if err != nil {
		return nil, err
	}

	// Then get MAC and IP
	mac, err := DetectMAC(conn, iface)
	if err != nil {
		return nil, fmt.Errorf("failed to detect MAC for %s: %w", iface, err)
	}

	ip, err := DetectCurrentIP(conn, iface)
	if err != nil {
		return nil, fmt.Errorf("failed to detect IP for %s: %w", iface, err)
	}

	return &InterfaceInfo{
		Name:      iface,
		MAC:       mac,
		IP:        ip,
		IsDefault: true,
	}, nil
}

// ListInterfaces lists all network interfaces on the remote host
func ListInterfaces(conn SSHExecutor) ([]*InterfaceInfo, error) {
	// Get list of interfaces (excluding loopback)
	result, err := conn.Exec("ip link show | grep '^[0-9]' | awk '{print $2}' | sed 's/:$//' | grep -v '^lo$'")
	if err != nil {
		return nil, fmt.Errorf("failed to list interfaces: %w", err)
	}

	if result.ExitCode != 0 {
		return nil, fmt.Errorf("failed to list interfaces: %s", result.Stderr)
	}

	ifaceNames := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(ifaceNames) == 0 || (len(ifaceNames) == 1 && ifaceNames[0] == "") {
		return nil, fmt.Errorf("no network interfaces found")
	}

	// Get the default interface name
	defaultIface, _ := DetectPrimaryInterface(conn)

	interfaces := make([]*InterfaceInfo, 0, len(ifaceNames))
	for _, name := range ifaceNames {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		info := &InterfaceInfo{
			Name:      name,
			IsDefault: name == defaultIface,
		}

		// Best effort to get MAC and IP
		if mac, err := DetectMAC(conn, name); err == nil {
			info.MAC = mac
		}

		if ip, err := DetectCurrentIP(conn, name); err == nil {
			info.IP = ip
		}

		interfaces = append(interfaces, info)
	}

	return interfaces, nil
}
