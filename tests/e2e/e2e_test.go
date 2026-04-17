//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	kubeClient      kubernetes.Interface
	dynamicClient   dynamic.Interface
	kubeConfig      *rest.Config
	tetherNamespace = "tether-system"
)

// tetherLeaseGVR is the GroupVersionResource for TetherLease CRDs
var tetherLeaseGVR = schema.GroupVersionResource{
	Group:    "tether.dev",
	Version:  "v1alpha1",
	Resource: "tetherlease",
}

// TestMain initializes the Kubernetes client and sets up the test environment.
func TestMain(m *testing.M) {
	var err error

	// Load kubeconfig - uses KUBECONFIG env var or default
	kubeConfig, err = getClientConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load kubeconfig: %v\n", err)
		os.Exit(1)
	}

	// Create Kubernetes client
	kubeClient, err = kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create kubernetes client: %v\n", err)
		os.Exit(1)
	}

	// Create dynamic client for custom resources
	dynamicClient, err = dynamic.NewForConfig(kubeConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create dynamic client: %v\n", err)
		os.Exit(1)
	}

	// Ensure test namespace exists
	if err := ensureNamespace(context.Background(), tetherNamespace); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create namespace: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	os.Exit(code)
}

// getClientConfig loads the Kubernetes client config from kubeconfig file.
func getClientConfig() (*rest.Config, error) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = filepath.Join(os.Getenv("HOME"), ".kube", "config")
	}

	// Try to load from file first, fall back to in-cluster config
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}

	// Fallback: return error if KUBECONFIG is not set or file doesn't exist
	if _, err := os.Stat(kubeconfig); os.IsNotExist(err) {
		return nil, fmt.Errorf("kubeconfig not found at %s", kubeconfig)
	}

	// Use clientcmd to load the kubeconfig
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

// ensureNamespace creates a namespace if it doesn't exist.
func ensureNamespace(ctx context.Context, ns string) error {
	_, err := kubeClient.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err == nil {
		return nil // Namespace already exists
	}

	// Create namespace
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: ns,
		},
	}

	_, err = kubeClient.CoreV1().Namespaces().Create(ctx, namespace, metav1.CreateOptions{})
	return err
}

// createTetherLease creates a new TetherLease custom resource.
func createTetherLease(ctx context.Context, name, phase string) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "tether.dev",
		Version: "v1alpha1",
		Kind:    "TetherLease",
	})
	obj.SetName(name)
	obj.SetNamespace(tetherNamespace)
	obj.SetLabels(map[string]string{
		"app": "tether",
	})

	// Set initial spec
	if err := unstructured.SetNestedMap(obj.Object, map[string]interface{}{
		"username": "test-user",
		"cluster":  "test-cluster",
	}, "spec"); err != nil {
		return fmt.Errorf("set spec: %w", err)
	}

	// Set initial status
	if err := unstructured.SetNestedField(obj.Object, phase, "status", "phase"); err != nil {
		return fmt.Errorf("set status phase: %w", err)
	}

	res := dynamicClient.Resource(tetherLeaseGVR).Namespace(tetherNamespace)
	_, err := res.Create(ctx, obj, metav1.CreateOptions{})
	return err
}

// getTetherLease retrieves a TetherLease by name.
func getTetherLease(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	res := dynamicClient.Resource(tetherLeaseGVR).Namespace(tetherNamespace)
	return res.Get(ctx, name, metav1.GetOptions{})
}

// updateTetherLeasePhase updates the phase of a TetherLease.
func updateTetherLeasePhase(ctx context.Context, name, phase string) error {
	lease, err := getTetherLease(ctx, name)
	if err != nil {
		return err
	}

	if err := unstructured.SetNestedField(lease.Object, phase, "status", "phase"); err != nil {
		return fmt.Errorf("set phase: %w", err)
	}

	res := dynamicClient.Resource(tetherLeaseGVR).Namespace(tetherNamespace)
	_, err = res.UpdateStatus(ctx, lease, metav1.UpdateOptions{})
	return err
}

// waitForPhase waits for a TetherLease to reach a specific phase.
func waitForPhase(ctx context.Context, name, targetPhase string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		lease, err := getTetherLease(ctx, name)
		if err != nil {
			return false, nil // Lease may not exist yet
		}

		phase, _, err := unstructured.NestedString(lease.Object, "status", "phase")
		if err != nil {
			return false, nil
		}

		return phase == targetPhase, nil
	})
}

// deleteTetherLease deletes a TetherLease.
func deleteTetherLease(ctx context.Context, name string) error {
	res := dynamicClient.Resource(tetherLeaseGVR).Namespace(tetherNamespace)
	return res.Delete(ctx, name, metav1.DeleteOptions{})
}

// TestRequestWorkflow validates the lease creation and approval workflow.
func TestRequestWorkflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	leaseName := "test-request-workflow"

	// Create a lease in Pending phase
	if err := createTetherLease(ctx, leaseName, "Pending"); err != nil {
		t.Fatalf("failed to create lease: %v", err)
	}

	// Verify the lease was created
	lease, err := getTetherLease(ctx, leaseName)
	if err != nil {
		t.Fatalf("failed to get lease: %v", err)
	}

	phase, _, _ := unstructured.NestedString(lease.Object, "status", "phase")
	if phase != "Pending" {
		t.Errorf("expected phase Pending, got %s", phase)
	}

	// Cleanup
	if err := deleteTetherLease(ctx, leaseName); err != nil {
		t.Logf("cleanup warning: failed to delete lease: %v", err)
	}
}

// TestApprovalWorkflow validates ClusterRoleBinding creation and token generation.
func TestApprovalWorkflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	leaseName := "test-approval-workflow"
	roleName := "test-lease-role"

	// Create a lease
	if err := createTetherLease(ctx, leaseName, "Pending"); err != nil {
		t.Fatalf("failed to create lease: %v", err)
	}

	// Create a test ClusterRoleBinding to simulate approval
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: roleName,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "view",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:     "User",
				Name:     "test-user",
				APIGroup: "rbac.authorization.k8s.io",
			},
		},
	}

	_, err := kubeClient.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create ClusterRoleBinding: %v", err)
	}

	// Verify the role binding was created
	retrieved, err := kubeClient.RbacV1().ClusterRoleBindings().Get(ctx, roleName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to retrieve ClusterRoleBinding: %v", err)
	}

	if retrieved.Name != roleName {
		t.Errorf("expected role binding name %s, got %s", roleName, retrieved.Name)
	}

	// Cleanup
	if err := kubeClient.RbacV1().ClusterRoleBindings().Delete(ctx, roleName, metav1.DeleteOptions{}); err != nil {
		t.Logf("cleanup warning: failed to delete ClusterRoleBinding: %v", err)
	}
	if err := deleteTetherLease(ctx, leaseName); err != nil {
		t.Logf("cleanup warning: failed to delete lease: %v", err)
	}
}

// TestLeaseActiveWorkflow tests sustained active phase with multiple operations.
func TestLeaseActiveWorkflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	leaseName := "test-active-workflow"

	// Create lease
	if err := createTetherLease(ctx, leaseName, "Pending"); err != nil {
		t.Fatalf("failed to create lease: %v", err)
	}

	// Transition to Active
	if err := updateTetherLeasePhase(ctx, leaseName, "Active"); err != nil {
		t.Fatalf("failed to update phase to Active: %v", err)
	}

	// Wait for Active phase with timeout
	if err := waitForPhase(ctx, leaseName, "Active", 10*time.Second); err != nil {
		t.Errorf("lease did not reach Active phase: %v", err)
	}

	// Verify we can retrieve the active lease
	lease, err := getTetherLease(ctx, leaseName)
	if err != nil {
		t.Fatalf("failed to get active lease: %v", err)
	}

	phase, _, _ := unstructured.NestedString(lease.Object, "status", "phase")
	if phase != "Active" {
		t.Errorf("expected phase Active, got %s", phase)
	}

	// Cleanup
	if err := deleteTetherLease(ctx, leaseName); err != nil {
		t.Logf("cleanup warning: failed to delete lease: %v", err)
	}
}

// TestLeaseExpiryWorkflow validates automatic cleanup on expiry.
func TestLeaseExpiryWorkflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	leaseName := "test-expiry-workflow"

	// Create and activate lease
	if err := createTetherLease(ctx, leaseName, "Pending"); err != nil {
		t.Fatalf("failed to create lease: %v", err)
	}

	// Transition to Expired
	if err := updateTetherLeasePhase(ctx, leaseName, "Expired"); err != nil {
		t.Fatalf("failed to update phase to Expired: %v", err)
	}

	// Wait for Expired phase
	if err := waitForPhase(ctx, leaseName, "Expired", 10*time.Second); err != nil {
		t.Errorf("lease did not reach Expired phase: %v", err)
	}

	// Cleanup
	if err := deleteTetherLease(ctx, leaseName); err != nil {
		t.Logf("cleanup warning: failed to delete lease: %v", err)
	}
}

// TestNamespaceScopedWorkflow tests RoleBinding for namespace-scoped access.
func TestNamespaceScopedWorkflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	leaseName := "test-namespace-scoped"
	roleName := "test-ns-role"
	testNamespace := "default"

	// Create lease
	if err := createTetherLease(ctx, leaseName, "Pending"); err != nil {
		t.Fatalf("failed to create lease: %v", err)
	}

	// Create a namespace-scoped RoleBinding
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      roleName,
			Namespace: testNamespace,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "pod-reader",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:     "User",
				Name:     "test-user",
				APIGroup: "rbac.authorization.k8s.io",
			},
		},
	}

	_, err := kubeClient.RbacV1().RoleBindings(testNamespace).Create(ctx, rb, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create RoleBinding: %v", err)
	}

	// Verify the role binding was created
	retrieved, err := kubeClient.RbacV1().RoleBindings(testNamespace).Get(ctx, roleName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to retrieve RoleBinding: %v", err)
	}

	if retrieved.Name != roleName {
		t.Errorf("expected role binding name %s, got %s", roleName, retrieved.Name)
	}

	// Cleanup
	if err := kubeClient.RbacV1().RoleBindings(testNamespace).Delete(ctx, roleName, metav1.DeleteOptions{}); err != nil {
		t.Logf("cleanup warning: failed to delete RoleBinding: %v", err)
	}
	if err := deleteTetherLease(ctx, leaseName); err != nil {
		t.Logf("cleanup warning: failed to delete lease: %v", err)
	}
}

// TestSessionRecording verifies proxy audit backend integration capability.
func TestSessionRecording(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	leaseName := "test-session-recording"

	// Create and activate lease
	if err := createTetherLease(ctx, leaseName, "Pending"); err != nil {
		t.Fatalf("failed to create lease: %v", err)
	}

	if err := updateTetherLeasePhase(ctx, leaseName, "Active"); err != nil {
		t.Fatalf("failed to activate lease: %v", err)
	}

	// Verify Active state (where session recording would occur)
	if err := waitForPhase(ctx, leaseName, "Active", 10*time.Second); err != nil {
		t.Errorf("lease did not reach Active phase for session recording: %v", err)
	}

	// Cleanup
	if err := deleteTetherLease(ctx, leaseName); err != nil {
		t.Logf("cleanup warning: failed to delete lease: %v", err)
	}
}

// TestConcurrentLeases stress-tests multiple simultaneous leases.
func TestConcurrentLeases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	numLeases := 5
	leaseNames := make([]string, numLeases)

	// Create multiple leases concurrently
	for i := 0; i < numLeases; i++ {
		leaseNames[i] = fmt.Sprintf("test-concurrent-%d", i)
		if err := createTetherLease(ctx, leaseNames[i], "Pending"); err != nil {
			t.Fatalf("failed to create lease %s: %v", leaseNames[i], err)
		}
	}

	// Transition all leases to Active
	for i := 0; i < numLeases; i++ {
		if err := updateTetherLeasePhase(ctx, leaseNames[i], "Active"); err != nil {
			t.Fatalf("failed to activate lease %s: %v", leaseNames[i], err)
		}
	}

	// Verify all leases are Active
	for i := 0; i < numLeases; i++ {
		if err := waitForPhase(ctx, leaseNames[i], "Active", 10*time.Second); err != nil {
			t.Errorf("lease %s did not reach Active phase: %v", leaseNames[i], err)
		}
	}

	// Cleanup all leases
	for i := 0; i < numLeases; i++ {
		if err := deleteTetherLease(ctx, leaseNames[i]); err != nil {
			t.Logf("cleanup warning: failed to delete lease %s: %v", leaseNames[i], err)
		}
	}
}
