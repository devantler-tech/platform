package portaldirectory

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func membershipResponse(rows []any, total any) map[string]any {
	return map[string]any{
		"pagination":     map[string]any{"totalResult": total, "appliedLimit": "2"},
		"administrators": rows,
	}
}

func organizationMembership(user, org string, roles ...string) map[string]any {
	return map[string]any{
		"user":         map[string]any{"id": user, "organizationId": "owning-org"},
		"organization": map[string]any{"id": org},
		"roles":        roles,
	}
}

func readMemberships(t *testing.T, response any) (string, error) {
	t.Helper()
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(t.TempDir(), "response.json")
	if err := os.WriteFile(fixture, data, 0o600); err != nil {
		t.Fatal(err)
	}
	harness := `set -euo pipefail
fixture=$1
portal_account_id=portal-user
customers_org_id=customers
portal_account=business-portal
fail() { printf '%s\n' "$1" >&2; exit 1; }
connect() {
  [[ $1 == zitadel.internal_permission.v2.InternalPermissionService/ListAdministrators ]] || fail 'organization-scoped endpoint is incomplete'
  [[ -z ${api_org:-} ]] || fail 'organization header must be absent'
  jq -e '.pagination == {offset:"0",limit:2,asc:true} and
    .filters == [{inUserIdsFilter:{ids:["portal-user"]}}] and
    (keys | sort) == ["filters","pagination"]' <<< "$2" >/dev/null || fail 'unexpected membership query'
  cat "$fixture"
}
`
	cmd := exec.Command("bash", "-s", "--", fixture)
	cmd.Stdin = strings.NewReader(harness + producerFunction(t, "read_portal_memberships") + "\nread_portal_memberships\n")
	output, runErr := cmd.CombinedOutput()
	return strings.TrimSpace(string(output)), runErr
}

func TestMembershipSearchAcceptsOnlyTheCompleteIntendedGrant(t *testing.T) {
	row := organizationMembership("portal-user", "customers", "ORG_USER_MANAGER")
	for _, total := range []any{"1", 1} {
		output, err := readMemberships(t, membershipResponse([]any{row}, total))
		if err != nil {
			t.Fatalf("complete intended membership failed: %v: %s", err, output)
		}
		if output != `[{"orgId":"customers","iam":false,"projectId":"","roles":["ORG_USER_MANAGER"]}]` {
			t.Fatalf("unexpected normalized membership: %s", output)
		}
	}
}

func TestMembershipSearchAcceptsAProvedEmptyResult(t *testing.T) {
	for _, response := range []any{
		membershipResponse([]any{}, "0"),
		map[string]any{"pagination": map[string]any{"appliedLimit": "2"}}, // Native protobuf zero/repeated omission.
	} {
		output, err := readMemberships(t, response)
		if err != nil || output != "[]" {
			t.Fatalf("empty result failed: %v: %s", err, output)
		}
	}
}

func TestMembershipSearchRejectsExtraHiddenOrUnexaminedPermissions(t *testing.T) {
	intended := organizationMembership("portal-user", "customers", "ORG_USER_MANAGER")
	extra := organizationMembership("portal-user", "other-org", "ORG_OWNER")
	instance := map[string]any{"user": map[string]any{"id": "portal-user"}, "instance": true, "roles": []string{"IAM_OWNER"}}
	for name, response := range map[string]any{
		"extra organization":        membershipResponse([]any{intended, extra}, "2"),
		"extra beyond first page":   membershipResponse([]any{intended}, "2"),
		"hidden permission":         membershipResponse([]any{}, "1"),
		"wrong user":                membershipResponse([]any{organizationMembership("other-user", "customers", "ORG_USER_MANAGER")}, "1"),
		"wrong granted org":         membershipResponse([]any{extra}, "1"),
		"extra org role":            membershipResponse([]any{organizationMembership("portal-user", "customers", "ORG_USER_MANAGER", "ORG_OWNER")}, "1"),
		"instance permission":       membershipResponse([]any{instance}, "1"),
		"count missing rows":        membershipResponse([]any{intended}, "0"),
		"duplicate rows":            membershipResponse([]any{intended, intended}, "1"),
		"null rows":                 membershipResponse(nil, "0"),
		"negative count":            membershipResponse([]any{}, "-1"),
		"fractional count":          membershipResponse([]any{}, 0.5),
		"null count":                membershipResponse([]any{}, nil),
		"missing pagination":        map[string]any{"administrators": []any{}},
		"zero page limit":           map[string]any{"pagination": map[string]any{"totalResult": "0", "appliedLimit": "0"}},
		"missing applied limit":     map[string]any{"pagination": map[string]any{"totalResult": "0"}},
		"unverified response shape": map[string]any{"result": []any{}},
	} {
		t.Run(name, func(t *testing.T) {
			if output, err := readMemberships(t, response); err == nil {
				t.Fatalf("incomplete or excessive permission proof accepted: %s", output)
			}
		})
	}
}
