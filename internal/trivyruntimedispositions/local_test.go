package trivyruntimedispositions

import (
	"os"
	"path/filepath"
	"testing"
)

func localFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, "app/kustomization.yaml", []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [../base]\ncomponents: [../component]\npatches: [{path: ../patch.yaml}]\nconfigMapGenerator: [{name: settings, files: [settings=../settings.txt]}]\n"))
	write(t, root, "base/kustomization.yaml", []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [object.yaml]\n"))
	write(t, root, "base/object.yaml", []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: application}\n"))
	write(t, root, "component/kustomization.yaml", []byte("apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\nresources: []\n"))
	write(t, root, "patch.yaml", []byte("- op: add\n  path: /metadata/labels/anything\n  value: change\n"))
	write(t, root, "settings.txt", []byte("local input\n"))
	if err := localKustomize(root, "app", map[string]bool{}); err != nil {
		t.Fatalf("valid local closure rejected: %v", err)
	}
	return root
}
func TestNativeRendererInputClosureRemainsLocal(t *testing.T) {
	for name, change := range map[string][2]string{
		"remote-resource":    {"../base", "https://example.invalid/base"},
		"git-resource":       {"../base", "git@example.invalid:base"},
		"host-resource":      {"../base", "github.com/example/repo/base"},
		"escape-resource":    {"../base", "../../../outside"},
		"remote-patch":       {"../patch.yaml", "https://example.invalid/patch.yaml"},
		"remote-generator":   {"settings=../settings.txt", "settings=https://example.invalid/settings.txt"},
		"absolute-generator": {"settings=../settings.txt", "settings=/etc/passwd"},
	} {
		t.Run(name, func(t *testing.T) {
			root := localFixture(t)
			replace(t, root, "app/kustomization.yaml", change[0], change[1])
			if err := localKustomize(root, "app", map[string]bool{}); err == nil {
				t.Fatal("nonlocal renderer input accepted")
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		root := localFixture(t)
		if err := os.Remove(filepath.Join(root, "settings.txt")); err != nil {
			t.Fatal(err)
		}
		external := filepath.Join(t.TempDir(), "settings.txt")
		if err := os.WriteFile(external, []byte("external\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(root, "settings.txt")); err != nil {
			t.Fatal(err)
		}
		if err := localKustomize(root, "app", map[string]bool{}); err == nil {
			t.Fatal("linked renderer input accepted")
		}
	})
}
