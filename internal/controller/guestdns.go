package controller

import (
	"context"
	"fmt"

	aigridv1alpha1 "github.com/kylape/grid-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// mergeDNSServers merges this operator's forward-zone entries into a
// dns.operator spec.servers list. Foreign entries — notably Lighthouse's
// clusterset.local, injected by Submariner — are preserved untouched;
// entries previously managed by this operator are replaced; entries whose
// zone no longer belongs to the grid are removed.
//
// A blind merge patch here destroys cross-cluster service discovery; this
// function is the reason wiring is a read-modify-write, never a patch.
func mergeDNSServers(existing []interface{}, zoneServiceIP string, zones []string) []interface{} {
	desired := map[string]bool{}
	for _, zone := range zones {
		desired[dnsServerEntryPrefix+zone] = true
	}

	merged := make([]interface{}, 0, len(existing)+len(zones))
	for _, raw := range existing {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(entry, "name")
		_, managedByUs := desired[name]
		if managedByUs {
			continue // replaced below
		}
		if hasManagedPrefix(name) && !desired[name] {
			continue // stale entry for a zone we no longer serve
		}
		merged = append(merged, entry)
	}

	for _, zone := range zones {
		merged = append(merged, map[string]interface{}{
			"name":  dnsServerEntryPrefix + zone,
			"zones": []interface{}{zone},
			"forwardPlugin": map[string]interface{}{
				"upstreams": []interface{}{zoneServiceIP},
			},
		})
	}
	return merged
}

func hasManagedPrefix(name string) bool {
	return len(name) >= len(dnsServerEntryPrefix) && name[:len(dnsServerEntryPrefix)] == dnsServerEntryPrefix
}

// configureGuestDNS applies the forward zones to a guest's dns.operator CR
// and restarts its CoreDNS pods.
//
// ConfigMap changes do not restart CoreDNS — the dns-operator does not
// reliably roll pods on server-block changes — so the pods are deleted
// explicitly after every change. The daemonset recreates them with the
// current configuration within seconds.
func (r *AIGridReconciler) configureGuestDNS(ctx context.Context, guest client.Client, zoneServiceIP string, zones []string) (bool, error) {
	dns, err := getUnstructured(ctx, guest, dnsGVK, "", "default")
	if err != nil {
		return false, err
	}

	existing, _, _ := unstructured.NestedSlice(dns.Object, "spec", "servers")
	merged := mergeDNSServers(existing, zoneServiceIP, zones)
	if len(existing) == len(merged) && dnsServersEqual(existing, merged) {
		return false, nil
	}

	if err := unstructured.SetNestedSlice(dns.Object, merged, "spec", "servers"); err != nil {
		return false, err
	}
	if err := guest.Update(ctx, dns); err != nil {
		return false, err
	}

	// Restart the guest's CoreDNS pods to pick up the new server blocks.
	pods := &corev1.PodList{}
	if err := guest.List(ctx, pods, client.InNamespace("openshift-dns"), client.MatchingLabels{"dns.operator.openshift.io/daemonset-dns": "default"}); err != nil {
		return true, err
	}
	for _, pod := range pods.Items {
		if err := guest.Delete(ctx, &pod); err != nil {
			return true, err
		}
	}
	return true, nil
}

func dnsServersEqual(a, b []interface{}) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		entryA, okA := a[i].(map[string]interface{})
		entryB, okB := b[i].(map[string]interface{})
		if !okA || !okB {
			return false
		}
		nameA, _, _ := unstructured.NestedString(entryA, "name")
		nameB, _, _ := unstructured.NestedString(entryB, "name")
		if nameA != nameB {
			return false
		}
	}
	return true
}

// guestAppsDomain reads the guest's public apps domain from its default
// IngressController.
func guestAppsDomain(ctx context.Context, guest client.Client) (string, error) {
	ic, err := getUnstructured(ctx, guest, ingressControllerGVK, "openshift-ingress-operator", "default")
	if err != nil {
		return "", err
	}
	domain, found, err := unstructured.NestedString(ic.Object, "status", "domain")
	if err != nil {
		return "", err
	}
	if !found || domain == "" {
		return "", fmt.Errorf("ingresscontroller default has no status.domain yet")
	}
	return domain, nil
}

// passthroughServiceIP finds a site's HyperShift passthrough ingress
// service — the direct L4 entry to the guest's router — in its
// control-plane namespace. The service name carries a random suffix, so it
// is located by prefix.
func (r *AIGridReconciler) passthroughServiceIP(ctx context.Context, controlPlaneNamespace string) (string, error) {
	services := &corev1.ServiceList{}
	if err := r.List(ctx, services, client.InNamespace(controlPlaneNamespace)); err != nil {
		return "", err
	}
	for _, service := range services.Items {
		if len(service.Name) > len("default-ingress-passthrough-service-") &&
			service.Name[:len("default-ingress-passthrough-service-")] == "default-ingress-passthrough-service-" {
			if service.Spec.ClusterIP != "" && service.Spec.ClusterIP != "None" {
				return service.Spec.ClusterIP, nil
			}
		}
	}
	return "", fmt.Errorf("no passthrough service with ClusterIP in %s yet", controlPlaneNamespace)
}

// wire is the Wiring phase: deploy the zone server with records for every
// site, apply the host-side policies, and configure every spoke guest's
// DNS. The hub is excluded — it runs no GridSite and no gateway, so it has
// nothing to wire.
func (r *AIGridReconciler) wire(ctx context.Context, grid *aigridv1alpha1.AIGrid) (bool, error) {
	dnsNamespace, err := r.dnsNamespace(ctx, grid)
	if err != nil {
		return false, err
	}

	// Gather zone records: each site's passthrough IP (host-side) and apps
	// domain (guest-side).
	var records []zoneRecord
	for _, site := range grid.Spec.Sites {
		name := SiteName(grid, site)
		record := zoneRecord{Site: name}

		guest, err := r.siteKubeconfig(ctx, grid, name)
		if err != nil {
			return false, err
		}
		record.AppsDomain, err = guestAppsDomain(ctx, guest)
		if err != nil {
			return false, err
		}
		record.PassthroughIP, err = r.passthroughServiceIP(ctx, ControlPlaneNamespace(name))
		if err != nil {
			return false, err
		}
		records = append(records, record)
	}

	// Deploy (or update) the zone server and capture its service IP.
	zoneServiceIP, err := r.ensureZoneServer(ctx, grid, dnsNamespace, records)
	if err != nil {
		return false, err
	}

	// Apply the host-side policies in every site's control-plane namespace.
	if err := r.applyPolicies(ctx, grid, dnsNamespace, zoneServiceIP, records); err != nil {
		return false, err
	}

	// Configure each spoke guest's DNS: grid.internal plus a
	// router-override zone per site, forwarded to the zone server.
	var zones []string
	zones = append(zones, "grid.internal")
	for _, record := range records {
		zones = append(zones, RouterOverrideZone(record))
	}

	changed := false
	for _, site := range spokeSites(grid) {
		guest, err := r.siteKubeconfig(ctx, grid, SiteName(grid, site))
		if err != nil {
			return false, err
		}
		siteChanged, err := r.configureGuestDNS(ctx, guest, zoneServiceIP, zones)
		if err != nil {
			return false, err
		}
		changed = changed || siteChanged
	}

	// Verify with a live query from the host before declaring the phase
	// done. CoreDNS refuses unmatched zones silently; only a real query
	// proves the config is live.
	if err := probeZone(zoneServiceIP, records); err != nil {
		return false, err
	}

	if changed {
		// Pods were bounced; give CoreDNS a beat to come back before the
		// next verification round.
		return false, nil
	}

	for i := range grid.Status.Sites {
		if grid.Status.Sites[i].Role != aigridv1alpha1.SiteRoleHub {
			grid.Status.Sites[i].Phase = aigridv1alpha1.SitePhaseWired
			if grid.Status.Sites[i].AppsDomain == "" {
				for _, record := range records {
					if record.Site == grid.Status.Sites[i].Name {
						grid.Status.Sites[i].AppsDomain = record.AppsDomain
					}
				}
			}
		}
	}
	return true, nil
}
