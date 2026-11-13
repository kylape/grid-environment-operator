package controller

import (
	"context"

	aigridv1alpha1 "github.com/kylape/grid-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// dnsEgressPolicy permits a guest's virt-launchers to reach the zone
// server on DNS. It is applied in every site's control-plane namespace.
//
// Two rule shapes are required because OVN evaluates egress policy against
// the destination after service DNAT: the pod port (5353) matches
// post-DNAT, the service ClusterIP with :53 matches pre-DNAT. The managed
// HyperShift policies use port-unrestricted podSelectors for exactly this
// reason; we keep the port restrictions and cover both shapes.
func dnsEgressPolicy(dnsNamespace, zoneServiceIP string) *networkingv1.NetworkPolicy {
	port5353 := intstr.FromInt32(5353)
	port53 := intstr.FromInt32(53)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "allow-grid-dns"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"kubevirt.io": "virt-launcher"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
							"kubernetes.io/metadata.name": dnsNamespace,
						}},
						PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": zoneServerName}},
					}},
					Ports: dnsPorts(port5353),
				},
				{
					To: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{CIDR: zoneServiceIP + "/32"},
					}},
					Ports: dnsPorts(port53),
				},
			},
		},
	}
}

// passthroughEgressPolicy permits a guest's virt-launchers to reach every
// site's passthrough ingress service — the direct L4 path to each guest's
// router. It is applied in every site's control-plane namespace.
//
// The podSelector peer is port-unrestricted (mirroring the managed
// policies' idiom) because the post-DNAT destination port is the NodePort
// (30130), not the service port (443); the ipBlock rule carries the
// precise pre-DNAT shape.
func passthroughEgressPolicy(peerNamespaces, passthroughIPs []string) *networkingv1.NetworkPolicy {
	peers := namespacePodPeers(peerNamespaces)

	port443 := intstr.FromInt32(443)
	var ipRules []networkingv1.NetworkPolicyEgressRule
	for _, ip := range passthroughIPs {
		ipRules = append(ipRules, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{CIDR: ip + "/32"},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Port: &port443, Protocol: protoTCP()}},
		})
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "allow-grid-passthrough"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"kubevirt.io": "virt-launcher"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: append([]networkingv1.NetworkPolicyEgressRule{{
				To: peers, // no port restriction: post-DNAT ports vary
			}}, ipRules...),
		},
	}
}

// interSiteUDPPolicy permits the Submariner cable between the grid's
// sites: UDP 500/4500/4490 between virt-launchers. It replaces the LWE
// operator's peer-group feature — grid networking knowledge lives here.
func interSiteUDPPolicy(peerNamespaces []string) *networkingv1.NetworkPolicy {
	var ports []networkingv1.NetworkPolicyPort
	for _, port := range []int32{500, 4500, 4490} {
		p := intstr.FromInt32(port)
		ports = append(ports, networkingv1.NetworkPolicyPort{Port: &p, Protocol: protoUDP()})
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "allow-grid-submariner"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"kubevirt.io": "virt-launcher"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{To: namespacePodPeers(peerNamespaces), Ports: ports}},
		},
	}
}

// zoneServerIngressPolicy admits DNS queries to the zone server from any
// source, on the DNS ports only. The server answers a handful of A records
// and nothing else, so it is not sensitive — and restricting the source
// would block the operator's own verification probe, which runs from the
// operator pod (or a developer's devcontainer), not from the grid's
// virt-launchers. The explicit policy also survives a future where
// namespace-mode LWE namespaces gain default-deny policies; today they
// carry none.
func zoneServerIngressPolicy() *networkingv1.NetworkPolicy {
	port5353 := intstr.FromInt32(5353)
	port53 := intstr.FromInt32(53)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "allow-grid-zone-queries"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": zoneServerName}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				Ports: []networkingv1.NetworkPolicyPort{
					{Port: &port5353, Protocol: protoUDP()},
					{Port: &port5353, Protocol: protoTCP()},
					{Port: &port53, Protocol: protoUDP()},
					{Port: &port53, Protocol: protoTCP()},
				},
			}},
		},
	}
}

// applyPolicies applies the full policy set for a grid: three policies in
// every site's control-plane namespace plus the zone server's ingress
// policy in the DNS namespace.
func (r *AIGridReconciler) applyPolicies(ctx context.Context, grid *aigridv1alpha1.AIGrid, dnsNamespace, zoneServiceIP string, records []zoneRecord) error {
	var siteNamespaces, passthroughIPs []string
	for _, record := range records {
		siteNamespaces = append(siteNamespaces, ControlPlaneNamespace(record.Site))
		if record.PassthroughIP != "" {
			passthroughIPs = append(passthroughIPs, record.PassthroughIP)
		}
	}
	if len(siteNamespaces) == 0 {
		return nil
	}

	policies := []*networkingv1.NetworkPolicy{
		dnsEgressPolicy(dnsNamespace, zoneServiceIP),
		passthroughEgressPolicy(siteNamespaces, passthroughIPs),
		interSiteUDPPolicy(siteNamespaces),
	}
	for _, record := range records {
		target := ControlPlaneNamespace(record.Site)
		for _, policy := range policies {
			applied := policy.DeepCopy()
			applied.Namespace = target
			if err := r.applyPolicy(ctx, applied); err != nil {
				return err
			}
		}
	}

	zonePolicy := zoneServerIngressPolicy()
	zonePolicy.Namespace = dnsNamespace
	return r.applyPolicy(ctx, zonePolicy)
}

func (r *AIGridReconciler) applyPolicy(ctx context.Context, policy *networkingv1.NetworkPolicy) error {
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		return nil
	})
	return err
}

func namespacePodPeers(namespaces []string) []networkingv1.NetworkPolicyPeer {
	var peers []networkingv1.NetworkPolicyPeer
	for _, namespace := range namespaces {
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"kubernetes.io/metadata.name": namespace,
			}},
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubevirt.io": "virt-launcher"}},
		})
	}
	return peers
}

func dnsPorts(port intstr.IntOrString) []networkingv1.NetworkPolicyPort {
	return []networkingv1.NetworkPolicyPort{
		{Port: &port, Protocol: protoUDP()},
		{Port: &port, Protocol: protoTCP()},
	}
}

func protoUDP() *corev1.Protocol {
	proto := corev1.ProtocolUDP
	return &proto
}

func protoTCP() *corev1.Protocol {
	proto := corev1.ProtocolTCP
	return &proto
}
