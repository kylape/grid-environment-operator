package controller

import (
	"context"
	"fmt"
	"strings"

	aigridv1alpha1 "github.com/kylape/grid-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"
)

// managedClusterRoleLabel is the label the GitOps fan-out reads to decide
// which charts land on which spoke. Its key is fixed by the grid charts.
const managedClusterRoleLabel = "ai-grid/role"

// managedClusterFanOutLabels are the labels the GitOps fan-out selects and
// templates on, replicated from the validated demo chain:
//
//   - ai-grid/enabled gates every spoke placement — without it nothing
//     installs, which is exactly how a grid silently stalls
//   - ai-grid/submariner-private-network selects the KubeVirt networking
//     mode (forceUDPEncaps, no NAT-T, one gateway) the topology requires
//   - ai-grid/machine-network carries the host pod network peers arrive
//     from, slashes replaced by underscores
func (r *AIGridReconciler) managedClusterFanOutLabels(ctx context.Context) (map[string]string, error) {
	cidr, err := r.hostPodNetworkCIDR(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"ai-grid/enabled":                    "true",
		"ai-grid/submariner-private-network": "true",
		"ai-grid/machine-network":            strings.ReplaceAll(cidr, "/", "_"),
	}, nil
}

// hostPodNetworkCIDR reads the host cluster's pod network CIDR.
func (r *AIGridReconciler) hostPodNetworkCIDR(ctx context.Context) (string, error) {
	network := object(networkGVK, "", "cluster")
	if err := r.Get(ctx, client.ObjectKeyFromObject(network), network); err != nil {
		return "", fmt.Errorf("reading host network config: %w", err)
	}
	clusterNetwork, found, err := unstructured.NestedSlice(network.Object, "spec", "clusterNetwork")
	if err != nil || !found || len(clusterNetwork) == 0 {
		return "", fmt.Errorf("host network config has no clusterNetwork")
	}
	entry, ok := clusterNetwork[0].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("host network config clusterNetwork entry is malformed")
	}
	cidr, _ := entry["cidr"].(string)
	if cidr == "" {
		return "", fmt.Errorf("host network config clusterNetwork entry has no cidr")
	}
	return cidr, nil
}

// importSites is the Importing phase: register every spoke with the hub's
// ACM and wait for the joins.
//
// Recreating a site LWE under the same name requires deleting the hub-side
// ManagedCluster first: the import flow trusts persisted join state that
// does not survive guest re-creation, leaving the new guest agentless while
// the hub believes it joined. Stale clusters are detected by the absence of
// the klusterlet namespace on the guest.
func (r *AIGridReconciler) importSites(ctx context.Context, grid *aigridv1alpha1.AIGrid) (bool, error) {
	hub := r.hubClient(ctx, grid)
	if hub == nil {
		return false, nil
	}

	allJoined := true
	fanOutLabels, err := r.managedClusterFanOutLabels(ctx)
	if err != nil {
		return false, err
	}
	for _, site := range spokeSites(grid) {
		name := SiteName(grid, site)

		guest, err := r.siteKubeconfig(ctx, grid, name)
		if err != nil {
			return false, err
		}

		mc, err := getUnstructured(ctx, hub, managedClusterGVK, "", name)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return false, err
			}
			mc = nil
		}

		if mc != nil && !r.guestHasKlusterlet(ctx, guest) {
			// Stale registration from a destroyed guest: remove it so the
			// fresh import below runs.
			if err := deleteUnstructured(ctx, hub, managedClusterGVK, "", name); err != nil {
				return false, err
			}
			mc = nil
		}

		if mc == nil {
			mc = object(managedClusterGVK, "", name)
			if _, err := controllerutil.CreateOrUpdate(ctx, hub, mc, func() error {
				labels := objectMetaLabels(mc)
				labels[gridLabel] = grid.Name
				labels[peerGroupLabel] = peerGroup(grid)
				for key, value := range fanOutLabels {
					labels[key] = value
				}
				mc.SetLabels(labels)
				// Accept the cluster on creation: without it the spoke's
				// CSR is never approved and the join hangs unless ACM's
				// auto-accept is enabled.
				return unstructured.SetNestedField(mc.Object, true, "spec", "hubAcceptsClient")
			}); err != nil {
				return false, err
			}
		} else {
			// Existing ManagedCluster: reconcile the fan-out labels —
			// grids created before a label was added, or labels lost
			// hub-side, must not silently stall the fan-out.
			updated := false
			labels := objectMetaLabels(mc)
			for key, value := range fanOutLabels {
				if labels[key] != value {
					labels[key] = value
					updated = true
				}
			}
			if updated {
				mc.SetLabels(labels)
				if err := hub.Update(ctx, mc); err != nil {
					return false, err
				}
			}
		}

		if !managedClusterJoined(mc) {
			allJoined = false
			if err := r.applyImportDocuments(ctx, hub, guest, name); err != nil {
				return false, err
			}
		}

		// The application-manager addon's install strategy is Manual: the
		// per-cluster ManagedClusterAddOn must be created by the importer,
		// exactly as the repo's import script does. Without it the addon
		// never deploys, its cluster secret never reaches the hub, and the
		// argo-registration GitOpsCluster cannot register the spoke with
		// Argo — so the fan-out generates no spoke applications and the
		// grid silently stalls (found live).
		addon := object(managedClusterAddOnGVK, name, "application-manager")
		if _, err := controllerutil.CreateOrUpdate(ctx, hub, addon, func() error {
			if err := unstructured.SetNestedMap(addon.Object, map[string]interface{}{}, "spec"); err != nil {
				return err
			}
			return nil
		}); err != nil {
			return false, err
		}
	}
	return allJoined, nil
}

// guestHasKlusterlet checks whether the ACM agent namespace exists on the
// guest — the cheapest reliable marker that the agent is installed.
func (r *AIGridReconciler) guestHasKlusterlet(ctx context.Context, guest client.Client) bool {
	namespace := &corev1.Namespace{}
	if err := guest.Get(ctx, client.ObjectKey{Name: klusterletNamespace}, namespace); err != nil {
		return false
	}
	return true
}

// applyImportDocuments fetches the hub-generated import secret and applies
// its manifests to the guest. The secret contains the klusterlet CRDs and
// the import document; both are multi-document YAML.
func (r *AIGridReconciler) applyImportDocuments(ctx context.Context, hub, guest client.Client, name string) error {
	secret := &corev1.Secret{}
	if err := hub.Get(ctx, client.ObjectKey{Namespace: name, Name: name + "-import"}, secret); err != nil {
		// The hub generates the secret shortly after ManagedCluster
		// creation; retry on the next reconcile.
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	for _, key := range []string{"crds.yaml", "import.yaml"} {
		raw, ok := secret.Data[key]
		if !ok {
			continue
		}
		for _, document := range splitYAMLDocuments(string(raw)) {
			if err := applyDocument(ctx, guest, document); err != nil {
				return fmt.Errorf("applying %s for %s: %w", key, name, err)
			}
		}
	}
	return nil
}

// applyDocument applies a single YAML document to a cluster using
// server-side apply. Documents are re-applied on every reconcile because
// the credentials inside them rotate hub-side: a create-only apply leaves
// agents running stale tokens forever. (Found live: the hub rotated the
// klusterlet's bootstrap credential after the initial apply, and the join
// hung silently on HTTP 401 until the secret was refreshed.)
func applyDocument(ctx context.Context, c client.Client, document string) error {
	var content map[string]interface{}
	if err := yaml.Unmarshal([]byte(document), &content); err != nil {
		return fmt.Errorf("parsing document: %w", err)
	}
	if content == nil {
		return nil
	}
	documentObject := &unstructured.Unstructured{Object: content}
	if documentObject.GetKind() == "" || documentObject.GetAPIVersion() == "" {
		return nil
	}
	if err := c.Patch(ctx, documentObject, client.Apply, client.ForceOwnership,
		client.FieldOwner("aigrid.llm.redhat.com/import")); err != nil {
		return fmt.Errorf("applying %s %s/%s: %w",
			documentObject.GetKind(), documentObject.GetNamespace(), documentObject.GetName(), err)
	}
	return nil
}
