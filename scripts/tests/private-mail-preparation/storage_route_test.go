package preparation_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMailRollbackSelectsExactlyOnePersistentStore(t *testing.T) {
	const verified = "private-mail-longhorn-v2 "
	const oldName = ".private-mail-v1-fenced"
	const newName = ".private-mail-v2-copy-complete"
	stamp := verified + strings.Repeat("a", 64) + "\n"
	production := render(t, "k8s/providers/hetzner/apps")
	sts := object(t, production, "StatefulSet", "private-mail")
	pod := mapping(t, mapping(t, mapping(t, sts["spec"])["template"])["spec"])
	initializers := sequence(t, pod["initContainers"])
	selector := mapping(t, initializers[1])
	if selector["name"] != "select-mail-store" {
		t.Fatal("the rollback artifact must select the store before any application writer starts")
	}
	script := sequence(t, selector["args"])[0].(string)
	if strings.Contains(script, "${") {
		t.Fatal("the literal selector must survive strict Flux post-build substitution")
	}
	for _, tt := range []struct {
		name, old, target, selected string
	}{
		{name: "untouched destination keeps V1", selected: "old"},
		{name: "verified target fences V1 before first write", target: stamp, selected: "new"},
		{name: "rollback keeps V2 after it has served", old: stamp, target: stamp, selected: "new"},
		{name: "missing target proof cannot restart V1", old: stamp},
		{name: "damaged target proof cannot restart V1", old: stamp, target: "partial\n"},
		{name: "unrecognized target proof cannot restart V1", target: stamp + "\n"},
		{name: "different fence cannot restart either store", old: verified + strings.Repeat("b", 64) + "\n", target: stamp},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			old, target, routed := filepath.Join(root, "old"), filepath.Join(root, "new"), filepath.Join(root, "route")
			template := filepath.Join(root, "datastore.json")
			for _, path := range []string{filepath.Join(old, "data"), filepath.Join(target, "data"), routed} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			write := func(path, data string) {
				if data != "" {
					if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			write(filepath.Join(old, oldName), tt.old)
			write(filepath.Join(target, newName), tt.target)
			write(template, "{\n  \"path\": \""+old+"/data\",\n  \"@type\": \"RocksDb\"\n}\n")
			command := strings.NewReplacer("/var/lib/stalwart-v2", target, "/var/lib/stalwart", old,
				"/etc/private-mail-datastore/config.json", template, "/store-selection", routed).Replace(script)
			run := func() ([]byte, error) {
				return exec.CommandContext(t.Context(), "/bin/sh", "-c", command).CombinedOutput()
			}
			out, err := run()
			if (err == nil) != (tt.selected != "") {
				t.Fatalf("selection output: %s, err=%v", out, err)
			}
			if tt.selected == "" {
				if _, err = os.Lstat(filepath.Join(routed, "config.json")); !os.IsNotExist(err) {
					t.Fatal("an ambiguous receipt must leave no writable route")
				}
				return
			}
			rendered, err := os.ReadFile(filepath.Join(routed, "config.json"))
			var config map[string]string
			if err != nil || json.Unmarshal(rendered, &config) != nil ||
				config["path"] != filepath.Join(root, tt.selected, "data") || config["@type"] != "RocksDb" {
				t.Fatalf("storage route = %q, err=%v", rendered, err)
			}
			if tt.selected == "new" {
				fence, err := os.ReadFile(filepath.Join(old, oldName))
				if err != nil || string(fence) != stamp {
					t.Fatal("V2 became writable without a durable V1 fence")
				}
			}
			if out, err = run(); err != nil {
				t.Fatalf("restart changed the serving store: %s, err=%v", out, err)
			}
		})
	}
}
