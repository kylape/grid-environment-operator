package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	aigridv1alpha1 "github.com/kylape/grid-environment-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	clientcmd "k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultZoneServerImage is the host cluster's own CoreDNS build, pinned by
// digest: guaranteed pullable on the host and version-matched to the DNS
// machinery the workaround interoperates with. Overridable via flag.
const DefaultZoneServerImage = "quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:a5d57b0110669d9ecbd4b3202af13b8b1c859a072f2262899dfd229bdb0f71be"

const (
	// gitOpsNamespace is where the root Argo Application lands on the hub.
	gitOpsNamespace = "openshift-gitops"

	// zoneServerName names the per-grid CoreDNS deployment and service.
	zoneServerName = "grid-zone-server"

	// gridLabel / roleLabel mark objects as belonging to a specific grid.
	gridLabel = "aigrid.llm.redhat.com/grid"
	roleLabel = "aigrid.llm.redhat.com/role"

	// peerGroupLabel is applied to LWEs as identifying metadata. It no
	// longer activates LWE-operator features: this operator owns all
	// grid networking policies.
	peerGroupLabel = "llm.redhat.com/peer-group"

	// dnsServerEntryPrefix marks the dns.operator server entries this
	// operator manages, so merges never clobber foreign entries
	// (e.g. Lighthouse's clusterset.local).
	dnsServerEntryPrefix = "aigrid-"

	// klusterletNamespace existing on a guest means the ACM agent is
	// installed; used to detect stale ManagedClusters.
	klusterletNamespace = "open-cluster-management-agent"
)

// GVKs for APIs the operator drives through unstructured clients.
var (
	lweGVK = schema.GroupVersionKind{Group: "llm.redhat.com", Version: "v1alpha1", Kind: "LLMWorkloadEnvironment"}

	dnsGVK = schema.GroupVersionKind{Group: "operator.openshift.io", Version: "v1", Kind: "DNS"}

	ingressControllerGVK = schema.GroupVersionKind{Group: "operator.openshift.io", Version: "v1", Kind: "IngressController"}

	managedClusterGVK = schema.GroupVersionKind{Group: "cluster.open-cluster-management.io", Version: "v1", Kind: "ManagedCluster"}

	applicationGVK = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}

	argocdGVK = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "ArgoCD"}

	applicationSetGVK = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "ApplicationSet"}

	networkGVK = schema.GroupVersionKind{Group: "config.openshift.io", Version: "v1", Kind: "Network"}

	subscriptionGVK = schema.GroupVersionKind{Group: "operators.coreos.com", Version: "v1alpha1", Kind: "Subscription"}

	gridSiteGVK = schema.GroupVersionKind{Group: "grid.praxis.fast", Version: "v1alpha1", Kind: "GridSite"}

	operatorGroupGVK = schema.GroupVersionKind{Group: "operators.coreos.com", Version: "v1", Kind: "OperatorGroup"}

	routeGVK = schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"}
)

// SiteName resolves a site spec's LWE name.
func SiteName(grid *aigridv1alpha1.AIGrid, site aigridv1alpha1.SiteSpec) string {
	if site.Name != "" {
		return site.Name
	}
	return fmt.Sprintf("%s-%s", grid.Name, site.Role)
}

// DNSLweName is the namespace-mode LWE hosting the grid's zone server.
func DNSLweName(grid *aigridv1alpha1.AIGrid) string {
	return grid.Name + "-dns"
}

// ControlPlaneNamespace is the HyperShift control-plane namespace for an
// LWE: the namespace whose NetworkPolicies govern the guest's traffic and
// where the passthrough ingress service lives.
func ControlPlaneNamespace(lweName string) string {
	return "env-" + lweName + "-cluster"
}

func peerGroup(grid *aigridv1alpha1.AIGrid) string {
	if grid.Spec.PeerGroup != "" {
		return grid.Spec.PeerGroup
	}
	return grid.Name
}

func gridDuration(grid *aigridv1alpha1.AIGrid) string {
	if grid.Spec.Duration != "" {
		return grid.Spec.Duration
	}
	return "72h"
}

// spokeSites returns the grid's non-hub sites.
func spokeSites(grid *aigridv1alpha1.AIGrid) []aigridv1alpha1.SiteSpec {
	var spokes []aigridv1alpha1.SiteSpec
	for _, site := range grid.Spec.Sites {
		if site.Role != aigridv1alpha1.SiteRoleHub {
			spokes = append(spokes, site)
		}
	}
	return spokes
}

func object(gvk schema.GroupVersionKind, namespace, name string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvk)
	o.SetNamespace(namespace)
	o.SetName(name)
	return o
}

func getUnstructured(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, namespace, name string) (*unstructured.Unstructured, error) {
	o := object(gvk, namespace, name)
	if err := c.Get(ctx, client.ObjectKeyFromObject(o), o); err != nil {
		return nil, err
	}
	return o, nil
}

func deleteUnstructured(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, namespace, name string) error {
	return client.IgnoreNotFound(c.Delete(ctx, object(gvk, namespace, name)))
}

func listUnstructured(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, namespace string, labels client.MatchingLabels) (*unstructured.UnstructuredList, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk)
	opts := []client.ListOption{client.InNamespace(namespace)}
	if labels != nil {
		opts = append(opts, labels)
	}
	if err := c.List(ctx, list, opts...); err != nil {
		return nil, err
	}
	return list, nil
}

// guestClient builds a controller-runtime client for a guest cluster from
// the base64 kubeconfig published in its LWE's status. Clients are rebuilt
// on every reconcile: reconciles are tens of seconds apart and guest tokens
// expire, so caching buys nothing but stale-credential bugs.
func guestClient(kubeconfigBase64 string) (client.Client, error) {
	if kubeconfigBase64 == "" {
		return nil, fmt.Errorf("LWE has no kubeconfig in status")
	}
	raw, err := base64.StdEncoding.DecodeString(kubeconfigBase64)
	if err != nil {
		return nil, fmt.Errorf("decoding kubeconfig: %w", err)
	}
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing kubeconfig: %w", err)
	}
	return client.New(restConfig, client.Options{Scheme: scheme.Scheme})
}

// lweKubeconfig returns the guest kubeconfig published by an LWE.
func lweKubeconfig(lwe *unstructured.Unstructured) (string, error) {
	value, found, err := unstructured.NestedString(lwe.Object, "status", "kubeconfig")
	if err != nil {
		return "", err
	}
	if !found || value == "" {
		return "", fmt.Errorf("LWE %s has no kubeconfig in status", lwe.GetName())
	}
	return value, nil
}

// lweReadyCondition returns the LWE's Ready condition: its status, plus
// the reason and message the LWE operator recorded for it. The reason is
// surfaced into the AIGrid status so a stuck site explains itself.
func lweReadyCondition(lwe *unstructured.Unstructured) (reason, message string, ready bool) {
	conditions, found, err := unstructured.NestedSlice(lwe.Object, "status", "conditions")
	if err != nil || !found {
		return "NoReadyCondition", "LWE has not reported a Ready condition yet", false
	}
	for _, raw := range conditions {
		condition, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if condition["type"] == "Ready" {
			status, _ := condition["status"].(string)
			reason, _ := condition["reason"].(string)
			message, _ := condition["message"].(string)
			if status == "True" {
				return reason, message, true
			}
			return defaultString(reason, "NotReady"), message, false
		}
	}
	return "NoReadyCondition", "LWE has not reported a Ready condition yet", false
}

// lweReady reads the LWE's Ready condition.
func lweReady(lwe *unstructured.Unstructured) bool {
	_, _, ready := lweReadyCondition(lwe)
	return ready
}

// managedClusterJoined reads the ManagedClusterJoined condition.
func managedClusterJoined(mc *unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(mc.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, raw := range conditions {
		condition, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if condition["type"] == "ManagedClusterJoined" && condition["status"] == "True" {
			return true
		}
	}
	return false
}

// gridSiteConnected reads the GridSite Connected condition. After the DNS
// workaround, this condition is the end-to-end path probe: the grid's own
// checker dials the advertised endpoint through the wired L4 path.
func gridSiteConnected(site *unstructured.Unstructured) (string, bool) {
	conditions, found, err := unstructured.NestedSlice(site.Object, "status", "conditions")
	if err != nil || !found {
		return "Unknown", false
	}
	for _, raw := range conditions {
		condition, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if condition["type"] == "Connected" {
			status, _ := condition["status"].(string)
			return status, status == "True"
		}
	}
	return "Unknown", false
}

// splitYAMLDocuments splits a multi-document YAML string, dropping empties.
func splitYAMLDocuments(manifest string) []string {
	var documents []string
	for _, document := range strings.Split(manifest, "\n---") {
		if strings.TrimSpace(document) != "" {
			documents = append(documents, document)
		}
	}
	return documents
}

func objectMetaLabels(object metav1.Object) map[string]string {
	labels := object.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	return labels
}
