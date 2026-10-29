package main

import "testing"

func TestRebuildRequiresMeasuredCapacityAndPreservesTheLastV1Worker(t *testing.T) {
	if err := verifyRebuildBudget("prod-worker-1", evacuatedFixture()); err != nil {
		t.Fatal(err)
	}
	if verifyRebuildBudget("prod-worker-3", evacuatedFixture()) == nil {
		t.Fatal("last V1 worker could be fenced before migration")
	}
	for name, change := range map[string]func(*inventory){
		"unknown size":           func(s *inventory) { delete(s.volumes[0]["spec"].(object), "size") },
		"logical overcommit":     func(s *inventory) { s.volumes[0]["spec"].(object)["size"] = "96636764160" },
		"actual data overcommit": func(s *inventory) { s.volumes[0]["status"].(object)["actualSize"] = "85899345920" },
		"missing measurement":    func(s *inventory) { delete(s.volumes[0]["status"].(object), "actualSize") },
	} {
		t.Run(name, func(t *testing.T) {
			s := evacuatedFixture()
			change(&s)
			if verifyRebuildBudget("prod-worker-1", s) == nil {
				t.Fatal("unverified raw-disk capacity accepted")
			}
		})
	}
	s := evacuatedFixture()
	for _, v := range s.volumes {
		v["spec"].(object)["dataEngine"] = "v2"
	}
	if err := verifyRebuildBudget("prod-worker-3", s); err != nil {
		t.Fatal(err)
	}
}
