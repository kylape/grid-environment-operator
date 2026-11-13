// Package controller implements the AIGrid phase-machine reconciler.
//
// Host-cluster RBAC markers live here on purpose: controller-gen collects
// package-target markers (kubebuilder:rbac) only from package-clause doc
// comments in this toolchain — struct- or function-attached markers are
// silently ignored, which is how hand-maintained RBAC rot begins.
// Guest-cluster operations go through LWE-issued kubeconfigs and are not
// governed by this role.
// +kubebuilder:rbac:groups=aigrid.llm.redhat.com,resources=aigrids,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=aigrid.llm.redhat.com,resources=aigrids/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=aigrid.llm.redhat.com,resources=aigrids/finalizers,verbs=update
// +kubebuilder:rbac:groups=llm.redhat.com,resources=llmworkloadenvironments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps;services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=config.openshift.io,resources=networks,verbs=get;list;watch
package controller
