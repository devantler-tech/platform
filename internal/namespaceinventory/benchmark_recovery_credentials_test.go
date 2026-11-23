package namespaceinventory

import (
	"maps"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
)

func TestBenchmarkAdmissionCredentialsRequireTheConfirmedOriginalRun(t *testing.T) {
	// The directory is deliberately absent. Reaching the private-file check
	// proves dispatch admission without loading any production credential.
	directory := "/dev/shm/longhorn-v2." + filepath.Base(filepath.Dir(t.TempDir()))
	env := map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main",
		"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_RUN_ATTEMPT": "1", "RUNNER_ENVIRONMENT": "github-hosted",
		"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/prepare-longhorn-v2-node.yaml@refs/heads/main",
		"GITHUB_SHA":          strings.Repeat("1", 40), "GITHUB_RUN_ID": "5678",
		"LONGHORN_V2_OPERATION": "recover-bound-v1-benchmark", "LONGHORN_V2_CONFIRM": "recover-bound-longhorn-v1-benchmark:1234",
		"LONGHORN_V2_CREDENTIAL_DIR": directory, "KUBECONFIG": directory + "/kubeconfig",
	}
	peer := netip.MustParseAddr("203.0.113.10")
	t.Run("ordinary benchmark", func(t *testing.T) {
		values := maps.Clone(env)
		values["LONGHORN_V2_OPERATION"], values["LONGHORN_V2_CONFIRM"] = "benchmark-v1", "benchmark-longhorn-storage"
		_, _, err := LoadBenchmarkAdmissionClient(func(key string) string { return values[key] }, peer, "")
		if err == nil || err.Error() != "credential directory is not private" {
			t.Fatalf("ordinary benchmark did not reach the private credential check: %v", err)
		}
	})
	for _, tc := range []struct {
		name, original, key, value, want string
	}{
		{"confirmed earlier run", "1234", "", "", "credential directory is not private"},
		{"unconfirmed earlier run", "9876", "", "", "protected benchmark admission dispatch is unverified"},
		{"no original run", "", "", "", "protected benchmark admission dispatch is unverified"},
		{"same run", "1234", "GITHUB_RUN_ID", "1234", "protected benchmark admission dispatch is unverified"},
		{"another operation", "1234", "LONGHORN_V2_OPERATION", "benchmark-v2", "protected benchmark admission dispatch is unverified"},
		{"explicit node", "1234", "LONGHORN_V2_NODE", "prod-worker-1", "protected benchmark admission dispatch is unverified"},
		{"explicit database", "1234", "LONGHORN_V2_DATABASE", "coroot-db", "protected benchmark admission dispatch is unverified"},
		{"other workflow", "1234", "GITHUB_WORKFLOW_REF", "other/workflow@main", "protected benchmark admission dispatch is unverified"},
		{"second attempt", "1234", "GITHUB_RUN_ATTEMPT", "2", "protected benchmark admission dispatch is unverified"},
		{"different ref", "1234", "GITHUB_REF", "refs/heads/other", "protected benchmark admission dispatch is unverified"},
		{"wrong credential directory", "1234", "LONGHORN_V2_CREDENTIAL_DIR", "/dev/shm/unrelated", "fixed benchmark admission credentials and direct IPv4 peer are required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := maps.Clone(env)
			if tc.key != "" {
				values[tc.key] = tc.value
			}
			getenv := func(key string) string { return values[key] }
			c, address, err := LoadBenchmarkAdmissionClient(getenv, peer, tc.original)
			if c != nil || address != nil || err == nil || err.Error() != tc.want {
				t.Fatalf("credential boundary returned %v\nwant %s", err, tc.want)
			}
		})
	}
}
