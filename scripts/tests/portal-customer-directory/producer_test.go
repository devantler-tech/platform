package portaldirectory

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
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

func TestCustomerProviderTransitionPreservesAccess(t *testing.T) {
	function := producerFunction(t, "reconcile_customer_login_provider")
	for _, mode := range []string{"inherited", "legacy", "both", "shared", "rollback", "unexpected", "unconfirmed-add"} {
		t.Run(mode, func(t *testing.T) {
			harness := `set -euo pipefail
mode=$1
work=$2
customers_org_id=customers
customer_policy={}
changes=()
from=legacy to=shared owner=IDP_OWNER_TYPE_SYSTEM
case "$mode" in
  inherited) ids='[]';;
  legacy|unconfirmed-add) ids='["legacy"]';;
  both) ids='["legacy","shared"]';;
  shared|rollback) ids='["shared"]';;
  unexpected) ids='["legacy","unrelated"]';;
esac
if [[ $mode == rollback ]]; then
  from=shared to=legacy owner=IDP_OWNER_TYPE_ORG
fi
printf '%s\n' "$ids" >"$work/ids"
fail() { printf '%s\n' "$1" >&2; exit 1; }
copy_customer_github_links() {
  [[ $1 == "$from" && $2 == "$to" ]] || fail 'wrong migration direction'
  touch "$work/copied"
}
api() {
  [[ $api_org == customers ]] || fail 'wrong scope'
  case "$1 $2" in
    'GET /management/v1/policies/login')
      jq -nc --arg mode "$mode" --argjson pending "$([[ -e $work/ensured ]] && echo false || echo true)" \
        '{policy:{isDefault:($mode=="inherited" and $pending)}}'
      ;;
    'POST /management/v1/policies/login/idps/_search')
      jq '{result: map({idpId:.})}' "$work/ids"
      ;;
    'POST /management/v1/policies/login/idps')
      [[ -e $work/copied ]] || fail 'provider appeared before the links were copied'
      jq -e --arg to "$to" --arg owner "$owner" '. == {idpId:$to,ownerType:$owner}' <<<"$3" >/dev/null || fail 'wrong provider write'
      if [[ $mode != unconfirmed-add ]]; then
        jq --arg to "$to" '. + [$to]' "$work/ids" >"$work/next"
        mv "$work/next" "$work/ids"
      fi
      ;;
    "DELETE /management/v1/policies/login/idps/$from")
      jq -e --arg to "$to" 'index($to) != null' "$work/ids" >/dev/null || fail 'removed a provider before its replacement was confirmed'
      jq --arg from "$from" 'map(select(. != $from))' "$work/ids" >"$work/next"
      mv "$work/next" "$work/ids"
      touch "$work/removed"
      ;;
    *) fail 'unexpected provider request' ;;
  esac
}
ensure_login_policy() {
  [[ $1 == customers && $2 == '{}' && $3 == "$to" && $4 == "$owner" && -e $work/copied ]] || fail 'wrong policy reconciliation'
  if [[ $mode == inherited ]]; then
    jq -nc --arg to "$to" '[$to]' >"$work/ids"
  fi
  touch "$work/ensured"
}
`
			work := t.TempDir()
			cmd := exec.Command("bash", "-s", "--", mode, work)
			cmd.Stdin = strings.NewReader(harness + function + "\nreconcile_customer_login_provider \"$from\" \"$to\" \"$owner\"\n")
			output, err := cmd.CombinedOutput()
			wantOK := mode != "unexpected" && mode != "unconfirmed-add"
			if (err == nil) != wantOK {
				t.Fatalf("expected transition success=%t, got %v: %s", wantOK, err, output)
			}
			for marker, want := range map[string]bool{
				"copied":  mode != "unexpected",
				"removed": mode == "legacy" || mode == "both" || mode == "rollback",
				"ensured": wantOK,
			} {
				_, err := os.Stat(filepath.Join(work, marker))
				if (err == nil) != want {
					t.Fatalf("unexpected %s marker: %v", marker, err)
				}
			}
		})
	}
}

func TestSharedProviderMigrationUsesOnlyExistingCustomerLinks(t *testing.T) {
	script := strings.Join([]string{
		producerFunction(t, "customer_github_links"),
		producerFunction(t, "read_customer_subjects"),
		producerFunction(t, "copy_customer_github_links"),
	}, "\n")
	for _, mode := range []string{"copy", "already-copied", "conflict", "truncated-customers", "truncated-links",
		"foreign-customer", "duplicate-customer", "changed-directory", "invalid-subject", "empty"} {
		t.Run(mode, func(t *testing.T) {
			harness := `set -euo pipefail
mode=$1
work=$2
changes=()
customers_org_id=customers
fail() { printf '%s\n' "$1" >&2; exit 1; }
connect() {
  case $1 in
    zitadel.user.v2.UserService/ListUsers)
      jq -e '.query.limit == 100 and .query.asc == true and
        .queries == [{typeQuery:{type:"TYPE_HUMAN"}},{organizationIdQuery:{organizationId:"customers"}}]' <<<"$2" >/dev/null
      local subject owner=customers sequence=42
      case $(jq -er '.query.offset' <<<"$2") in
        0)
          if [[ $mode == empty ]]; then
            printf '%s\n' '{"details":{"processedSequence":"42"}}'
            return
          fi
          subject=alice
          ;;
        1)
          if [[ $mode == truncated-customers ]]; then
            printf '%s\n' '{"details":{"totalResult":"2","processedSequence":"42"}}'
            return
          fi
          subject=bob
          [[ $mode != foreign-customer ]] || owner=other
          [[ $mode != duplicate-customer ]] || subject=alice
          [[ $mode != changed-directory ]] || sequence=43
          [[ $mode != invalid-subject ]] || subject=$'bob\nalice'
          ;;
        *) fail 'unexpected customer directory offset' ;;
      esac
      jq -nc --arg owner "$owner" --arg subject "$subject" --arg sequence "$sequence" \
        '{details:{totalResult:"2",processedSequence:$sequence},
          result:[{userId:$subject,details:{resourceOwner:$owner},human:{}}]}'
      ;;
    zitadel.user.v2.UserService/ListIDPLinks)
      case $(jq -r '.userId' <<<"$2") in
        alice)
          local rows='[{"idpId":"old","userId":"111","userName":"alice-gh"}]'
          if [[ -e $work/wrote || $mode == already-copied ]]; then
            rows=$(jq '. + [{idpId:"shared",userId:"111",userName:"alice-gh"}]' <<<"$rows")
          elif [[ $mode == conflict ]]; then
            rows=$(jq '. + [{idpId:"shared",userId:"222",userName:"another"}]' <<<"$rows")
          fi
          jq -nc --argjson a "$rows" '{details:{totalResult:($a|length|tostring)},result:$a}'
          ;;
        bob)
          if [[ $mode == truncated-links ]]; then
            printf '%s\n' '{"details":{"totalResult":"1"}}'
          else
            printf '%s\n' '{"details":{"totalResult":"0"}}'
          fi ;;
        *) fail 'unexpected subject' ;;
      esac
      ;;
    zitadel.user.v2.UserService/AddIDPLink)
      jq -e '. == {userId:"alice",idpLink:{idpId:"shared",userId:"111",userName:"alice-gh"}}' <<<"$2" >/dev/null
      touch "$work/wrote"
      printf '{}\n'
      ;;
    *) fail 'unexpected API call' ;;
  esac
}

`
			work := t.TempDir()
			cmd := exec.Command("bash", "-s", "--", mode, work)
			cmd.Stdin = strings.NewReader(harness + script + "\ncopy_customer_github_links old shared\n")
			output, err := cmd.CombinedOutput()
			wantOK := mode == "copy" || mode == "already-copied" || mode == "empty"
			if (err == nil) != wantOK {
				t.Fatalf("expected migration success=%t, got %v: %s", wantOK, err, output)
			}
			_, writeErr := os.Stat(filepath.Join(work, "wrote"))
			if (writeErr == nil) != (mode == "copy") {
				t.Fatalf("unexpected migration write: %v", writeErr)
			}
		})
	}
}

func TestSharedInstanceLoginRestoresPreviousSettings(t *testing.T) {
	script := strings.Join([]string{
		producerFunction(t, "instance_login_providers"),
		producerFunction(t, "instance_login_default_org"),
		producerFunction(t, "reconcile_shared_instance_login"),
	}, "\n")
	for _, mode := range []string{"round-trip", "previous-member", "inherited-disabled", "inherited-enabled",
		"never-activated", "different-provider", "rollback-retry", "unrecorded-baseline"} {
		t.Run(mode, func(t *testing.T) {
			harness := `set -euo pipefail
mode=$1
state=$2
fixture_mode=$1
portal_token_secret=portal-token
normalise_policy='.'
key=platform.devantler.tech/shared-portal-login-baseline
fail() { printf '%s\n' "$1" >&2; exit 1; }
change() { jq -c "$@" "$state" > "$state.next"; mv "$state.next" "$state"; }
secret_json() { [[ $1 == "$portal_token_secret" ]]; jq -c '.secret' "$state"; }
patch_secret() {
  [[ $2 == "$portal_token_secret" ]] || fail 'wrong destination'
  [[ $fixture_mode != unrecorded-baseline ]] || return 0
  change --arg key "$key" --argjson patch "$1" '
    $patch.metadata.annotations[$key] as $value |
    if $value == null then del(.secret.metadata.annotations[$key])
    else .secret.metadata.annotations[$key]=$value end'
}
api() {
  case "$1 $2" in
    'GET /admin/v1/policies/login') jq -c '{policy}' "$state" ;;
    'PUT /admin/v1/policies/login') change --argjson policy "$3" '.policy=$policy' ;;
    'POST /admin/v1/policies/login/idps/_search')
      jq -c '{details:{totalResult:(.providers|length|tostring)},result:[.providers[]|{idpId:.}]}' "$state"
      ;;
    'POST /admin/v1/policies/login/idps') change --argjson data "$3" '.providers += [$data.idpId]' ;;
    'DELETE /admin/v1/policies/login/idps/shared') change '.providers -= ["shared"]' ;;
    *) fail 'unexpected policy API call' ;;
  esac
}
connect() {
  case "$1" in
    zitadel.feature.v2.FeatureService/GetInstanceFeatures)
      [[ $2 == '{"inheritance":true}' ]] || fail 'did not read effective feature'
      jq -c '.feature | {loginDefaultOrg:(if .enabled then . else del(.enabled) end),userSchema:{enabled:true}}' "$state"
      ;;
    zitadel.feature.v2.FeatureService/SetInstanceFeatures)
      jq -e 'keys == ["loginDefaultOrg"]' <<<"$2" >/dev/null
      [[ ! -e $state.refuse-feature ]] || fail 'feature temporarily unavailable'
      change --argjson data "$2" '.feature.enabled=$data.loginDefaultOrg'
      ;;
    *) fail 'unexpected feature API call' ;;
  esac
}
`
			run := `
case $mode in
  previous-member) change '.providers=["shared"]' ;;
  inherited-disabled) change '.feature={enabled:false,source:"SOURCE_SYSTEM"}' ;;
  inherited-enabled) change '.feature.source="SOURCE_SYSTEM"' ;;
esac
cp "$state" "$state.before"
if [[ $mode == never-activated ]]; then
  reconcile_shared_instance_login off shared
  cmp "$state.before" "$state"
  exit
fi
reconcile_shared_instance_login on shared
baseline=$(jq -er --arg key "$key" '.secret.metadata.annotations[$key]' "$state")
jq -e --arg key "$key" '
  .secret.metadata.annotations[$key] != null and .providers == ["shared"] and .feature.enabled == false and
  (.policy | .allowUsernamePassword and .allowExternalIdp and .forceMfa and (.allowRegister | not))' "$state" >/dev/null
reconcile_shared_instance_login on shared
[[ $(jq -er --arg key "$key" '.secret.metadata.annotations[$key]' "$state") == "$baseline" ]]
change '.policy.defaultRedirectUri="https://example.invalid/current" | .providers += ["unrelated"]'
if [[ $mode == different-provider ]]; then
  reconcile_shared_instance_login off another
  exit
fi
if [[ $mode == rollback-retry ]]; then
  touch "$state.refuse-feature"
  if bash -e -s -- "$mode" "$state" < "$state.rollback"; then fail 'claimed rollback after failed write'; fi
  jq -e --arg key "$key" '.secret.metadata.annotations[$key] != null' "$state" >/dev/null
  rm "$state.refuse-feature"
fi
reconcile_shared_instance_login off shared
jq -es '.[1] == (.[0] | .policy.defaultRedirectUri="https://example.invalid/current" | .providers += ["unrelated"])' \
  "$state.before" "$state" >/dev/null
`
			initial := `{"secret":{"metadata":{"annotations":{"unrelated":"keep"}},"data":{"token":"kept"}},"policy":{"allowUsernamePassword":false,"allowRegister":true,"allowExternalIdp":false,"forceMfa":false,"defaultRedirectUri":"https://example.invalid/before"},"providers":[],"feature":{"enabled":true,"source":"SOURCE_INSTANCE"}}`
			state := filepath.Join(t.TempDir(), "instance.json")
			if err := os.WriteFile(state, []byte(initial), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(state+".rollback", []byte(harness+script+"\nreconcile_shared_instance_login off shared\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-s", "--", mode, state)
			cmd.Stdin = strings.NewReader(harness + script + "\n" + run)
			output, err := cmd.CombinedOutput()
			wantError := map[string]string{
				"inherited-enabled":   "requires an instance-managed or disabled login default organization",
				"different-provider":  "rollback settings do not match this provider",
				"unrecorded-baseline": "could not verify the shared login rollback settings",
			}[mode]
			if wantError != "" {
				if err == nil || !strings.Contains(string(output), wantError) {
					t.Fatalf("expected %q, got %v: %s", wantError, err, output)
				}
			} else if err != nil {
				t.Fatalf("instance reconciliation failed: %v: %s", err, output)
			}
		})
	}
}

func TestCustomerDirectoryBeyondSingleRequestLimit(t *testing.T) {
	harness := `set -euo pipefail
customers_org_id=customers
fail() { printf '%s\n' "$1" >&2; exit 1; }
connect() {
  [[ $1 == zitadel.user.v2.UserService/ListUsers ]]
  jq -c '
    .query | (.offset | tonumber) as $start | [$start + .limit, 1012] | min as $end |
    {details:{totalResult:"1012",processedSequence:"42"},result:[
      range($start;$end) | {userId:("user-" + tostring),details:{resourceOwner:"customers"},human:{}}]}' <<<"$2"
}
`
	subjects := filepath.Join(t.TempDir(), "subjects")
	cmd := exec.Command("bash", "-s", "--", subjects)
	cmd.Stdin = strings.NewReader(harness + producerFunction(t, "read_customer_subjects") + "\nread_customer_subjects \"$1\"\n")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("paging the customer directory failed: %v: %s", err, output)
	}
	data, err := os.ReadFile(subjects)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]string, 1012)
	for i := range want {
		want[i] = fmt.Sprintf("user-%d\n", i)
	}
	if string(data) != strings.Join(want, "") {
		t.Fatal("paged customer subjects differ from the complete directory")
	}
}
