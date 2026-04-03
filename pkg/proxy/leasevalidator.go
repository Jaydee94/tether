package proxy

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	// labelTokenType is the label key that identifies session-token Secrets.
	labelTokenType = "tether.dev/type"
	// labelTokenLease is the label key that links a token Secret to its TetherLease.
	labelTokenLease = "tether.dev/lease"
	// tokenSecretType is the expected value of labelTokenType on session-token Secrets.
	tokenSecretType = "session-token"
	// tokenDataKey is the Secret data key that holds the raw token bytes.
	tokenDataKey = "token"
)

var tetherLeaseGVR = schema.GroupVersionResource{
	Group:    "tether.dev",
	Version:  "v1alpha1",
	Resource: "tetherleases",
}

// KubernetesLeaseValidator validates proxy tokens by looking up k8s Secrets
// created by the Tether operator when a TetherLease is activated.
// It also verifies that the associated TetherLease is still Active.
type KubernetesLeaseValidator struct {
	kubeClient    kubernetes.Interface
	dynamicClient dynamic.Interface
	namespace     string
}

// NewKubernetesLeaseValidator creates a validator backed by live Kubernetes API calls.
// namespace is the k8s namespace where session-token Secrets are stored (e.g. "tether-system").
func NewKubernetesLeaseValidator(kubeClient kubernetes.Interface, dynamicClient dynamic.Interface, namespace string) *KubernetesLeaseValidator {
	if namespace == "" {
		namespace = "tether-system"
	}
	return &KubernetesLeaseValidator{
		kubeClient:    kubeClient,
		dynamicClient: dynamicClient,
		namespace:     namespace,
	}
}

// Validate looks up the token in k8s Secrets and returns the lease name (used as sessionID).
// It returns an error if the token is unknown or the associated lease is not Active.
func (v *KubernetesLeaseValidator) Validate(ctx context.Context, token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("empty token")
	}

	secrets, err := v.kubeClient.CoreV1().Secrets(v.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelTokenType + "=" + tokenSecretType,
	})
	if err != nil {
		return "", fmt.Errorf("listing token secrets in namespace %q: %w", v.namespace, err)
	}

	for i := range secrets.Items {
		s := &secrets.Items[i]
		storedToken := strings.TrimSpace(string(s.Data[tokenDataKey]))
		if storedToken != token {
			continue
		}
		leaseName := s.Labels[labelTokenLease]
		if leaseName == "" {
			continue
		}
		if err := v.checkLeaseActive(ctx, leaseName); err != nil {
			return "", fmt.Errorf("lease %q: %w", leaseName, err)
		}
		return leaseName, nil
	}

	return "", fmt.Errorf("invalid or expired token")
}

// checkLeaseActive fetches the TetherLease and verifies it is in the Active phase.
func (v *KubernetesLeaseValidator) checkLeaseActive(ctx context.Context, leaseName string) error {
	obj, err := v.dynamicClient.Resource(tetherLeaseGVR).Get(ctx, leaseName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting TetherLease %q: %w", leaseName, err)
	}

	phase, found, err := unstructured.NestedString(obj.Object, "status", "phase")
	if err != nil {
		return fmt.Errorf("reading phase from TetherLease %q: %w", leaseName, err)
	}
	if !found || phase != "Active" {
		if !found {
			phase = "(unset)"
		}
		return fmt.Errorf("lease phase is %q, expected Active", phase)
	}
	return nil
}
