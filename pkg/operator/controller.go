package operator

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	tetherv1alpha1 "github.com/Jaydee94/tether/pkg/api/v1alpha1"
)

const (
	finalizerName = "tether.dev/cleanup"
	bindingPrefix = "tether-lease-"
	tokenPrefix   = "tether-token-"

	// LabelTokenType is applied to session-token Secrets.
	LabelTokenType = "tether.dev/type"
	// LabelTokenLease links a token Secret to its TetherLease.
	LabelTokenLease = "tether.dev/lease"
	// TokenSecretType is the value of LabelTokenType for session tokens.
	TokenSecretType = "session-token"
	// TokenDataKey is the key inside the Secret's data map that holds the raw token.
	TokenDataKey = "token"
)

var (
	leaseActivationsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lease_activations_total",
		Help: "Total number of TetherLease activations.",
	})

	leaseExpirationsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lease_expirations_total",
		Help: "Total number of TetherLease expirations.",
	})

	leaseRevocationsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lease_revocations_total",
		Help: "Total number of TetherLease revocations.",
	})

	activeLeases = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "active_leases",
		Help: "Current number of active TetherLeases.",
	})
)

func init() {
	metrics.Registry.MustRegister(
		leaseActivationsTotal,
		leaseExpirationsTotal,
		leaseRevocationsTotal,
		activeLeases,
	)
}

// +kubebuilder:rbac:groups=tether.dev,resources=tetherleases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tether.dev,resources=tetherleases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tether.dev,resources=tetherleases/finalizers,verbs=update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// TetherLeaseReconciler reconciles TetherLease objects.
type TetherLeaseReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	TokenNamespace  string // namespace in which session-token Secrets are stored
	ClusterRegistry ClusterRegistry
}

// SetupWithManager sets up the controller with the Manager.
func (r *TetherLeaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tetherv1alpha1.TetherLease{}).
		Owns(&rbacv1.ClusterRoleBinding{}).
		Owns(&rbacv1.RoleBinding{}).
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

	// If approvers are configured AND not yet approved, gate the lease into PendingApproval.
	if len(lease.Spec.Approvers) > 0 && lease.Status.ApprovedBy == "" {
		message := fmt.Sprintf("waiting for approval from: %s", strings.Join(lease.Spec.Approvers, ", "))
		logger.Info("Lease requires approval, holding in PendingApproval", "approvers", lease.Spec.Approvers)
		lease.Status.Phase = tetherv1alpha1.PhasePendingApproval
		if err := r.Status().Update(ctx, lease); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("Lease transitioned to PendingApproval", "message", message)
		return ctrl.Result{}, nil
	}

	duration, err := time.ParseDuration(lease.Spec.Duration)
	if err != nil {
		logger.Error(err, "Invalid duration in lease spec", "duration", lease.Spec.Duration)
		return ctrl.Result{}, err
	}

	// Resolve target cluster
	clusterName := lease.Spec.Cluster
	if clusterName == "" {
		clusterName = "local"
	}

	var targetClient client.Client
	if r.ClusterRegistry != nil {
		clusterClient, err := r.ClusterRegistry.GetCluster(ctx, clusterName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("getting cluster %q: %w", clusterName, err)
		}
		targetClient = clusterClient.Client
		logger.Info("Creating binding in target cluster", "cluster", clusterName, "lease", lease.Name)
	} else {
		// Backward compatibility: no cluster registry means single-cluster mode
		targetClient = r.Client
		clusterName = "local"
	}

	bindingName := bindingPrefix + lease.Name
	if lease.Spec.Namespace != "" {
		rb := r.buildRoleBinding(lease, bindingName)
		if err := r.createOrUpdateRoleBindingInCluster(ctx, targetClient, rb); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		crb := r.buildClusterRoleBinding(lease, bindingName)
		if err := r.createOrUpdateClusterRoleBindingInCluster(ctx, targetClient, crb); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Generate and store a session token in the control plane cluster
	secretName := tokenPrefix + lease.Name
	token, err := generateSessionToken()
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("generating session token: %w", err)
	}
	expiresAt := metav1.NewTime(time.Now().Add(duration))
	tokenSecret := r.buildTokenSecret(lease, secretName, token, expiresAt.Time)
	if err := r.createOrUpdateSecret(ctx, tokenSecret); err != nil {
		return ctrl.Result{}, err
	}

	lease.Status.Phase = tetherv1alpha1.PhaseActive
	lease.Status.ExpiresAt = &expiresAt
	lease.Status.BindingName = bindingName
	lease.Status.TokenSecret = secretName
	lease.Status.Cluster = clusterName

	if err := r.Status().Update(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}

	leaseActivationsTotal.Inc()
	activeLeases.Inc()
	logger.Info("Lease activated", "lease", lease.Name, "cluster", clusterName, "expiresAt", expiresAt.Time, "tokenSecret", secretName)
	return ctrl.Result{RequeueAfter: duration}, nil
}

// reconcilePendingApproval watches for an approver to set status.approvedBy or status.deniedBy.
func (r *TetherLeaseReconciler) reconcilePendingApproval(ctx context.Context, lease *tetherv1alpha1.TetherLease) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if lease.Status.DeniedBy != "" {
		logger.Info("Lease denied", "deniedBy", lease.Status.DeniedBy)
		lease.Status.Phase = tetherv1alpha1.PhaseDenied
		if err := r.Status().Update(ctx, lease); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if lease.Status.ApprovedBy != "" {
		logger.Info("Lease approved, resetting to Pending for activation", "approvedBy", lease.Status.ApprovedBy)
		lease.Status.Phase = tetherv1alpha1.PhasePending
		if err := r.Status().Update(ctx, lease); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Still waiting - requeue after 30 seconds to poll.
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
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
	if err := r.deleteBinding(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteTokenSecret(ctx, lease.Status.TokenSecret); err != nil {
		return ctrl.Result{}, err
	}
	lease.Status.Phase = tetherv1alpha1.PhaseExpired
	lease.Status.TokenSecret = ""
	if err := r.Status().Update(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}
	leaseExpirationsTotal.Inc()
	activeLeases.Dec()
	return ctrl.Result{}, nil
}

func (r *TetherLeaseReconciler) handleRevoked(ctx context.Context, lease *tetherv1alpha1.TetherLease) (ctrl.Result, error) {
	if lease.Status.BindingName != "" {
		if err := r.deleteBinding(ctx, lease); err != nil {
			return ctrl.Result{}, err
		}
		lease.Status.BindingName = ""
	}
	if lease.Status.TokenSecret != "" {
		if err := r.deleteTokenSecret(ctx, lease.Status.TokenSecret); err != nil {
			return ctrl.Result{}, err
		}
		lease.Status.TokenSecret = ""
	}
	if err := r.Status().Update(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}
	leaseRevocationsTotal.Inc()
	activeLeases.Dec()
	return ctrl.Result{}, nil
}

func (r *TetherLeaseReconciler) handleDeletion(ctx context.Context, lease *tetherv1alpha1.TetherLease) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if controllerutil.ContainsFinalizer(lease, finalizerName) {
		if lease.Status.BindingName != "" {
			logger.Info("Deleting binding during lease cleanup", "binding", lease.Status.BindingName)
			if err := r.deleteBinding(ctx, lease); err != nil {
				return ctrl.Result{}, err
			}
		}
		if lease.Status.TokenSecret != "" {
			logger.Info("Deleting token Secret during lease cleanup", "secret", lease.Status.TokenSecret)
			if err := r.deleteTokenSecret(ctx, lease.Status.TokenSecret); err != nil {
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

func (r *TetherLeaseReconciler) createOrUpdateRoleBinding(ctx context.Context, rb *rbacv1.RoleBinding) error {
	existing := &rbacv1.RoleBinding{}
	err := r.Get(ctx, client.ObjectKey{Name: rb.Name, Namespace: rb.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, rb)
	}
	if err != nil {
		return err
	}
	existing.RoleRef = rb.RoleRef
	existing.Subjects = rb.Subjects
	existing.Labels = rb.Labels
	existing.Annotations = rb.Annotations
	return r.Update(ctx, existing)
}

func (r *TetherLeaseReconciler) createOrUpdateClusterRoleBindingInCluster(ctx context.Context, targetClient client.Client, crb *rbacv1.ClusterRoleBinding) error {
	existing := &rbacv1.ClusterRoleBinding{}
	err := targetClient.Get(ctx, client.ObjectKey{Name: crb.Name}, existing)
	if errors.IsNotFound(err) {
		return targetClient.Create(ctx, crb)
	}
	if err != nil {
		return err
	}
	existing.RoleRef = crb.RoleRef
	existing.Subjects = crb.Subjects
	existing.Labels = crb.Labels
	existing.Annotations = crb.Annotations
	return targetClient.Update(ctx, existing)
}

func (r *TetherLeaseReconciler) createOrUpdateRoleBindingInCluster(ctx context.Context, targetClient client.Client, rb *rbacv1.RoleBinding) error {
	existing := &rbacv1.RoleBinding{}
	err := targetClient.Get(ctx, client.ObjectKey{Name: rb.Name, Namespace: rb.Namespace}, existing)
	if errors.IsNotFound(err) {
		return targetClient.Create(ctx, rb)
	}
	if err != nil {
		return err
	}
	existing.RoleRef = rb.RoleRef
	existing.Subjects = rb.Subjects
	existing.Labels = rb.Labels
	existing.Annotations = rb.Annotations
	return targetClient.Update(ctx, existing)
}

func (r *TetherLeaseReconciler) createOrUpdateSecret(ctx context.Context, secret *corev1.Secret) error {
	existing := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Name: secret.Name, Namespace: secret.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, secret)
	}
	if err != nil {
		return err
	}
	existing.Data = secret.Data
	existing.Labels = secret.Labels
	existing.Annotations = secret.Annotations
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

func (r *TetherLeaseReconciler) deleteRoleBinding(ctx context.Context, namespace, name string) error {
	if name == "" || namespace == "" {
		return nil
	}
	rb := &rbacv1.RoleBinding{}
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, rb)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting RoleBinding %s/%s: %w", namespace, name, err)
	}
	return r.Delete(ctx, rb)
}

func (r *TetherLeaseReconciler) deleteClusterRoleBindingInCluster(ctx context.Context, targetClient client.Client, name string) error {
	if name == "" {
		return nil
	}
	crb := &rbacv1.ClusterRoleBinding{}
	err := targetClient.Get(ctx, client.ObjectKey{Name: name}, crb)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting ClusterRoleBinding %s: %w", name, err)
	}
	return targetClient.Delete(ctx, crb)
}

func (r *TetherLeaseReconciler) deleteRoleBindingInCluster(ctx context.Context, targetClient client.Client, namespace, name string) error {
	if name == "" || namespace == "" {
		return nil
	}
	rb := &rbacv1.RoleBinding{}
	err := targetClient.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, rb)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting RoleBinding %s/%s: %w", namespace, name, err)
	}
	return targetClient.Delete(ctx, rb)
}

// deleteBinding deletes either a RoleBinding or ClusterRoleBinding depending on the lease spec.
func (r *TetherLeaseReconciler) deleteBinding(ctx context.Context, lease *tetherv1alpha1.TetherLease) error {
	// Resolve target cluster for deletion
	clusterName := lease.Status.Cluster
	if clusterName == "" {
		clusterName = "local"
	}

	var targetClient client.Client
	if r.ClusterRegistry != nil {
		clusterClient, err := r.ClusterRegistry.GetCluster(ctx, clusterName)
		if err != nil {
			// If the target cluster is unreachable, log the error but don't fail
			// The lease will be retried via the reconcile queue
			return fmt.Errorf("getting cluster %q for binding deletion: %w", clusterName, err)
		}
		targetClient = clusterClient.Client
	} else {
		targetClient = r.Client
	}

	if lease.Spec.Namespace != "" {
		return r.deleteRoleBindingInCluster(ctx, targetClient, lease.Spec.Namespace, lease.Status.BindingName)
	}
	return r.deleteClusterRoleBindingInCluster(ctx, targetClient, lease.Status.BindingName)
}

func (r *TetherLeaseReconciler) deleteTokenSecret(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	ns := r.tokenNamespace()
	secret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, secret)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting token Secret %s/%s: %w", ns, name, err)
	}
	return r.Delete(ctx, secret)
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

func (r *TetherLeaseReconciler) buildRoleBinding(lease *tetherv1alpha1.TetherLease, name string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: lease.Spec.Namespace,
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

func (r *TetherLeaseReconciler) buildTokenSecret(lease *tetherv1alpha1.TetherLease, name, token string, expiresAt time.Time) *corev1.Secret {
	ns := r.tokenNamespace()
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				LabelTokenType:                 TokenSecretType,
				LabelTokenLease:                lease.Name,
				"app.kubernetes.io/managed-by": "tether",
			},
			Annotations: map[string]string{
				"tether.dev/user":       lease.Spec.User,
				"tether.dev/role":       lease.Spec.Role,
				"tether.dev/reason":     lease.Spec.Reason,
				"tether.dev/expires-at": expiresAt.UTC().Format(time.RFC3339),
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			TokenDataKey: []byte(token),
		},
	}
}

func (r *TetherLeaseReconciler) tokenNamespace() string {
	if r.TokenNamespace != "" {
		return r.TokenNamespace
	}
	return "tether-system"
}

// generateSessionToken creates a cryptographically random 32-byte URL-safe base64 token.
func generateSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return base64.URLEncoding.EncodeToString(b), nil
}
