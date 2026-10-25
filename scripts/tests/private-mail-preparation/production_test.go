package preparation_test

import (
	"strings"
	"testing"
)

func TestProductionAdministrationUsesEncryptedSettings(t *testing.T) {
	objects := render(t, "k8s/providers/hetzner/apps")
	sts := object(t, objects, "StatefulSet", "private-mail")
	pod := mapping(t, mapping(t, mapping(t, sts["spec"])["template"])["spec"])
	if len(sequence(t, pod["initContainers"])) != 1 || len(sequence(t, pod["containers"])) != 1 {
		t.Fatal("production must initialize before normal administration serving")
	}
	secret := object(t, objects, "Secret", "private-mail-settings")
	if mapping(t, secret["metadata"])["namespace"] != "private-mail" || secret["data"] != nil {
		t.Fatal("production settings must bind only to the mail workload")
	}
	values := mapping(t, secret["stringData"])
	value, ok := values["settings.json"].(string)
	if len(values) != 1 || !ok || !strings.HasPrefix(value, "ENC[AES256_GCM,") {
		t.Fatal("private production settings must remain encrypted in rendered source")
	}
	age := sequence(t, mapping(t, secret["sops"])["age"])
	if len(age) != 1 || mapping(t, age[0])["recipient"] != "age1rk6fs67kly3h4zux5za429z3grjtvs8vcunav4sa28pxk738najsk8wgs6" {
		t.Fatal("production settings must use the existing production recovery recipient")
	}
	ports := sequence(t, mapping(t, object(t, objects, "Service", "private-mail")["spec"])["ports"])
	if len(ports) != 1 || mapping(t, ports[0])["port"] != 8080 {
		t.Fatal("administration activation must expose only normal management")
	}
	route := object(t, objects, "HTTPRoute", "private-mail")
	annotations := mapping(t, mapping(t, route["metadata"])["annotations"])
	if annotations["gethomepage.dev/name"] != "Stalwart" || annotations["gethomepage.dev/group"] != "Personal" {
		t.Fatal("production must register the Stalwart Homepage entry")
	}
	for _, o := range objects {
		m, _ := o["metadata"].(map[string]any)
		if m["namespace"] == "private-mail" && (o["kind"] == "TCPRoute" || o["kind"] == "TLSRoute" || o["kind"] == "UDPRoute") {
			t.Fatal("administration must not activate public mail transport")
		}
	}
}
