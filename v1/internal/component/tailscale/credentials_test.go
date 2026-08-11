package tailscale

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSecretStore is a synthetic stand-in for OpenBAO.
type fakeSecretStore struct {
	stored   map[string]interface{}
	readErr  error
	writeErr error
	writes   int
}

func (f *fakeSecretStore) ReadSecretV2(ctx context.Context, mount, path string) (map[string]interface{}, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.stored, nil
}

func (f *fakeSecretStore) WriteSecretV2(ctx context.Context, mount, path string, data map[string]interface{}) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.stored = data
	f.writes++
	return nil
}

func TestResolveCredentials(t *testing.T) {
	t.Run("reads stored credentials when config has none", func(t *testing.T) {
		store := &fakeSecretStore{stored: map[string]interface{}{
			ClientIDKey:     "stored-id",
			ClientSecretKey: "stored-secret",
		}}

		id, secret, storedNew, err := ResolveCredentials(context.Background(), store, "", "")
		require.NoError(t, err)
		assert.Equal(t, "stored-id", id)
		assert.Equal(t, "stored-secret", secret)
		assert.False(t, storedNew)
		assert.Equal(t, 0, store.writes, "reading must not write")
	})

	// A literal credential in stack.yaml is persisted so OpenBAO becomes the
	// authoritative copy and the plaintext can be replaced with a reference.
	t.Run("persists literal config credentials to the store", func(t *testing.T) {
		store := &fakeSecretStore{}

		id, secret, storedNew, err := ResolveCredentials(context.Background(), store, "cfg-id", "cfg-secret")
		require.NoError(t, err)
		assert.Equal(t, "cfg-id", id)
		assert.Equal(t, "cfg-secret", secret)
		assert.True(t, storedNew)
		require.Equal(t, 1, store.writes)
		assert.Equal(t, "cfg-id", store.stored[ClientIDKey])
		assert.Equal(t, "cfg-secret", store.stored[ClientSecretKey])
	})

	t.Run("config credentials override stored ones", func(t *testing.T) {
		store := &fakeSecretStore{stored: map[string]interface{}{
			ClientIDKey:     "old-id",
			ClientSecretKey: "old-secret",
		}}

		id, _, _, err := ResolveCredentials(context.Background(), store, "new-id", "new-secret")
		require.NoError(t, err)
		assert.Equal(t, "new-id", id)
		assert.Equal(t, "new-id", store.stored[ClientIDKey])
	})

	// The error must point at the documentation, per the repeatability rule.
	t.Run("missing credentials error links the docs", func(t *testing.T) {
		store := &fakeSecretStore{}

		_, _, _, err := ResolveCredentials(context.Background(), store, "", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), DocsURL)
		assert.Contains(t, err.Error(), "oauth_client_id")
	})

	t.Run("partial config credentials fall through to the store", func(t *testing.T) {
		store := &fakeSecretStore{stored: map[string]interface{}{
			ClientIDKey:     "stored-id",
			ClientSecretKey: "stored-secret",
		}}

		// Only an ID in config is not a usable pair.
		id, _, storedNew, err := ResolveCredentials(context.Background(), store, "cfg-id", "")
		require.NoError(t, err)
		assert.Equal(t, "stored-id", id)
		assert.False(t, storedNew)
	})

	t.Run("partial stored credentials are treated as missing", func(t *testing.T) {
		store := &fakeSecretStore{stored: map[string]interface{}{ClientIDKey: "stored-id"}}

		_, _, _, err := ResolveCredentials(context.Background(), store, "", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), DocsURL)
	})

	t.Run("empty stored values are treated as missing", func(t *testing.T) {
		store := &fakeSecretStore{stored: map[string]interface{}{
			ClientIDKey:     "",
			ClientSecretKey: "",
		}}

		_, _, _, err := ResolveCredentials(context.Background(), store, "", "")
		require.Error(t, err)
	})

	t.Run("read failure reports missing credentials", func(t *testing.T) {
		store := &fakeSecretStore{readErr: fmt.Errorf("secret not found")}

		_, _, _, err := ResolveCredentials(context.Background(), store, "", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), DocsURL)
	})

	t.Run("write failure is propagated", func(t *testing.T) {
		store := &fakeSecretStore{writeErr: fmt.Errorf("permission denied")}

		_, _, _, err := ResolveCredentials(context.Background(), store, "cfg-id", "cfg-secret")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "permission denied")
	})

	t.Run("nil store errors", func(t *testing.T) {
		_, _, _, err := ResolveCredentials(context.Background(), nil, "id", "secret")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "secret store cannot be nil")
	})
}

func TestErrCredentialsMissing(t *testing.T) {
	err := ErrCredentialsMissing()
	require.Error(t, err)
	assert.Contains(t, err.Error(), DocsURL)
	assert.Contains(t, err.Error(), "devices:write")
}
