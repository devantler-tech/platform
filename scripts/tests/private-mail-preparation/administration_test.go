package preparation_test

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// Removing the separate initialization process, or mounting its private inputs
// into the serving process, would expose initialization authority to the service.
func TestAdministrationSeparatesInitializationAuthority(t *testing.T) {
	objects := render(t, "k8s/bases/apps/private-mail-administration")
	sts := object(t, objects, "StatefulSet", "private-mail")
	pod := mapping(t, mapping(t, mapping(t, sts["spec"])["template"])["spec"])
	if pod["automountServiceAccountToken"] != false || pod["hostNetwork"] == true || pod["hostPID"] == true {
		t.Fatal("administration must not gain host or Kubernetes API authority")
	}
	inits, containers := sequence(t, pod["initContainers"]), sequence(t, pod["containers"])
	if len(inits) != 1 || len(containers) != 1 {
		t.Fatal("one initializer must complete before the separate server starts")
	}
	init, server := mapping(t, inits[0]), mapping(t, containers[0])
	image, ok := init["image"].(string)
	if init["command"] != nil || !reflect.DeepEqual(init["args"], []any{"--proxy-client-ip=false"}) {
		t.Fatal("initialization must retain the image's fail-closed bootstrap entrypoint")
	}
	if !reflect.DeepEqual(server["command"], []any{"/usr/local/bin/stalwart"}) || !reflect.DeepEqual(server["args"], []any{"--config", "/etc/stalwart/config.json"}) {
		t.Fatal("normal serving must run Stalwart directly after initialization succeeds")
	}
	// The server's one environment variable is its public web address, a
	// non-secret value; the initializer has none.
	publicURL := []any{map[string]any{"name": "STALWART_PUBLIC_URL", "value": "https://stalwart.${domain}"}}
	if init["env"] != nil || !reflect.DeepEqual(server["env"], publicURL) {
		t.Fatal("the workload must not inject recovery authority or private passwords through environment variables")
	}
	for _, c := range []map[string]any{init, server} {
		if c["envFrom"] != nil {
			t.Fatal("the workload must not inject recovery authority or private passwords through environment variables")
		}
		sc := mapping(t, c["securityContext"])
		if sc["runAsNonRoot"] != true || sc["readOnlyRootFilesystem"] != true || sc["allowPrivilegeEscalation"] != false {
			t.Fatal("both processes require restricted execution and read-only root filesystems")
		}
	}
	volumes := map[string]map[string]any{}
	for _, item := range sequence(t, pod["volumes"]) {
		v := mapping(t, item)
		volumes[stringField(t, v, "name")] = v
		if v["hostPath"] != nil {
			t.Fatal("mail must not mount host files")
		}
	}
	initMounts, serverMounts := map[string]bool{}, map[string]bool{}
	for _, item := range sequence(t, init["volumeMounts"]) {
		m := mapping(t, item)
		initMounts[stringField(t, m, "name")] = true
	}
	for _, item := range sequence(t, server["volumeMounts"]) {
		m := mapping(t, item)
		serverMounts[stringField(t, m, "name")] = true
		v := volumes[stringField(t, m, "name")]
		if v["projected"] != nil || m["mountPath"] == "/etc/private-mail" {
			t.Fatal("normal serving must not receive bootstrap passwords or settings")
		}
		if v["secret"] != nil && mapping(t, v["secret"])["secretName"] != "private-mail-tls" {
			t.Fatal("normal serving must not receive account credentials through a direct Secret mount")
		}
	}
	privateInputs := mapping(t, volumes["private-inputs"]["projected"])
	if privateInputs["defaultMode"] != 0440 || !initMounts["private-inputs"] || serverMounts["private-inputs"] {
		t.Fatal("private inputs must be readable only by the initializer's restricted group")
	}
	paths := map[string]string{}
	for _, source := range sequence(t, privateInputs["sources"]) {
		secret := mapping(t, mapping(t, source)["secret"])
		if secret["optional"] == true {
			t.Fatal("missing private inputs must prevent initialization")
		}
		for _, item := range sequence(t, secret["items"]) {
			path := stringField(t, mapping(t, item), "path")
			if paths[path] != "" {
				t.Fatal("private input paths must be unique")
			}
			paths[path] = stringField(t, secret, "name")
		}
	}
	if !reflect.DeepEqual(paths, map[string]string{
		"settings.json": "private-mail-settings", "administrator-password": "private-mail-admin",
		"maintainer-password": "private-mail-maintainer", "agent-password": "private-mail-agent",
		"relay-username": "private-mail-relay", "relay-password": "private-mail-relay",
		"maintainer-aliases": "private-mail-aliases",
	}) {
		t.Fatal("initialization must consume the durable read-back credentials, never a replaceable generator cache")
	}
	for _, name := range []string{"initialization-temporary", "temporary"} {
		if mapping(t, volumes[name]["emptyDir"])["medium"] != "Memory" || initMounts[name] && serverMounts[name] {
			t.Fatal("each process needs its own memory-backed temporary files")
		}
	}
	if !initMounts["initialization-temporary"] || !serverMounts["temporary"] {
		t.Fatal("initialization and serving temporary files must not share a volume")
	}
	if !ok || !regexp.MustCompile(`^ghcr\.io/devantler-tech/platform-private-mail-bootstrap@sha256:[a-f0-9]{64}$`).MatchString(image) || server["image"] != image {
		t.Fatal("both processes must use the same immutable published bootstrap image")
	}
}

// A projected ConfigMap directory cannot provide the missing mount points for
// another volume beneath it. Check the relationship between rendered mounts,
// rather than relying on a particular configuration or certificate filename.
func TestAdministrationConfigMapDoesNotCoverAnotherVolume(t *testing.T) {
	objects := render(t, "k8s/bases/apps/private-mail-administration")
	sts := object(t, objects, "StatefulSet", "private-mail")
	pod := mapping(t, mapping(t, mapping(t, sts["spec"])["template"])["spec"])
	volumes := map[string]map[string]any{}
	for _, item := range sequence(t, pod["volumes"]) {
		volume := mapping(t, item)
		volumes[stringField(t, volume, "name")] = volume
	}
	for _, group := range []string{"initContainers", "containers"} {
		for _, item := range sequence(t, pod[group]) {
			container := mapping(t, item)
			mounts := sequence(t, container["volumeMounts"])
			for _, item := range mounts {
				mount := mapping(t, item)
				if volumes[stringField(t, mount, "name")]["configMap"] == nil {
					continue
				}
				parent := strings.TrimRight(stringField(t, mount, "mountPath"), "/") + "/"
				for _, other := range mounts {
					if strings.HasPrefix(stringField(t, mapping(t, other), "mountPath"), parent) {
						t.Fatalf("container %q: ConfigMap directory covers another volume mount", stringField(t, container, "name"))
					}
				}
			}
		}
	}
}

// Accidentally adding a recovery/mail Service port or an additive ingress grant
// must not expose bootstrap authority or activate public mail during administration.
func TestAdministrationExposesOnlyNormalManagement(t *testing.T) {
	objects := render(t, "k8s/bases/apps/private-mail-administration")
	service := mapping(t, object(t, objects, "Service", "private-mail")["spec"])
	if service["type"] != "ClusterIP" {
		t.Fatal("administration must use the existing HTTPS gateway")
	}
	ports := sequence(t, service["ports"])
	if len(ports) != 1 || mapping(t, ports[0])["port"] != 8080 || mapping(t, ports[0])["targetPort"] != "management" {
		t.Fatal("only normal management may be selected by the Service")
	}
	policy := mapping(t, object(t, objects, "CiliumNetworkPolicy", "private-mail")["spec"])
	ingress := sequence(t, policy["ingress"])
	if len(ingress) != 1 || !reflect.DeepEqual(mapping(t, ingress[0])["fromEntities"], []any{"ingress"}) {
		t.Fatal("only gateway ingress may reach administration")
	}
	allowed := sequence(t, mapping(t, sequence(t, mapping(t, ingress[0])["toPorts"])[0])["ports"])
	if len(allowed) != 1 || mapping(t, allowed[0])["port"] != "8080" {
		t.Fatal("gateway ingress must not allow recovery or mail ports")
	}
	denies := sequence(t, policy["ingressDeny"])
	if len(denies) != 1 || !reflect.DeepEqual(mapping(t, denies[0])["fromEntities"], []any{"all"}) {
		t.Fatal("recovery and mail denial must override additive source grants")
	}
	denied := map[string]bool{}
	for _, rule := range sequence(t, mapping(t, denies[0])["toPorts"]) {
		for _, item := range sequence(t, mapping(t, rule)["ports"]) {
			p := mapping(t, item)
			if p["protocol"] != "TCP" {
				t.Fatal("listener denials must apply to TCP")
			}
			denied[stringField(t, p, "port")] = true
		}
	}
	if !reflect.DeepEqual(denied, map[string]bool{"18080": true, "1025": true, "1587": true, "1465": true, "1993": true}) {
		t.Fatal("recovery and all prepared mail listeners must remain inaccessible")
	}
	// Outbound mail leaves only through the SES relay. The grants must be exactly
	// DNS, inspected so the name rule is enforced, and the relay on 587: a wider
	// rule would let the server deliver directly or reach anything else.
	if _, present := policy["egressDeny"]; present {
		t.Fatal("the preparation egress deny would override the relay grant")
	}
	relay := "email-smtp.eu-central-1.amazonaws.com"
	wantEgress := []any{
		map[string]any{
			"toFQDNs": []any{map[string]any{"matchName": relay}},
			"toPorts": []any{map[string]any{"ports": []any{map[string]any{"port": "587", "protocol": "TCP"}}}},
		},
		map[string]any{
			"toEndpoints": []any{map[string]any{"matchLabels": map[string]any{"k8s:io.kubernetes.pod.namespace": "kube-system", "k8s-app": "kube-dns"}}},
			"toPorts": []any{map[string]any{
				"ports": []any{map[string]any{"port": "53", "protocol": "UDP"}, map[string]any{"port": "53", "protocol": "TCP"}},
				"rules": map[string]any{"dns": []any{map[string]any{"matchName": relay}, map[string]any{"matchPattern": "*.cluster.local"}}},
			}},
		},
	}
	if !reflect.DeepEqual(policy["egress"], wantEgress) {
		t.Fatal("administration egress must be exactly DNS and the SES relay on 587")
	}
	relaySecret := mapping(t, object(t, objects, "ExternalSecret", "private-mail-relay")["spec"])
	var relayKeys []string
	for _, item := range sequence(t, relaySecret["data"]) {
		ref := mapping(t, mapping(t, item)["remoteRef"])
		if ref["key"] != "infrastructure/email/ses-smtp" {
			t.Fatal("the relay login must come from its one OpenBao key")
		}
		relayKeys = append(relayKeys, stringField(t, mapping(t, item), "secretKey")+"="+stringField(t, ref, "property"))
	}
	if !reflect.DeepEqual(relayKeys, []string{"username=username", "password=password"}) || mapping(t, relaySecret["target"])["name"] != "private-mail-relay" {
		t.Fatal("the relay ExternalSecret must project exactly the SES login")
	}
	route := object(t, objects, "HTTPRoute", "private-mail")
	meta := mapping(t, route["metadata"])
	annotations := mapping(t, meta["annotations"])
	if annotations["gethomepage.dev/enabled"] != "true" || annotations["gethomepage.dev/name"] != "Stalwart" || annotations["gethomepage.dev/href"] != "https://stalwart.${domain}/admin" {
		t.Fatal("the serving route must register a usable Stalwart Homepage entry on Stalwart's own hostname")
	}
	// Stalwart's web side moves to stalwart.${domain}; mail.${domain} keeps
	// serving it until Bulwark takes that name (#6662).
	if !reflect.DeepEqual(mapping(t, route["spec"])["hostnames"], []any{"mail.${domain}", "stalwart.${domain}"}) {
		t.Fatal("administration must answer on its current and its new hostname, and its Homepage link must use one of them")
	}
	certificate := mapping(t, object(t, objects, "Certificate", "private-mail")["spec"])
	if certificate["secretName"] != "private-mail-tls" || !reflect.DeepEqual(certificate["dnsNames"], []any{"mail.${domain}"}) {
		t.Fatal("the native server's TLS files must cover its configured hostname")
	}
}

// Bulwark calls Stalwart's JMAP and OAuth endpoints from its own origin
// (#6662). The route answers those calls with CORS headers that name that one
// origin and allow no credentials; Stalwart's own permissive CORS stays off.
// A wildcard origin, an allowed credential or a dropped header would widen or
// break that, so the whole response policy is pinned in the base and in the
// production render, where the route must also stay DNS-only on both names.
func TestAdministrationRouteAllowsOnlyBulwarkOrigin(t *testing.T) {
	want := map[string]any{
		"type": "ResponseHeaderModifier",
		"responseHeaderModifier": map[string]any{
			"set": []any{
				map[string]any{"name": "Strict-Transport-Security", "value": "max-age=63072000; includeSubDomains; preload"},
				map[string]any{"name": "Access-Control-Allow-Origin", "value": "https://mail.${domain}"},
				map[string]any{"name": "Access-Control-Allow-Methods", "value": "GET, POST, PUT, DELETE, OPTIONS"},
				map[string]any{"name": "Access-Control-Allow-Headers", "value": "Authorization, Content-Type, Accept, X-Requested-With"},
				map[string]any{"name": "Access-Control-Max-Age", "value": "600"},
			},
			"add": []any{
				map[string]any{"name": "Vary", "value": "Origin"},
			},
		},
	}
	for _, path := range []string{"k8s/bases/apps/private-mail-administration", "k8s/providers/hetzner/apps"} {
		route := object(t, render(t, path), "HTTPRoute", "private-mail")
		spec := mapping(t, route["spec"])
		if !reflect.DeepEqual(spec["hostnames"], []any{"mail.${domain}", "stalwart.${domain}"}) {
			t.Fatalf("%s: the route must serve Stalwart on both hostnames", path)
		}
		var responses []any
		for _, rule := range sequence(t, spec["rules"]) {
			for _, filter := range sequence(t, mapping(t, rule)["filters"]) {
				if mapping(t, filter)["type"] == "ResponseHeaderModifier" {
					responses = append(responses, filter)
				}
			}
		}
		if len(responses) != 1 || !reflect.DeepEqual(responses[0], want) {
			t.Fatalf("%s: the response policy must be exactly HSTS and CORS for Bulwark's origin, got %v", path, responses)
		}
		if path == "k8s/providers/hetzner/apps" {
			annotations := mapping(t, mapping(t, route["metadata"])["annotations"])
			if annotations["external-dns.kubernetes.io/cloudflare-proxied"] != "false" {
				t.Fatal("both hostnames must be published DNS-only, like the mail host")
			}
		}
	}
}
