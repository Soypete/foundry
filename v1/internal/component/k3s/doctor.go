package k3s

import (
	"fmt"
	"strings"

	"github.com/catalystcommunity/foundry/v1/internal/network"
)

// Finding is one thing wrong with a running cluster.
//
// Findings describe cross-component faults: a cluster whose components are each
// individually healthy can still be incoherent, and that is what `foundry stack
// status` cannot see. The canonical example is an API VIP sharing an interface
// with Flannel, where every component reports ready while pod traffic is
// mis-routed.
type Finding struct {
	// Check names the invariant that failed.
	Check string

	// Node is the host the finding applies to, empty when cluster-wide.
	Node string

	// Summary is a one-line statement of what is wrong.
	Summary string

	// Detail explains why it matters.
	Detail string

	// Remedy is the command an operator can run, when Foundry cannot fix it.
	Remedy string

	// Fixable reports whether Doctor can repair this with --fix.
	Fixable bool
}

// String renders a finding for the terminal.
func (f Finding) String() string {
	var b strings.Builder
	if f.Node != "" {
		fmt.Fprintf(&b, "✗ %s (%s): %s", f.Check, f.Node, f.Summary)
	} else {
		fmt.Fprintf(&b, "✗ %s: %s", f.Check, f.Summary)
	}
	if f.Detail != "" {
		fmt.Fprintf(&b, "\n    %s", f.Detail)
	}
	if f.Remedy != "" {
		fmt.Fprintf(&b, "\n    fix: %s", f.Remedy)
	}
	return b.String()
}

// NodeDiagnosis is the observed network state of one node.
type NodeDiagnosis struct {
	Name string

	// NodeIP is the address the configuration says this node should use.
	NodeIP string

	// FlannelIface is the interface Flannel is pinned to.
	FlannelIface string
}

// DiagnoseNodeNetwork reports faults in one node's data plane setup.
//
// It never changes anything: every check is a read, so this is safe to run
// against a cluster someone else is working on.
func DiagnoseNodeNetwork(executor SSHExecutor, node NodeDiagnosis, vip string) []Finding {
	var findings []Finding

	if vip != "" && node.NodeIP == vip {
		findings = append(findings, Finding{
			Check:   "flannel-endpoint",
			Node:    node.Name,
			Summary: fmt.Sprintf("node-ip is the API VIP %s", vip),
			Detail: "The VIP moves between control plane nodes, so peers sending pod " +
				"traffic there follow the API server role rather than this node.",
			Remedy: "set node_ip to this node's own address in stack.yaml",
		})
	}

	if node.NodeIP != "" && node.FlannelIface != "" && vip != "" {
		addrs, err := network.InterfaceAddresses(executor, node.FlannelIface)
		if err == nil {
			for _, addr := range addrs {
				if addr != vip {
					continue
				}
				findings = append(findings, Finding{
					Check:   "vip-on-flannel-interface",
					Node:    node.Name,
					Summary: fmt.Sprintf("%s carries both %s and the API VIP %s", node.FlannelIface, node.NodeIP, vip),
					Detail: "Pinning Flannel by interface name is not enough: K3s resolves " +
						"that name to whichever address it picks, which can be the VIP.",
					Remedy:  "remove kube-vip, or give it its own interface",
					Fixable: true,
				})
			}
		}
	}

	return findings
}

// DiagnoseFlannelEndpoints compares each node's advertised Flannel endpoint
// against the address it was configured with.
//
// This is the observable counterpart to the pre-install checks: it reads what
// the cluster actually settled on, which is the only way to catch a node that
// was configured correctly and then drifted.
func DiagnoseFlannelEndpoints(executor SSHExecutor, nodes []FlannelNode, vip string) []Finding {
	var findings []Finding

	for _, node := range nodes {
		publicIP, err := flannelPublicIP(executor, node.Name)
		if err != nil || publicIP == "" {
			continue
		}

		switch {
		case vip != "" && publicIP == vip:
			findings = append(findings, Finding{
				Check:   "flannel-endpoint",
				Node:    node.Name,
				Summary: fmt.Sprintf("advertises the API VIP %s as its Flannel endpoint", vip),
				Detail: "Other nodes are sending this node's pod traffic to a floating " +
					"address. Cross-node pod traffic involving this node will fail once " +
					"the VIP moves.",
				Remedy:  "remove kube-vip, then restart k3s so Flannel re-selects the node address",
				Fixable: true,
			})
		case publicIP != node.NodeIP:
			findings = append(findings, Finding{
				Check:   "flannel-endpoint",
				Node:    node.Name,
				Summary: fmt.Sprintf("advertises %s but is configured with %s", publicIP, node.NodeIP),
				Detail:  "Peers will send this node's pod traffic to an address it does not own.",
				Remedy:  fmt.Sprintf("restart k3s on %s after correcting node_ip", node.Name),
			})
		}
	}

	return findings
}

// DiagnoseStaleKubeVIP reports a kube-vip deployment on a cluster that no
// longer wants one.
func DiagnoseStaleKubeVIP(executor SSHExecutor, vipEnabled bool, vip string) []Finding {
	if vipEnabled {
		return nil
	}

	installed, err := IsKubeVIPInstalled(executor)
	if err != nil || !installed {
		return nil
	}

	summary := "kube-vip is deployed but this cluster has one control plane host"
	if vip != "" {
		summary = fmt.Sprintf("kube-vip is deployed for %s but this cluster has one control plane host", vip)
	}

	return []Finding{{
		Check:   "kube-vip",
		Summary: summary,
		Detail: "A VIP with one control plane provides no failover, and kube-vip keeps " +
			"it on the interface Flannel uses, where it can be selected as the VXLAN endpoint.",
		Remedy:  "sudo k3s kubectl delete daemonset -n kube-system kube-vip",
		Fixable: true,
	}}
}

// DiagnoseStaleVIPReferences finds Services still publishing the VIP in
// externalIPs after it has been withdrawn.
//
// These do not break pod networking, but they advertise an address nothing
// answers on, so they are worth reporting before someone debugs a dead endpoint.
func DiagnoseStaleVIPReferences(executor SSHExecutor, vip string) []Finding {
	if vip == "" {
		return nil
	}

	result, err := executor.Exec(fmt.Sprintf(
		"sudo k3s kubectl get svc -A -o jsonpath='{range .items[?(@.spec.externalIPs)]}{.metadata.namespace}/{.metadata.name} {.spec.externalIPs}{\"\\n\"}{end}' 2>/dev/null | grep -F %s", vip))
	if err != nil || result.ExitCode != 0 {
		return nil
	}

	var findings []Finding
	for _, line := range strings.Split(strings.TrimSpace(result.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		svc := strings.Fields(line)[0]
		findings = append(findings, Finding{
			Check:   "stale-vip-reference",
			Summary: fmt.Sprintf("Service %s still publishes the withdrawn VIP %s in externalIPs", svc, vip),
			Detail:  "Nothing answers on that address once kube-vip is gone.",
			Remedy:  fmt.Sprintf("kubectl patch svc -n %s --type=json -p '[{\"op\":\"remove\",\"path\":\"/spec/externalIPs\"}]'", strings.ReplaceAll(svc, "/", " ")),
		})
	}
	return findings
}

// RemoveKubeVIP deletes the kube-vip resources Foundry created.
//
// This lives behind `stack doctor --fix` rather than in the install path:
// tearing down a DaemonSet is not something an operator can review at the point
// of an install command. kube-vip holds the VIP itself, so deleting the
// DaemonSet releases the address on pod teardown; Foundry never touches the
// interface directly.
func RemoveKubeVIP(executor SSHExecutor) error {
	// Delete by regenerating exactly what setupKubeVIP applied. The manifests
	// were applied imperatively rather than labelled, so this is the only way to
	// be sure the same set is removed.
	manifests := FormatManifests(
		GenerateKubeVIPRBACManifest(),
		GenerateKubeVIPCloudProviderManifest(),
	)
	escaped := strings.ReplaceAll(manifests, "'", "'\"'\"'")

	if result, err := executor.Exec(fmt.Sprintf("echo '%s' | sudo k3s kubectl delete --ignore-not-found -f - 2>/dev/null", escaped)); err != nil {
		return fmt.Errorf("failed to remove kube-vip support resources: %w", err)
	} else if result.ExitCode != 0 && !strings.Contains(result.Stderr, "NotFound") {
		return fmt.Errorf("failed to remove kube-vip support resources: %s", result.Stderr)
	}

	result, err := executor.Exec("sudo k3s kubectl delete daemonset -n kube-system kube-vip --ignore-not-found")
	if err != nil {
		return fmt.Errorf("failed to remove the kube-vip daemonset: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("failed to remove the kube-vip daemonset: %s", result.Stderr)
	}

	return nil
}
