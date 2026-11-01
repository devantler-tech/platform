package readonly_test

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type object = map[string]any

func root(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func field(o any, keys ...string) any {
	for _, key := range keys {
		m, _ := o.(object)
		o = m[key]
	}
	return o
}

func list(o any, keys ...string) []any { values, _ := field(o, keys...).([]any); return values }

func render(t *testing.T, provider string) []object {
	t.Helper()
	cmd := exec.Command("kubectl", "kustomize", filepath.Join(root(t), "k8s/providers", provider, "infrastructure/controllers"))
	b, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(b))
	var result []object
	for {
		var o object
		err = decoder.Decode(&o)
		if err == io.EOF {
			return result
		}
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, o)
	}
}

func resource(t *testing.T, items []object, kind, namespace, name string) object {
	t.Helper()
	for _, o := range items {
		if o["kind"] == kind && field(o, "metadata", "namespace") == namespace && field(o, "metadata", "name") == name {
			return o
		}
	}
	t.Fatalf("missing rendered %s %s/%s", kind, namespace, name)
	return nil
}

func authConfig(t *testing.T, items []object) object {
	t.Helper()
	o := resource(t, items, "ConfigMap", "oauth2-proxy", "auth-proxy-config")
	var config object
	if err := yaml.Unmarshal([]byte(field(o, "data", "dynamic.yaml").(string)), &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestLonghornRouteRequiresOIDCAndStaysProductionOnly(t *testing.T) {
	items := render(t, "hetzner")
	route := resource(t, items, "HTTPRoute", "longhorn-system", "longhorn")
	rules := field(route, "spec", "rules").([]any)
	if len(rules) != 1 {
		t.Fatal("Longhorn must have one authenticated route")
	}
	backends := field(rules[0], "backendRefs").([]any)
	if len(backends) != 1 || field(backends[0], "name") != "oauth2-proxy" || field(backends[0], "namespace") != "oauth2-proxy" || field(backends[0], "port") != 80 {
		t.Fatal("Longhorn bypasses OIDC")
	}
	if fmt.Sprint(field(route, "spec", "hostnames")) != "[longhorn.${domain}]" || field(route, "metadata", "annotations", "gethomepage.dev/enabled") != "true" {
		t.Fatal("Longhorn needs its own discoverable host")
	}
	parents := field(route, "spec", "parentRefs").([]any)
	if len(parents) != 1 || field(parents[0], "sectionName") != "https" {
		t.Fatal("Longhorn must use HTTPS")
	}
	grant := resource(t, items, "ReferenceGrant", "oauth2-proxy", "allow-oauth2-proxy-backends")
	allowed := false
	for _, from := range field(grant, "spec", "from").([]any) {
		allowed = allowed || field(from, "namespace") == "longhorn-system" && field(from, "kind") == "HTTPRoute"
	}
	if !allowed {
		t.Fatal("missing cross-namespace auth backend grant")
	}
	config := authConfig(t, items)
	if field(config, "http", "routers", "longhorn", "service") != "longhorn" || field(config, "http", "services", "longhorn", "loadBalancer", "servers").([]any)[0].(object)["url"] != "http://longhorn-frontend.longhorn-system.svc.cluster.local:80" {
		t.Fatal("wrong authenticated Longhorn upstream")
	}
	for _, o := range render(t, "docker") {
		if o["kind"] == "HTTPRoute" && field(o, "metadata", "namespace") == "longhorn-system" {
			t.Fatal("production Longhorn route leaked into local")
		}
	}
}

var requests = []struct {
	method, path string
	allowed      bool
}{
	{"GET", "/", true}, {"HEAD", "/index.html", true}, {"GET", "/v1/volumes", true}, {"GET", "/v1/nodes", true}, {"GET", "/v1/ws/volumes", true},
	{"GET", "/index.abc123.js", true}, {"GET", "/static/logo.abc123.svg", true}, {"GET", "/favicon.ico", true},
	// Actual v1.13.0 asset URLs observed in the deployed HTML shell.
	{"GET", "/runtime~main.6d7bda24.js?697c2a38c2a3b317069c", true},
	{"GET", "/styles.beeabb27.async.js?697c2a38c2a3b317069c", true},
	{"GET", "/main.5284f33a.async.js?697c2a38c2a3b317069c", true},
	{"GET", "/styles.css?697c2a38c2a3b317069c", true},
	{"POST", "/runtime~main.6d7bda24.js", false}, {"GET", "/v1/runtime~main.js", false},
	{"GET", "/v1", true}, {"GET", "/v1/schemas", true}, {"GET", "/v1/settings?limit=100", true},
	{"GET", "/v1/ws/1s/volumes", true}, {"GET", "/v1/ws/5s/snapshotgroups", true},
	{"GET", "/v1/supportbundles/node/bundle/download", false}, {"HEAD", "/v1/supportbundles/node/bundle/download", false},
	{"GET", "/v1/backingimages/fixture/download", false}, {"GET", "/v1/future-mutating-get", false},
	{"GET", "/static/../v1/supportbundles/node/bundle/download", false},
	{"GET", "/static/%2e%2e/v1/supportbundles/node/bundle/download", false},
	{"POST", "/v1/volumes/%2e%2e%2fsupportbundles?action=snapshotList", false},
	{"POST", "/v1/volumes/%252e%252e%252fsupportbundles?action=snapshotList", false},
	{"POST", "/v1/volumes/fixture?action=snapshotList", true}, {"POST", "/v1/volumes/fixture?action=snapshotGet", true},
	{"POST", "/v1/volumes/fixture?action=snapshotCRList", true}, {"POST", "/v1/volumes/fixture?action=recurringJobList", true},
	{"POST", "/v1/backupvolumes/fixture?action=backupList", true}, {"POST", "/v1/backupvolumes/fixture?action=backupGet", true},
	{"POST", "/v1/volumes", false}, {"DELETE", "/v1/volumes/fixture", false}, {"PUT", "/v1/settings/v2-data-engine", false},
	{"POST", "/v1/volumes/fixture?action=attach", false}, {"POST", "/v1/volumes/fixture?action=detach", false},
	{"POST", "/v1/volumes/fixture?action=snapshotDelete", false}, {"POST", "/v1/volumes/fixture?action=snapshotRevert", false},
	{"POST", "/v1/nodes/fixture?action=diskUpdate", false}, {"POST", "/v1/backupvolumes/fixture?action=backupDelete", false},
	{"POST", "/v1/volumes/fixture?action=attach&action=snapshotList", false}, {"PATCH", "/v1/nodes/fixture", false}, {"OPTIONS", "/v1/volumes", false},
}

func TestLonghornNetworkBoundaryAllowsReadsAndRefusesWrites(t *testing.T) {
	items := render(t, "hetzner")
	for _, boundary := range []struct{ ns, name, direction, peers string }{
		{"oauth2-proxy", "allow-auth-proxy", "egress", "toEndpoints"}, {"longhorn-system", "allow-longhorn", "ingress", "fromEndpoints"},
	} {
		policy := resource(t, items, "CiliumNetworkPolicy", boundary.ns, boundary.name)
		var httpRules []any
		for _, rule := range list(policy, "spec", boundary.direction) {
			for _, peer := range list(rule, boundary.peers) {
				ns := field(peer, "matchLabels", "k8s:io.kubernetes.pod.namespace")
				wantedNS, wantedApp := "longhorn-system", "longhorn-ui"
				if boundary.direction == "ingress" {
					wantedNS, wantedApp = "oauth2-proxy", "auth-proxy"
				}
				if ns != wantedNS {
					continue
				}
				if field(peer, "matchLabels", "app") != wantedApp {
					t.Fatal("Longhorn read access is not bound to the exact proxy/UI workload")
				}
				for _, port := range field(rule, "toPorts").([]any) {
					if fmt.Sprint(field(port, "ports")) != "[map[port:8000 protocol:TCP]]" {
						t.Fatal("Longhorn UI grant must reach only TCP 8000")
					}
					rules, ok := field(port, "rules", "http").([]any)
					if !ok || len(rules) == 0 {
						t.Fatal("Longhorn UI has an unrestricted HTTP grant")
					}
					httpRules = append(httpRules, rules...)
				}
			}
		}
		if len(httpRules) == 0 {
			t.Fatalf("%s has no Longhorn read-only rules", boundary.name)
		}
		// Cilium's pinned Envoy rejects RE2 programs above 100 instructions.
		// Go's instruction count is an estimate, so retain a margin and still
		// require the live policy and browser acceptance after deployment.
		for _, rule := range httpRules {
			path, _ := field(rule, "path").(string)
			expression, err := syntax.Parse(path, syntax.Perl)
			if err != nil {
				t.Fatal(err)
			}
			program, err := syntax.Compile(expression.Simplify())
			if err != nil {
				t.Fatal(err)
			}
			if len(program.Inst) > 80 {
				t.Fatalf("%s has an oversized HTTP read rule (Go estimate %d); split it before native evaluation", boundary.name, len(program.Inst))
			}
		}
		for _, req := range requests {
			allowed := false
			for _, rule := range httpRules {
				method, _ := field(rule, "method").(string)
				path, _ := field(rule, "path").(string)
				allowed = allowed || regexp.MustCompile(method).MatchString(req.method) && (path == "" || regexp.MustCompile(path).MatchString(req.path))
			}
			if allowed != req.allowed {
				t.Errorf("%s: %s %s allowed=%t want=%t", boundary.name, req.method, req.path, allowed, req.allowed)
			}
		}
	}
}

func TestLonghornRouterUsesExplicitReadActionPredicates(t *testing.T) {
	config := authConfig(t, render(t, "hetzner"))
	rule, _ := field(config, "http", "routers", "longhorn", "rule").(string)
	if !strings.Contains(rule, "Method(`GET`)") || !strings.Contains(rule, "Method(`POST`)") || !strings.Contains(rule, "QueryRegexp(`action`") {
		t.Fatal("the authenticated router lacks its explicit read-only action boundary")
	}
}
