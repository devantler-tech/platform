package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestActualQueuePreflight runs on both PRs and merge groups. A post-deploy
// membership check cannot stop a diagnostic rerun of an already-evicted group.
func TestActualQueuePreflight(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateQueuePreflight(data); err != nil {
		t.Fatal(err)
	}
}

// validateQueuePreflight checks the real invocation, receipt and token bindings
// rather than accepting a lookalike comment or a guard after production access.
func validateQueuePreflight(data []byte) error {
	var workflow struct {
		Jobs map[string]struct {
			If          string            `yaml:"if"`
			Continue    any               `yaml:"continue-on-error"`
			Permissions map[string]string `yaml:"permissions"`
			Outputs     map[string]string `yaml:"outputs"`
			Steps       []struct {
				ID       string            `yaml:"id"`
				If       string            `yaml:"if"`
				Run      string            `yaml:"run"`
				Uses     string            `yaml:"uses"`
				Continue any               `yaml:"continue-on-error"`
				Env      map[string]string `yaml:"env"`
				With     map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return err
	}
	changes, deploy := workflow.Jobs["changes"], workflow.Jobs["deploy-prod"]
	if changes.If != "" || changes.Continue != nil || deploy.Continue != nil {
		return errQueueContract("jobs must not skip or suppress the receipt")
	}
	if changes.Outputs["queue_receipt"] != "${{ steps.queue-receipt.outputs.receipt }}" {
		return errQueueContract("missing queue receipt output")
	}
	for _, job := range []string{"changes", "deploy-prod"} {
		if workflow.Jobs[job].Permissions["pull-requests"] != "read" || workflow.Jobs[job].Permissions["contents"] != "read" {
			return errQueueContract("missing read-only queue permissions")
		}
		guard, checkout, setup, composite := -1, -1, -1, -1
		for i, step := range workflow.Jobs[job].Steps {
			if strings.HasPrefix(step.Uses, "actions/checkout@") {
				checkout = i
			}
			if strings.HasPrefix(step.Uses, "actions/setup-go@") && step.With["go-version-file"] == "go.mod" {
				setup = i
			}
			if step.Uses == deployCompositePath {
				if composite != -1 || job == "changes" {
					return errQueueContract("production access must occur exactly once after the guard")
				}
				composite = i
			}
			if step.ID != "queue-receipt" && step.ID != "queue-preflight" {
				continue
			}
			if guard != -1 {
				return errQueueContract("duplicate queue guard")
			}
			guard = i
			mode := "capture"
			condition := "github.event_name == 'merge_group'"
			if job == "deploy-prod" {
				mode, condition = "check", ""
			}
			expectedID := "queue-receipt"
			if job == "deploy-prod" {
				expectedID = "queue-preflight"
			}
			if step.ID != expectedID {
				return errQueueContract("guard has the wrong output identity")
			}
			if step.If != condition || step.Continue != nil || strings.TrimSpace(step.Run) != "go run ./scripts/verify-merge-group-queued "+mode || step.Env["GH_TOKEN"] != "${{ github.token }}" {
				return errQueueContract("guard invocation can skip, suppress or misread queue state")
			}
			if mode == "check" && step.Env["QUEUE_RECEIPT"] != "${{ needs.changes.outputs.queue_receipt }}" {
				return errQueueContract("lost initial receipt")
			}
		}
		if guard < 0 || checkout < 0 || setup < checkout || guard <= setup {
			return errQueueContract("guard must follow checkout and pinned Go setup")
		}
		if job == "changes" && guard != setup+1 {
			return errQueueContract("capture must precede the helper suite")
		}
		if job == "deploy-prod" && composite != guard+1 {
			return errQueueContract("guard must immediately precede production access")
		}
	}
	return nil
}

func TestQueuePreflightWiringMutations(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, from, to string }{
		{"missing output", "      queue_receipt: ${{ steps.queue-receipt.outputs.receipt }}", "      queue_receipt: ''"},
		{"missing source", "${{ needs.changes.outputs.queue_receipt }}", "${{ github.sha }}"},
		{"wrong token", "GH_TOKEN: ${{ github.token }}", "GH_TOKEN: ${{ secrets.OTHER_TOKEN }}"},
		{"suppressed", "        id: queue-preflight", "        continue-on-error: true\n        id: queue-preflight"},
		{"skipped", "        id: queue-preflight", "        if: ${{ false }}\n        id: queue-preflight"},
		{"wrong mode", "go run ./scripts/verify-merge-group-queued check", "go run ./scripts/verify-merge-group-queued capture"},
		{"tolerated", "go run ./scripts/verify-merge-group-queued check", "go run ./scripts/verify-merge-group-queued check || true"},
		{"not immediate", "        run: go run ./scripts/verify-merge-group-queued check", "        run: go run ./scripts/verify-merge-group-queued check\n\n      - run: echo unsafe-intervening-step"},
		{"wrong output id", "        id: queue-receipt", "        id: queue-preflight"},
		{"extra deployment before guard", "      - name: 🧾 Refuse an evicted or replaced merge group", "      - uses: ./.github/actions/deploy-prod\n\n      - name: 🧾 Refuse an evicted or replaced merge group"},
		{"lost permission", "pull-requests: read # verify the same queue entry before production access", "pull-requests: none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := strings.ReplaceAll(string(data), tc.from, tc.to)
			if mutated == string(data) {
				t.Fatal("mutation changed nothing")
			}
			if err := validateQueuePreflight([]byte(mutated)); err == nil {
				t.Fatal("accepted broken pre-production guard")
			}
		})
	}
}

type errQueueContract string

func (e errQueueContract) Error() string { return string(e) }
