package tailscale

import (
	"context"
	"fmt"
	"testing"

	"github.com/catalystcommunity/foundry/v1/internal/helm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeIngressLister returns synthetic tailnet state.
type fakeIngressLister struct {
	address      string
	addressState AddressState
	addressErr   error
	ingresses    []Ingress
	listErr      error
}

func (f *fakeIngressLister) OperatorAddressState(ctx context.Context) (string, AddressState, error) {
	if f.addressErr != nil {
		return "", AddressServiceMissing, f.addressErr
	}
	state := f.addressState
	if f.address != "" {
		state = AddressFound
	}
	return f.address, state, nil
}

func (f *fakeIngressLister) ListTailscaleIngresses(ctx context.Context) ([]Ingress, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.ingresses, nil
}

func deployedRelease() []helm.Release {
	return []helm.Release{{Name: OperatorReleaseName, Namespace: DefaultNamespace, Status: "deployed"}}
}

func newHealthChecker(t *testing.T, helmClient *mockHelmClient, lister IngressLister) *HealthChecker {
	t.Helper()
	helmInstaller, err := NewHelmInstaller(helmClient, validConfig())
	require.NoError(t, err)
	checker, err := NewHealthChecker(helmInstaller, lister)
	require.NoError(t, err)
	return checker
}

func TestNewHealthChecker(t *testing.T) {
	helmInstaller, err := NewHelmInstaller(&mockHelmClient{}, validConfig())
	require.NoError(t, err)

	t.Run("nil helm installer", func(t *testing.T) {
		_, err := NewHealthChecker(nil, &fakeIngressLister{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "helm installer cannot be nil")
	})

	t.Run("nil ingress lister", func(t *testing.T) {
		_, err := NewHealthChecker(helmInstaller, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ingress lister cannot be nil")
	})
}

func TestHealthCheck(t *testing.T) {
	t.Run("reports operator address and ingresses", func(t *testing.T) {
		checker := newHealthChecker(t, &mockHelmClient{releases: deployedRelease()}, &fakeIngressLister{
			address: "100.90.1.5",
			ingresses: []Ingress{
				{Name: "kei-web", Hostname: "kei-web.tail-scale.ts.net", Ready: true},
				{Name: "abac", Hostname: "abac.tail-scale.ts.net", Ready: true},
			},
		})

		health, err := checker.Check(context.Background())
		require.NoError(t, err)
		assert.True(t, health.Installed)
		assert.Equal(t, "deployed", health.ReleaseStatus)
		assert.Equal(t, "100.90.1.5", health.OperatorAddress)
		require.Len(t, health.Ingresses, 2)
		// Sorted by name for stable output.
		assert.Equal(t, "abac", health.Ingresses[0].Name)
		assert.Equal(t, "kei-web", health.Ingresses[1].Name)
		assert.True(t, health.Healthy())
	})

	t.Run("not installed short-circuits the tailnet lookups", func(t *testing.T) {
		lister := &fakeIngressLister{addressErr: fmt.Errorf("should not be called")}
		checker := newHealthChecker(t, &mockHelmClient{}, lister)

		health, err := checker.Check(context.Background())
		require.NoError(t, err)
		assert.False(t, health.Installed)
		assert.False(t, health.Healthy())
	})

	// An unready ingress is the observable symptom of a proxy that is not
	// serving; health must not report green.
	t.Run("unready ingress makes the integration unhealthy", func(t *testing.T) {
		checker := newHealthChecker(t, &mockHelmClient{releases: deployedRelease()}, &fakeIngressLister{
			address: "100.90.1.5",
			ingresses: []Ingress{
				{Name: "kei-web", Ready: true},
				{Name: "kei-oidc", Ready: false},
			},
		})

		health, err := checker.Check(context.Background())
		require.NoError(t, err)
		assert.False(t, health.Healthy())
		assert.Contains(t, health.Summary(), "1/2 ingress ready")
	})

	// The operator may not have registered a tailnet address yet; that is a
	// reportable state, not an error.
	t.Run("missing operator address is reported not fatal", func(t *testing.T) {
		checker := newHealthChecker(t, &mockHelmClient{releases: deployedRelease()},
			&fakeIngressLister{addressState: AddressNotAssigned})

		health, err := checker.Check(context.Background())
		require.NoError(t, err)
		assert.Empty(t, health.OperatorAddress)
		assert.Contains(t, health.Summary(), "not yet registered on the tailnet")
	})

	t.Run("a non-deployed release is unhealthy", func(t *testing.T) {
		checker := newHealthChecker(t,
			&mockHelmClient{releases: []helm.Release{{Name: OperatorReleaseName, Status: "failed"}}},
			&fakeIngressLister{address: "100.90.1.5"})

		health, err := checker.Check(context.Background())
		require.NoError(t, err)
		assert.True(t, health.Installed)
		assert.False(t, health.Healthy())
	})

	t.Run("propagates release lookup failure", func(t *testing.T) {
		checker := newHealthChecker(t, &mockHelmClient{listErr: fmt.Errorf("cluster unreachable")}, &fakeIngressLister{})

		_, err := checker.Check(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cluster unreachable")
	})

	t.Run("propagates ingress listing failure", func(t *testing.T) {
		checker := newHealthChecker(t, &mockHelmClient{releases: deployedRelease()},
			&fakeIngressLister{listErr: fmt.Errorf("forbidden")})

		_, err := checker.Check(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "forbidden")
	})

	t.Run("propagates address lookup failure", func(t *testing.T) {
		checker := newHealthChecker(t, &mockHelmClient{releases: deployedRelease()},
			&fakeIngressLister{addressErr: fmt.Errorf("no such service")})

		_, err := checker.Check(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no such service")
	})
}

func TestHealthSummary(t *testing.T) {
	tests := []struct {
		name        string
		health      Health
		wantContain string
	}{
		{
			name:        "not installed",
			health:      Health{},
			wantContain: "not installed",
		},
		{
			name: "deployed with all ingress ready",
			health: Health{
				Installed: true, ReleaseStatus: "deployed", OperatorAddress: "100.90.1.5",
				Ingresses: []Ingress{{Name: "a", Ready: true}},
			},
			wantContain: "1/1 ingress ready",
		},
		{
			name: "deployed with no ingresses",
			health: Health{
				Installed: true, ReleaseStatus: "deployed", OperatorAddress: "100.90.1.5",
			},
			wantContain: "0/0 ingress ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Contains(t, tt.health.Summary(), tt.wantContain)
		})
	}
}

func TestHealthHealthy(t *testing.T) {
	t.Run("deployed with no ingresses is healthy", func(t *testing.T) {
		h := Health{Installed: true, ReleaseStatus: "deployed"}
		assert.True(t, h.Healthy())
	})

	t.Run("status match is case-insensitive", func(t *testing.T) {
		h := Health{Installed: true, ReleaseStatus: "Deployed"}
		assert.True(t, h.Healthy())
	})

	t.Run("not installed is never healthy", func(t *testing.T) {
		assert.False(t, Health{ReleaseStatus: "deployed"}.Healthy())
	})
}

func TestReleaseStatus(t *testing.T) {
	t.Run("reports an existing release", func(t *testing.T) {
		installer, err := NewHelmInstaller(&mockHelmClient{releases: deployedRelease()}, validConfig())
		require.NoError(t, err)

		status, exists, err := installer.ReleaseStatus(context.Background())
		require.NoError(t, err)
		assert.True(t, exists)
		assert.Equal(t, "deployed", status)
	})

	t.Run("reports a missing release", func(t *testing.T) {
		installer, err := NewHelmInstaller(&mockHelmClient{}, validConfig())
		require.NoError(t, err)

		_, exists, err := installer.ReleaseStatus(context.Background())
		require.NoError(t, err)
		assert.False(t, exists)
	})

	t.Run("ignores unrelated releases", func(t *testing.T) {
		installer, err := NewHelmInstaller(&mockHelmClient{
			releases: []helm.Release{{Name: "grafana", Status: "deployed"}},
		}, validConfig())
		require.NoError(t, err)

		_, exists, err := installer.ReleaseStatus(context.Background())
		require.NoError(t, err)
		assert.False(t, exists)
	})

	t.Run("propagates list failure", func(t *testing.T) {
		installer, err := NewHelmInstaller(&mockHelmClient{listErr: fmt.Errorf("forbidden")}, validConfig())
		require.NoError(t, err)

		_, _, err = installer.ReleaseStatus(context.Background())
		require.Error(t, err)
	})
}

// TestInstallOperatorRecoversFromNameInUse covers the case where List gave a
// wrong answer (transient failure or missing permission) but Helm reports the
// release exists.
func TestInstallOperatorRecoversFromNameInUse(t *testing.T) {
	helmClient := &mockHelmClient{
		listErr:    fmt.Errorf("forbidden"),
		installErr: fmt.Errorf("cannot re-use a name that is still in use"),
	}
	installer, err := NewHelmInstaller(helmClient, validConfig())
	require.NoError(t, err)

	require.NoError(t, installer.InstallOperator(context.Background()))
	assert.Len(t, helmClient.installCalls, 1)
	assert.Len(t, helmClient.upgradeCalls, 1, "must fall back to upgrade")
}

// TestHealthAddressDescription covers how each empty-address cause is reported.
// "no operator service found" and "not yet registered" call for different
// action, so they must not read alike.
func TestHealthAddressDescription(t *testing.T) {
	tests := []struct {
		name   string
		health Health
		want   string
	}{
		{
			name:   "address present",
			health: Health{OperatorAddress: "operator.ts.net", AddressState: AddressFound},
			want:   "operator.ts.net",
		},
		{
			name:   "no service found",
			health: Health{AddressState: AddressServiceMissing},
			want:   "no operator service found",
		},
		{
			name:   "service exists but unregistered",
			health: Health{AddressState: AddressNotAssigned},
			want:   "not yet registered on the tailnet",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.health.AddressDescription())
		})
	}
}

// TestHealthCheckPropagatesAddressState confirms the state survives from the
// lister through to Health, which is what the command prints.
func TestHealthCheckPropagatesAddressState(t *testing.T) {
	for _, tt := range []struct {
		name  string
		state AddressState
		want  string
	}{
		{"missing service", AddressServiceMissing, "no operator service found"},
		{"unregistered", AddressNotAssigned, "not yet registered on the tailnet"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			checker := newHealthChecker(t, &mockHelmClient{releases: deployedRelease()},
				&fakeIngressLister{addressState: tt.state})

			health, err := checker.Check(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tt.state, health.AddressState)
			assert.Contains(t, health.Summary(), tt.want)
		})
	}
}
