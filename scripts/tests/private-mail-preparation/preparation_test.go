package preparation_test

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"
)

func root() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
}

func render(t *testing.T, path string) []map[string]any {
	t.Helper()
	out, err := exec.Command("kubectl", "kustomize", filepath.Join(root(), path)).CombinedOutput()
	if err != nil {
		t.Fatalf("mail deployment must render: %v: %s", err, out)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(out))
	var result []map[string]any
	for {
		var object map[string]any
		if err := decoder.Decode(&object); errors.Is(err, io.EOF) {
			return result
		} else if err != nil {
			t.Fatal(err)
		}
		if object != nil {
			result = append(result, object)
		}
	}
}

func mapping(t *testing.T, value any) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected a mapping, got %T", value)
	}
	return result
}

func sequence(t *testing.T, value any) []any {
	t.Helper()
	result, ok := value.([]any)
	if !ok || len(result) == 0 {
		t.Fatalf("expected a nonempty sequence, got %T", value)
	}
	return result
}

func object(t *testing.T, objects []map[string]any, kind, name string) map[string]any {
	t.Helper()
	for _, o := range objects {
		metadata, _ := o["metadata"].(map[string]any)
		if o["kind"] == kind && metadata["name"] == name {
			return o
		}
	}
	t.Fatalf("missing %s %s", kind, name)
	return nil
}

func TestPreparationRendersWithoutPublicMailOrAPIAuthority(t *testing.T) {
	objects := render(t, "k8s/bases/apps/private-mail")
	sts := object(t, objects, "StatefulSet", "private-mail")
	spec := mapping(t, mapping(t, mapping(t, sts["spec"])["template"])["spec"])
	if spec["automountServiceAccountToken"] != false {
		t.Fatal("mail server must not receive Kubernetes API credentials")
	}
	if spec["hostNetwork"] == true || spec["hostPID"] == true {
		t.Fatal("mail preparation must remain isolated from the host")
	}
	container := mapping(t, sequence(t, spec["containers"])[0])
	context := mapping(t, container["securityContext"])
	if context["runAsNonRoot"] != true || context["readOnlyRootFilesystem"] != true || context["allowPrivilegeEscalation"] != false {
		t.Fatal("mail server must run restricted and without writable root or privilege escalation")
	}
	env := sequence(t, container["env"])
	recovery, suppliedCredential := false, false
	for _, item := range env {
		e := mapping(t, item)
		if e["name"] == "STALWART_RECOVERY_MODE" {
			recovery = e["value"] == "1"
		}
		if e["name"] == "STALWART_RECOVERY_ADMIN" {
			_, suppliedCredential = e["valueFrom"].(map[string]any)
			if e["value"] != nil {
				t.Fatal("administrator credential must never be committed")
			}
		}
	}
	if !recovery || !suppliedCredential {
		t.Fatal("preparation must use management-only recovery with a supplied credential, never a generated log password")
	}
	for _, o := range objects {
		s, _ := o["spec"].(map[string]any)
		switch o["kind"] {
		case "Ingress", "HTTPRoute", "TCPRoute", "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding":
			t.Fatalf("preparation must not expose mail or grant API authority: %s", o["kind"])
		case "Service":
			if s["type"] != nil && s["type"] != "ClusterIP" {
				t.Fatal("preparation must not expose a public service")
			}
			for _, port := range sequence(t, s["ports"]) {
				if mapping(t, port)["port"] != 8080 {
					t.Fatal("only the internal management port may be exposed")
				}
			}
		}
	}
	object(t, objects, "PodDisruptionBudget", "private-mail")
	object(t, objects, "PersistentVolumeClaim", "private-mail-data")
	object(t, objects, "CiliumNetworkPolicy", "private-mail")
	object(t, objects, "ExternalSecret", "private-mail-admin")
}

func TestPreparationIsAbsentFromProductionAndLocal(t *testing.T) {
	for _, path := range []string{"k8s/providers/hetzner/apps", "k8s/providers/docker/apps"} {
		for _, o := range render(t, path) {
			m, _ := o["metadata"].(map[string]any)
			if m["namespace"] == "private-mail" || (o["kind"] == "Namespace" && m["name"] == "private-mail") {
				t.Fatalf("private mail must be absent by default from %s", path)
			}
		}
	}
}
