package operator

import (
	"context"
	"fmt"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	tetherv1alpha1 "github.com/Jaydee94/tether/pkg/api/v1alpha1"
)

const (
	finalizerName = "tether.dev/cleanup"
	bindingPrefix = "tether-lease-"
)

// +kubebuilder:rbac:groups=tether.dev,resources=tetherleases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tether.dev,resources=tetherleases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tether.dev,resources=tetherleases/finalizers,verbs=update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;patch;delete

// TetherLeaseReconciler reconciles TetherLease objects.
type TetherLeaseReconciler struct {
	client.Client
	Scheme *runtime.Scheme
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
	logger := log.FromContext(ctx)

	lease := &tetherv1alpha1.TetherLease{}
	if err := r.Get(ctx, req.NamespacedName, lease); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
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
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	switch lease.Status.Phase {
	case "", tetherv1alpha1.PhasePending:
		logger.Info("Activating lease", "lease", lease.Name, "user", lease.Spec.User, "role", lease.Spec.Role)
		return r.activateLease(ctx, lease)
	case tetherv1alpha1.PhaseActive:
		return r.reconcileActive(ctx, lease)
	case tetherv1alpha1.PhaseRevoked:
		return r.handleRevoked(ctx, lease)
	case tetherv1alpha1.PhaseExpired:
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

func (r *TetherLeaseReconciler) activateLease(ctx context.Context, lease *tetherv1alpha1.TetherLease) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	duration, err := time.ParseDuration(lease.Spec.Duration)
	if err != nil {
		logger.Error(err, "Invalid duration in lease spec", "duration", lease.Spec.Duration)
		return ctrl.Result{}, err
	}

	bindingName := bindingPrefix + lease.Name
	crb := r.buildClusterRoleBinding(lease, bindingName)

	if err := r.createOrUpdateClusterRoleBinding(ctx, crb); err != nil {
		return ctrl.Result{}, err
	}

	expiresAt := metav1.NewTime(time.Now().Add(duration))
	lease.Status.Phase = tetherv1alpha1.PhaseActive
	lease.Status.ExpiresAt = &expiresAt
	lease.Status.BindingName = bindingName

	if err := r.Status().Update(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Lease activated", "lease", lease.Name, "expiresAt", expiresAt.Time)
	return ctrl.Result{RequeueAfter: duration}, nil
}

func (r *TetherLeaseReconciler) reconcileActive(ctx context.Context, lease *tetherv1alpha1.TetherLease) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if lease.Status.ExpiresAt == nil {
		logger.Info("Active lease missing expiresAt, re-activating", "lease", lease.Name)
		lease.Status.Phase = tetherv1alpha1.PhasePending
		if err := r.Status().Update(ctx, lease); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	remaining := time.Until(lease.Status.ExpiresAt.Time)
	if remaining <= 0 {
		logger.Info("Lease expired, cleaning up", "lease", lease.Name)
		return r.expireLease(ctx, lease)
	}

	return ctrl.Result{RequeueAfter: remaining}, nil
}

func (r *TetherLeaseReconciler) expireLease(ctx context.Context, lease *tetherv1alpha1.TetherLease) (ctrl.Result, error) {
	if err := r.deleteClusterRoleBinding(ctx, lease.Status.BindingName); err != nil {
		return ctrl.Result{}, err
	}
	lease.Status.Phase = tetherv1alpha1.PhaseExpired
	if err := r.Status().Update(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *TetherLeaseReconciler) handleRevoked(ctx context.Context, lease *tetherv1alpha1.TetherLease) (ctrl.Result, error) {
	if lease.Status.BindingName != "" {
		if err := r.deleteClusterRoleBinding(ctx, lease.Status.BindingName); err != nil {
			return ctrl.Result{}, err
		}
		lease.Status.BindingName = ""
		if err := r.Status().Update(ctx, lease); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *TetherLeaseReconciler) handleDeletion(ctx context.Context, lease *tetherv1alpha1.TetherLease) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if controllerutil.ContainsFinalizer(lease, finalizerName) {
		if lease.Status.BindingName != "" {
			logger.Info("Deleting ClusterRoleBinding during lease cleanup", "binding", lease.Status.BindingName)
			if err := r.deleteClusterRoleBinding(ctx, lease.Status.BindingName); err != nil {
				return ctrl.Result{}, err
			}
		}
		controllerutil.RemoveFinalizer(lease, finalizerName)
		if err := r.Update(ctx, lease); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
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
