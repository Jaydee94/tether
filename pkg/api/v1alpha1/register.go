package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "tether.dev", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(&TetherLease{}, &TetherLeaseList{})
}

// Resource returns a GroupResource for the given resource name.
func Resource(resource string) schema.GroupResource {
	return GroupVersion.WithResource(resource).GroupResource()
}

// Kind returns a GroupKind for the given kind name.
func Kind(kind string) schema.GroupKind {
	return schema.GroupKind{Group: GroupVersion.Group, Kind: kind}
}
