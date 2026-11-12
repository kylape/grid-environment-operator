package controller

import (
	"context"
	"encoding/json"
	"fmt"

	aigridv1alpha1 "github.com/kylape/grid-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// bootstrapHub is the HubBootstrapped phase, adapted from the repo's
// bootstrap.sh (BOOTSTRAP_PROFILE=dedicated-hub):
//
//  1. install the OpenShift GitOps operator (namespace, OperatorGroup,
//     Subscription — static, docs-derived manifests)
//  2. wait for the Argo CD CRDs and the default Argo CD instance
//  3. grant the Argo controller cluster-admin so the fan-out can create
//     Applications and ApplicationSets cluster-wide
//  4. create the root Application pointing at the repo's bootstrap
//     directory, which applies the root ApplicationSet — the fan-out that
//     installs everything else, ACM included. From there the repo manages
//     itself.
//  5. seed the hub-side quay pull secret (personal quay.io/rhoai
//     credentials) that the rhoai-prerequisites ACM app distributes to
//     every managed cluster
//
// The phase is done when the ManagedCluster CRD is established on the hub
// — the fan-out has installed ACM — because that is what the next phase
// (Imported) depends on.
func (r *AIGridReconciler) bootstrapHub(ctx context.Context, grid *aigridv1alpha1.AIGrid) (bool, error) {
	hub := r.hubClient(ctx, grid)
	if hub == nil {
		return false, nil
	}

	if err := r.ensureGitOpsOperator(ctx, hub); err != nil {
		return false, err
	}

	if ready, err := argoCDAvailable(ctx, hub); err != nil || !ready {
		return false, err
	}

	if err := r.ensureGitOpsAdminBinding(ctx, hub); err != nil {
		return false, err
	}

	if err := r.ensureRootApplication(ctx, hub, grid); err != nil {
		return false, err
	}

	if err := r.seedHubQuaySecret(ctx, grid, hub); err != nil {
		return false, err
	}

	return managedClusterCRDEstablished(ctx, hub)
}

// ensureGitOpsOperator installs the OpenShift GitOps operator from
// redhat-operators, mirroring bootstrap/namespace.yaml,
// bootstrap/gitops-operator-group.yaml, and
// bootstrap/openshift-gitops-sub.yaml.
func (r *AIGridReconciler) ensureGitOpsOperator(ctx context.Context, hub client.Client) error {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "openshift-gitops-operator"}}
	if _, err := controllerutil.CreateOrUpdate(ctx, hub, namespace, func() error {
		return nil
	}); err != nil {
		return err
	}

	operatorGroup := object(operatorGroupGVK, "openshift-gitops-operator", "openshift-gitops-operator")
	if _, err := controllerutil.CreateOrUpdate(ctx, hub, operatorGroup, func() error {
		if err := unstructured.SetNestedField(operatorGroup.Object, "Default", "spec", "upgradeStrategy"); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	subscription := object(subscriptionGVK, "openshift-gitops-operator", "openshift-gitops-operator")
	_, err := controllerutil.CreateOrUpdate(ctx, hub, subscription, func() error {
		if err := unstructured.SetNestedField(subscription.Object, "openshift-gitops-operator", "spec", "name"); err != nil {
			return err
		}
		fields := map[string]string{
			"channel":             "latest",
			"source":              "redhat-operators",
			"sourceNamespace":     "openshift-marketplace",
			"installPlanApproval": "Automatic",
		}
		for key, value := range fields {
			if err := unstructured.SetNestedField(subscription.Object, value, "spec", key); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

// argoCDAvailable reports whether the default Argo CD instance is running.
// It also implies the Application/ApplicationSet CRDs are established.
func argoCDAvailable(ctx context.Context, hub client.Client) (bool, error) {
	argocd := object(argocdGVK, "openshift-gitops", "openshift-gitops")
	if err := hub.Get(ctx, client.ObjectKeyFromObject(argocd), argocd); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	phase, found, err := unstructured.NestedString(argocd.Object, "status", "phase")
	if err != nil || !found {
		return false, nil
	}
	return phase == "Available", nil
}

// ensureGitOpsAdminBinding grants the Argo application controller
// cluster-admin, mirroring bootstrap/openshift-gitops-admin.yaml. Without
// it the fan-out cannot create Applications and ApplicationSets
// cluster-wide.
func (r *AIGridReconciler) ensureGitOpsAdminBinding(ctx context.Context, hub client.Client) error {
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "openshift-gitops-admin"},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, hub, binding, func() error {
		binding.Subjects = []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      "openshift-gitops-argocd-application-controller",
			Namespace: "openshift-gitops",
		}}
		binding.RoleRef = rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "cluster-admin",
		}
		return nil
	})
	return err
}

// ensureRootApplication points the hub's Argo at the repo's bootstrap
// directory. The directory contains the root ApplicationSet, which fans
// out every app — including ACM — and then manages itself. This mirrors
// the repo's generated dedicated-hub-root Application, with the source
// taken from the AIGrid spec instead of being hardcoded.
func (r *AIGridReconciler) ensureRootApplication(ctx context.Context, hub client.Client, grid *aigridv1alpha1.AIGrid) error {
	application := object(applicationGVK, gitOpsNamespace, grid.Name)
	_, err := controllerutil.CreateOrUpdate(ctx, hub, application, func() error {
		if err := unstructured.SetNestedMap(application.Object, map[string]interface{}{
			"repoURL":        grid.Spec.GitOps.Repo,
			"targetRevision": defaultString(grid.Spec.GitOps.Revision, "main"),
			"path":           defaultString(grid.Spec.GitOps.Path, "bootstrap/dedicated-hub"),
			"directory":      map[string]interface{}{"include": "applicationset.yaml"},
		}, "spec", "source"); err != nil {
			return err
		}
		if err := unstructured.SetNestedMap(application.Object, map[string]interface{}{
			"server":    "https://kubernetes.default.svc",
			"namespace": gitOpsNamespace,
		}, "spec", "destination"); err != nil {
			return err
		}
		if err := unstructured.SetNestedField(application.Object, "default", "spec", "project"); err != nil {
			return err
		}
		return unstructured.SetNestedMap(application.Object, map[string]interface{}{
			"automated": map[string]interface{}{"prune": false, "selfHeal": true},
			"retry": map[string]interface{}{
				"limit":   int64(10),
				"backoff": map[string]interface{}{"duration": "30s", "factor": int64(2), "maxDuration": "10m"},
			},
		}, "spec", "syncPolicy")
	})
	return err
}

// seedHubQuaySecret replicates apps/rhoai-prerequisites/create-hub-secret.sh:
// the personal quay.io/rhoai credentials (taken from the grid's pull
// secret) land in a hub-side secret that the rhoai-prerequisites ACM app
// distributes to every managed cluster. Note the script's quirk, preserved
// deliberately: both quay.io and quay.io/rhoai carry the personal auth.
func (r *AIGridReconciler) seedHubQuaySecret(ctx context.Context, grid *aigridv1alpha1.AIGrid, hub client.Client) error {
	pullSecret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: grid.Namespace, Name: grid.Spec.PullSecretRef.Name}, pullSecret); err != nil {
		return err
	}
	config, err := parseDockerConfig(pullSecret.Data[".dockerconfigjson"])
	if err != nil {
		return err
	}
	personalAuth, ok := config.Auths["quay.io/rhoai"]
	if !ok || personalAuth.Auth == "" {
		return fmt.Errorf("grid pull secret has no quay.io/rhoai credential to seed the hub with")
	}

	auths := map[string]interface{}{
		"quay.io":       map[string]interface{}{"auth": personalAuth.Auth},
		"quay.io/rhoai": map[string]interface{}{"auth": personalAuth.Auth},
	}
	if stage, ok := config.Auths["registry.stage.redhat.io"]; ok && stage.Auth != "" {
		auths["registry.stage.redhat.io"] = map[string]interface{}{"auth": stage.Auth}
	}

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rhoai-prerequisites"}}
	if _, err := controllerutil.CreateOrUpdate(ctx, hub, namespace, func() error {
		return nil
	}); err != nil {
		return err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rhoai-quay-pull-secret", Namespace: "rhoai-prerequisites"},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, hub, secret, func() error {
		secret.Type = corev1.SecretTypeDockerConfigJson
		configJSON, err := json.Marshal(map[string]interface{}{"auths": auths})
		if err != nil {
			return err
		}
		secret.Data = map[string][]byte{".dockerconfigjson": configJSON}
		return nil
	})
	return err
}

// managedClusterCRDEstablished reports whether the fan-out has installed
// ACM on the hub — the gate for the Imported phase.
func managedClusterCRDEstablished(ctx context.Context, hub client.Client) (bool, error) {
	crd := object(crdGVK, "", "managedclusters.cluster.open-cluster-management.io")
	if err := hub.Get(ctx, client.ObjectKeyFromObject(crd), crd); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	conditions, found, err := unstructured.NestedSlice(crd.Object, "status", "conditions")
	if err != nil || !found {
		return false, nil
	}
	for _, raw := range conditions {
		condition, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if condition["type"] == "Established" && condition["status"] == "True" {
			return true, nil
		}
	}
	return false, nil
}
