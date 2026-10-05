package main

import (
	"strings"
	"testing"
)

func TestCapacityAccountsForNeighborsAndInitReservations(t *testing.T) {
	node := `{"status":{"allocatable":{"cpu":"7.8","memory":"30Gi","ephemeral-storage":"100Gi"}}}`
	reservations := "P\towned\tRunning\nR\t3\t12Gi\t32Gi\nP\tsystem\tRunning\nR\t200m\t1Gi\t1Gi\nR\t100m\t256Mi\t1Gi\n"
	if err := verifyBudget([]byte(node), []byte(reservations), "owned"); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ name, node, records string }{
		{"memory", strings.ReplaceAll(node, "30Gi", "14Gi"), reservations},
		{"cpu", strings.ReplaceAll(node, "7.8", "3.5"), reservations},
		{"storage", strings.ReplaceAll(node, "100Gi", "48Gi"), reservations},
		{"unknown-quantity", node, strings.ReplaceAll(reservations, "200m", "not-known")},
		{"missing-read", node, ""},
		{"truncated-record", node, "P\tsystem\tRunning\nR\t200m\t1Gi"},
	} {
		t.Run(item.name, func(t *testing.T) {
			if verifyBudget([]byte(item.node), []byte(item.records), "owned") == nil {
				t.Fatal("accepted insufficient or unknown headroom")
			}
		})
	}
}

func TestQuotaRequiresCompleteHeadroom(t *testing.T) {
	good := `{"items":[{"spec":{"hard":{"pods":"3","requests.memory":"16Gi","limits.memory":"16Gi"}},"status":{"hard":{"pods":"3","requests.memory":"16Gi","limits.memory":"16Gi"},"used":{"pods":"1","requests.memory":"1Gi","limits.memory":"1Gi"}}}]}`
	if err := verifyQuota([]byte(good)); err != nil {
		t.Fatal(err)
	}
	if err := verifyQuota([]byte(`{"items":[]}`)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.ReplaceAll(good, `"pods":"3"`, `"pods":"1"`),
		strings.ReplaceAll(good, `"limits.memory":"16Gi"`, `"limits.memory":"14Gi"`),
		strings.ReplaceAll(good, `"requests.memory":"1Gi",`, ``),
		`{"items":[{"spec":{"hard":{"pods":"1"}},"status":{}}]}`,
		`{"items":[{"spec":{"hard":{"pods":"1"}},"status":{"hard":{"pods":"5"},"used":{"pods":"1"}}}]}`,
		`{"items":[{"spec":{"hard":{"pods":"1"},"scopes":["Terminating"]},"status":{"hard":{"pods":"1"},"used":{"pods":"0"}}}]}`,
		`{}`, `{"items":[{"status":{"hard":{"pods":"unknown"},"used":{"pods":"0"}}}]}`,
	} {
		if verifyQuota([]byte(bad)) == nil {
			t.Fatal("accepted incomplete or insufficient quota")
		}
	}
}
