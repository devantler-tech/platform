package main

import "testing"

func journalFixture() journal {
	return journal{
		Schema: 1, Target: "prod-worker-1", Source: "1234567890123456789012345678901234567890", OwnerRun: "1234", Phase: "evacuating",
		NodeUID: "node-uid", ProviderID: "hcloud://42", MachineUUID: "machine-uuid", LonghornUID: "longhorn-uid", EphemeralUUID: "ephemeral-uuid",
		OriginalDisks: object{"data": object{"path": "/var/lib/longhorn", "diskType": "filesystem", "allowScheduling": true, "evictionRequested": false}},
	}
}

func TestJournalMustBindTheExactResetScope(t *testing.T) {
	if err := validateJournal(journalFixture(), "prod-worker-1"); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*journal){
		"unknown schema":            func(j *journal) { j.Schema = 2 },
		"different worker":          func(j *journal) { j.Target = "prod-worker-2" },
		"missing source":            func(j *journal) { j.Source = "" },
		"missing owner":             func(j *journal) { j.OwnerRun = "" },
		"missing node identity":     func(j *journal) { j.NodeUID = "" },
		"missing provider identity": func(j *journal) { j.ProviderID = "" },
		"missing machine identity":  func(j *journal) { j.MachineUUID = "" },
		"missing old partition":     func(j *journal) { j.EphemeralUUID = "" },
		"unknown phase":             func(j *journal) { j.Phase = "retry-anything" },
		"multiple old disks":        func(j *journal) { j.OriginalDisks["extra"] = object{"path": "/other"} },
		"unrecognized old disk":     func(j *journal) { j.OriginalDisks["data"].(object)["path"] = "/" },
	} {
		t.Run(name, func(t *testing.T) {
			j := journalFixture()
			mutate(&j)
			if err := validateJournal(j, "prod-worker-1"); err == nil {
				t.Fatal("unbound recovery scope accepted")
			}
		})
	}
}

func TestReviewedWorkflowPathOnlyNamesTheProcedureOnMain(t *testing.T) {
	const workflow = ".github/workflows/prepare-longhorn-v2-node.yaml"
	for _, path := range []string{workflow, workflow + "@main", workflow + "@refs/heads/main"} {
		if !reviewedWorkflowPath(path) {
			t.Fatalf("%s refused", path)
		}
	}
	for _, path := range []string{"", workflow + "@feature", workflow + "@refs/heads/feature", workflow + "@refs/tags/main", ".github/workflows/other.yaml@main"} {
		if reviewedWorkflowPath(path) {
			t.Fatalf("%q accepted", path)
		}
	}
}
