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

	"gopkg.in/yaml.v3"
)

type command func(context.Context, string, ...string) ([]byte, error)

const target = "20Gi"
const responseLimit = 4 << 20

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func str(v any) string         { s, _ := v.(string); return s }
func number(v any) int         { n, _ := v.(int); return n }
func array(v any) []any        { a, _ := v.([]any); return a }
func field(m map[string]any, keys ...string) any {
	var v any = m
	for _, key := range keys {
		v = obj(v)[key]
	}
	return v
}
func decode(b []byte) (map[string]any, error) {
	var m map[string]any
	if len(b) > responseLimit || !json.Valid(b) || yaml.Unmarshal(b, &m) != nil || m == nil {
		return nil, errors.New("incomplete response")
	}
	return m, nil
}
func valid(r map[string]any, api, kind, ns, name string) bool {
	namespace := field(r, "metadata", "namespace")
	return r["apiVersion"] == api && r["kind"] == kind && (namespace == ns || ns == "" && namespace == nil) && validIdentity(r, name)
}
func validIdentity(r map[string]any, name string) bool {
	return field(r, "metadata", "name") == name && str(field(r, "metadata", "uid")) != "" && str(field(r, "metadata", "resourceVersion")) != "" && field(r, "metadata", "deletionTimestamp") == nil
}

type engine struct {
	execute                             command
	sha, revision, runID, deploymentRun string
}

func (e engine) read(ctx context.Context, args ...string) (map[string]any, error) {
	b, err := e.execute(ctx, "kubectl", append([]string{"--context=admin@prod", "--request-timeout=20s"}, args...)...)
	if err != nil {
		return nil, errors.New("read unavailable")
	}
	return decode(b)
}
func (e engine) current(ctx context.Context) bool {
	b, err := e.execute(ctx, "bash", "scripts/verify-prod-recovery-source.sh", e.sha)
	return err == nil && strings.TrimSpace(string(b)) == "PROD_RECOVERY_SOURCE=PASS"
}
func (e engine) api(ctx context.Context, path string) ([]byte, error) {
	b, err := e.execute(ctx, "gh", "api", "--hostname=github.com", path)
	if err != nil || len(b) > responseLimit || !json.Valid(b) {
		return nil, errors.New("deployment evidence unavailable")
	}
	return b, nil
}
func (e engine) accepted(ctx context.Context) bool {
	b, err := e.api(ctx, "repos/devantler-tech/platform/actions/runs/"+e.deploymentRun)
	if err != nil {
		return false
	}
	r, err := decode(b)
	ci := r["path"] == ".github/workflows/ci.yaml" && r["event"] == "merge_group"
	cd := r["path"] == ".github/workflows/cd.yaml" && r["event"] == "workflow_dispatch"
	if err != nil || fmt.Sprint(r["id"]) != e.deploymentRun || r["head_sha"] != e.sha || r["status"] != "completed" || r["conclusion"] != "success" || field(r, "repository", "full_name") != "devantler-tech/platform" || !ci && !cd {
		return false
	}
	b, err = e.api(ctx, "repos/devantler-tech/platform/deployments?sha="+e.sha+"&environment=prod&per_page=100")
	var ds []map[string]any
	if err != nil || yaml.Unmarshal(b, &ds) != nil || len(ds) == 0 || len(ds) >= 100 {
		return false
	}
	for _, d := range ds {
		id := fmt.Sprint(d["id"])
		if !regexp.MustCompile(`^[1-9][0-9]{0,19}$`).MatchString(id) || d["sha"] != e.sha || d["environment"] != "prod" {
			return false
		}
		b, err = e.api(ctx, "repos/devantler-tech/platform/deployments/"+id+"/statuses?per_page=100")
		var statuses []map[string]any
		if err != nil || yaml.Unmarshal(b, &statuses) != nil || len(statuses) == 0 || len(statuses) >= 100 {
			return false
		}
		s := statuses[0]
		if s["state"] == "success" && s["environment"] == "prod" && strings.HasPrefix(str(s["log_url"]), "https://github.com/devantler-tech/platform/actions/runs/"+e.deploymentRun+"/job/") {
			return true
		}
	}
	return false
}
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

type snapshot struct {
	claims, volumes, pods map[string]map[string]any
	workload, class       map[string]any
}

func (e engine) collect(ctx context.Context) (snapshot, error) {
	s := snapshot{claims: map[string]map[string]any{}, volumes: map[string]map[string]any{}, pods: map[string]map[string]any{}}
	for _, name := range []string{"flux-system", "infrastructure-controllers", "infrastructure", "apps"} {
		k, err := e.read(ctx, "--namespace=flux-system", "get", "kustomization", name, "--output=json")
		if err != nil || !valid(k, "kustomize.toolkit.fluxcd.io/v1", "Kustomization", "flux-system", name) || !ready(k) || field(k, "status", "lastAppliedRevision") != e.revision {
			return s, errors.New("source unsettled")
		}
	}
	r, err := e.read(ctx, "--namespace=flux-system", "get", "ocirepository", "flux-system", "--output=json")
	if err != nil || !valid(r, "source.toolkit.fluxcd.io/v1", "OCIRepository", "flux-system", "flux-system") || !ready(r) || field(r, "status", "artifact", "revision") != e.revision {
		return s, errors.New("artifact unverified")
	}
	s.workload, err = e.read(ctx, "--namespace=openbao", "get", "statefulset", "openbao", "--output=json")
	if err != nil || !valid(s.workload, "apps/v1", "StatefulSet", "openbao", "openbao") || number(field(s.workload, "spec", "replicas")) != 3 || number(field(s.workload, "metadata", "generation")) < 1 || field(s.workload, "status", "observedGeneration") != field(s.workload, "metadata", "generation") || number(field(s.workload, "status", "readyReplicas")) != 3 {
		return s, errors.New("workload unready")
	}
	s.class, err = e.read(ctx, "get", "storageclass", "hcloud", "--output=json")
	if err != nil || !valid(s.class, "storage.k8s.io/v1", "StorageClass", "", "hcloud") || !validIdentity(s.class, "hcloud") || s.class["provisioner"] != "csi.hetzner.cloud" || s.class["allowVolumeExpansion"] != true {
		return s, errors.New("expansion capability unverified")
	}
	for i := 0; i < 3; i++ {
		podName := fmt.Sprintf("openbao-%d", i)
		name := "audit-" + podName
		c, err := e.read(ctx, "--namespace=openbao", "get", "persistentvolumeclaim", name, "--output=json")
		if err != nil || !valid(c, "v1", "PersistentVolumeClaim", "openbao", name) || field(c, "status", "phase") != "Bound" || field(c, "spec", "storageClassName") != "hcloud" || field(c, "metadata", "labels", "app.kubernetes.io/name") != "openbao" {
			return s, errors.New("claim identity unverified")
		}
		req, cap := str(field(c, "spec", "resources", "requests", "storage")), str(field(c, "status", "capacity", "storage"))
		_, typedConditions := field(c, "status", "conditions").([]any)
		if req != "1Gi" && req != "10Gi" && req != target || cap != "10Gi" && cap != target || field(c, "status", "conditions") != nil && !typedConditions {
			return s, errors.New("unreviewed capacity")
		}
		pvName := str(field(c, "spec", "volumeName"))
		if !regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`).MatchString(pvName) {
			return s, errors.New("binding unavailable")
		}
		v, err := e.read(ctx, "get", "persistentvolume", pvName, "--output=json")
		if err != nil || !valid(v, "v1", "PersistentVolume", "", pvName) || !validIdentity(v, pvName) || field(v, "status", "phase") != "Bound" || field(v, "spec", "storageClassName") != "hcloud" || field(v, "spec", "csi", "driver") != "csi.hetzner.cloud" || str(field(v, "spec", "csi", "volumeHandle")) == "" || field(v, "spec", "claimRef", "namespace") != "openbao" || field(v, "spec", "claimRef", "name") != name || field(v, "spec", "claimRef", "uid") != field(c, "metadata", "uid") {
			return s, errors.New("volume identity unverified")
		}
		pvCapacity := field(v, "spec", "capacity", "storage")
		if pvCapacity != "10Gi" && pvCapacity != target {
			return s, errors.New("unreviewed backing capacity")
		}
		pod, err := e.read(ctx, "--namespace=openbao", "get", "pod", podName, "--output=json")
		if err != nil || !auditPod(pod, podName, name, str(field(s.workload, "metadata", "uid"))) {
			return s, errors.New("audit mount unverified")
		}
		status, err := e.read(ctx, "--namespace=openbao", "exec", podName, "--container=openbao", "--", "bao", "status", "-format=json")
		if err != nil || status["initialized"] != true || status["sealed"] != false || status["ha_enabled"] != true {
			return s, errors.New("service health unverified")
		}
		s.claims[name], s.volumes[name], s.pods[name] = c, v, pod
	}
	return s, nil
}
func auditPod(p map[string]any, name, claim, owner string) bool {
	if !valid(p, "v1", "Pod", "openbao", name) || field(p, "status", "phase") != "Running" {
		return false
	}
	healthy, owned, mounted, bound, containers := 0, 0, 0, 0, 0
	for _, v := range array(field(p, "status", "conditions")) {
		c := obj(v)
		if c["type"] == "Ready" {
			if c["status"] != "True" {
				return false
			}
			healthy++
		}
	}
	for _, v := range array(field(p, "metadata", "ownerReferences")) {
		o := obj(v)
		if o["apiVersion"] == "apps/v1" && o["kind"] == "StatefulSet" && o["name"] == "openbao" && o["uid"] == owner && o["controller"] == true {
			owned++
		}
	}
	for _, v := range array(field(p, "spec", "volumes")) {
		o := obj(v)
		if o["name"] == "audit" {
			if field(o, "persistentVolumeClaim", "claimName") != claim {
				return false
			}
			bound++
		}
	}
	for _, v := range array(field(p, "spec", "containers")) {
		c := obj(v)
		if c["name"] == "openbao" {
			containers++
			for _, v := range array(c["volumeMounts"]) {
				m := obj(v)
				if m["name"] == "audit" {
					if m["mountPath"] != "/openbao/audit" || m["readOnly"] == true || m["subPath"] != nil || m["subPathExpr"] != nil {
						return false
					}
					mounted++
				}
			}
		}
	}
	return healthy == 1 && owned == 1 && mounted == 1 && bound == 1 && containers == 1
}
func stableResource(r map[string]any, omitCapacity bool) map[string]any {
	b, _ := json.Marshal(r)
	copy, _ := decode(b)
	spec := obj(copy["spec"])
	if omitCapacity {
		delete(spec, "capacity")
	} else if requests := obj(field(copy, "spec", "resources", "requests")); requests != nil {
		delete(requests, "storage")
	}
	return map[string]any{"uid": field(copy, "metadata", "uid"), "name": field(copy, "metadata", "name"), "namespace": field(copy, "metadata", "namespace"), "owners": field(copy, "metadata", "ownerReferences"), "labels": field(copy, "metadata", "labels"), "annotations": field(copy, "metadata", "annotations"), "finalizers": field(copy, "metadata", "finalizers"), "spec": spec}
}
func preserved(a, b snapshot) bool {
	if !reflect.DeepEqual(stableResource(a.workload, false), stableResource(b.workload, false)) || !reflect.DeepEqual(a.class, b.class) {
		return false
	}
	for name, c := range a.claims {
		if !reflect.DeepEqual(stableResource(c, false), stableResource(b.claims[name], false)) || !reflect.DeepEqual(stableResource(a.volumes[name], true), stableResource(b.volumes[name], true)) || !reflect.DeepEqual(stableResource(a.pods[name], false), stableResource(b.pods[name], false)) {
			return false
		}
	}
	return true
}
func (e engine) write(ctx context.Context, value any, args ...string) (map[string]any, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp("", "openbao-audit-request-*.json")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(b); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	for i, arg := range args {
		if arg == "REQUEST" {
			args[i] = f.Name()
		}
		if arg == "--patch-file=REQUEST" {
			args[i] = "--patch-file=" + f.Name()
		}
	}
	return e.read(ctx, args...)
}
func run(args []string, out io.Writer, execute command, cost func(context.Context) bool) int {
	changed := false
	stage := "input"
	hold := func() int {
		code := "HOLD"
		if changed {
			code = "HOLD_CAPACITY_MAY_HAVE_CHANGED"
		}
		_, _ = fmt.Fprintln(out, "OPENBAO_AUDIT_CAPACITY="+code+" stage="+stage)
		return 2
	}
	snapshotHold := func(err error) int {
		if err != nil {
			switch err.Error() {
			case "source unsettled":
				stage = "root-source"
			case "artifact unverified":
				stage = "root-artifact"
			case "workload unready":
				stage = "workload"
			case "expansion capability unverified":
				stage = "storage-class"
			case "claim identity unverified":
				stage = "claim-identity"
			case "unreviewed capacity":
				stage = "claim-capacity"
			case "binding unavailable", "volume identity unverified":
				stage = "volume-binding"
			case "unreviewed backing capacity":
				stage = "volume-capacity"
			case "audit mount unverified":
				stage = "audit-mount"
			case "service health unverified":
				stage = "service-health"
			default:
				stage = "snapshot-read"
			}
		}
		return hold()
	}
	sha, rev, id, deploy := os.Getenv("GITHUB_SHA"), os.Getenv("AUDIT_CAPACITY_REVISION"), os.Getenv("GITHUB_RUN_ID"), os.Getenv("AUDIT_CAPACITY_DEPLOYMENT_RUN")
	if len(args) != 1 || args[0] != "--execute" && args[0] != "--preflight" || os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("GITHUB_EVENT_NAME") != "workflow_dispatch" || os.Getenv("GITHUB_WORKFLOW_REF") != "devantler-tech/platform/.github/workflows/expand-openbao-audit.yaml@refs/heads/main" || os.Getenv("GITHUB_REPOSITORY") != "devantler-tech/platform" || os.Getenv("GITHUB_REF") != "refs/heads/main" || os.Getenv("GITHUB_RUN_ATTEMPT") != "1" || os.Getenv("AUDIT_CAPACITY_CONFIRM") != "EXPAND OPENBAO AUDIT ONLY TO 20GI" || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(sha) || !regexp.MustCompile(`^latest@sha256:[0-9a-f]{64}$`).MatchString(rev) || !regexp.MustCompile(`^[1-9][0-9]{0,19}$`).MatchString(id) || !regexp.MustCompile(`^[1-9][0-9]{0,19}$`).MatchString(deploy) {
		return hold()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	e := engine{execute: execute, sha: sha, revision: rev, runID: id, deploymentRun: deploy}
	stage = "source"
	if !e.current(ctx) {
		return hold()
	}
	stage = "pricing"
	if !cost(ctx) {
		return hold()
	}
	stage = "accepted-deployment"
	if !e.accepted(ctx) {
		return hold()
	}
	stage = "initial-snapshot"
	before, err := e.collect(ctx)
	if err != nil {
		return snapshotHold(err)
	}
	stage = "separated-snapshot"
	if _, err = execute(ctx, "sleep", "5"); err != nil {
		return hold()
	}
	second, err := e.collect(ctx)
	if err != nil || !preserved(before, second) {
		return snapshotHold(err)
	}
	if args[0] == "--preflight" {
		if !e.current(ctx) {
			stage = "source"
			return hold()
		}
		result := "OPENBAO_AUDIT_CAPACITY=PREFLIGHT_OK claims=3 writes=0 consumptionReceipts=0\n"
		if n, err := io.WriteString(out, result); err != nil || n != len(result) {
			return 2
		}
		return 0
	}
	stage = "consumption-receipt"
	receiptName := "openbao-audit-expansion-" + id
	b, err := execute(ctx, "kubectl", "--context=admin@prod", "--request-timeout=20s", "--namespace=flux-system", "get", "configmap", receiptName, "--ignore-not-found", "--output=json")
	if err != nil || len(bytes.TrimSpace(b)) != 0 || !e.current(ctx) {
		return hold()
	}
	receipt := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": receiptName, "namespace": "flux-system"}, "immutable": true, "data": map[string]any{"source": sha, "revision": rev, "run": id, "target": target, "scope": "audit-only"}}
	r, err := e.write(ctx, receipt, "--namespace=flux-system", "create", "-f", "REQUEST", "--output=json")
	if err != nil || !valid(r, "v1", "ConfigMap", "flux-system", receiptName) || r["immutable"] != true || !reflect.DeepEqual(r["data"], receipt["data"]) {
		return hold()
	}
	receiptUID := field(r, "metadata", "uid")
	for i := 0; i < 3; i++ {
		stage = "before-patch"
		name := fmt.Sprintf("audit-openbao-%d", i)
		current, err := e.collect(ctx)
		if err != nil || !preserved(before, current) || !e.current(ctx) {
			return snapshotHold(err)
		}
		c := current.claims[name]
		if field(c, "spec", "resources", "requests", "storage") == target {
			continue
		}
		stage = "consumption-receipt"
		recorded, err := e.read(ctx, "--namespace=flux-system", "get", "configmap", receiptName, "--output=json")
		if err != nil || !valid(recorded, "v1", "ConfigMap", "flux-system", receiptName) || field(recorded, "metadata", "uid") != receiptUID || recorded["immutable"] != true || !reflect.DeepEqual(recorded["data"], receipt["data"]) {
			return hold()
		}
		stage = "pricing"
		if !cost(ctx) {
			return hold()
		}
		patch := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": field(c, "metadata", "uid")}, {"op": "test", "path": "/metadata/resourceVersion", "value": field(c, "metadata", "resourceVersion")}, {"op": "replace", "path": "/spec/resources/requests/storage", "value": target}}
		changed = true
		stage = "capacity-patch"
		r, err = e.write(ctx, patch, "--namespace=openbao", "patch", "persistentvolumeclaim", name, "--type=json", "--patch-file=REQUEST", "--output=json")
		if err != nil || !valid(r, "v1", "PersistentVolumeClaim", "openbao", name) || field(r, "metadata", "uid") != field(c, "metadata", "uid") || field(r, "spec", "resources", "requests", "storage") != target || !reflect.DeepEqual(stableResource(c, false), stableResource(r, false)) {
			return hold()
		}
	}
	for sample := 0; sample < 2; sample++ {
		stage = "resize-readback"
		complete := false
		for i := 0; i < 60; i++ {
			if _, err = execute(ctx, "sleep", "5"); err != nil {
				return hold()
			}
			after, err := e.collect(ctx)
			if err != nil || !preserved(before, after) || !e.current(ctx) {
				return snapshotHold(err)
			}
			complete = true
			for name, c := range after.claims {
				complete = complete && field(c, "spec", "resources", "requests", "storage") == target && field(c, "status", "capacity", "storage") == target && field(after.volumes[name], "spec", "capacity", "storage") == target && len(array(field(c, "status", "conditions"))) == 0
			}
			if complete {
				break
			}
		}
		if !complete {
			return hold()
		}
	}
	result := "OPENBAO_AUDIT_CAPACITY=EXPANDED claims=3 target=20Gi preservationSamples=2\n"
	if n, err := io.WriteString(out, result); err != nil || n != len(result) {
		return 2
	}
	return 0
}

type bounded struct{ bytes.Buffer }

func (b *bounded) Write(p []byte) (int, error) {
	if b.Len()+len(p) > responseLimit {
		return 0, errors.New("response exceeds bound")
	}
	return b.Buffer.Write(p)
}
func capture(ctx context.Context, p string, a ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, p, a...)
	var out, failure bounded
	cmd.Stdout = &out
	cmd.Stderr = &failure
	if cmd.Run() != nil {
		return nil, errors.New("command unavailable")
	}
	return out.Bytes(), nil
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, capture, priceAllowed)) }
