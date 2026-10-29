package emailcredentials_test

import (
	"encoding/json"
	"gopkg.in/yaml.v3"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupRefreshRequiresTheOwnedCompletedSnapshotAndMirror(t *testing.T) {
	for _, scenario := range []string{"complete", "owned-unfinished", "owned-complete", "foreign-unfinished", "jobs-read-failed", "jobs-partial", "active", "image", "pvc", "changed", "create-unknown", "failed", "replacement", "pod-missing"} {
		t.Run(scenario, func(t *testing.T) {
			work := t.TempDir()
			if err := os.Chmod(work, 0700); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(repositoryRoot(), "k8s/bases/infrastructure/vault-backup/cron-job.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			var cron map[string]any
			if yaml.Unmarshal(raw, &cron) != nil {
				t.Fatal("cron fixture")
			}
			meta := cron["metadata"].(map[string]any)
			meta["uid"] = "11111111-1111-4111-8111-111111111111"
			meta["resourceVersion"] = "100"
			meta["labels"] = map[string]any{"kustomize.toolkit.fluxcd.io/name": "infrastructure", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}
			spec := cron["spec"].(map[string]any)
			spec["jobTemplate"].(map[string]any)["metadata"] = map[string]any{"labels": map[string]any{"example.invalid/retain": "yes"}, "annotations": map[string]any{"example.invalid/retain": "yes"}}
			template := spec["jobTemplate"].(map[string]any)["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			volumes := template["volumes"].([]any)
			volumes[0].(map[string]any)["persistentVolumeClaim"].(map[string]any)["claimName"] = "vault-snapshots-recovery"
			mirror := template["containers"].([]any)[0].(map[string]any)
			mirror["env"].([]any)[1].(map[string]any)["value"] = "true"
			if scenario == "active" {
				cron["status"] = map[string]any{"active": []any{map[string]any{"name": "other"}}}
			}
			if scenario == "image" {
				mirror["image"] = "unreviewed.invalid/image:latest"
			}
			if scenario == "pvc" {
				volumes[0].(map[string]any)["persistentVolumeClaim"].(map[string]any)["claimName"] = "other"
			}
			data, err := json.Marshal(cron)
			if err != nil {
				t.Fatal(err)
			}
			if os.WriteFile(filepath.Join(work, "cron.json"), data, 0600) != nil {
				t.Fatal("fixture")
			}
			bin := filepath.Join(work, "bin")
			if os.Mkdir(bin, 0700) != nil {
				t.Fatal("bin")
			}
			fake := `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FIXTURE/calls"
shift 5
case "$1" in
 get)
  case "$2" in
   cronjob)
    if [[ "$SCENARIO" == changed && -f "$FIXTURE/read" ]]; then jq '.metadata.resourceVersion="101"' "$FIXTURE/cron.json"; else cat "$FIXTURE/cron.json"; fi
    touch "$FIXTURE/read" ;;
   jobs)
    if [[ "$SCENARIO" == jobs-read-failed ]];then printf SENSITIVE_FIXTURE_VALUE >&2;exit 2;fi
    jq -n --arg scenario "$SCENARIO" '{apiVersion:"v1",kind:"List",metadata:{continue:(if $scenario=="jobs-partial" then "opaque" else "" end)},items:(if ($scenario=="owned-unfinished" or $scenario=="owned-complete" or $scenario=="foreign-unfinished") then [{apiVersion:"batch/v1",kind:"Job",metadata:{uid:"66666666-6666-4666-8666-666666666666",namespace:"openbao",ownerReferences:[{apiVersion:"batch/v1",kind:"CronJob",name:"vault-snapshot",uid:(if $scenario=="foreign-unfinished" then "99999999-9999-4999-8999-999999999999" else "11111111-1111-4111-8111-111111111111" end),controller:true}]},status:(if $scenario=="owned-complete" then {succeeded:1,conditions:[{type:"Complete",status:"True"}]} else {} end)}] else [] end)}' ;;
   job)
    if [[ "$SCENARIO" == replacement ]];then jq '.metadata.uid="33333333-3333-4333-8333-333333333333"' "$FIXTURE/created.json"; else cat "$FIXTURE/created.json";fi ;;
   pods)
    if [[ "$SCENARIO" == pod-missing ]];then printf '{"items":[]}';else
    jq '{items:[{metadata:{ownerReferences:[{kind:"Job",uid:.metadata.uid,controller:true}]},status:{phase:"Succeeded",initContainerStatuses:[{name:"install-mirror-tools",state:{terminated:{exitCode:0}}},{name:"snapshot",state:{terminated:{exitCode:0}}}],containerStatuses:[{name:"mirror",state:{terminated:{exitCode:0}}}]}}]}' "$FIXTURE/created.json";fi ;;
   *) exit 2 ;;
  esac ;;
 create)
  cat "$3" > "$FIXTURE/planned.json"
  if [[ "$SCENARIO" == create-unknown ]];then printf 'SENSITIVE_FIXTURE_VALUE' >&2;exit 2;fi
  jq '.metadata.uid="22222222-2222-4222-8222-222222222222"|.status={succeeded:1,conditions:[{type:"Complete",status:"True"}]}' "$FIXTURE/planned.json" > "$FIXTURE/created.json"
  if [[ "$SCENARIO" == failed ]];then jq '.status={failed:1,conditions:[{type:"Failed",status:"True"}]}' "$FIXTURE/created.json" > "$FIXTURE/failed.json";mv "$FIXTURE/failed.json" "$FIXTURE/created.json";fi
  cat "$FIXTURE/created.json" ;;
 wait) exit 0 ;;
 *) printf 'SENSITIVE_FIXTURE_VALUE' >&2;exit 2 ;;
esac
`
			if os.WriteFile(filepath.Join(bin, "kubectl"), []byte(fake), 0700) != nil {
				t.Fatal("kubectl fixture")
			}
			cmd := exec.Command("bash", filepath.Join(repositoryRoot(), "scripts/refresh-email-recovery-backup.sh"))
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FIXTURE="+work, "SCENARIO="+scenario, "EMAIL_BACKUP_WORK_DIR="+work, "GITHUB_EVENT_NAME=workflow_dispatch", "GITHUB_REF=refs/heads/main", "GITHUB_REPOSITORY=devantler-tech/platform", "GITHUB_RUN_ID=12345", "GITHUB_RUN_ATTEMPT=1", "EMAIL_BACKUP_CONFIRM=refresh-email-recovery-backup")
			output, err := cmd.CombinedOutput()
			success := scenario == "complete" || scenario == "owned-complete" || scenario == "foreign-unfinished"
			if (err == nil) != success {
				t.Fatalf("scenario result: error=%v output=%s", err, output)
			}
			if strings.Contains(string(output), "SENSITIVE_FIXTURE_VALUE") || strings.Contains(string(output), "cloudflarestorage") || strings.Contains(string(output), "11111111") {
				t.Fatal("private input escaped")
			}
			if success && !strings.Contains(string(output), "EMAIL_BACKUP_REFRESH=COMPLETED snapshot=1 mirror=1 restore=UNVERIFIED") {
				t.Fatal("missing bounded completion receipt")
			}
			calls, _ := os.ReadFile(filepath.Join(work, "calls"))
			if strings.Contains(string(calls), " delete ") {
				t.Fatal("unknown resource was deleted")
			}
			early := scenario == "owned-unfinished" || scenario == "jobs-read-failed" || scenario == "jobs-partial" || scenario == "active" || scenario == "image" || scenario == "pvc" || scenario == "changed"
			if early && strings.Contains(string(calls), " create ") {
				t.Fatal("unsafe source created a job")
			}
			if success {
				planned, err := os.ReadFile(filepath.Join(work, "planned.json"))
				if err != nil {
					t.Fatal(err)
				}
				var job map[string]any
				if json.Unmarshal(planned, &job) != nil {
					t.Fatal("job")
				}
				jm := job["metadata"].(map[string]any)
				if jm["name"] != "vault-snapshot-refresh-12345-1" || jm["namespace"] != "openbao" {
					t.Fatal("job identity")
				}
				if jm["labels"].(map[string]any)["example.invalid/retain"] != "yes" || jm["annotations"].(map[string]any)["example.invalid/retain"] != "yes" {
					t.Fatal("reviewed template metadata lost")
				}
				owner := jm["ownerReferences"].([]any)[0].(map[string]any)
				if owner["uid"] != meta["uid"] || owner["controller"] != true {
					t.Fatal("CronJob concurrency ownership lost")
				}
			}
		})
	}
}

func TestBackupRefreshWorkflowRequiresTheConfirmedNativeMainDispatch(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repositoryRoot(), ".github/workflows/refresh-email-recovery-backup.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		On          map[string]any
		Permissions map[string]string
		Concurrency struct {
			Group  string
			Cancel bool `yaml:"cancel-in-progress"`
			Queue  string
		}
		Jobs map[string]struct {
			Environment string
			Permissions map[string]string
			Steps       []verificationStep
		}
	}
	if yaml.Unmarshal(data, &w) != nil || len(w.On) != 1 || w.On["workflow_dispatch"] == nil || len(w.Permissions) != 0 || w.Concurrency.Group != "prod-deploy" || w.Concurrency.Cancel || w.Concurrency.Queue != "max" || len(w.Jobs) != 1 {
		t.Fatal("unprotected refresh workflow")
	}
	job, ok := w.Jobs["refresh"]
	if !ok || job.Environment != "prod" || len(job.Permissions) != 1 || job.Permissions["contents"] != "read" || len(job.Steps) < 2 {
		t.Fatal("refresh authorization")
	}
	guard := job.Steps[0]
	if guard.Uses != "" || len(guard.Env) != 1 || guard.Env["CONFIRM"] != "${{ inputs.confirm }}" {
		t.Fatal("guard must run before checkout and credentials")
	}
	for _, bad := range []string{"", "event", "ref", "repo", "attempt", "runner", "confirmation"} {
		t.Run(bad, func(t *testing.T) {
			e, ref, repo, attempt, runner, confirm := "workflow_dispatch", "refs/heads/main", "devantler-tech/platform", "1", "github-hosted", "refresh-email-recovery-backup"
			switch bad {
			case "event":
				e = "pull_request"
			case "ref":
				ref = "refs/heads/other"
			case "repo":
				repo = "other/repo"
			case "attempt":
				attempt = "2"
			case "runner":
				runner = "self-hosted"
			case "confirmation":
				confirm = ""
			}
			cmd := exec.Command("bash", "-c", guard.Run)
			cmd.Env = append(os.Environ(), "GITHUB_EVENT_NAME="+e, "GITHUB_REF="+ref, "GITHUB_REPOSITORY="+repo, "GITHUB_RUN_ATTEMPT="+attempt, "RUNNER_ENVIRONMENT="+runner, "CONFIRM="+confirm)
			output, err := cmd.CombinedOutput()
			if (err == nil) != (bad == "") {
				t.Fatalf("guard result %v: %s", err, output)
			}
		})
	}
}

// The normalizer's mask command must reach Actions even when endpoint selection fails.
func TestStorageRecoveryCallersPreserveEndpointMaskCommands(t *testing.T) {
	for _, name := range []string{"prepare-email-storage-key.yaml", "verify-email-storage-key-recovery.yaml", "refresh-email-recovery-backup.yaml"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(repositoryRoot(), ".github/workflows", name))
			if err != nil {
				t.Fatal(err)
			}
			var w struct {
				Jobs map[string]struct{ Steps []verificationStep }
			}
			if yaml.Unmarshal(raw, &w) != nil {
				t.Fatal("workflow")
			}
			run := ""
			for _, job := range w.Jobs {
				for _, step := range job.Steps {
					if strings.Contains(step.Run, "./scripts/use-prod-stable-api-endpoint.sh") {
						if run != "" {
							t.Fatal("multiple privileged callers")
						}
						run = step.Run
					}
				}
			}
			if run == "" {
				t.Fatal("missing endpoint normalizer")
			}
			work := t.TempDir()
			bin := filepath.Join(work, "bin")
			scripts := filepath.Join(work, "scripts")
			ram := filepath.Join(work, "ram")
			for _, dir := range []string{bin, scripts, ram} {
				if os.Mkdir(dir, 0700) != nil {
					t.Fatal("workspace")
				}
			}
			stubs := map[string]string{
				filepath.Join(bin, "findmnt"):                             "#!/bin/sh\nprintf 'tmpfs\\n'\n",
				filepath.Join(bin, "git"):                                 "#!/bin/sh\nprintf '%s\\trefs/heads/main\\n' \"$GITHUB_SHA\"\n",
				filepath.Join(scripts, "use-prod-stable-api-endpoint.sh"): "#!/bin/sh\nprintf '%s\\n' '::add-mask::fixture-private-endpoint' >&2\nexit 2\n",
			}
			for path, body := range stubs {
				if os.WriteFile(path, []byte(body), 0700) != nil {
					t.Fatal("stub")
				}
			}
			run = strings.ReplaceAll(run, "/dev/shm", ram)
			cmd := exec.Command("bash", "-c", run)
			cmd.Dir = work
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "GITHUB_SHA="+strings.Repeat("a", 40), "GITHUB_EVENT_NAME=workflow_dispatch", "GITHUB_REF=refs/heads/main", "GITHUB_REPOSITORY=devantler-tech/platform", "GITHUB_RUN_ID=12345", "GITHUB_RUN_ATTEMPT=1", "RUNNER_ENVIRONMENT=github-hosted", "RUNNER_TEMP="+work, "KUBE_CONFIG=synthetic-kubeconfig", "HCLOUD_TOKEN=synthetic-token")
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatal("failed endpoint selection was accepted")
			}
			if !strings.Contains(string(output), "::add-mask::fixture-private-endpoint") {
				t.Fatalf("endpoint mask swallowed by caller: %s", output)
			}
		})
	}
}
