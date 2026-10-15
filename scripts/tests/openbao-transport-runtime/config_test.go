package transport_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Validate the declared Raft HCL with the same released server as production.
// Only local paths/listeners and the in-cluster registration integration are
// substituted. The empty store is never initialized and no credentials are read.
func TestDeclaredRaftConfigHasNoUnsupportedFields(t *testing.T) {
	binary := os.Getenv("OPENBAO_TLS_TEST_BINARY")
	if binary == "" {
		t.Skip("native binary is supplied by the mandatory chart regression")
	}
	version, err := exec.Command(binary, "version").Output()
	if err != nil || !strings.HasPrefix(string(version), "OpenBao v2.6.3 ") {
		t.Fatal("expected the verified OpenBao 2.6.3 binary")
	}
	data, err := os.ReadFile("../../../k8s/bases/infrastructure/controllers/openbao/helm-release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var release struct {
		Spec struct {
			Values struct {
				Server struct {
					HA struct {
						Raft struct{ Config string }
					} `yaml:"ha"`
				}
			}
		}
	}
	if err := yaml.Unmarshal(data, &release); err != nil {
		t.Fatal(err)
	}
	declared := release.Spec.Values.Server.HA.Raft.Config
	if declared == "" {
		t.Fatal("declared Raft configuration is missing")
	}
	for _, tc := range []struct {
		name    string
		extra   string
		warning bool
	}{
		{name: "declared configuration"},
		{name: "unknown field is still reported", extra: "\nsynthetic_unsupported_option = true\n", warning: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "data"), 0700); err != nil {
				t.Fatal(err)
			}
			config := strings.NewReplacer(
				"0.0.0.0:8200", "127.0.0.1:0",
				"0.0.0.0:8201", "127.0.0.1:0",
				"/openbao/data", filepath.ToSlash(filepath.Join(root, "data")),
				"/openbao/audit/audit.log", filepath.ToSlash(filepath.Join(root, "audit.log")),
				`service_registration "kubernetes" {}`, "",
			).Replace(declared) + "\napi_addr = \"http://127.0.0.1:8200\"\ncluster_addr = \"https://127.0.0.1:8201\"\n" + tc.extra
			path := filepath.Join(root, "config.hcl")
			if err := os.WriteFile(path, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary, "server", "-test-verify-only", "-config="+path)
			command.Env = []string{"PATH=" + os.Getenv("PATH")}
			command.Dir = root
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("native configuration validation failed: %v\n%s", err, output)
			}
			warning := "unknown or unsupported field"
			if tc.warning {
				warning += " synthetic_unsupported_option"
			}
			if got := strings.Contains(string(output), warning); got != tc.warning {
				t.Errorf("unsupported-field warning = %t, want %t\n%s", got, tc.warning, output)
			}
		})
	}
}
