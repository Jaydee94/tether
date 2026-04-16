//go:build integration

package proxy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// testEnv is the shared envtest environment for all integration tests in this suite.
var (
	testKubeClient    kubernetes.Interface
	testDynamicClient dynamic.Interface
)

// TestMain sets up a real kube-apiserver via envtest, installs the TetherLease CRD,
// and tears everything down after all integration tests finish.
func TestMain(m *testing.M) {
	// Locate the CRD manifest relative to this file's module root.
	// When running with `go test -tags integration ./pkg/proxy/...` from the repo
	// root the CRD path resolves to config/crd/tetherlease.yaml.
	crdPath := filepath.Join("..", "..", "config", "crd")

	env := &envtest.Environment{
		CRDDirectoryPaths: []string{crdPath},
	}

	cfg, err := env.Start()
	if err != nil {
		panic("envtest start: " + err.Error())
	}

	testKubeClient, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		_ = env.Stop()
		panic("kubernetes client: " + err.Error())
	}

	testDynamicClient, err = dynamic.NewForConfig(cfg)
	if err != nil {
		_ = env.Stop()
		panic("dynamic client: " + err.Error())
	}

	// Create the tether-system namespace used by all tests.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tether-system"}}
	if _, err := testKubeClient.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{}); err != nil {
		_ = env.Stop()
		panic("create namespace: " + err.Error())
	}

	code := m.Run()

	_ = env.Stop()
	os.Exit(code)
}

// createTokenSecret creates (or replaces) a session-token Secret in envtest.
func createTokenSecret(t *testing.T, leaseName, token, ns string) {
	t.Helper()
	secret := buildTokenSecret(leaseName, token, ns)
	_, err := testKubeClient.CoreV1().Secrets(ns).Create(context.Background(), secret, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create token secret: %v", err)
	}
	t.Cleanup(func() {
		_ = testKubeClient.CoreV1().Secrets(ns).Delete(context.Background(), secret.Name, metav1.DeleteOptions{})
	})
}

// createTetherLease creates a TetherLease CR in envtest with the given phase.
func createTetherLease(t *testing.T, name, phase string) {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "tether.dev", Version: "v1alpha1", Kind: "TetherLease",
	})
	obj.SetName(name)
	obj.SetNamespace("tether-system") // TetherLease is namespace-scoped
	if err := unstructured.SetNestedField(obj.Object, phase, "status", "phase"); err != nil {
		t.Fatalf("set nested field: %v", err)
	}

	res := testDynamicClient.Resource(tetherLeaseGVR).Namespace("tether-system")
	created, err := res.Create(context.Background(), obj, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create TetherLease: %v", err)
	}

	// Status must be updated separately (status sub-resource).
	created.Object["status"] = map[string]interface{}{"phase": phase}
	if _, err = res.UpdateStatus(context.Background(), created, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update TetherLease status: %v", err)
	}

	t.Cleanup(func() {
		_ = res.Delete(context.Background(), name, metav1.DeleteOptions{})
	})
}

func TestIntegration_ValidToken_ActiveLease(t *testing.T) {
	const (
		leaseName = "it-active-lease"
		token     = "integration-valid-token"
		ns        = "tether-system"
	)

	createTokenSecret(t, leaseName, token, ns)
	createTetherLease(t, leaseName, "Active")

	v := NewKubernetesLeaseValidator(testKubeClient, testDynamicClient, ns)
	sessionID, err := v.Validate(context.Background(), token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sessionID != leaseName {
		t.Errorf("expected sessionID %q, got %q", leaseName, sessionID)
	}
}

func TestIntegration_ValidToken_ExpiredLease(t *testing.T) {
	const (
		leaseName = "it-expired-lease"
		token     = "integration-expired-token"
		ns        = "tether-system"
	)

	createTokenSecret(t, leaseName, token, ns)
	createTetherLease(t, leaseName, "Expired")

	v := NewKubernetesLeaseValidator(testKubeClient, testDynamicClient, ns)
	_, err := v.Validate(context.Background(), token)
	if err == nil {
		t.Error("expected error for expired lease")
	}
}

func TestIntegration_UnknownToken(t *testing.T) {
	const ns = "tether-system"

	createTokenSecret(t, "it-unknown-lease", "correct-token", ns)

	v := NewKubernetesLeaseValidator(testKubeClient, testDynamicClient, ns)
	_, err := v.Validate(context.Background(), "wrong-token")
	if err == nil {
		t.Error("expected error for unknown token")
	}
}

func TestIntegration_EmptyToken(t *testing.T) {
	v := NewKubernetesLeaseValidator(testKubeClient, testDynamicClient, "tether-system")
	_, err := v.Validate(context.Background(), "")
	if err == nil {
		t.Error("expected error for empty token")
	}
}

func TestIntegration_MissingLeaseLabel(t *testing.T) {
	const (
		token = "integration-nolabel-token"
		ns    = "tether-system"
	)

	// Create a Secret with the session-token type label but no tether.dev/lease label.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tether-token-nolabel-it",
			Namespace: ns,
			Labels: map[string]string{
				labelTokenType: tokenSecretType,
			},
		},
		Data: map[string][]byte{tokenDataKey: []byte(token)},
	}
	if _, err := testKubeClient.CoreV1().Secrets(ns).Create(context.Background(), secret, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	t.Cleanup(func() {
		_ = testKubeClient.CoreV1().Secrets(ns).Delete(context.Background(), secret.Name, metav1.DeleteOptions{})
	})

	v := NewKubernetesLeaseValidator(testKubeClient, testDynamicClient, ns)
	_, err := v.Validate(context.Background(), token)
	if err == nil {
		t.Error("expected error when tether.dev/lease label is missing")
	}
}
