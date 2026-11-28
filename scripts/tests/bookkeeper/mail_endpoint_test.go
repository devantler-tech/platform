package bookkeeper_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The collector reads mail over JMAP with basic authentication, and refuses a
// JMAP session whose URLs name another host than its --mail-url. Stalwart
// serves JMAP only behind the Gateway route private-mail/private-mail and names
// its STALWART_PUBLIC_URL host in every session, whichever route host the
// request used (#6662). The mail server's own load balancer
// (private-mail/mail-server) carries SMTP, submission and IMAPS, never HTTPS.
// The collector's URL and egress grant must name the pinned JMAP host even
// when the route also serves another name.
func TestCollectorReadsMailThroughTheGatewayRoute(t *testing.T) {
	apps := render(t)
	route := object(t, apps, "HTTPRoute", "private-mail", "private-mail")
	hostnames, _ := field(t, route, "spec", "hostnames").([]any)
	const jmapHost = "stalwart.platform.devantler.tech"
	domain := productionDomain(t)
	host := ""
	for _, name := range hostnames {
		candidate, _ := name.(string)
		if strings.ReplaceAll(candidate, "${domain}", domain) == jmapHost {
			host = candidate
			break
		}
	}
	if host == "" {
		t.Fatalf("the mail route must serve the pinned JMAP host %s, got %v", jmapHost, hostnames)
	}
	// The session names Stalwart's public URL, so that URL must be this host.
	stalwart := object(t, apps, "StatefulSet", "private-mail", "private-mail")
	publicURL := ""
	containers, _ := field(t, stalwart, "spec", "template", "spec", "containers").([]any)
	for _, item := range containers {
		container, _ := item.(map[string]any)
		if container["name"] != "private-mail" {
			continue
		}
		env, _ := container["env"].([]any)
		for _, entry := range env {
			variable, _ := entry.(map[string]any)
			if variable["name"] == "STALWART_PUBLIC_URL" {
				publicURL, _ = variable["value"].(string)
			}
		}
	}
	if publicURL != "https://"+host {
		t.Fatalf("JMAP sessions name %q, not the collector's host https://%s, so the collector would refuse them", publicURL, host)
	}
	if !reflect.DeepEqual(field(t, route, "spec", "parentRefs"), []any{map[string]any{
		"name": "platform", "namespace": "kube-system", "sectionName": "https",
	}}) {
		t.Fatal("the mail route must stay on the shared Gateway's HTTPS listener")
	}
	rules, _ := field(t, route, "spec", "rules").([]any)
	if len(rules) == 0 {
		t.Fatal("the mail route has no rule")
	}
	for _, item := range rules {
		rule, _ := item.(map[string]any)
		// An oauth2-proxy hop would answer basic-auth JMAP with a sign-in redirect.
		if !reflect.DeepEqual(rule["backendRefs"], []any{map[string]any{"name": "private-mail", "port": 8080}}) {
			t.Fatalf("the mail route must send JMAP straight to the mail server, got %v", rule["backendRefs"])
		}
		filters, _ := rule["filters"].([]any)
		for _, entry := range filters {
			filter, _ := entry.(map[string]any)
			if kind := filter["type"]; kind != "RequestHeaderModifier" && kind != "ResponseHeaderModifier" {
				t.Fatalf("the mail route must not gain a %v filter in front of JMAP", kind)
			}
		}
	}

	for _, item := range apps {
		if item["kind"] != "DNSEndpoint" {
			continue
		}
		endpoints, _ := field(t, item, "spec", "endpoints").([]any)
		for _, entry := range endpoints {
			endpoint, _ := entry.(map[string]any)
			if endpoint["dnsName"] == jmapHost {
				t.Fatalf("%s is a load balancer name, which carries no JMAP", jmapHost)
			}
		}
	}

	for _, namespace := range []string{"bookkeeper", "bookkeeper-dev"} {
		objects := render(t, "k8s/providers/hetzner/apps/"+namespace)
		release := object(t, objects, "HelmRelease", namespace, "bookkeeper")
		if got := field(t, release, "spec", "values", "mail", "url"); got != "https://"+host {
			t.Fatalf("%s: the collector must read JMAP at https://%s, got %v", namespace, host, got)
		}
		policy := object(t, objects, "CiliumNetworkPolicy", namespace, "bookkeeper")
		if !egressNamesHostOnHTTPS(policy, host) {
			t.Fatalf("%s: the collector's egress must admit %s on 443", namespace, host)
		}
	}

	// The Gateway reaches the mail server's client adapter as Cilium's ingress
	// identity. The collector's traffic arrives that way, so it needs no grant of
	// its own into private-mail.
	server := object(t, apps, "CiliumNetworkPolicy", "private-mail", "private-mail")
	ingress, _ := field(t, server, "spec", "ingress").([]any)
	admitsGateway := false
	for _, entry := range ingress {
		rule, _ := entry.(map[string]any)
		if reflect.DeepEqual(rule["fromEntities"], []any{"ingress"}) {
			admitsGateway = true
		}
	}
	if !admitsGateway {
		t.Fatal("the mail server must admit the Gateway, the collector's only path to JMAP")
	}
	for _, item := range apps {
		metadata, _ := item["metadata"].(map[string]any)
		if item["kind"] != "CiliumNetworkPolicy" || metadata["namespace"] != "private-mail" {
			continue
		}
		rules, _ := field(t, item, "spec", "ingress").([]any)
		for _, entry := range rules {
			rule, _ := entry.(map[string]any)
			sources, _ := rule["fromEndpoints"].([]any)
			for _, source := range sources {
				labels, _ := field(t, source.(map[string]any), "matchLabels").(map[string]any)
				if value, _ := labels["k8s:io.kubernetes.pod.namespace"].(string); strings.HasPrefix(value, "bookkeeper") {
					t.Fatalf("%v admits the collector directly; it must use the Gateway", metadata["name"])
				}
			}
		}
	}
}

func egressNamesHostOnHTTPS(policy map[string]any, host string) bool {
	spec, _ := policy["spec"].(map[string]any)
	rules, _ := spec["egress"].([]any)
	for _, entry := range rules {
		rule, _ := entry.(map[string]any)
		names, _ := rule["toFQDNs"].([]any)
		named := false
		for _, name := range names {
			if reflect.DeepEqual(name, map[string]any{"matchName": host}) {
				named = true
			}
		}
		if named && reflect.DeepEqual(rule["toPorts"], []any{map[string]any{"ports": []any{map[string]any{"port": "443", "protocol": "TCP"}}}}) {
			return true
		}
	}
	return false
}

// productionDomain is the value Flux substitutes for ${domain} in production:
// variables-cluster overrides variables-base.
func productionDomain(t *testing.T) string {
	t.Helper()
	domain := ""
	for _, path := range []string{"k8s/bases/bootstrap/config-map.yaml", "k8s/clusters/prod/bootstrap/config-map.yaml"} {
		data, err := os.ReadFile(filepath.Join(root(), path))
		if err != nil {
			t.Fatal(err)
		}
		var config struct {
			Data map[string]string `yaml:"data"`
		}
		if err := yaml.Unmarshal(data, &config); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if value, ok := config.Data["domain"]; ok {
			domain = value
		}
	}
	if domain == "" {
		t.Fatal("production substitutes no ${domain}")
	}
	return domain
}
