package preparation_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMailServesOnlyTheVerifiedV2Store(t *testing.T) {
	stamp := "private-mail-longhorn-v2 " + strings.Repeat("a", 64) + "\n"
	production := render(t, "k8s/providers/hetzner/apps")
	sts := object(t, production, "StatefulSet", "private-mail")
	pod := mapping(t, mapping(t, mapping(t, sts["spec"])["template"])["spec"])
	selector := mapping(t, sequence(t, pod["initContainers"])[0])
	if selector["name"] != "select-mail-store" {
		t.Fatal("verify the persistent store before starting an application writer")
	}
	script := sequence(t, selector["args"])[0].(string)
	if strings.Contains(script, "$"+"{") {
		t.Fatal("the selector must survive strict Flux post-build substitution")
	}
	for _, tt := range []struct {
		name, receipt string
		missingData   bool
		want          bool
	}{
		{name: "verified V2 starts without V1", receipt: stamp, want: true},
		{name: "missing copy receipt"},
		{name: "invalid copy receipt", receipt: "partial\n"},
		{name: "multiple receipt lines", receipt: stamp + "\n"},
		{name: "missing database", receipt: stamp, missingData: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			target, routed := filepath.Join(root, "new"), filepath.Join(root, "route")
			template := filepath.Join(root, "datastore.json")
			for _, path := range []string{target, routed} {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if !tt.missingData {
				if err := os.Mkdir(filepath.Join(target, "data"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if tt.receipt != "" {
				if err := os.WriteFile(filepath.Join(target, ".private-mail-v2-copy-complete"), []byte(tt.receipt), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(template, []byte("{\n  \"path\": \"/var/lib/stalwart/data\",\n  \"@type\": \"RocksDb\"\n}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			command := strings.NewReplacer("/var/lib/stalwart-v2", target,
				"/etc/private-mail-datastore/config.json", template, "/store-selection", routed).Replace(script)
			run := func() ([]byte, error) {
				return exec.CommandContext(t.Context(), "/bin/sh", "-c", command).CombinedOutput()
			}
			out, err := run()
			if (err == nil) != tt.want {
				t.Fatalf("selection output: %s, err=%v", out, err)
			}
			configPath := filepath.Join(routed, "config.json")
			if !tt.want {
				if _, err = os.Lstat(configPath); !os.IsNotExist(err) {
					t.Fatal("an unverified V2 store must leave no writable route")
				}
				return
			}
			rendered, err := os.ReadFile(configPath)
			var config map[string]string
			if err != nil || json.Unmarshal(rendered, &config) != nil ||
				config["path"] != filepath.Join(target, "data") || config["@type"] != "RocksDb" {
				t.Fatalf("storage route = %q, err=%v", rendered, err)
			}
			if out, err = run(); err != nil {
				t.Fatalf("restart changed the serving store: %s, err=%v", out, err)
			}
		})
	}
}
