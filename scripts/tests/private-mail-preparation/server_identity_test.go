package preparation_test

import (
	"strings"
	"testing"
)

// Production names the mail server mail.devantler.tech, the host its DNS
// record publishes and its certificate covers, and gives it a separate web
// host. The setup's web host and the serving process's public URL must name the
// same host, or the documents that set up mail apps and the JMAP session would
// point at different places.
func TestProductionNamesTheMailServer(t *testing.T) {
	objects := render(t, "k8s/providers/hetzner/apps")
	pod := mapping(t, mapping(t, mapping(t, object(t, objects, "StatefulSet", "private-mail")["spec"])["template"])["spec"])
	var configure, server map[string]any
	for _, item := range sequence(t, pod["initContainers"]) {
		if c := mapping(t, item); c["name"] == "configure" {
			configure = c
		}
	}
	for _, item := range sequence(t, pod["containers"]) {
		if c := mapping(t, item); c["name"] == "private-mail" {
			server = c
		}
	}
	if configure == nil || server == nil {
		t.Fatal("missing the setup or the serving container")
	}
	flags := map[string][]string{}
	for _, item := range sequence(t, configure["args"]) {
		arg, ok := item.(string)
		name, value, found := strings.Cut(arg, "=")
		if !ok || !found {
			continue
		}
		flags[name] = append(flags[name], value)
	}
	if len(flags["--hostname"]) != 1 || len(flags["--web-hostname"]) != 1 {
		t.Fatalf("the setup must name the server exactly once each, got %v", flags)
	}
	hostname, webHostname := flags["--hostname"][0], flags["--web-hostname"][0]
	if hostname != "mail.devantler.tech" {
		t.Fatalf("the mail server must be named mail.devantler.tech, got %s", hostname)
	}

	published := false
	dns := mapping(t, object(t, objects, "DNSEndpoint", "mail-server")["spec"])
	for _, item := range sequence(t, dns["endpoints"]) {
		published = published || mapping(t, item)["dnsName"] == hostname
	}
	certified := false
	for _, name := range sequence(t, mapping(t, object(t, objects, "Certificate", "private-mail")["spec"])["dnsNames"]) {
		certified = certified || name == hostname
	}
	if !published || !certified {
		t.Fatalf("%s must be the published mail host and in the certificate (published=%v certified=%v)", hostname, published, certified)
	}

	if configure["env"] != nil {
		t.Fatal("the setup takes its names as flags, never from its environment")
	}
	env := sequence(t, server["env"])
	if len(env) != 1 || mapping(t, env[0])["name"] != "STALWART_PUBLIC_URL" {
		t.Fatalf("the serving process must carry only its public URL, got %v", env)
	}
	// Both carry the Flux ${domain} placeholder, so compare the exact text.
	if raw := stringField(t, mapping(t, env[0]), "value"); raw != "https://"+webHostname {
		t.Fatalf("the public URL %q must be https on the setup's web host %s, with no path", raw, webHostname)
	}
	if webHostname == hostname {
		t.Fatal("the web host must differ from the mail host, which serves no HTTPS")
	}
}
