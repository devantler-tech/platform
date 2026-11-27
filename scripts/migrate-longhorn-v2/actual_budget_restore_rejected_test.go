package main

import (
	"strings"
	"testing"
)

const rejectedRestoreTestUID = "3ad77aa9-0fd6-4343-a799-75e7f0117380"

func TestActualBudgetRetryRequiresExactRejectedRestore(t *testing.T) {
	for _, problem := range []string{"", "different UID", "completed", "already attempted", "other reason", "destination changed"} {
		t.Run(problem, func(t *testing.T) {
			assets, err := actualBudgetRestoreAssets("102", "101", "restore-worker")
			if err != nil {
				t.Fatal(err)
			}
			desired := assets[len(assets)-1]
			actual := cloneActualBudgetPreparation(t, desired)
			valueObject(actual, "metadata")["uid"] = rejectedRestoreTestUID
			valueObject(actual, "metadata")["resourceVersion"] = "123"
			const reason = "Error in parsing resource modifiers provided in configmap velero/actual-budget-recovery-102-modifiers: failed to decode yaml data into resource modifiers, err: error unmarshaling JSON: while decoding JSON: json: unknown field \"op\""
			status := object{"phase": "FailedValidation", "validationErrors": []any{reason}}
			actual["status"] = status
			switch problem {
			case "different UID":
				valueObject(actual, "metadata")["uid"] = "4bd77aa9-0fd6-4343-a799-75e7f0117380"
			case "completed":
				status["phase"] = "Completed"
			case "already attempted":
				status["restoreItemOperationsAttempted"] = float64(1)
			case "other reason":
				status["validationErrors"] = []any{"Backup missing"}
			case "destination changed":
				valueObject(actual, "spec", "namespaceMapping")[actualBudgetNamespace] = actualBudgetNamespace
			}
			if got := actualBudgetRestoreWasRejected(actual, desired, rejectedRestoreTestUID); got != (problem == "") {
				t.Fatalf("unexpected rejected-restore proof: %t", got)
			}
		})
	}
}

func TestActualBudgetRetryDispatchRequiresBothExactUIDs(t *testing.T) {
	env := dispatchFixture()
	const namespaceUID = "4bd77aa9-0fd6-4343-a799-75e7f0117380"
	env["GITHUB_RUN_ID"], env["LONGHORN_V2_NODE"] = "103", ""
	env["LONGHORN_V2_OPERATION"] = actualBudgetRestoreRetry
	base := actualBudgetRestoreRetry + ":101:" + strings.Repeat("a", 40) + ":" +
		strings.Repeat("b", 64) + ":102:" + strings.Repeat("c", 40) + ":" + namespaceUID
	for _, tail := range []string{"", ":" + namespaceUID, ":" + rejectedRestoreTestUID} {
		env["LONGHORN_V2_CONFIRM"] = base + tail
		got, err := authorizeActualBudgetRestore(func(key string) string { return env[key] })
		expected := tail == ":"+rejectedRestoreTestUID
		if (err == nil) != expected || expected && (got.prior != nil || got.rejected == nil || got.rejected.restoreUID != rejectedRestoreTestUID) {
			t.Fatalf("unexpected restore retry authorization: %v", err)
		}
	}
}

func TestActualBudgetResumeCanBindTheRetainedRejectedScope(t *testing.T) {
	env := dispatchFixture()
	const resumedNamespace = "4bd77aa9-0fd6-4343-a799-75e7f0117380"
	const rejectedNamespace = "7bd77aa9-0fd6-4343-a799-75e7f0117380"
	backupSource := strings.Repeat("a", 40)
	resumedSource := strings.Repeat("c", 40)
	rejectedSource := strings.Repeat("d", 40)
	env["GITHUB_RUN_ID"], env["LONGHORN_V2_NODE"] = "104", ""
	env["LONGHORN_V2_OPERATION"] = actualBudgetRestoreResume
	base := actualBudgetRestoreResume + ":101:" + backupSource + ":" +
		strings.Repeat("b", 64) + ":103:" + resumedSource + ":" + resumedNamespace
	for _, test := range []struct {
		name, suffix string
		valid        bool
	}{
		{name: "ordinary resume", valid: true},
		{name: "retained rejection", suffix: ":102:" + rejectedSource + ":" + rejectedNamespace + ":" + rejectedRestoreTestUID, valid: true},
		{name: "only the rejected namespace", suffix: ":102:" + rejectedSource + ":" + rejectedNamespace},
		{name: "same workflow", suffix: ":103:" + rejectedSource + ":" + rejectedNamespace + ":" + rejectedRestoreTestUID},
		{name: "same namespace", suffix: ":102:" + rejectedSource + ":" + resumedNamespace + ":" + rejectedRestoreTestUID},
		{name: "same object", suffix: ":102:" + rejectedSource + ":" + rejectedNamespace + ":" + resumedNamespace},
	} {
		t.Run(test.name, func(t *testing.T) {
			env["LONGHORN_V2_CONFIRM"] = base + test.suffix
			got, err := authorizeActualBudgetRestore(func(key string) string { return env[key] })
			if (err == nil) != test.valid {
				t.Fatalf("unexpected restore resume authorization: %v", err)
			}
			if test.valid && (got.prior == nil || got.prior.namespaceUID != resumedNamespace) {
				t.Fatal("the already-prepared restore namespace was not bound")
			}
			if test.valid && test.suffix != "" && (got.rejected == nil || got.rejected.prior.run != "102" ||
				got.rejected.prior.source != rejectedSource || got.rejected.prior.namespaceUID != rejectedNamespace ||
				got.rejected.restoreUID != rejectedRestoreTestUID) {
				t.Fatal("the rejected namespace and Restore were not bound")
			}
		})
	}
}
