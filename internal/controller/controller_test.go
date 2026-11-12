package controller

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestBuildCorefileServesEverySite(t *testing.T) {
	records := []zoneRecord{
		{Site: "grid-hub", AppsDomain: "apps.example-a.com", PassthroughIP: "172.30.1.1"},
		{Site: "grid-inference", AppsDomain: "apps.example-b.com", PassthroughIP: "172.30.2.2"},
	}

	corefile := buildCorefile(records)

	for _, expected := range []string{
		"grid.internal:5353",
		"172.30.1.1 grid-hub.grid.internal",
		"172.30.2.2 grid-inference.grid.internal",
		"172.30.2.2 grid-inference-signals.grid.internal",
		"router-default.apps.example-b.com:5353",
		"172.30.2.2 router-default.apps.example-b.com",
	} {
		if !strings.Contains(corefile, expected) {
			t.Errorf("Corefile missing %q:\n%s", expected, corefile)
		}
	}
}

func TestBuildCorefileSkipsIncompleteRecords(t *testing.T) {
	records := []zoneRecord{
		{Site: "incomplete"}, // no IP, no domain
		{Site: "complete", AppsDomain: "apps.example.com", PassthroughIP: "172.30.1.1"},
	}

	corefile := buildCorefile(records)

	if strings.Contains(corefile, "incomplete") {
		t.Errorf("Corefile should skip incomplete records:\n%s", corefile)
	}
	if !strings.Contains(corefile, "complete.grid.internal") {
		t.Errorf("Corefile missing complete record:\n%s", corefile)
	}
}

func TestMergeDNSServersPreservesForeignEntries(t *testing.T) {
	existing := []interface{}{
		map[string]interface{}{
			"name":  "lighthouse",
			"zones": []interface{}{"clusterset.local"},
			"forwardPlugin": map[string]interface{}{
				"policy":    "Random",
				"upstreams": []interface{}{"172.29.53.216"},
			},
		},
	}

	merged := mergeDNSServers(existing, "172.30.9.9", []string{"grid.internal"})

	if len(merged) != 2 {
		t.Fatalf("expected 2 servers after merge, got %d: %#v", len(merged), merged)
	}
	lighthouse, ok := merged[0].(map[string]interface{})
	if !ok || lighthouse["name"] != "lighthouse" {
		t.Errorf("lighthouse entry not preserved in first position: %#v", merged[0])
	}
	ours, ok := merged[1].(map[string]interface{})
	if !ok {
		t.Fatalf("managed entry is not a map: %#v", merged[1])
	}
	if ours["name"] != dnsServerEntryPrefix+"grid.internal" {
		t.Errorf("managed entry misnamed: %#v", ours)
	}
	upstreams, found, _ := unstructured.NestedSlice(ours, "forwardPlugin", "upstreams")
	if !found || len(upstreams) != 1 || upstreams[0] != "172.30.9.9" {
		t.Errorf("managed entry upstreams wrong: %#v", ours)
	}
}

func TestMergeDNSServersReplacesAndRemovesManagedEntries(t *testing.T) {
	existing := []interface{}{
		map[string]interface{}{
			"name":          dnsServerEntryPrefix + "grid.internal",
			"zones":         []interface{}{"grid.internal"},
			"forwardPlugin": map[string]interface{}{"upstreams": []interface{}{"172.30.0.1"}},
		},
		map[string]interface{}{
			"name":          dnsServerEntryPrefix + "router-default.apps.stale.example.com",
			"zones":         []interface{}{"router-default.apps.stale.example.com"},
			"forwardPlugin": map[string]interface{}{"upstreams": []interface{}{"172.30.0.1"}},
		},
		map[string]interface{}{
			"name":  "lighthouse",
			"zones": []interface{}{"clusterset.local"},
		},
	}

	merged := mergeDNSServers(existing, "172.30.9.9", []string{"grid.internal"})

	names := serverEntryNames(merged)
	if len(names) != 2 || names[0] != "lighthouse" || names[1] != dnsServerEntryPrefix+"grid.internal" {
		t.Errorf("expected [lighthouse, aigrid-grid.internal], got %v", names)
	}

	// The stale managed entry must be gone, and the surviving managed
	// entry must point at the current zone server.
	for _, entry := range merged {
		if m, ok := entry.(map[string]interface{}); ok && m["name"] == dnsServerEntryPrefix+"grid.internal" {
			upstreams, _, _ := unstructured.NestedSlice(m, "forwardPlugin", "upstreams")
			if len(upstreams) != 1 || upstreams[0] != "172.30.9.9" {
				t.Errorf("managed entry not refreshed: %#v", m)
			}
		}
	}
}

func serverEntryNames(entries []interface{}) []string {
	var names []string
	for _, entry := range entries {
		if m, ok := entry.(map[string]interface{}); ok {
			names = append(names, m["name"].(string))
		}
	}
	return names
}

func TestPoliciesCoverPostDNATPorts(t *testing.T) {
	dns := dnsEgressPolicy("grid-dns-ns", "172.30.9.9")

	// The podSelector rule must carry the pod port (5353); the ipBlock
	// rule must carry the service port (53). Both must be UDP and TCP.
	podRulePorts := dns.Spec.Egress[0].Ports
	if len(podRulePorts) != 2 {
		t.Fatalf("expected 2 ports on podSelector rule, got %d", len(podRulePorts))
	}
	for _, port := range podRulePorts {
		if port.Port.IntValue() != 5353 {
			t.Errorf("podSelector rule port = %d, want 5353 (post-DNAT pod port)", port.Port.IntValue())
		}
	}
	ipRulePorts := dns.Spec.Egress[1].Ports
	for _, port := range ipRulePorts {
		if port.Port.IntValue() != 53 {
			t.Errorf("ipBlock rule port = %d, want 53 (pre-DNAT service port)", port.Port.IntValue())
		}
	}

	passthrough := passthroughEgressPolicy([]string{"env-a-cluster", "env-b-cluster"}, []string{"172.30.1.1"})
	// The virt-launcher peer rule must be port-unrestricted.
	if passthrough.Spec.Egress[0].Ports != nil {
		t.Errorf("passthrough podSelector rule must not restrict ports (post-DNAT ports vary)")
	}
	// The ipBlock rules must carry :443 TCP.
	for _, rule := range passthrough.Spec.Egress[1:] {
		if len(rule.Ports) != 1 || rule.Ports[0].Port.IntValue() != 443 {
			t.Errorf("passthrough ipBlock rule ports wrong: %#v", rule.Ports)
		}
	}
}

func TestInterSiteUDPPolicyPorts(t *testing.T) {
	policy := interSiteUDPPolicy([]string{"env-a-cluster"})
	if len(policy.Spec.Egress) != 1 {
		t.Fatalf("expected 1 egress rule, got %d", len(policy.Spec.Egress))
	}
	ports := policy.Spec.Egress[0].Ports
	if len(ports) != 3 {
		t.Fatalf("expected 3 ports (500/4500/4490), got %d", len(ports))
	}
	for i, want := range []int{500, 4500, 4490} {
		if ports[i].Port.IntValue() != want {
			t.Errorf("port[%d] = %d, want %d", i, ports[i].Port.IntValue(), want)
		}
		if *ports[i].Protocol != "UDP" {
			t.Errorf("port[%d] protocol = %s, want UDP", i, *ports[i].Protocol)
		}
	}
}

func TestRouterOverrideZone(t *testing.T) {
	record := zoneRecord{Site: "s", AppsDomain: "apps.example.com"}
	if got := RouterOverrideZone(record); got != "router-default.apps.example.com" {
		t.Errorf("RouterOverrideZone = %q", got)
	}
}
