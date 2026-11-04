package trivyruntimedispositions

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorldRenderedClosure(t *testing.T) {
	for name, data := range map[string]string{
		"extra-pvc":      "apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata: {name: extra, namespace: world-at-ruin}\nspec: {}\n",
		"extra-secret":   "apiVersion: v1\nkind: Secret\nmetadata: {name: extra, namespace: world-at-ruin}\n",
		"extra-grant":    "apiVersion: rbac.authorization.k8s.io/v1\nkind: RoleBinding\nmetadata: {name: extra, namespace: world-at-ruin}\nroleRef: {kind: ClusterRole, name: admin}\nsubjects: []\n",
		"outside-grant":  "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata: {name: extra}\nroleRef: {kind: ClusterRole, name: admin}\nsubjects: [{kind: ServiceAccount, name: world-at-ruin, namespace: world-at-ruin}]\n",
		"outside-source": "apiVersion: source.toolkit.fluxcd.io/v1\nkind: OCIRepository\nmetadata: {name: extra, namespace: other}\nspec: {url: 'oci://ghcr.io/devantler-tech/world-at-ruin/zone-manifests'}\n",
		"outside-target": "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata: {name: extra, namespace: other}\nspec: {targetNamespace: world-at-ruin}\n",
		"outside-policy": "apiVersion: kyverno.io/v1\nkind: ClusterPolicy\nmetadata: {name: extra}\nspec: {rules: [{name: extra, mutate: {targets: [{kind: Deployment, namespace: world-at-ruin, name: anything}]}}]}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkRendered("test", []byte(data), map[string]string{}); err == nil {
				t.Fatal("novel effective World input accepted")
			}
		})
	}
	unrelated := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: unrelated, namespace: other}\ndata: {value: changed}\n")
	if err := checkRendered("test", unrelated, map[string]string{}); err != nil {
		t.Fatal(err)
	}
}

// The mandatory native boundary enables this test inside the verified tool helper.
// Ordinary Go unit jobs do not download tools or require ambient kubectl.
func TestNativeWorldProviderOverlayControls(t *testing.T) {
	if os.Getenv("PLATFORM_RUNTIME_NATIVE_CONTROLS") != "1" {
		t.Skip("mandatory native runtime boundary owns renderer controls")
	}
	root := t.TempDir()
	if err := os.CopyFS(filepath.Join(root, "k8s"), os.DirFS(filepath.Join(repository(t), "k8s"))); err != nil {
		t.Fatal(err)
	}
	render := func() []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "kubectl", "kustomize", "k8s/providers/hetzner/apps")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "KUBECONFIG=/dev/null")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("native provider render failed: %v: %s", err, stderr.String())
		}
		return out
	}
	layer := "hetzner/apps"
	baseline := render()
	if err := checkRendered(layer, baseline, effectivePins[layer]); err != nil {
		t.Fatalf("native baseline invalid: %v", err)
	}
	path := "k8s/providers/hetzner/apps/kustomization.yaml"
	base, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatal(err)
	}
	changeConfig := func(resource string, patch bool) {
		t.Helper()
		obj, err := object(base)
		if err != nil {
			t.Fatal(err)
		}
		if resource != "" {
			rows, _ := obj["resources"].([]any)
			obj["resources"] = append(rows, resource)
		}
		if patch {
			rows, _ := obj["patches"].([]any)
			obj["patches"] = append(rows, map[string]any{
				"target": map[string]any{"group": "rbac.authorization.k8s.io", "version": "v1", "kind": "Role", "namespace": "world-at-ruin", "name": "world-at-ruin-zone-trial-operator"},
				"patch":  "- op: replace\n  path: /rules/0/verbs\n  value: [create, get]\n",
			})
		}
		data, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		write(t, root, path, data)
	}
	write(t, root, "k8s/providers/hetzner/apps/unrelated-native.yaml", []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: unrelated-native, namespace: other}\ndata: {value: changed}\n"))
	changeConfig("unrelated-native.yaml", false)
	unrelated := render()
	if bytes.Equal(unrelated, baseline) {
		t.Fatal("unrelated native control changed nothing")
	}
	if err := checkRendered(layer, unrelated, effectivePins[layer]); err != nil {
		t.Fatalf("unrelated source-graph/object change refused: %v", err)
	}
	cases := map[string]string{
		"persistence": "apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata: {name: injected-native, namespace: world-at-ruin}\nspec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 1Gi}}}\n",
		"credential":  "apiVersion: v1\nkind: Secret\nmetadata: {name: injected-native, namespace: world-at-ruin}\ntype: Opaque\nstringData: {credential: synthetic-boundary-only}\n",
		"binding":     "apiVersion: rbac.authorization.k8s.io/v1\nkind: RoleBinding\nmetadata: {name: injected-native, namespace: world-at-ruin}\nroleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: admin}\nsubjects: [{kind: ServiceAccount, name: world-at-ruin, namespace: world-at-ruin}]\n",
		"source":      "apiVersion: source.toolkit.fluxcd.io/v1\nkind: OCIRepository\nmetadata: {name: injected-native, namespace: other}\nspec: {url: 'oci://ghcr.io/devantler-tech/world-at-ruin/zone-manifests', ref: {tag: unreviewed}}\n",
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			write(t, root, "k8s/providers/hetzner/apps/injected-native.yaml", []byte(data))
			changeConfig("injected-native.yaml", false)
			out := render()
			if bytes.Equal(out, baseline) {
				t.Fatal("native injection changed nothing")
			}
			err := checkRendered(layer, out, effectivePins[layer])
			if err == nil || !strings.Contains(err.Error(), "World trial") {
				t.Fatalf("actual native injection did not refuse its premise: %v", err)
			}
		})
	}
	t.Run("overlay-widened-role", func(t *testing.T) {
		changeConfig("", true)
		out := render()
		if bytes.Equal(out, baseline) {
			t.Fatal("native role overlay changed nothing")
		}
		err := checkRendered(layer, out, effectivePins[layer])
		if err == nil || !strings.Contains(err.Error(), "effective runtime premise changed") {
			t.Fatalf("native permission overlay did not refuse its premise: %v", err)
		}
	})
	for name, control := range map[string]struct{ group, kind, name, patch string }{
		"overlay-extra-subject":  {"rbac.authorization.k8s.io", "RoleBinding", "world-at-ruin-zone-trial-operator", "- op: add\n  path: /subjects/-\n  value: {apiGroup: rbac.authorization.k8s.io, kind: User, name: 'oidc:other@example.invalid'}\n"},
		"overlay-changed-digest": {"source.toolkit.fluxcd.io", "OCIRepository", "world-at-ruin", "- op: replace\n  path: /spec/ref/digest\n  value: sha256:0000000000000000000000000000000000000000000000000000000000000000\n"},
	} {
		t.Run(name, func(t *testing.T) {
			obj, err := object(base)
			if err != nil {
				t.Fatal(err)
			}
			rows, _ := obj["patches"].([]any)
			obj["patches"] = append(rows, map[string]any{
				"target": map[string]any{"group": control.group, "version": "v1", "kind": control.kind, "namespace": "world-at-ruin", "name": control.name},
				"patch":  control.patch,
			})
			data, err := json.Marshal(obj)
			if err != nil {
				t.Fatal(err)
			}
			write(t, root, path, data)
			out := render()
			if bytes.Equal(out, baseline) {
				t.Fatal("native overlay changed nothing")
			}
			err = checkRendered(layer, out, effectivePins[layer])
			if err == nil || !strings.Contains(err.Error(), "effective runtime premise changed") {
				t.Fatalf("native identity/source overlay did not refuse its premise: %v", err)
			}
		})
	}
}
