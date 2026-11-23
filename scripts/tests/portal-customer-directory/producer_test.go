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
	containers, ok := field(t, doc, "spec", "jobTemplate", "spec", "template", "spec", "containers").([]any)
	if !ok || len(containers) == 0 {
		t.Fatal("missing producer containers")
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatal("producer container is not a mapping")
	}
	command, ok := container["command"].([]any)
	if !ok || len(command) == 0 {
		t.Fatal("missing producer command")
	}
	script, ok := command[len(command)-1].(string)
	if !ok {
		t.Fatal("producer script is not a string")
	}
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

func TestPortalOperatorProvisioningIsIsolatedAndUsesTheExactLink(t *testing.T) {
	script := producerFunction(t, "prepare_portal_operator")
	for _, mode := range []string{"new", "existing", "customer-org", "foreign-user", "wrong-username", "wrong-link", "unverified-email"} {
		t.Run(mode, func(t *testing.T) {
			harness := `set -euo pipefail
mode=$1
changes=()
org_id=instance-admin-org
customers_org_id=customer-org
portal_operators_org='Portal operators'
portal_operators_domain=portal-operators.example.invalid
portal_operator_id=02bf754c-559b-486d-83f7-ed89cfc38d08
portal_operator_username=portal-operator-02bf754c-559b-486d-83f7-ed89cfc38d08
admin_github_login=fictional-owner
admin_github_id=1234
ADMIN_EMAIL=operator@example.invalid
admins='{"result":[{"human":{"email":{"email":"operator@example.invalid","isVerified":true},"profile":{"givenName":"Test","familyName":"Operator"}}}]}'
if [[ $mode == unverified-email ]]; then
  admins=$(jq '.result[0].human.email.isVerified=false' <<<"$admins")
fi
state=$2
fail() { printf '%s\n' "$1" >&2; exit 1; }
connect() {
  case $1 in
    zitadel.org.v2.OrganizationService/ListOrganizations)
      jq -e '.queries == [{nameQuery:{name:"Portal operators",method:"TEXT_QUERY_METHOD_EQUALS"}}]' <<<"$2" >/dev/null
      local id=operator-org
      [[ $mode != customer-org ]] || id=$customers_org_id
      jq -nc --arg id "$id" '{result:[{id:$id,primaryDomain:"portal-operators.example.invalid"}]}'
      ;;
    zitadel.user.v2.UserService/ListUsers)
      jq -e --arg id "$portal_operator_id" '.queries == [{inUserIdsQuery:{userIds:[$id]}}]' <<<"$2" >/dev/null
      if [[ $mode != new && $mode != unverified-email || -e $state ]]; then
        local owner=operator-org name=$portal_operator_username
        [[ $mode != foreign-user ]] || owner=$customers_org_id
        [[ $mode != wrong-username ]] || name=$ADMIN_EMAIL
        jq -nc --arg id "$portal_operator_id" --arg owner "$owner" --arg name "$name" \
          '{result:[{userId:$id,details:{resourceOwner:$owner},state:"USER_STATE_ACTIVE",username:$name,
            human:{email:{email:"operator@example.invalid",isVerified:true}}}]}'
      else
        printf '%s\n' '{"result":[]}'
      fi
      ;;
    zitadel.user.v2.UserService/CreateUser)
      jq -e --arg id "$portal_operator_id" --arg name "$portal_operator_username" \
        '. == {userId:$id,organizationId:"operator-org",username:$name,
        human:{profile:{givenName:"Test",familyName:"Operator"},email:{email:"operator@example.invalid",isVerified:true},
          idpLinks:[{idpId:"approved-provider",userId:"1234",userName:"fictional-owner"}]}}' <<<"$2" >/dev/null || fail 'unexpected portal operator creation request'
      touch "$state"
      jq -nc --arg id "$portal_operator_id" '{id:$id}'
      ;;
    zitadel.user.v2.UserService/ListIDPLinks)
      jq -e --arg id "$portal_operator_id" '.userId == $id' <<<"$2" >/dev/null
      local upstream=$admin_github_id
      [[ $mode != wrong-link ]] || upstream=5678
      jq -nc --arg u "$upstream" '{result:[{idpId:"approved-provider",userId:$u}]}'
      ;;
    *) fail 'unexpected API call' ;;
  esac
}
`
			state := filepath.Join(t.TempDir(), "created")
			cmd := exec.Command("bash", "-s", "--", mode, state)
			cmd.Stdin = strings.NewReader(harness + script + "\nprepare_portal_operator approved-provider\n")
			output, err := cmd.CombinedOutput()
			wantOK := mode == "new" || mode == "existing"
			if (err == nil) != wantOK {
				t.Fatalf("expected success=%t, got %v: %s", wantOK, err, output)
			}
			_, writeErr := os.Stat(state)
			if (writeErr == nil) != (mode == "new") {
				t.Fatalf("unexpected provisioning write: %v", writeErr)
			}
		})
	}
}
