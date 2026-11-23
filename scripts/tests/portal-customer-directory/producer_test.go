package portaldirectory

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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
	for _, mode := range []string{"inherited", "legacy", "both", "shared", "rollback", "unexpected", "unconfirmed-add",
		"unknown-both", "reset-drift", "reset-after-migration", "rollback-then-settled", "prepared-then-visible",
		"cancel-prepared-both", "cancel-prepared-after-removal", "cancel-prepared-inherited",
		"resume-prepared-inherited", "cancel-overlap-outgoing"} {
		t.Run(mode, func(t *testing.T) {
			harness := `set -euo pipefail
mode=$1
work=$2
customers_org_id=customers
customer_policy={}
legacy_customer_idp=legacy
changes=()
from=legacy to=shared owner=IDP_OWNER_TYPE_SYSTEM
phase_record=null
case "$mode" in
  inherited|reset-after-migration|cancel-prepared-inherited|resume-prepared-inherited) ids='[]';;
  legacy|unconfirmed-add|reset-drift|rollback-then-settled|cancel-prepared-after-removal) ids='["legacy"]';;
  both|unknown-both|cancel-prepared-both|cancel-overlap-outgoing) ids='["legacy","shared"]';;
  shared|rollback|prepared-then-visible) ids='["shared"]';;
  unexpected) ids='["legacy","unrelated"]';;
esac
case "$mode" in
  both|prepared-then-visible|cancel-prepared-*|resume-prepared-inherited|cancel-overlap-outgoing)
    phase_record='{"version":1,"phase":"prepared","from":"legacy","to":"shared"}';;
  reset-drift|reset-after-migration|rollback|shared) phase_record='{"version":1,"phase":"applied","from":"legacy","to":"shared"}';;
  rollback-then-settled) phase_record='{"version":1,"phase":"applied","from":"shared","to":"legacy"}';;
esac
if [[ $mode == rollback || $mode == rollback-then-settled || $mode == cancel-* ]]; then
  from=shared to=legacy owner=IDP_OWNER_TYPE_ORG
fi
printf '%s\n' "$ids" >"$work/ids"
printf '%s\n' "$phase_record" >"$work/migration"
fail() { printf '%s\n' "$1" >&2; exit 1; }
customer_migration_state() { cat "$work/migration"; }
save_customer_migration() {
  [[ $2 == "$from" && $3 == "$to" ]] || fail 'wrong recorded migration direction'
  jq -nc --arg phase "$1" --arg from "$2" --arg to "$3" \
    '{version:1,phase:$phase,from:$from,to:$to}' >"$work/migration"
  touch "$work/$1"
}
reconcile_customer_github_links() {
  [[ $1 == "$from" && $2 == "$to" ]] || fail 'wrong migration direction'
  if [[ $mode == rollback ]]; then
    [[ ${3:-} == authoritative ]] || fail 'the confirmed rollback provider lost authority'
  fi
  touch "$work/copied"
}
read_customer_subjects() { printf '%s\n' synthetic >"$1"; printf '%s\n' unchanged; }
customer_directory_page() { printf '%s\n' unchanged; }
customer_directory_stamp() { cat; }
verify_customer_github_links() {
  [[ $1 == "$from" && $2 == "$to" && $4 == preserve-destination && -s $3 ]] ||
    fail 'wrong membership or verification mode for provider overlap'
  touch "$work/scanned-overlap"
  [[ $mode != cancel-overlap-outgoing ]] || fail 'would orphan an approved GitHub link'
}
api() {
  [[ $api_org == customers ]] || fail 'wrong scope'
  case "$1 $2" in
    'GET /management/v1/policies/login')
      jq -nc --arg mode "$mode" --argjson pending "$([[ -e $work/ensured ]] && echo false || echo true)" \
        '{policy:{isDefault:((["inherited","reset-after-migration","cancel-prepared-inherited","resume-prepared-inherited"]|index($mode) != null) and $pending)}}'
      ;;
    'POST /management/v1/policies/login/idps/_search')
      jq '{result: map({idpId:.})}' "$work/ids"
      ;;
    'POST /management/v1/policies/login/idps')
      [[ -e $work/copied ]] || fail 'provider appeared before the links were copied'
      jq -e --arg to "$to" '.phase == "prepared" and .to == $to' "$work/migration" >/dev/null ||
        fail 'provider appeared before the copy was durably recorded'
      jq -e --arg to "$to" --arg owner "$owner" '. == {idpId:$to,ownerType:$owner}' <<<"$3" >/dev/null || fail 'wrong provider write'
      if [[ $mode != unconfirmed-add ]]; then
        jq --arg to "$to" '. + [$to]' "$work/ids" >"$work/next"
        mv "$work/next" "$work/ids"
      fi
      ;;
    "DELETE /management/v1/policies/login/idps/$from")
      touch "$work/delete-attempted"
      jq -e --arg to "$to" 'index($to) != null' "$work/ids" >/dev/null || fail 'removed a provider before its replacement was confirmed'
      jq --arg from "$from" 'map(select(. != $from))' "$work/ids" >"$work/next"
      mv "$work/next" "$work/ids"
      touch "$work/removed"
      ;;
    *) fail 'unexpected provider request' ;;
  esac
}
ensure_login_policy() {
  [[ $1 == customers && $2 == '{}' && $3 == "$to" && $4 == "$owner" ]] || fail 'wrong policy reconciliation'
  if [[ $mode == inherited || $mode == resume-prepared-inherited ]]; then
    [[ -e $work/copied ]] || fail 'shared policy published before links were copied'
    jq -e --arg to "$to" '.phase == "prepared" and .to == $to' "$work/migration" >/dev/null ||
      fail 'shared policy published before the migration was prepared'
  fi
  if [[ $mode == inherited || $mode == reset-after-migration || $mode == *-prepared-inherited ]]; then
    jq -nc --arg to "$to" '[$to]' >"$work/ids"
  fi
  touch "$work/ensured"
}
`
			work := t.TempDir()
			cmd := exec.Command("bash", "-s", "--", mode, work)
			cmd.Stdin = strings.NewReader(harness + function + "\nreconcile_customer_login_provider \"$from\" \"$to\" \"$owner\"\n")
			output, err := cmd.CombinedOutput()
			wantOK := mode != "unexpected" && mode != "unconfirmed-add" && mode != "unknown-both" &&
				mode != "reset-drift" && mode != "cancel-overlap-outgoing"
			if (err == nil) != wantOK {
				t.Fatalf("expected transition success=%t, got %v: %s", wantOK, err, output)
			}
			wantRemoval := mode == "legacy" || mode == "both" || mode == "rollback" || mode == "cancel-prepared-both"
			wantCopy := mode == "legacy" || mode == "rollback" || mode == "inherited" ||
				mode == "unconfirmed-add" || mode == "resume-prepared-inherited"
			wantFinish := mode == "legacy" || mode == "rollback" || mode == "inherited" || mode == "both" ||
				mode == "prepared-then-visible" || strings.HasPrefix(mode, "cancel-prepared-") || mode == "resume-prepared-inherited"
			for marker, want := range map[string]bool{
				"copied":           wantCopy,
				"prepared":         wantCopy,
				"applied":          wantFinish,
				"delete-attempted": wantRemoval,
				"removed":          wantRemoval,
				"ensured":          wantOK,
				"scanned-overlap":  mode == "both" || mode == "cancel-prepared-both" || mode == "cancel-overlap-outgoing",
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
		producerFunction(t, "customer_github_link_page"),
		producerFunction(t, "customer_github_links"),
		producerFunction(t, "customer_directory_page"),
		producerFunction(t, "customer_directory_stamp"),
		producerFunction(t, "read_customer_subjects"),
		producerFunction(t, "customer_github_link_change"),
		producerFunction(t, "verify_customer_github_links"),
		producerFunction(t, "reconcile_customer_github_links"),
	}, "\n")
	for _, mode := range []string{"copy", "already-copied", "conflict", "truncated-customers", "truncated-links",
		"foreign-customer", "duplicate-customer", "changed-directory", "first-page-drift", "invalid-subject", "empty",
		"revoked-source", "revoked-during-plan", "no-op-revoked-during-copy", "link-projection-changed",
		"authoritative-replaced", "overlap-outgoing-only", "overlap-incoming-only"} {
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
      jq -e '(.query.limit == 1 or .query.limit == 100) and .query.asc == true and
        .queries == [{typeQuery:{type:"TYPE_HUMAN"}},{organizationIdQuery:{organizationId:"customers"}}]' <<<"$2" >/dev/null ||
        fail 'unexpected customer directory request'
      local subject owner=customers sequence=42
      if [[ $mode == first-page-drift ]]; then
        if [[ $(jq -r '.query.limit' <<<"$2") == 1 ]]; then
          printf '%s\n' '{"details":{"totalResult":"2","processedSequence":"42"},"result":[{"userId":"former","details":{"resourceOwner":"customers"},"human":{}}]}'
          return
        fi
        sequence=43
      fi
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
          if [[ $mode == revoked-source ]]; then
            rows='[{"idpId":"shared","userId":"111","userName":"alice-gh"}]'
            [[ ! -e $work/revoked ]] || rows='[]'
          elif [[ $mode == revoked-during-plan ]]; then
            [[ ! -e $work/observed ]] || rows='[]'
            touch "$work/observed"
          elif [[ $mode == no-op-revoked-during-copy ]]; then
            rows=$(jq '. + [{idpId:"shared",userId:"111",userName:"alice-gh"}]' <<<"$rows")
            [[ ! -e $work/bob-copied ]] || rows=$(jq 'map(select(.idpId == "shared"))' <<<"$rows")
          fi
          if [[ -e $work/wrote || $mode == already-copied || $mode == link-projection-changed ]]; then
            rows=$(jq '. + [{idpId:"shared",userId:"111",userName:"alice-gh"}]' <<<"$rows")
          elif [[ $mode == conflict || ( $mode == authoritative-replaced && ! -e $work/revoked ) ]]; then
            rows=$(jq '. + [{idpId:"shared",userId:"222",userName:"another"}]' <<<"$rows")
          elif [[ $mode == overlap-incoming-only ]]; then
            rows='[{"idpId":"shared","userId":"222","userName":"another"}]'
          fi
          jq -nc --argjson a "$rows" '{details:{totalResult:($a|length|tostring),processedSequence:"42"},result:$a}'
          ;;
        bob)
          if [[ $mode == truncated-links ]]; then
            printf '%s\n' '{"details":{"totalResult":"1"}}'
          elif [[ $mode == link-projection-changed && -e $work/bob-read ]]; then
            printf '%s\n' '{"details":{"totalResult":"0","processedSequence":"43"}}'
          elif [[ $mode == no-op-revoked-during-copy ]]; then
            local rows='[{"idpId":"old","userId":"222","userName":"bob-gh"}]'
            if [[ -e $work/bob-copied ]]; then
              rows=$(jq '. + [{idpId:"shared",userId:"222",userName:"bob-gh"}]' <<<"$rows")
            fi
            jq -nc --argjson a "$rows" '{details:{totalResult:($a|length|tostring),processedSequence:"42"},result:$a}'
          else
            touch "$work/bob-read"
            printf '%s\n' '{"details":{"totalResult":"0","processedSequence":"42"}}'
          fi ;;
        *) fail 'unexpected subject' ;;
      esac
      ;;
    zitadel.user.v2.UserService/AddIDPLink)
      if [[ $mode == no-op-revoked-during-copy ]]; then
        jq -e '. == {userId:"bob",idpLink:{idpId:"shared",userId:"222",userName:"bob-gh"}}' <<<"$2" >/dev/null ||
          fail 'unexpected changed link in fixture'
        touch "$work/bob-copied"
        printf '{}\n'
        return
      fi
      jq -e '. == {userId:"alice",idpLink:{idpId:"shared",userId:"111",userName:"alice-gh"}}' <<<"$2" >/dev/null
      touch "$work/wrote"
      printf '{}\n'
      ;;
    zitadel.user.v2.UserService/RemoveIDPLink)
      local stale_id=111
      [[ $mode != authoritative-replaced ]] || stale_id=222
      jq -e --arg id "$stale_id" '. == {userId:"alice",idpId:"shared",linkedUserId:$id}' <<<"$2" >/dev/null ||
        fail 'tried to revoke a different account or provider'
      touch "$work/revoked"
      printf '{}\n'
      ;;
    *) fail 'unexpected API call' ;;
  esac
}

`
			work := t.TempDir()
			cmd := exec.Command("bash", "-s", "--", mode, work)
			command := "\nreconcile_customer_github_links old shared\n"
			switch mode {
			case "authoritative-replaced":
				command = "\nreconcile_customer_github_links old shared authoritative\n"
			case "overlap-outgoing-only", "overlap-incoming-only":
				command = "\nread_customer_subjects \"$work/subjects\" >/dev/null\nverify_customer_github_links old shared \"$work/subjects\" preserve-destination\n"
			}
			cmd.Stdin = strings.NewReader(harness + script + command)
			output, err := cmd.CombinedOutput()
			wantOK := mode == "copy" || mode == "already-copied" || mode == "empty" || mode == "revoked-source" ||
				mode == "authoritative-replaced" || mode == "overlap-incoming-only"
			if (err == nil) != wantOK {
				t.Fatalf("expected migration success=%t, got %v: %s", wantOK, err, output)
			}
			_, writeErr := os.Stat(filepath.Join(work, "wrote"))
			if (writeErr == nil) != (mode == "copy" || mode == "authoritative-replaced") {
				t.Fatalf("unexpected migration write: %v", writeErr)
			}
			if _, err := os.Stat(filepath.Join(work, "revoked")); (err == nil) != (mode == "revoked-source" || mode == "authoritative-replaced") {
				t.Fatalf("unexpected removal of an inactive link: %v", err)
			}
			if mode == "no-op-revoked-during-copy" {
				if _, err := os.Stat(filepath.Join(work, "bob-copied")); err != nil ||
					!strings.Contains(string(output), "before activating the provider") {
					t.Fatalf("expected refusal after Bob's copy and Alice's revocation, got %v: %s", err, output)
				}
			}
			if mode == "link-projection-changed" && !strings.Contains(string(output), "changed during final verification") {
				t.Fatalf("expected the final link scan to reject a changed projection: %s", output)
			}
		})
	}
}

func TestSetupTokenCoversTheJobDeadline(t *testing.T) {
	doc := manifest(t, "k8s/providers/hetzner/apps/customer-identity/cron-job-setup.yaml")
	deadline, ok := field(t, doc, "spec", "jobTemplate", "spec", "activeDeadlineSeconds").(int)
	if !ok {
		t.Fatal("missing setup Job deadline")
	}
	init, ok := field(t, doc, "spec", "jobTemplate", "spec", "template", "spec", "initContainers").([]any)
	if !ok || len(init) != 1 {
		t.Fatal("expected the setup Job signing init container")
	}
	signer, ok := init[0].(map[string]any)
	if !ok || signer["name"] != "sign-token" {
		t.Fatal("expected the setup token signer")
	}
	command, ok := field(t, signer, "command").([]any)
	if !ok || len(command) != 3 || command[0] != "/bin/sh" || command[1] != "-c" {
		t.Fatal("expected the signing script")
	}
	script, ok := command[2].(string)
	if !ok {
		t.Fatal("the signing script must be a string")
	}
	assignments := regexp.MustCompile(`(?m)^lifetime=([0-9]+)$`).FindAllStringSubmatch(script, -1)
	if len(assignments) != 1 {
		t.Fatal("the signer must declare exactly one lifetime in seconds")
	}
	lifetime, err := strconv.Atoi(assignments[0][1])
	if err != nil || lifetime <= deadline || lifetime > deadline+60 {
		t.Fatalf("setup token lifetime %s must cover the Job deadline %ds with at most 60 seconds of margin", assignments[0][1], deadline)
	}
}

func TestSharedInstanceLoginRestoresPreviousSettings(t *testing.T) {
	script := strings.Join([]string{
		producerFunction(t, "instance_login_providers"),
		producerFunction(t, "instance_login_default_org"),
		producerFunction(t, "reconcile_shared_instance_login"),
	}, "\n")
	for _, mode := range []string{"round-trip", "previous-member", "inherited-disabled", "inherited-enabled",
		"never-activated", "different-provider", "rollback-retry", "unrecorded-baseline", "preflight-only"} {
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
if [[ $mode == preflight-only || $mode == inherited-enabled ]]; then
  reconcile_shared_instance_login on shared preflight
  jq -es --arg key "$key" '
    (.[1] | del(.secret.metadata.annotations[$key])) == .[0] and
    (.[1].secret.metadata.annotations[$key] | fromjson | .member == false and .feature.enabled == true)' \
    "$state.before" "$state" >/dev/null
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
	script := strings.Join([]string{
		producerFunction(t, "customer_directory_page"),
		producerFunction(t, "customer_directory_stamp"),
		producerFunction(t, "read_customer_subjects"),
	}, "\n")
	cmd.Stdin = strings.NewReader(harness + script + "\nread_customer_subjects \"$1\"\n")
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

func TestSharedRollbackLookupUsesTheRecordedSystemProvider(t *testing.T) {
	const harness = `set -euo pipefail
mode=$1
legacy_customer_idp=legacy
idp_name=GitHub
fail() { printf '%s\n' "$1" >&2; exit 1; }
api() {
  [[ $1 == POST && $2 == /admin/v1/idps/templates/_search ]] || fail 'lookup tried to modify the provider'
  jq -e '.queries == [{idpNameQuery:{name:"GitHub",method:"TEXT_QUERY_METHOD_EQUALS"}}]' <<<"$3" >/dev/null
  case $mode in
    missing) printf '{"result":[]}\n';;
    wrong-owner) printf '{"result":[{"id":"shared","name":"GitHub","owner":"IDP_OWNER_TYPE_ORG","config":{"github":{}}}]}\n';;
    wrong-kind) printf '{"result":[{"id":"shared","name":"GitHub","owner":"IDP_OWNER_TYPE_SYSTEM","config":{"oidc":{}}}]}\n';;
    *) printf '{"result":[{"id":"shared","name":"GitHub","owner":"IDP_OWNER_TYPE_SYSTEM","config":{"github":{}}}]}\n';;
  esac
}
`
	function := producerFunction(t, "find_shared_github_for_rollback")
	for _, tt := range []struct {
		name, from, to string
		ok             bool
	}{
		{"forward-journal", "legacy", "shared", true},
		{"reverse-journal", "shared", "legacy", true},
		{"unrelated-journal", "unrelated", "shared", false},
		{"missing", "legacy", "shared", false},
		{"wrong-owner", "legacy", "shared", false},
		{"wrong-kind", "legacy", "shared", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command("bash", "-s", "--", tt.name, tt.from, tt.to)
			cmd.Stdin = strings.NewReader(harness + function +
				"\nfind_shared_github_for_rollback \"$(jq -nc --arg from \"$2\" --arg to \"$3\" '{from:$from,to:$to}')\"\n")
			output, err := cmd.CombinedOutput()
			if (err == nil) != tt.ok || tt.ok && strings.TrimSpace(string(output)) != "shared" {
				t.Fatalf("expected recorded provider success=%t, got %v: %s", tt.ok, err, output)
			}
		})
	}
}

func TestSharedLoginPreflightAndLegacyRepairOrder(t *testing.T) {
	const harness = `set -euo pipefail
fixture=$1
events=$2
customers_org_id=customers
customer_policy={}
customer_idp={}
idp_name=GitHub
github_client_secret=synthetic
github_annotation=unused
shared_portal_setup=off
if [[ $fixture == activation* ]]; then shared_portal_setup=on; fi
log_call() { printf '%s\n' "$*" >>"$events"; }
customer_migration_state() {
  if [[ $fixture == rollback* ]]; then printf '{"phase":"applied","from":"legacy","to":"shared"}\n'; else printf 'null\n'; fi
}
find_shared_github_for_rollback() {
  jq -e '.phase == "applied" and .from == "legacy" and .to == "shared"' <<<"$1" >/dev/null
  log_call shared-lookup
  printf '%s\n' shared
}
ensure_portal_shared_github() {
  log_call shared-template
  [[ $fixture != rollback-template-update-refused ]] || return 1
  shared_customer_idp=shared
}
prepare_portal_operator() {
  log_call "operator $1"
  [[ $fixture != *operator-mismatch ]]
}
reconcile_shared_instance_login() {
  log_call "instance $*"
  [[ $fixture != activation-refused ]]
}
ensure_idp() { log_call legacy-template; idp_id=legacy; }
ensure_login_policy() { log_call "legacy-policy $1 $3"; }
reconcile_customer_login_provider() { log_call "transition $1 $2"; }
`
	for _, tt := range []struct {
		name  string
		calls string
		ok    bool
	}{
		{"off", "legacy-template\nlegacy-policy customers legacy\nshared-template\ninstance off shared\noperator shared\n", true},
		{"off-operator-mismatch", "legacy-template\nlegacy-policy customers legacy\nshared-template\ninstance off shared\noperator shared\n", false},
		{"activation-refused", "shared-template\noperator shared\ninstance on shared preflight\n", false},
		{"activation", "shared-template\noperator shared\ninstance on shared preflight\nlegacy-template\ntransition legacy shared\ninstance on shared\n", true},
		{"rollback-operator-mismatch", "legacy-template\nshared-lookup\ntransition shared legacy\ninstance off shared\nshared-template\noperator shared\n", false},
		{"rollback-template-update-refused", "legacy-template\nshared-lookup\ntransition shared legacy\ninstance off shared\nshared-template\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			events := filepath.Join(t.TempDir(), "events")
			cmd := exec.Command("bash", "-s", "--", tt.name, events)
			cmd.Stdin = strings.NewReader(harness + producerFunction(t, "reconcile_portal_github") + "\nreconcile_portal_github\n")
			if output, err := cmd.CombinedOutput(); (err == nil) != tt.ok {
				t.Fatalf("expected reconciliation success=%t, got %v: %s", tt.ok, err, output)
			}
			calls, err := os.ReadFile(events)
			if err != nil {
				t.Fatal(err)
			}
			if string(calls) != tt.calls {
				t.Fatalf("unexpected reconciliation order:\n%s", calls)
			}
		})
	}
}
