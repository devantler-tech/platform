package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	umamiNameFenceMessage = "The retained Umami claim name is being released"
	umamiNamePolicyKind   = "validatingadmissionpolicies.admissionregistration.k8s.io"
	umamiNameBindingKind  = "validatingadmissionpolicybindings.admissionregistration.k8s.io"
	umamiNameFenceOwner   = "storage.platform.devantler.tech/umami-name-fence-owner"
)

func umamiNameFencePolicy(record umamiStandbyReplacementRecord) object {
	name := record.Original.Claim.Name
	quoted := strconv.Quote(name)
	oldName := "(has(object.metadata.name) && object.metadata.name == " + quoted + ")"
	mounts := func(spec string) string {
		return "(has(" + spec + ".volumes) && " + spec +
			".volumes.exists(v, has(v.persistentVolumeClaim) && v.persistentVolumeClaim.claimName == " + quoted + "))"
	}
	podMatch := oldName + " || " + mounts("dyn(object.spec)")
	jobName := "(has(object.metadata.name) && object.metadata.name in [" + strconv.Quote(name+"-join") + ", " + strconv.Quote(name+"-restore") + "])"
	jobMatch := jobName + " || (has(dyn(object.spec).template) && " + mounts("dyn(object.spec).template.spec") + ")"
	namespace := record.Original.Claim.Namespace
	policyName := "lh-" + namespace + "-claim-release-" + record.NameRelease.Owner.Run
	owner := record.Original.Claim.UID + ":" + record.NameRelease.Join.UID + ":" + record.NameRelease.Owner.Source
	creates := func(group, version string, resources ...any) object {
		return object{"apiGroups": []any{group}, "apiVersions": []any{version}, "operations": []any{"CREATE"}, "resources": resources, "scope": "Namespaced"}
	}
	return object{
		"apiVersion": benchmarkAdmissionAPI, "kind": "ValidatingAdmissionPolicy",
		"metadata": object{"name": policyName, "annotations": object{umamiNameFenceOwner: owner}},
		"spec": object{
			"failurePolicy": "Fail",
			"matchConstraints": object{"matchPolicy": "Equivalent", "namespaceSelector": object{}, "objectSelector": object{},
				"resourceRules": []any{creates("", "v1", "pods", "persistentvolumeclaims"), creates("batch", "v1", "jobs")}},
			"matchConditions": []any{object{"name": namespace + "-namespace", "expression": "request.namespace == " + strconv.Quote(namespace)}},
			"validations": []any{object{"message": cnpgNameFenceMessage(namespace), "expression": `!(request.resource.resource == "pods" && (` + podMatch + `)) && ` +
				`!(request.resource.resource == "jobs" && (` + jobMatch + `)) && ` +
				`!(request.resource.resource == "persistentvolumeclaims" && ` + oldName + ")"}},
		},
	}
}

func cnpgNameFenceMessage(namespace string) string {
	if namespace == "wedding-app" {
		return "The retained Wedding claim name is being released"
	}
	return umamiNameFenceMessage
}

func installUmamiNameFence(ctx context.Context, j *umamiStandbyReplacementJournal) error {
	if err := recordUmamiNameFence(ctx, j); err != nil {
		return err
	}
	var lastProof error
	err := waitReadyWithin(ctx, 2*time.Minute, func(ctx context.Context) error {
		proof := proveUmamiNameFence(ctx, j)
		if ctx.Err() == nil {
			lastProof = proof
		}
		return proof
	})
	if err != nil {
		return errors.Join(err, lastProof)
	}
	return nil
}

func recordUmamiNameFence(ctx context.Context, j *umamiStandbyReplacementJournal) error {
	release := *j.record.NameRelease
	desired := umamiNameFencePolicy(j.record)
	name := text(desired, "metadata", "name")
	source := j.executor().Source
	if _, err := j.read(ctx); err != nil {
		return err
	}
	policy, err := j.c.get(ctx, umamiNamePolicyKind, "", name, release.Fence.PolicyUID == "")
	if err != nil {
		return err
	}
	if policy == nil {
		policy, err = j.c.create(ctx, source, desired)
		if err != nil {
			return err
		}
	}
	policyUID := text(policy, "metadata", "uid")
	if !isolationUID.MatchString(policyUID) || !matchesUmamiNamePolicy(policy, desired, policyUID) ||
		(release.Fence.PolicyUID != "" && release.Fence.PolicyUID != policyUID) {
		return errors.New("the Umami name fence policy creation is unverified")
	}
	if release.Fence.PolicyUID == "" {
		release.Fence.PolicyUID = policyUID
		next := j.record
		checkpoint := release
		next.NameRelease = &checkpoint
		if err = j.transition(ctx, next); err != nil {
			return err
		}
	}
	desiredBinding := benchmarkDenyBinding(name, policyUID)
	valueObject(desiredBinding, "metadata")["annotations"] = value(desired, "metadata", "annotations")
	binding, err := j.c.get(ctx, umamiNameBindingKind, "", name, true)
	if err != nil {
		return err
	}
	if binding == nil {
		binding, err = j.c.create(ctx, source, desiredBinding)
		if err != nil {
			return err
		}
	}
	release.Fence.BindingUID = text(binding, "metadata", "uid")
	if !matchesOwnedBenchmarkFence(policy, binding, desired, release.Fence) || !umamiNameFenceOwned(binding, desired) {
		return errors.New("the Umami name fence binding creation is unverified")
	}
	next := j.record
	next.Phase, next.NameRelease = umamiNameFenced, &release
	return j.transition(ctx, next)
}

func umamiNameFenceOwned(resource, desired object) bool {
	owner := text(desired, "metadata", "annotations", umamiNameFenceOwner)
	return owner != "" && text(resource, "metadata", "annotations", umamiNameFenceOwner) == owner
}

func matchesUmamiNamePolicy(policy, desired object, uid string) bool {
	return umamiNameFenceOwned(policy, desired) && matchesOwnedBenchmarkPolicy(policy, desired, uid)
}

func proveUmamiNameFence(ctx context.Context, j *umamiStandbyReplacementJournal) error {
	desired, receipt := umamiNameFencePolicy(j.record), j.record.NameRelease.Fence
	database := cnpgSource{Namespace: j.record.Original.Database.Namespace, Name: j.record.Original.Database.Name}
	profile, admitted := cnpgStandbyProfileForSource(database)
	if !admitted {
		return errors.New("the claim name fence is outside the selected database transition")
	}
	return proveReferenceFenceOnServingAPIs(ctx, j.c, j.executor().Source, j.record.Run, desired, receipt,
		func(ctx context.Context, client *http.Client, endpoint *url.URL) error {
			return probeCNPGNameFence(ctx, client, endpoint, profile, text(desired, "metadata", "name"), j.record.Original.Claim.Name)
		})
}

func probeCNPGNameFence(ctx context.Context, client *http.Client, endpoint *url.URL, profile cnpgStandbyProfile, policy, claim string) error {
	namespace := profile.Source.Namespace
	meta := func(name string) object { return object{"name": name, "namespace": namespace} }
	podSpec := object{
		"restartPolicy": "Never", "automountServiceAccountToken": false,
		"securityContext": object{"runAsNonRoot": true, "runAsUser": 65532, "seccompProfile": object{"type": "RuntimeDefault"}},
		"containers": []any{object{"name": "probe", "image": "registry.k8s.io/pause:3.10",
			"securityContext": object{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": object{"drop": []string{"ALL"}}}}},
		"volumes": []any{object{"name": "old-data", "persistentVolumeClaim": object{"claimName": claim}}},
	}
	claimSpec := object{"accessModes": []any{"ReadWriteOnce"}, "storageClassName": "longhorn-v2",
		"resources": object{"requests": object{"storage": profile.Size}}}
	probes := []struct {
		group, resource, path, name string
		body                        object
	}{
		{"", "pods", "/api/v1/namespaces/" + namespace + "/pods", "longhorn-v2-name-fence-probe",
			object{"apiVersion": "v1", "kind": "Pod", "metadata": meta("longhorn-v2-name-fence-probe"), "spec": podSpec}},
		{"batch", "jobs", "/apis/batch/v1/namespaces/" + namespace + "/jobs", claim + "-join",
			object{"apiVersion": "batch/v1", "kind": "Job", "metadata": meta(claim + "-join"), "spec": object{"template": object{"spec": podSpec}}}},
		{"", "persistentvolumeclaims", "/api/v1/namespaces/" + namespace + "/persistentvolumeclaims", claim,
			object{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": meta(claim), "spec": claimSpec}},
	}
	for _, probe := range probes {
		code, raw, err := benchmarkAdmissionRequest(ctx, client, endpoint, probe.path, http.MethodPost, "application/json", probe.body)
		if err != nil {
			return fmt.Errorf("a serving API did not return the %s %s dry-run response: %w", profile.Source.Name, probe.resource, err)
		}
		// The installed policy omits validation.reason. Kubernetes attributes
		// the denial to the same policy and binding but returns Invalid (422).
		if code != http.StatusUnprocessableEntity ||
			!nativePolicyDenialMatches(code, raw, metav1.StatusReasonInvalid, probe.group, probe.resource, probe.name, policy, policy, cnpgNameFenceMessage(namespace)) {
			return fmt.Errorf("a serving API did not prove the exact %s %s fence (HTTP %d)", profile.Source.Name, probe.resource, code)
		}
	}
	return nil
}

func removeUmamiNameFence(ctx context.Context, j *umamiStandbyReplacementJournal) error {
	desired := umamiNameFencePolicy(j.record)
	name := text(desired, "metadata", "name")
	c, source := j.c, j.executor().Source
	policy, err := c.get(ctx, umamiNamePolicyKind, "", name, true)
	if err != nil {
		return err
	}
	receipt := j.record.NameRelease.Fence
	binding, err := c.get(ctx, umamiNameBindingKind, "", name, true)
	if err != nil || (policy != nil && !matchesUmamiNamePolicy(policy, desired, receipt.PolicyUID)) ||
		(binding != nil && (!matchesOwnedBenchmarkFence(policy, binding, desired, receipt) || !umamiNameFenceOwned(binding, desired))) {
		return errors.New("the original Umami name fence changed before cleanup")
	}
	if err = observeUmamiReleasedOriginal(ctx, j, binding != nil); err != nil {
		return err
	}
	bindings, err := c.list(ctx, umamiNameBindingKind, "")
	if err != nil {
		return err
	}
	for _, other := range bindings {
		if text(other, "spec", "policyName") == name && benchmarkIdentity(other) != benchmarkIdentity(binding) {
			return errors.New("a foreign binding references the Umami name fence")
		}
	}
	base := "/apis/" + benchmarkAdmissionAPI + "/"
	if binding != nil {
		if err = c.deleteObserved(ctx, source, base+"validatingadmissionpolicybindings/"+name, binding); err != nil {
			return err
		}
		if err = waitForUmamiNameFenceRemoval(ctx, c, umamiNameBindingKind, name); err != nil {
			return err
		}
	}
	if policy != nil {
		if err = c.deleteObserved(ctx, source, base+"validatingadmissionpolicies/"+name, policy); err != nil {
			return err
		}
	}
	return waitForUmamiNameFenceRemoval(ctx, c, umamiNamePolicyKind, name)
}

func waitForUmamiNameFenceRemoval(ctx context.Context, c client, kind, name string) error {
	return waitReadyWithin(ctx, time.Minute, func(ctx context.Context) error {
		resource, err := c.get(ctx, kind, "", name, true)
		if err != nil || resource != nil {
			return errors.New("the original Umami admission fence is still present")
		}
		return nil
	})
}
