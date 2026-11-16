package portaldirectory

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func producerFunction(t *testing.T, name string) string {
	t.Helper()
	doc := manifest(t, "k8s/providers/hetzner/apps/customer-identity/cron-job-setup.yaml")
	containers := field(t, doc, "spec", "jobTemplate", "spec", "template", "spec", "containers").([]any)
	container := containers[0].(map[string]any)
	command := container["command"].([]any)
	script := command[len(command)-1].(string)
	start := name + "() {"
	begin := strings.Index(script, start)
	if begin < 0 {
		t.Fatalf("missing producer function %s", name)
	}
	end := strings.Index(script[begin:], "\n}")
	if end < 0 {
		t.Fatal("unterminated producer reconciliation")
	}
	return script[begin : begin+end+2]
}

func reconcileOrganization(t *testing.T, initialOrg, actualOrg, patchMode string) (map[string]string, string, error) {
	t.Helper()
	data := map[string]string{"token": base64.StdEncoding.EncodeToString([]byte("synthetic-token"))}
	if initialOrg != "" {
		data["org-id"] = base64.StdEncoding.EncodeToString([]byte(initialOrg))
	}
	initial, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(file, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	harness := `set -euo pipefail
changes=()
portal_token_secret=business-portal-zitadel-token
customers_org_id=$2
state=$1
patch_mode=$3
fail() { printf '%s\n' "$1" >&2; exit 1; }
secret_json() { cat "$state"; }
patch_secret() {
  [[ $2 == "$portal_token_secret" ]] || fail 'wrong destination'
  [[ $patch_mode != ignore ]] || return 0
  jq -s '.[0] * .[1]' "$state" <(printf '%s' "$1") > "$state.next"
  mv "$state.next" "$state"
}
`
	// The only executable input is the trusted repository's actual bootstrap function.
	cmd := exec.Command("bash", "-s", "--", file, actualOrg, patchMode)
	cmd.Stdin = strings.NewReader(harness + producerFunction(t, "keep_portal_organization") + "\nkeep_portal_organization\nprintf 'changes=%s\\n' \"${#changes[@]}\"\n")
	output, runErr := cmd.CombinedOutput()
	final, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(final, &result); err != nil {
		t.Fatal(err)
	}
	return result.Data, string(output), runErr
}

func TestOrganizationReconciliationPreservesTheCredential(t *testing.T) {
	for _, initial := range []string{"", "987654321", "123456789"} {
		t.Run("initial_"+initial, func(t *testing.T) {
			data, output, err := reconcileOrganization(t, initial, "123456789", "apply")
			if err != nil {
				t.Fatalf("reconciliation failed: %v: %s", err, output)
			}
			if data["token"] != base64.StdEncoding.EncodeToString([]byte("synthetic-token")) ||
				data["org-id"] != base64.StdEncoding.EncodeToString([]byte("123456789")) {
				t.Fatal("organization reconciliation changed the credential or kept the wrong organization")
			}
			wantChanges := "changes=1"
			if initial == "123456789" {
				wantChanges = "changes=0"
			}
			if strings.TrimSpace(output) != wantChanges {
				t.Fatalf("unexpected reconciliation or leaked output: %q", output)
			}
		})
	}
}

func TestOrganizationReconciliationRefusesAnUnknownTarget(t *testing.T) {
	data, _, err := reconcileOrganization(t, "987654321", "", "apply")
	if err == nil || data["org-id"] != base64.StdEncoding.EncodeToString([]byte("987654321")) {
		t.Fatal("unknown target must fail without changing the stored organization")
	}
}

func TestOrganizationReconciliationRequiresAReadback(t *testing.T) {
	data, _, err := reconcileOrganization(t, "987654321", "123456789", "ignore")
	if err == nil || data["org-id"] != base64.StdEncoding.EncodeToString([]byte("987654321")) {
		t.Fatal("an ignored write must fail rather than claiming the organization was stored")
	}
}
