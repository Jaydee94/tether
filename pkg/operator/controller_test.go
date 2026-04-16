package operator

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("adding corev1 scheme: %v", err)
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

	r := &TetherLeaseReconciler{Client: cl, Scheme: scheme, TokenNamespace: "tether-system"}

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
	if updated.Status.TokenSecret == "" {
		t.Error("expected TokenSecret to be set")
	}

	crb := &rbacv1.ClusterRoleBinding{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: bindingPrefix + "test-lease"}, crb); err != nil {
		t.Fatalf("getting ClusterRoleBinding: %v", err)
	}
	if crb.Subjects[0].Name != "alice" {
		t.Errorf("expected subject alice, got %q", crb.Subjects[0].Name)
	}

	// Verify token Secret was created.
	tokenSecret := &corev1.Secret{}
	if err := cl.Get(context.Background(), types.NamespacedName{
		Name:      tokenPrefix + "test-lease",
		Namespace: "tether-system",
	}, tokenSecret); err != nil {
		t.Fatalf("getting token Secret: %v", err)
	}
	if len(tokenSecret.Data[TokenDataKey]) == 0 {
		t.Error("expected token Secret to have token data")
	}
	if tokenSecret.Labels[LabelTokenType] != TokenSecretType {
		t.Errorf("expected label %s=%s, got %s", LabelTokenType, TokenSecretType, tokenSecret.Labels[LabelTokenType])
	}
	if tokenSecret.Labels[LabelTokenLease] != "test-lease" {
		t.Errorf("expected label %s=test-lease, got %s", LabelTokenLease, tokenSecret.Labels[LabelTokenLease])
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
			TokenSecret: tokenPrefix + "expired-lease",
		},
	}

	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bindingPrefix + "expired-lease"},
	}
	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tokenPrefix + "expired-lease", Namespace: "tether-system"},
		Data:       map[string][]byte{TokenDataKey: []byte("some-token")},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(lease).WithObjects(lease, crb, tokenSecret).Build()
	r := &TetherLeaseReconciler{Client: cl, Scheme: scheme, TokenNamespace: "tether-system"}

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
	if updated.Status.TokenSecret != "" {
		t.Errorf("expected TokenSecret to be cleared after expiry, got %q", updated.Status.TokenSecret)
	}

	// Token Secret should be deleted.
	remaining := &corev1.Secret{}
	err = cl.Get(context.Background(), types.NamespacedName{Name: tokenPrefix + "expired-lease", Namespace: "tether-system"}, remaining)
	if err == nil {
		t.Error("expected token Secret to be deleted after expiry")
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
			TokenSecret: tokenPrefix + "revoked-lease",
		},
	}
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bindingPrefix + "revoked-lease"},
	}
	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tokenPrefix + "revoked-lease", Namespace: "tether-system"},
		Data:       map[string][]byte{TokenDataKey: []byte("some-token")},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(lease).WithObjects(lease, crb, tokenSecret).Build()
	r := &TetherLeaseReconciler{Client: cl, Scheme: scheme, TokenNamespace: "tether-system"}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "revoked-lease"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	existingCRB := &rbacv1.ClusterRoleBinding{}
	err = cl.Get(context.Background(), types.NamespacedName{Name: bindingPrefix + "revoked-lease"}, existingCRB)
	if err == nil {
		t.Error("expected ClusterRoleBinding to be deleted")
	}

	existingSecret := &corev1.Secret{}
	err = cl.Get(context.Background(), types.NamespacedName{Name: tokenPrefix + "revoked-lease", Namespace: "tether-system"}, existingSecret)
	if err == nil {
		t.Error("expected token Secret to be deleted after revocation")
	}
}

func TestReconcile_NamespaceScoped_RoleBinding(t *testing.T) {
	scheme := newTestScheme(t)

	lease := &tetherv1alpha1.TetherLease{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ns-lease",
		},
		Spec: tetherv1alpha1.TetherLeaseSpec{
			User:      "dave",
			Role:      "developer",
			Duration:  "30m",
			Reason:    "deploying hotfix",
			Namespace: "dev",
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(lease).WithObjects(lease).Build()
	r := &TetherLeaseReconciler{Client: cl, Scheme: scheme, TokenNamespace: "tether-system"}

	// First reconcile: adds finalizer
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "ns-lease"}})
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	// Second reconcile: activates lease
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "ns-lease"}})
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	updated := &tetherv1alpha1.TetherLease{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "ns-lease"}, updated); err != nil {
		t.Fatalf("getting lease: %v", err)
	}
	if updated.Status.Phase != tetherv1alpha1.PhaseActive {
		t.Errorf("expected Active, got %q", updated.Status.Phase)
	}

	// A RoleBinding should exist in the 'dev' namespace.
	rb := &rbacv1.RoleBinding{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: bindingPrefix + "ns-lease", Namespace: "dev"}, rb); err != nil {
		t.Fatalf("getting RoleBinding: %v", err)
	}
	if rb.Subjects[0].Name != "dave" {
		t.Errorf("expected subject dave, got %q", rb.Subjects[0].Name)
	}
	if rb.RoleRef.Name != "developer" {
		t.Errorf("expected roleRef developer, got %q", rb.RoleRef.Name)
	}

	// A ClusterRoleBinding should NOT exist.
	crb := &rbacv1.ClusterRoleBinding{}
	err = cl.Get(context.Background(), types.NamespacedName{Name: bindingPrefix + "ns-lease"}, crb)
	if err == nil {
		t.Error("expected no ClusterRoleBinding for a namespace-scoped lease")
	}
}

func TestGenerateSessionToken(t *testing.T) {
	tok1, err := generateSessionToken()
	if err != nil {
		t.Fatalf("generateSessionToken: %v", err)
	}
	if len(tok1) == 0 {
		t.Error("expected non-empty token")
	}

	tok2, err := generateSessionToken()
	if err != nil {
		t.Fatalf("generateSessionToken: %v", err)
	}
	if tok1 == tok2 {
		t.Error("expected tokens to be unique")
	}
}
