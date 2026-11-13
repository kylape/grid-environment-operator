package controller

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	aigridv1alpha1 "github.com/kylape/grid-environment-operator/api/v1alpha1"
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

	// CoreDNS refuses duplicate zone definitions — with multiple sites,
	// grid.internal must appear as exactly ONE server block. (Found live:
	// per-site blocks crash-looped the zone server.)
	if count := strings.Count(corefile, "grid.internal:5353"); count != 1 {
		t.Errorf("grid.internal zone defined %d times, must be exactly 1:\n%s", count, corefile)
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

func TestIngressLeafSigning(t *testing.T) {
	// Build a CA with an ECDSA key, as the feature expects.
	caKey, err := ecdsaGenerateKey()
	if err != nil {
		t.Fatalf("generating CA key: %v", err)
	}
	caTemplate := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-lab-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, caKey.Public(), caKey)
	if err != nil {
		t.Fatalf("creating CA: %v", err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caKeyDER, _ := x509.MarshalPKCS8PrivateKey(caKey)
	caKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: caKeyDER})

	ca, err := parseIngressCA(caPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("parsing CA: %v", err)
	}

	host := "grid-ci-grid.apps.cluster.example.com"
	certificatePEM, keyPEM, err := ca.signIngressLeaf(host)
	if err != nil {
		t.Fatalf("signing leaf: %v", err)
	}

	// The leaf must cover the host and verify against the CA.
	if !ingressLeafValid(certificatePEM, host) {
		t.Error("freshly signed leaf must be valid for the host")
	}
	leafBlock, _ := pem.Decode(certificatePEM)
	leaf, err := x509.ParseCertificate(leafBlock.Bytes)
	if err != nil {
		t.Fatalf("parsing signed leaf: %v", err)
	}
	caBlock, _ := pem.Decode(caPEM)
	parsedCA, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatalf("parsing CA cert: %v", err)
	}
	if err := leaf.CheckSignatureFrom(parsedCA); err != nil {
		t.Errorf("leaf does not chain to the CA: %v", err)
	}

	// The leaf's key must be usable.
	keyBlock, _ := pem.Decode(keyPEM)
	leafKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatalf("parsing leaf key: %v", err)
	}
	if err := ecdsaChecker(leaf, leafKey); err != nil {
		t.Errorf("leaf key does not match certificate: %v", err)
	}

	// Validity checks: wrong host and garbage are rejected.
	if ingressLeafValid(certificatePEM, "other.example.com") {
		t.Error("leaf must not validate for a different host")
	}
	if ingressLeafValid([]byte("not pem"), host) {
		t.Error("garbage must not validate")
	}
}

func ecdsaChecker(certificate *x509.Certificate, key interface{}) error {
	ecdsaKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return fmt.Errorf("leaf key is not ECDSA")
	}
	if certificate.PublicKey.(*ecdsa.PublicKey).Equal(&ecdsaKey.PublicKey) {
		return nil
	}
	return fmt.Errorf("public keys differ")
}

func TestMissingPullSecretKeys(t *testing.T) {
	// The validated composition: every required key present.
	complete := &dockerConfig{Auths: map[string]struct {
		Auth string `json:"auth"`
	}{
		"cloud.openshift.com":         {},
		"quay.io":                     {},
		"quay.io/rhoai":               {},
		"registry.redhat.io":          {},
		"registry.connect.redhat.com": {},
		"registry.stage.redhat.io":    {},
	}}
	if missing := missingPullSecretKeys(complete, requiredPullSecretKeys); len(missing) != 0 {
		t.Errorf("complete composition reported missing keys: %v", missing)
	}

	// The first corruption incident: path-scoped key dropped, everything
	// else intact. This must fail — no single token covers both quay
	// repositories.
	dropped := &dockerConfig{Auths: map[string]struct {
		Auth string `json:"auth"`
	}{
		"cloud.openshift.com": {},
		"quay.io":             {},
		"registry.redhat.io":  {},
	}}
	missing := missingPullSecretKeys(dropped, requiredPullSecretKeys)
	if len(missing) != 1 || missing[0] != "quay.io/rhoai" {
		t.Errorf("expected [quay.io/rhoai] missing, got %v", missing)
	}

	// Optional keys absent: not an error.
	if missing := missingPullSecretKeys(dropped, optionalPullSecretKeys); len(missing) == 0 {
		t.Error("sanity: optional keys should be reported missing when absent")
	}
}

func TestParseDockerConfigRejectsGarbage(t *testing.T) {
	if _, err := parseDockerConfig([]byte("not json")); err == nil {
		t.Error("garbage should fail to parse")
	}
	if _, err := parseDockerConfig([]byte(`{}`)); err == nil {
		t.Error("config without auths should fail")
	}
	valid := `{"auths":{"quay.io":{"auth":"dXNlcjpwYXNz"}}}`
	config, err := parseDockerConfig([]byte(valid))
	if err != nil {
		t.Fatalf("valid config failed to parse: %v", err)
	}
	if _, ok := config.Auths["quay.io"]; !ok {
		t.Error("quay.io key not parsed")
	}
}

func lweWithReadyCondition(status, reason, message string) *unstructured.Unstructured {
	lwe := object(lweGVK, "test", "test-lwe")
	lwe.Object["status"] = map[string]interface{}{
		"conditions": []interface{}{
			map[string]interface{}{
				"type": "Ready", "status": status, "reason": reason, "message": message,
			},
		},
	}
	return lwe
}

func TestLWEReadyCondition(t *testing.T) {
	if _, _, ready := lweReadyCondition(lweWithReadyCondition("True", "Ready", "")); !ready {
		t.Error("Ready=True should report ready")
	}

	reason, message, ready := lweReadyCondition(lweWithReadyCondition("False", "NamespaceCollision", "env-x exists"))
	if ready {
		t.Error("Ready=False should report not ready")
	}
	if reason != "NamespaceCollision" || message != "env-x exists" {
		t.Errorf("reason/message not surfaced: %q / %q", reason, message)
	}

	reason, _, ready = lweReadyCondition(object(lweGVK, "test", "fresh"))
	if ready {
		t.Error("no status should report not ready")
	}
	if reason != "NoReadyCondition" {
		t.Errorf("expected NoReadyCondition, got %q", reason)
	}
}

func TestApplyClusterQueue(t *testing.T) {
	grid := &aigridv1alpha1.AIGrid{Spec: aigridv1alpha1.AIGridSpec{ClusterQueue: "ci-grids"}}

	// Fresh LWE: the queue is set.
	fresh := object(lweGVK, "test", "fresh")
	if err := applyClusterQueue(fresh, grid); err != nil {
		t.Fatalf("fresh apply failed: %v", err)
	}
	if queue, _, _ := unstructured.NestedString(fresh.Object, "spec", "clusterQueue"); queue != "ci-grids" {
		t.Errorf("queue not set: %q", queue)
	}

	// Existing LWE with the same queue: no error, no change.
	if err := applyClusterQueue(fresh, grid); err != nil {
		t.Fatalf("matching apply failed: %v", err)
	}

	// Existing LWE with a different queue: immutability error.
	fresh.Object["spec"].(map[string]interface{})["clusterQueue"] = "other-queue"
	if err := applyClusterQueue(fresh, grid); err == nil {
		t.Error("queue change must be refused")
	}

	// No queue desired: never set, never an error.
	unset := object(lweGVK, "test", "unset")
	if err := applyClusterQueue(unset, &aigridv1alpha1.AIGrid{}); err != nil {
		t.Fatalf("empty desired failed: %v", err)
	}
	if _, found, _ := unstructured.NestedString(unset.Object, "spec", "clusterQueue"); found {
		t.Error("empty desired must not set a queue")
	}
}
