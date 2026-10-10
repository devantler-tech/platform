package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

type object map[string]any

func evidence(body []byte) (object, error) {
	if len(body) > 1<<20 || unambiguous(body) != nil {
		return nil, errors.New("ambiguous or oversized proof")
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	var o object
	if d.Decode(&o) != nil || o == nil || d.Decode(new(any)) != io.EOF {
		return nil, errors.New("incomplete proof")
	}
	return o, nil
}
func currentReady(o any) bool {
	gen, gok := numberAt(o, "metadata", "generation")
	observed, ook := numberAt(o, "status", "observedGeneration")
	if !gok || gen < 1 || !ook || gen != observed || at(o, "metadata", "deletionTimestamp") != nil || at(o, "spec", "suspend") == true {
		return false
	}
	conditions, ok := sliceAt(o, "status", "conditions")
	if !ok {
		return false
	}
	ready := 0
	seen := map[string]bool{}
	for _, c := range conditions {
		typ := textAt(c, "type")
		if typ == "" || seen[typ] {
			return false
		}
		seen[typ] = true
		if typ == "Ready" {
			cg, cok := numberAt(c, "observedGeneration")
			if textAt(c, "status") != "True" || !cok || cg != gen {
				return false
			}
			ready++
		}
		if (typ == "Reconciling" || typ == "Stalled") && textAt(c, "status") == "True" {
			return false
		}
	}
	return ready == 1
}
func controllerProof(body []byte) error {
	o, err := evidence(body)
	if err != nil {
		return err
	}
	dep := at(o, "Deployment")
	j := at(o, "Journal")
	ticket := textAt(o, "Ticket")
	pods, pok := sliceAt(o, "Pods")
	sets, sok := sliceAt(o, "ReplicaSets")
	old, ook := sliceAt(j, "controllerPodUIDs")
	if !exactObject(dep, "arc-controller", "arc-systems") || textAt(dep, "metadata", "uid") != textAt(j, "controllerUID") ||
		!pok || len(pods) != 1 || !sok || !ook || len(old) < 1 || len(old) > 32 || ticket == "" || ticket != textAt(j, "controllerTicket") ||
		textAt(dep, "spec", "template", "metadata", "annotations", "platform.devantler.tech/arc-retirement-restart") != ticket {
		return errors.New("controller process receipt is incomplete")
	}
	gen, gok := numberAt(dep, "metadata", "generation")
	observed, obsok := numberAt(dep, "status", "observedGeneration")
	if !gok || gen < 1 || !obsok || observed != gen {
		return errors.New("controller rollout generation is stale")
	}
	for _, path := range [][]string{{"spec", "replicas"}, {"status", "replicas"}, {"status", "updatedReplicas"}, {"status", "readyReplicas"}, {"status", "availableReplicas"}} {
		n, ok := numberAt(dep, path...)
		if !ok || n != 1 {
			return errors.New("controller rollout is incomplete")
		}
	}
	if !effectiveZero(dep, "status", "unavailableReplicas") {
		return errors.New("controller is unavailable")
	}
	containers, cok := sliceAt(dep, "spec", "template", "spec", "containers")
	if !cok || len(containers) != 1 || textAt(containers[0], "name") != "manager" {
		return errors.New("unexpected controller executable")
	}
	command, comok := sliceAt(containers[0], "command")
	args, aok := sliceAt(containers[0], "args")
	if !comok || len(command) != 1 || command[0] != "/manager" || !aok {
		return errors.New("unexpected controller command")
	}
	scope, mode := 0, 0
	for _, v := range args {
		arg, ok := v.(string)
		if !ok || !regexp.MustCompile(`^--[a-z][a-z0-9-]*(=.*)?$`).MatchString(arg) {
			return errors.New("ambiguous controller arguments")
		}
		switch {
		case strings.HasPrefix(arg, "--watch-single-namespace"):
			if arg != "--watch-single-namespace=arc-runners" {
				return errors.New("unexpected controller scope")
			}
			scope++
		case strings.HasPrefix(arg, "--watch-namespace"):
			return errors.New("additional controller scope")
		case strings.HasPrefix(arg, "--auto-scaling-runner-set-only"):
			if arg != "--auto-scaling-runner-set-only" {
				return errors.New("unexpected controller mode")
			}
			mode++
		}
	}
	if scope != 1 || mode != 1 {
		return errors.New("controller scope or mode is missing")
	}
	pod := pods[0]
	if textAt(pod, "uid") == "" || textAt(pod, "phase") != "Running" || textAt(pod, "ready") != "True" || textAt(pod, "deleting") != "" || textAt(pod, "ownerKind") != "ReplicaSet" {
		return errors.New("current controller process is not Ready")
	}
	seen := map[string]bool{}
	for _, v := range old {
		id, ok := v.(string)
		if !ok || id == "" || seen[id] || id == textAt(pod, "uid") {
			return errors.New("old controller process remains or its receipt is ambiguous")
		}
		seen[id] = true
	}
	owners := 0
	for _, rs := range sets {
		if textAt(rs, "metadata", "uid") == textAt(pod, "ownerUID") && controllingOwner(rs, "Deployment", textAt(dep, "metadata", "uid")) && at(rs, "metadata", "deletionTimestamp") == nil {
			owners++
		}
	}
	if owners != 1 {
		return errors.New("controller Pod does not belong to the current deployment")
	}
	return nil
}
func sourceProof(body []byte) error {
	o, err := evidence(body)
	if err != nil {
		return err
	}
	digest := textAt(o, "Digest")
	ticket := textAt(o, "Ticket")
	src := at(o, "OCI")
	layers, ok := sliceAt(o, "Kustomizations")
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(digest) || ticket == "" || !ok || len(layers) != 3 ||
		!exactObject(src, "flux-system", "flux-system") || !currentReady(src) || !sourceArtifact(src, digest) ||
		textAt(src, "spec", "verify", "provider") != "cosign" {
		return errors.New("signed published source is not current")
	}
	conditions, _ := sliceAt(src, "status", "conditions")
	gen, _ := numberAt(src, "metadata", "generation")
	verified := false
	for _, condition := range conditions {
		if textAt(condition, "type") == "SourceVerified" {
			observed, ok := numberAt(condition, "observedGeneration")
			verified = textAt(condition, "status") == "True" && ok && observed == gen
		}
	}
	if !verified {
		return errors.New("current artifact signature has not been verified")
	}
	seen := map[string]bool{}
	for _, layer := range layers {
		name := textAt(layer, "metadata", "name")
		if (name != "flux-system" && name != "infrastructure-controllers" && name != "infrastructure") || seen[name] ||
			!exactObject(layer, name, "flux-system") || !currentReady(layer) ||
			textAt(layer, "spec", "sourceRef", "kind") != "OCIRepository" || textAt(layer, "spec", "sourceRef", "name") != "flux-system" ||
			textAt(layer, "metadata", "annotations", reconcileKey) == "disabled" ||
			textAt(layer, "metadata", "annotations", "reconcile.fluxcd.io/requestedAt") != ticket ||
			textAt(layer, "status", "lastHandledReconcileAt") != ticket ||
			textAt(layer, "status", "lastAppliedRevision") != "latest@"+digest || textAt(layer, "status", "lastAttemptedRevision") != "latest@"+digest {
			return errors.New("inactive source has not joined every current layer")
		}
		seen[name] = true
	}
	return nil
}

// Flux's revision identifies the signed upstream OCI manifest. Its digest is
// the independently hashed stored archive, not the registry manifest digest.
func sourceArtifact(src any, digest string) bool {
	return textAt(src, "status", "artifact", "revision") == "latest@"+digest && regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(textAt(src, "status", "artifact", "digest"))
}
func at(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			if o, yes := v.(object); yes {
				m = map[string]any(o)
			} else {
				return nil
			}
		}
		v = m[k]
	}
	return v
}
func textAt(o any, keys ...string) string { v, _ := at(o, keys...).(string); return v }
func numberAt(o any, keys ...string) (int64, bool) {
	v, ok := at(o, keys...).(json.Number)
	if !ok {
		return 0, false
	}
	n, e := v.Int64()
	return n, e == nil
}
func zeroAt(o any, keys ...string) bool           { n, ok := numberAt(o, keys...); return ok && n == 0 }
func effectiveZero(o any, keys ...string) bool    { return at(o, keys...) == nil || zeroAt(o, keys...) }
func sliceAt(o any, keys ...string) ([]any, bool) { v, ok := at(o, keys...).([]any); return v, ok }
func exactObject(o any, name, namespace string) bool {
	return textAt(o, "metadata", "name") == name && textAt(o, "metadata", "namespace") == namespace &&
		textAt(o, "metadata", "uid") != "" && at(o, "metadata", "deletionTimestamp") == nil
}
func controllingOwner(o any, kind, uid string) bool {
	owners, ok := sliceAt(o, "metadata", "ownerReferences")
	if !ok {
		return false
	}
	matches := 0
	for _, owner := range owners {
		if at(owner, "controller") == true {
			if textAt(owner, "kind") != kind || textAt(owner, "uid") != uid {
				return false
			}
			matches++
		}
	}
	return matches == 1
}
func drainProof(body []byte) error {
	o, err := evidence(body)
	if err != nil {
		return err
	}
	hr := at(o, "HR")
	ars := at(o, "ARS")
	ers := at(o, "ERS")
	listeners, lok := sliceAt(o, "Listeners")
	pods, pok := sliceAt(o, "ListenerPods")
	if !lok || !pok || at(o, "ChildrenAbsent") != true || at(o, "NodesAbsent") != true {
		return errors.New("runner work or capacity remains")
	}
	if hr != nil && (at(hr, "spec", "suspend") != false || !zeroAt(hr, "spec", "values", "minRunners") || !zeroAt(hr, "spec", "values", "maxRunners") || textAt(hr, "metadata", "annotations", reconcileKey) != "disabled") {
		return errors.New("release is not fenced at explicit zero")
	}
	if ars == nil {
		if len(listeners) != 0 || len(pods) != 0 || ers != nil {
			return errors.New("unowned ARC children remain")
		}
		return nil
	}
	gen, gok := numberAt(ars, "metadata", "generation")
	observed, ook := numberAt(ars, "status", "observedGeneration")
	if !exactObject(ars, "platform-linux", "arc-runners") || !gok || gen < 1 || !ook || observed != gen || !zeroAt(ars, "spec", "minRunners") || !zeroAt(ars, "spec", "maxRunners") {
		return errors.New("scale set is not current and explicitly zero")
	}
	if len(listeners) == 0 {
		if len(pods) != 0 {
			return errors.New("old listener process remains")
		}
	} else {
		if len(listeners) != 1 || len(pods) != 1 {
			return errors.New("listener ownership is incomplete")
		}
		al := listeners[0]
		pod := pods[0]
		scaleID, idok := numberAt(al, "spec", "runnerScaleSetId")
		if !exactObject(al, textAt(al, "metadata", "name"), "arc-systems") || textAt(al, "metadata", "name") == "" ||
			textAt(al, "spec", "autoscalingRunnerSetName") != "platform-linux" || textAt(al, "spec", "autoscalingRunnerSetNamespace") != "arc-runners" ||
			textAt(al, "spec", "ephemeralRunnerSetName") != "platform-linux" || !idok || scaleID < 1 ||
			textAt(ars, "metadata", "annotations", "runner-scale-set-id") != fmt.Sprint(scaleID) ||
			!effectiveZero(al, "spec", "minRunners") || !effectiveZero(al, "spec", "maxRunners") ||
			textAt(pod, "uid") == "" || textAt(pod, "ownerKind") != "AutoscalingListener" || textAt(pod, "ownerUID") != textAt(al, "metadata", "uid") ||
			textAt(pod, "phase") != "Running" || textAt(pod, "ready") != "True" || textAt(pod, "deleting") != "" {
			return errors.New("current zero listener has not replaced every old listener process")
		}
	}
	if ers != nil {
		actionable := at(ers, "spec", "actionableRevision")
		applied := at(ers, "status", "appliedActionableRevision")
		if actionable == nil {
			actionable = json.Number("0")
		}
		if applied == nil {
			applied = json.Number("0")
		}
		a, aok := actionable.(json.Number)
		b, bok := applied.(json.Number)
		ai, ae := a.Int64()
		bi, be := b.Int64()
		if !exactObject(ers, "platform-linux", "arc-runners") || !controllingOwner(ers, "AutoscalingRunnerSet", textAt(ars, "metadata", "uid")) ||
			textAt(ers, "metadata", "annotations", "actions.github.com/autoscaling-runner-set-generation") != fmt.Sprint(gen) ||
			!effectiveZero(ers, "spec", "replicas") || textAt(ers, "status", "phase") != "Running" || !aok || !bok || ae != nil || be != nil || ai < 0 || ai != bi {
			return errors.New("zero runner-set revision has not been processed")
		}
	}
	return nil
}
