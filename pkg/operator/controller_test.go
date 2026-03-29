package operator

import (
	"context"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	tetherv1alpha1 "github.com/Jaydee94/tether/pkg/api/v1alpha1"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := tetherv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding tether scheme: %v", err)
	}
	if err := rbacv1.AddToScheme(s); err != nil {
		t.Fatalf("adding rbac scheme: %v", err)
	}
	return s
}

func TestReconcile_PendingToActive(t *testing.T) {
	scheme := newTestScheme(t)

	lease := &tetherv1alpha1.TetherLease{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-lease",
		},
		Spec: tetherv1alpha1.TetherLeaseSpec{
			User:     "alice",
			Role:     "cluster-admin",
			Duration: "1h",
			Reason:   "testing",
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(lease).WithObjects(lease).Build()

	r := &TetherLeaseReconciler{Client: cl, Scheme: scheme}

	// First reconcile: adds finalizer
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-lease"}})
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	// Second reconcile: activates lease
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-lease"}})
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	updated := &tetherv1alpha1.TetherLease{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "test-lease"}, updated); err != nil {
		t.Fatalf("getting lease: %v", err)
	}
	if updated.Status.Phase != tetherv1alpha1.PhaseActive {
		t.Errorf("expected Active, got %q", updated.Status.Phase)
	}
	if updated.Status.ExpiresAt == nil {
		t.Error("expected ExpiresAt to be set")
	}

	crb := &rbacv1.ClusterRoleBinding{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: bindingPrefix + "test-lease"}, crb); err != nil {
		t.Fatalf("getting ClusterRoleBinding: %v", err)
	}
	if crb.Subjects[0].Name != "alice" {
		t.Errorf("expected subject alice, got %q", crb.Subjects[0].Name)
	}
}

func TestReconcile_ActiveExpiry(t *testing.T) {
	scheme := newTestScheme(t)

	past := metav1.NewTime(time.Now().Add(-1 * time.Second))
	lease := &tetherv1alpha1.TetherLease{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "expired-lease",
			Finalizers: []string{finalizerName},
		},
		Spec: tetherv1alpha1.TetherLeaseSpec{
			User:     "bob",
			Role:     "view",
			Duration: "1h",
		},
		Status: tetherv1alpha1.TetherLeaseStatus{
			Phase:       tetherv1alpha1.PhaseActive,
			ExpiresAt:   &past,
			BindingName: bindingPrefix + "expired-lease",
		},
	}

	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bindingPrefix + "expired-lease"},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(lease).WithObjects(lease, crb).Build()
	r := &TetherLeaseReconciler{Client: cl, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "expired-lease"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	updated := &tetherv1alpha1.TetherLease{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "expired-lease"}, updated); err != nil {
		t.Fatalf("getting lease: %v", err)
	}
	if updated.Status.Phase != tetherv1alpha1.PhaseExpired {
		t.Errorf("expected Expired, got %q", updated.Status.Phase)
	}
}

func TestReconcile_Revoked(t *testing.T) {
	scheme := newTestScheme(t)

	lease := &tetherv1alpha1.TetherLease{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "revoked-lease",
			Finalizers: []string{finalizerName},
		},
		Spec: tetherv1alpha1.TetherLeaseSpec{
			User: "charlie", Role: "edit", Duration: "1h",
		},
		Status: tetherv1alpha1.TetherLeaseStatus{
			Phase:       tetherv1alpha1.PhaseRevoked,
			BindingName: bindingPrefix + "revoked-lease",
		},
	}
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bindingPrefix + "revoked-lease"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(lease).WithObjects(lease, crb).Build()
	r := &TetherLeaseReconciler{Client: cl, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "revoked-lease"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	existing := &rbacv1.ClusterRoleBinding{}
	err = cl.Get(context.Background(), types.NamespacedName{Name: bindingPrefix + "revoked-lease"}, existing)
	if err == nil {
		t.Error("expected ClusterRoleBinding to be deleted")
	}
}
