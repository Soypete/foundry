package stack

import (
	"context"
	"fmt"

	"github.com/catalystcommunity/foundry/v1/internal/component/k3s"
	"github.com/catalystcommunity/foundry/v1/internal/component/statushelpers"
	"github.com/catalystcommunity/foundry/v1/internal/config"
	"github.com/catalystcommunity/foundry/v1/internal/host"
	"github.com/urfave/cli/v3"
)

// DoctorCommand handles the 'foundry stack doctor' command.
//
// `stack status` asks whether each component is installed and healthy. Doctor
// asks a different question: whether the running cluster is coherent. A cluster
// can report every component green while pod traffic is mis-routed, because the
// fault is in how components interact rather than in any one of them.
var DoctorCommand = &cli.Command{
	Name:  "doctor",
	Usage: "Diagnose cross-component faults in a running cluster",
	Description: "Reads the live cluster and reports invariants that are broken across " +
		"components, such as an API VIP sharing an interface with Flannel. Changes " +
		"nothing unless --fix is given, so it is safe to run at any time.",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:  "fix",
			Usage: "Apply the repairs for findings Foundry can fix",
		},
	},
	Action: runStackDoctor,
}

func runStackDoctor(ctx context.Context, cmd *cli.Command) error {
	configPath, err := config.FindConfig(cmd.String("config"))
	if err != nil {
		return fmt.Errorf("failed to find config: %w", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	cpHosts := cfg.GetClusterControlPlaneHosts()
	if len(cpHosts) == 0 {
		return fmt.Errorf("no cluster-control-plane hosts are configured; there is nothing to diagnose")
	}

	configDir, err := config.GetConfigDir()
	if err != nil {
		return fmt.Errorf("failed to get config directory: %w", err)
	}

	fmt.Println("Diagnosing cluster...")
	fmt.Println()

	control, closeControl, err := connectForDoctor(cpHosts[0], configDir, cfg.Cluster.Name)
	if err != nil {
		return err
	}
	defer closeControl()

	findings := collectFindings(ctx, cfg, control, configDir)

	if len(findings) == 0 {
		fmt.Println("✓ No cross-component faults found")
		return nil
	}

	for _, f := range findings {
		fmt.Println(f)
		fmt.Println()
	}

	if !cmd.Bool("fix") {
		fixable := 0
		for _, f := range findings {
			if f.Fixable {
				fixable++
			}
		}
		if fixable > 0 {
			fmt.Printf("%d of %d findings can be repaired: re-run with --fix\n", fixable, len(findings))
		}
		// A non-zero exit makes this usable as a check.
		return fmt.Errorf("%d finding(s)", len(findings))
	}

	return applyFixes(control, findings)
}

// collectFindings runs every check against the live cluster, reading only.
func collectFindings(ctx context.Context, cfg *config.Config, control k3s.SSHExecutor, configDir string) []k3s.Finding {
	var findings []k3s.Finding

	findings = append(findings, k3s.DiagnoseStaleKubeVIP(control, cfg.VIPEnabled(), cfg.Cluster.VIP)...)
	findings = append(findings, k3s.DiagnoseStaleVIPReferences(control, cfg.Cluster.VIP)...)

	// Per-node checks need each node's own connection, because an interface's
	// address list is only visible from the node that owns it.
	var flannelNodes []k3s.FlannelNode
	for _, h := range cfg.GetClusterHosts() {
		nodeIP, err := h.K3sNodeIP()
		if err != nil {
			continue
		}
		flannelNodes = append(flannelNodes, k3s.FlannelNode{Name: h.Hostname, NodeIP: nodeIP})

		nodeExec, closeNode, err := connectForDoctor(h, configDir, cfg.Cluster.Name)
		if err != nil {
			fmt.Printf("⚠ could not reach %s to check its interfaces: %v\n\n", h.Hostname, err)
			continue
		}
		findings = append(findings, k3s.DiagnoseNodeNetwork(nodeExec, k3s.NodeDiagnosis{
			Name:         h.Hostname,
			NodeIP:       nodeIP,
			FlannelIface: h.FlannelInterface,
		}, cfg.Cluster.VIP)...)
		closeNode()
	}

	findings = append(findings, k3s.DiagnoseFlannelEndpoints(control, flannelNodes, cfg.Cluster.VIP)...)

	return findings
}

// applyFixes repairs the findings Foundry can repair, announcing each one
// before it acts.
func applyFixes(control k3s.SSHExecutor, findings []k3s.Finding) error {
	fixed, skipped := 0, 0

	for _, f := range findings {
		if !f.Fixable {
			skipped++
			continue
		}

		switch f.Check {
		case "kube-vip", "vip-on-flannel-interface", "flannel-endpoint":
			fmt.Println("→ Removing kube-vip; it releases the VIP when its pods stop.")
			if err := k3s.RemoveKubeVIP(control); err != nil {
				return fmt.Errorf("failed to remove kube-vip: %w", err)
			}
			fmt.Println("✓ kube-vip removed")
			fmt.Println()
			fmt.Println("Restart k3s on the affected nodes so Flannel re-selects the node address:")
			fmt.Println("    sudo systemctl restart k3s")
			fixed++
			// Every VIP-related finding shares this one repair.
			return summarizeFixes(fixed, skipped, len(findings))
		default:
			skipped++
		}
	}

	return summarizeFixes(fixed, skipped, len(findings))
}

func summarizeFixes(fixed, skipped, total int) error {
	fmt.Println()
	fmt.Printf("Repaired %d of %d findings.\n", fixed, total)
	if skipped > 0 {
		fmt.Printf("%d need an operator decision; see the fix lines above.\n", skipped)
	}
	if fixed < total {
		return fmt.Errorf("%d finding(s) remain", total-fixed)
	}
	return nil
}

// connectForDoctor opens a read-only SSH session to a host, returning the
// executor and a close function.
func connectForDoctor(h *host.Host, configDir, clusterName string) (k3s.SSHExecutor, func(), error) {
	registered, err := statushelpers.FindHostByHostname(h.Hostname)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to find host %s: %w", h.Hostname, err)
	}

	conn, err := statushelpers.ConnectToHost(registered, configDir, clusterName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to %s: %w", h.Hostname, err)
	}

	// *ssh.Connection already satisfies k3s.SSHExecutor, so no adapter is needed.
	return conn, func() { conn.Close() }, nil
}
