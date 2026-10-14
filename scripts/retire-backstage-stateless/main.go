// retire-backstage-stateless is separate from the globally read-only assessor.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
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

func object(v any) map[string]any { o, _ := v.(map[string]any); return o }
func text(v any) string           { s, _ := v.(string); return s }
func field(o map[string]any, keys ...string) any {
	var v any = o
	for _, k := range keys {
		v = object(v)[k]
	}
	return v
}
func integer(v any) int { n, _ := v.(int); return n }

const maxResponse = 4 << 20
const inventoryKinds = "service,configmap,secret,serviceaccount,replicaset,externalsecrets.external-secrets.io,passwords.generators.external-secrets.io,backups.postgresql.cnpg.io,scheduledbackups.postgresql.cnpg.io,objectstores.barmancloud.cnpg.io"

func decode(b []byte) (map[string]any, error) {
	var o map[string]any
	if len(b) > maxResponse || !json.Valid(b) || yaml.Unmarshal(b, &o) != nil || o == nil {
		return nil, errors.New("incomplete JSON evidence")
	}
	return o, nil
}
func members(o map[string]any) ([]map[string]any, error) {
	a, ok := o["items"].([]any)
	if o["apiVersion"] != "v1" || o["kind"] != "List" || !ok || len(a) > 512 || field(o, "metadata", "continue") != nil && field(o, "metadata", "continue") != "" {
		return nil, errors.New("incomplete list")
	}
	result := make([]map[string]any, 0, len(a))
	seen := map[string]bool{}
	for _, v := range a {
		r := object(v)
		key := identity(r)
		if !identityComplete(r) || seen[key] {
			return nil, errors.New("incomplete resource")
		}
		seen[key] = true
		result = append(result, r)
	}
	return result, nil
}
func metadataValid(o map[string]any) bool {
	return identityComplete(o) && field(o, "metadata", "deletionTimestamp") == nil
}
func identityComplete(o map[string]any) bool {
	return text(o["apiVersion"]) != "" && text(o["kind"]) != "" && text(field(o, "metadata", "name")) != "" && text(field(o, "metadata", "uid")) != "" && text(field(o, "metadata", "resourceVersion")) != ""
}
func identity(o map[string]any) string {
	return text(o["apiVersion"]) + "/" + text(o["kind"]) + "/" + text(field(o, "metadata", "namespace")) + "/" + text(field(o, "metadata", "name"))
}
func ready(o map[string]any) bool {
	if !metadataValid(o) {
		return false
	}
	g := integer(field(o, "metadata", "generation"))
	if g < 1 || integer(field(o, "status", "observedGeneration")) != g {
		return false
	}
	for _, v := range array(field(o, "status", "conditions")) {
		c := object(v)
		if c["type"] == "Reconciling" && c["status"] != "False" {
			return false
		}
	}
	for _, v := range array(field(o, "status", "conditions")) {
		c := object(v)
		if c["type"] == "Ready" && c["status"] == "True" && integer(c["observedGeneration"]) == g {
			return true
		}
	}
	return false
}
func array(v any) []any { a, _ := v.([]any); return a }

// A missing list is valid Kubernetes metadata; a malformed or incomplete list
// is not evidence that an object has no garbage-collection dependencies.
func ownerReferences(r map[string]any) ([]map[string]any, error) {
	v := field(r, "metadata", "ownerReferences")
	if v == nil {
		return nil, nil
	}
	a, ok := v.([]any)
	if !ok || len(a) > 512 {
		return nil, errors.New("malformed owner references")
	}
	owners := make([]map[string]any, 0, len(a))
	seen := map[string]bool{}
	for _, v := range a {
		o := object(v)
		uid := text(o["uid"])
		if text(o["apiVersion"]) == "" || text(o["kind"]) == "" || text(o["name"]) == "" || uid == "" || seen[uid] {
			return nil, errors.New("incomplete owner reference")
		}
		seen[uid] = true
		owners = append(owners, o)
	}
	return owners, nil
}

func independentOfRetirement(r map[string]any, retiring map[string]string) error {
	owners, err := ownerReferences(r)
	if err != nil {
		return err
	}
	for _, owner := range owners {
		for _, uid := range retiring {
			if owner["uid"] == uid {
				return errors.New("retained object depends on a retiring identity")
			}
		}
	}
	return nil
}

// Helm storage is base64 inside Kubernetes Secret base64, normally gzip JSON.
// Decode every retained revision under the same evidence bound, never to disk.
func storedRelease(secret map[string]any) (map[string]any, error) {
	data := object(secret["data"])
	if len(data) != 1 {
		return nil, errors.New("incomplete Helm storage")
	}
	b, err := base64.StdEncoding.DecodeString(text(data["release"]))
	if err != nil || len(b) > maxResponse {
		return nil, errors.New("invalid Secret encoding")
	}
	b, err = base64.StdEncoding.DecodeString(string(b))
	if err != nil || len(b) > maxResponse {
		return nil, errors.New("invalid Helm encoding")
	}
	if bytes.HasPrefix(b, []byte{0x1f, 0x8b, 0x08}) {
		r, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, errors.New("invalid Helm compression")
		}
		b, err = io.ReadAll(io.LimitReader(r, maxResponse+1))
		closeErr := r.Close()
		if err != nil || closeErr != nil || len(b) > maxResponse {
			return nil, errors.New("incomplete bounded Helm history")
		}
	}
	return decode(b)
}

type snapshot struct {
	release, live, inventory, namespace, volumes, owner, chart, source, layers map[string]any
	releases                                                                   []map[string]any
	retiring                                                                   map[string]string
	fence                                                                      string
	writers                                                                    map[string]map[string]any
	layerSources                                                               map[string]map[string]any
	bundle, monitoring                                                         map[string]any
}
type engine struct {
	execute              command
	revision, sha, runID string
	retiring             map[string]string
	ownerUID             string
	fence                string
}

func (e engine) read(ctx context.Context, args ...string) (map[string]any, error) {
	b, err := e.execute(ctx, "kubectl", append([]string{"--context=admin@prod", "--request-timeout=30s"}, args...)...)
	if err != nil {
		return nil, errors.New("read failed")
	}
	return decode(b)
}
func (e engine) optional(ctx context.Context, args ...string) (map[string]any, error) {
	b, err := e.execute(ctx, "kubectl", append([]string{"--context=admin@prod", "--request-timeout=30s"}, args...)...)
	if err != nil {
		return nil, errors.New("optional read failed")
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	return decode(b)
}
func (e engine) sourceCurrent(ctx context.Context) error {
	b, err := e.execute(ctx, "bash", "scripts/verify-prod-recovery-source.sh", e.sha)
	if err != nil || strings.TrimSpace(string(b)) != "PROD_RECOVERY_SOURCE=PASS" {
		return errors.New("current main unverified")
	}
	return nil
}
func (e engine) collect(ctx context.Context) (snapshot, error) {
	s := snapshot{retiring: e.retiring, fence: e.fence}
	if e.fence != "" && e.requireFence(ctx) != nil {
		return s, errors.New("durable admission fence unverified")
	}
	b, err := e.execute(ctx, "helm", "status", "backstage", "--namespace=backstage", "--kube-context=admin@prod", "--output=json")
	if err != nil {
		return s, errors.New("release read failed")
	}
	s.release, err = decode(b)
	if err != nil {
		return s, err
	}
	reads := []struct {
		target *map[string]any
		args   []string
	}{
		{&s.live, []string{"--namespace=backstage", "get", "deployment,pod,persistentvolumeclaim,clusters.postgresql.cnpg.io", "--output=json"}},
		{&s.inventory, []string{"--namespace=backstage", "get", inventoryKinds, "--output=json"}},
		{&s.namespace, []string{"get", "namespace", "backstage", "--output=json"}},
		{&s.volumes, []string{"get", "persistentvolumes", "--output=json"}},
		{&s.layers, []string{"get", "kustomizations.kustomize.toolkit.fluxcd.io", "--all-namespaces", "--output=json"}},
		{&s.source, []string{"--namespace=flux-system", "get", "ocirepository", "flux-system", "--output=json"}},
	}
	for _, r := range reads {
		*r.target, err = e.read(ctx, r.args...)
		if err != nil {
			return s, err
		}
	}
	if err := e.collectLayerSources(ctx, &s); err != nil {
		return s, err
	}
	list, err := e.read(ctx, "get", "helmreleases.helm.toolkit.fluxcd.io", "--all-namespaces", "--output=json")
	if err != nil {
		return s, err
	}
	s.releases, err = members(list)
	if err != nil {
		return s, err
	}
	for _, r := range s.releases {
		if field(r, "metadata", "name") == "backstage" && field(r, "metadata", "namespace") == "backstage" {
			s.owner = r
		}
	}
	if s.owner != nil {
		chart := text(field(s.owner, "status", "helmChart"))
		if chart != "backstage/backstage-backstage" {
			return s, errors.New("unexpected chart identity")
		}
		s.chart, err = e.optional(ctx, "--namespace=backstage", "get", "helmchart", "backstage-backstage", "--ignore-not-found", "--output=json")
		if err != nil {
			return s, err
		}
	} else {
		s.chart, err = e.optional(ctx, "--namespace=backstage", "get", "helmchart", "backstage-backstage", "--ignore-not-found", "--output=json")
		if err != nil {
			return s, err
		}
	}
	if err := e.collectWriters(ctx, &s); err != nil {
		return s, err
	}
	return s, nil
}

func stateless(v any) bool {
	s := object(v)
	if len(array(s["containers"])) == 0 {
		return false
	}
	for _, k := range []string{"containers", "initContainers", "ephemeralContainers"} {
		if s[k] != nil {
			if _, ok := s[k].([]any); !ok {
				return false
			}
		}
		for _, v := range array(s[k]) {
			if field(object(v), "lifecycle", "preStop") != nil {
				return false
			}
		}
	}
	if s["volumes"] != nil {
		if _, ok := s["volumes"].([]any); !ok {
			return false
		}
	}
	for _, v := range array(s["volumes"]) {
		o := object(v)
		allowed := false
		for _, k := range []string{"emptyDir", "configMap", "secret", "projected", "downwardAPI"} {
			if object(o[k]) != nil {
				allowed = true
			}
		}
		if len(o) != 2 || !allowed {
			return false
		}
	}
	return true
}
func footprint(r map[string]any) (map[string]map[string]any, error) {
	if r["hooks"] != nil {
		if _, ok := r["hooks"].([]any); !ok {
			return nil, errors.New("malformed hooks")
		}
	}
	if r["name"] != "backstage" || r["namespace"] != "backstage" || field(r, "info", "status") != "deployed" || integer(r["version"]) < 1 || len(array(r["hooks"])) > 0 {
		return nil, errors.New("release is not stable")
	}
	expected := map[string]bool{"apps/v1/Deployment/backstage": true, "v1/Service/backstage": true, "v1/ConfigMap/backstage-app-config": true}
	d := yaml.NewDecoder(strings.NewReader(text(r["manifest"])))
	required := map[string]bool{}
	for {
		var o map[string]any
		err := d.Decode(&o)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(o) == 0 {
			continue
		}
		key := text(o["apiVersion"]) + "/" + text(o["kind"]) + "/" + text(field(o, "metadata", "name"))
		ns := text(field(o, "metadata", "namespace"))
		if !expected[key] || required[key] || ns != "" && ns != "backstage" || field(o, "metadata", "annotations", "helm.sh/hook") != nil || field(o, "metadata", "annotations", "helm.sh/resource-policy") != nil {
			return nil, errors.New("unreviewed footprint")
		}
		if o["kind"] == "Deployment" && !stateless(field(o, "spec", "template", "spec")) {
			return nil, errors.New("persistent workload")
		}
		required[key] = true
	}
	if len(required) != 3 {
		return nil, errors.New("incomplete footprint")
	}
	observed := map[string]map[string]any{}
	for key, v := range object(field(r, "info", "resources")) {
		if key == "v1/Pod(related)" {
			continue
		}
		for _, raw := range array(v) {
			o := object(raw)
			key := text(o["apiVersion"]) + "/" + text(o["kind"]) + "/" + text(field(o, "metadata", "name"))
			if !expected[key] || observed[key] != nil || !metadataValid(o) || field(o, "metadata", "namespace") != "backstage" || field(o, "metadata", "annotations", "meta.helm.sh/release-name") != "backstage" || field(o, "metadata", "annotations", "meta.helm.sh/release-namespace") != "backstage" {
				return nil, errors.New("unproven Helm ownership")
			}
			observed[key] = o
		}
	}
	if len(observed) != 3 {
		return nil, errors.New("incomplete Helm GET")
	}
	return observed, nil
}
func (e engine) eligible(s snapshot) error {
	if _, err := footprint(s.release); err != nil {
		return err
	}
	h := s.owner
	if field(h, "spec", "waitStrategy") != nil {
		return errors.New("native wait profile uncharacterized")
	}
	if !metadataValid(h) || h["apiVersion"] != "helm.toolkit.fluxcd.io/v2" || h["kind"] != "HelmRelease" || field(h, "spec", "suspend") == true || field(h, "spec", "kubeConfig") != nil || field(h, "spec", "serviceAccountName") != nil && field(h, "spec", "serviceAccountName") != "" || field(h, "status", "storageNamespace") != "backstage" {
		return errors.New("uninstall owner unverified")
	}
	finalizer := false
	for _, v := range array(field(h, "metadata", "finalizers")) {
		if v == "finalizers.fluxcd.io" {
			finalizer = true
		}
	}
	if !finalizer {
		return errors.New("native uninstall finalizer absent")
	}
	for _, k := range []string{"releaseName", "targetNamespace", "storageNamespace"} {
		if v := text(field(h, "spec", k)); v != "" && v != "backstage" {
			return errors.New("cross-scope release")
		}
	}
	if integer(field(h, "metadata", "generation")) < 1 || integer(field(h, "status", "observedGeneration")) != integer(field(h, "metadata", "generation")) {
		return errors.New("unobserved release generation")
	}
	sourceWait := waitingForUnavailableSource(h, s.chart)
	if !sourceWait {
		for _, v := range array(field(h, "status", "conditions")) {
			o := object(v)
			if o["type"] == "Reconciling" && o["status"] != "False" {
				return errors.New("active Helm action")
			}
		}
		if !ready(h) {
			return errors.New("unready uninstall owner")
		}
	}
	released := false
	for _, v := range array(field(h, "status", "conditions")) {
		c := object(v)
		released = released || c["type"] == "Released" && c["status"] == "True" && c["observedGeneration"] == field(h, "metadata", "generation")
	}
	if !released && !sourceWait {
		return errors.New("unreleased uninstall owner")
	}
	if !controllerHistoryMatches(s) {
		return errors.New("controller and stored release history differ")
	}
	if !metadataValid(s.chart) {
		return errors.New("chart is terminating")
	}
	if err := e.chartOwned(s); err != nil {
		return err
	}
	if err := e.deployed(s); err != nil {
		return err
	}
	_, err := preserved(s)
	return err
}

// Published v1.5.5 Snapshots.Latest sorts descending by Version, not input
// order. Refuse ambiguous/incomplete evidence before native uninstall starts.
func controllerHistoryMatches(s snapshot) bool {
	latest, ok := latestControllerHistory(s.owner)
	chartName := text(field(s.release, "chart", "metadata", "name"))
	chartVersion := text(field(s.release, "chart", "metadata", "version"))
	return ok && chartName != "" && chartVersion != "" && latest["chartName"] == chartName && latest["chartVersion"] == chartVersion && latest["name"] == s.release["name"] && latest["namespace"] == s.release["namespace"] && latest["version"] == s.release["version"] && latest["status"] == field(s.release, "info", "status") && latest["status"] == "deployed"
}

func latestControllerHistory(owner map[string]any) (map[string]any, bool) {
	history, ok := field(owner, "status", "history").([]any)
	if !ok || len(history) == 0 || len(history) > 512 {
		return nil, false
	}
	var latest map[string]any
	versions := map[int]bool{}
	for _, v := range history {
		h := object(v)
		version := integer(h["version"])
		if version < 1 || versions[version] || text(h["name"]) == "" || text(h["namespace"]) == "" || text(h["status"]) == "" {
			return nil, false
		}
		versions[version] = true
		if latest == nil || version > integer(latest["version"]) {
			latest = h
		}
	}
	return latest, true
}
func (e engine) chartOwned(s snapshot) error {
	c := s.chart
	if !identityComplete(c) || c["apiVersion"] != "source.toolkit.fluxcd.io/v1" || c["kind"] != "HelmChart" || field(c, "metadata", "namespace") != "backstage" || field(c, "metadata", "name") != "backstage-backstage" || field(c, "metadata", "labels", "helm.toolkit.fluxcd.io/name") != "backstage" || field(c, "metadata", "labels", "helm.toolkit.fluxcd.io/namespace") != "backstage" || field(c, "spec", "chart") != "backstage" || field(c, "spec", "sourceRef", "kind") != "HelmRepository" || field(c, "spec", "sourceRef", "name") != "backstage" || field(c, "spec", "sourceRef", "namespace") != nil && field(c, "spec", "sourceRef", "namespace") != "backstage" {
		return errors.New("chart ownership unproven")
	}
	ownerUID := text(field(s.owner, "metadata", "uid"))
	if ownerUID == "" {
		ownerUID = e.ownerUID
	}
	owners, err := ownerReferences(c)
	if err != nil {
		return err
	}
	// Published Flux v1.5.5 associates generated charts using the labels above,
	// not an ownerReference. References, when present, must still match exactly.
	for _, o := range owners {
		if o["kind"] != "HelmRelease" || o["name"] != "backstage" || o["uid"] != ownerUID || ownerUID == "" {
			return errors.New("chart has a foreign owner")
		}
	}
	for _, r := range s.releases {
		if identity(r) == identity(s.owner) {
			continue
		}
		ref := object(field(r, "spec", "chartRef"))
		ns := text(ref["namespace"])
		if ns == "" {
			ns = text(field(r, "metadata", "namespace"))
		}
		if ref["kind"] == "HelmChart" && ref["name"] == "backstage-backstage" && ns == "backstage" || field(r, "status", "helmChart") == "backstage/backstage-backstage" {
			return errors.New("chart has another consumer")
		}
	}
	return nil
}
func (e engine) deployed(s snapshot) error {
	if !metadataValid(s.source) || s.source["kind"] != "OCIRepository" || !ready(s.source) || field(s.source, "status", "artifact", "revision") != e.revision {
		return errors.New("deployed source unverified")
	}
	layers, err := members(s.layers)
	if err != nil {
		return err
	}
	apps := false
	for _, k := range layers {
		if !ready(k) || !currentLayerInventory(s, k) {
			return errors.New("unsettled inventory")
		}
		entries, ok := field(k, "status", "inventory", "entries").([]any)
		if !ok || len(entries) == 0 {
			return errors.New("missing inventory")
		}
		for _, v := range entries {
			id := text(object(v)["id"])
			if id == "" || id == "backstage_backstage_helm.toolkit.fluxcd.io_HelmRelease" || id == "backstage_backstage-backstage_source.toolkit.fluxcd.io_HelmChart" || id == "backstage_backstage_apps_Deployment" || id == "backstage_backstage__Service" || id == "backstage_backstage-app-config__ConfigMap" {
				return errors.New("source still owns target")
			}
		}
		if field(k, "metadata", "name") == "apps" && field(k, "metadata", "namespace") == "flux-system" {
			apps = field(k, "spec", "sourceRef", "kind") == "OCIRepository" && field(k, "spec", "sourceRef", "name") == "flux-system" && field(k, "status", "lastAppliedRevision") == e.revision
		}
	}
	if !apps {
		return errors.New("applications source unverified")
	}
	return nil
}

// preserved projects only stable data identities/specifications. Secret values
// remain in process memory and are compared, never logged or journaled.
func preserved(s snapshot) (map[string]map[string]any, error) {
	result := map[string]map[string]any{}
	for key, source := range s.layerSources {
		result["inventorySource/"+key] = map[string]any{"object": projection(source), "revision": field(source, "status", "artifact", "revision")}
	}
	for name, writer := range s.writers {
		result["controller/Identity//"+name] = map[string]any{"uid": field(writer, "metadata", "uid"), "spec": writer["spec"]}
	}
	if s.bundle != nil {
		result["controller/Identity//bundle"] = projection(s.bundle)
	}
	if !metadataValid(s.namespace) || s.namespace["kind"] != "Namespace" || field(s.namespace, "metadata", "name") != "backstage" || field(s.namespace, "metadata", "annotations", "kustomize.toolkit.fluxcd.io/prune") != "disabled" {
		return nil, errors.New("namespace preservation unverified")
	}
	result[identity(s.namespace)] = projection(s.namespace)
	if s.fence != "" {
		if field(s.namespace, "metadata", "annotations", fenceAnnotation) != s.fence {
			return nil, errors.New("owned retirement journal changed")
		}
		annotations := map[string]any{}
		for key, value := range object(field(s.namespace, "metadata", "annotations")) {
			if key != fenceAnnotation {
				annotations[key] = value
			}
		}
		result[identity(s.namespace)]["annotations"] = annotations
	}
	live, err := members(s.live)
	if err != nil {
		return nil, err
	}
	inv, err := members(s.inventory)
	if err != nil {
		return nil, err
	}
	volumes, err := members(s.volumes)
	if err != nil {
		return nil, err
	}
	if s.retiring == nil {
		s.retiring, err = retiringIdentities(s)
		if err != nil {
			return nil, err
		}
	}
	if err := independentOfRetirement(s.namespace, s.retiring); err != nil {
		return nil, err
	}
	dbUID := ""
	dbPrimary := ""
	primaryObserved := false
	instances := 0
	dbPods := 0
	claims := []map[string]any{}
	history := false
	for _, r := range live {
		if r["kind"] == "Cluster" && field(r, "metadata", "name") == "backstage-db" {
			dbUID = text(field(r, "metadata", "uid"))
			dbPrimary = text(field(r, "status", "currentPrimary"))
			instances = integer(field(r, "spec", "instances"))
			if instances < 1 || integer(field(r, "status", "readyInstances")) != instances || text(field(r, "status", "currentPrimary")) == "" || field(r, "status", "currentPrimary") != field(r, "status", "targetPrimary") {
				return nil, errors.New("database is not fully healthy")
			}
		}
	}
	if dbUID == "" {
		return nil, errors.New("database missing")
	}
	for _, r := range append(live, inv...) {
		if field(r, "metadata", "namespace") != "backstage" {
			return nil, errors.New("foreign preservation object")
		}
		if uid, ok := s.retiring[identity(r)]; ok {
			if field(r, "metadata", "uid") != uid {
				return nil, errors.New("retiring identity replaced")
			}
			continue
		}
		if targetResource(r) {
			return nil, errors.New("unbound retiring identity")
		}
		if !metadataValid(r) {
			return nil, errors.New("retained resource is terminating")
		}
		if err := independentOfRetirement(r, s.retiring); err != nil {
			return nil, err
		}
		if r["kind"] == "Secret" && field(r, "metadata", "labels", "owner") == "helm" && field(r, "metadata", "labels", "name") == "backstage" {
			version := text(field(r, "metadata", "labels", "version"))
			owners, err := ownerReferences(r)
			if err != nil || len(owners) != 0 || r["type"] != "helm.sh/release.v1" || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(version) || field(r, "metadata", "name") != "sh.helm.release.v1.backstage.v"+version || text(field(r, "data", "release")) == "" {
				return nil, errors.New("unproven retained Helm history")
			}
			stored, err := storedRelease(r)
			status := text(field(stored, "info", "status"))
			latest := version == fmt.Sprint(s.release["version"])
			if err != nil || stored["name"] != "backstage" || stored["namespace"] != "backstage" || fmt.Sprint(stored["version"]) != version || status == "" || status != field(r, "metadata", "labels", "status") || latest && (!sameStoredContent(stored, s.release) || status != field(s.release, "info", "status")) || !latest && status != "superseded" && status != "failed" && status != "uninstalled" {
				return nil, errors.New("stored history identity or lifecycle unverified")
			}
			labels := map[string]any{}
			for k, v := range object(field(r, "metadata", "labels")) {
				if k != "status" && k != "modifiedAt" {
					labels[k] = v
				}
			}
			result[identity(r)] = map[string]any{"uid": field(r, "metadata", "uid"), "type": r["type"], "labels": labels, "annotations": field(r, "metadata", "annotations"), "owners": field(r, "metadata", "ownerReferences"), "finalizers": field(r, "metadata", "finalizers")}
			if !latest {
				// Only the latest revision participates in the native uninstall;
				// its decoded content is checked separately by sameStoredContent.
				result[identity(r)]["data"] = r["data"]
			}
			history = history || latest
			continue
		}
		if r["kind"] == "PersistentVolumeClaim" {
			if err := retainedClaimOwnership(s, r); err != nil {
				return nil, err
			}
			claims = append(claims, r)
		}
		if r["kind"] == "Pod" {
			if field(r, "metadata", "labels", "cnpg.io/cluster") != "backstage-db" {
				return nil, errors.New("unreviewed workload")
			}
			healthy := false
			for _, v := range array(field(r, "status", "conditions")) {
				o := object(v)
				healthy = healthy || o["type"] == "Ready" && o["status"] == "True"
			}
			owned := false
			for _, v := range array(field(r, "metadata", "ownerReferences")) {
				o := object(v)
				owned = owned || o["uid"] == dbUID && o["kind"] == "Cluster"
			}
			if !healthy || !owned || field(r, "status", "phase") != "Running" {
				return nil, errors.New("database pod unhealthy")
			}
			dbPods++
			primaryObserved = primaryObserved || field(r, "metadata", "name") == dbPrimary
		}
		result[identity(r)], err = controllerProjection(s, r, s.fence != "")
		if err != nil {
			return nil, err
		}
	}
	if dbPods != instances || !primaryObserved || len(claims) == 0 || !history {
		return nil, errors.New("database coverage incomplete")
	}
	for _, c := range claims {
		found := false
		for _, v := range volumes {
			if metadataValid(v) && field(v, "metadata", "name") == field(c, "spec", "volumeName") && field(v, "spec", "claimRef", "namespace") == "backstage" && field(v, "spec", "claimRef", "name") == field(c, "metadata", "name") && field(v, "spec", "claimRef", "uid") == field(c, "metadata", "uid") && field(v, "status", "phase") == "Bound" {
				if field(v, "spec", "persistentVolumeReclaimPolicy") != "Retain" {
					return nil, errors.New("physical storage retention unverified")
				}
				if err := independentOfRetirement(v, s.retiring); err != nil {
					return nil, err
				}
				found = true
				result[identity(v)] = projection(v)
			}
		}
		if !found {
			return nil, errors.New("claim volume identity mismatch")
		}
	}
	return result, nil
}

// Only descendants of the captured Deployment are excluded from preservation.
// Names or app labels alone never identify a deletable or ignorable workload.
func retiringIdentities(s snapshot) (map[string]string, error) {
	owned, err := footprint(s.release)
	if err != nil {
		return nil, err
	}
	live, err := members(s.live)
	if err != nil {
		return nil, err
	}
	inv, err := members(s.inventory)
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	for _, r := range []map[string]any{s.owner, s.chart} {
		if !identityComplete(r) {
			return nil, errors.New("incomplete retirement owner")
		}
		result[identity(r)] = text(field(r, "metadata", "uid"))
	}
	seen := map[string]bool{}
	deploymentUID := text(field(owned["apps/v1/Deployment/backstage"], "metadata", "uid"))
	resources := append(live, inv...)
	for _, r := range resources {
		key := text(r["apiVersion"]) + "/" + text(r["kind"]) + "/" + text(field(r, "metadata", "name"))
		if expected := owned[key]; expected != nil {
			if !metadataValid(r) || field(r, "metadata", "uid") != field(expected, "metadata", "uid") || field(r, "metadata", "resourceVersion") != field(expected, "metadata", "resourceVersion") {
				return nil, errors.New("live footprint differs from Helm GET")
			}
			result[identity(r)] = text(field(r, "metadata", "uid"))
			seen[key] = true
		}
		if r["kind"] == "ReplicaSet" && ownedBy(r, "Deployment", deploymentUID) {
			if !metadataValid(r) {
				return nil, errors.New("unsettled descendant")
			}
			result[identity(r)] = text(field(r, "metadata", "uid"))
		}
	}
	if len(seen) != 3 {
		return nil, errors.New("incomplete live footprint")
	}
	for _, r := range live {
		if r["kind"] != "Pod" || !targetResource(r) {
			continue
		}
		bound := false
		for _, rs := range inv {
			if rs["kind"] == "ReplicaSet" && result[identity(rs)] != "" && ownedBy(r, "ReplicaSet", text(field(rs, "metadata", "uid"))) {
				bound = true
			}
		}
		if !bound || !metadataValid(r) {
			return nil, errors.New("unbound workload descendant")
		}
		result[identity(r)] = text(field(r, "metadata", "uid"))
	}
	return result, nil
}
func ownedBy(r map[string]any, kind, uid string) bool {
	owners, ok := field(r, "metadata", "ownerReferences").([]any)
	return ok && len(owners) == 1 && object(owners[0])["kind"] == kind && object(owners[0])["uid"] == uid && uid != ""
}
func absent(s snapshot) bool {
	for _, list := range []map[string]any{s.live, s.inventory} {
		rs, err := members(list)
		if err != nil {
			return false
		}
		for _, r := range rs {
			if _, ok := s.retiring[identity(r)]; ok || targetResource(r) {
				return false
			}
		}
	}
	return true
}
func unchangedOwner(a, b, chart map[string]any) bool {
	if !metadataValid(b) || !reflect.DeepEqual(projection(a), projection(b)) || !reflect.DeepEqual(field(a, "metadata", "finalizers"), field(b, "metadata", "finalizers")) || field(a, "metadata", "generation") != field(b, "metadata", "generation") {
		return false
	}
	if waitingForUnavailableSource(b, chart) {
		return true
	}
	for _, v := range array(field(b, "status", "conditions")) {
		c := object(v)
		if c["type"] == "Reconciling" && c["status"] != "False" {
			return false
		}
	}
	return true
}
func targetResource(r map[string]any) bool {
	name := text(field(r, "metadata", "name"))
	kind := text(r["kind"])
	if kind == "Deployment" || kind == "Service" {
		return name == "backstage"
	}
	if kind == "ConfigMap" {
		return name == "backstage-app-config"
	}
	return kind == "Pod" && field(r, "metadata", "labels", "app.kubernetes.io/name") == "backstage" && field(r, "metadata", "labels", "app.kubernetes.io/instance") == "backstage" && field(r, "metadata", "labels", "app.kubernetes.io/component") == "backstage"
}
func projection(r map[string]any) map[string]any {
	return map[string]any{"uid": field(r, "metadata", "uid"), "spec": r["spec"], "data": r["data"], "stringData": r["stringData"], "binaryData": r["binaryData"], "immutable": r["immutable"], "type": r["type"], "owners": field(r, "metadata", "ownerReferences"), "annotations": field(r, "metadata", "annotations"), "labels": field(r, "metadata", "labels"), "finalizers": field(r, "metadata", "finalizers")}
}
func sameRelease(a, b map[string]any) bool {
	normalize := func(r map[string]any) map[string]any {
		data, _ := json.Marshal(r)
		copy, _ := decode(data)
		for _, v := range array(field(copy, "info", "resources", "v1/Pod(related)")) {
			o := object(v)
			if o["apiVersion"] != "v1" || o["kind"] != "PodList" || len(array(o["items"])) == 0 || text(field(o, "metadata", "resourceVersion")) == "" {
				return nil
			}
			delete(object(o["metadata"]), "resourceVersion")
		}
		return copy
	}
	x, y := normalize(a), normalize(b)
	return x != nil && y != nil && reflect.DeepEqual(x, y)
}
func sameStoredContent(a, b map[string]any) bool {
	normalize := func(r map[string]any) map[string]any {
		data, err := json.Marshal(r)
		if err != nil {
			return nil
		}
		copy, err := decode(data)
		if err != nil {
			return nil
		}
		info := object(copy["info"])
		// Keep notes, deployment dates and unknown information fields. Native
		// uninstall changes only these fields; GET-derived resources are transient.
		for _, key := range []string{"status", "deleted", "description", "resources"} {
			delete(info, key)
		}
		return copy
	}
	x, y := normalize(a), normalize(b)
	return x != nil && y != nil && reflect.DeepEqual(x, y)
}
func (e engine) write(ctx context.Context, value any, args ...string) (response map[string]any, resultErr error) {
	response, _, resultErr = e.writeObserved(ctx, value, args...)
	return response, resultErr
}

// submitted records the transport invocation, not server acceptance. Preparation
// failures are known not submitted; command failures may have reached the server.
func (e engine) writeObserved(ctx context.Context, value any, args ...string) (response map[string]any, submitted bool, resultErr error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, false, err
	}
	f, err := os.CreateTemp("", "portal-retirement-request-*.json")
	if err != nil {
		return nil, false, err
	}
	defer func() {
		// An already-removed request is clean. Other cleanup failures cannot
		// turn a completed write into permission for another lifecycle action.
		if err := os.Remove(f.Name()); err != nil && !os.IsNotExist(err) && resultErr == nil {
			response, resultErr = nil, errors.New("request cleanup failed")
		}
	}()
	if _, err = f.Write(data); err != nil {
		if closeErr := f.Close(); closeErr != nil {
			return nil, false, errors.New("request write and close failed")
		}
		return nil, false, err
	}
	if err = f.Close(); err != nil {
		return nil, false, err
	}
	for i, a := range args {
		if a == "REQUEST" {
			args[i] = f.Name()
		}
		if a == "--patch-file=REQUEST" {
			args[i] = "--patch-file=" + f.Name()
		}
	}
	submitted = true
	b, err := e.execute(ctx, "kubectl", append([]string{"--context=admin@prod", "--request-timeout=30s"}, args...)...)
	if err != nil {
		// The bounded failure stays in memory solely for dry-run denial checks.
		// Callers never print it or infer deletion permission from an error.
		return map[string]any{"failure": string(b)}, submitted, errors.New("write outcome unknown")
	}
	response, resultErr = decode(b)
	return response, submitted, resultErr
}
func tests(o map[string]any) []map[string]any {
	return []map[string]any{{"op": "test", "path": "/metadata/uid", "value": field(o, "metadata", "uid")}, {"op": "test", "path": "/metadata/resourceVersion", "value": field(o, "metadata", "resourceVersion")}}
}
func uninstallOptions() map[string]any {
	return map[string]any{"keepHistory": true, "disableHooks": true, "disableWait": false, "deletionPropagation": "foreground", "timeout": "5m"}
}
func (e engine) remove(ctx context.Context, r map[string]any, path string) (bool, error) {
	if err := e.sourceCurrent(ctx); err != nil {
		return false, err
	}
	o, submitted, err := e.writeObserved(ctx, deleteOptions(r), "delete", "--raw", path, "-f", "REQUEST")
	if err != nil {
		return submitted, err
	}
	if o["apiVersion"] == "v1" && o["kind"] == "Status" && o["status"] == "Success" {
		return submitted, nil
	}
	if identityComplete(o) && identity(o) == identity(r) && field(o, "metadata", "uid") == field(r, "metadata", "uid") && field(o, "metadata", "deletionTimestamp") != nil {
		return submitted, nil
	}
	return submitted, errors.New("delete response is incomplete")
}

func run(args []string, out io.Writer, execute command) int {
	nativeSubmitted := false
	hold := func(code string) int {
		if nativeSubmitted {
			code = "HOLD_NATIVE_LIFECYCLE_CONTINUES reason=" + code
		}
		if _, err := fmt.Fprintln(out, "PORTAL-RETIREMENT="+code); err != nil {
			return 2
		}
		return 2
	}
	if len(args) != 1 || args[0] != "--execute" || os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("GITHUB_REPOSITORY") != "devantler-tech/platform" || os.Getenv("GITHUB_REF") != "refs/heads/main" || os.Getenv("GITHUB_RUN_ATTEMPT") != "1" || os.Getenv("PORTAL_RETIREMENT_CONFIRM") != "RETIRE BACKSTAGE STATELESS ONLY" || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(os.Getenv("GITHUB_SHA")) || !regexp.MustCompile(`^[1-9][0-9]{0,19}$`).MatchString(os.Getenv("GITHUB_RUN_ID")) || !regexp.MustCompile(`^latest@sha256:[0-9a-f]{64}$`).MatchString(os.Getenv("PORTAL_RETIREMENT_REVISION")) {
		return hold("HOLD_CONFIRMATION")
	}
	e := engine{execute: execute, revision: os.Getenv("PORTAL_RETIREMENT_REVISION"), sha: os.Getenv("GITHUB_SHA"), runID: os.Getenv("GITHUB_RUN_ID")}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	if e.sourceCurrent(ctx) != nil {
		return hold("HOLD_PRECONDITION")
	}
	b, err := execute(ctx, "go", "run", "./scripts/assess-backstage-retirement", "--live")
	if err != nil || strings.TrimSpace(string(b)) != "PORTAL-FOOTPRINT=ASSESSED_STATELESS objects=3 dataRetirement=UNASSESSED mutation=NONE" {
		return hold("HOLD_PRECONDITION")
	}
	first, err := e.collect(ctx)
	if err == nil && field(first.namespace, "metadata", "annotations", fenceAnnotation) != nil {
		return hold("HOLD_EXISTING_NATIVE_OPERATION")
	}
	if err != nil || e.eligible(first) != nil {
		return hold("HOLD_PRECONDITION")
	}
	e.retiring, err = retiringIdentities(first)
	if err != nil {
		return hold("HOLD_PRECONDITION")
	}
	first.retiring = e.retiring
	e.ownerUID = text(field(first.owner, "metadata", "uid"))
	if _, err = execute(ctx, "sleep", "5"); err != nil {
		return hold("HOLD_PRECONDITION")
	}
	second, err := e.collect(ctx)
	if err != nil || e.eligible(second) != nil || !sameRelease(first.release, second.release) || !reflect.DeepEqual(first.owner, second.owner) || !reflect.DeepEqual(first.chart, second.chart) {
		return hold("HOLD_PRECONDITION")
	}
	secondTargets, err := retiringIdentities(second)
	if err != nil || !reflect.DeepEqual(e.retiring, secondTargets) {
		return hold("HOLD_PRECONDITION")
	}
	before, err := preserved(first)
	if err != nil {
		return hold("HOLD_PRECONDITION")
	}
	again, err := preserved(second)
	if err != nil || !reflect.DeepEqual(before, again) {
		return hold("HOLD_PRECONDITION")
	}
	name := "backstage-retirement-" + e.runID
	used, err := e.optional(ctx, "--namespace=flux-system", "get", "configmap", name, "--ignore-not-found", "--output=json")
	if err != nil || used != nil {
		return hold("HOLD_PRECONDITION")
	}
	if e.sourceCurrent(ctx) != nil {
		return hold("HOLD_PRECONDITION")
	}
	receipt := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": name, "namespace": "flux-system"}, "data": map[string]any{"source": e.sha, "revision": e.revision, "run": e.runID, "attempt": "1", "scope": "stateless-only", "releaseUID": field(second.owner, "metadata", "uid"), "chartUID": field(second.chart, "metadata", "uid"), "dataRetirement": "UNASSESSED"}}
	created, err := e.write(ctx, receipt, "create", "-f", "REQUEST", "--output=json")
	if err != nil || !metadataValid(created) || identity(created) != identity(receipt) || !reflect.DeepEqual(created["data"], receipt["data"]) {
		return hold("HOLD_MUTATION")
	}
	if e.sourceCurrent(ctx) != nil {
		return hold("HOLD_MUTATION")
	}
	e.fence, err = e.installFence(ctx, second, before)
	if err != nil {
		return hold("HOLD_ADMISSION")
	}
	// The initial separated samples compare full content. Only after installing
	// our exact journal do both sides use the field-scoped controller projection.
	before, err = normalizePreserved(normalizedNamespace(second, e.fence))
	if err != nil {
		return hold("HOLD_ADMISSION")
	}
	opts := uninstallOptions()
	if e.sourceCurrent(ctx) != nil {
		return hold("HOLD_MUTATION")
	}
	patch := append(tests(second.owner), map[string]any{"op": "add", "path": "/spec/uninstall", "value": opts})
	patched, err := e.write(ctx, patch, "--namespace=backstage", "patch", "helmrelease", "backstage", "--type=json", "--patch-file=REQUEST", "--output=json")
	if err != nil || field(patched, "metadata", "uid") != field(second.owner, "metadata", "uid") || !reflect.DeepEqual(field(patched, "spec", "uninstall"), opts) || field(patched, "spec", "suspend") == true {
		return hold("HOLD_MUTATION")
	}
	current, err := e.collect(ctx)
	if err != nil || !sameRelease(second.release, current.release) || !unchangedOwner(patched, current.owner, current.chart) || !controllerHistoryMatches(current) || !reflect.DeepEqual(current.chart, second.chart) || e.deployed(current) != nil {
		return hold("HOLD_MUTATION")
	}
	currentTargets, err := retiringIdentities(current)
	if err != nil || !reflect.DeepEqual(e.retiring, currentTargets) {
		return hold("HOLD_MUTATION")
	}
	kept, err := preserved(current)
	if err != nil || !reflect.DeepEqual(before, kept) {
		return hold("HOLD_MUTATION")
	}
	if e.admissionControls(ctx, current) != nil {
		return hold("HOLD_ADMISSION")
	}
	// Kubernetes owns the ongoing finalizer once DELETE may have been submitted.
	// A workflow error, timeout or process stop cannot cancel its retries.
	if nativeSubmitted, err = e.remove(ctx, current.owner, "/apis/helm.toolkit.fluxcd.io/v2/namespaces/backstage/helmreleases/backstage"); err != nil {
		return hold("HOLD_MUTATION")
	}
	var after snapshot
	// The native uninstall is allowed five minutes. A two-minute observation
	// window would falsely fail an otherwise valid finalizer lifecycle.
	for i := 0; i < 66; i++ {
		after, err = e.collect(ctx)
		if err != nil {
			return hold("HOLD_POSTCONDITION")
		}
		kept, err = preserved(after)
		if err != nil || !reflect.DeepEqual(before, kept) || e.deployed(after) != nil || !sameStoredContent(second.release, after.release) {
			return hold("HOLD_POSTCONDITION")
		}
		if after.owner == nil && field(after.release, "info", "status") == "uninstalled" && absent(after) {
			break
		}
		if _, err = execute(ctx, "sleep", "5"); err != nil {
			return hold("HOLD_POSTCONDITION")
		}
	}
	if after.owner != nil || field(after.release, "info", "status") != "uninstalled" || !absent(after) {
		return hold("HOLD_POSTCONDITION")
	}
	if after.chart != nil {
		if field(after.chart, "metadata", "uid") != field(second.chart, "metadata", "uid") || e.chartOwned(after) != nil {
			return hold("HOLD_POSTCONDITION")
		}
		if field(after.chart, "metadata", "deletionTimestamp") == nil {
			if _, err = e.remove(ctx, after.chart, "/apis/source.toolkit.fluxcd.io/v1/namespaces/backstage/helmcharts/backstage-backstage"); err != nil {
				return hold("HOLD_MUTATION")
			}
		}
		for i := 0; i < 66; i++ {
			if _, err = execute(ctx, "sleep", "5"); err != nil {
				return hold("HOLD_POSTCONDITION")
			}
			after, err = e.collect(ctx)
			if err != nil || after.owner != nil || !absent(after) || e.deployed(after) != nil || !sameStoredContent(second.release, after.release) || field(after.release, "info", "status") != "uninstalled" {
				return hold("HOLD_POSTCONDITION")
			}
			kept, err = preserved(after)
			if err != nil || !reflect.DeepEqual(before, kept) {
				return hold("HOLD_POSTCONDITION")
			}
			if after.chart == nil {
				break
			}
			if field(after.chart, "metadata", "uid") != field(second.chart, "metadata", "uid") || field(after.chart, "metadata", "deletionTimestamp") == nil {
				return hold("HOLD_POSTCONDITION")
			}
		}
		if after.chart != nil {
			return hold("HOLD_POSTCONDITION")
		}
	}
	for sample := 0; sample < 2; sample++ {
		if _, err = execute(ctx, "sleep", "5"); err != nil {
			return hold("HOLD_POSTCONDITION")
		}
		after, err = e.collect(ctx)
		if err != nil || after.owner != nil || after.chart != nil || field(after.release, "info", "status") != "uninstalled" || !sameStoredContent(second.release, after.release) || e.deployed(after) != nil {
			return hold("HOLD_POSTCONDITION")
		}
		if !absent(after) {
			return hold("HOLD_POSTCONDITION")
		}
		kept, err = preserved(after)
		if err != nil || !reflect.DeepEqual(before, kept) {
			return hold("HOLD_POSTCONDITION")
		}
	}
	result := "PORTAL-RETIREMENT=RETIRED_STATELESS objects=3 preservationSamples=2 dataRetirement=UNASSESSED"
	if n, err := fmt.Fprintln(out, result); err != nil || n != len(result)+1 {
		return 2
	}
	return 0
}
func capture(ctx context.Context, program string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	var b, failure boundedOutput
	cmd.Stdout, cmd.Stderr = &b, &failure
	if cmd.Run() != nil {
		if failure.Len() > 0 {
			return failure.Bytes(), errors.New("command unavailable")
		}
		return b.Bytes(), errors.New("command unavailable")
	}
	return b.Bytes(), nil
}

type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxResponse {
		return 0, errors.New("response exceeds bound")
	}
	return b.Buffer.Write(p)
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, capture)) }
