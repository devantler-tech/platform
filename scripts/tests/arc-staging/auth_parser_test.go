package arcstaging_test

import (
	"reflect"
	"testing"
)

func TestAuthorizationParserRequiresOneLiteralWrite(t *testing.T) {
	const role = "bao write auth/kubernetes/role/reader policies=readonly ttl=1h"
	for _, fixture := range []struct {
		name, source string
	}{
		{"missing", "printf unrelated"},
		{"duplicate", role + "\n" + role},
		{"command substitution", "bao write auth/kubernetes/role/reader policies=$(printf readonly)"},
		{"variable expansion", "bao write auth/kubernetes/role/reader policies=$POLICY"},
		{"assignment", "TOKEN=value " + role},
		{"background", role + " &"},
		{"negation", "! " + role},
		{"output redirection", role + " > /tmp/output"},
		{"pipeline", role + " | tee /tmp/output"},
		{"conditional", role + " && false"},
		{"heredoc expansion", role + " <<POLICY\n$(printf payload)\nPOLICY\n"},
		{"malformed", role + " '"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if _, _, err := parseBaoWrite(fixture.source, "bao", "write", "auth/kubernetes/role/reader"); err == nil {
				t.Fatal("accepted a missing, ambiguous or non-literal authorization write")
			}
		})
	}
	args, payload, err := parseBaoWrite(role, "bao", "write", "auth/kubernetes/role/reader")
	want := []string{"write", "auth/kubernetes/role/reader", "policies=readonly", "ttl=1h"}
	if err != nil || !reflect.DeepEqual(args, want) || payload != "" {
		t.Fatalf("literal role write: args=%v payload=%q err=%v", args, payload, err)
	}
}

func TestAuthorizationParserReadsQuotedHeredocsWithoutEvaluation(t *testing.T) {
	const source = "bao policy write readonly - <<'POLICY'\npath \"secret/data/example\" { capabilities = [\"read\"] }\nPOLICY\n"
	args, payload, err := parseBaoWrite(source, "bao", "policy", "write", "readonly")
	wantArgs := []string{"policy", "write", "readonly", "-"}
	const wantPayload = "path \"secret/data/example\" { capabilities = [\"read\"] }\n"
	if err != nil || !reflect.DeepEqual(args, wantArgs) || payload != wantPayload {
		t.Fatalf("literal policy write: args=%v payload=%q err=%v", args, payload, err)
	}
}
