package operator

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	rbacv1 "k8s.io/api/rbac/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	tetherv1alpha1 "github.com/Jaydee94/tether/pkg/api/v1alpha1"
)

const (
	finalizerName    = "tether.dev/cleanup"
	bindingPrefix    = "tether-lease-"
	minLeaseDuration = 1 * time.Minute
	maxLeaseDuration = 8 * time.Hour
)

var (
	allowedClusterRoles = map[string]struct{}{
		"view":          {},
		"edit":          {},
		"admin":         {},
		"cluster-admin": {},
	}
	allowedRolePattern = regexp.MustCompile(`^tether-[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	allowedUserPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._:-]{2,127}$`)
)

var reconcileOutcomesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "tether_operator_reconcile_outcomes_total",
		Help: "Count of reconcile lifecycle outcomes by result and reason.",
	},
	[]string{"outcome", "reason"},
)

func init() {
	metrics.Registry.MustRegister(reconcileOutcomesTotal)
}

// +kubebuilder:rbac:groups=tether.dev,resources=tetherleases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tether.dev,resources=tetherleases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tether.dev,resources=tetherleases/finalizers,verbs=update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;patch;delete

// TetherLeaseReconciler reconciles TetherLease objects.
type TetherLeaseReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// SetupWithManager sets up the controller with the Manager.
func (r *TetherLeaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tetherv1alpha1.TetherLease{}).
		Owns(&rbacv1.ClusterRoleBinding{}).
		Complete(r)
}

// Reconcile handles TetherLease state transitions.
func (r *TetherLeaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("lease", req.Name, "namespace", req.Namespace)

	lease := &tetherv1alpha1.TetherLease{}
	if err := r.Get(ctx, req.NamespacedName, lease); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		r.recordOutcome("error", "get_failed")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !lease.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, lease)
	}

	// Ensure finalizer is present
	if !controllerutil.ContainsFinalizer(lease, finalizerName) {
		controllerutil.AddFinalizer(lease, finalizerName)
		if err := r.Update(ctx, lease); err != nil {
			r.recordOutcome("error", "finalizer_update_failed")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	switch lease.Status.Phase {
	case "", tetherv1alpha1.PhasePending:
		logger.Info("Reconciling pending lease activation", "user", lease.Spec.User, "role", lease.Spec.Role)
		return r.activateLease(ctx, lease)
	case tetherv1alpha1.PhasePendingApproval:
		return r.reconcilePendingApproval(ctx, lease)
	case tetherv1alpha1.PhaseActive:
		return r.reconcileActive(ctx, lease)
	case tetherv1alpha1.PhaseRevoked:
		return r.handleRevoked(ctx, lease)
	case tetherv1alpha1.PhaseExpired, tetherv1alpha1.PhaseDenied:
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

func (r *TetherLeaseReconciler) activateLease(ctx context.Context, lease *tetherv1alpha1.TetherLease) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	fromPhase := lease.Status.Phase

	// If approvers are configured AND not yet approved, gate the lease into PendingApproval
	if len(lease.Spec.Approvers) > 0 && lease.Status.ApprovedBy == "" {
		message := fmt.Sprintf("waiting for approval from: %s", strings.Join(lease.Spec.Approvers, ", "))
		logger.Info("Lease requires approval, holding in PendingApproval", "approvers", lease.Spec.Approvers)
		if err := r.updateLeaseStatus(ctx, lease, func(status *tetherv1alpha1.TetherLeaseStatus) {
			status.Phase = tetherv1alpha1.PhasePendingApproval
			status.ObservedGeneration = lease.Generation
			apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
				Type:               tetherv1alpha1.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             tetherv1alpha1.ReasonPendingApproval,
				Message:            message,
				ObservedGeneration: lease.Generation,
			})
		}); err != nil {
			r.recordOutcome("error", "status_update_failed")
			return ctrl.Result{}, err
		}
		r.logTransition(logger, lease, fromPhase, tetherv1alpha1.PhasePendingApproval, tetherv1alpha1.ReasonPendingApproval)
		r.emitEvent(lease, "Normal", tetherv1alpha1.ReasonPendingApproval, message)
		r.recordOutcome("pending_approval", tetherv1alpha1.ReasonPendingApproval)
		return ctrl.Result{}, nil
	}

	if err := validateLeaseRole(lease.Spec.Role); err != nil {
		message := fmt.Sprintf("invalid role %q: %v", lease.Spec.Role, err)
		logger.Error(err, "Validation failed for lease role", "role", lease.Spec.Role, "reason", tetherv1alpha1.ReasonInvalidRole)
		r.emitEvent(lease, "Warning", tetherv1alpha1.ReasonInvalidRole, message)
		r.recordOutcome("validation_failed", tetherv1alpha1.ReasonInvalidRole)
		if statusErr := r.updateRejectedLeaseStatus(ctx, lease, tetherv1alpha1.ReasonInvalidRole, message); statusErr != nil {
			logger.Error(statusErr, "Failed to update status condition for invalid role", "lease", lease.Name)
		}
		return ctrl.Result{}, nil
	}

	if err := validateLeaseUser(lease.Spec.User); err != nil {
		message := fmt.Sprintf("invalid user %q: %v", lease.Spec.User, err)
		logger.Error(err, "Validation failed for lease user", "user", lease.Spec.User, "reason", tetherv1alpha1.ReasonInvalidUser)
		r.emitEvent(lease, "Warning", tetherv1alpha1.ReasonInvalidUser, message)
		r.recordOutcome("validation_failed", tetherv1alpha1.ReasonInvalidUser)
		if statusErr := r.updateRejectedLeaseStatus(ctx, lease, tetherv1alpha1.ReasonInvalidUser, message); statusErr != nil {
			logger.Error(statusErr, "Failed to update status condition for invalid user", "lease", lease.Name)
		}
		return ctrl.Result{}, nil
	}

	duration, err := time.ParseDuration(lease.Spec.Duration)
	if err != nil {
		message := fmt.Sprintf("invalid duration %q: %v", lease.Spec.Duration, err)
		logger.Error(err, "Validation failed for lease duration", "duration", lease.Spec.Duration, "reason", tetherv1alpha1.ReasonInvalidDuration)
		r.emitEvent(lease, "Warning", tetherv1alpha1.ReasonInvalidDuration, message)
		r.recordOutcome("validation_failed", tetherv1alpha1.ReasonInvalidDuration)
		if statusErr := r.updateRejectedLeaseStatus(ctx, lease, tetherv1alpha1.ReasonInvalidDuration, message); statusErr != nil {
			logger.Error(statusErr, "Failed to update status condition for invalid duration", "lease", lease.Name)
		}
		return ctrl.Result{}, nil
	}

	if err := validateLeaseDuration(duration); err != nil {
		reason := tetherv1alpha1.ReasonDurationTooLong
		if duration < minLeaseDuration {
			reason = tetherv1alpha1.ReasonDurationTooShort
		}
		message := fmt.Sprintf("invalid duration %q: %v", lease.Spec.Duration, err)
		logger.Error(err, "Validation failed for lease duration policy", "duration", lease.Spec.Duration, "reason", reason)
		r.emitEvent(lease, "Warning", reason, message)
		r.recordOutcome("validation_failed", reason)
		if statusErr := r.updateRejectedLeaseStatus(ctx, lease, reason, message); statusErr != nil {
			logger.Error(statusErr, "Failed to update status condition for duration policy", "lease", lease.Name)
		}
		return ctrl.Result{}, nil
	}

	bindingName := bindingPrefix + lease.Name
	crb := r.buildClusterRoleBinding(lease, bindingName)

	if err := r.createOrUpdateClusterRoleBinding(ctx, crb); err != nil {
		logger.Error(err, "Activation failed while reconciling ClusterRoleBinding", "binding", bindingName, "reason", tetherv1alpha1.ReasonActivationFailed)
		r.emitEvent(lease, "Warning", tetherv1alpha1.ReasonActivationFailed, err.Error())
		r.recordOutcome("activation_failed", tetherv1alpha1.ReasonActivationFailed)
		if statusErr := r.setReadyCondition(ctx, lease, metav1.ConditionFalse, tetherv1alpha1.ReasonActivationFailed, err.Error()); statusErr != nil {
			logger.Error(statusErr, "Failed to update status condition for activation failure", "lease", lease.Name)
		}
		return ctrl.Result{}, err
	}

	expiresAt := metav1.NewTime(time.Now().Add(duration))
	if err := r.updateLeaseStatus(ctx, lease, func(status *tetherv1alpha1.TetherLeaseStatus) {
		status.Phase = tetherv1alpha1.PhaseActive
		status.ExpiresAt = &expiresAt
		status.BindingName = bindingName
		status.ObservedGeneration = lease.Generation
		apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type:               tetherv1alpha1.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             tetherv1alpha1.ReasonActivated,
			Message:            "lease activated",
			ObservedGeneration: lease.Generation,
		})
	}); err != nil {
		r.recordOutcome("error", "status_update_failed")
		return ctrl.Result{}, err
	}

	r.logTransition(logger, lease, fromPhase, tetherv1alpha1.PhaseActive, tetherv1alpha1.ReasonActivated)
	r.emitEvent(lease, "Normal", tetherv1alpha1.ReasonActivated, "lease activated")
	r.recordOutcome("activated", tetherv1alpha1.ReasonActivated)
	logger.Info("Lease activated", "expiresAt", expiresAt.Time)
	return ctrl.Result{RequeueAfter: duration}, nil
}

func (r *TetherLeaseReconciler) reconcilePendingApproval(ctx context.Context, lease *tetherv1alpha1.TetherLease) (ctrl.Result, error) {
	// Lease is waiting for human approval via tetherctl approve/deny.
	// The controller just holds here; status transitions happen via status patches from tetherctl.
	// Re-check: if ApprovedBy is set, proceed to activation.
	if lease.Status.ApprovedBy != "" {
		logger := log.FromContext(ctx)
		logger.Info("Lease approved, proceeding to activation", "approvedBy", lease.Status.ApprovedBy)
		// Clear the phase so activateLease will run (skipping approver gate since we have ApprovedBy).
		if err := r.updateLeaseStatus(ctx, lease, func(status *tetherv1alpha1.TetherLeaseStatus) {
			status.Phase = tetherv1alpha1.PhasePending
			status.ObservedGeneration = lease.Generation
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	// If DeniedBy is set, the lease was denied via tetherctl deny — phase should already be Denied.
	// No-op: just wait.
	return ctrl.Result{}, nil
}

func (r *TetherLeaseReconciler) reconcileActive(ctx context.Context, lease *tetherv1alpha1.TetherLease) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if lease.Status.ExpiresAt == nil {
		logger.Info("Active lease missing expiresAt, re-activating", "reason", tetherv1alpha1.ReasonActivationFailed)
		r.emitEvent(lease, "Warning", tetherv1alpha1.ReasonActivationFailed, "active lease missing expiresAt")
		r.recordOutcome("activation_failed", tetherv1alpha1.ReasonActivationFailed)
		if err := r.updateLeaseStatus(ctx, lease, func(status *tetherv1alpha1.TetherLeaseStatus) {
			status.Phase = tetherv1alpha1.PhasePending
			status.ObservedGeneration = lease.Generation
			apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
				Type:               tetherv1alpha1.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             tetherv1alpha1.ReasonActivationFailed,
				Message:            "active lease missing expiresAt",
				ObservedGeneration: lease.Generation,
			})
		}); err != nil {
			r.recordOutcome("error", "status_update_failed")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	remaining := time.Until(lease.Status.ExpiresAt.Time)
	if remaining <= 0 {
		logger.Info("Lease expired, cleaning up", "reason", tetherv1alpha1.ReasonExpired)
		return r.expireLease(ctx, lease)
	}

	return ctrl.Result{RequeueAfter: remaining}, nil
}

func (r *TetherLeaseReconciler) expireLease(ctx context.Context, lease *tetherv1alpha1.TetherLease) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	fromPhase := lease.Status.Phase

	if err := r.deleteClusterRoleBinding(ctx, lease.Status.BindingName); err != nil {
		r.recordOutcome("error", "binding_delete_failed")
		return ctrl.Result{}, err
	}
	if err := r.updateLeaseStatus(ctx, lease, func(status *tetherv1alpha1.TetherLeaseStatus) {
		status.Phase = tetherv1alpha1.PhaseExpired
		status.ObservedGeneration = lease.Generation
		apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type:               tetherv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             tetherv1alpha1.ReasonExpired,
			Message:            "lease expired",
			ObservedGeneration: lease.Generation,
		})
	}); err != nil {
		r.recordOutcome("error", "status_update_failed")
		return ctrl.Result{}, err
	}
	r.logTransition(logger, lease, fromPhase, tetherv1alpha1.PhaseExpired, tetherv1alpha1.ReasonExpired)
	r.emitEvent(lease, "Normal", tetherv1alpha1.ReasonExpired, "lease expired")
	r.recordOutcome("expired", tetherv1alpha1.ReasonExpired)
	return ctrl.Result{}, nil
}

func (r *TetherLeaseReconciler) handleRevoked(ctx context.Context, lease *tetherv1alpha1.TetherLease) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	fromPhase := lease.Status.Phase

	updatedBinding := false
	if lease.Status.BindingName != "" {
		if err := r.deleteClusterRoleBinding(ctx, lease.Status.BindingName); err != nil {
			r.recordOutcome("error", "binding_delete_failed")
			return ctrl.Result{}, err
		}
		updatedBinding = true
	}

	if err := r.updateLeaseStatus(ctx, lease, func(status *tetherv1alpha1.TetherLeaseStatus) {
		if updatedBinding {
			status.BindingName = ""
		}
		status.ObservedGeneration = lease.Generation
		apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type:               tetherv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             tetherv1alpha1.ReasonRevoked,
			Message:            "lease revoked",
			ObservedGeneration: lease.Generation,
		})
	}); err != nil {
		r.recordOutcome("error", "status_update_failed")
		return ctrl.Result{}, err
	}
	r.logTransition(logger, lease, fromPhase, tetherv1alpha1.PhaseRevoked, tetherv1alpha1.ReasonRevoked)
	r.emitEvent(lease, "Normal", tetherv1alpha1.ReasonRevoked, "lease revoked")
	r.recordOutcome("revoked", tetherv1alpha1.ReasonRevoked)
	return ctrl.Result{}, nil
}

func (r *TetherLeaseReconciler) setReadyCondition(ctx context.Context, lease *tetherv1alpha1.TetherLease, conditionStatus metav1.ConditionStatus, reason, message string) error {
	return r.updateLeaseStatus(ctx, lease, func(status *tetherv1alpha1.TetherLeaseStatus) {
		status.ObservedGeneration = lease.Generation
		apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type:               tetherv1alpha1.ConditionReady,
			Status:             conditionStatus,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: lease.Generation,
		})
	})
}

func (r *TetherLeaseReconciler) updateRejectedLeaseStatus(ctx context.Context, lease *tetherv1alpha1.TetherLease, reason, message string) error {
	return r.updateLeaseStatus(ctx, lease, func(status *tetherv1alpha1.TetherLeaseStatus) {
		status.Phase = tetherv1alpha1.PhasePending
		status.ObservedGeneration = lease.Generation
		apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type:               tetherv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: lease.Generation,
		})
	})
}

func validateLeaseRole(role string) error {
	trimmedRole := strings.TrimSpace(role)
	if trimmedRole == "" {
		return fmt.Errorf("role must not be empty")
	}
	if trimmedRole != role {
		return fmt.Errorf("role must not contain leading or trailing whitespace")
	}
	if _, ok := allowedClusterRoles[role]; ok {
		return nil
	}
	if allowedRolePattern.MatchString(role) {
		return nil
	}
	return fmt.Errorf("role must be one of [view edit admin cluster-admin] or match %q", allowedRolePattern.String())
}

func validateLeaseUser(user string) error {
	trimmedUser := strings.TrimSpace(user)
	if trimmedUser == "" {
		return fmt.Errorf("user must not be empty")
	}
	if trimmedUser != user {
		return fmt.Errorf("user must not contain leading or trailing whitespace")
	}
	if strings.HasPrefix(user, "system:") {
		return fmt.Errorf("system principals are not allowed")
	}
	if user == "kubernetes-admin" {
		return fmt.Errorf("reserved admin principal is not allowed")
	}
	if !allowedUserPattern.MatchString(user) {
		return fmt.Errorf("user must match %q", allowedUserPattern.String())
	}
	return nil
}

func validateLeaseDuration(duration time.Duration) error {
	if duration < minLeaseDuration {
		return fmt.Errorf("duration must be at least %s", minLeaseDuration)
	}
	if duration > maxLeaseDuration {
		return fmt.Errorf("duration must be at most %s", maxLeaseDuration)
	}
	return nil
}

func (r *TetherLeaseReconciler) updateLeaseStatus(ctx context.Context, lease *tetherv1alpha1.TetherLease, mutate func(status *tetherv1alpha1.TetherLeaseStatus)) error {
	originalStatus := lease.Status.DeepCopy()
	mutate(&lease.Status)

	if apiequality.Semantic.DeepEqual(*originalStatus, lease.Status) {
		return nil
	}

	return r.Status().Update(ctx, lease)
}

func (r *TetherLeaseReconciler) handleDeletion(ctx context.Context, lease *tetherv1alpha1.TetherLease) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if controllerutil.ContainsFinalizer(lease, finalizerName) {
		if lease.Status.BindingName != "" {
			logger.Info("Deleting ClusterRoleBinding during lease cleanup", "binding", lease.Status.BindingName)
			if err := r.deleteClusterRoleBinding(ctx, lease.Status.BindingName); err != nil {
				r.recordOutcome("error", "binding_delete_failed")
				return ctrl.Result{}, err
			}
		}
		controllerutil.RemoveFinalizer(lease, finalizerName)
		if err := r.Update(ctx, lease); err != nil {
			r.recordOutcome("error", "finalizer_remove_failed")
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *TetherLeaseReconciler) emitEvent(lease *tetherv1alpha1.TetherLease, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(lease, eventType, reason, message)
}

func (r *TetherLeaseReconciler) recordOutcome(outcome, reason string) {
	if reason == "" {
		reason = "none"
	}
	reconcileOutcomesTotal.WithLabelValues(outcome, reason).Inc()
}

func (r *TetherLeaseReconciler) logTransition(logger logr.Logger, lease *tetherv1alpha1.TetherLease, fromPhase, toPhase tetherv1alpha1.TetherLeasePhase, reason string) {
	logger.Info("Lease phase transition", "lease", lease.Name, "namespace", lease.Namespace, "fromPhase", string(fromPhase), "toPhase", string(toPhase), "reason", reason)
}

func (r *TetherLeaseReconciler) buildClusterRoleBinding(lease *tetherv1alpha1.TetherLease, name string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "tether",
				"tether.dev/lease":             lease.Name,
			},
			Annotations: map[string]string{
				"tether.dev/user":   lease.Spec.User,
				"tether.dev/reason": lease.Spec.Reason,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     lease.Spec.Role,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:     rbacv1.UserKind,
				APIGroup: rbacv1.GroupName,
				Name:     lease.Spec.User,
			},
		},
	}
}

func (r *TetherLeaseReconciler) createOrUpdateClusterRoleBinding(ctx context.Context, crb *rbacv1.ClusterRoleBinding) error {
	existing := &rbacv1.ClusterRoleBinding{}
	err := r.Get(ctx, client.ObjectKey{Name: crb.Name}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, crb)
	}
	if err != nil {
		return err
	}
	existing.RoleRef = crb.RoleRef
	existing.Subjects = crb.Subjects
	existing.Labels = crb.Labels
	existing.Annotations = crb.Annotations
	return r.Update(ctx, existing)
}

func (r *TetherLeaseReconciler) deleteClusterRoleBinding(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	crb := &rbacv1.ClusterRoleBinding{}
	err := r.Get(ctx, client.ObjectKey{Name: name}, crb)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting ClusterRoleBinding %s: %w", name, err)
	}
	return r.Delete(ctx, crb)
}
