package tailscale

import (
	"context"
	"fmt"
)

const (
	// DocsURL points at the Tailscale instructions for creating the OAuth
	// client the operator needs.
	DocsURL = "https://tailscale.com/kb/1236/kubernetes-operator#prerequisites"

	// SecretMount is the OpenBAO KV v2 mount holding Foundry's secrets.
	SecretMount = "foundry-core"

	// SecretPath is the OpenBAO path holding the Tailscale OAuth credentials.
	SecretPath = "tailscale"

	// ClientIDKey and ClientSecretKey are the keys within that secret.
	ClientIDKey     = "client_id"
	ClientSecretKey = "client_secret"
)

// SecretStore reads and writes Foundry's secrets. *openbao.Client satisfies it.
type SecretStore interface {
	ReadSecretV2(ctx context.Context, mount, path string) (map[string]interface{}, error)
	WriteSecretV2(ctx context.Context, mount, path string, data map[string]interface{}) error
}

// ErrCredentialsMissing reports that no OAuth credentials are available, with a
// pointer to the documentation for creating them.
func ErrCredentialsMissing() error {
	return fmt.Errorf("Tailscale is enabled but OAuth credentials are missing.\n\n"+
		"Create an OAuth client with the devices:write and auth_keys scopes, then set\n"+
		"components.tailscale.oauth_client_id and oauth_client_secret in stack.yaml.\n"+
		"Instructions: %s", DocsURL)
}

// ResolveCredentials returns the OAuth credentials for the operator, preferring
// literal values in the component config and falling back to OpenBAO.
//
// A literal credential found in the config is written to OpenBAO so it survives
// as the authoritative copy; the caller is then expected to replace it in
// stack.yaml with a ${secret:...} reference so the plaintext does not persist
// in the config file. Returns ErrCredentialsMissing when neither source has
// them.
func ResolveCredentials(ctx context.Context, store SecretStore, configID, configSecret string) (clientID, clientSecret string, storedNew bool, err error) {
	if store == nil {
		return "", "", false, fmt.Errorf("secret store cannot be nil")
	}

	// Literal values in the config win, and are persisted to OpenBAO.
	if configID != "" && configSecret != "" {
		data := map[string]interface{}{
			ClientIDKey:     configID,
			ClientSecretKey: configSecret,
		}
		if err := store.WriteSecretV2(ctx, SecretMount, SecretPath, data); err != nil {
			return "", "", false, fmt.Errorf("failed to store Tailscale credentials in OpenBAO: %w", err)
		}
		return configID, configSecret, true, nil
	}

	// Otherwise read the stored credentials.
	existing, readErr := store.ReadSecretV2(ctx, SecretMount, SecretPath)
	if readErr != nil || existing == nil {
		return "", "", false, ErrCredentialsMissing()
	}

	id, _ := existing[ClientIDKey].(string)
	secret, _ := existing[ClientSecretKey].(string)
	if id == "" || secret == "" {
		return "", "", false, ErrCredentialsMissing()
	}

	return id, secret, false, nil
}
