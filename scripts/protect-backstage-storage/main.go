package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/portalstorage"
	"gopkg.in/yaml.v3"
)

type command func(context.Context, string, ...string) ([]byte, error)
type pause func(context.Context, time.Duration) error

const responseLimit = 4 << 20
const journal = "platform.devantler.tech/backstage-retirement"

type engine struct {
	execute                      command
	sha, revision, deploymentRun string
}
type snapshot struct {
	bindings  []portalstorage.Binding
	preserved map[string]map[string]any
}

// run checks the protected invocation and separated snapshots; only execute mode can submit guarded retention changes.
func run(ctx context.Context, args []string, env func(string) string, execute command, wait pause, out io.Writer) error {
	check := reflect.DeepEqual(args, []string{"--check"}) && env("PORTAL_STORAGE_OPERATION") == "check" && env("PORTAL_STORAGE_CONFIRM") == "CHECK BACKSTAGE STORAGE ONLY"
	protect := reflect.DeepEqual(args, []string{"--execute"}) && env("PORTAL_STORAGE_OPERATION") == "protect" && env("PORTAL_STORAGE_CONFIRM") == "PROTECT BACKSTAGE STORAGE ONLY"
	if !check && !protect || env("GITHUB_ACTIONS") != "true" || env("GITHUB_REPOSITORY") != "devantler-tech/platform" || env("GITHUB_EVENT_NAME") != "workflow_dispatch" || env("GITHUB_REF") != "refs/heads/main" || env("GITHUB_WORKFLOW_REF") != "devantler-tech/platform/.github/workflows/protect-backstage-storage.yaml@refs/heads/main" || env("GITHUB_RUN_ATTEMPT") != "1" || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(env("GITHUB_SHA")) || !regexp.MustCompile(`^latest@sha256:[a-f0-9]{64}$`).MatchString(env("PORTAL_STORAGE_REVISION")) {
		return errors.New("protected main invocation unverified")
	}
	for _, key := range []string{"GITHUB_RUN_ID", "PORTAL_STORAGE_DEPLOYMENT_RUN"} {
		if !regexp.MustCompile(`^[1-9][0-9]{0,19}$`).MatchString(env(key)) {
			return errors.New("protected run identity unverified")
		}
	}
	e := engine{execute: execute, sha: env("GITHUB_SHA"), revision: env("PORTAL_STORAGE_REVISION"), deploymentRun: env("PORTAL_STORAGE_DEPLOYMENT_RUN")}
	baseline, err := e.collect(ctx, false)
	if err != nil {
		return err
	}
	policies := map[string]string{}
	for _, b := range baseline.bindings {
		policies[str(field(b.Volume, "metadata", "name"))] = str(field(b.Volume, "spec", "persistentVolumeReclaimPolicy"))
	}
	if err := wait(ctx, 5*time.Second); err != nil {
		return errors.New("sample separation failed")
	}
	second, err := e.collect(ctx, false)
	if err != nil {
		return err
	}
	if !unchanged(baseline, second, policies) {
		return errors.New("initial separated samples differ")
	}
	if check {
		wouldChange := 0
		for _, policy := range policies {
			if policy == "Delete" {
				wouldChange++
			}
		}
		return writeReceipt(out, map[string]any{"result": "CHECKED", "claims": len(second.bindings), "changed": 0, "wouldChange": wouldChange, "alreadyRetained": len(second.bindings) - wouldChange, "separatedSamples": true, "retirement": "NOT_EXECUTED"})
	}
	changed := 0
	for _, binding := range baseline.bindings {
		name := str(field(binding.Volume, "metadata", "name"))
		if policies[name] == "Retain" {
			continue
		}
		fresh, err := e.collect(ctx, false)
		if err != nil || !unchanged(baseline, fresh, policies) {
			return errors.New("pre-write state changed")
		}
		var volume map[string]any
		for _, b := range fresh.bindings {
			if field(b.Volume, "metadata", "name") == name {
				volume = b.Volume
			}
		}
		if volume == nil {
			return errors.New("pre-write target missing")
		}
		if err := e.protect(ctx, volume); err != nil {
			return err
		}
		policies[name] = "Retain"
		changed++
		post, err := e.collect(ctx, false)
		if err != nil || !unchanged(baseline, post, policies) {
			return errors.New("post-write state unverified; protections retained")
		}
	}
	first, err := e.collect(ctx, true)
	if err != nil || !unchanged(baseline, first, policies) {
		return errors.New("physical retention readback unverified")
	}
	if err := wait(ctx, 5*time.Second); err != nil {
		return errors.New("sample separation failed")
	}
	last, err := e.collect(ctx, true)
	if err != nil || !unchanged(baseline, last, policies) {
		return errors.New("final separated samples differ")
	}
	return writeReceipt(out, map[string]any{"result": "PROTECTED", "claims": len(last.bindings), "changed": changed, "alreadyRetained": len(last.bindings) - changed, "separatedSamples": true, "retirement": "NOT_EXECUTED"})
}

// writeReceipt requires a complete compact receipt, never private evidence.
func writeReceipt(out io.Writer, result map[string]any) error {
	receipt, err := json.Marshal(result)
	if err != nil {
		return errors.New("result encoding failed")
	}
	receipt = append(receipt, '\n')
	if n, err := out.Write(receipt); err != nil || n != len(receipt) {
		return errors.New("terminal receipt incomplete; protections retained")
	}
	return nil
}

// obj returns a decoded object or nil; callers treat missing fields as unverified evidence.
func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }

// str accepts only string-valued identity fields.
func str(v any) string { s, _ := v.(string); return s }

// number accepts integer-valued fields from the bounded YAML decoder.
func number(v any) int { n, _ := v.(int); return n }

// array accepts only list-valued decoded evidence.
func array(v any) []any { a, _ := v.([]any); return a }

// field reads nested evidence without inventing a default for an absent value.
func field(m map[string]any, keys ...string) any {
	var v any = m
	for _, k := range keys {
		v = obj(v)[k]
	}
	return v
}

// decode rejects oversized, malformed or non-object responses before evidence is used.
func decode(b []byte) (map[string]any, error) {
	var m map[string]any
	if len(b) > responseLimit || !json.Valid(b) || yaml.Unmarshal(b, &m) != nil || m == nil {
		return nil, errors.New("incomplete response")
	}
	return m, nil
}

// valid requires the expected API identity, UID, resource version and non-terminating lifecycle.
func valid(r map[string]any, api, kind, ns, name string) bool {
	scope := field(r, "metadata", "namespace")
	return r["apiVersion"] == api && r["kind"] == kind && (scope == ns || ns == "" && scope == nil) && field(r, "metadata", "name") == name && str(field(r, "metadata", "uid")) != "" && str(field(r, "metadata", "resourceVersion")) != "" && field(r, "metadata", "deletionTimestamp") == nil
}

// read uses the fixed protected production context with bounded responses and request duration.
func (e engine) read(ctx context.Context, args ...string) (map[string]any, error) {
	b, err := e.execute(ctx, "kubectl", append([]string{"--context=admin@prod", "--request-timeout=30s"}, args...)...)
	if err != nil {
		return nil, errors.New("read unavailable")
	}
	r, err := decode(b)
	if err != nil {
		return nil, err
	}
	// Kubernetes' native typed lists omit TypeMeta on individual items. Bind
	// only absent fields to these exact core API list envelopes, never to a
	// generic List, a foreign API, or an explicitly conflicting item identity.
	if r["apiVersion"] == "v1" {
		kind := map[string]string{"PersistentVolumeClaimList": "PersistentVolumeClaim", "PersistentVolumeList": "PersistentVolume", "PodList": "Pod"}[str(r["kind"])]
		if kind != "" {
			for _, raw := range array(r["items"]) {
				item := obj(raw)
				if item == nil {
					return nil, errors.New("typed list item identity unverified")
				}
				for key, expected := range map[string]string{"apiVersion": "v1", "kind": kind} {
					if value, present := item[key]; present {
						if value != expected {
							return nil, errors.New("typed list item identity unverified")
						}
					} else {
						item[key] = expected
					}
				}
			}
		}
	}
	return r, nil
}

// current requires the reviewed source guard's complete current-main proof.
func (e engine) current(ctx context.Context) bool {
	b, err := e.execute(ctx, "bash", "scripts/verify-prod-recovery-source.sh", e.sha)
	return err == nil && strings.TrimSpace(string(b)) == "PROD_RECOVERY_SOURCE=PASS"
}

// api reads bounded JSON deployment evidence through the short-lived job identity.
func (e engine) api(ctx context.Context, path string) ([]byte, error) {
	b, err := e.execute(ctx, "gh", "api", "--hostname=github.com", path)
	if err != nil || len(b) > responseLimit || !json.Valid(b) {
		return nil, errors.New("deployment evidence unavailable")
	}
	return b, nil
}

// accepted joins the exact successful normal publisher, production deployment status and signed artifact witness.
func (e engine) accepted(ctx context.Context) error {
	b, err := e.api(ctx, "repos/devantler-tech/platform/actions/runs/"+e.deploymentRun)
	if err != nil {
		return errors.New("deployment run unavailable")
	}
	r, err := decode(b)
	ci := r["path"] == ".github/workflows/ci.yaml" && r["event"] == "merge_group"
	cd := r["path"] == ".github/workflows/cd.yaml" && r["event"] == "workflow_dispatch"
	if err != nil || fmt.Sprint(r["id"]) != e.deploymentRun || r["head_sha"] != e.sha || r["status"] != "completed" || r["conclusion"] != "success" || field(r, "repository", "full_name") != "devantler-tech/platform" || !ci && !cd {
		return errors.New("deployment run identity unverified")
	}
	branch, attempt := str(r["head_branch"]), fmt.Sprint(r["run_attempt"])
	if !regexp.MustCompile(`^[1-9][0-9]{0,5}$`).MatchString(attempt) || ci && !regexp.MustCompile(`^gh-readonly-queue/main/pr-[1-9][0-9]*-[a-f0-9]{40}$`).MatchString(branch) || cd && branch != "main" {
		return errors.New("deployment run identity unverified")
	}
	b, err = e.api(ctx, "repos/devantler-tech/platform/deployments?sha="+e.sha+"&environment=prod&per_page=100")
	var ds []map[string]any
	if err != nil || yaml.Unmarshal(b, &ds) != nil || len(ds) == 0 || len(ds) >= 100 {
		return errors.New("deployment census unavailable")
	}
	for _, d := range ds {
		id := fmt.Sprint(d["id"])
		if !regexp.MustCompile(`^[1-9][0-9]{0,19}$`).MatchString(id) || d["sha"] != e.sha || d["environment"] != "prod" {
			return errors.New("deployment identity unverified")
		}
		b, err = e.api(ctx, "repos/devantler-tech/platform/deployments/"+id+"/statuses?per_page=100")
		var statuses []map[string]any
		if err != nil || yaml.Unmarshal(b, &statuses) != nil || len(statuses) == 0 || len(statuses) >= 100 {
			return errors.New("deployment status unavailable")
		}
		s := statuses[0]
		if s["state"] == "success" && s["environment"] == "prod" && regexp.MustCompile(`^https://github\.com/devantler-tech/platform/actions/runs/`+e.deploymentRun+`/job/[1-9][0-9]*$`).MatchString(str(s["log_url"])) {
			return e.artifactAccepted(ctx, str(r["path"]), "refs/heads/"+branch, attempt)
		}
	}
	return errors.New("deployment acceptance unverified")
}

// ready requires settled current-generation Flux readiness, not an older successful condition.
func ready(r map[string]any) bool {
	g := number(field(r, "metadata", "generation"))
	if g < 1 || number(field(r, "status", "observedGeneration")) != g || field(r, "spec", "suspend") == true {
		return false
	}
	count := 0
	for _, v := range array(field(r, "status", "conditions")) {
		c := obj(v)
		if c["type"] == "Reconciling" && c["status"] != "False" {
			return false
		}
		if c["type"] == "Ready" {
			if c["status"] != "True" || number(c["observedGeneration"]) != g {
				return false
			}
			count++
		}
	}
	return count == 1
}

// collect validates the accepted revision, fixed database, complete reciprocal storage census and healthy claim consumers.
func (e engine) collect(ctx context.Context, requireRetain bool) (snapshot, error) {
	s := snapshot{preserved: map[string]map[string]any{}}
	if !e.current(ctx) {
		return s, errors.New("current main unverified")
	}
	if err := e.accepted(ctx); err != nil {
		return s, err
	}
	for _, name := range []string{"flux-system", "bootstrap", "infrastructure-controllers", "infrastructure", "apps"} {
		r, err := e.read(ctx, "--namespace=flux-system", "get", "kustomization", name, "--output=json")
		sourceNamespace := field(r, "spec", "sourceRef", "namespace")
		if err != nil || !valid(r, "kustomize.toolkit.fluxcd.io/v1", "Kustomization", "flux-system", name) || !ready(r) || field(r, "status", "lastAppliedRevision") != e.revision || field(r, "spec", "sourceRef", "kind") != "OCIRepository" || field(r, "spec", "sourceRef", "name") != "flux-system" || sourceNamespace != nil && sourceNamespace != "" && sourceNamespace != "flux-system" {
			return s, errors.New("deployed layers unverified")
		}
		s.preserved["layer/"+name] = projection(r, false)
	}
	r, err := e.read(ctx, "--namespace=flux-system", "get", "ocirepository", "flux-system", "--output=json")
	if err != nil || !valid(r, "source.toolkit.fluxcd.io/v1", "OCIRepository", "flux-system", "flux-system") || !ready(r) || field(r, "status", "artifact", "revision") != e.revision {
		return s, errors.New("deployed source unverified")
	}
	s.preserved["source"] = projection(r, false)
	ns, err := e.read(ctx, "get", "namespace", "backstage", "--output=json")
	if err != nil || !valid(ns, "v1", "Namespace", "", "backstage") || field(ns, "status", "phase") != "Active" || field(ns, "metadata", "annotations", journal) != nil {
		return s, errors.New("namespace retirement journal or lifecycle unverified")
	}
	s.preserved["namespace"] = projection(ns, false)
	db, err := e.read(ctx, "--namespace=backstage", "get", "clusters.postgresql.cnpg.io", "backstage-db", "--output=json")
	if err != nil || !valid(db, "postgresql.cnpg.io/v1", "Cluster", "backstage", "backstage-db") {
		return s, errors.New("database identity unverified")
	}
	instances := number(field(db, "spec", "instances"))
	primary := str(field(db, "status", "currentPrimary"))
	if instances < 1 || instances > 10 || number(field(db, "status", "readyInstances")) != instances || primary == "" || primary != field(db, "status", "targetPrimary") {
		return s, errors.New("database availability unverified")
	}
	s.preserved["database"] = projection(db, false)
	claims, err := e.read(ctx, "get", "--raw=/api/v1/namespaces/backstage/persistentvolumeclaims")
	if err != nil || str(field(claims, "metadata", "resourceVersion")) == "" {
		return s, errors.New("claim census unavailable")
	}
	volumes, err := e.read(ctx, "get", "--raw=/api/v1/persistentvolumes")
	if err != nil || str(field(volumes, "metadata", "resourceVersion")) == "" {
		return s, errors.New("volume census unavailable")
	}
	s.bindings, err = portalstorage.ValidateBindings(str(field(db, "metadata", "uid")), claims, volumes, requireRetain)
	if err != nil || len(s.bindings) != instances {
		return s, errors.New("complete database storage join unverified")
	}
	handles, claimNames := map[string]bool{}, map[string]bool{}
	for _, b := range s.bindings {
		handle := str(field(b.Volume, "spec", "csi", "volumeHandle"))
		if field(b.Claim, "spec", "storageClassName") != "longhorn-wffc" || field(b.Volume, "spec", "csi", "driver") != "driver.longhorn.io" || handle == "" || handles[handle] {
			return s, errors.New("physical storage source unverified")
		}
		handles[handle] = true
		cn, vn := str(field(b.Claim, "metadata", "name")), str(field(b.Volume, "metadata", "name"))
		claimNames[cn] = true
		s.preserved["claim/"+cn] = projection(b.Claim, false)
		s.preserved["volume/"+vn] = projection(b.Volume, true)
	}
	pods, err := e.read(ctx, "get", "--raw=/api/v1/namespaces/backstage/pods")
	if err != nil {
		return s, errors.New("pod census unavailable")
	}
	if err := validatePods(pods, str(field(db, "metadata", "uid")), primary, instances, claimNames, s.preserved); err != nil {
		return s, err
	}
	if !e.current(ctx) {
		return s, errors.New("current main changed during read")
	}
	if err := e.accepted(ctx); err != nil {
		return s, err
	}
	return s, nil
}

// validatePods requires every database instance to own and mount its captured claim and rejects foreign consumers.
func validatePods(pods map[string]any, dbUID, primary string, instances int, claims map[string]bool, kept map[string]map[string]any) error {
	items, ok := pods["items"].([]any)
	remaining := field(pods, "metadata", "remainingItemCount")
	if pods["apiVersion"] != "v1" || pods["kind"] != "PodList" || !ok || len(items) > 512 || str(field(pods, "metadata", "resourceVersion")) == "" || field(pods, "metadata", "continue") != nil && field(pods, "metadata", "continue") != "" || remaining != nil && remaining != 0 {
		return errors.New("pod census incomplete")
	}
	seen, used, uidSeen := map[string]bool{}, map[string]bool{}, map[string]bool{}
	count := 0
	primaryReady := false
	for _, raw := range items {
		p := obj(raw)
		name := str(field(p, "metadata", "name"))
		uid := str(field(p, "metadata", "uid"))
		if !valid(p, "v1", "Pod", "backstage", name) || seen[name] || uidSeen[uid] {
			return errors.New("pod identity incomplete")
		}
		seen[name], uidSeen[uid] = true, true
		owned := false
		owners, typed := field(p, "metadata", "ownerReferences").([]any)
		if typed && len(owners) == 1 {
			o := obj(owners[0])
			owned = o["apiVersion"] == "postgresql.cnpg.io/v1" && o["kind"] == "Cluster" && o["name"] == "backstage-db" && o["uid"] == dbUID && o["controller"] == true
		}
		labelled := field(p, "metadata", "labels", "cnpg.io/cluster") == "backstage-db"
		if owned != labelled {
			return errors.New("database pod ownership differs")
		}
		mounted := 0
		for _, v := range array(field(p, "spec", "volumes")) {
			cn := str(field(obj(v), "persistentVolumeClaim", "claimName"))
			if !claims[cn] {
				continue
			}
			if !owned || used[cn] || cn != name {
				return errors.New("retained claim consumer differs")
			}
			used[cn] = true
			mounted++
		}
		if !owned {
			continue
		}
		readyCount := 0
		for _, c := range array(field(p, "status", "conditions")) {
			if field(obj(c), "type") == "Ready" && field(obj(c), "status") == "True" {
				readyCount++
			}
		}
		if field(p, "status", "phase") != "Running" || readyCount != 1 || mounted != 1 {
			return errors.New("database pod availability unverified")
		}
		count++
		primaryReady = primaryReady || name == primary
		kept["pod/"+name] = projection(p, false)
	}
	if count != instances || !primaryReady || len(used) != len(claims) {
		return errors.New("database pod coverage incomplete")
	}
	return nil
}

// projection preserves identities, metadata and specifications while excluding only API bookkeeping and an explicitly allowed policy transition.
func projection(r map[string]any, ignorePolicy bool) map[string]any {
	metadata := map[string]any{}
	for k, v := range obj(r["metadata"]) {
		if k != "resourceVersion" && k != "managedFields" {
			metadata[k] = v
		}
	}
	spec := map[string]any{}
	for k, v := range obj(r["spec"]) {
		if !ignorePolicy || k != "persistentVolumeReclaimPolicy" {
			spec[k] = v
		}
	}
	return map[string]any{"apiVersion": r["apiVersion"], "kind": r["kind"], "metadata": metadata, "spec": spec}
}

// unchanged checks the complete preserved census and each expected reclaim policy.
func unchanged(baseline, current snapshot, policies map[string]string) bool {
	if !reflect.DeepEqual(baseline.preserved, current.preserved) || len(current.bindings) != len(policies) {
		return false
	}
	for _, b := range current.bindings {
		if field(b.Volume, "spec", "persistentVolumeReclaimPolicy") != policies[str(field(b.Volume, "metadata", "name"))] {
			return false
		}
	}
	return true
}

// retentionPatch tests exact volume identity and the full specification before changing only Delete to Retain.
func retentionPatch(v map[string]any) ([]byte, error) {
	if field(v, "spec", "persistentVolumeReclaimPolicy") != "Delete" {
		return nil, errors.New("only Delete to Retain is permitted")
	}
	return json.Marshal([]map[string]any{{"op": "test", "path": "/apiVersion", "value": "v1"}, {"op": "test", "path": "/kind", "value": "PersistentVolume"}, {"op": "test", "path": "/metadata/uid", "value": field(v, "metadata", "uid")}, {"op": "test", "path": "/metadata/resourceVersion", "value": field(v, "metadata", "resourceVersion")}, {"op": "test", "path": "/spec", "value": v["spec"]}, {"op": "replace", "path": "/spec/persistentVolumeReclaimPolicy", "value": "Retain"}})
}

// protect submits one native guarded patch and rejects ambiguous or widened readback without retrying.
func (e engine) protect(ctx context.Context, volume map[string]any) error {
	patch, err := retentionPatch(volume)
	if err != nil {
		return err
	}
	b, err := e.execute(ctx, "kubectl", "--context=admin@prod", "--request-timeout=30s", "patch", "persistentvolume", str(field(volume, "metadata", "name")), "--type=json", "--patch", string(patch), "--output=json", "--field-manager=portal-storage-protection")
	if err != nil {
		return errors.New("volume patch outcome unverified; no retry")
	}
	result, err := decode(b)
	if err != nil || field(result, "spec", "persistentVolumeReclaimPolicy") != "Retain" || str(field(result, "metadata", "resourceVersion")) == "" || !reflect.DeepEqual(projection(volume, true), projection(result, true)) {
		return errors.New("volume patch readback differs; no retry")
	}
	return nil
}

type boundedBuffer struct{ bytes.Buffer }

// Write limits child stdout while retaining ordinary buffer semantics.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > responseLimit-b.Len() {
		return 0, errors.New("response limit exceeded")
	}
	return b.Buffer.Write(p)
}

// execute bounds child runtime and output, discards private diagnostics and never forwards a separate registry token.
func execute(ctx context.Context, name string, args ...string) ([]byte, error) {
	verifier := name == "gh" && len(args) >= 2 && args[0] == "attestation" && args[1] == "verify"
	if verifier && strings.TrimSpace(os.Getenv("GH_TOKEN")) == "" {
		return nil, errors.New("registry reader unavailable")
	}
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "PORTAL_STORAGE_GHCR_READER_TOKEN=") {
			continue
		}
		env = append(env, entry)
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	var out boundedBuffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, errors.New("command failed")
	}
	return out.Bytes(), nil
}

// wait separates samples with a cancellable timer.
func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// main runs the protected command under a finite deadline and reports HOLD without raw evidence on failure.
func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Getenv, execute, wait, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "PORTAL_STORAGE_PROTECTION=HOLD")
		fmt.Fprintln(os.Stderr, "PORTAL_STORAGE_REASON="+holdReason(err))
		os.Exit(1)
	}
}
