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

func publishCredential(t *testing.T, organization, acceptance, patchMode string) (map[string]string, string, error) {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "secret.json")
	events := filepath.Join(dir, "events")
	initial := map[string]any{"data": map[string]string{
		"token":    base64.StdEncoding.EncodeToString([]byte("synthetic-old-token")),
		"token-id": base64.StdEncoding.EncodeToString([]byte("old-id")),
		"org-id":   base64.StdEncoding.EncodeToString([]byte("old-org")),
	}}
	encoded, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(events, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	harness := `set -euo pipefail
state=$1
events=$2
customers_org_id=$3
acceptance=$4
patch_mode=$5
portal_token_secret=business-portal-zitadel-token
portal_account=business-portal
export PORTAL_TOKEN=synthetic-new-token PORTAL_TOKEN_ID=new-id
fail() { printf '%s\n' "$1" >&2; exit 1; }
sleep() { :; }
secret_json() { cat "$state"; }
portal_token_works() {
  [[ $1 == "$PORTAL_TOKEN" ]] || fail 'wrong credential proof'
  printf 'proof\n' >> "$events"
  [[ $acceptance == accept ]]
}
patch_secret() {
  [[ $2 == "$portal_token_secret" ]] || fail 'wrong destination'
  printf 'patch\n' >> "$events"
  [[ $patch_mode != ignore ]] || return 0
  local patch=$1
  if [[ $patch_mode == ignore-org ]]; then patch=$(jq 'del(.data["org-id"])' <<< "$patch"); fi
  if [[ $patch_mode == ignore-id ]]; then patch=$(jq 'del(.data["token-id"])' <<< "$patch"); fi
  jq -s '.[0] * .[1]' "$state" <(printf '%s' "$patch") > "$state.next"
  mv "$state.next" "$state"
}
`
	cmd := exec.Command("bash", "-s", "--", state, events, organization, acceptance, patchMode)
	cmd.Stdin = strings.NewReader(harness + producerFunction(t, "publish_portal_token") + "\npublish_portal_token\n")
	output, runErr := cmd.CombinedOutput()
	if strings.Contains(string(output), "synthetic-") {
		t.Fatal("credential publication exposed a token")
	}
	final, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(final, &result); err != nil {
		t.Fatal(err)
	}
	sequence, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	return result.Data, string(sequence), runErr
}

func TestPublicationProvesThenWritesTheMatchingCredentialAtomically(t *testing.T) {
	data, events, err := publishCredential(t, "new-org", "accept", "apply")
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"token": "synthetic-new-token", "token-id": "new-id", "org-id": "new-org"} {
		if data[key] != base64.StdEncoding.EncodeToString([]byte(value)) {
			t.Fatalf("publication did not keep %s", key)
		}
	}
	if events != "proof\npatch\n" {
		t.Fatalf("credential must be proved before a single atomic patch: %q", events)
	}
}

func TestPublicationRefusesAnUnprovedTokenWithoutChangingTheOldPair(t *testing.T) {
	data, events, err := publishCredential(t, "new-org", "reject", "apply")
	if err == nil || strings.Contains(events, "patch") {
		t.Fatal("a refused credential must never be published")
	}
	if data["token"] != base64.StdEncoding.EncodeToString([]byte("synthetic-old-token")) ||
		data["org-id"] != base64.StdEncoding.EncodeToString([]byte("old-org")) {
		t.Fatal("a refused token changed the old credential pair")
	}
}

func TestPublicationRequiresAllThreeReadBackFields(t *testing.T) {
	for _, mode := range []string{"ignore", "ignore-org", "ignore-id"} {
		t.Run(mode, func(t *testing.T) {
			_, _, err := publishCredential(t, "new-org", "accept", mode)
			if err == nil {
				t.Fatal("incomplete publication must fail")
			}
		})
	}
}

func TestPublicationRefusesAnUnknownOrganization(t *testing.T) {
	_, events, err := publishCredential(t, "", "accept", "apply")
	if err == nil || strings.Contains(events, "patch") {
		t.Fatal("unknown organization must not be published")
	}
}
