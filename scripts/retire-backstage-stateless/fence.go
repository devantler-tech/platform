package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const fenceAnnotation = "platform.devantler.tech/backstage-retirement"
const fencePolicyPath = "k8s/providers/hetzner/infrastructure/cluster-policies/restrict-backstage-retirement.yaml"
const nativeControllerImage = "ghcr.io/fluxcd/helm-controller:v1.5.5@sha256:b4b8f7715cc7cb61382eeb82bea108d1bdd965b00e7192c5dc9ab7436a33658b"

func expectedFenceSpec() (map[string]any, error) {
	b, err := os.ReadFile(fencePolicyPath)
	if err != nil {
		return nil, errors.New("reviewed admission source unavailable")
	}
	var policy map[string]any
	if yaml.Unmarshal(b, &policy) != nil || policy["kind"] != "ClusterPolicy" || field(policy, "metadata", "name") != "restrict-backstage-retirement" {
		return nil, errors.New("reviewed admission source incomplete")
	}
	spec := object(policy["spec"])
	if len(spec) == 0 || spec["background"] != false || field(spec, "webhookConfiguration", "failurePolicy") != "Fail" || len(array(spec["rules"])) != 5 {
		return nil, errors.New("reviewed admission contract incomplete")
	}
	return spec, nil
}

func canonicalFenceSpec(spec map[string]any) map[string]any {
	b, err := json.Marshal(spec)
	if err != nil {
		return nil
	}
	copy, err := decode(b)
	if err != nil {
		return nil
	}
	// Only these observed API defaults may differ from the reviewed declaration.
	// A disabled admission gate or any changed rule is never normalized away.
	for key, value := range map[string]any{"admission": true, "emitWarning": false, "validationFailureAction": "Audit"} {
		if copy[key] == value {
			delete(copy, key)
		}
	}
	for _, v := range array(copy["rules"]) {
		rule := object(v)
		if rule["skipBackgroundRequests"] == true {
			delete(rule, "skipBackgroundRequests")
		}
	}
	return copy
}

func (e engine) requireFence(ctx context.Context) error {
	expected, err := expectedFenceSpec()
	if err != nil {
		return err
	}
	p, err := e.read(ctx, "get", "clusterpolicy", "restrict-backstage-retirement", "--output=json")
	if err != nil || !metadataValid(p) || p["apiVersion"] != "kyverno.io/v1" || p["kind"] != "ClusterPolicy" || field(p, "metadata", "namespace") != nil || field(p, "metadata", "name") != "restrict-backstage-retirement" || integer(field(p, "metadata", "generation")) < 1 || !reflect.DeepEqual(canonicalFenceSpec(object(p["spec"])), canonicalFenceSpec(expected)) {
		return errors.New("effective admission declaration unverified")
	}
	for _, v := range array(field(p, "status", "conditions")) {
		c := object(v)
		if c["type"] == "Ready" && c["status"] == "True" {
			return nil
		}
	}
	return errors.New("admission readiness unverified")
}

func (e engine) fenceRecord(s snapshot, kept map[string]map[string]any) (string, error) {
	targets := []map[string]any{}
	objects, err := footprint(s.release)
	if err != nil {
		return "", err
	}
	for _, r := range objects {
		targets = append(targets, map[string]any{"kind": r["kind"], "name": field(r, "metadata", "name"), "uid": field(r, "metadata", "uid")})
	}
	for _, r := range []map[string]any{s.owner, s.chart} {
		targets = append(targets, map[string]any{"kind": r["kind"], "name": field(r, "metadata", "name"), "uid": field(r, "metadata", "uid")})
	}
	sort.Slice(targets, func(i, j int) bool { return text(targets[i]["kind"]) < text(targets[j]["kind"]) })
	retained := []map[string]any{}
	for key, projection := range kept {
		if strings.HasPrefix(key, "controller/") {
			continue
		}
		parts := strings.Split(key, "/")
		if len(parts) < 4 {
			return "", errors.New("retained identity incomplete")
		}
		kind, name := parts[len(parts)-3], parts[len(parts)-1]
		// Namespace is separately fenced. Database Pod deletion must remain
		// possible for healthy native replacement; readback still checks them.
		if kind != "Namespace" && kind != "Pod" {
			row := map[string]any{"kind": kind, "name": name, "uid": projection["uid"]}
			for _, list := range []map[string]any{s.live, s.inventory} {
				for _, candidate := range arrayObjects(list) {
					if identity(candidate) == key {
						profile, err := controllerProfile(s, candidate)
						if err != nil {
							return "", err
						}
						if profile == "ca" || profile == "leaf" {
							row["renewCertificate"] = profile
						} else if profile != "" {
							row["controllerProfile"] = profile
						}
					}
				}
			}
			retained = append(retained, row)
		}
	}
	sort.Slice(retained, func(i, j int) bool {
		return text(retained[i]["kind"])+"/"+text(retained[i]["name"]) < text(retained[j]["kind"])+"/"+text(retained[j]["name"])
	})
	uids := []string{}
	for _, uid := range e.retiring {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	var latest map[string]any
	// Release version is an integer; derive the actual storage identity instead
	// of relying on the controller history's array order.
	for _, r := range retained {
		if r["kind"] == "Secret" && r["name"] == "sh.helm.release.v1.backstage.v"+fmt.Sprint(s.release["version"]) {
			latest = map[string]any{"uid": r["uid"]}
		}
	}
	if text(latest["uid"]) == "" {
		return "", errors.New("latest history identity unavailable")
	}
	record := map[string]any{"version": 1, "phase": "fenced", "namespaceUID": field(s.namespace, "metadata", "uid"), "sourceSHA": e.sha, "revision": e.revision, "run": e.runID, "targets": targets, "retiringUIDs": uids, "retained": retained, "latestHistoryUID": latest["uid"]}
	for _, writer := range []struct{ key, recordKey string }{{"cnpg", "cnpgWriterUID"}, {"rootCA", "rootCAWriterUID"}, {"trust", "trustWriterUID"}} {
		if s.writers[writer.key] != nil {
			record[writer.recordKey] = field(s.writers[writer.key], "metadata", "uid")
		}
	}
	b, err := json.Marshal(record)
	if err != nil || len(b) > 32<<10 {
		return "", errors.New("retirement journal exceeds bound")
	}
	return string(b), nil
}

func (e engine) installFence(ctx context.Context, s snapshot, kept map[string]map[string]any) (string, error) {
	if err := e.requireFence(ctx); err != nil {
		return "", err
	}
	controller, err := e.read(ctx, "--namespace=flux-system", "get", "deployment", "helm-controller", "--output=json")
	if err != nil || !nativeControllerSupported(controller) {
		return "", errors.New("published native controller identity unverified")
	}
	record, err := e.fenceRecord(s, kept)
	if err != nil {
		return "", err
	}
	patch := append(tests(s.namespace), map[string]any{"op": "add", "path": "/metadata/annotations/platform.devantler.tech~1backstage-retirement", "value": record})
	p, err := e.write(ctx, patch, "patch", "namespace", "backstage", "--type=json", "--patch-file=REQUEST", "--output=json")
	if err != nil || !metadataValid(p) || field(p, "metadata", "uid") != field(s.namespace, "metadata", "uid") || field(p, "metadata", "annotations", fenceAnnotation) != record {
		return "", errors.New("durable retirement fence unconfirmed")
	}
	return record, nil
}

func nativeControllerSupported(controller map[string]any) bool {
	containers := array(field(controller, "spec", "template", "spec", "containers"))
	generation := integer(field(controller, "metadata", "generation"))
	replicas := integer(field(controller, "spec", "replicas"))
	if !metadataValid(controller) || controller["apiVersion"] != "apps/v1" || controller["kind"] != "Deployment" || field(controller, "metadata", "namespace") != "flux-system" || field(controller, "metadata", "name") != "helm-controller" || generation < 1 || replicas < 1 || integer(field(controller, "status", "observedGeneration")) != generation || integer(field(controller, "status", "readyReplicas")) != replicas || integer(field(controller, "status", "updatedReplicas")) != replicas || integer(field(controller, "status", "availableReplicas")) != replicas || field(controller, "spec", "template", "spec", "serviceAccountName") != "helm-controller" || len(containers) != 1 || object(containers[0])["image"] != nativeControllerImage {
		return false
	}
	container := object(containers[0])
	if container["args"] != nil {
		if _, ok := container["args"].([]any); !ok {
			return false
		}
	}
	args := array(container["args"])
	if len(args) > 128 {
		return false
	}
	for i, value := range args {
		argument := text(value)
		if argument == "" || strings.HasPrefix(argument, "--default-service-account=") && argument != "--default-service-account=" || argument == "--default-service-account" && (i+1 >= len(args) || text(args[i+1]) != "") {
			return false
		}
		if strings.Contains(argument, "UseHelm3Defaults") {
			matches := regexp.MustCompile(`UseHelm3Defaults=([^,[:space:]]+)`).FindAllStringSubmatch(argument, -1)
			if len(matches) == 0 || len(matches) != strings.Count(argument, "UseHelm3Defaults") {
				return false
			}
			for _, match := range matches {
				if match[1] != "false" {
					return false
				}
			}
		}
	}
	return true
}

func resourcePath(r map[string]any) (string, error) {
	name := text(field(r, "metadata", "name"))
	if !regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`).MatchString(name) {
		return "", errors.New("invalid resource name")
	}
	key := text(r["apiVersion"]) + "/" + text(r["kind"])
	paths := map[string]string{"apps/v1/Deployment": "/apis/apps/v1/namespaces/backstage/deployments/", "v1/Service": "/api/v1/namespaces/backstage/services/", "v1/ConfigMap": "/api/v1/namespaces/backstage/configmaps/", "helm.toolkit.fluxcd.io/v2/HelmRelease": "/apis/helm.toolkit.fluxcd.io/v2/namespaces/backstage/helmreleases/", "source.toolkit.fluxcd.io/v1/HelmChart": "/apis/source.toolkit.fluxcd.io/v1/namespaces/backstage/helmcharts/", "v1/Secret": "/api/v1/namespaces/backstage/secrets/", "v1/PersistentVolumeClaim": "/api/v1/namespaces/backstage/persistentvolumeclaims/", "v1/PersistentVolume": "/api/v1/persistentvolumes/", "v1/Namespace": "/api/v1/namespaces/"}
	path := paths[key]
	if path == "" || r["kind"] != "PersistentVolume" && r["kind"] != "Namespace" && field(r, "metadata", "namespace") != "backstage" {
		return "", errors.New("unreviewed API target")
	}
	return path + name, nil
}

func deleteOptions(r map[string]any) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Orphan", "preconditions": map[string]any{"uid": field(r, "metadata", "uid"), "resourceVersion": field(r, "metadata", "resourceVersion")}}
}

// These are server-side dry runs, not deletions. A generic RBAC/network error
// cannot impersonate the expected policy denial. No Secret payload is sent.
func (e engine) admissionControls(ctx context.Context, s snapshot) error {
	objects, err := footprint(s.release)
	if err != nil {
		return err
	}
	kept, err := preserved(s)
	if err != nil {
		return err
	}
	controls := []map[string]any{s.owner, s.chart}
	for _, r := range objects {
		controls = append(controls, r)
	}
	for _, list := range []map[string]any{s.live, s.inventory, s.volumes} {
		rs, err := members(list)
		if err != nil {
			return err
		}
		for _, r := range rs {
			if kept[identity(r)] != nil && (r["kind"] == "Secret" || r["kind"] == "PersistentVolumeClaim" || r["kind"] == "PersistentVolume") {
				controls = append(controls, r)
			}
		}
	}
	controls = append(controls, s.namespace)
	for _, r := range controls {
		path, err := resourcePath(r)
		if err != nil {
			return err
		}
		response, err := e.write(ctx, deleteOptions(r), "delete", "--raw", path+"?dryRun=All", "-f", "REQUEST")
		_, target := e.retiring[identity(r)]
		if target {
			confirmed := response["kind"] == "Status" && response["status"] == "Success" || identityComplete(response) && identity(response) == identity(r) && field(response, "metadata", "uid") == field(r, "metadata", "uid")
			if err != nil || !confirmed {
				return errors.New("native target admission unverified")
			}
		} else {
			rule := "preserve-retained-identities"
			if r["kind"] == "Namespace" {
				rule = "preserve-retirement-journal"
			}
			failure := text(response["failure"])
			if err == nil || !strings.Contains(failure, "admission webhook") || !strings.Contains(failure, "restrict-backstage-retirement:") || !strings.Contains(failure, rule+":") {
				return errors.New("retained-data admission unverified")
			}
		}
	}
	// A late second consumer is also a name-based chart-cleanup race. This
	// minimal release is validated only by server dry-run and is never created.
	probe := map[string]any{"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease", "metadata": map[string]any{"name": "backstage-retirement-probe-" + e.runID, "namespace": "backstage"}, "spec": map[string]any{"interval": "5m", "chartRef": map[string]any{"kind": "HelmChart", "name": "backstage-backstage", "namespace": "backstage"}}}
	response, err := e.write(ctx, probe, "create", "-f", "REQUEST", "--dry-run=server", "--output=json")
	failure := text(response["failure"])
	if err == nil || !strings.Contains(failure, "admission webhook") || !strings.Contains(failure, "restrict-backstage-retirement:") || !strings.Contains(failure, "fence-chart-consumers:") {
		return errors.New("exclusive chart admission unverified")
	}
	return e.requireFence(ctx)
}
