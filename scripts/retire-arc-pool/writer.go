package main

import (
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

type writerEpoch struct {
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
}
type writerLayer struct {
	Name string `json:"name"`
	writerEpoch
}
type writerReceipt struct {
	Owner  identity         `json:"owner"`
	Digest string           `json:"digest"`
	Ticket string           `json:"ticket"`
	OCI    writerEpoch      `json:"oci"`
	Layers []writerLayer    `json:"layers"`
	Flux   *fluxWriterEpoch `json:"flux,omitempty"`
}

const reviewedFluxImage = "ghcr.io/fluxcd/kustomize-controller:v1.8.5@sha256:70e2a25edeee82690e68662409fb1b60f7d3084c93435e8ab5337a6e0244ffed"
const reviewedFluxImageID = "ghcr.io/fluxcd/kustomize-controller@sha256:70e2a25edeee82690e68662409fb1b60f7d3084c93435e8ab5337a6e0244ffed"

type fluxWriterEpoch struct {
	UID        string   `json:"uid"`
	Generation int64    `json:"generation"`
	PodUIDs    []string `json:"podUIDs"`
	Image      string   `json:"image"`
}

func fluxWriterProof(value any) (*fluxWriterEpoch, error) {
	dep := at(value, "Deployment")
	gen, gok := numberAt(dep, "metadata", "generation")
	observed, ook := numberAt(dep, "status", "observedGeneration")
	if !exactObject(dep, "kustomize-controller", "flux-system") || !gok || gen < 1 || !ook || observed != gen || at(dep, "spec", "paused") == true || !effectiveZero(dep, "status", "terminatingReplicas") || !effectiveZero(dep, "status", "unavailableReplicas") {
		return nil, errors.New("source writer rollout is incomplete")
	}
	for _, path := range [][]string{{"spec", "replicas"}, {"status", "replicas"}, {"status", "updatedReplicas"}, {"status", "readyReplicas"}, {"status", "availableReplicas"}} {
		n, ok := numberAt(dep, path...)
		if !ok || n != 2 {
			return nil, errors.New("source writer replica inventory changed")
		}
	}
	containers, cok := sliceAt(dep, "spec", "template", "spec", "containers")
	if !cok || len(containers) != 1 || textAt(containers[0], "name") != "manager" || textAt(containers[0], "image") != reviewedFluxImage {
		return nil, errors.New("source failure proof requires the reviewed controller image")
	}
	pods, pok := sliceAt(value, "Pods")
	sets, sok := sliceAt(value, "ReplicaSets")
	if !pok || len(pods) != 2 || !sok {
		return nil, errors.New("source writer process inventory is incomplete")
	}
	f := &fluxWriterEpoch{UID: textAt(dep, "metadata", "uid"), Generation: gen, Image: reviewedFluxImage}
	seen := map[string]bool{}
	for _, pod := range pods {
		uid := textAt(pod, "uid")
		if uid == "" || seen[uid] || textAt(pod, "phase") != "Running" || textAt(pod, "ready") != "True" || textAt(pod, "deleting") != "" || textAt(pod, "ownerKind") != "ReplicaSet" || textAt(pod, "imageID") != reviewedFluxImageID {
			return nil, errors.New("source writer has an old or unreviewed process")
		}
		owners := 0
		for _, rs := range sets {
			if textAt(rs, "metadata", "uid") == textAt(pod, "ownerUID") && controllingOwner(rs, "Deployment", f.UID) && at(rs, "metadata", "deletionTimestamp") == nil {
				owners++
			}
		}
		if owners != 1 {
			return nil, errors.New("source writer process ownership is incomplete")
		}
		seen[uid] = true
		f.PodUIDs = append(f.PodUIDs, uid)
	}
	sort.Strings(f.PodUIDs)
	return f, nil
}

func decodeWriter(raw []byte) (*writerReceipt, error) {
	fields, err := canonicalFields(raw, []string{"owner", "digest", "ticket", "oci", "layers"}, "flux")
	if err != nil {
		return nil, err
	}
	owner, err := canonicalFields(fields["owner"], []string{"run", "attempt", "sha"}, "")
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"run", "attempt", "sha"} {
		if _, err := exactString(owner, key); err != nil {
			return nil, err
		}
	}
	var w writerReceipt
	if json.Unmarshal(raw, &w) != nil || !validIdentity(w.Owner) || !validSource(w.Owner.SHA, w.Digest) || !regexp.MustCompile(`^native-`+w.Owner.Run+`-`+w.Owner.Attempt+`-[0-9a-f]{32}$`).MatchString(w.Ticket) {
		return nil, errors.New("invalid source barrier producer")
	}
	if _, err := canonicalFields(fields["oci"], []string{"uid", "generation"}, ""); err != nil || w.OCI.UID == "" || w.OCI.Generation < 1 {
		return nil, errors.New("invalid source epoch")
	}
	var rows []json.RawMessage
	if json.Unmarshal(fields["layers"], &rows) != nil || len(rows) != 3 {
		return nil, errors.New("incomplete source barrier layers")
	}
	seen := map[string]bool{}
	for i, row := range rows {
		if _, err := canonicalFields(row, []string{"name", "uid", "generation"}, ""); err != nil {
			return nil, err
		}
		layer := w.Layers[i]
		if (layer.Name != "flux-system" && layer.Name != "infrastructure-controllers" && layer.Name != "infrastructure") || seen[layer.Name] || layer.UID == "" || layer.Generation < 1 {
			return nil, errors.New("ambiguous source layer epoch")
		}
		seen[layer.Name] = true
	}
	if w.Flux != nil {
		if _, err := canonicalFields(fields["flux"], []string{"uid", "generation", "podUIDs", "image"}, ""); err != nil || w.Flux.UID == "" || w.Flux.Generation < 1 || w.Flux.Image != reviewedFluxImage || len(w.Flux.PodUIDs) != 2 || w.Flux.PodUIDs[0] == "" || w.Flux.PodUIDs[1] == "" || w.Flux.PodUIDs[0] >= w.Flux.PodUIDs[1] {
			return nil, errors.New("invalid source failure runtime epoch")
		}
	}
	return &w, nil
}
func recordWriter(s state, body []byte) ([]operation, error) {
	j, err := verify(s)
	if err != nil {
		return nil, err
	}
	if j.Baseline == nil || j.Phase != "fenced" || sourceWriterProof(s, j, body) != nil {
		return nil, errors.New("source barrier is incomplete")
	}
	proof, err := evidence(body)
	if err != nil {
		return nil, err
	}
	ticket := textAt(proof, "Ticket")
	if !regexp.MustCompile(`^native-` + s.Owner.Run + `-` + s.Owner.Attempt + `-[0-9a-f]{32}$`).MatchString(ticket) {
		return nil, errors.New("source barrier belongs to another invocation")
	}
	src := at(proof, "OCI")
	gen, _ := numberAt(src, "metadata", "generation")
	w := &writerReceipt{Owner: s.Owner, Digest: textAt(proof, "Digest"), Ticket: ticket, OCI: writerEpoch{UID: textAt(src, "metadata", "uid"), Generation: gen}}
	if sourceProof(body) != nil {
		w.Flux, err = fluxWriterProof(at(proof, "Flux"))
		if err != nil {
			return nil, err
		}
	}
	layers, _ := sliceAt(proof, "Kustomizations")
	for _, layer := range layers {
		gen, _ := numberAt(layer, "metadata", "generation")
		w.Layers = append(w.Layers, writerLayer{Name: textAt(layer, "metadata", "name"), writerEpoch: writerEpoch{UID: textAt(layer, "metadata", "uid"), Generation: gen}})
	}
	sort.Slice(w.Layers, func(i, k int) bool { return w.Layers[i].Name < w.Layers[k].Name })
	raw, _ := json.Marshal(w)
	if _, err := decodeWriter(raw); err != nil {
		return nil, err
	}
	j.Baseline.Writer = w
	return record(s, j)
}

// A partial opening can never become Ready after its missing CREATE is closed.
// Flux v1.8.5 finalizes the request nonce after SSA v0.67.5 has joined every
// apply worker. Its precise dry-run denial and ProgressingWithRetry conditions
// therefore prove a completed attempt, while never asserting source readiness.
// Keep the normal and post-publication Ready proof unchanged.
func sourceWriterProof(s state, j journal, body []byte) error {
	if sourceProof(body) == nil {
		return nil
	}
	o, err := evidence(body)
	if err != nil || j.Baseline == nil || j.Phase != "fenced" || j.Baseline.Maximum != 0 || textAt(o, "Digest") != j.Baseline.Digest {
		return errors.New("opening writer authority is incomplete")
	}
	ticket := textAt(o, "Ticket")
	if !regexp.MustCompile(`^native-` + s.Owner.Run + `-` + s.Owner.Attempt + `-[0-9a-f]{32}$`).MatchString(ticket) {
		return errors.New("opening writer request is not fresh for this attempt")
	}
	src := at(o, "OCI")
	gen, _ := numberAt(src, "metadata", "generation")
	w := &writerReceipt{Owner: s.Owner, Digest: j.Baseline.Digest, Ticket: ticket, OCI: writerEpoch{UID: textAt(src, "metadata", "uid"), Generation: gen}}
	layers, ok := sliceAt(o, "Kustomizations")
	if !ok || len(layers) != 3 {
		return errors.New("opening source layer inventory is incomplete")
	}
	failed := 0
	for _, layer := range layers {
		name := textAt(layer, "metadata", "name")
		gen, gok := numberAt(layer, "metadata", "generation")
		w.Layers = append(w.Layers, writerLayer{Name: name, writerEpoch: writerEpoch{UID: textAt(layer, "metadata", "uid"), Generation: gen}})
		if !gok || gen < 1 || textAt(layer, "metadata", "annotations", "reconcile.fluxcd.io/requestedAt") != ticket || textAt(layer, "status", "lastHandledReconcileAt") != ticket || textAt(layer, "status", "lastAttemptedRevision") != "latest@"+w.Digest {
			return errors.New("opening source attempt is stale or unacknowledged")
		}
		if currentReady(layer) && textAt(layer, "status", "lastAppliedRevision") == "latest@"+w.Digest {
			continue
		}
		if name != "infrastructure" || !completedMissingCreate(s, j, layer, gen) {
			return errors.New("opening source failure is not the completed missing-create denial")
		}
		failed++
	}
	if failed != 1 {
		return errors.New("opening source failure is ambiguous")
	}
	before, err := fluxWriterProof(at(o, "FluxBefore"))
	if err != nil {
		return err
	}
	w.Flux, err = fluxWriterProof(at(o, "Flux"))
	if err != nil || !reflect.DeepEqual(before, w.Flux) {
		return errors.New("source failure runtime changed across the fresh request")
	}
	// Reuse the exact signed OCI and three unchanged configuration checks. This
	// does not normalize or manufacture any Ready condition in the evidence.
	o["Writer"] = w
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return writerCurrentProof(raw)
}

func completedMissingCreate(s state, j journal, layer any, generation int64) bool {
	conditions, ok := sliceAt(layer, "status", "conditions")
	if !ok {
		return false
	}
	seen := map[string]bool{}
	ready, retry := false, false
	for _, condition := range conditions {
		typ := textAt(condition, "type")
		if typ == "" || seen[typ] {
			return false
		}
		seen[typ] = true
		g, gok := numberAt(condition, "observedGeneration")
		switch typ {
		case "Stalled":
			if textAt(condition, "status") == "True" {
				return false
			}
		case "Reconciling":
			retry = gok && g == generation && textAt(condition, "status") == "True" && textAt(condition, "reason") == "ProgressingWithRetry"
		case "Ready":
			if !gok || g != generation || textAt(condition, "status") != "False" || textAt(condition, "reason") != "ReconciliationFailed" {
				return false
			}
			message := textAt(condition, "message")
			for _, missing := range []struct {
				absent       bool
				target, rule string
			}{
				{s.Release == nil && j.HRUID == "", "HelmRelease/arc-runners/platform-runners", "fence-pool-retirement"},
				{s.Credential == nil && j.ESOUID == "", "ExternalSecret/arc-runners/arc-github-app", "fence-credential-recreation"},
			} {
				if missing.absent && strings.HasPrefix(message, missing.target+` dry-run failed: admission webhook "validate.kyverno.svc-fail" denied the request:`) && strings.Contains(message, "\nrestrict-arc-retirement:\n  "+missing.rule+":") {
					ready = true
				}
			}
		}
	}
	return ready && retry
}
func writerCurrentProof(body []byte) error {
	o, err := evidence(body)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(at(o, "Writer"))
	if err != nil {
		return err
	}
	w, err := decodeWriter(raw)
	if err != nil {
		return err
	}
	src := at(o, "OCI")
	gen, gok := numberAt(src, "metadata", "generation")
	if !exactObject(src, "flux-system", "flux-system") || !currentReady(src) || textAt(src, "metadata", "uid") != w.OCI.UID || !gok || gen != w.OCI.Generation || !sourceArtifact(src, w.Digest) || textAt(src, "spec", "verify", "provider") != "cosign" {
		return errors.New("completed source barrier authority changed")
	}
	verified := false
	conditions, _ := sliceAt(src, "status", "conditions")
	for _, condition := range conditions {
		if textAt(condition, "type") == "SourceVerified" {
			cg, ok := numberAt(condition, "observedGeneration")
			verified = textAt(condition, "status") == "True" && ok && cg == gen
		}
	}
	if !verified {
		return errors.New("current source signature is not verified")
	}
	if w.Flux != nil {
		current, err := fluxWriterProof(at(o, "Flux"))
		if err != nil || !reflect.DeepEqual(current, w.Flux) {
			return errors.New("completed source failure runtime changed")
		}
	}
	layers, ok := sliceAt(o, "Kustomizations")
	if !ok || len(layers) != 3 {
		return errors.New("source layer inventory changed")
	}
	expected := map[string]writerEpoch{}
	for _, layer := range w.Layers {
		expected[layer.Name] = layer.writerEpoch
	}
	seen := map[string]bool{}
	for _, layer := range layers {
		name := textAt(layer, "metadata", "name")
		epoch, ok := expected[name]
		g, gok := numberAt(layer, "metadata", "generation")
		if !ok || seen[name] || !exactObject(layer, name, "flux-system") || textAt(layer, "metadata", "uid") != epoch.UID || !gok || g != epoch.Generation || at(layer, "spec", "suspend") == true || textAt(layer, "metadata", "annotations", reconcileKey) == "disabled" || textAt(layer, "spec", "sourceRef", "kind") != "OCIRepository" || textAt(layer, "spec", "sourceRef", "name") != "flux-system" || (textAt(layer, "spec", "sourceRef", "namespace") != "" && textAt(layer, "spec", "sourceRef", "namespace") != "flux-system") {
			return errors.New("source layer configuration changed")
		}
		seen[name] = true
	}
	return nil
}
func verifyDelete(s state) (journal, error) {
	j, err := verify(s)
	if err != nil {
		return j, err
	}
	if j.Baseline != nil && (j.Baseline.Writer == nil || !s.WriterCurrent) {
		return j, errors.New("baseline deletion lacks unchanged completed writer authority")
	}
	if !strings.Contains("|uninstalling|credential-removing|", "|"+j.Phase+"|") {
		return j, errors.New("unexpected deletion phase")
	}
	return j, nil
}
