package controller

import (
	"context"
	"fmt"

	aigridv1alpha1 "github.com/kylape/grid-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// bundleUnpackRetryAnnotation makes OLM retry failed bundle unpacks at a
// reasonable interval instead of wedging on the default backoff. The chart
// has no knob for this; it is applied at runtime once the fan-out creates
// the OperatorGroups.
const bundleUnpackRetryAnnotation = "operatorframework.io/bundle-unpack-min-retry-interval"

// gridNamespace is where grid components land on each guest.
const gridNamespace = "redhat-ai-grid"

// installGitOps is the GitOpsSynced phase: label the spokes with their
// roles (triggering the repo's chart fan-out), create the root Application
// on the hub, annotate the OperatorGroups, and wait for the root app to
// sync.
//
// This phase overlaps Wiring by design: the grid's connect checks retry,
// so the grid can install while DNS wiring lands.
func (r *AIGridReconciler) installGitOps(ctx context.Context, grid *aigridv1alpha1.AIGrid) (bool, error) {
	hub := r.hubClient(ctx, grid)
	if hub == nil {
		return false, nil
	}

	// Role labels on the spoke ManagedClusters: this is what the fan-out
	// keys on. Applied only after the join succeeded (Imported is a prior
	// phase), so the OperatorGroups they create appear on live guests.
	for _, site := range spokeSites(grid) {
		name := SiteName(grid, site)
		mc := object(managedClusterGVK, "", name)
		if _, err := controllerutil.CreateOrUpdate(ctx, hub, mc, func() error {
			labels := objectMetaLabels(mc)
			labels[gridLabel] = grid.Name
			labels[peerGroupLabel] = peerGroup(grid)
			labels[managedClusterRoleLabel] = string(site.Role)
			mc.SetLabels(labels)
			return nil
		}); err != nil {
			return false, err
		}
	}

	// The root Application: points the hub's Argo at the grid's repo.
	application := object(applicationGVK, gitOpsNamespace, grid.Name)
	if _, err := controllerutil.CreateOrUpdate(ctx, hub, application, func() error {
		if err := unstructured.SetNestedMap(application.Object, map[string]interface{}{
			"repoURL":        grid.Spec.GitOps.Repo,
			"targetRevision": defaultString(grid.Spec.GitOps.Revision, "main"),
			"path":           defaultString(grid.Spec.GitOps.Path, "."),
		}, "spec", "source"); err != nil {
			return err
		}
		if err := unstructured.SetNestedMap(application.Object, map[string]interface{}{
			"server":    "https://kubernetes.default.svc",
			"namespace": grid.Namespace,
		}, "spec", "destination"); err != nil {
			return err
		}
		if err := unstructured.SetNestedField(application.Object, "default", "spec", "project"); err != nil {
			return err
		}
		return unstructured.SetNestedMap(application.Object, map[string]interface{}{
			"automated": map[string]interface{}{"selfHeal": true},
		}, "spec", "syncPolicy")
	}); err != nil {
		return false, err
	}

	// Annotate the OperatorGroups once the fan-out creates them.
	for _, site := range spokeSites(grid) {
		guest, err := r.siteKubeconfig(ctx, grid, SiteName(grid, site))
		if err != nil {
			return false, err
		}
		if err := r.annotateOperatorGroups(ctx, guest); err != nil {
			return false, err
		}
	}

	// Readiness: the root Application reports Synced.
	sync, found, err := unstructured.NestedString(application.Object, "status", "sync", "status")
	if err != nil || !found {
		return false, nil
	}
	return sync == "Synced", nil
}

// annotateOperatorGroups applies the bundle-unpack retry annotation to the
// rhods OperatorGroup on a guest, if it exists yet.
func (r *AIGridReconciler) annotateOperatorGroups(ctx context.Context, guest client.Client) error {
	og, err := getUnstructured(ctx, guest, operatorGroupGVK, "redhat-ods-operator", "rhods-operator")
	if err != nil {
		// The fan-out has not created it yet; retry on the next reconcile.
		return client.IgnoreNotFound(err)
	}
	annotations := og.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if annotations[bundleUnpackRetryAnnotation] == "1m" {
		return nil
	}
	annotations[bundleUnpackRetryAnnotation] = "1m"
	og.SetAnnotations(annotations)
	return guest.Update(ctx, og)
}

// awaitGrid is the GridReady phase: every spoke's GridSite reports
// Connected (the grid's own end-to-end probe, which exercises the wired
// path), and the consumer gateway is exposed through an ingress Route.
func (r *AIGridReconciler) awaitGrid(ctx context.Context, grid *aigridv1alpha1.AIGrid) (bool, error) {
	allConnected := true
	for i, site := range grid.Status.Sites {
		if site.Role == aigridv1alpha1.SiteRoleHub {
			continue
		}
		guest, err := r.siteKubeconfig(ctx, grid, site.Name)
		if err != nil {
			return false, err
		}
		gridSite, err := getUnstructured(ctx, guest, gridSiteGVK, "default", site.Name)
		if err != nil {
			// The grid chart has not deployed the GridSite yet.
			allConnected = false
			continue
		}
		status, connected := gridSiteConnected(gridSite)
		grid.Status.Sites[i].GridSite = status
		if !connected {
			allConnected = false
		}
	}
	if !allConnected {
		return false, nil
	}

	// Expose the consumer gateway on the first ingress site. The Route
	// lives in the guest's own routing table, so the host router's
	// wildcard passthrough for the site's apps domain makes it reachable
	// from the host network and beyond — the one exposure path that works
	// natively.
	for _, site := range grid.Status.Sites {
		if site.Role != aigridv1alpha1.SiteRoleIngress || site.AppsDomain == "" {
			continue
		}
		guest, err := r.siteKubeconfig(ctx, grid, site.Name)
		if err != nil {
			return false, err
		}

		// The gateway service must exist before the route can target it.
		gateway := &corev1.Service{}
		if err := guest.Get(ctx, client.ObjectKey{Namespace: gridNamespace, Name: "grid-gateway"}, gateway); err != nil {
			return false, client.IgnoreNotFound(err)
		}

		host := "grid-" + grid.Name + "." + site.AppsDomain
		route := object(routeGVK, gridNamespace, "grid-"+grid.Name)
		if _, err := controllerutil.CreateOrUpdate(ctx, guest, route, func() error {
			if err := unstructured.SetNestedField(route.Object, host, "spec", "host"); err != nil {
				return err
			}
			if err := unstructured.SetNestedMap(route.Object, map[string]interface{}{
				"termination": "passthrough",
			}, "spec", "tls"); err != nil {
				return err
			}
			return unstructured.SetNestedMap(route.Object, map[string]interface{}{
				"name": "grid-gateway",
				"port": int64(443),
			}, "spec", "to")
		}); err != nil {
			return false, err
		}

		grid.Status.IngressEndpointURL = "https://" + host
		break
	}

	for i := range grid.Status.Sites {
		if grid.Status.Sites[i].Role != aigridv1alpha1.SiteRoleHub {
			grid.Status.Sites[i].Phase = aigridv1alpha1.SitePhaseGridReady
		}
	}
	if grid.Status.IngressEndpointURL == "" {
		return false, fmt.Errorf("no ingress site with apps domain to expose the gateway")
	}
	return true, nil
}
