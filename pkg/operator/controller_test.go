package operator

import (
	"context"
	"strings"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

func expectEventContains(t *testing.T, events <-chan string, want string) {
	t.Helper()

	select {
	case event := <-events:
		if !strings.Contains(event, want) {
			t.Fatalf("expected event containing %q, got %q", want, event)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("timed out waiting for event containing %q", want)
	}
}

func expectNoClusterRoleBinding(t *testing.T, cl client.Client, leaseName string) {
	t.Helper()
	crb := &rbacv1.ClusterRoleBinding{}
	err := cl.Get(context.Background(), types.NamespacedName{Name: bindingPrefix + leaseName}, crb)
	if err == nil {
		t.Fatalf("expected no ClusterRoleBinding for lease %q", leaseName)
	}
	if !k8serrors.IsNotFound(err) {
		t.Fatalf("expected not found for ClusterRoleBinding, got: %v", err)
	}
}

func TestReconcile_PendingToActive(t *testing.T) {
	scheme := newTestScheme(t)

	lease := &tetherv1alpha1.TetherLease{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-lease",
			Generation: 3,
		},
		Spec: tetherv1alpha1.TetherLeaseSpec{
			User:     "alice",
			Role:     "cluster-admin",
			Duration: "1h",
			Reason:   "testing",
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(lease).WithObjects(lease).Build()
	recorder := record.NewFakeRecorder(10)

	r := &TetherLeaseReconciler{Client: cl, Scheme: scheme, Recorder: recorder}

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
	if updated.Status.ObservedGeneration != updated.Generation {
		t.Errorf("expected observedGeneration %d, got %d", updated.Generation, updated.Status.ObservedGeneration)
	}

	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tetherv1alpha1.ConditionReady)
	if ready == nil {
		t.Fatal("expected Ready condition")
	}
	if ready.Status != metav1.ConditionTrue {
		t.Errorf("expected Ready=True, got %q", ready.Status)
	}
	if ready.Reason != tetherv1alpha1.ReasonActivated {
		t.Errorf("expected reason %q, got %q", tetherv1alpha1.ReasonActivated, ready.Reason)
	}
	if ready.ObservedGeneration != updated.Generation {
		t.Errorf("expected condition observedGeneration %d, got %d", updated.Generation, ready.ObservedGeneration)
	}

	crb := &rbacv1.ClusterRoleBinding{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: bindingPrefix + "test-lease"}, crb); err != nil {
		t.Fatalf("getting ClusterRoleBinding: %v", err)
	}
	if crb.Subjects[0].Name != "alice" {
		t.Errorf("expected subject alice, got %q", crb.Subjects[0].Name)
	}

	expectEventContains(t, recorder.Events, tetherv1alpha1.ReasonActivated)
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
	recorder := record.NewFakeRecorder(10)
	r := &TetherLeaseReconciler{Client: cl, Scheme: scheme, Recorder: recorder}

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

	expectEventContains(t, recorder.Events, tetherv1alpha1.ReasonExpired)
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
	recorder := record.NewFakeRecorder(10)
	r := &TetherLeaseReconciler{Client: cl, Scheme: scheme, Recorder: recorder}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "revoked-lease"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	existing := &rbacv1.ClusterRoleBinding{}
	err = cl.Get(context.Background(), types.NamespacedName{Name: bindingPrefix + "revoked-lease"}, existing)
	if err == nil {
		t.Error("expected ClusterRoleBinding to be deleted")
	}

	expectEventContains(t, recorder.Events, tetherv1alpha1.ReasonRevoked)
}

func TestReconcile_InvalidDurationSetsReadyFalse(t *testing.T) {
	scheme := newTestScheme(t)

	lease := &tetherv1alpha1.TetherLease{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "bad-duration",
			Generation: 7,
		},
		Spec: tetherv1alpha1.TetherLeaseSpec{
			User:     "alice",
			Role:     "cluster-admin",
			Duration: "definitely-not-a-duration",
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(lease).WithObjects(lease).Build()
	recorder := record.NewFakeRecorder(10)
	r := &TetherLeaseReconciler{Client: cl, Scheme: scheme, Recorder: recorder}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "bad-duration"}})
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "bad-duration"}})
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	updated := &tetherv1alpha1.TetherLease{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "bad-duration"}, updated); err != nil {
		t.Fatalf("getting lease: %v", err)
	}

	if updated.Status.ObservedGeneration != updated.Generation {
		t.Errorf("expected observedGeneration %d, got %d", updated.Generation, updated.Status.ObservedGeneration)
	}

	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tetherv1alpha1.ConditionReady)
	if ready == nil {
		t.Fatal("expected Ready condition")
	}
	if ready.Status != metav1.ConditionFalse {
		t.Errorf("expected Ready=False, got %q", ready.Status)
	}
	if ready.Reason != tetherv1alpha1.ReasonInvalidDuration {
		t.Errorf("expected reason %q, got %q", tetherv1alpha1.ReasonInvalidDuration, ready.Reason)
	}
	if !strings.Contains(ready.Message, "invalid duration") {
		t.Errorf("expected error message to mention invalid duration, got %q", ready.Message)
	}
	if ready.ObservedGeneration != updated.Generation {
		t.Errorf("expected condition observedGeneration %d, got %d", updated.Generation, ready.ObservedGeneration)
	}
	if updated.Status.Phase == tetherv1alpha1.PhaseActive {
		t.Fatalf("expected rejected lease to stay non-Active")
	}

	expectNoClusterRoleBinding(t, cl, "bad-duration")

	expectEventContains(t, recorder.Events, tetherv1alpha1.ReasonInvalidDuration)
}

func TestReconcile_InvalidRoleRejectedBeforeRBAC(t *testing.T) {
	scheme := newTestScheme(t)

	lease := &tetherv1alpha1.TetherLease{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "bad-role",
			Generation: 5,
		},
		Spec: tetherv1alpha1.TetherLeaseSpec{
			User:     "alice",
			Role:     "evil-superuser",
			Duration: "30m",
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(lease).WithObjects(lease).Build()
	recorder := record.NewFakeRecorder(10)
	r := &TetherLeaseReconciler{Client: cl, Scheme: scheme, Recorder: recorder}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "bad-role"}})
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "bad-role"}})
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	updated := &tetherv1alpha1.TetherLease{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "bad-role"}, updated); err != nil {
		t.Fatalf("getting lease: %v", err)
	}
	if updated.Status.Phase == tetherv1alpha1.PhaseActive {
		t.Fatalf("expected rejected lease to stay non-Active")
	}

	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tetherv1alpha1.ConditionReady)
	if ready == nil {
		t.Fatal("expected Ready condition")
	}
	if ready.Reason != tetherv1alpha1.ReasonInvalidRole {
		t.Errorf("expected reason %q, got %q", tetherv1alpha1.ReasonInvalidRole, ready.Reason)
	}

	expectNoClusterRoleBinding(t, cl, "bad-role")
	expectEventContains(t, recorder.Events, tetherv1alpha1.ReasonInvalidRole)
}

func TestReconcile_ReservedUserRejectedBeforeRBAC(t *testing.T) {
	scheme := newTestScheme(t)

	lease := &tetherv1alpha1.TetherLease{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "bad-user",
			Generation: 9,
		},
		Spec: tetherv1alpha1.TetherLeaseSpec{
			User:     "system:admin",
			Role:     "view",
			Duration: "30m",
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(lease).WithObjects(lease).Build()
	recorder := record.NewFakeRecorder(10)
	r := &TetherLeaseReconciler{Client: cl, Scheme: scheme, Recorder: recorder}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "bad-user"}})
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "bad-user"}})
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	updated := &tetherv1alpha1.TetherLease{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "bad-user"}, updated); err != nil {
		t.Fatalf("getting lease: %v", err)
	}
	if updated.Status.Phase == tetherv1alpha1.PhaseActive {
		t.Fatalf("expected rejected lease to stay non-Active")
	}

	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tetherv1alpha1.ConditionReady)
	if ready == nil {
		t.Fatal("expected Ready condition")
	}
	if ready.Reason != tetherv1alpha1.ReasonInvalidUser {
		t.Errorf("expected reason %q, got %q", tetherv1alpha1.ReasonInvalidUser, ready.Reason)
	}

	expectNoClusterRoleBinding(t, cl, "bad-user")
	expectEventContains(t, recorder.Events, tetherv1alpha1.ReasonInvalidUser)
}

func TestReconcile_DurationOverMaxRejectedBeforeRBAC(t *testing.T) {
	scheme := newTestScheme(t)

	lease := &tetherv1alpha1.TetherLease{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "too-long",
			Generation: 11,
		},
		Spec: tetherv1alpha1.TetherLeaseSpec{
			User:     "alice",
			Role:     "view",
			Duration: "9h",
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(lease).WithObjects(lease).Build()
	recorder := record.NewFakeRecorder(10)
	r := &TetherLeaseReconciler{Client: cl, Scheme: scheme, Recorder: recorder}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "too-long"}})
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "too-long"}})
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	updated := &tetherv1alpha1.TetherLease{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "too-long"}, updated); err != nil {
		t.Fatalf("getting lease: %v", err)
	}
	if updated.Status.Phase == tetherv1alpha1.PhaseActive {
		t.Fatalf("expected rejected lease to stay non-Active")
	}

	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tetherv1alpha1.ConditionReady)
	if ready == nil {
		t.Fatal("expected Ready condition")
	}
	if ready.Reason != tetherv1alpha1.ReasonDurationTooLong {
		t.Errorf("expected reason %q, got %q", tetherv1alpha1.ReasonDurationTooLong, ready.Reason)
	}

	expectNoClusterRoleBinding(t, cl, "too-long")
	expectEventContains(t, recorder.Events, tetherv1alpha1.ReasonDurationTooLong)
}

func TestReconcile_DurationTooShortRejectedBeforeRBAC(t *testing.T) {
	scheme := newTestScheme(t)

	lease := &tetherv1alpha1.TetherLease{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "too-short",
			Generation: 12,
		},
		Spec: tetherv1alpha1.TetherLeaseSpec{
			User:     "alice",
			Role:     "view",
			Duration: "59s",
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(lease).WithObjects(lease).Build()
	recorder := record.NewFakeRecorder(10)
	r := &TetherLeaseReconciler{Client: cl, Scheme: scheme, Recorder: recorder}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "too-short"}})
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "too-short"}})
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	updated := &tetherv1alpha1.TetherLease{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "too-short"}, updated); err != nil {
		t.Fatalf("getting lease: %v", err)
	}

	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tetherv1alpha1.ConditionReady)
	if ready == nil {
		t.Fatal("expected Ready condition")
	}
	if ready.Status != metav1.ConditionFalse {
		t.Errorf("expected Ready=False, got %q", ready.Status)
	}
	if ready.Reason != tetherv1alpha1.ReasonDurationTooShort {
		t.Errorf("expected reason %q, got %q", tetherv1alpha1.ReasonDurationTooShort, ready.Reason)
	}
	if updated.Status.Phase == tetherv1alpha1.PhaseActive {
		t.Fatalf("expected rejected lease to stay non-Active")
	}

	expectNoClusterRoleBinding(t, cl, "too-short")
	expectEventContains(t, recorder.Events, tetherv1alpha1.ReasonDurationTooShort)
}

func TestReconcile_ValidDurationBoundsActivateLease(t *testing.T) {
	scheme := newTestScheme(t)

	testCases := []struct {
		name     string
		duration string
	}{
		{name: "min-boundary", duration: "1m"},
		{name: "max-boundary", duration: "8h"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			lease := &tetherv1alpha1.TetherLease{
				ObjectMeta: metav1.ObjectMeta{
					Name:       tc.name,
					Generation: 3,
				},
				Spec: tetherv1alpha1.TetherLeaseSpec{
					User:     "alice",
					Role:     "view",
					Duration: tc.duration,
				},
			}

			cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(lease).WithObjects(lease).Build()
			recorder := record.NewFakeRecorder(10)
			r := &TetherLeaseReconciler{Client: cl, Scheme: scheme, Recorder: recorder}

			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: tc.name}})
			if err != nil {
				t.Fatalf("reconcile 1: %v", err)
			}

			_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: tc.name}})
			if err != nil {
				t.Fatalf("reconcile 2: %v", err)
			}

			updated := &tetherv1alpha1.TetherLease{}
			if err := cl.Get(context.Background(), types.NamespacedName{Name: tc.name}, updated); err != nil {
				t.Fatalf("getting lease: %v", err)
			}
			if updated.Status.Phase != tetherv1alpha1.PhaseActive {
				t.Fatalf("expected Active, got %q", updated.Status.Phase)
			}

			ready := apimeta.FindStatusCondition(updated.Status.Conditions, tetherv1alpha1.ConditionReady)
			if ready == nil {
				t.Fatal("expected Ready condition")
			}
			if ready.Reason != tetherv1alpha1.ReasonActivated {
				t.Fatalf("expected reason %q, got %q", tetherv1alpha1.ReasonActivated, ready.Reason)
			}

			expectEventContains(t, recorder.Events, tetherv1alpha1.ReasonActivated)
		})
	}
}

// ---- Approval workflow tests ----

func TestReconcile_PendingWithApprovers_GoesToPendingApproval(t *testing.T) {
	scheme := newTestScheme(t)

	lease := &tetherv1alpha1.TetherLease{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "approval-lease",
			Generation: 1,
		},
		Spec: tetherv1alpha1.TetherLeaseSpec{
			User:      "carol",
			Role:      "view",
			Duration:  "30m",
			Approvers: []string{"alice", "bob"},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(lease).WithObjects(lease).Build()
	recorder := record.NewFakeRecorder(10)
	r := &TetherLeaseReconciler{Client: cl, Scheme: scheme, Recorder: recorder}

	// First reconcile: adds finalizer
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "approval-lease"}})
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	// Second reconcile: should gate to PendingApproval
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "approval-lease"}})
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	updated := &tetherv1alpha1.TetherLease{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "approval-lease"}, updated); err != nil {
		t.Fatalf("getting lease: %v", err)
	}
	if updated.Status.Phase != tetherv1alpha1.PhasePendingApproval {
		t.Errorf("expected PendingApproval, got %q", updated.Status.Phase)
	}

	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tetherv1alpha1.ConditionReady)
	if ready == nil {
		t.Fatal("expected Ready condition")
	}
	if ready.Status != metav1.ConditionFalse {
		t.Errorf("expected Ready=False, got %q", ready.Status)
	}
	if ready.Reason != tetherv1alpha1.ReasonPendingApproval {
		t.Errorf("expected reason %q, got %q", tetherv1alpha1.ReasonPendingApproval, ready.Reason)
	}

	// No CRB should be created
	expectNoClusterRoleBinding(t, cl, "approval-lease")

	expectEventContains(t, recorder.Events, tetherv1alpha1.ReasonPendingApproval)
}

func TestReconcile_PendingApproval_ApprovedActivatesLease(t *testing.T) {
	scheme := newTestScheme(t)

	lease := &tetherv1alpha1.TetherLease{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "approved-lease",
			Generation: 1,
			Finalizers: []string{finalizerName},
		},
		Spec: tetherv1alpha1.TetherLeaseSpec{
			User:      "carol",
			Role:      "view",
			Duration:  "30m",
			Approvers: []string{"alice"},
		},
		Status: tetherv1alpha1.TetherLeaseStatus{
			Phase:      tetherv1alpha1.PhasePendingApproval,
			ApprovedBy: "alice", // simulates tetherctl approve having been called
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(lease).WithObjects(lease).Build()
	recorder := record.NewFakeRecorder(10)
	r := &TetherLeaseReconciler{Client: cl, Scheme: scheme, Recorder: recorder}

	// First reconcile from PendingApproval: sees ApprovedBy, resets to Pending
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "approved-lease"}})
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	// Second reconcile: should now activate (ApprovedBy is set, skips approver gate)
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "approved-lease"}})
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	updated := &tetherv1alpha1.TetherLease{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "approved-lease"}, updated); err != nil {
		t.Fatalf("getting lease: %v", err)
	}
	if updated.Status.Phase != tetherv1alpha1.PhaseActive {
		t.Errorf("expected Active, got %q", updated.Status.Phase)
	}
	if updated.Status.ExpiresAt == nil {
		t.Error("expected ExpiresAt to be set")
	}

	expectEventContains(t, recorder.Events, tetherv1alpha1.ReasonActivated)
}

func TestReconcile_PendingApproval_DeniedLease(t *testing.T) {
	scheme := newTestScheme(t)

	lease := &tetherv1alpha1.TetherLease{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "denied-lease",
			Generation: 1,
			Finalizers: []string{finalizerName},
		},
		Spec: tetherv1alpha1.TetherLeaseSpec{
			User:      "carol",
			Role:      "view",
			Duration:  "30m",
			Approvers: []string{"alice"},
		},
		Status: tetherv1alpha1.TetherLeaseStatus{
			Phase:    tetherv1alpha1.PhaseDenied,
			DeniedBy: "alice",
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(lease).WithObjects(lease).Build()
	recorder := record.NewFakeRecorder(10)
	r := &TetherLeaseReconciler{Client: cl, Scheme: scheme, Recorder: recorder}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "denied-lease"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	updated := &tetherv1alpha1.TetherLease{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "denied-lease"}, updated); err != nil {
		t.Fatalf("getting lease: %v", err)
	}
	// Denied leases should remain in Denied phase (terminal state)
	if updated.Status.Phase != tetherv1alpha1.PhaseDenied {
		t.Errorf("expected Denied, got %q", updated.Status.Phase)
	}

	// No CRB should be created
	expectNoClusterRoleBinding(t, cl, "denied-lease")
}

func TestReconcile_PendingApproval_HoldsUntilActed(t *testing.T) {
	scheme := newTestScheme(t)

	lease := &tetherv1alpha1.TetherLease{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "waiting-lease",
			Generation: 1,
			Finalizers: []string{finalizerName},
		},
		Spec: tetherv1alpha1.TetherLeaseSpec{
			User:      "carol",
			Role:      "view",
			Duration:  "30m",
			Approvers: []string{"alice"},
		},
		Status: tetherv1alpha1.TetherLeaseStatus{
			Phase: tetherv1alpha1.PhasePendingApproval,
			// No ApprovedBy or DeniedBy: still waiting
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(lease).WithObjects(lease).Build()
	recorder := record.NewFakeRecorder(10)
	r := &TetherLeaseReconciler{Client: cl, Scheme: scheme, Recorder: recorder}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "waiting-lease"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	updated := &tetherv1alpha1.TetherLease{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "waiting-lease"}, updated); err != nil {
		t.Fatalf("getting lease: %v", err)
	}
	// Should stay in PendingApproval
	if updated.Status.Phase != tetherv1alpha1.PhasePendingApproval {
		t.Errorf("expected PendingApproval, got %q", updated.Status.Phase)
	}

	// No CRB should be created
	expectNoClusterRoleBinding(t, cl, "waiting-lease")
}
