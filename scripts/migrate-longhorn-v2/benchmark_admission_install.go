package main

import (
	"context"
	"errors"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
)

const benchmarkAdmissionAPI = "admissionregistration.k8s.io/v1"

type benchmarkFenceReceipt struct{ PolicyUID, BindingUID string }

func benchmarkFenceNames(phase, run string) []string {
	suffix := "-" + phase + "-" + run
	return []string{"lh-benchmark-retire-pv" + suffix, "lh-benchmark-retire-backend" + suffix, "lh-benchmark-retire-instances" + suffix}
}

func validateBenchmarkFenceReceipts(record benchmarkRecord) error {
	if record.Version == 3 && record.Phase == "v1" && record.Cleaned && len(record.Fences) != len(benchmarkFenceNames(record.Phase, record.Run)) {
		return errors.New("a completed V1 benchmark needs all original retirement fence receipts")
	}
	if record.FencePending == "" && record.FenceCheckpoint == "" && len(record.Fences) == 0 {
		return nil
	}
	original := record
	original.Cleaned = false
	if record.Phase != "v1" || !validBenchmarkOwner(record.Run, record.Source) || verifyRetirementReceipts(original) != nil {
		return errors.New("benchmark fence journal has no verified original scratch")
	}
	names := benchmarkFenceNames(record.Phase, record.Run)
	if record.FenceCheckpoint == "" && (record.Fences[names[2]].PolicyUID != "" || strings.HasPrefix(record.FencePending, names[2]+"/")) ||
		record.FenceCheckpoint != "" && (!benchmarkCheckpointPattern.MatchString(record.FenceCheckpoint) || !strings.HasPrefix(record.FenceCheckpoint, record.Run+"-")) {
		return errors.New("benchmark instance checkpoint is invalid")
	}
	seen := map[string]bool{}
	for _, resource := range record.Resources {
		seen[resource.UID] = true
	}
	for name, receipt := range record.Fences {
		if !slices.Contains(names, name) || receipt.PolicyUID == "" || receipt.PolicyUID == receipt.BindingUID ||
			seen[receipt.PolicyUID] || seen[receipt.BindingUID] || record.Cleaned && receipt.BindingUID == "" {
			return errors.New("benchmark fence UID receipt is invalid")
		}
		seen[receipt.PolicyUID] = true
		if receipt.BindingUID != "" {
			seen[receipt.BindingUID] = true
		}
	}
	if record.FencePending == "" {
		return nil
	}
	name, kind, ok := strings.Cut(record.FencePending, "/")
	receipt := record.Fences[name]
	expectedKind := "policy"
	if receipt.PolicyUID != "" {
		expectedKind = "binding"
	}
	if record.Cleaned || !ok || !slices.Contains(names, name) || receipt.BindingUID != "" || kind != expectedKind {
		return errors.New("benchmark fence creation intent is invalid")
	}
	return nil
}

func benchmarkDenyBinding(name, policyUID string) object {
	return object{
		"apiVersion": benchmarkAdmissionAPI, "kind": "ValidatingAdmissionPolicyBinding",
		"metadata": object{
			"name": name,
			"ownerReferences": []any{object{
				"apiVersion": benchmarkAdmissionAPI, "kind": "ValidatingAdmissionPolicy", "name": name, "uid": policyUID,
			}},
		},
		"spec": object{"policyName": name, "validationActions": []any{"Deny"}},
	}
}

func matchesOwnedBenchmarkPolicy(policy, desired object, uid string) bool {
	return uid != "" && text(policy, "metadata", "uid") == uid &&
		text(policy, "metadata", "resourceVersion") != "" && value(policy, "metadata", "deletionTimestamp") == nil &&
		text(policy, "apiVersion") == benchmarkAdmissionAPI && text(policy, "kind") == "ValidatingAdmissionPolicy" &&
		text(policy, "metadata", "name") == text(desired, "metadata", "name") &&
		reflect.DeepEqual(value(policy, "spec"), value(desired, "spec"))
}

func matchesOwnedBenchmarkFence(policy, binding, desired object, receipt benchmarkFenceReceipt) bool {
	expected := benchmarkDenyBinding(text(desired, "metadata", "name"), receipt.PolicyUID)
	return matchesOwnedBenchmarkPolicy(policy, desired, receipt.PolicyUID) &&
		receipt.BindingUID != "" && receipt.BindingUID != receipt.PolicyUID && text(binding, "metadata", "uid") == receipt.BindingUID &&
		text(binding, "metadata", "resourceVersion") != "" && value(binding, "metadata", "deletionTimestamp") == nil &&
		text(binding, "apiVersion") == benchmarkAdmissionAPI && text(binding, "kind") == "ValidatingAdmissionPolicyBinding" &&
		text(binding, "metadata", "name") == text(expected, "metadata", "name") &&
		reflect.DeepEqual(value(binding, "metadata", "ownerReferences"), value(expected, "metadata", "ownerReferences")) &&
		reflect.DeepEqual(value(binding, "spec"), value(expected, "spec"))
}

// A failed or ambiguous CREATE leaves durable intent. A new object with the
// right name or body is never adopted without its original API UID receipt.
func installBenchmarkReferenceFence(ctx context.Context, c client, record *benchmarkRecord, desired object) error {
	policies, err := benchmarkRetirementFences(*record)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(desired, policies[0]) && !reflect.DeepEqual(desired, policies[1]) {
		return errors.New("benchmark fence is outside the reviewed reference scope")
	}
	return installBenchmarkFence(ctx, c, record, desired)
}

func activeBenchmarkV1FenceOwner(c client, record benchmarkRecord) bool {
	return c.context == "admin@prod" && authorizeBenchmark(os.Getenv, "v1") == nil &&
		record.Phase == "v1" && record.Source == os.Getenv("GITHUB_SHA") && record.Run == os.Getenv("GITHUB_RUN_ID") &&
		record.Recovery == nil && !record.Cleaned && record.FencePending == ""
}

func installBenchmarkFence(ctx context.Context, c client, record *benchmarkRecord, desired object) error {
	name := text(desired, "metadata", "name")
	if !activeBenchmarkV1FenceOwner(c, *record) || !slices.Contains(benchmarkFenceNames(record.Phase, record.Run), name) {
		return errors.New("benchmark fence installation has no verified workflow owner or target")
	}
	receipt := record.Fences[name]
	if receipt.BindingUID != "" {
		_, err := observeBenchmarkReferenceFence(ctx, c, desired, receipt)
		return err
	}
	if receipt.PolicyUID == "" {
		next := *record
		next.FencePending = name + "/policy"
		if err := advanceBenchmarkFenceJournal(ctx, c, record, next); err != nil {
			return err
		}
		policy, err := c.create(ctx, record.Source, desired)
		if err != nil {
			return err
		}
		receipt.PolicyUID = text(policy, "metadata", "uid")
		if !matchesOwnedBenchmarkPolicy(policy, desired, receipt.PolicyUID) {
			return errors.New("benchmark policy creation readback differs")
		}
		if err := saveBenchmarkFenceReceipt(ctx, c, record, name, receipt); err != nil {
			return err
		}
	}
	policy, err := c.get(ctx, "validatingadmissionpolicies.admissionregistration.k8s.io", "", name, false)
	if err != nil || !matchesOwnedBenchmarkPolicy(policy, desired, receipt.PolicyUID) {
		return errors.New("original benchmark fence policy is unverified")
	}
	next := *record
	next.FencePending = name + "/binding"
	if err := advanceBenchmarkFenceJournal(ctx, c, record, next); err != nil {
		return err
	}
	binding, err := c.create(ctx, record.Source, benchmarkDenyBinding(name, receipt.PolicyUID))
	if err != nil {
		return err
	}
	receipt.BindingUID = text(binding, "metadata", "uid")
	if !matchesOwnedBenchmarkFence(policy, binding, desired, receipt) {
		return errors.New("benchmark binding creation readback differs")
	}
	if err := saveBenchmarkFenceReceipt(ctx, c, record, name, receipt); err != nil {
		return err
	}
	_, err = observeBenchmarkReferenceFence(ctx, c, desired, receipt)
	return err
}

func saveBenchmarkFenceReceipt(ctx context.Context, c client, record *benchmarkRecord, name string, receipt benchmarkFenceReceipt) error {
	next := *record
	next.Fences = maps.Clone(record.Fences)
	if next.Fences == nil {
		next.Fences = map[string]benchmarkFenceReceipt{}
	}
	next.Fences[name], next.FencePending = receipt, ""
	return advanceBenchmarkFenceJournal(ctx, c, record, next)
}

func advanceBenchmarkFenceJournal(ctx context.Context, c client, record *benchmarkRecord, next benchmarkRecord) error {
	cm, actual, err := readBenchmark(ctx, c, record.Phase)
	if err != nil || cm == nil || !reflect.DeepEqual(actual, *record) {
		return errors.New("benchmark journal changed before recording its fence")
	}
	if err := validateBenchmarkFenceReceipts(next); err != nil {
		return err
	}
	if err := writeBenchmarkProgress(ctx, c, cm, record.Source, next); err != nil {
		return err
	}
	checked, actual, err := readBenchmark(ctx, c, record.Phase)
	if err != nil || text(checked, "metadata", "uid") != text(cm, "metadata", "uid") || !reflect.DeepEqual(actual, next) {
		return errors.New("benchmark fence progress readback changed")
	}
	*record = next
	return nil
}

func removeCompletedBenchmarkFences(ctx context.Context, c client, record benchmarkRecord) error {
	if !activeBenchmarkV1FenceOwner(c, record) || len(record.Fences) != len(benchmarkFenceNames(record.Phase, record.Run)) {
		return errors.New("benchmark fence cleanup has no complete original receipts")
	}
	groups, _, err := readBenchmarkInstanceGroups(ctx, c)
	if err != nil {
		return err
	}
	children, err := qualifyRetirementInstances(record, groups)
	if err != nil || len(children) != 0 {
		return errors.New("native V1 benchmark children still depend on their retirement fence")
	}
	const bindingResource = "validatingadmissionpolicybindings.admissionregistration.k8s.io"
	const policyResource = "validatingadmissionpolicies.admissionregistration.k8s.io"
	bindings, err := c.list(ctx, bindingResource, "")
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		name := text(binding, "spec", "policyName")
		if receipt, managed := record.Fences[name]; managed && text(binding, "metadata", "uid") != receipt.BindingUID {
			return errors.New("a foreign binding references an original benchmark policy")
		}
	}
	for _, name := range benchmarkFenceNames(record.Phase, record.Run) {
		receipt := record.Fences[name]
		binding, err := c.get(ctx, bindingResource, "", name, false)
		if err != nil || text(binding, "metadata", "uid") != receipt.BindingUID || receipt.BindingUID == "" ||
			text(binding, "spec", "policyName") != name {
			return errors.New("the original benchmark policy binding changed before retirement")
		}
		policy, err := c.get(ctx, policyResource, "", name, false)
		if err != nil || text(policy, "metadata", "uid") != receipt.PolicyUID || receipt.PolicyUID == "" {
			return errors.New("the original benchmark policy changed before retirement")
		}
		base := "/apis/" + benchmarkAdmissionAPI + "/"
		if err := removeScratch(ctx, c, record.Source, bindingResource, "", base+"validatingadmissionpolicybindings/"+name, binding); err != nil {
			return err
		}
		if err := removeScratch(ctx, c, record.Source, policyResource, "", base+"validatingadmissionpolicies/"+name, policy); err != nil {
			return err
		}
	}
	return nil
}
