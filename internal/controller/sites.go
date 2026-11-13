package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	aigridv1alpha1 "github.com/kylape/grid-environment-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// provision ensures the zone-server LWE (namespace mode) and one HyperShift
// LWE per site, then waits for all of them to become Ready.
func (r *AIGridReconciler) provision(ctx context.Context, grid *aigridv1alpha1.AIGrid) (bool, error) {
	// Fail fast on a bad pull secret: both demo incidents (missing
	// path-scoped key; replaced instead of merged composition) would have
	// been caught here before any LWE existed. Retried on the phase cadence
	// so fixing the secret self-heals.
	if err := r.validatePullSecret(ctx, grid); err != nil {
		return false, err
	}

	desired := map[string]bool{}
	for _, site := range grid.Spec.Sites {
		desired[SiteName(grid, site)] = true
	}
	desired[DNSLweName(grid)] = true

	// Remove LWEs for sites that no longer exist in the spec.
	existing := &unstructured.UnstructuredList{}
	existing.SetGroupVersionKind(lweGVK)
	if err := r.List(ctx, existing, client.InNamespace(grid.Namespace), client.MatchingLabels{gridLabel: grid.Name}); err != nil {
		return false, err
	}
	for _, lwe := range existing.Items {
		if !desired[lwe.GetName()] {
			if err := r.Delete(ctx, &lwe); err != nil {
				return false, err
			}
		}
	}

	// The zone-server LWE: namespace mode, same lifetime as the grid.
	dnsLwe := object(lweGVK, grid.Namespace, DNSLweName(grid))
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dnsLwe, func() error {
		setGridLabels(dnsLwe, grid, "")
		if err := unstructured.SetNestedField(dnsLwe.Object, "Namespace", "spec", "mode"); err != nil {
			return err
		}
		return applyClusterQueue(dnsLwe, grid)
	}); err != nil {
		return false, err
	}
	if err := r.reconcileDuration(ctx, dnsLwe, grid); err != nil {
		return false, err
	}

	allReady := lweReady(dnsLwe)
	grid.Status.DNSLweName = DNSLweName(grid)

	// Site LWEs: HyperShift mode with the grid's pull secret.
	siteReadiness := map[string][2]string{}
	for _, site := range grid.Spec.Sites {
		name := SiteName(grid, site)
		siteLwe := object(lweGVK, grid.Namespace, name)
		desiredSize := defaultString(site.Size, "small")
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, siteLwe, func() error {
			setGridLabels(siteLwe, grid, string(site.Role))
			if err := unstructured.SetNestedField(siteLwe.Object, "HyperShift", "spec", "mode"); err != nil {
				return err
			}
			// Size is immutable on the LWE API: refuse a change here with
			// a clear message rather than letting the apiserver reject
			// the update generically.
			existingSize, found, err := unstructured.NestedString(siteLwe.Object, "spec", "hypershift", "size")
			if err != nil {
				return err
			}
			if found && existingSize != "" && existingSize != desiredSize {
				return fmt.Errorf("site %s: size %q cannot be changed to %q on an existing LWE; remove and re-add the site instead", name, existingSize, desiredSize)
			}
			if err := unstructured.SetNestedMap(siteLwe.Object, map[string]interface{}{
				"size":       desiredSize,
				"replicas":   int64(defaultInt32(site.Replicas, 1)),
				"pullSecret": map[string]interface{}{"name": grid.Spec.PullSecretRef.Name},
				// Registry mirrors are platform facts, not grid policy:
				// pre-release rhoai/rhaii images exist only on their
				// mirrors, and the LWE platform forbids guests from
				// creating IDMS objects directly (ValidatingAdmissionPolicy
				// "mirror") — the HostedCluster, configured through this
				// field, is the only sanctioned path (found live: without
				// it, the ACM mirror distribution goes NonCompliant and
				// every RHOAI bundle unpack fails its deadline).
				"imageDigestMirrorSet": imageDigestMirrorSet(),
			}, "spec", "hypershift"); err != nil {
				return err
			}
			return applyClusterQueue(siteLwe, grid)
		}); err != nil {
			return false, err
		}
		if err := r.reconcileDuration(ctx, siteLwe, grid); err != nil {
			return false, err
		}
		if err := r.reconcileSiteDrift(ctx, siteLwe, site); err != nil {
			return false, err
		}
		if reason, message, ready := lweReadyCondition(siteLwe); !ready {
			allReady = false
			siteReadiness[name] = [2]string{reason, message}
		}
	}

	// Rebuild status.Sites in spec order, preserving enrichment from later
	// phases (appsDomain, gridSite) across reconciles.
	previous := siteStatusIndex(grid)
	grid.Status.Sites = nil
	for _, site := range grid.Spec.Sites {
		status := aigridv1alpha1.SiteStatus{
			Role:    site.Role,
			Name:    SiteName(grid, site),
			LWEName: SiteName(grid, site),
		}
		if old, ok := previous[status.Name]; ok {
			status.Phase = old.Phase
			status.AppsDomain = old.AppsDomain
			status.GridSite = old.GridSite
			status.KubeconfigSecret = old.KubeconfigSecret
		}
		if allReady {
			status.Phase = aigridv1alpha1.SitePhaseImporting
		}
		if blocked, ok := siteReadiness[status.Name]; ok {
			status.Message = strings.TrimSpace(blocked[0] + ": " + blocked[1])
		}
		grid.Status.Sites = append(grid.Status.Sites, status)
	}

	// Aggregate the earliest expiration across all of the grid's LWEs.
	if allReady {
		earliest := time.Time{}
		for _, lwe := range append(existing.Items, *dnsLwe) {
			if raw, found, _ := unstructured.NestedString(lwe.Object, "status", "expirationTimestamp"); found && raw != "" {
				if timestamp, err := time.Parse(time.RFC3339, raw); err == nil {
					if earliest.IsZero() || timestamp.Before(earliest) {
						earliest = timestamp
					}
				}
			}
		}
		// The list above may be stale for just-created LWEs; read the site
		// LWEs again for expiration. Simplicity over cleverness: the next
		// reconcile corrects any miss.
		for _, site := range grid.Spec.Sites {
			lwe, err := getUnstructured(ctx, r.Client, lweGVK, grid.Namespace, SiteName(grid, site))
			if err != nil {
				continue
			}
			if raw, found, _ := unstructured.NestedString(lwe.Object, "status", "expirationTimestamp"); found && raw != "" {
				if timestamp, err := time.Parse(time.RFC3339, raw); err == nil {
					if earliest.IsZero() || timestamp.Before(earliest) {
						earliest = timestamp
					}
				}
			}
		}
		if !earliest.IsZero() {
			grid.Status.ExpirationTimestamp = &metav1.Time{Time: earliest}
		}
	}

	return allReady, nil
}

// reconcileDuration propagates spec.duration drift to an LWE. LWE duration
// is mutable: expiration extends without recreating the environment.
func (r *AIGridReconciler) reconcileDuration(ctx context.Context, lwe *unstructured.Unstructured, grid *aigridv1alpha1.AIGrid) error {
	desired := gridDuration(grid)
	current, found, err := unstructured.NestedString(lwe.Object, "spec", "duration")
	if err != nil {
		return err
	}
	if found && current == desired {
		return nil
	}
	if err := unstructured.SetNestedField(lwe.Object, desired, "spec", "duration"); err != nil {
		return err
	}
	return r.Update(ctx, lwe)
}

// reconcileSiteDrift enforces the LWE API's own immutability rules. Mode,
// size, and profile cannot change on an existing LWE; the AIGrid spec must
// not silently promise otherwise. Replicas scale in place.
func (r *AIGridReconciler) reconcileSiteDrift(ctx context.Context, lwe *unstructured.Unstructured, site aigridv1alpha1.SiteSpec) error {
	size, _, _ := unstructured.NestedString(lwe.Object, "spec", "hypershift", "size")
	if size != "" && site.Size != "" && size != site.Size {
		return fmt.Errorf("site %s: size %q cannot be changed to %q on an existing LWE; remove and re-add the site instead", lwe.GetName(), size, site.Size)
	}
	replicas, found, err := unstructured.NestedInt64(lwe.Object, "spec", "hypershift", "replicas")
	if err != nil {
		return err
	}
	if found && site.Replicas > 0 && replicas != int64(site.Replicas) {
		if err := unstructured.SetNestedField(lwe.Object, int64(site.Replicas), "spec", "hypershift", "replicas"); err != nil {
			return err
		}
		return r.Update(ctx, lwe)
	}
	return nil
}

func setGridLabels(object metav1.Object, grid *aigridv1alpha1.AIGrid, role string) {
	labels := objectMetaLabels(object)
	labels[gridLabel] = grid.Name
	labels[peerGroupLabel] = peerGroup(grid)
	if role != "" {
		labels[roleLabel] = role
	}
	object.SetLabels(labels)
}

// siteKubeconfig fetches a site's LWE and returns a guest client.
func (r *AIGridReconciler) siteKubeconfig(ctx context.Context, grid *aigridv1alpha1.AIGrid, name string) (client.Client, error) {
	lwe, err := getUnstructured(ctx, r.Client, lweGVK, grid.Namespace, name)
	if err != nil {
		return nil, err
	}
	kubeconfig, err := lweKubeconfig(lwe)
	if err != nil {
		return nil, err
	}
	return guestClient(kubeconfig)
}

// hubClient returns a client for the grid's hub site, or nil if the hub
// LWE has no published kubeconfig yet.
func (r *AIGridReconciler) hubClient(ctx context.Context, grid *aigridv1alpha1.AIGrid) client.Client {
	for _, site := range grid.Spec.Sites {
		if site.Role != aigridv1alpha1.SiteRoleHub {
			continue
		}
		hub, err := r.siteKubeconfig(ctx, grid, SiteName(grid, site))
		if err != nil {
			return nil
		}
		return hub
	}
	return nil
}

func siteStatusIndex(grid *aigridv1alpha1.AIGrid) map[string]aigridv1alpha1.SiteStatus {
	index := map[string]aigridv1alpha1.SiteStatus{}
	for _, status := range grid.Status.Sites {
		index[status.Name] = status
	}
	return index
}

// dnsNamespace is the host namespace backing the grid's namespace-mode LWE.
func (r *AIGridReconciler) dnsNamespace(ctx context.Context, grid *aigridv1alpha1.AIGrid) (string, error) {
	lwe, err := getUnstructured(ctx, r.Client, lweGVK, grid.Namespace, DNSLweName(grid))
	if err != nil {
		return "", err
	}
	namespace, found, err := unstructured.NestedString(lwe.Object, "status", "namespaceRef", "name")
	if err != nil {
		return "", err
	}
	if !found || namespace == "" {
		return "", fmt.Errorf("DNS LWE %s has no namespace reference yet", lwe.GetName())
	}
	return namespace, nil
}

func defaultString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func defaultInt32(value, fallback int32) int32 {
	if value > 0 {
		return value
	}
	return fallback
}

// imageDigestMirrorSet mirrors pre-release registries to where the images
// actually exist, matching the validated demo configuration exactly.
func imageDigestMirrorSet() map[string]interface{} {
	mirror := func(source string, mirrors ...string) map[string]interface{} {
		list := make([]interface{}, len(mirrors))
		for i, m := range mirrors {
			list[i] = m
		}
		return map[string]interface{}{
			"source":             source,
			"mirrors":            list,
			"mirrorSourcePolicy": "NeverContactSource",
		}
	}
	return map[string]interface{}{
		"imageDigestMirrors": []interface{}{
			mirror("registry.redhat.io/rhoai", "quay.io/rhoai"),
			mirror("registry.redhat.io/rhaii", "registry.stage.redhat.io/rhaii"),
			mirror("registry.redhat.io/rhaii-early-access", "registry.stage.redhat.io/rhaii-early-access"),
			mirror("registry.redhat.io/rhaii-fast", "registry.stage.redhat.io/rhaii-fast"),
		},
	}
}

// applyClusterQueue sets the optional per-grid Kueue ClusterQueue on an
// LWE. The LWE API makes clusterQueue immutable, so a change on an
// existing LWE is an error the user resolves by removing and re-adding
// the site — never a silent update. When the grid specifies no queue, the
// LWE's own default (inference-workspaces) applies.
func applyClusterQueue(lwe *unstructured.Unstructured, grid *aigridv1alpha1.AIGrid) error {
	desired := grid.Spec.ClusterQueue
	if desired == "" {
		return nil
	}
	existing, found, err := unstructured.NestedString(lwe.Object, "spec", "clusterQueue")
	if err != nil {
		return err
	}
	if found && existing != "" {
		if existing != desired {
			return fmt.Errorf("site %s: clusterQueue %q cannot be changed to %q on an existing LWE; remove and re-add the site instead", lwe.GetName(), existing, desired)
		}
		return nil
	}
	return unstructured.SetNestedField(lwe.Object, desired, "spec", "clusterQueue")
}

// blockingSiteMessage surfaces why a site is stuck, from the LWE's own
// Ready condition, so `kubectl get aigrid` answers "why is this not
// ready" without inspecting the LWE.
func blockingSiteMessage(grid *aigridv1alpha1.AIGrid) string {
	for i := range grid.Status.Sites {
		site := &grid.Status.Sites[i]
		if site.Message != "" {
			return fmt.Sprintf("site %s: %s", site.Name, site.Message)
		}
	}
	return ""
}
