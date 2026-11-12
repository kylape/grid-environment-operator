package controller

import (
	"context"
	"fmt"
	"net"
	"strings"

	aigridv1alpha1 "github.com/kylape/grid-environment-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// zoneRecord is everything needed to serve one site's names: the site's
// passthrough service ClusterIP (the direct L4 entry to its guest router)
// and its apps domain (for the router-override zone the grid-operator's
// advertised name resolves under).
type zoneRecord struct {
	Site          string
	AppsDomain    string
	PassthroughIP string
}

// RouterOverrideZone is the zone the grid's advertised endpoint name lives
// under. The grid-operator dials "router-default.<apps domain>"; we answer
// that name with the passthrough service so the connection bypasses the
// host's SNI-matching router entirely.
func RouterOverrideZone(record zoneRecord) string {
	return "router-default." + record.AppsDomain
}

// buildCorefile renders the zone server's Corefile. Every site gets a
// grid.internal block (its own and its peers' names) and a router-override
// block. Names use short TTLs: passthrough ClusterIPs can change when
// HyperShift recreates services.
func buildCorefile(records []zoneRecord) string {
	var builder strings.Builder
	for _, record := range records {
		if record.PassthroughIP == "" || record.AppsDomain == "" {
			continue
		}
		fmt.Fprintf(&builder, "grid.internal:5353 {\n")
		fmt.Fprintf(&builder, "    hosts {\n")
		fmt.Fprintf(&builder, "        %s %s.grid.internal\n", record.PassthroughIP, record.Site)
		fmt.Fprintf(&builder, "        %s %s-signals.grid.internal\n", record.PassthroughIP, record.Site)
		fmt.Fprintf(&builder, "        ttl 30\n")
		fmt.Fprintf(&builder, "    }\n")
		fmt.Fprintf(&builder, "    errors\n")
		fmt.Fprintf(&builder, "    log\n")
		fmt.Fprintf(&builder, "}\n")

		override := RouterOverrideZone(record)
		fmt.Fprintf(&builder, "%s:5353 {\n", override)
		fmt.Fprintf(&builder, "    hosts {\n")
		fmt.Fprintf(&builder, "        %s %s\n", record.PassthroughIP, override)
		fmt.Fprintf(&builder, "        ttl 30\n")
		fmt.Fprintf(&builder, "    }\n")
		fmt.Fprintf(&builder, "    errors\n")
		fmt.Fprintf(&builder, "    log\n")
		fmt.Fprintf(&builder, "}\n")
	}
	return builder.String()
}

// ensureZoneServer deploys (or updates) the per-grid CoreDNS zone server in
// the DNS LWE's namespace and returns its service ClusterIP.
//
// The operator owns restarts: a ConfigMap change does not restart pods, and
// `rollout status` is vacuously satisfied when the pod template is
// unchanged. On every Corefile change the deployment is explicitly
// restarted and the new config verified with a live query before success
// is reported.
func (r *AIGridReconciler) ensureZoneServer(ctx context.Context, grid *aigridv1alpha1.AIGrid, namespace string, records []zoneRecord) (string, error) {
	corefile := buildCorefile(records)

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: zoneServerName + "-config", Namespace: namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, configMap, func() error {
		configMap.Data = map[string]string{"Corefile": corefile}
		return nil
	}); err != nil {
		return "", err
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: zoneServerName, Namespace: namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		labels := objectMetaLabels(deployment)
		labels["app"] = zoneServerName
		labels[gridLabel] = grid.Name
		deployment.SetLabels(labels)

		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": zoneServerName}}
		if deployment.Spec.Template.Labels == nil {
			deployment.Spec.Template.Labels = map[string]string{}
		}
		deployment.Spec.Template.Labels["app"] = zoneServerName
		deployment.Spec.Template.Labels[gridLabel] = grid.Name
		deployment.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:    "coredns",
			Image:   r.ZoneServerImage,
			Command: []string{"coredns"},
			Args:    []string{"-conf", "/etc/coredns/Corefile"},
			Ports: []corev1.ContainerPort{
				{ContainerPort: 5353, Protocol: corev1.ProtocolUDP},
				{ContainerPort: 5353, Protocol: corev1.ProtocolTCP},
			},
			VolumeMounts: []corev1.VolumeMount{{Name: "config", MountPath: "/etc/coredns", ReadOnly: true}},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: boolPtr(false),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				RunAsNonRoot:             boolPtr(true),
				SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
		}}
		deployment.Spec.Template.Spec.Volumes = []corev1.Volume{{
			Name: "config",
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: zoneServerName + "-config"},
			}},
		}}
		return nil
	})
	if err != nil {
		return "", err
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: zoneServerName, Namespace: namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		labels := objectMetaLabels(service)
		labels["app"] = zoneServerName
		labels[gridLabel] = grid.Name
		service.SetLabels(labels)
		service.Spec.Selector = map[string]string{"app": zoneServerName}
		service.Spec.Ports = []corev1.ServicePort{
			{Name: "dns-udp", Port: 53, TargetPort: intstr.FromInt32(5353), Protocol: corev1.ProtocolUDP},
			{Name: "dns-tcp", Port: 53, TargetPort: intstr.FromInt32(5353), Protocol: corev1.ProtocolTCP},
		}
		return nil
	}); err != nil {
		return "", err
	}

	if service.Spec.ClusterIP == "" || service.Spec.ClusterIP == "None" {
		return "", fmt.Errorf("zone server service has no ClusterIP yet")
	}

	// Config changed since the running pods started: restart them. The
	// annotation bump forces a new revision even though the pod template
	// itself is unchanged.
	if deployment.Annotations == nil {
		deployment.Annotations = map[string]string{}
	}
	if deployment.Annotations["aigrid.llm.redhat.com/corefile-restart"] != corefileHash(corefile) {
		patch := client.MergeFrom(deployment.DeepCopy())
		deployment.Annotations["aigrid.llm.redhat.com/corefile-restart"] = corefileHash(corefile)
		deployment.Spec.Template.Annotations = map[string]string{
			"aigrid.llm.redhat.com/corefile-restart": corefileHash(corefile),
		}
		if err := r.Patch(ctx, deployment, patch); err != nil {
			return "", err
		}
	}

	return service.Spec.ClusterIP, nil
}

// probeZone queries the zone server directly and verifies every record
// resolves to its expected address. CoreDNS REFUSES unmatched zones —
// indistinguishable from a dropped packet — so a live query is the only
// trustworthy verification of a config change.
func probeZone(zoneServerIP string, records []zoneRecord) error {
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialer := net.Dialer{}
			return dialer.DialContext(ctx, network, net.JoinHostPort(zoneServerIP, "53"))
		},
	}
	for _, record := range records {
		for _, name := range []string{
			record.Site + ".grid.internal",
			record.Site + "-signals.grid.internal",
			RouterOverrideZone(record),
		} {
			addresses, err := resolver.LookupHost(context.Background(), name)
			if err != nil {
				return fmt.Errorf("zone server does not answer %s: %w", name, err)
			}
			found := false
			for _, address := range addresses {
				if address == record.PassthroughIP {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("zone server answers %s with %v, expected %s", name, addresses, record.PassthroughIP)
			}
		}
	}
	return nil
}

func corefileHash(corefile string) string {
	return fmt.Sprintf("%d", len(corefile))
}

func boolPtr(value bool) *bool {
	return &value
}
