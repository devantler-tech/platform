package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	umamiNameFenceMessage = "The retained Umami claim name is being released"
	umamiNamePolicyKind   = "validatingadmissionpolicies.admissionregistration.k8s.io"
	umamiNameBindingKind  = "validatingadmissionpolicybindings.admissionregistration.k8s.io"
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
	policyName := "lh-umami-claim-release-" + record.NameRelease.Owner.Run
	creates := func(group, version string, resources ...any) object {
		return object{"apiGroups": []any{group}, "apiVersions": []any{version}, "operations": []any{"CREATE"}, "resources": resources, "scope": "Namespaced"}
	}
	return object{
		"apiVersion": benchmarkAdmissionAPI, "kind": "ValidatingAdmissionPolicy", "metadata": object{"name": policyName},
		"spec": object{
			"failurePolicy": "Fail",
			"matchConstraints": object{"matchPolicy": "Equivalent", "namespaceSelector": object{}, "objectSelector": object{},
				"resourceRules": []any{creates("", "v1", "pods", "persistentvolumeclaims"), creates("batch", "v1", "jobs")}},
			"matchConditions": []any{object{"name": "umami-namespace", "expression": `request.namespace == "umami"`}},
			"validations": []any{object{"message": umamiNameFenceMessage, "expression": `!(request.resource.resource == "pods" && (` + podMatch + `)) && ` +
				`!(request.resource.resource == "jobs" && (` + jobMatch + `)) && ` +
				`!(request.resource.resource == "persistentvolumeclaims" && ` + oldName + ")"}},
		},
	}
}

func installUmamiNameFence(ctx context.Context, j *umamiStandbyReplacementJournal) error {
	release := *j.record.NameRelease
	desired := umamiNameFencePolicy(j.record)
	name := text(desired, "metadata", "name")
	source := j.executor().Source
	if _, err := j.read(ctx); err != nil {
		return err
	}
	policy, err := j.c.create(ctx, source, desired)
	if err != nil {
		return err
	}
	release.Fence.PolicyUID = text(policy, "metadata", "uid")
	if !matchesOwnedBenchmarkPolicy(policy, desired, release.Fence.PolicyUID) {
		return errors.New("the Umami name fence policy creation is unverified")
	}
	binding, err := j.c.create(ctx, source, benchmarkDenyBinding(name, release.Fence.PolicyUID))
	if err != nil {
		return err
	}
	release.Fence.BindingUID = text(binding, "metadata", "uid")
	if !matchesOwnedBenchmarkFence(policy, binding, desired, release.Fence) {
		return errors.New("the Umami name fence binding creation is unverified")
	}
	next := j.record
	next.Phase, next.NameRelease = umamiNameFenced, &release
	if err = j.transition(ctx, next); err != nil {
		return err
	}
	return waitReadyWithin(ctx, 2*time.Minute, func(ctx context.Context) error { return proveUmamiNameFence(ctx, j) })
}

func proveUmamiNameFence(ctx context.Context, j *umamiStandbyReplacementJournal) error {
	desired, receipt := umamiNameFencePolicy(j.record), j.record.NameRelease.Fence
	return proveReferenceFenceOnServingAPIs(ctx, j.c, j.executor().Source, j.record.Run, desired, receipt,
		func(ctx context.Context, client *http.Client, endpoint *url.URL) error {
			return probeUmamiNameFence(ctx, client, endpoint, text(desired, "metadata", "name"), j.record.Original.Claim.Name)
		})
}

func probeUmamiNameFence(ctx context.Context, client *http.Client, endpoint *url.URL, policy, claim string) error {
	meta := func(name string) object { return object{"name": name, "namespace": "umami"} }
	podSpec := object{
		"restartPolicy": "Never", "automountServiceAccountToken": false,
		"securityContext": object{"runAsNonRoot": true, "runAsUser": 65532, "seccompProfile": object{"type": "RuntimeDefault"}},
		"containers": []any{object{"name": "probe", "image": "registry.k8s.io/pause:3.10",
			"securityContext": object{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": object{"drop": []string{"ALL"}}}}},
		"volumes": []any{object{"name": "old-data", "persistentVolumeClaim": object{"claimName": claim}}},
	}
	claimSpec := object{"accessModes": []any{"ReadWriteOnce"}, "storageClassName": "longhorn-v2",
		"resources": object{"requests": object{"storage": "5Gi"}}}
	probes := []struct {
		group, resource, path, name string
		body                        object
	}{
		{"", "pods", "/api/v1/namespaces/umami/pods", "longhorn-v2-name-fence-probe",
			object{"apiVersion": "v1", "kind": "Pod", "metadata": meta("longhorn-v2-name-fence-probe"), "spec": podSpec}},
		{"batch", "jobs", "/apis/batch/v1/namespaces/umami/jobs", claim + "-join",
			object{"apiVersion": "batch/v1", "kind": "Job", "metadata": meta(claim + "-join"), "spec": object{"template": object{"spec": podSpec}}}},
		{"", "persistentvolumeclaims", "/api/v1/namespaces/umami/persistentvolumeclaims", claim,
			object{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": meta(claim), "spec": claimSpec}},
	}
	for _, probe := range probes {
		code, raw, err := benchmarkAdmissionRequest(ctx, client, endpoint, probe.path, http.MethodPost, "application/json", probe.body)
		if err != nil || !benchmarkDenialMatches(code, raw, probe.group, probe.resource, probe.name, policy, policy, umamiNameFenceMessage) {
			return errors.New("a serving API did not prove the exact Umami " + probe.resource + " fence")
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
	if err != nil || (policy != nil && !matchesOwnedBenchmarkPolicy(policy, desired, receipt.PolicyUID)) ||
		(binding != nil && !matchesOwnedBenchmarkFence(policy, binding, desired, receipt)) {
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
