package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestUmamiFenceResumesExactBindingAfterAmbiguousCreation(t *testing.T) {
	const (
		source     = "1111111111111111111111111111111111111111"
		recovery   = "2222222222222222222222222222222222222222"
		journalUID = "ac005841-d001-4001-8001-000000000001"
		policyUID  = "ac005841-d002-4002-8002-000000000002"
		bindingUID = "ac005841-d003-4003-8003-000000000003"
	)
	owner := umamiReplacementOwner{Run: "456", Source: recovery}
	original := umamiStandbyReplacementRecord{
		Version: 1, Run: "123", Source: source, Phase: umamiNameFencePending,
		Original: cnpgStandbyCutover{
			Database: benchmarkResource{Name: "umami-db", Namespace: "umami"},
			Consumer: benchmarkResource{Name: "umami-db-1", Namespace: "umami"},
			Claim:    benchmarkResource{Name: "umami-db-1", Namespace: "umami", UID: freshTestUID},
		},
		NameRelease: &umamiStandbyNameRelease{Owner: owner, Join: benchmarkResource{"umami-db-1-join", "umami", freshTestUID}},
	}
	recordJSON, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	cm := object{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{"name": umamiStandbyJournalName(original),
			"namespace": umamiStandbyJournalNamespace, "uid": journalUID, "resourceVersion": "10",
			"labels": object{"app.kubernetes.io/managed-by": "github-actions", "storage.platform.devantler.tech/recovery-run": original.Run}},
		"data": object{"record": string(recordJSON)},
	}
	var policy, binding object
	policiesCreated, bindingsCreated := 0, 0
	c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if program == "git" {
			return []byte(recovery + "\trefs/heads/main\n"), nil
		}
		if program != "kubectl" || len(args) < 4 {
			return nil, errors.New("unexpected command")
		}
		switch args[2] {
		case "get":
			switch args[3] {
			case "configmap":
				return json.Marshal(cm)
			case umamiNamePolicyKind:
				if policy != nil {
					return json.Marshal(policy)
				}
			case umamiNameBindingKind:
				if binding != nil {
					return json.Marshal(binding)
				}
			}
			return nil, nil
		case "create":
			var created object
			if err := json.Unmarshal(input, &created); err != nil {
				return nil, err
			}
			valueObject(created, "metadata")["resourceVersion"] = "500"
			switch text(created, "kind") {
			case "ValidatingAdmissionPolicy":
				policiesCreated++
				valueObject(created, "metadata")["uid"] = policyUID
				policy = created
				return json.Marshal(policy)
			case "ValidatingAdmissionPolicyBinding":
				bindingsCreated++
				valueObject(created, "metadata")["uid"] = bindingUID
				binding = created
				return nil, errors.New("the accepted binding response was interrupted")
			}
		case "patch":
			var changes []object
			if err := json.Unmarshal(input, &changes); err != nil || len(changes) != 3 ||
				text(changes[2], "path") != "/data/record" {
				return nil, errors.New("unexpected journal write")
			}
			valueObject(cm, "data")["record"] = text(changes[2], "value")
			return nil, nil
		}
		return nil, errors.New("unexpected mutation")
	}}
	j := umamiStandbyReplacementJournal{c: c, uid: journalUID, record: original, resume: &owner}
	ctx := t.Context()
	if err := recordUmamiNameFence(ctx, &j); err == nil || j.record.Phase != umamiNameFencePending ||
		j.record.NameRelease.Fence != (benchmarkFenceReceipt{PolicyUID: policyUID}) {
		t.Fatal("the original policy UID was not durably recorded before the binding request")
	}
	annotations := valueObject(binding, "metadata", "annotations")
	bindingOwner := annotations[umamiNameFenceOwner]
	annotations[umamiNameFenceOwner] = "foreign"
	if err := recordUmamiNameFence(ctx, &j); err == nil {
		t.Fatal("an unowned binding was adopted")
	}
	annotations[umamiNameFenceOwner] = bindingOwner
	if err := recordUmamiNameFence(ctx, &j); err != nil || j.record.Phase != umamiNameFenced ||
		j.record.NameRelease.Fence != (benchmarkFenceReceipt{PolicyUID: policyUID, BindingUID: bindingUID}) ||
		policiesCreated != 1 || bindingsCreated != 1 {
		t.Fatalf("verified continuation did not bind the accepted resources without creating duplicates: %v", err)
	}
}
