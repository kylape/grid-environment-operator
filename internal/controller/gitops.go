package controller

import (
	"context"
	"fmt"
	"strings"

	aigridv1alpha1 "github.com/kylape/grid-environment-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
func (r *AIGridReconciler) installGitOps(ctx context.Context, grid *aigridv1alpha1.AIGrid) (bool, string, error) {
	hub := r.hubClient(ctx, grid)
	if hub == nil {
		return false, "", nil
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
			return false, "", err
		}
	}

	// Annotate the OperatorGroups once the fan-out creates them.
	for _, site := range spokeSites(grid) {
		guest, err := r.siteKubeconfig(ctx, grid, SiteName(grid, site))
		if err != nil {
			return false, "", err
		}
		if err := r.annotateOperatorGroups(ctx, guest); err != nil {
			return false, "", err
		}
	}

	// The root Application was created by the HubBootstrapped phase; this
	// phase waits for it to finish syncing the fan-out. Once handed off
	// (deleted because the repo's own root Application took over), the
	// installation is self-managing and this phase is done.
	application, err := getUnstructured(ctx, hub, applicationGVK, gitOpsNamespace, grid.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return true, "", nil
		}
		return false, "", err
	}

	// Readiness: the root Application reports Synced.
	sync, found, err := unstructured.NestedString(application.Object, "status", "sync", "status")
	if err != nil || !found {
		return false, "", nil
	}
	// Surface failed fan-out syncs: a stuck Application (typically retries
	// exhausted against a dependency that arrived late) must be visible in
	// the AIGrid status rather than a silent hang. Attribution only —
	// re-triggering syncs is deliberately not automated, because deleting
	// an Application cascades to its resources, which is destructive for
	// stateful apps.
	blocker, err := r.fanOutSyncErrors(ctx, hub, grid)
	if err != nil {
		return false, "", err
	}
	if blocker != "" {
		return false, blocker, nil
	}
	return sync == "Synced", "", nil
}

// fanOutSyncErrors lists the grid's fan-out Applications and reports any
// whose last sync operation failed — typically retries exhausted against a
// dependency that arrived late, like model manifests applied before the
// serving CRD landed on the spoke.
func (r *AIGridReconciler) fanOutSyncErrors(ctx context.Context, hub client.Client, grid *aigridv1alpha1.AIGrid) (string, error) {
	apps, err := listUnstructured(ctx, hub, applicationGVK, gitOpsNamespace, nil)
	if err != nil {
		return "", err
	}
	var stuck []string
	for _, app := range apps.Items {
		name := app.GetName()
		if name == grid.Name || !isGridFanOutApp(name, grid) {
			continue
		}
		phase, found, _ := unstructured.NestedString(app.Object, "status", "operationState", "phase")
		if !found || (phase != "Failed" && phase != "Error") {
			continue
		}
		message, _, _ := unstructured.NestedString(app.Object, "status", "operationState", "message")
		if len(message) > 160 {
			message = message[:160] + "..."
		}
		stuck = append(stuck, fmt.Sprintf("%s: %s", name, message))
	}
	if len(stuck) == 0 {
		return "", nil
	}
	return "application sync failed (needs re-sync): " + strings.Join(stuck, "; "), nil
}

// isGridFanOutApp matches Applications generated for this grid's spoke
// sites: fan-out app names embed the site name (e.g.
// rhoai-ci-grid-inference-c6f3cf84, acm-submariner-ci-grid-ingress).
func isGridFanOutApp(name string, grid *aigridv1alpha1.AIGrid) bool {
	for _, site := range grid.Status.Sites {
		if site.Role == aigridv1alpha1.SiteRoleHub {
			continue
		}
		if strings.Contains(name, "-"+site.Name) {
			return true
		}
	}
	return false
}

// guestServiceCA reads the service-serving CA the guest signs its
// in-cluster service certificates with — the reencrypt route's backend
// CA.
func guestServiceCA(ctx context.Context, guest client.Client) (string, error) {
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "openshift-service-ca.crt", Namespace: gridNamespace}}
	if err := guest.Get(ctx, client.ObjectKeyFromObject(configMap), configMap); err != nil {
		return "", fmt.Errorf("reading service CA on guest: %w", err)
	}
	certificate := configMap.Data["service-ca.crt"]
	if certificate == "" {
		return "", fmt.Errorf("service CA configmap has no service-ca.crt")
	}
	return certificate, nil
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

		if site.Role == aigridv1alpha1.SiteRoleIngress {
			// Ingress sites deploy only the consumer gateway: their
			// GridSite structurally reports AddressMissing (no provider
			// endpoint to self-check). Readiness for them is the
			// consumer gateway being available.
			gateway := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "grid-gateway", Namespace: gridNamespace}}
			if err := guest.Get(ctx, client.ObjectKeyFromObject(gateway), gateway); err != nil {
				allConnected = false
				continue
			}
			if gateway.Status.AvailableReplicas < 1 {
				allConnected = false
			}
			continue
		}

		// Inference sites run provider gateways: their GridSite Connected
		// condition is the end-to-end path probe through the wired DNS.
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
		// The route is reencrypt rather than passthrough: the guest router
		// terminates TLS with its ingress certificate, whose wildcard
		// covers the route host, and re-encrypts to the gateway using the
		// service CA. Passthrough would present the gateway's service
		// certificate, whose SAN is the in-cluster service name — a
		// hostname mismatch for every browser (found live).
		serviceCA, err := guestServiceCA(ctx, guest)
		if err != nil {
			return false, err
		}
		route := object(routeGVK, gridNamespace, "grid-"+grid.Name)
		if _, err := controllerutil.CreateOrUpdate(ctx, guest, route, func() error {
			if err := unstructured.SetNestedField(route.Object, host, "spec", "host"); err != nil {
				return err
			}
			if err := unstructured.SetNestedMap(route.Object, map[string]interface{}{
				"termination":                   "Reencrypt",
				"destinationCACertificate":      serviceCA,
				"insecureEdgeTerminationPolicy": "Redirect",
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
