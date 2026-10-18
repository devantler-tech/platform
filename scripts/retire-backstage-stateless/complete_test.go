package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

type completionFixture struct {
	portal       *portalFixture
	record       string
	receipt      map[string]any
	handoffPaths []string
	handoffFiles map[string][]byte
	sourceChecks int
	cleared      bool
	patches      int
	sleeps       int
	readFailures int
	onSleep      func()
	postMutation func()
}

func completedRetirementFixture(t *testing.T) *completionFixture {
	t.Helper()
	completionRunner(t)
	f := retirementFixture(t)
	f.deleted = true
	f.historyStatus = "uninstalled"
	f.fenceInstalled = true
	db := item(f.live, 2)
	object(db["spec"])["instances"] = 2
	object(db["status"])["readyInstances"] = 2
	object(db["status"])["currentPrimary"] = "backstage-db-4"
	object(db["status"])["targetPrimary"] = "backstage-db-4"
	object(db["status"])["conditions"] = []any{
		map[string]any{"type": "Ready", "status": "True"},
		map[string]any{"type": "ContinuousArchiving", "status": "True", "reason": "ContinuousArchivingSuccess"},
	}
	claim2, _ := decode(encode(t, item(f.live, 3)))
	object(claim2["metadata"])["name"] = "backstage-db-4"
	object(claim2["metadata"])["uid"] = "claim-uid-4"
	object(claim2["spec"])["volumeName"] = "retained-volume-4"
	object(claim2["metadata"])["labels"] = map[string]any{"cnpg.io/cluster": "backstage-db", "role": "primary", "cnpg.io/instanceRole": "primary"}
	object(item(f.live, 3)["metadata"])["labels"] = map[string]any{"cnpg.io/cluster": "backstage-db", "role": "replica", "cnpg.io/instanceRole": "replica"}
	pod2, _ := decode(encode(t, item(f.live, 4)))
	object(pod2["metadata"])["name"] = "backstage-db-4"
	object(pod2["metadata"])["uid"] = "database-pod-uid-4"
	object(pod2["metadata"])["labels"] = map[string]any{"cnpg.io/cluster": "backstage-db", "role": "primary", "cnpg.io/instanceRole": "primary"}
	object(item(f.live, 4)["metadata"])["labels"] = map[string]any{"cnpg.io/cluster": "backstage-db", "role": "replica", "cnpg.io/instanceRole": "replica"}
	f.live["items"] = append(f.live["items"].([]any), claim2, pod2)
	pv2, _ := decode(encode(t, item(f.volumes, 0)))
	object(pv2["metadata"])["name"] = "retained-volume-4"
	object(pv2["metadata"])["uid"] = "volume-uid-4"
	object(field(pv2, "spec", "claimRef"))["name"] = "backstage-db-4"
	object(field(pv2, "spec", "claimRef"))["uid"] = "claim-uid-4"
	f.volumes["items"] = append(f.volumes["items"].([]any), pv2)

	originalRevision := "latest@sha256:" + strings.Repeat("d", 64)
	currentRevision := os.Getenv("PORTAL_RETIREMENT_REVISION")
	apps := item(f.layers, 0)
	object(apps["status"])["lastAppliedRevision"] = originalRevision
	object(apps["status"])["lastAttemptedRevision"] = currentRevision
	object(apps["status"])["conditions"] = journalBlockedConditions()
	for _, name := range []string{"bootstrap", "flux-system", "infrastructure", "infrastructure-controllers"} {
		root, _ := decode(encode(t, apps))
		object(root["metadata"])["name"] = name
		object(root["metadata"])["uid"] = name + "-uid"
		object(root["status"])["lastAppliedRevision"] = currentRevision
		object(root["status"])["lastAttemptedRevision"] = currentRevision
		object(root["status"])["conditions"] = readyConditions()
		f.layers["items"] = append(f.layers["items"].([]any), root)
	}

	record := map[string]any{
		"version": 1, "phase": "fenced", "namespaceUID": "namespace-uid",
		"sourceSHA": strings.Repeat("c", 40), "revision": originalRevision, "run": "42",
		"targets": []any{
			map[string]any{"kind": "HelmRelease", "name": "backstage", "uid": "release-uid"},
			map[string]any{"kind": "HelmChart", "name": "backstage-backstage", "uid": "chart-uid"},
			map[string]any{"kind": "Deployment", "name": "backstage", "uid": "deployment-uid"},
			map[string]any{"kind": "Service", "name": "backstage", "uid": "service-uid"},
			map[string]any{"kind": "ConfigMap", "name": "backstage-app-config", "uid": "configmap-uid"},
		},
		"retiringUIDs": []any{"chart-uid", "configmap-uid", "deployment-uid", "release-uid", "service-uid"},
		"retained":     []any{}, "latestHistoryUID": "history-uid", "helmWriterUID": "helm-writer-uid", "cnpgWriterUID": "cnpg-controller-uid",
	}
	b, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	object(field(f.namespace, "metadata", "annotations"))[fenceAnnotation] = string(b)
	receipt := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "backstage-retirement-42", "namespace": "flux-system", "uid": "receipt-uid", "resourceVersion": "1"}, "data": map[string]any{"source": strings.Repeat("c", 40), "revision": originalRevision, "run": "42", "attempt": "1", "scope": "stateless-only", "releaseUID": "release-uid", "chartUID": "chart-uid", "dataRetirement": "UNASSESSED"}}
	return &completionFixture{portal: f, record: string(b), receipt: receipt, handoffPaths: []string{"scripts/retire-backstage-stateless/complete.go"}, handoffFiles: map[string][]byte{}}
}

func completionRunner(t *testing.T) {
	t.Helper()
	protectedRunner(t)
	t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
	t.Setenv("GITHUB_WORKFLOW_REF", "devantler-tech/platform/.github/workflows/complete-backstage-retirement.yaml@refs/heads/main")
	t.Setenv("GITHUB_RUN_ID", "999")
	t.Setenv("GITHUB_RUN_ATTEMPT", "1")
	t.Setenv("GITHUB_SHA", strings.Repeat("a", 40))
	t.Setenv("PORTAL_RETIREMENT_CONFIRM", "COMPLETE BACKSTAGE RETIREMENT JOURNAL ONLY")
	t.Setenv("PORTAL_RETIREMENT_REVISION", "latest@sha256:"+strings.Repeat("b", 64))
	t.Setenv("PORTAL_RETIREMENT_DEPLOYMENT_RUN", "456")
}

func deploymentHandoffRunner(t *testing.T) {
	t.Helper()
	t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
	t.Setenv("GITHUB_WORKFLOW_REF", "devantler-tech/platform/.github/workflows/cd.yaml@refs/heads/main")
	t.Setenv("GITHUB_RUN_ID", "999")
	t.Setenv("GITHUB_RUN_ATTEMPT", "1")
	t.Setenv("GITHUB_SHA", strings.Repeat("a", 40))
	t.Setenv("PORTAL_RETIREMENT_REVISION", "latest@sha256:"+strings.Repeat("b", 64))
}

func deploymentMergeGroupHandoffRunner(t *testing.T) {
	t.Helper()
	queueRef := "refs/heads/gh-readonly-queue/main/pr-5048-" + strings.Repeat("d", 40)
	t.Setenv("GITHUB_EVENT_NAME", "merge_group")
	t.Setenv("GITHUB_WORKFLOW_REF", "devantler-tech/platform/.github/workflows/ci.yaml@"+queueRef)
	t.Setenv("GITHUB_REF", queueRef)
	t.Setenv("GITHUB_RUN_ID", "999")
	t.Setenv("GITHUB_RUN_ATTEMPT", "1")
	t.Setenv("GITHUB_SHA", strings.Repeat("a", 40))
	t.Setenv("PORTAL_RETIREMENT_REVISION", "latest@sha256:"+strings.Repeat("b", 64))
}

func readyConditions() []any {
	return []any{
		map[string]any{"type": "Reconciling", "status": "False", "observedGeneration": 1},
		map[string]any{"type": "Ready", "status": "True", "observedGeneration": 1},
	}
}

func journalBlockedConditions() []any {
	return []any{
		map[string]any{"type": "Reconciling", "status": "True", "observedGeneration": 1, "reason": "ProgressingWithRetry", "message": "Detecting drift"},
		map[string]any{"type": "Ready", "status": "False", "observedGeneration": 1, "reason": "ReconciliationFailed", "message": "Namespace/backstage dry-run failed: restrict-backstage-retirement preserve-retirement-journal"},
	}
}

func (f *completionFixture) execute(ctx context.Context, program string, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	if program == "kubectl" && strings.Contains(joined, "get persistentvolumes") && f.readFailures > 0 {
		f.readFailures--
		return nil, fmt.Errorf("transient evidence read failure")
	}
	if program == "sleep" {
		f.sleeps++
		if f.onSleep != nil {
			f.onSleep()
			f.onSleep = nil
		}
		return nil, nil
	}
	if program == "bash" && strings.Contains(joined, "scripts/verify-prod-recovery-source.sh") {
		f.sourceChecks++
		return []byte("PROD_RECOVERY_SOURCE=PASS\n"), nil
	}
	if program == "git" {
		switch {
		case strings.Contains(joined, "merge-base --is-ancestor"):
			return nil, nil
		case strings.Contains(joined, "diff --no-ext-diff --no-renames --name-only"):
			return []byte(strings.Join(f.handoffPaths, "\n") + "\n"), nil
		case strings.Contains(joined, "show --no-ext-diff --no-textconv"):
			parts := strings.Split(args[len(args)-1], ":")
			if len(parts) != 2 {
				return nil, fmt.Errorf("invalid git show target")
			}
			content, ok := f.handoffFiles[parts[1]]
			if !ok {
				return nil, fmt.Errorf("missing handoff content")
			}
			return content, nil
		}
	}
	if program == "gh" {
		switch {
		case strings.Contains(joined, "actions/runs/456"):
			return encode(f.portal.t, map[string]any{"id": 456, "run_attempt": 1, "head_branch": "main", "path": ".github/workflows/cd.yaml", "event": "workflow_dispatch", "head_sha": strings.Repeat("a", 40), "status": "completed", "conclusion": "success", "repository": map[string]any{"full_name": "devantler-tech/platform"}}), nil
		case strings.Contains(joined, "deployments?sha="):
			return encode(f.portal.t, []any{map[string]any{"id": 789, "sha": strings.Repeat("a", 40), "environment": "prod"}}), nil
		case strings.Contains(joined, "deployments/789/statuses"):
			return encode(f.portal.t, []any{map[string]any{"state": "success", "environment": "prod", "log_url": "https://github.com/devantler-tech/platform/actions/runs/456/job/777"}}), nil
		case strings.Contains(joined, "attestation verify"):
			return completionAttestation(f.portal.t), nil
		}
	}
	if program == "kubectl" && strings.Contains(joined, "get configmap backstage-retirement-42") {
		return encode(f.portal.t, f.receipt), nil
	}
	if program == "kubectl" && strings.Contains(joined, "patch namespace backstage") {
		f.patches++
		path := ""
		for _, arg := range args {
			if strings.HasPrefix(arg, "--patch-file=") {
				path = strings.TrimPrefix(arg, "--patch-file=")
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var ops []map[string]any
		if json.Unmarshal(data, &ops) != nil || len(ops) != 4 || ops[0]["path"] != "/metadata/uid" || ops[0]["value"] != "namespace-uid" || ops[1]["path"] != "/metadata/resourceVersion" || ops[2]["path"] != "/metadata/annotations/platform.devantler.tech~1backstage-retirement" || ops[2]["value"] != f.record || ops[3]["op"] != "remove" || ops[3]["path"] != "/metadata/annotations/platform.devantler.tech~1backstage-retirement" {
			return nil, fmt.Errorf("completion patch escaped its exact journal")
		}
		delete(object(field(f.portal.namespace, "metadata", "annotations")), fenceAnnotation)
		object(f.portal.namespace["metadata"])["resourceVersion"] = "14"
		for _, raw := range array(f.portal.layers["items"]) {
			root := object(raw)
			if field(root, "metadata", "name") == "apps" {
				object(root["status"])["lastAppliedRevision"] = os.Getenv("PORTAL_RETIREMENT_REVISION")
				object(root["status"])["lastAttemptedRevision"] = os.Getenv("PORTAL_RETIREMENT_REVISION")
				object(root["status"])["conditions"] = readyConditions()
			}
		}
		f.cleared = true
		if f.postMutation != nil {
			f.postMutation()
		}
		return encode(f.portal.t, f.portal.namespace), nil
	}
	return f.portal.execute(ctx, program, args...)
}

func completionAttestation(t *testing.T) []byte {
	t.Helper()
	digest := strings.Repeat("b", 64)
	return encode(t, []any{map[string]any{"verificationResult": map[string]any{
		"signature": map[string]any{"certificate": map[string]any{
			"issuer": "https://token.actions.githubusercontent.com", "subjectAlternativeName": "https://github.com/devantler-tech/platform/.github/workflows/cd.yaml@refs/heads/main",
			"sourceRepositoryURI": "https://github.com/devantler-tech/platform", "sourceRepositoryDigest": strings.Repeat("a", 40), "sourceRepositoryRef": "refs/heads/main",
			"runInvocationURI": "https://github.com/devantler-tech/platform/actions/runs/456/attempts/1",
		}},
		"statement": map[string]any{"predicateType": "https://slsa.dev/provenance/v1", "subject": []any{map[string]any{"name": "ghcr.io/devantler-tech/platform/manifests", "digest": map[string]any{"sha256": digest}}}},
	}}})
}

func TestCompletionClearsOnlyExactTerminalJournal(t *testing.T) {
	f := completedRetirementFixture(t)
	var out bytes.Buffer
	if code := complete([]string{"--complete"}, &out, f.execute); code != 0 || !f.cleared || f.patches != 1 || out.String() != "PORTAL-RETIREMENT=COMPLETED journal=REMOVED preservationSamples=2 flux=READY dataRetirement=UNASSESSED\n" {
		t.Fatalf("terminal retirement was not completed exactly once: code=%d patches=%d output=%s", code, f.patches, &out)
	}
}

func TestDeploymentHandoffAcceptsOnlyExactTerminalJournalBlock(t *testing.T) {
	f := completedRetirementFixture(t)
	deploymentHandoffRunner(t)
	var out bytes.Buffer
	if code := admitDeploymentHandoff([]string{"--admit-deployment-handoff"}, &out, f.execute); code != 0 || f.patches != 0 || f.sourceChecks != 2 || out.String() != "PORTAL-RETIREMENT=DEPLOYMENT_HANDOFF preservationSamples=2 mutation=NONE\n" {
		e := engine{execute: f.execute, revision: os.Getenv("PORTAL_RETIREMENT_REVISION"), sha: os.Getenv("GITHUB_SHA"), readOnly: true}
		namespace, namespaceErr := e.read(context.Background(), "get", "namespace", "backstage", "--output=json")
		record, recordErr := parseCompletionRecord(text(field(namespace, "metadata", "annotations", fenceAnnotation)), namespace)
		e.fence, e.retiring, e.ownerUID = record.raw, record.retiring, record.releaseUID
		fenceErr := e.requireFence(context.Background())
		observed, collectErr := e.collect(context.Background())
		t.Fatalf("exact terminal journal block was not admitted read-only: code=%d patches=%d output=%s namespace=%v record=%v fence=%v collect=%v completed=%v roots=%v", code, f.patches, &out, namespaceErr, recordErr, fenceErr, collectErr, completedState(observed, record, true), platformRootState(observed, record, e.revision, false))
	}
}

func TestDeploymentHandoffWaitsForTheExactJournalOnlyRetry(t *testing.T) {
	f := completedRetirementFixture(t)
	deploymentHandoffRunner(t)
	apps := item(f.portal.layers, 0)
	object(apps["status"])["conditions"] = []any{
		map[string]any{"type": "Ready", "status": "False", "observedGeneration": 1, "reason": "DependencyNotReady", "message": "Dependencies do not meet ready condition"},
	}
	f.onSleep = func() { object(apps["status"])["conditions"] = journalBlockedConditions() }
	var out bytes.Buffer
	if code := admitDeploymentHandoff([]string{"--admit-deployment-handoff"}, &out, f.execute); code != 0 || f.patches != 0 || f.sleeps != 2 || out.String() != "PORTAL-RETIREMENT=DEPLOYMENT_HANDOFF preservationSamples=2 mutation=NONE\n" {
		t.Fatalf("bounded wait did not admit the later exact journal-only retry: code=%d patches=%d sleeps=%d output=%s", code, f.patches, f.sleeps, &out)
	}
}

func TestDeploymentHandoffWaitsForTransientDatabaseHealth(t *testing.T) {
	f := completedRetirementFixture(t)
	deploymentHandoffRunner(t)
	db := item(f.portal.live, 2)
	object(db["status"])["readyInstances"] = 1
	f.onSleep = func() { object(db["status"])["readyInstances"] = 2 }
	var out bytes.Buffer
	if code := admitDeploymentHandoff([]string{"--admit-deployment-handoff"}, &out, f.execute); code != 0 || f.patches != 0 || f.sleeps != 2 || out.String() != "PORTAL-RETIREMENT=DEPLOYMENT_HANDOFF preservationSamples=2 mutation=NONE\n" {
		t.Fatalf("bounded wait did not admit restored database health: code=%d patches=%d sleeps=%d output=%s", code, f.patches, f.sleeps, &out)
	}
}

func TestDeploymentHandoffRetriesAnIncompleteEvidenceRead(t *testing.T) {
	f := completedRetirementFixture(t)
	deploymentHandoffRunner(t)
	f.readFailures = 1
	var out bytes.Buffer
	if code := admitDeploymentHandoff([]string{"--admit-deployment-handoff"}, &out, f.execute); code != 0 || f.patches != 0 || f.sleeps != 2 || out.String() != "PORTAL-RETIREMENT=DEPLOYMENT_HANDOFF preservationSamples=2 mutation=NONE\n" {
		t.Fatalf("bounded wait did not recover an incomplete evidence read: code=%d patches=%d sleeps=%d output=%s", code, f.patches, f.sleeps, &out)
	}
}

func TestDeploymentHandoffDoesNotWaitWhenARetiredTargetReappears(t *testing.T) {
	f := completedRetirementFixture(t)
	deploymentHandoffRunner(t)
	f.portal.deleted = false
	var out bytes.Buffer
	if code := admitDeploymentHandoff([]string{"--admit-deployment-handoff"}, &out, f.execute); code == 0 || f.patches != 0 || f.sleeps != 0 || out.String() != "PORTAL-RETIREMENT=HOLD_DEPLOYMENT_HANDOFF stage=evidence-first\n" {
		t.Fatalf("changed terminal state was retried or admitted: code=%d patches=%d sleeps=%d output=%s", code, f.patches, f.sleeps, &out)
	}
}

func TestDeploymentHandoffDoesNotRetryAPartiallyCollectedRetirementTarget(t *testing.T) {
	f := completedRetirementFixture(t)
	deploymentHandoffRunner(t)
	owner, err := decode(encode(t, f.portal.hr))
	if err != nil {
		t.Fatal(err)
	}
	object(owner["metadata"])["uid"] = "recreated-release-uid"
	delete(object(owner["status"]), "helmChart")
	present := true
	f.onSleep = func() { present = false }
	execute := func(ctx context.Context, program string, args ...string) ([]byte, error) {
		if present && program == "kubectl" && strings.Contains(strings.Join(args, " "), "get helmreleases") {
			return encode(t, map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{owner}}), nil
		}
		return f.execute(ctx, program, args...)
	}
	var out bytes.Buffer
	if code := admitDeploymentHandoff([]string{"--admit-deployment-handoff"}, &out, execute); code == 0 || f.patches != 0 || f.sleeps != 0 || out.String() != "PORTAL-RETIREMENT=HOLD_DEPLOYMENT_HANDOFF stage=evidence-first\n" {
		t.Fatalf("partially collected replacement was retried or admitted: code=%d patches=%d sleeps=%d output=%s", code, f.patches, f.sleeps, &out)
	}
}

func TestDeploymentHandoffAcceptsExactMainMergeGroupTerminalJournalBlock(t *testing.T) {
	f := completedRetirementFixture(t)
	deploymentMergeGroupHandoffRunner(t)
	var out bytes.Buffer
	if code := admitDeploymentHandoff([]string{"--admit-deployment-handoff"}, &out, f.execute); code != 0 || f.patches != 0 || f.sourceChecks != 0 || out.String() != "PORTAL-RETIREMENT=DEPLOYMENT_HANDOFF preservationSamples=2 mutation=NONE\n" {
		t.Fatalf("exact main merge-group terminal journal block was not admitted read-only: code=%d patches=%d sourceChecks=%d output=%s", code, f.patches, f.sourceChecks, &out)
	}
}

var reviewedRecoveryPinnedFixtures = map[string]string{
	"k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml": "reviewed tenant RGD fixture\n",
	"k8s/bases/infrastructure/resource-graph-definitions/webapp/resource-graph-definition.yaml": "reviewed webapp RGD fixture\n",
	"ksail.prod.yaml": "reviewed production validation fixture\n",
	"ksail.yaml":      "reviewed local validation fixture\n",
	"scripts/rgd-template-static-scan-baseline.tsv":            "reviewed RGD baseline fixture\n",
	"scripts/tests/test-rgd-template-static-scan.sh":           "reviewed RGD gate fixture\n",
	"scripts/validate-eks-ci-role-policy/approved-surface.txt": "reviewed authorization baseline fixture\n",
	"scripts/validate-eks-ci-role-policy/main.go":              "reviewed authorization validator fixture\n",
}

func reviewedRecoveryPinnedHashes(t *testing.T, f *completionFixture) map[string]string {
	t.Helper()
	if len(reviewedRecoveryPinnedFixtures) != len(deploymentHandoffPinnedHashes) {
		t.Fatal("reviewed recovery fixture path census differs from production")
	}
	hashes := make(map[string]string, len(reviewedRecoveryPinnedFixtures))
	for path, fixture := range reviewedRecoveryPinnedFixtures {
		if _, ok := deploymentHandoffPinnedHashes[path]; !ok {
			t.Fatalf("reviewed recovery fixture is not production-pinned: %s", path)
		}
		content := []byte(fixture)
		f.handoffFiles[path] = content
		hashes[path] = fmt.Sprintf("%x", sha256.Sum256(content))
	}
	return hashes
}

func TestDeploymentHandoffAcceptsCurrentReviewedRecoveryDiff(t *testing.T) {
	f := completedRetirementFixture(t)
	f.handoffPaths = []string{
		".github/actions/deploy-prod/action.yml",
		".github/workflows/complete-backstage-retirement.yaml",
		"k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml",
		"k8s/bases/infrastructure/resource-graph-definitions/webapp/resource-graph-definition.yaml",
		"ksail.prod.yaml",
		"ksail.yaml",
		"scripts/reconcile-flux-workloads.sh",
		"scripts/reconcile-talos-kernel-args/main.go",
		"scripts/reconcile-talos-kernel-args/main_test.go",
		"scripts/retire-backstage-stateless/README.md",
		"scripts/retire-backstage-stateless/complete.go",
		"scripts/retire-backstage-stateless/complete_test.go",
		"scripts/retire-backstage-stateless/complete_workflow_test.go",
		"scripts/retire-backstage-stateless/fence.go",
		"scripts/retire-backstage-stateless/fence_test.go",
		"scripts/retire-backstage-stateless/main.go",
		"scripts/retire-backstage-stateless/native_test.go",
		"scripts/rgd-template-static-scan-baseline.tsv",
		"scripts/tests/test-flux-reconcile-control-plane-restart.sh",
		"scripts/tests/test-rgd-template-static-scan.sh",
		"scripts/tests/test-talos-render-kernel-args.sh",
		"scripts/validate-eks-ci-role-policy/approved-surface.txt",
		"scripts/validate-eks-ci-role-policy/main.go",
	}
	pinnedHashes := reviewedRecoveryPinnedHashes(t, f)
	record := completionRecord{source: strings.Repeat("c", 40)}
	e := engine{execute: f.execute, sha: strings.Repeat("a", 40), readOnly: true}
	if err := deploymentHandoffSourceSafeWithPins(context.Background(), e, record, pinnedHashes); err != nil {
		t.Fatalf("reviewed recovery diff was not admitted: %v", err)
	}
}

func TestDeploymentHandoffRejectsChangedPinnedRecoveryFile(t *testing.T) {
	f := completedRetirementFixture(t)
	path := "ksail.prod.yaml"
	f.handoffPaths = []string{path}
	pinnedHashes := reviewedRecoveryPinnedHashes(t, f)
	f.handoffFiles[path] = append(f.handoffFiles[path], []byte("# unrelated later edit\n")...)
	record := completionRecord{source: strings.Repeat("c", 40)}
	e := engine{execute: f.execute, sha: strings.Repeat("a", 40), readOnly: true}
	if err := deploymentHandoffSourceSafeWithPins(context.Background(), e, record, pinnedHashes); err == nil {
		t.Fatal("changed pinned recovery file was admitted")
	}
}

func TestDeploymentHandoffRejectsAnyOtherAppsFailure(t *testing.T) {
	f := completedRetirementFixture(t)
	deploymentHandoffRunner(t)
	apps := item(f.portal.layers, 0)
	object(apps["status"])["conditions"] = []any{
		map[string]any{"type": "Ready", "status": "False", "observedGeneration": 1, "reason": "ReconciliationFailed", "message": "another application failed"},
	}
	var out bytes.Buffer
	if code := admitDeploymentHandoff([]string{"--admit-deployment-handoff"}, &out, f.execute); code == 0 || f.patches != 0 || out.String() != "PORTAL-RETIREMENT=HOLD_DEPLOYMENT_HANDOFF stage=evidence-first\n" {
		t.Fatalf("unrelated applications failure was admitted: code=%d patches=%d output=%s", code, f.patches, &out)
	}
}

func TestDeploymentHandoffRejectsApplicationChangesAfterTheRetirementSource(t *testing.T) {
	f := completedRetirementFixture(t)
	deploymentHandoffRunner(t)
	f.handoffPaths = []string{"k8s/providers/hetzner/apps/kustomization.yaml"}
	var out bytes.Buffer
	if code := admitDeploymentHandoff([]string{"--admit-deployment-handoff"}, &out, f.execute); code == 0 || f.patches != 0 || out.String() != "PORTAL-RETIREMENT=HOLD_DEPLOYMENT_HANDOFF stage=source-diff\n" {
		t.Fatalf("application drift was admitted before terminal completion: code=%d patches=%d output=%s", code, f.patches, &out)
	}
}

func TestDeploymentHandoffRejectsNonDeploymentWorkflow(t *testing.T) {
	f := completedRetirementFixture(t)
	deploymentHandoffRunner(t)
	t.Setenv("GITHUB_WORKFLOW_REF", "devantler-tech/platform/.github/workflows/ci.yaml@refs/heads/main")
	var out bytes.Buffer
	if code := admitDeploymentHandoff([]string{"--admit-deployment-handoff"}, &out, f.execute); code == 0 || f.patches != 0 || out.String() != "PORTAL-RETIREMENT=HOLD_DEPLOYMENT_HANDOFF stage=admission\n" {
		t.Fatalf("non-CD workflow admitted the handoff: code=%d patches=%d output=%s", code, f.patches, &out)
	}
}

func TestDeploymentHandoffRejectsForeignMergeGroupRef(t *testing.T) {
	f := completedRetirementFixture(t)
	deploymentMergeGroupHandoffRunner(t)
	t.Setenv("GITHUB_REF", "refs/heads/feature/backstage-retirement")
	var out bytes.Buffer
	if code := admitDeploymentHandoff([]string{"--admit-deployment-handoff"}, &out, f.execute); code == 0 || f.patches != 0 || out.String() != "PORTAL-RETIREMENT=HOLD_DEPLOYMENT_HANDOFF stage=admission\n" {
		t.Fatalf("foreign merge-group ref was admitted: code=%d patches=%d output=%s", code, f.patches, &out)
	}
}

func TestDeploymentHandoffRejectsMismatchedMergeGroupWorkflowRef(t *testing.T) {
	f := completedRetirementFixture(t)
	deploymentMergeGroupHandoffRunner(t)
	t.Setenv("GITHUB_WORKFLOW_REF", "devantler-tech/platform/.github/workflows/ci.yaml@refs/heads/gh-readonly-queue/main/pr-5049-"+strings.Repeat("e", 40))
	var out bytes.Buffer
	if code := admitDeploymentHandoff([]string{"--admit-deployment-handoff"}, &out, f.execute); code == 0 || f.patches != 0 || out.String() != "PORTAL-RETIREMENT=HOLD_DEPLOYMENT_HANDOFF stage=admission\n" {
		t.Fatalf("mismatched merge-group workflow ref was admitted: code=%d patches=%d output=%s", code, f.patches, &out)
	}
}

func TestCompletionRejectsForeignJournalTarget(t *testing.T) {
	f := completedRetirementFixture(t)
	var record map[string]any
	if json.Unmarshal([]byte(f.record), &record) != nil {
		t.Fatal("invalid fixture journal")
	}
	object(array(record["targets"])[0])["uid"] = "foreign-release-uid"
	b, _ := json.Marshal(record)
	f.record = string(b)
	object(field(f.portal.namespace, "metadata", "annotations"))[fenceAnnotation] = f.record
	var out bytes.Buffer
	if code := complete([]string{"--complete"}, &out, f.execute); code != 2 || f.patches != 0 {
		t.Fatalf("foreign journal target authorized completion: code=%d patches=%d output=%s", code, f.patches, &out)
	}
}

func TestCompletionRequiresContinuousArchiving(t *testing.T) {
	f := completedRetirementFixture(t)
	for _, condition := range array(field(item(f.portal.live, 2), "status", "conditions")) {
		if field(object(condition), "type") == "ContinuousArchiving" {
			object(condition)["status"] = "False"
		}
	}
	var out bytes.Buffer
	if code := complete([]string{"--complete"}, &out, f.execute); code != 2 || f.patches != 0 {
		t.Fatalf("failed archiving authorized completion: code=%d patches=%d output=%s", code, f.patches, &out)
	}
}

func TestCompletionRequiresTheAcceptedRevisionLive(t *testing.T) {
	f := completedRetirementFixture(t)
	object(f.portal.sources["status"])["artifact"] = map[string]any{"revision": "latest@sha256:" + strings.Repeat("e", 64)}
	var out bytes.Buffer
	if code := complete([]string{"--complete"}, &out, f.execute); code != 2 || f.patches != 0 {
		t.Fatalf("unaccepted live source authorized completion: code=%d patches=%d output=%s", code, f.patches, &out)
	}
}

func TestCompletionHoldsWhenRetainedStorageChangesAfterPatch(t *testing.T) {
	f := completedRetirementFixture(t)
	f.postMutation = func() { object(item(f.portal.volumes, 0)["spec"])["persistentVolumeReclaimPolicy"] = "Delete" }
	var out bytes.Buffer
	if code := complete([]string{"--complete"}, &out, f.execute); code != 2 || f.patches != 1 || !f.cleared {
		t.Fatalf("post-clear storage drift was accepted: code=%d patches=%d output=%s", code, f.patches, &out)
	}
}

func TestCompletionNeverTreatsAlreadyClearedJournalAsSuccess(t *testing.T) {
	f := completedRetirementFixture(t)
	delete(object(field(f.portal.namespace, "metadata", "annotations")), fenceAnnotation)
	var out bytes.Buffer
	if code := complete([]string{"--complete"}, &out, f.execute); code != 2 || f.patches != 0 {
		t.Fatalf("already-cleared state was accepted or mutated: code=%d patches=%d output=%s", code, f.patches, &out)
	}
}
