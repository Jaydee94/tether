package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=tl
// +kubebuilder:printcolumn:name="User",type=string,JSONPath=`.spec.user`
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.role`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Expires",type=string,JSONPath=`.status.expiresAt`

// TetherLease is the Schema for the tetherleases API.
type TetherLease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              TetherLeaseSpec   `json:"spec,omitempty"`
	Status            TetherLeaseStatus `json:"status,omitempty"`
}

// TetherLeaseSpec defines the desired state of TetherLease.
type TetherLeaseSpec struct {
	User     string `json:"user"`
	Role     string `json:"role"`
	Duration string `json:"duration"`
	Reason   string `json:"reason,omitempty"`
	// Namespace is the target namespace for a RoleBinding. When set, a RoleBinding is created in
	// that namespace instead of a ClusterRoleBinding, reducing blast radius to a single namespace.
	// When empty (default), a ClusterRoleBinding is created (existing behaviour).
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// TetherLeaseStatus defines the observed state of TetherLease.
type TetherLeaseStatus struct {
	Phase       TetherLeasePhase `json:"phase,omitempty"`
	ExpiresAt   *metav1.Time     `json:"expiresAt,omitempty"`
	BindingName string           `json:"bindingName,omitempty"`
	// TokenSecret is the name of the k8s Secret in the tether namespace that holds the session token.
	TokenSecret string `json:"tokenSecret,omitempty"`
}

// TetherLeasePhase describes the lifecycle phase of a TetherLease.
type TetherLeasePhase string

const (
	PhasePending TetherLeasePhase = "Pending"
	PhaseActive  TetherLeasePhase = "Active"
	PhaseExpired TetherLeasePhase = "Expired"
	PhaseRevoked TetherLeasePhase = "Revoked"
)

// +kubebuilder:object:root=true

// TetherLeaseList contains a list of TetherLease.
type TetherLeaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TetherLease `json:"items"`
}
