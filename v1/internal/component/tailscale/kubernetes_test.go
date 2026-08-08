package tailscale

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// newTestAdapter builds an adapter over fake Kubernetes clients seeded with objs.
func newTestAdapter(t *testing.T, objs ...runtime.Object) *KubeAdapter {
	t.Helper()
	scheme := runtime.NewScheme()
	gvrMap := map[schema.GroupVersionResource]string{
		{Group: "tailscale.com", Version: "v1alpha1", Resource: "connectors"}:   "ConnectorList",
		{Group: "tailscale.com", Version: "v1alpha1", Resource: "dnsconfigs"}:   "DNSConfigList",
		{Group: "tailscale.com", Version: "v1alpha1", Resource: "proxyclasses"}: "ProxyClassList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrMap)

	adapter, err := NewKubeAdapter(fake.NewSimpleClientset(objs...), dyn)
	require.NoError(t, err)
	return adapter
}

func TestNewKubeAdapter(t *testing.T) {
	t.Run("nil clientset", func(t *testing.T) {
		_, err := NewKubeAdapter(nil, dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "clientset cannot be nil")
	})

	t.Run("nil dynamic client", func(t *testing.T) {
		_, err := NewKubeAdapter(fake.NewSimpleClientset(), nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dynamic client cannot be nil")
	})
}

func TestEnsureSecret(t *testing.T) {
	data := map[string]string{ClientIDKey: "id", ClientSecretKey: "secret"}

	t.Run("creates the namespace and secret", func(t *testing.T) {
		adapter := newTestAdapter(t)
		require.NoError(t, adapter.EnsureSecret(context.Background(), DefaultNamespace, OAuthSecretName, data))

		secret, err := adapter.clientset.CoreV1().Secrets(DefaultNamespace).
			Get(context.Background(), OAuthSecretName, metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, "id", secret.StringData[ClientIDKey])

		_, err = adapter.clientset.CoreV1().Namespaces().
			Get(context.Background(), DefaultNamespace, metav1.GetOptions{})
		assert.NoError(t, err)
	})

	// Writing twice must converge rather than fail on AlreadyExists.
	t.Run("is idempotent", func(t *testing.T) {
		adapter := newTestAdapter(t)
		require.NoError(t, adapter.EnsureSecret(context.Background(), DefaultNamespace, OAuthSecretName, data))
		require.NoError(t, adapter.EnsureSecret(context.Background(), DefaultNamespace, OAuthSecretName, data))
	})

	t.Run("updates an existing secret", func(t *testing.T) {
		adapter := newTestAdapter(t)
		require.NoError(t, adapter.EnsureSecret(context.Background(), DefaultNamespace, OAuthSecretName, data))

		updated := map[string]string{ClientIDKey: "new-id", ClientSecretKey: "new-secret"}
		require.NoError(t, adapter.EnsureSecret(context.Background(), DefaultNamespace, OAuthSecretName, updated))

		secret, err := adapter.clientset.CoreV1().Secrets(DefaultNamespace).
			Get(context.Background(), OAuthSecretName, metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, "new-id", secret.StringData[ClientIDKey])
	})

	t.Run("tolerates a pre-existing namespace", func(t *testing.T) {
		adapter := newTestAdapter(t, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: DefaultNamespace},
		})
		assert.NoError(t, adapter.EnsureSecret(context.Background(), DefaultNamespace, OAuthSecretName, data))
	})
}

func TestApply(t *testing.T) {
	connector := func() map[string]interface{} {
		return map[string]interface{}{
			"apiVersion": "tailscale.com/v1alpha1",
			"kind":       "Connector",
			"metadata": map[string]interface{}{
				"name":      "foundry-subnet-router",
				"namespace": DefaultNamespace,
			},
			"spec": map[string]interface{}{
				"subnetRouter": map[string]interface{}{
					"advertiseRoutes": []interface{}{"172.16.0.0/16"},
				},
			},
		}
	}

	t.Run("creates a resource that does not exist", func(t *testing.T) {
		adapter := newTestAdapter(t)
		require.NoError(t, adapter.Apply(context.Background(), connector()))
	})

	// Applying an unchanged manifest twice keeps install idempotent.
	t.Run("updates an existing resource", func(t *testing.T) {
		adapter := newTestAdapter(t)
		require.NoError(t, adapter.Apply(context.Background(), connector()))
		assert.NoError(t, adapter.Apply(context.Background(), connector()))
	})

	t.Run("applies a DNSConfig", func(t *testing.T) {
		adapter := newTestAdapter(t)
		manifest := map[string]interface{}{
			"apiVersion": "tailscale.com/v1alpha1",
			"kind":       "DNSConfig",
			"metadata": map[string]interface{}{
				"name":      "ts-dns",
				"namespace": DefaultNamespace,
			},
		}
		assert.NoError(t, adapter.Apply(context.Background(), manifest))
	})

	t.Run("rejects an unsupported kind", func(t *testing.T) {
		adapter := newTestAdapter(t)
		err := adapter.Apply(context.Background(), map[string]interface{}{
			"apiVersion": "tailscale.com/v1alpha1",
			"kind":       "Mystery",
			"metadata":   map[string]interface{}{"name": "x", "namespace": DefaultNamespace},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported kind")
	})
}

func TestOperatorAddress(t *testing.T) {
	operatorService := func(status corev1.LoadBalancerStatus) *corev1.Service {
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: operatorServiceName, Namespace: DefaultNamespace},
			Status:     corev1.ServiceStatus{LoadBalancer: status},
		}
	}

	t.Run("returns the tailnet hostname", func(t *testing.T) {
		adapter := newTestAdapter(t, operatorService(corev1.LoadBalancerStatus{
			Ingress: []corev1.LoadBalancerIngress{{Hostname: "operator.tail-scale.ts.net"}},
		}))

		addr, err := adapter.OperatorAddress(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "operator.tail-scale.ts.net", addr)
	})

	t.Run("falls back to the IP", func(t *testing.T) {
		adapter := newTestAdapter(t, operatorService(corev1.LoadBalancerStatus{
			Ingress: []corev1.LoadBalancerIngress{{IP: "100.90.1.5"}},
		}))

		addr, err := adapter.OperatorAddress(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "100.90.1.5", addr)
	})

	// Not yet registered is a normal transient state, not an error.
	t.Run("returns empty when the service has no address", func(t *testing.T) {
		adapter := newTestAdapter(t, operatorService(corev1.LoadBalancerStatus{}))

		addr, err := adapter.OperatorAddress(context.Background())
		require.NoError(t, err)
		assert.Empty(t, addr)
	})

	t.Run("returns empty when the operator service is absent", func(t *testing.T) {
		adapter := newTestAdapter(t)

		addr, err := adapter.OperatorAddress(context.Background())
		require.NoError(t, err)
		assert.Empty(t, addr)
	})
}

// TestOperatorAddressState covers the distinction the bare address hides: a
// missing Service and a Service without an address are different faults.
func TestOperatorAddressState(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		adapter := newTestAdapter(t, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: operatorServiceName, Namespace: DefaultNamespace},
			Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{Hostname: "operator.tail-scale.ts.net"}},
			}},
		})

		addr, state, err := adapter.OperatorAddressState(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "operator.tail-scale.ts.net", addr)
		assert.Equal(t, AddressFound, state)
	})

	// The operator is deployed but has not joined the tailnet -- normal while
	// starting, a fault if it persists.
	t.Run("service exists with no address", func(t *testing.T) {
		adapter := newTestAdapter(t, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: operatorServiceName, Namespace: DefaultNamespace},
		})

		addr, state, err := adapter.OperatorAddressState(context.Background())
		require.NoError(t, err)
		assert.Empty(t, addr)
		assert.Equal(t, AddressNotAssigned, state)
	})

	t.Run("no service at all", func(t *testing.T) {
		adapter := newTestAdapter(t)

		addr, state, err := adapter.OperatorAddressState(context.Background())
		require.NoError(t, err)
		assert.Empty(t, addr)
		assert.Equal(t, AddressServiceMissing, state)
	})

	t.Run("falls back to externalName", func(t *testing.T) {
		adapter := newTestAdapter(t, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: operatorServiceName, Namespace: DefaultNamespace},
			Spec:       corev1.ServiceSpec{ExternalName: "operator.tail-scale.ts.net"},
		})

		addr, state, err := adapter.OperatorAddressState(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "operator.tail-scale.ts.net", addr)
		assert.Equal(t, AddressFound, state)
	})
}

// TestFindOperatorServiceByLabel covers an operator installed outside Foundry,
// whose Service is not at the conventional name. Assuming the name is what made
// the original lookup fragile.
func TestFindOperatorServiceByLabel(t *testing.T) {
	labelled := func(name string, labels map[string]string) *corev1.Service {
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: DefaultNamespace, Labels: labels},
			Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{Hostname: "found.tail-scale.ts.net"}},
			}},
		}
	}

	for _, tt := range []struct {
		name   string
		labels map[string]string
	}{
		{"chart name label", map[string]string{"app.kubernetes.io/name": "tailscale-operator"}},
		{"short name label", map[string]string{"app.kubernetes.io/name": "tailscale"}},
		{"legacy app label", map[string]string{"app": "tailscale-operator"}},
		{"bare operator label", map[string]string{"app": "operator"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			adapter := newTestAdapter(t, labelled("ts-operator-custom", tt.labels))

			addr, state, err := adapter.OperatorAddressState(context.Background())
			require.NoError(t, err)
			assert.Equal(t, "found.tail-scale.ts.net", addr)
			assert.Equal(t, AddressFound, state)
		})
	}

	t.Run("unrelated services are ignored", func(t *testing.T) {
		adapter := newTestAdapter(t, labelled("grafana", map[string]string{"app": "grafana"}))

		_, state, err := adapter.OperatorAddressState(context.Background())
		require.NoError(t, err)
		assert.Equal(t, AddressServiceMissing, state)
	})

	// The conventional name wins so a labelled decoy cannot shadow it.
	t.Run("prefers the conventional name", func(t *testing.T) {
		adapter := newTestAdapter(t,
			&corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: operatorServiceName, Namespace: DefaultNamespace},
				Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{
					Ingress: []corev1.LoadBalancerIngress{{Hostname: "canonical.ts.net"}},
				}},
			},
			labelled("other", map[string]string{"app.kubernetes.io/name": "tailscale-operator"}),
		)

		addr, _, err := adapter.OperatorAddressState(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "canonical.ts.net", addr)
	})
}

func TestListTailscaleIngresses(t *testing.T) {
	ingress := func(namespace, name, class string, lb []networkingv1.IngressLoadBalancerIngress) *networkingv1.Ingress {
		ing := &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Status: networkingv1.IngressStatus{
				LoadBalancer: networkingv1.IngressLoadBalancerStatus{Ingress: lb},
			},
		}
		if class != "" {
			ing.Spec.IngressClassName = &class
		}
		return ing
	}

	t.Run("returns only Tailscale-class ingresses", func(t *testing.T) {
		adapter := newTestAdapter(t,
			ingress("kei", "kei-web", tailscaleIngressClass,
				[]networkingv1.IngressLoadBalancerIngress{{Hostname: "kei-web.tail-scale.ts.net"}}),
			ingress("default", "public", "contour", nil),
			ingress("default", "unclassed", "", nil),
		)

		list, err := adapter.ListTailscaleIngresses(context.Background())
		require.NoError(t, err)
		require.Len(t, list, 1)
		assert.Equal(t, "kei/kei-web", list[0].Name)
		assert.Equal(t, "kei-web.tail-scale.ts.net", list[0].Hostname)
		assert.True(t, list[0].Ready)
	})

	// An ingress with no assigned address is the observable symptom of a proxy
	// that is not serving.
	t.Run("an ingress without an address is not ready", func(t *testing.T) {
		adapter := newTestAdapter(t,
			ingress("kei", "kei-oidc", tailscaleIngressClass, nil),
		)

		list, err := adapter.ListTailscaleIngresses(context.Background())
		require.NoError(t, err)
		require.Len(t, list, 1)
		assert.False(t, list[0].Ready)
		assert.Empty(t, list[0].Hostname)
	})

	t.Run("returns nothing when no ingresses exist", func(t *testing.T) {
		adapter := newTestAdapter(t)

		list, err := adapter.ListTailscaleIngresses(context.Background())
		require.NoError(t, err)
		assert.Empty(t, list)
	})

	t.Run("spans namespaces", func(t *testing.T) {
		adapter := newTestAdapter(t,
			ingress("kei", "a", tailscaleIngressClass,
				[]networkingv1.IngressLoadBalancerIngress{{Hostname: "a.ts.net"}}),
			ingress("pedro", "b", tailscaleIngressClass,
				[]networkingv1.IngressLoadBalancerIngress{{IP: "100.90.1.6"}}),
		)

		list, err := adapter.ListTailscaleIngresses(context.Background())
		require.NoError(t, err)
		assert.Len(t, list, 2)
	})
}

// operatorDeployment builds an operator Deployment carrying a tailnet hostname.
func operatorDeployment(name, hostname string, labels map[string]string) *appsv1.Deployment {
	env := []corev1.EnvVar{{Name: "APISERVER_PROXY", Value: "false"}}
	if hostname != "" {
		env = append(env, corev1.EnvVar{Name: operatorHostnameEnv, Value: hostname})
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: DefaultNamespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "operator", Env: env}},
				},
			},
		},
	}
}

// TestOperatorAddressWithoutAPIServerProxy is the regression test for reporting
// a healthy operator as unregistered.
//
// With APISERVER_PROXY=false the operator creates no Service for itself -- only
// headless proxy Services for the workloads it manages. Inferring registration
// from Service status therefore said "not yet registered" about an operator
// that was online with a tailnet identity. The identity comes from the
// Deployment, which exists in either mode.
func TestOperatorAddressWithoutAPIServerProxy(t *testing.T) {
	t.Run("reads the hostname from the deployment when no service exists", func(t *testing.T) {
		adapter := newTestAdapter(t, operatorDeployment("operator", "tailscale-operator", nil))

		addr, state, err := adapter.OperatorAddressState(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "tailscale-operator", addr)
		assert.Equal(t, AddressFound, state,
			"an operator with APISERVER_PROXY=false is registered, not missing")
	})

	t.Run("finds the deployment under its alternate name", func(t *testing.T) {
		adapter := newTestAdapter(t, operatorDeployment("tailscale-operator", "ts-op", nil))

		addr, state, err := adapter.OperatorAddressState(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "ts-op", addr)
		assert.Equal(t, AddressFound, state)
	})

	t.Run("finds the deployment by label", func(t *testing.T) {
		adapter := newTestAdapter(t, operatorDeployment("custom-name", "ts-op",
			map[string]string{"app.kubernetes.io/name": "tailscale-operator"}))

		addr, state, err := adapter.OperatorAddressState(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "ts-op", addr)
		assert.Equal(t, AddressFound, state)
	})

	// A deployed operator whose hostname cannot be read is not "missing".
	t.Run("deployment without a hostname is not-assigned rather than missing", func(t *testing.T) {
		adapter := newTestAdapter(t, operatorDeployment("operator", "", nil))

		_, state, err := adapter.OperatorAddressState(context.Background())
		require.NoError(t, err)
		assert.Equal(t, AddressNotAssigned, state)
	})

	t.Run("nothing deployed at all is missing", func(t *testing.T) {
		adapter := newTestAdapter(t)

		_, state, err := adapter.OperatorAddressState(context.Background())
		require.NoError(t, err)
		assert.Equal(t, AddressServiceMissing, state)
	})

	// The Deployment is authoritative, so a stale Service cannot override it.
	t.Run("deployment hostname wins over a service address", func(t *testing.T) {
		adapter := newTestAdapter(t,
			operatorDeployment("operator", "from-deployment", nil),
			&corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: operatorServiceName, Namespace: DefaultNamespace},
				Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{
					Ingress: []corev1.LoadBalancerIngress{{Hostname: "from-service"}},
				}},
			})

		addr, _, err := adapter.OperatorAddressState(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "from-deployment", addr)
	})

	// With the API-server proxy on there is a Service, and it is still used
	// when the Deployment carries no hostname.
	t.Run("falls back to the service when the deployment has no hostname", func(t *testing.T) {
		adapter := newTestAdapter(t,
			operatorDeployment("operator", "", nil),
			&corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: operatorServiceName, Namespace: DefaultNamespace},
				Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{
					Ingress: []corev1.LoadBalancerIngress{{Hostname: "proxy.ts.net"}},
				}},
			})

		addr, state, err := adapter.OperatorAddressState(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "proxy.ts.net", addr)
		assert.Equal(t, AddressFound, state)
	})
}
