package tailscale

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/catalystcommunity/foundry/v1/internal/helm"
)

// mockHelmClient implements a mock Helm client for testing
type mockHelmClient struct {
	addRepoErr   error
	installErr   error
	upgradeErr   error
	uninstallErr error
	listErr      error

	// releases is what List returns, letting a test model an operator that is
	// already installed.
	releases []helm.Release

	repoAddCalls   []helm.RepoAddOptions
	installCalls   []helm.InstallOptions
	upgradeCalls   []helm.UpgradeOptions
	uninstallCalls []helm.UninstallOptions
}

func (m *mockHelmClient) AddRepo(ctx context.Context, opts helm.RepoAddOptions) error {
	m.repoAddCalls = append(m.repoAddCalls, opts)
	return m.addRepoErr
}

func (m *mockHelmClient) Install(ctx context.Context, opts helm.InstallOptions) error {
	m.installCalls = append(m.installCalls, opts)
	return m.installErr
}

func (m *mockHelmClient) Upgrade(ctx context.Context, opts helm.UpgradeOptions) error {
	m.upgradeCalls = append(m.upgradeCalls, opts)
	return m.upgradeErr
}

func (m *mockHelmClient) Uninstall(ctx context.Context, opts helm.UninstallOptions) error {
	m.uninstallCalls = append(m.uninstallCalls, opts)
	return m.uninstallErr
}

func (m *mockHelmClient) List(ctx context.Context, namespace string) ([]helm.Release, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.releases, nil
}

func (m *mockHelmClient) Get(ctx context.Context, releaseName, namespace string) (*helm.Release, error) {
	return nil, nil
}

func (m *mockHelmClient) Close() error {
	return nil
}

func TestNewHelmInstaller(t *testing.T) {
	tests := []struct {
		name    string
		client  *mockHelmClient
		config  *Config
		wantErr bool
		errMsg  string
	}{
		{
			name:    "nil client",
			client:  nil,
			config:  &Config{},
			wantErr: true,
			errMsg:  "helm client cannot be nil",
		},
		{
			name:    "nil config",
			client:  &mockHelmClient{},
			config:  nil,
			wantErr: true,
			errMsg:  "config cannot be nil",
		},
		{
			name:   "valid inputs",
			client: &mockHelmClient{},
			config: &Config{
				OAuthClientID:     stringPtr("client-123"),
				OAuthClientSecret: stringPtr("secret-456"),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var client HelmClient
			if tt.client != nil {
				client = tt.client
			}
			// If tt.client is nil, client remains nil (typed nil interface)

			installer, err := NewHelmInstaller(client, tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewHelmInstaller() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" && err.Error() != tt.errMsg {
				t.Errorf("NewHelmInstaller() error message = %q, want %q", err.Error(), tt.errMsg)
				return
			}
			if !tt.wantErr && installer == nil {
				t.Error("NewHelmInstaller() returned nil without error")
			}
		})
	}
}

func TestHelmInstaller_AddRepository(t *testing.T) {
	tests := []struct {
		name       string
		addRepoErr error
		wantErr    bool
	}{
		{
			name:       "successful repo add",
			addRepoErr: nil,
			wantErr:    false,
		},
		{
			name:       "repo add failure",
			addRepoErr: context.DeadlineExceeded,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &mockHelmClient{
				addRepoErr: tt.addRepoErr,
			}
			config := &Config{
				OAuthClientID:     stringPtr("client-123"),
				OAuthClientSecret: stringPtr("secret-456"),
			}

			installer, err := NewHelmInstaller(mockClient, config)
			if err != nil {
				t.Fatalf("NewHelmInstaller() unexpected error: %v", err)
			}

			err = installer.AddRepository(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("AddRepository() error = %v, wantErr %v", err, tt.wantErr)
			}

			// Verify repo was added with correct parameters
			if !tt.wantErr && len(mockClient.repoAddCalls) == 1 {
				repo := mockClient.repoAddCalls[0]
				if repo.Name != TailscaleRepoName {
					t.Errorf("Repository name = %q, want %q", repo.Name, TailscaleRepoName)
				}
				if repo.URL != TailscaleRepoURL {
					t.Errorf("Repository URL = %q, want %q", repo.URL, TailscaleRepoURL)
				}
				if !repo.ForceUpdate {
					t.Error("Expected ForceUpdate to be true")
				}
			}
		})
	}
}

func TestHelmInstaller_InstallOperator(t *testing.T) {
	tests := []struct {
		name       string
		config     *Config
		installErr error
		wantErr    bool
		checkOpts  func(t *testing.T, opts helm.InstallOptions)
	}{
		{
			name: "successful install with minimal config",
			config: &Config{
				OAuthClientID:     stringPtr("client-123"),
				OAuthClientSecret: stringPtr("secret-456"),
			},
			installErr: nil,
			wantErr:    false,
			checkOpts: func(t *testing.T, opts helm.InstallOptions) {
				if opts.ReleaseName != OperatorReleaseName {
					t.Errorf("ReleaseName = %q, want %q", opts.ReleaseName, OperatorReleaseName)
				}
				if opts.Namespace != DefaultNamespace {
					t.Errorf("Namespace = %q, want %q", opts.Namespace, DefaultNamespace)
				}
				expectedChart := TailscaleRepoName + "/" + OperatorChartName
				if opts.Chart != expectedChart {
					t.Errorf("Chart = %q, want %q", opts.Chart, expectedChart)
				}
				if !opts.CreateNamespace {
					t.Error("Expected CreateNamespace to be true")
				}
				if !opts.Wait {
					t.Error("Expected Wait to be true")
				}
				if opts.Timeout != DefaultInstallTimeout {
					t.Errorf("Timeout = %v, want %v", opts.Timeout, DefaultInstallTimeout)
				}
			},
		},
		{
			name: "install with custom operator image",
			config: &Config{
				OAuthClientID:     stringPtr("client-123"),
				OAuthClientSecret: stringPtr("secret-456"),
				OperatorImage:     stringPtr("custom/operator:v1.0.0"),
			},
			installErr: nil,
			wantErr:    false,
			checkOpts: func(t *testing.T, opts helm.InstallOptions) {
				// Verify values carry the custom image, split into repository
				// and tag under operatorConfig.
				if opts.Values == nil {
					t.Fatal("Expected Values to be set")
				}
				image := operatorConfigImage(t, opts.Values)
				if image["repository"] != "custom/operator" {
					t.Errorf("image.repository = %v, want custom/operator", image["repository"])
				}
				if image["tag"] != "v1.0.0" {
					t.Errorf("image.tag = %v, want v1.0.0", image["tag"])
				}
			},
		},
		{
			name: "install failure",
			config: &Config{
				OAuthClientID:     stringPtr("client-123"),
				OAuthClientSecret: stringPtr("secret-456"),
			},
			installErr: context.DeadlineExceeded,
			wantErr:    true,
			checkOpts:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &mockHelmClient{
				installErr: tt.installErr,
			}

			installer, err := NewHelmInstaller(mockClient, tt.config)
			if err != nil {
				t.Fatalf("NewHelmInstaller() unexpected error: %v", err)
			}

			err = installer.InstallOperator(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("InstallOperator() error = %v, wantErr %v", err, tt.wantErr)
			}

			// Verify install was called with correct parameters
			if !tt.wantErr && len(mockClient.installCalls) == 1 && tt.checkOpts != nil {
				tt.checkOpts(t, mockClient.installCalls[0])
			}
		})
	}
}

func TestHelmInstaller_UninstallOperator(t *testing.T) {
	tests := []struct {
		name         string
		uninstallErr error
		wantErr      bool
	}{
		{
			name:         "successful uninstall",
			uninstallErr: nil,
			wantErr:      false,
		},
		{
			name:         "uninstall failure",
			uninstallErr: context.DeadlineExceeded,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &mockHelmClient{
				uninstallErr: tt.uninstallErr,
			}
			config := &Config{
				OAuthClientID:     stringPtr("client-123"),
				OAuthClientSecret: stringPtr("secret-456"),
			}

			installer, err := NewHelmInstaller(mockClient, config)
			if err != nil {
				t.Fatalf("NewHelmInstaller() unexpected error: %v", err)
			}

			err = installer.UninstallOperator(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("UninstallOperator() error = %v, wantErr %v", err, tt.wantErr)
			}

			// Verify uninstall was called with correct parameters
			if !tt.wantErr && len(mockClient.uninstallCalls) == 1 {
				opts := mockClient.uninstallCalls[0]
				if opts.ReleaseName != OperatorReleaseName {
					t.Errorf("ReleaseName = %q, want %q", opts.ReleaseName, OperatorReleaseName)
				}
				if opts.Namespace != DefaultNamespace {
					t.Errorf("Namespace = %q, want %q", opts.Namespace, DefaultNamespace)
				}
				if !opts.Wait {
					t.Error("Expected Wait to be true")
				}
				if opts.Timeout != DefaultInstallTimeout {
					t.Errorf("Timeout = %v, want %v", opts.Timeout, DefaultInstallTimeout)
				}
			}
		})
	}
}

func TestHelmInstaller_GenerateHelmValues(t *testing.T) {
	t.Run("nil config errors", func(t *testing.T) {
		installer := &HelmInstaller{}
		_, err := installer.generateHelmValues()
		if err == nil {
			t.Fatal("expected error for nil config")
		}
	})

	// OAuth credentials in Helm values are persisted in cleartext in the
	// release secret and exposed by `helm get values`. They belong in the
	// operator-oauth Kubernetes secret instead.
	t.Run("never contains OAuth credentials", func(t *testing.T) {
		installer := helmInstallerFor(t, &Config{
			OAuthClientID:     stringPtr("client-123"),
			OAuthClientSecret: stringPtr("secret-456"),
		})

		values, err := installer.generateHelmValues()
		if err != nil {
			t.Fatalf("generateHelmValues() unexpected error: %v", err)
		}
		rendered := fmt.Sprintf("%v", values)
		if strings.Contains(rendered, "secret-456") || strings.Contains(rendered, "client-123") {
			t.Errorf("OAuth credentials leaked into Helm values: %s", rendered)
		}
		if _, exists := values["oauth"]; exists {
			t.Error("values must not carry an oauth block")
		}
	})

	t.Run("minimal config produces no image override", func(t *testing.T) {
		installer := helmInstallerFor(t, &Config{
			OAuthClientID:     stringPtr("client-123"),
			OAuthClientSecret: stringPtr("secret-456"),
		})

		values, err := installer.generateHelmValues()
		if err != nil {
			t.Fatalf("generateHelmValues() unexpected error: %v", err)
		}
		if _, exists := values["operatorConfig"]; exists {
			t.Error("expected no operatorConfig for a minimal config")
		}
	})

	t.Run("splits operator image into repository and tag", func(t *testing.T) {
		installer := helmInstallerFor(t, &Config{
			OAuthClientID:     stringPtr("client-123"),
			OAuthClientSecret: stringPtr("secret-456"),
			OperatorImage:     stringPtr("custom/operator:v1.0.0"),
		})

		values, err := installer.generateHelmValues()
		if err != nil {
			t.Fatalf("generateHelmValues() unexpected error: %v", err)
		}
		image := operatorConfigImage(t, values)
		if image["repository"] != "custom/operator" {
			t.Errorf("repository = %v, want custom/operator", image["repository"])
		}
		if image["tag"] != "v1.0.0" {
			t.Errorf("tag = %v, want v1.0.0", image["tag"])
		}
	})

	t.Run("empty operator image adds nothing", func(t *testing.T) {
		installer := helmInstallerFor(t, &Config{
			OAuthClientID:     stringPtr("client-123"),
			OAuthClientSecret: stringPtr("secret-456"),
			OperatorImage:     stringPtr(""),
		})

		values, err := installer.generateHelmValues()
		if err != nil {
			t.Fatalf("generateHelmValues() unexpected error: %v", err)
		}
		if _, exists := values["operatorConfig"]; exists {
			t.Error("expected no operatorConfig for an empty image string")
		}
	})

	t.Run("passes configured tags as defaultTags", func(t *testing.T) {
		installer := helmInstallerFor(t, &Config{
			OAuthClientID:     stringPtr("client-123"),
			OAuthClientSecret: stringPtr("secret-456"),
			Tags:              []string{"tag:production"},
		})

		values, err := installer.generateHelmValues()
		if err != nil {
			t.Fatalf("generateHelmValues() unexpected error: %v", err)
		}
		opCfg, ok := values["operatorConfig"].(map[string]interface{})
		if !ok {
			t.Fatal("expected operatorConfig in values")
		}
		tags, ok := opCfg["defaultTags"].([]string)
		if !ok || len(tags) != 1 || tags[0] != "tag:production" {
			t.Errorf("defaultTags = %v, want [tag:production]", opCfg["defaultTags"])
		}
	})

	// Image and tags both write operatorConfig; neither may clobber the other.
	t.Run("image and tags coexist under operatorConfig", func(t *testing.T) {
		installer := helmInstallerFor(t, &Config{
			OAuthClientID:     stringPtr("client-123"),
			OAuthClientSecret: stringPtr("secret-456"),
			OperatorImage:     stringPtr("custom/operator:v1.0.0"),
			Tags:              []string{"tag:production"},
		})

		values, err := installer.generateHelmValues()
		if err != nil {
			t.Fatalf("generateHelmValues() unexpected error: %v", err)
		}
		opCfg := values["operatorConfig"].(map[string]interface{})
		if _, ok := opCfg["image"]; !ok {
			t.Error("image missing from operatorConfig")
		}
		if _, ok := opCfg["defaultTags"]; !ok {
			t.Error("defaultTags missing from operatorConfig")
		}
	})
}

func TestSplitImageRef(t *testing.T) {
	tests := []struct {
		image    string
		wantRepo string
		wantTag  string
	}{
		{"tailscale/operator:latest", "tailscale/operator", "latest"},
		{"tailscale/operator", "tailscale/operator", ""},
		{"custom/operator:v1.0.0", "custom/operator", "v1.0.0"},
		// A registry port must not be mistaken for a tag.
		{"registry:5000/tailscale/operator", "registry:5000/tailscale/operator", ""},
		{"registry:5000/tailscale/operator:v1.2", "registry:5000/tailscale/operator", "v1.2"},
	}

	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			repo, tag := splitImageRef(tt.image)
			if repo != tt.wantRepo || tag != tt.wantTag {
				t.Errorf("splitImageRef(%q) = (%q, %q), want (%q, %q)",
					tt.image, repo, tag, tt.wantRepo, tt.wantTag)
			}
		})
	}
}

// helmInstallerFor builds a HelmInstaller over a mock client.
func helmInstallerFor(t *testing.T, cfg *Config) *HelmInstaller {
	t.Helper()
	installer, err := NewHelmInstaller(&mockHelmClient{}, cfg)
	if err != nil {
		t.Fatalf("NewHelmInstaller() unexpected error: %v", err)
	}
	return installer
}

// operatorConfigImage extracts operatorConfig.image from Helm values.
func operatorConfigImage(t *testing.T, values map[string]interface{}) map[string]interface{} {
	t.Helper()
	opCfg, ok := values["operatorConfig"].(map[string]interface{})
	if !ok {
		t.Fatal("expected operatorConfig in values")
	}
	image, ok := opCfg["image"].(map[string]interface{})
	if !ok {
		t.Fatal("expected operatorConfig.image in values")
	}
	return image
}

func TestGenerateSecretData(t *testing.T) {
	tests := []struct {
		name    string
		config  *Config
		wantErr bool
		errMsg  string
		check   func(t *testing.T, data map[string]string)
	}{
		{
			name:    "nil config",
			config:  nil,
			wantErr: true,
			errMsg:  "config cannot be nil",
		},
		{
			name: "missing client ID",
			config: &Config{
				OAuthClientSecret: stringPtr("secret-456"),
			},
			wantErr: true,
			errMsg:  "OAuth client ID is required",
		},
		{
			name: "empty client ID",
			config: &Config{
				OAuthClientID:     stringPtr(""),
				OAuthClientSecret: stringPtr("secret-456"),
			},
			wantErr: true,
			errMsg:  "OAuth client ID is required",
		},
		{
			name: "missing client secret",
			config: &Config{
				OAuthClientID: stringPtr("client-123"),
			},
			wantErr: true,
			errMsg:  "OAuth client secret is required",
		},
		{
			name: "empty client secret",
			config: &Config{
				OAuthClientID:     stringPtr("client-123"),
				OAuthClientSecret: stringPtr(""),
			},
			wantErr: true,
			errMsg:  "OAuth client secret is required",
		},
		{
			name: "valid credentials",
			config: &Config{
				OAuthClientID:     stringPtr("client-123"),
				OAuthClientSecret: stringPtr("secret-456"),
			},
			wantErr: false,
			check: func(t *testing.T, data map[string]string) {
				if data["client_id"] != "client-123" {
					t.Errorf("client_id = %q, want %q", data["client_id"], "client-123")
				}
				if data["client_secret"] != "secret-456" {
					t.Errorf("client_secret = %q, want %q", data["client_secret"], "secret-456")
				}
				if len(data) != 2 {
					t.Errorf("Expected 2 keys in secret data, got %d", len(data))
				}
			},
		},
		{
			name: "secret reference format",
			config: &Config{
				OAuthClientID:     stringPtr("${secret:foundry-core/tailscale:client_id}"),
				OAuthClientSecret: stringPtr("${secret:foundry-core/tailscale:client_secret}"),
			},
			wantErr: false,
			check: func(t *testing.T, data map[string]string) {
				// Secret references should be stored as-is (resolution happens later)
				if data["client_id"] != "${secret:foundry-core/tailscale:client_id}" {
					t.Errorf("client_id not preserved as secret reference")
				}
				if data["client_secret"] != "${secret:foundry-core/tailscale:client_secret}" {
					t.Errorf("client_secret not preserved as secret reference")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := GenerateSecretData(tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("GenerateSecretData() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" && err.Error() != tt.errMsg {
				t.Errorf("GenerateSecretData() error message = %q, want %q", err.Error(), tt.errMsg)
				return
			}

			if !tt.wantErr && tt.check != nil {
				tt.check(t, data)
			}
		})
	}
}

func TestHelmConstants(t *testing.T) {
	// Verify Helm constants are correct
	if TailscaleRepoName != "tailscale" {
		t.Errorf("TailscaleRepoName = %q, want %q", TailscaleRepoName, "tailscale")
	}
	if TailscaleRepoURL != "https://pkgs.tailscale.com/helmcharts" {
		t.Errorf("TailscaleRepoURL = %q, want %q", TailscaleRepoURL, "https://pkgs.tailscale.com/helmcharts")
	}
	if OperatorChartName != "tailscale-operator" {
		t.Errorf("OperatorChartName = %q, want %q", OperatorChartName, "tailscale-operator")
	}
	if OperatorReleaseName != "tailscale-operator" {
		t.Errorf("OperatorReleaseName = %q, want %q", OperatorReleaseName, "tailscale-operator")
	}
	if DefaultInstallTimeout != 5*time.Minute {
		t.Errorf("DefaultInstallTimeout = %v, want %v", DefaultInstallTimeout, 5*time.Minute)
	}
}
