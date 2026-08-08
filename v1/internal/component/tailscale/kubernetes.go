package tailscale

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// tailscaleIngressClass is the ingressClassName the operator watches.
const tailscaleIngressClass = "tailscale"

// operatorServiceName is the Service the operator's own chart creates; its
// tailnet address is published as the LoadBalancer ingress hostname.
//
// Not assumed to exist: an operator installed by hand may name it differently,
// so lookup falls back to label discovery. See findOperatorService.
const operatorServiceName = "operator"

// operatorSelectors are label selectors used to find the operator's Service
// when it is not at the conventional name, most to least specific.
var operatorSelectors = []string{
	"app.kubernetes.io/name=tailscale-operator",
	"app.kubernetes.io/name=tailscale",
	"app=tailscale-operator",
	"app=operator",
}

// KubeAdapter implements the Kubernetes-facing interfaces this package needs
// (SecretWriter, KubernetesClient, IngressLister) over the standard clients.
type KubeAdapter struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
}

// NewKubeAdapter creates an adapter over the given Kubernetes clients.
func NewKubeAdapter(clientset kubernetes.Interface, dynamicClient dynamic.Interface) (*KubeAdapter, error) {
	if clientset == nil {
		return nil, fmt.Errorf("clientset cannot be nil")
	}
	if dynamicClient == nil {
		return nil, fmt.Errorf("dynamic client cannot be nil")
	}
	return &KubeAdapter{clientset: clientset, dynamic: dynamicClient}, nil
}

// EnsureSecret creates or updates a secret, creating its namespace if needed.
// Writing the same content twice leaves the secret unchanged.
func (k *KubeAdapter) EnsureSecret(ctx context.Context, namespace, name string, data map[string]string) error {
	if err := k.ensureNamespace(ctx, namespace); err != nil {
		return err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		StringData: data,
		Type:       corev1.SecretTypeOpaque,
	}

	secrets := k.clientset.CoreV1().Secrets(namespace)
	_, err := secrets.Create(ctx, secret, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create secret %s/%s: %w", namespace, name, err)
	}

	if _, err := secrets.Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to update secret %s/%s: %w", namespace, name, err)
	}
	return nil
}

// ensureNamespace creates the namespace when it does not already exist.
func (k *KubeAdapter) ensureNamespace(ctx context.Context, namespace string) error {
	_, err := k.clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check namespace %s: %w", namespace, err)
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if _, err := k.clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("failed to create namespace %s: %w", namespace, err)
	}
	return nil
}

// Apply creates or updates a custom resource. Applying an unchanged manifest
// is a no-op from the cluster's perspective, which keeps install idempotent.
func (k *KubeAdapter) Apply(ctx context.Context, manifest map[string]interface{}) error {
	obj := &unstructured.Unstructured{Object: manifest}

	gvr, err := gvrForManifest(obj)
	if err != nil {
		return err
	}

	namespace := obj.GetNamespace()
	resource := k.dynamic.Resource(gvr).Namespace(namespace)

	existing, err := resource.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := resource.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("failed to create %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read %s %s: %w", obj.GetKind(), obj.GetName(), err)
	}

	// Carry the resourceVersion across so the update is accepted.
	obj.SetResourceVersion(existing.GetResourceVersion())
	if _, err := resource.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to update %s %s: %w", obj.GetKind(), obj.GetName(), err)
	}
	return nil
}

// gvrForManifest derives the GroupVersionResource for a Tailscale custom
// resource. Only the kinds this package deploys are recognised.
func gvrForManifest(obj *unstructured.Unstructured) (schema.GroupVersionResource, error) {
	gv, err := schema.ParseGroupVersion(obj.GetAPIVersion())
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("invalid apiVersion %q: %w", obj.GetAPIVersion(), err)
	}

	var resource string
	switch obj.GetKind() {
	case "Connector":
		resource = "connectors"
	case "DNSConfig":
		resource = "dnsconfigs"
	case "ProxyClass":
		resource = "proxyclasses"
	default:
		return schema.GroupVersionResource{}, fmt.Errorf("unsupported kind %q", obj.GetKind())
	}

	return gv.WithResource(resource), nil
}

// OperatorAddress returns the operator's tailnet address.
//
// Returns "" when no address is available. Use OperatorAddressState to tell
// apart the reasons, which are operationally different.
func (k *KubeAdapter) OperatorAddress(ctx context.Context) (string, error) {
	addr, _, err := k.OperatorAddressState(ctx)
	return addr, err
}

// AddressState describes why an operator address lookup produced what it did.
type AddressState int

const (
	// AddressFound means a tailnet address or hostname was discovered.
	AddressFound AddressState = iota

	// AddressServiceMissing means the operator could not be located at all --
	// neither its Deployment nor an API-server-proxy Service was found.
	AddressServiceMissing

	// AddressNotAssigned means the operator was found but no tailnet identity
	// could be read from it. Normal while starting up, a fault if it persists.
	AddressNotAssigned
)

// operatorDeploymentNames are the Deployment names the operator is known by,
// most to least likely.
var operatorDeploymentNames = []string{"operator", "tailscale-operator"}

// operatorHostnameEnv is the environment variable carrying the operator's own
// tailnet hostname. It is set regardless of whether the API-server proxy is
// enabled, which makes it the authoritative source for the operator's identity.
const operatorHostnameEnv = "OPERATOR_HOSTNAME"

// OperatorAddressState returns the operator's tailnet address along with why it
// is empty when it is.
//
// The operator's identity is read from its Deployment rather than inferred from
// a Service. A Service exists only when the API-server proxy is enabled, so
// treating its absence as "not registered" reports a healthy operator as broken
// whenever APISERVER_PROXY is off.
//
// The two empty results are distinct: "could not find the operator" and "found
// it but read no tailnet identity" call for different action, and only the
// second is a normal transient state.
func (k *KubeAdapter) OperatorAddressState(ctx context.Context) (string, AddressState, error) {
	// The operator's own tailnet hostname, from its Deployment. This is the
	// authoritative source: the operator has a tailnet identity whenever it is
	// running, whether or not the API-server proxy is enabled.
	hostname, deploymentFound, err := k.operatorHostname(ctx)
	if err != nil {
		return "", AddressServiceMissing, err
	}
	if hostname != "" {
		return hostname, AddressFound, nil
	}

	// Otherwise fall back to an API-server-proxy Service, which exists only
	// when that mode is on.
	svc, err := k.findOperatorService(ctx)
	if err != nil {
		return "", AddressServiceMissing, err
	}
	if svc != nil {
		for _, ing := range svc.Status.LoadBalancer.Ingress {
			if ing.Hostname != "" {
				return ing.Hostname, AddressFound, nil
			}
			if ing.IP != "" {
				return ing.IP, AddressFound, nil
			}
		}
		if svc.Spec.ExternalName != "" {
			return svc.Spec.ExternalName, AddressFound, nil
		}
		return "", AddressNotAssigned, nil
	}

	// A running operator with no readable hostname is registered but opaque to
	// us; only when nothing was found at all is the operator genuinely absent.
	if deploymentFound {
		return "", AddressNotAssigned, nil
	}
	return "", AddressServiceMissing, nil
}

// operatorHostname reads the operator's tailnet hostname from its Deployment,
// and reports whether the Deployment was found at all.
func (k *KubeAdapter) operatorHostname(ctx context.Context) (hostname string, found bool, err error) {
	deployments := k.clientset.AppsV1().Deployments(DefaultNamespace)

	var deploy *appsv1.Deployment
	for _, name := range operatorDeploymentNames {
		d, getErr := deployments.Get(ctx, name, metav1.GetOptions{})
		if getErr == nil {
			deploy = d
			break
		}
		if !apierrors.IsNotFound(getErr) {
			return "", false, fmt.Errorf("failed to read operator deployment: %w", getErr)
		}
	}

	if deploy == nil {
		for _, selector := range operatorSelectors {
			list, listErr := deployments.List(ctx, metav1.ListOptions{LabelSelector: selector})
			if listErr != nil {
				return "", false, fmt.Errorf("failed to list deployments by %q: %w", selector, listErr)
			}
			if len(list.Items) > 0 {
				deploy = &list.Items[0]
				break
			}
		}
	}

	if deploy == nil {
		return "", false, nil
	}

	for _, container := range deploy.Spec.Template.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == operatorHostnameEnv && env.Value != "" {
				return env.Value, true, nil
			}
		}
	}

	return "", true, nil
}

// findOperatorService locates the operator's Service, returning nil when there
// is none. It tries the conventional name first, then label discovery, so an
// operator installed outside Foundry is still found.
func (k *KubeAdapter) findOperatorService(ctx context.Context) (*corev1.Service, error) {
	services := k.clientset.CoreV1().Services(DefaultNamespace)

	svc, err := services.Get(ctx, operatorServiceName, metav1.GetOptions{})
	if err == nil {
		return svc, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("failed to read operator service: %w", err)
	}

	for _, selector := range operatorSelectors {
		list, err := services.List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return nil, fmt.Errorf("failed to list services by %q: %w", selector, err)
		}
		if len(list.Items) > 0 {
			return &list.Items[0], nil
		}
	}

	return nil, nil
}

// ListTailscaleIngresses returns every Ingress served by the Tailscale ingress
// class, across all namespaces.
//
// Readiness is taken from the presence of a load balancer address: the operator
// publishes the tailnet hostname there once the proxy is actually serving, so
// an ingress without one is not reachable.
func (k *KubeAdapter) ListTailscaleIngresses(ctx context.Context) ([]Ingress, error) {
	list, err := k.clientset.NetworkingV1().Ingresses(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list ingresses: %w", err)
	}

	var result []Ingress
	for i := range list.Items {
		ing := &list.Items[i]
		if ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName != tailscaleIngressClass {
			continue
		}

		entry := Ingress{Name: fmt.Sprintf("%s/%s", ing.Namespace, ing.Name)}
		for _, lb := range ing.Status.LoadBalancer.Ingress {
			if lb.Hostname != "" {
				entry.Hostname = lb.Hostname
				break
			}
			if lb.IP != "" {
				entry.Hostname = lb.IP
				break
			}
		}
		entry.Ready = entry.Hostname != ""
		result = append(result, entry)
	}

	return result, nil
}
