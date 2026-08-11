package k3s

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

// FlannelNode describes one expected underlay endpoint and an executor on a
// different cluster host from which reachability can be tested.
type FlannelNode struct {
	Name   string
	NodeIP string
	Peer   SSHExecutor
}

var kubernetesName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*$`)

// ValidateFlannelPublicIPs fails closed when Flannel advertises an address
// other than Foundry's configured LAN node address, advertises the API VIP, or
// cannot be reached from another node on the underlay.
// flannelPublicIP reads the endpoint a node advertises to its peers.
//
// This is what Flannel actually settled on, as opposed to what the config asked
// for, so it is the only way to catch a node that drifted after installation.
func flannelPublicIP(control SSHExecutor, nodeName string) (string, error) {
	cmd := fmt.Sprintf("sudo k3s kubectl get node %s -o jsonpath='{.metadata.annotations.flannel\\.alpha\\.coreos\\.com/public-ip}'", nodeName)
	result, err := control.Exec(cmd)
	if err != nil || result.ExitCode != 0 {
		return "", fmt.Errorf("could not read Flannel public IP for node %s", nodeName)
	}
	return strings.TrimSpace(result.Stdout), nil
}

func ValidateFlannelPublicIPs(control SSHExecutor, nodes []FlannelNode, vip string) error {
	for _, node := range nodes {
		if !kubernetesName.MatchString(node.Name) || net.ParseIP(node.NodeIP) == nil {
			return fmt.Errorf("invalid Flannel validation target %q", node.Name)
		}
		publicIP, err := flannelPublicIP(control, node.Name)
		if err != nil {
			return err
		}
		if publicIP == vip {
			return fmt.Errorf("node %s advertises API VIP %s as its Flannel public IP", node.Name, vip)
		}
		if publicIP != node.NodeIP {
			return fmt.Errorf("node %s Flannel public IP %s differs from configured node IP %s", node.Name, publicIP, node.NodeIP)
		}
		if len(nodes) > 1 {
			if node.Peer == nil {
				return fmt.Errorf("no peer available to test Flannel reachability for node %s", node.Name)
			}
			ping, err := node.Peer.Exec(fmt.Sprintf("ping -c 1 -W 2 %s", publicIP))
			if err != nil || ping.ExitCode != 0 {
				return fmt.Errorf("Flannel public IP %s for node %s is unreachable from another node", publicIP, node.Name)
			}
		}
	}
	return nil
}
