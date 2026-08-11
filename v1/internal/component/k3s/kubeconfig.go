package k3s

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// KubeconfigClient defines the interface for storing and retrieving kubeconfig
type KubeconfigClient interface {
	ReadSecretV2(ctx context.Context, mount, path string) (map[string]interface{}, error)
	WriteSecretV2(ctx context.Context, mount, path string, data map[string]interface{}) error
}

// ClientEndpoint returns the address remote clients should use to reach the
// Kubernetes API for a given control plane host.
//
// A configured Tailscale address wins: the API VIP is internal to the cluster
// data plane and is deliberately not routable on the tailnet, so a kubeconfig
// pointing at it only works from the LAN. The Tailscale address is added to the
// API certificate SANs during provisioning, so it is a valid endpoint.
//
// Falls back to the VIP, and then to the node's own address. The last fallback
// matters for a single control plane cluster, where no VIP is deployed because
// a floating address with nothing to float between provides no failover: the
// node address is then the only endpoint there is.
func ClientEndpoint(tailscaleAddress, vip, nodeIP string) string {
	if tailscaleAddress != "" {
		return tailscaleAddress
	}
	if vip != "" {
		return vip
	}
	return nodeIP
}

// RetrieveKubeconfig retrieves the kubeconfig from a K3s control plane node
func RetrieveKubeconfig(executor SSHExecutor) (string, error) {
	// Read the kubeconfig file from the standard K3s location
	result, err := executor.Exec(fmt.Sprintf("sudo cat %s", KubeconfigPath))
	if err != nil {
		return "", fmt.Errorf("failed to retrieve kubeconfig: %w", err)
	}

	if result.ExitCode != 0 {
		return "", fmt.Errorf("failed to retrieve kubeconfig: exit code %d, stderr: %s", result.ExitCode, result.Stderr)
	}

	kubeconfig := strings.TrimSpace(result.Stdout)
	if kubeconfig == "" {
		return "", fmt.Errorf("kubeconfig is empty")
	}

	return kubeconfig, nil
}

// serverURLPattern matches the server URL on a kubeconfig cluster entry,
// capturing the leading whitespace and key so it can be preserved.
var serverURLPattern = regexp.MustCompile(`(?m)^(\s*server:\s*).*$`)

// ModifyKubeconfigServer rewrites every cluster's server URL to point at addr.
//
// It re-derives the endpoint rather than substituting a known-default address:
// a kubeconfig that was already rewritten (to a LAN address, or to a VIP) must
// still converge onto the current endpoint. Substituting only K3s's
// 127.0.0.1 default would silently no-op on exactly those kubeconfigs, which is
// how a stale endpoint survives a re-fetch.
func ModifyKubeconfigServer(kubeconfig string, addr string) string {
	if addr == "" {
		return kubeconfig
	}
	return serverURLPattern.ReplaceAllString(kubeconfig, "${1}"+KubeconfigServerURL(addr))
}

// KubeconfigServerURL renders the API server URL for a client endpoint address.
// IPv6 literals are bracketed so the result is a valid URL.
func KubeconfigServerURL(addr string) string {
	if strings.Contains(addr, ":") && !strings.HasPrefix(addr, "[") {
		return fmt.Sprintf("https://[%s]:6443", addr)
	}
	return fmt.Sprintf("https://%s:6443", addr)
}

// KubeconfigServerAddresses returns the server URLs currently present in a
// kubeconfig. Callers use it to decide whether a rewrite would change anything,
// so a converged endpoint can be reported as "no change" instead of rewritten.
func KubeconfigServerAddresses(kubeconfig string) []string {
	matches := serverURLPattern.FindAllStringSubmatch(kubeconfig, -1)
	servers := make([]string, 0, len(matches))
	for _, m := range matches {
		servers = append(servers, strings.TrimSpace(strings.TrimPrefix(m[0], m[1])))
	}
	return servers
}

// KubeconfigTargets reports whether every cluster entry in the kubeconfig
// already points at addr. An empty kubeconfig (no server entries) is not
// considered converged.
func KubeconfigTargets(kubeconfig string, addr string) bool {
	servers := KubeconfigServerAddresses(kubeconfig)
	if len(servers) == 0 {
		return false
	}
	want := KubeconfigServerURL(addr)
	for _, s := range servers {
		if s != want {
			return false
		}
	}
	return true
}

// StoreKubeconfig stores the kubeconfig in OpenBAO
func StoreKubeconfig(ctx context.Context, client KubeconfigClient, kubeconfig string) error {
	if kubeconfig == "" {
		return fmt.Errorf("kubeconfig cannot be empty")
	}

	data := map[string]interface{}{
		"kubeconfig": kubeconfig,
	}

	if err := client.WriteSecretV2(ctx, SecretMount, KubeconfigOpenBAOPath, data); err != nil {
		return fmt.Errorf("failed to store kubeconfig: %w", err)
	}

	return nil
}

// LoadKubeconfig retrieves the kubeconfig from OpenBAO
func LoadKubeconfig(ctx context.Context, client KubeconfigClient) (string, error) {
	data, err := client.ReadSecretV2(ctx, SecretMount, KubeconfigOpenBAOPath)
	if err != nil {
		return "", fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	kubeconfig, ok := data["kubeconfig"].(string)
	if !ok {
		return "", fmt.Errorf("kubeconfig is not a string")
	}

	if kubeconfig == "" {
		return "", fmt.Errorf("kubeconfig is empty")
	}

	return kubeconfig, nil
}

// RetrieveAndStoreKubeconfig retrieves the kubeconfig from a K3s node, points it
// at the given client endpoint address, and stores it in OpenBAO.
//
// addr is the address remote clients should use to reach the API server: the
// control plane's Tailscale address when one is configured, otherwise the API
// VIP. See ClientEndpoint.
func RetrieveAndStoreKubeconfig(ctx context.Context, executor SSHExecutor, client KubeconfigClient, addr string) error {
	// Retrieve kubeconfig from node
	kubeconfig, err := RetrieveKubeconfig(executor)
	if err != nil {
		return fmt.Errorf("failed to retrieve kubeconfig: %w", err)
	}

	// Point the kubeconfig at the client endpoint
	kubeconfig = ModifyKubeconfigServer(kubeconfig, addr)

	// Store in OpenBAO
	if err := StoreKubeconfig(ctx, client, kubeconfig); err != nil {
		return fmt.Errorf("failed to store kubeconfig: %w", err)
	}

	return nil
}

// RefreshStoredKubeconfig re-points the stored kubeconfig at addr, returning
// whether anything changed. It is the repair path for a kubeconfig whose
// endpoint has gone stale — for example one written before a control plane was
// given a Tailscale address, which therefore still points at the LAN or VIP.
//
// It is idempotent: when the stored kubeconfig already targets addr, nothing is
// written and it reports false.
func RefreshStoredKubeconfig(ctx context.Context, client KubeconfigClient, addr string) (bool, error) {
	if addr == "" {
		return false, fmt.Errorf("client endpoint address cannot be empty")
	}

	kubeconfig, err := LoadKubeconfig(ctx, client)
	if err != nil {
		return false, fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	if KubeconfigTargets(kubeconfig, addr) {
		return false, nil
	}

	updated := ModifyKubeconfigServer(kubeconfig, addr)
	if updated == kubeconfig {
		// No server entry to rewrite; treat as a malformed kubeconfig rather
		// than silently reporting success.
		return false, fmt.Errorf("kubeconfig contains no server entry to update")
	}

	if err := StoreKubeconfig(ctx, client, updated); err != nil {
		return false, fmt.Errorf("failed to store kubeconfig: %w", err)
	}

	return true, nil
}
