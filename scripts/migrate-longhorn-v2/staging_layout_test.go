package main

import (
	"strconv"
	"testing"
)

func TestStagingBudgetPreservesFullCopiesAndWorkingSpace(t *testing.T) {
	s := inventory{}
	// The current 20 claims total 117 GiB. Use 40 GiB actual to round up.
	for _, size := range []uint64{2, 2, 2, 2, 2, 2, 5, 5, 5, 5, 5, 5, 5, 10, 10, 10, 10, 10, 10, 10} {
		s.volumes = append(s.volumes, object{
			"spec":   object{"size": strconv.FormatUint(size<<30, 10), "dataEngine": "v1"},
			"status": object{"actualSize": float64(2 << 30)},
		})
	}
	if err := verifyRebuildBudget("prod-worker-1", s); err == nil {
		t.Fatal("117 GiB fit on the original permanent disk")
	}
	if err := verifyStagingBudget(s); err != nil {
		t.Fatalf("larger staging disk rejected the measured migration footprint: %v", err)
	}
	// Adding 18 GiB to an existing 2 GiB volume crosses the 134 GiB limit.
	s.volumes[0]["spec"].(object)["size"] = strconv.FormatUint(20<<30, 10)
	if err := verifyStagingBudget(s); err == nil {
		t.Fatal("staging budget spent reserved logical headroom")
	}
	s.volumes[0]["spec"].(object)["size"] = strconv.FormatUint(2<<30, 10)
	s.volumes[0]["status"].(object)["actualSize"] = float64(61 << 30)
	if err := verifyStagingBudget(s); err == nil {
		t.Fatal("staging budget spent the physical free-space floor")
	}
}

func TestStagingLayoutRequiresTheLargerEncryptedPartition(t *testing.T) {
	raw, ephemeral := desiredConfigFixture()
	if err := verifyStagingLayout(raw, ephemeral); err == nil {
		t.Fatal("original layout accepted as larger staging capacity")
	}
	rawPartition := valueObject(raw, "spec", "provisioning", "partitionSpec")
	rawPartition["minSize"], rawPartition["maxSize"] = float64(144<<30), float64(144<<30)
	valueObject(ephemeral, "spec", "provisioning", "partitionSpec")["maxSize"] = float64(152 << 30)
	if err := verifyStagingLayout(raw, ephemeral); err != nil {
		t.Fatal(err)
	}
	if err := verifyDesiredLayout(raw, ephemeral); err == nil {
		t.Fatal("staging layout accepted for a permanent worker")
	}
}
