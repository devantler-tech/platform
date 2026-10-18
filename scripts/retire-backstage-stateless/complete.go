package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

const completionWorkflow = "devantler-tech/platform/.github/workflows/complete-backstage-retirement.yaml@refs/heads/main"
const deploymentWorkflow = "devantler-tech/platform/.github/workflows/cd.yaml@refs/heads/main"
const mergeGroupDeploymentWorkflow = "devantler-tech/platform/.github/workflows/ci.yaml@"

var platformRoots = map[string]bool{
	"apps": true, "bootstrap": true, "flux-system": true,
	"infrastructure": true, "infrastructure-controllers": true,
}

var deploymentHandoffPaths = map[string]bool{
	".github/actions/deploy-prod/action.yml":                       true,
	".github/workflows/complete-backstage-retirement.yaml":         true,
	"scripts/reconcile-flux-workloads.sh":                          true,
	"scripts/reconcile-talos-kernel-args/main.go":                  true,
	"scripts/reconcile-talos-kernel-args/main_test.go":             true,
	"scripts/retire-backstage-stateless/README.md":                 true,
	"scripts/retire-backstage-stateless/complete.go":               true,
	"scripts/retire-backstage-stateless/complete_test.go":          true,
	"scripts/retire-backstage-stateless/complete_workflow_test.go": true,
	"scripts/retire-backstage-stateless/fence.go":                  true,
	"scripts/retire-backstage-stateless/fence_test.go":             true,
	"scripts/retire-backstage-stateless/main.go":                   true,
	"scripts/retire-backstage-stateless/native_test.go":            true,
	"scripts/tests/test-flux-reconcile-control-plane-restart.sh":   true,
	"scripts/tests/test-talos-render-kernel-args.sh":               true,
}

func deploymentHandoffContext() (admitted, currentMain bool) {
	event, ref, workflow := os.Getenv("GITHUB_EVENT_NAME"), os.Getenv("GITHUB_REF"), os.Getenv("GITHUB_WORKFLOW_REF")
	currentMain = event == "workflow_dispatch" && ref == "refs/heads/main" && workflow == deploymentWorkflow
	mergeGroup := event == "merge_group" && workflow == mergeGroupDeploymentWorkflow+ref && regexp.MustCompile(`^refs/heads/gh-readonly-queue/main/pr-[1-9][0-9]*-[a-f0-9]{40}$`).MatchString(ref)
	return currentMain || mergeGroup, currentMain
}

type completionRecord struct {
	raw, source, revision, run, namespaceUID, releaseUID, chartUID string
	retiring                                                       map[string]string
}

type completionAttestationRecord struct {
	VerificationResult struct {
		Signature struct {
			Certificate struct {
				Issuer, SubjectAlternativeName, SourceRepositoryURI string
				SourceRepositoryDigest, SourceRepositoryRef         string
				RunInvocationURI                                    string
			} `json:"certificate"`
		} `json:"signature"`
		Statement struct {
			PredicateType string `json:"predicateType"`
			Subject       []struct {
				Name   string            `json:"name"`
				Digest map[string]string `json:"digest"`
			} `json:"subject"`
		} `json:"statement"`
	} `json:"verificationResult"`
}

func completionAPI(ctx context.Context, execute command, path string) ([]byte, error) {
	b, err := execute(ctx, "gh", "api", "--hostname=github.com", path)
	if err != nil || len(b) > maxResponse || !json.Valid(b) {
		return nil, errors.New("deployment evidence unavailable")
	}
	return b, nil
}

// completionAccepted binds the current live artifact to one successful normal
// production deployment. The terminal-maintenance workflow cannot witness itself.
func completionAccepted(ctx context.Context, e engine, deploymentRun string) error {
	b, err := completionAPI(ctx, e.execute, "repos/devantler-tech/platform/actions/runs/"+deploymentRun)
	if err != nil {
		return err
	}
	r, err := decode(b)
	ci := r["path"] == ".github/workflows/ci.yaml" && r["event"] == "merge_group"
	cd := r["path"] == ".github/workflows/cd.yaml" && r["event"] == "workflow_dispatch"
	if err != nil || fmt.Sprint(r["id"]) != deploymentRun || r["head_sha"] != e.sha || r["status"] != "completed" || r["conclusion"] != "success" || field(r, "repository", "full_name") != "devantler-tech/platform" || !ci && !cd {
		return errors.New("deployment run identity unverified")
	}
	branch, attempt := text(r["head_branch"]), fmt.Sprint(r["run_attempt"])
	if !regexp.MustCompile(`^[1-9][0-9]{0,5}$`).MatchString(attempt) || ci && !regexp.MustCompile(`^gh-readonly-queue/main/pr-[1-9][0-9]*-[a-f0-9]{40}$`).MatchString(branch) || cd && branch != "main" {
		return errors.New("deployment run identity unverified")
	}
	b, err = completionAPI(ctx, e.execute, "repos/devantler-tech/platform/deployments?sha="+e.sha+"&environment=prod&per_page=100")
	var deployments []map[string]any
	if err != nil || json.Unmarshal(b, &deployments) != nil || len(deployments) == 0 || len(deployments) >= 100 {
		return errors.New("deployment census unavailable")
	}
	accepted := false
	for _, deployment := range deployments {
		id := fmt.Sprint(deployment["id"])
		if !regexp.MustCompile(`^[1-9][0-9]{0,19}$`).MatchString(id) || deployment["sha"] != e.sha || deployment["environment"] != "prod" {
			return errors.New("deployment identity unverified")
		}
		b, err = completionAPI(ctx, e.execute, "repos/devantler-tech/platform/deployments/"+id+"/statuses?per_page=100")
		var statuses []map[string]any
		if err != nil || json.Unmarshal(b, &statuses) != nil || len(statuses) == 0 || len(statuses) >= 100 {
			return errors.New("deployment status unavailable")
		}
		status := statuses[0]
		accepted = accepted || status["state"] == "success" && status["environment"] == "prod" && regexp.MustCompile(`^https://github\.com/devantler-tech/platform/actions/runs/`+deploymentRun+`/job/[1-9][0-9]*$`).MatchString(text(status["log_url"]))
	}
	if !accepted {
		return errors.New("deployment acceptance unverified")
	}
	digest := strings.TrimPrefix(e.revision, "latest@")
	workflow := text(r["path"])
	ref := "refs/heads/" + branch
	identity := "https://github.com/devantler-tech/platform/" + workflow + "@" + ref
	b, err = e.execute(ctx, "gh", "attestation", "verify", "oci://ghcr.io/devantler-tech/platform/manifests@"+digest,
		"--bundle-from-oci", "--repo", "devantler-tech/platform", "--predicate-type", "https://slsa.dev/provenance/v1",
		"--source-digest", e.sha, "--cert-identity-regex", "^"+regexp.QuoteMeta(identity)+"$", "--format", "json")
	if err != nil || len(b) > maxResponse || !json.Valid(b) {
		return errors.New("artifact verifier unavailable")
	}
	var attestations []completionAttestationRecord
	if json.Unmarshal(b, &attestations) != nil || len(attestations) == 0 || len(attestations) > 30 {
		return errors.New("artifact response unverified")
	}
	wantRun := "https://github.com/devantler-tech/platform/actions/runs/" + deploymentRun + "/attempts/" + attempt
	wantDigest := strings.TrimPrefix(digest, "sha256:")
	matched := false
	for _, attestation := range attestations {
		certificate := attestation.VerificationResult.Signature.Certificate
		statement := attestation.VerificationResult.Statement
		if certificate.Issuer != "https://token.actions.githubusercontent.com" || certificate.SubjectAlternativeName != identity || certificate.SourceRepositoryURI != "https://github.com/devantler-tech/platform" || certificate.SourceRepositoryDigest != e.sha || certificate.SourceRepositoryRef != ref || certificate.RunInvocationURI == "" || statement.PredicateType != "https://slsa.dev/provenance/v1" || len(statement.Subject) != 1 {
			return errors.New("artifact identity unverified")
		}
		subject := statement.Subject[0]
		if subject.Name != "ghcr.io/devantler-tech/platform/manifests" || len(subject.Digest) != 1 || subject.Digest["sha256"] != wantDigest || !regexp.MustCompile(`^https://github\.com/devantler-tech/platform/actions/runs/[1-9][0-9]{0,19}/attempts/[1-9][0-9]{0,5}$`).MatchString(certificate.RunInvocationURI) {
			return errors.New("artifact identity unverified")
		}
		matched = matched || certificate.RunInvocationURI == wantRun
	}
	if !matched {
		return errors.New("artifact identity unverified")
	}
	return nil
}

func parseCompletionRecord(raw string, namespace map[string]any) (completionRecord, error) {
	record := completionRecord{raw: raw, retiring: map[string]string{}}
	if raw == "" || len(raw) > 32<<10 || !json.Valid([]byte(raw)) {
		return record, errors.New("journal unavailable")
	}
	value, err := decode([]byte(raw))
	if err != nil {
		return record, errors.New("journal malformed")
	}
	allowed := map[string]bool{"version": true, "phase": true, "namespaceUID": true, "sourceSHA": true, "revision": true, "run": true, "targets": true, "retiringUIDs": true, "retained": true, "latestHistoryUID": true, "helmWriterUID": true, "cnpgWriterUID": true, "rootCAWriterUID": true, "trustWriterUID": true}
	for key := range value {
		if !allowed[key] {
			return record, errors.New("journal field unrecognized")
		}
	}
	record.source, record.revision, record.run = text(value["sourceSHA"]), text(value["revision"]), text(value["run"])
	record.namespaceUID = text(value["namespaceUID"])
	if integer(value["version"]) != 1 || value["phase"] != "fenced" || record.namespaceUID == "" || record.namespaceUID != field(namespace, "metadata", "uid") || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(record.source) || !regexp.MustCompile(`^latest@sha256:[a-f0-9]{64}$`).MatchString(record.revision) || !regexp.MustCompile(`^[1-9][0-9]{0,19}$`).MatchString(record.run) || text(value["latestHistoryUID"]) == "" || text(value["helmWriterUID"]) == "" || text(value["cnpgWriterUID"]) == "" {
		return record, errors.New("journal identity unverified")
	}
	if _, ok := value["retained"].([]any); !ok || len(array(value["retained"])) > 512 {
		return record, errors.New("journal retained census malformed")
	}
	expected := map[string]string{"HelmRelease/backstage": "", "HelmChart/backstage-backstage": "", "Deployment/backstage": "", "Service/backstage": "", "ConfigMap/backstage-app-config": ""}
	for _, rawTarget := range array(value["targets"]) {
		target := object(rawTarget)
		key, uid := text(target["kind"])+"/"+text(target["name"]), text(target["uid"])
		prior, found := expected[key]
		if len(target) != 3 || !found || prior != "" || uid == "" {
			return record, errors.New("journal targets malformed")
		}
		expected[key] = uid
	}
	if len(array(value["targets"])) != len(expected) {
		return record, errors.New("journal target census incomplete")
	}
	uidSet := map[string]bool{}
	for _, rawUID := range array(value["retiringUIDs"]) {
		uid := text(rawUID)
		if uid == "" || uidSet[uid] || len(uidSet) >= 512 {
			return record, errors.New("journal retiring census malformed")
		}
		uidSet[uid] = true
	}
	for key, uid := range expected {
		if uid == "" || !uidSet[uid] {
			return record, errors.New("journal target identity missing")
		}
		var identityKey string
		switch key {
		case "HelmRelease/backstage":
			identityKey, record.releaseUID = "helm.toolkit.fluxcd.io/v2/HelmRelease/backstage/backstage", uid
		case "HelmChart/backstage-backstage":
			identityKey, record.chartUID = "source.toolkit.fluxcd.io/v1/HelmChart/backstage/backstage-backstage", uid
		case "Deployment/backstage":
			identityKey = "apps/v1/Deployment/backstage/backstage"
		case "Service/backstage":
			identityKey = "v1/Service/backstage/backstage"
		case "ConfigMap/backstage-app-config":
			identityKey = "v1/ConfigMap/backstage/backstage-app-config"
		}
		record.retiring[identityKey] = uid
	}
	return record, nil
}

func completionReceiptValid(receipt map[string]any, record completionRecord) bool {
	want := map[string]any{"source": record.source, "revision": record.revision, "run": record.run, "attempt": "1", "scope": "stateless-only", "releaseUID": record.releaseUID, "chartUID": record.chartUID, "dataRetirement": "UNASSESSED"}
	return metadataValid(receipt) && receipt["apiVersion"] == "v1" && receipt["kind"] == "ConfigMap" && field(receipt, "metadata", "namespace") == "flux-system" && field(receipt, "metadata", "name") == "backstage-retirement-"+record.run && reflect.DeepEqual(receipt["data"], want)
}

func archivingHealthy(s snapshot) bool {
	clusters, claims, pods := 0, 0, 0
	for _, resource := range arrayObjects(s.live) {
		switch resource["kind"] {
		case "Cluster":
			if field(resource, "metadata", "name") != "backstage-db" || integer(field(resource, "spec", "instances")) != 2 || integer(field(resource, "status", "readyInstances")) != 2 || text(field(resource, "status", "currentPrimary")) == "" || field(resource, "status", "currentPrimary") != field(resource, "status", "targetPrimary") {
				return false
			}
			readyCondition, archiving := false, false
			for _, rawCondition := range array(field(resource, "status", "conditions")) {
				condition := object(rawCondition)
				readyCondition = readyCondition || condition["type"] == "Ready" && condition["status"] == "True"
				archiving = archiving || condition["type"] == "ContinuousArchiving" && condition["status"] == "True"
			}
			if !readyCondition || !archiving {
				return false
			}
			clusters++
		case "PersistentVolumeClaim":
			if regexp.MustCompile(`^backstage-db-[1-9][0-9]*$`).MatchString(text(field(resource, "metadata", "name"))) {
				claims++
			}
		case "Pod":
			if field(resource, "metadata", "labels", "cnpg.io/cluster") == "backstage-db" {
				pods++
			}
		}
	}
	return clusters == 1 && claims == 2 && pods == 2
}

func completedState(s snapshot, record completionRecord, fenced bool) error {
	if field(s.namespace, "metadata", "uid") != record.namespaceUID || fenced && field(s.namespace, "metadata", "annotations", fenceAnnotation) != record.raw || !fenced && field(s.namespace, "metadata", "annotations", fenceAnnotation) != nil {
		return errors.New("namespace journal state unverified")
	}
	if s.owner != nil || s.chart != nil || field(s.release, "name") != "backstage" || field(s.release, "namespace") != "backstage" || field(s.release, "info", "status") != "uninstalled" || !absent(s) || !archivingHealthy(s) {
		return errors.New("terminal retirement state unverified")
	}
	return nil
}

func platformRootState(s snapshot, record completionRecord, currentRevision string, completed bool) error {
	if !metadataValid(s.source) || s.source["kind"] != "OCIRepository" || !ready(s.source) || field(s.source, "status", "artifact", "revision") != currentRevision {
		return errors.New("platform source unverified")
	}
	current := text(field(s.source, "status", "artifact", "revision"))
	seen := map[string]bool{}
	for _, root := range arrayObjects(s.layers) {
		if field(root, "metadata", "namespace") != "flux-system" || !platformRoots[text(field(root, "metadata", "name"))] {
			continue
		}
		name := text(field(root, "metadata", "name"))
		if seen[name] || field(root, "spec", "sourceRef", "kind") != "OCIRepository" || field(root, "spec", "sourceRef", "name") != "flux-system" || integer(field(root, "metadata", "generation")) < 1 || integer(field(root, "status", "observedGeneration")) != integer(field(root, "metadata", "generation")) || field(root, "spec", "suspend") == true {
			return errors.New("platform root identity unverified")
		}
		seen[name] = true
		entries, ok := field(root, "status", "inventory", "entries").([]any)
		if !ok || len(entries) == 0 {
			return errors.New("platform root inventory unavailable")
		}
		for _, rawEntry := range entries {
			id := text(field(object(rawEntry), "id"))
			if id == "" || id == "backstage_backstage_helm.toolkit.fluxcd.io_HelmRelease" || id == "backstage_backstage-backstage_source.toolkit.fluxcd.io_HelmChart" || id == "backstage_backstage_apps_Deployment" || id == "backstage_backstage__Service" || id == "backstage_backstage-app-config__ConfigMap" {
				return errors.New("platform root still owns a retirement target")
			}
		}
		if name != "apps" || completed {
			if !ready(root) || field(root, "status", "lastAppliedRevision") != current || field(root, "status", "lastAttemptedRevision") != current || !currentLayerInventory(s, root) {
				return errors.New("platform root not current")
			}
			continue
		}
		blocked := false
		for _, rawCondition := range array(field(root, "status", "conditions")) {
			condition := object(rawCondition)
			message := text(condition["message"])
			blocked = blocked || condition["type"] == "Ready" && condition["status"] == "False" && condition["reason"] == "ReconciliationFailed" && strings.Contains(message, "Namespace/backstage dry-run failed") && strings.Contains(message, "restrict-backstage-retirement") && strings.Contains(message, "preserve-retirement-journal")
		}
		if !blocked || field(root, "status", "lastAppliedRevision") != record.revision || field(root, "status", "lastAttemptedRevision") != current {
			return errors.New("applications root is not blocked only by the journal")
		}
	}
	if len(seen) != len(platformRoots) {
		return errors.New("platform root census incomplete")
	}
	return nil
}

func stableCompletionProjection(s snapshot, record completionRecord) (map[string]map[string]any, error) {
	// The real pre-clear apps status is separately bound to the one exact
	// journal-denial above. Normalize only a deep copy of that status so the
	// existing complete-inventory preservation proof can still cover claims.
	layers, err := decode(mustJSON(s.layers))
	if err != nil {
		return nil, errors.New("platform inventory projection incomplete")
	}
	current := field(s.source, "status", "artifact", "revision")
	for _, raw := range array(layers["items"]) {
		root := object(raw)
		if field(root, "metadata", "namespace") == "flux-system" && field(root, "metadata", "name") == "apps" {
			status := object(root["status"])
			status["lastAppliedRevision"] = current
			status["lastAttemptedRevision"] = current
			status["conditions"] = []any{
				map[string]any{"type": "Reconciling", "status": "False", "observedGeneration": field(root, "metadata", "generation")},
				map[string]any{"type": "Ready", "status": "True", "observedGeneration": field(root, "metadata", "generation")},
			}
		}
	}
	s.layers = layers
	return preserved(normalizedNamespace(s, record.raw))
}

func deploymentHandoffSourceSafe(ctx context.Context, e engine, record completionRecord) error {
	if _, err := e.execute(ctx, "git", "merge-base", "--is-ancestor", record.source, e.sha); err != nil {
		return errors.New("retirement source is not an ancestor")
	}
	b, err := e.execute(ctx, "git", "diff", "--no-ext-diff", "--no-renames", "--name-only", record.source+".."+e.sha, "--")
	if err != nil || len(b) > maxResponse {
		return errors.New("retirement source diff unavailable")
	}
	seen := map[string]bool{}
	for _, path := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if path == "" {
			continue
		}
		if !deploymentHandoffPaths[path] || seen[path] {
			return errors.New("retirement source diff is not maintenance-only")
		}
		seen[path] = true
	}
	return nil
}

// waitForDeploymentHandoffRoots allows Flux's ordinary retry loop to replace a
// transient dependency/build status with the one exact journal-only block that
// can authorize the handoff. The wait is read-only and bounded inside the
// caller's ten-minute context; every sample must keep the terminal retirement
// state intact, and no other final applications failure is ever accepted.
func waitForDeploymentHandoffRoots(ctx context.Context, e engine, record completionRecord, observed snapshot) (snapshot, error) {
	const attempts = 50
	for attempt := 0; attempt < attempts; attempt++ {
		if platformRootState(observed, record, e.revision, false) == nil {
			return observed, nil
		}
		if attempt == attempts-1 {
			break
		}
		if _, err := e.execute(ctx, "sleep", "10"); err != nil {
			return observed, errors.New("platform root retry interrupted")
		}
		next, err := e.collect(ctx)
		if err != nil {
			return observed, err
		}
		if err := completedState(next, record, true); err != nil {
			return observed, err
		}
		observed = next
	}
	return observed, errors.New("platform roots did not reach the journal-only block")
}

// admitDeploymentHandoff lets a current-main CD run continue only when its
// reconcile failed for the one terminal journal state that the separate
// completion workflow is designed to clear. It is read-only: the successful CD
// run supplies the independently attested deployment receipt, then the
// completion workflow repeats this proof before removing the exact journal.
func admitDeploymentHandoff(args []string, out io.Writer, execute command) int {
	hold := func(stage string) int {
		_, _ = fmt.Fprintln(out, "PORTAL-RETIREMENT=HOLD_DEPLOYMENT_HANDOFF stage="+stage)
		return 2
	}
	admitted, currentMain := deploymentHandoffContext()
	if len(args) != 1 || args[0] != "--admit-deployment-handoff" || os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("GITHUB_REPOSITORY") != "devantler-tech/platform" || !admitted || os.Getenv("GITHUB_RUN_ATTEMPT") != "1" || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(os.Getenv("GITHUB_SHA")) || !regexp.MustCompile(`^[1-9][0-9]{0,19}$`).MatchString(os.Getenv("GITHUB_RUN_ID")) || !regexp.MustCompile(`^latest@sha256:[a-f0-9]{64}$`).MatchString(os.Getenv("PORTAL_RETIREMENT_REVISION")) {
		return hold("admission")
	}
	e := engine{execute: execute, revision: os.Getenv("PORTAL_RETIREMENT_REVISION"), sha: os.Getenv("GITHUB_SHA"), readOnly: true}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if currentMain && e.sourceCurrent(ctx) != nil {
		return hold("source")
	}
	namespace, err := e.read(ctx, "get", "namespace", "backstage", "--output=json")
	if err != nil || !metadataValid(namespace) || namespace["kind"] != "Namespace" {
		return hold("namespace")
	}
	record, err := parseCompletionRecord(text(field(namespace, "metadata", "annotations", fenceAnnotation)), namespace)
	if err != nil {
		return hold("journal")
	}
	if deploymentHandoffSourceSafe(ctx, e, record) != nil {
		return hold("source-diff")
	}
	e.fence, e.retiring, e.ownerUID = record.raw, record.retiring, record.releaseUID
	receipt, err := e.optional(ctx, "--namespace=flux-system", "get", "configmap", "backstage-retirement-"+record.run, "--ignore-not-found", "--output=json")
	if err != nil || !completionReceiptValid(receipt, record) {
		return hold("receipt")
	}
	first, err := e.collect(ctx)
	if err != nil || completedState(first, record, true) != nil {
		return hold("evidence-first")
	}
	first, err = waitForDeploymentHandoffRoots(ctx, e, record, first)
	if err != nil {
		return hold("evidence-first")
	}
	preservedFirst, err := stableCompletionProjection(first, record)
	if err != nil {
		return hold("preservation-first")
	}
	if _, err = execute(ctx, "sleep", "5"); err != nil {
		return hold("sample-separation")
	}
	second, err := e.collect(ctx)
	if err != nil || completedState(second, record, true) != nil || platformRootState(second, record, e.revision, false) != nil {
		return hold("evidence-second")
	}
	preservedSecond, err := stableCompletionProjection(second, record)
	if err != nil || !reflect.DeepEqual(preservedFirst, preservedSecond) {
		return hold("preservation-second")
	}
	secondReceipt, err := e.optional(ctx, "--namespace=flux-system", "get", "configmap", "backstage-retirement-"+record.run, "--ignore-not-found", "--output=json")
	if err != nil || !reflect.DeepEqual(receipt, secondReceipt) || deploymentHandoffSourceSafe(ctx, e, record) != nil || currentMain && e.sourceCurrent(ctx) != nil {
		return hold("closing-source")
	}
	const result = "PORTAL-RETIREMENT=DEPLOYMENT_HANDOFF preservationSamples=2 mutation=NONE\n"
	if n, err := io.WriteString(out, result); err != nil || n != len(result) {
		return 2
	}
	return 0
}

func complete(args []string, out io.Writer, execute command) int {
	hold := func(stage string) int {
		_, _ = fmt.Fprintln(out, "PORTAL-RETIREMENT=HOLD_COMPLETION stage="+stage)
		return 2
	}
	deploymentRun := os.Getenv("PORTAL_RETIREMENT_DEPLOYMENT_RUN")
	if len(args) != 1 || args[0] != "--complete" || os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("GITHUB_REPOSITORY") != "devantler-tech/platform" || os.Getenv("GITHUB_EVENT_NAME") != "workflow_dispatch" || os.Getenv("GITHUB_REF") != "refs/heads/main" || os.Getenv("GITHUB_WORKFLOW_REF") != completionWorkflow || os.Getenv("GITHUB_RUN_ATTEMPT") != "1" || os.Getenv("PORTAL_RETIREMENT_CONFIRM") != "COMPLETE BACKSTAGE RETIREMENT JOURNAL ONLY" || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(os.Getenv("GITHUB_SHA")) || !regexp.MustCompile(`^[1-9][0-9]{0,19}$`).MatchString(os.Getenv("GITHUB_RUN_ID")) || !regexp.MustCompile(`^[1-9][0-9]{0,19}$`).MatchString(deploymentRun) || !regexp.MustCompile(`^latest@sha256:[a-f0-9]{64}$`).MatchString(os.Getenv("PORTAL_RETIREMENT_REVISION")) {
		return hold("confirmation")
	}
	e := engine{execute: execute, revision: os.Getenv("PORTAL_RETIREMENT_REVISION"), sha: os.Getenv("GITHUB_SHA")}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()
	if e.sourceCurrent(ctx) != nil || completionAccepted(ctx, e, deploymentRun) != nil {
		return hold("accepted-deployment")
	}
	namespace, err := e.read(ctx, "get", "namespace", "backstage", "--output=json")
	if err != nil || !metadataValid(namespace) || namespace["kind"] != "Namespace" {
		return hold("namespace")
	}
	record, err := parseCompletionRecord(text(field(namespace, "metadata", "annotations", fenceAnnotation)), namespace)
	if err != nil {
		return hold("journal")
	}
	e.fence, e.retiring, e.ownerUID = record.raw, record.retiring, record.releaseUID
	receipt, err := e.optional(ctx, "--namespace=flux-system", "get", "configmap", "backstage-retirement-"+record.run, "--ignore-not-found", "--output=json")
	if err != nil || !completionReceiptValid(receipt, record) {
		return hold("receipt")
	}
	first, err := e.collect(ctx)
	if err != nil || completedState(first, record, true) != nil || platformRootState(first, record, e.revision, false) != nil {
		return hold("evidence-first")
	}
	before, err := stableCompletionProjection(first, record)
	if err != nil {
		return hold("preservation-first")
	}
	if _, err = execute(ctx, "sleep", "5"); err != nil {
		return hold("sample-separation")
	}
	second, err := e.collect(ctx)
	if err != nil || completedState(second, record, true) != nil || platformRootState(second, record, e.revision, false) != nil {
		return hold("evidence-second")
	}
	again, err := stableCompletionProjection(second, record)
	if err != nil || !reflect.DeepEqual(before, again) {
		return hold("preservation-second")
	}
	secondReceipt, err := e.optional(ctx, "--namespace=flux-system", "get", "configmap", "backstage-retirement-"+record.run, "--ignore-not-found", "--output=json")
	if err != nil || !reflect.DeepEqual(receipt, secondReceipt) || e.sourceCurrent(ctx) != nil || completionAccepted(ctx, e, deploymentRun) != nil {
		return hold("write-precondition")
	}
	patch := append(tests(second.namespace), map[string]any{"op": "test", "path": "/metadata/annotations/platform.devantler.tech~1backstage-retirement", "value": record.raw}, map[string]any{"op": "remove", "path": "/metadata/annotations/platform.devantler.tech~1backstage-retirement"})
	updated, err := e.write(ctx, patch, "patch", "namespace", "backstage", "--type=json", "--patch-file=REQUEST", "--output=json")
	if err != nil || !metadataValid(updated) || field(updated, "metadata", "uid") != record.namespaceUID || field(updated, "metadata", "annotations", fenceAnnotation) != nil {
		return hold("patch")
	}
	e.fence = ""
	var after snapshot
	readyObserved := false
	for i := 0; i < 180; i++ {
		after, err = e.collect(ctx)
		if err == nil && completedState(after, record, false) == nil && platformRootState(after, record, e.revision, true) == nil {
			kept, preservationErr := stableCompletionProjection(after, record)
			if preservationErr == nil && reflect.DeepEqual(before, kept) {
				readyObserved = true
				break
			}
		}
		if _, sleepErr := execute(ctx, "sleep", "10"); sleepErr != nil {
			return hold("postcondition")
		}
	}
	if !readyObserved {
		return hold("postcondition")
	}
	for sample := 0; sample < 2; sample++ {
		if _, err = execute(ctx, "sleep", "5"); err != nil {
			return hold("final-separation")
		}
		after, err = e.collect(ctx)
		if err != nil || completedState(after, record, false) != nil || platformRootState(after, record, e.revision, true) != nil {
			return hold("final-evidence")
		}
		kept, err := stableCompletionProjection(after, record)
		if err != nil || !reflect.DeepEqual(before, kept) {
			return hold("final-preservation")
		}
		finalReceipt, receiptErr := e.optional(ctx, "--namespace=flux-system", "get", "configmap", "backstage-retirement-"+record.run, "--ignore-not-found", "--output=json")
		if receiptErr != nil || !reflect.DeepEqual(receipt, finalReceipt) {
			return hold("final-receipt")
		}
	}
	keys := make([]string, 0, len(e.retiring))
	for key := range e.retiring {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) != 5 {
		return hold("target-census")
	}
	const result = "PORTAL-RETIREMENT=COMPLETED journal=REMOVED preservationSamples=2 flux=READY dataRetirement=UNASSESSED\n"
	if n, err := io.WriteString(out, result); err != nil || n != len(result) {
		return 2
	}
	return 0
}
