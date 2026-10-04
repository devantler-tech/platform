package main

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"strings"
	"testing"
)

func TestCoverage(t *testing.T) {
	tests := []struct{ name, rules, want string }{
		{"complete", "toFQDNs: [{matchName: api.example.test}, {matchName: sts.example.test}]\n    toPorts: [{serverNames: [api.example.test, sts.example.test]}]", ""},
		{"missing", "toFQDNs: [{matchName: api.example.test}, {matchName: sts.example.test}]\n    toPorts: [{serverNames: [api.example.test]}]", "sts.example.test"},
		{"unpinned", "toFQDNs: [{matchName: api.example.test}]\n    toPorts: [{ports: [{port: '443'}]}]", ""},
		{"explicit-empty", "toFQDNs: [{matchName: api.example.test}]\n    toPorts: [{serverNames: []}]", "api.example.test"},
		{"other-rule-cannot-mask", "toFQDNs: [{matchName: api.example.test}]\n    toPorts: [{serverNames: [other.example.test]}]\n  - toFQDNs: [{matchName: other.example.test}]\n    toPorts: [{serverNames: [api.example.test, other.example.test]}]", "api.example.test"},
		{"other-port-cannot-mask", "toFQDNs: [{matchName: api.example.test}]\n    toPorts: [{serverNames: [other.example.test]}, {serverNames: [api.example.test]}]", "api.example.test"},
		{"malformed-names", "toFQDNs: [{matchName: api.example.test}]\n    toPorts: [{serverNames: invalid}]", "serverNames must be a sequence"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := "kind: CiliumNetworkPolicy\nmetadata: {name: fixture}\nspec:\n  egress:\n  - " + tt.rules + "\n"
			count, err := check(strings.NewReader(input), "fixture.yaml")
			if tt.want == "" {
				if err != nil || count != 1 {
					t.Fatalf("count=%d err=%v", count, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("wanted %q, got %v", tt.want, err)
			}
		})
	}
}
func TestMultipleDocumentsAndSpecs(t *testing.T) {
	input := "kind: ConfigMap\ndata: {irrelevant: value}\n---\nkind: CiliumClusterwideNetworkPolicy\nmetadata: {name: wide}\nspecs:\n- egress:\n  - toFQDNs: [{matchName: missing.example.test}]\n    toPorts: [{serverNames: [present.example.test]}]\n"
	_, err := check(strings.NewReader(input), "multi.yaml")
	if err == nil || !strings.Contains(err.Error(), "wide specs[0] egress[0]") || !strings.Contains(err.Error(), "missing.example.test") {
		t.Fatalf("got %v", err)
	}
}
func TestUnrelatedDocument(t *testing.T) {
	count, err := check(strings.NewReader("kind: Deployment\nspec: {egress: unrelated}\n"), "other.yaml")
	if err != nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}
func TestCIWiring(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyWiring(data); err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{"go run ./scripts/guard-cilium-sni k8s", "'scripts/guard-cilium-sni/**'"} {
		broken := strings.Replace(string(data), old, "removed", 1)
		if err := verifyWiring([]byte(broken)); err == nil {
			t.Fatalf("accepted missing CI wiring: %s", old)
		}
	}
}
func verifyWiring(data []byte) error {
	type step struct {
		ID       string               `yaml:"id"`
		Run      string               `yaml:"run"`
		If       string               `yaml:"if"`
		Continue yaml.Node            `yaml:"continue-on-error"`
		With     map[string]yaml.Node `yaml:"with"`
	}
	var workflow struct {
		Jobs map[string]struct {
			If    string   `yaml:"if"`
			Needs []string `yaml:"needs"`
			Steps []step   `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return err
	}
	job := workflow.Jobs["validate"]
	wantedIf := "github.event_name == 'pull_request' && (needs.changes.outputs.k8s == 'true' || needs.changes.outputs.bridge_validation == 'true')"
	if strings.Join(strings.Fields(job.If), " ") != wantedIf || len(job.Needs) != 1 || job.Needs[0] != "changes" {
		return fmt.Errorf("manifest validation job is no longer reachable through changes")
	}
	found := false
	for _, s := range job.Steps {
		if strings.TrimSpace(s.Run) == "go test ./scripts/guard-cilium-sni\ngo run ./scripts/guard-cilium-sni k8s" && s.If == "needs.changes.outputs.k8s == 'true'" && s.Continue.Kind == 0 {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("missing enforcing SNI validation step")
	}
	mergeGroupCovered := false
	for _, s := range workflow.Jobs["changes"].Steps {
		if strings.TrimSpace(s.Run) == "go test ./scripts/guard-cilium-sni\ngo run ./scripts/guard-cilium-sni k8s" && s.If == "" && s.Continue.Kind == 0 {
			mergeGroupCovered = true
		}
	}
	if !mergeGroupCovered {
		return fmt.Errorf("missing unconditional merge-group SNI guard")
	}
	var paths map[string][]string
	for _, s := range workflow.Jobs["changes"].Steps {
		if s.ID == "filter" {
			n := s.With["filters"]
			if err := yaml.Unmarshal([]byte(n.Value), &paths); err != nil {
				return err
			}
		}
	}
	covered := false
	for _, p := range paths["k8s"] {
		if p == "scripts/guard-cilium-sni/**" {
			covered = true
		}
	}
	if !covered {
		return fmt.Errorf("SNI guard changes do not reach the k8s path filter")
	}
	return nil
}
func TestKustomizePatchDocument(t *testing.T) {
	count, err := check(strings.NewReader("- op: add\n  path: /spec/template\n  value: {}\n"), "patch.yaml")
	if err != nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestYAMLAliases(t *testing.T) {
	input := "kind: CiliumNetworkPolicy\nmetadata: {name: aliased}\nspec:\n  egress:\n  - toFQDNs: [{matchName: api.example.test}]\n    toPorts:\n    - serverNames: &names [&name api.example.test]\n    - serverNames: *names\n    - serverNames: [*name]\n"
	count, err := check(strings.NewReader(input), "aliases.yaml")
	if err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestPolicyInListCannotHide(t *testing.T) {
	for _, kind := range []string{"List", "CiliumNetworkPolicyList", "CiliumClusterwideNetworkPolicyList"} {
		input := "kind: CiliumNetworkPolicy\nmetadata: {name: outside}\nspec: {}\n---\nkind: " + kind + "\nitems:\n- kind: List\n  items:\n  - kind: CiliumNetworkPolicy\n    metadata: {name: inside}\n    spec:\n      egress:\n      - toFQDNs: [{matchName: missing.example.test}]\n        toPorts: [{serverNames: [present.example.test]}]\n"
		_, err := check(strings.NewReader(input), "lists.yaml")
		if err == nil || !strings.Contains(err.Error(), "missing.example.test") {
			t.Fatalf("%s: got %v", kind, err)
		}
	}
}
