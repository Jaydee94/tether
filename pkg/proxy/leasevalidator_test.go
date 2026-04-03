package proxy

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

func buildActiveLease(name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "tether.dev",
		Version: "v1alpha1",
		Kind:    "TetherLease",
	})
	obj.SetName(name)
	_ = unstructured.SetNestedField(obj.Object, "Active", "status", "phase")
	return obj
}

func buildTokenSecret(leaseName, token, ns string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tether-token-" + leaseName,
			Namespace: ns,
			Labels: map[string]string{
				labelTokenType:  tokenSecretType,
				labelTokenLease: leaseName,
			},
		},
		Data: map[string][]byte{
			tokenDataKey: []byte(token),
		},
	}
}

func TestKubernetesLeaseValidator_ValidToken(t *testing.T) {
	const (
		leaseName = "my-lease"
		token     = "super-secret-token"
		ns        = "tether-system"
	)

	kubeClient := kubefake.NewSimpleClientset(buildTokenSecret(leaseName, token, ns))

	scheme := runtime.NewScheme()
	dynClient := dynfake.NewSimpleDynamicClient(scheme,
		buildActiveLease(leaseName),
	)

	v := NewKubernetesLeaseValidator(kubeClient, dynClient, ns)
	sessionID, err := v.Validate(context.Background(), token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sessionID != leaseName {
		t.Errorf("expected sessionID %q, got %q", leaseName, sessionID)
	}
}

func TestKubernetesLeaseValidator_InvalidToken(t *testing.T) {
	const ns = "tether-system"

	kubeClient := kubefake.NewSimpleClientset(
		buildTokenSecret("my-lease", "correct-token", ns),
	)

	scheme := runtime.NewScheme()
	dynClient := dynfake.NewSimpleDynamicClient(scheme)

	v := NewKubernetesLeaseValidator(kubeClient, dynClient, ns)
	_, err := v.Validate(context.Background(), "wrong-token")
	if err == nil {
		t.Error("expected error for unknown token")
	}
}

func TestKubernetesLeaseValidator_InactiveLease(t *testing.T) {
	const (
		leaseName = "inactive-lease"
		token     = "some-token"
		ns        = "tether-system"
	)

	kubeClient := kubefake.NewSimpleClientset(buildTokenSecret(leaseName, token, ns))

	// Build an Expired lease.
	expiredLease := buildActiveLease(leaseName)
	_ = unstructured.SetNestedField(expiredLease.Object, "Expired", "status", "phase")

	scheme := runtime.NewScheme()
	dynClient := dynfake.NewSimpleDynamicClient(scheme, expiredLease)

	v := NewKubernetesLeaseValidator(kubeClient, dynClient, ns)
	_, err := v.Validate(context.Background(), token)
	if err == nil {
		t.Error("expected error for inactive lease")
	}
}

func TestKubernetesLeaseValidator_EmptyToken(t *testing.T) {
	kubeClient := kubefake.NewSimpleClientset()
	scheme := runtime.NewScheme()
	dynClient := dynfake.NewSimpleDynamicClient(scheme)

	v := NewKubernetesLeaseValidator(kubeClient, dynClient, "tether-system")
	_, err := v.Validate(context.Background(), "")
	if err == nil {
		t.Error("expected error for empty token")
	}
}
