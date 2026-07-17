package refreshfluxghcrauth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFakeKSailReadsTheConfiguredRegistryField(t *testing.T) {
	workspace := t.TempDir()
	configPath := filepath.Join(workspace, "ksail.yaml")
	config := `spec:
  cluster:
    localRegistry:
      registry: ghcr.io/example/wrong
# devantler:${GHCR_TOKEN}@ghcr.io/devantler-tech/platform/manifests
`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	setFakeKSailEnvironment(t, workspace)

	exitCode := fakeKSail([]string{"cluster", "create", "--config", configPath})

	if exitCode == 0 {
		t.Fatal("fake KSail accepted the protected registry literal outside spec.cluster.localRegistry.registry")
	}
}

func TestFakeTalosImageOperationsRequireCRINamespace(t *testing.T) {
	for _, operation := range []string{"remove", "pull"} {
		t.Run(operation, func(t *testing.T) {
			workspace := t.TempDir()
			t.Setenv("FAKE_SYNC_STATE_DIR", workspace)
			t.Setenv("FAKE_TALOS_NODES_CURRENT", "true")
			t.Setenv("TALOS_LOG", filepath.Join(workspace, "talos.log"))
			t.Setenv("OPERATION_LOG", filepath.Join(workspace, "operations.log"))
			if operation == "pull" {
				touchMarker("talos-remove-10.0.0.2")
			}

			exitCode := fakeTalosctl([]string{
				"--nodes", "10.0.0.2", "image", operation,
				"ghcr.io/devantler-tech/ksail:v1.2.3",
			})

			if exitCode == 0 {
				t.Fatalf("fake Talos %s accepted an image operation without --namespace cri", operation)
			}
		})
	}
}

func TestFakeCurlRequiresScopeDataPrefix(t *testing.T) {
	workspace := t.TempDir()
	configPath := filepath.Join(workspace, "curl.config")
	outputPath := filepath.Join(workspace, "response.json")
	if err := os.WriteFile(configPath, []byte("user = fixture:token\n"), 0o600); err != nil {
		t.Fatalf("write curl config: %v", err)
	}

	exitCode := fakeCurl([]string{
		"--disable",
		"--config", configPath,
		"--output", outputPath,
		"--data-urlencode", "not-scope=repository:devantler-tech/ksail:pull",
		"--write-out", "%{http_code}",
		"--silent",
		"--show-error",
		"--get",
		"https://ghcr.io/token",
	})

	if exitCode == 0 {
		t.Fatal("fake curl accepted token scope data without the scope= prefix")
	}
}

func TestFakeCurlRequiresAnchoredUserConfig(t *testing.T) {
	workspace := t.TempDir()
	configPath := filepath.Join(workspace, "curl.config")
	outputPath := filepath.Join(workspace, "response.json")
	if err := os.WriteFile(configPath, []byte(`header = "x-user = disguised"`), 0o600); err != nil {
		t.Fatalf("write curl config: %v", err)
	}

	exitCode := fakeCurl([]string{
		"--disable",
		"--config", configPath,
		"--output", outputPath,
		"--data-urlencode", "scope=repository:devantler-tech/ksail:pull",
		"--write-out", "%{http_code}",
		"--silent",
		"--show-error",
		"--get",
		"https://ghcr.io/token",
	})

	if exitCode == 0 {
		t.Fatal("fake curl accepted an embedded user setting instead of an anchored user line")
	}
}

func TestFakeCurlRequiresBoundedTimeouts(t *testing.T) {
	tests := []struct {
		name   string
		config string
		args   []string
	}{
		{
			name:   "token exchange",
			config: "user = fixture:token\n",
			args: []string{
				"--disable",
				"--config", "config",
				"--output", "response",
				"--data-urlencode", "scope=repository:devantler-tech/ksail:pull",
				"--write-out", "%{http_code}",
				"--silent",
				"--show-error",
				"--get",
				"https://ghcr.io/token",
			},
		},
		{
			name:   "manifest read",
			config: "header = \"Authorization: Bearer fixture-registry-token\"\n",
			args: []string{
				"--disable",
				"--config", "config",
				"--output", "/dev/null",
				"--write-out", "%{http_code}",
				"--header", "Accept: application/vnd.oci.image.manifest.v1+json",
				"--silent",
				"--show-error",
				"https://ghcr.io/v2/devantler-tech/ksail/manifests/latest",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			configPath := filepath.Join(workspace, "curl.config")
			responsePath := filepath.Join(workspace, "response.json")
			t.Setenv("REGISTRY_READ_LOG", filepath.Join(workspace, "registry-reads.log"))
			if err := os.WriteFile(configPath, []byte(test.config), 0o600); err != nil {
				t.Fatalf("write curl config: %v", err)
			}
			args := append([]string(nil), test.args...)
			for index, argument := range args {
				switch argument {
				case "config":
					args[index] = configPath
				case "response":
					args[index] = responsePath
				}
			}

			exitCode := fakeCurl(args)

			if exitCode == 0 {
				t.Fatal("fake curl accepted an unbounded registry request")
			}
		})
	}
}

func setFakeKSailEnvironment(t *testing.T, workspace string) {
	t.Helper()
	for _, name := range []string{
		"KSAIL_TOKEN_CAPTURE",
		"KSAIL_USERNAME_CAPTURE",
		"KSAIL_REVISION_CAPTURE",
		"KSAIL_COMMAND_CAPTURE",
		"KSAIL_CONFIG_PATH_CAPTURE",
		"KSAIL_REGISTRY_CAPTURE",
		"KSAIL_REGISTRY_OVERRIDE_CAPTURE",
	} {
		t.Setenv(name, filepath.Join(workspace, name))
	}
	t.Setenv("KSAIL_SPEC_CLUSTER_LOCALREGISTRY_REGISTRY", `${GHCR_USERNAME}:${GHCR_TOKEN}@ghcr.io/devantler-tech/platform/manifests`)
	t.Setenv("GHCR_TOKEN", "fixture-token")
	t.Setenv("GHCR_USERNAME", "devantler")
	t.Setenv("GHCR_PULL_REVISION", "fixture-revision")
}
