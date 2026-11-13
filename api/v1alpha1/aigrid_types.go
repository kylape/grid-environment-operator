package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SiteRole selects a site's responsibility within the grid.
// +kubebuilder:validation:Enum=hub;inference;ingress
type SiteRole string

const (
	// SiteRoleHub is the control-plane site: ACM, Argo, grid enrollment.
	SiteRoleHub SiteRole = "hub"
	// SiteRoleInference serves models.
	SiteRoleInference SiteRole = "inference"
	// SiteRoleIngress consumes models through the grid.
	SiteRoleIngress SiteRole = "ingress"
)

// SiteSpec defines one grid site.
type SiteSpec struct {
	// Name is the site's LWE name. Defaults to <grid-name>-<role>.
	// Must be at most 51 characters (HyperShift LWE namespace limit).
	// +optional
	Name string `json:"name,omitempty"`

	// Role selects the site's responsibility within the grid.
	Role SiteRole `json:"role"`

	// Size selects the HyperShift LWE worker size.
	// +kubebuilder:validation:Enum=small;medium;large
	// +optional
	Size string `json:"size,omitempty"`

	// Replicas is the desired number of KubeVirt worker nodes.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Replicas int32 `json:"replicas,omitempty"`
}

// GitOpsSpec points at the repository that installs the grid.
type GitOpsSpec struct {
	// Repo is the Git repository URL.
	Repo string `json:"repo"`

	// Revision is the Git revision to install from.
	// +optional
	Revision string `json:"revision,omitempty"`

	// Path is the bootstrap directory within the repository containing
	// the root ApplicationSet (the fan-out that installs everything,
	// ACM included). Defaults to bootstrap/dedicated-hub.
	// +optional
	Path string `json:"path,omitempty"`
}

// AIGridSpec defines the desired state of an AIGrid.
type AIGridSpec struct {
	// Sites are the grid's sites. Exactly one hub, at least one inference,
	// and at least one ingress site are required.
	// +kubebuilder:validation:MinItems=3
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:XValidation:message="exactly one hub site is required",rule="self.filter(s, s.role == 'hub').size() == 1"
	// +kubebuilder:validation:XValidation:message="at least one inference site is required",rule="self.filter(s, s.role == 'inference').size() >= 1"
	// +kubebuilder:validation:XValidation:message="at least one ingress site is required",rule="self.filter(s, s.role == 'ingress').size() >= 1"
	Sites []SiteSpec `json:"sites"`

	// GitOps identifies the repository the grid is installed from.
	GitOps GitOpsSpec `json:"gitOps"`

	// PullSecretRef names the pull secret for all LWEs. It must be the
	// path-scoped composition: quay.io (console auth, openshift-release-dev)
	// and quay.io/rhoai (pre-release bundles) as separate keys.
	PullSecretRef corev1.LocalObjectReference `json:"pullSecretRef"`

	// Duration is the requested lifetime of the grid's LWEs, in Go
	// time.Duration syntax. Defaults to 72h.
	// +optional
	Duration string `json:"duration,omitempty"`

	// ClusterQueue is the Kueue ClusterQueue backing each site's
	// LocalQueue, separating this grid's capacity from others (e.g. CI
	// grids apart from interactive dev grids). Optional: when unset,
	// sites use the LWE operator's default (inference-workspaces).
	// The LWE API treats clusterQueue as immutable: changing it requires
	// removing and re-adding the site.
	// +optional
	ClusterQueue string `json:"clusterQueue,omitempty"`

	// PeerGroup is the LWE peer-group label value applied to all LWEs.
	// Defaults to the AIGrid's name.
	// +optional
	// +kubebuilder:validation:XValidation:message="peerGroup is immutable",rule="self == oldSelf"
	PeerGroup string `json:"peerGroup,omitempty"`
}

// SiteStatus is the observed state of one grid site.
type SiteStatus struct {
	// Role is the site's role.
	Role SiteRole `json:"role"`

	// Name is the resolved site name.
	Name string `json:"name"`

	// LWEName is the site's LWE resource name.
	LWEName string `json:"lweName"`

	// Phase is the site's progress through reconciliation.
	// +optional
	Phase string `json:"phase,omitempty"`

	// KubeconfigSecret names the secret holding this site's kubeconfig.
	// +optional
	KubeconfigSecret string `json:"kubeconfigSecret,omitempty"`

	// GridSite is the site's GridSite Connected condition, once the grid
	// is installed (the operator's end-to-end path probe).
	// +optional
	GridSite string `json:"gridSite,omitempty"`

	// AppsDomain is the site's public apps domain.
	// +optional
	AppsDomain string `json:"appsDomain,omitempty"`

	// Message carries the blocking reason while the site is not ready,
	// taken from the LWE's own Ready condition — so `kubectl get aigrid`
	// answers "why is this stuck" without inspecting the LWE.
	// +optional
	Message string `json:"message,omitempty"`
}

// AIGridStatus is the observed state of an AIGrid.
type AIGridStatus struct {
	// Conditions report grid-level phase progress:
	// Provisioned, Imported, Wired, GitOpsSynced, GridReady.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Sites is the observed state of each site, in spec order.
	// +optional
	Sites []SiteStatus `json:"sites,omitempty"`

	// DNSLweName is the namespace-mode LWE hosting the zone server.
	// +optional
	DNSLweName string `json:"dnsLweName,omitempty"`

	// IngressEndpointURL is the consumer gateway's externally reachable
	// URL, set once the grid is ready.
	// +optional
	IngressEndpointURL string `json:"ingressEndpointURL,omitempty"`

	// ExpirationTimestamp is the earliest LWE expiration in the grid.
	// +optional
	ExpirationTimestamp *metav1.Time `json:"expirationTimestamp,omitempty"`

	// ObservedGeneration is the spec generation most recently reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=aig
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="GridReady")].status`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.ingressEndpointURL`
// +kubebuilder:printcolumn:name="Expires",type=string,JSONPath=`.status.expirationTimestamp`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AIGrid is a multi-site AI Grid environment: HyperShift LWE sites, the
// cross-site networking they need, and a GitOps-driven grid installation.
type AIGrid struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AIGridSpec   `json:"spec,omitempty"`
	Status AIGridStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AIGridList contains a list of AIGrid.
type AIGridList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AIGrid `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AIGrid{}, &AIGridList{})
}
