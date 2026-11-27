package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestUmamiReplacementJournalCannotSkipOrRebindProgress(t *testing.T) {
	old := umamiStandbyReplacementRecord{
		Version: 1, Source: "23494e7388b3b1a7fc27da52f9ef5799cfb6ef10", Run: "45942954918",
		Phase: umamiReplacementClaimRetained, Primary: benchmarkResource{Name: "umami-db-1", Namespace: "umami"},
	}
	for phase, valid := range map[string]bool{
		umamiReplacementPlanned: false, umamiReplacementClaimRetained: false,
		umamiReplacementEvictionPending: true, umamiReplacementStandbyRebuilt: false,
	} {
		next := old
		next.Phase = phase
		if err := verifyUmamiReplacementTransition(old, next); (err == nil) != valid {
			t.Fatalf("unexpected phase result for %s: %v", phase, err)
		}
	}
	next := old
	next.Phase = umamiReplacementEvictionPending
	next.Primary.Name = "umami-db-3"
	if verifyUmamiReplacementTransition(old, next) == nil {
		t.Fatal("the journal accepted a different writer")
	}
	next = old
	next.Phase = umamiReplacementEvictionPending
	next.Original.Claim.Name = "foreign"
	if verifyUmamiReplacementTransition(old, next) == nil {
		t.Fatal("the journal accepted a replaced rollback claim")
	}
}

func TestUmamiReplacementJournalHoldsAnUncertainPatch(t *testing.T) {
	observation, pod, claim, pv, volume, pdb := umamiStandbyCutoverFixture(t)
	witness, err := bindUmamiStandbyCutover(observation, observation.Pods, text(pod, "metadata", "name"), claim, pv, volume, pdb)
	if err != nil {
		t.Fatal(err)
	}
	witness.Database.UID = "ac005841-9999-4999-8999-999999999999"
	witness.Spare.Cluster = witness.Database
	witness.Spare.Pod = benchmarkResource{"umami-db-4", "umami", "ac005841-8444-4444-8444-444444444444"}
	record := umamiStandbyReplacementRecord{
		Version: 1, Source: "23494e7388b3b1a7fc27da52f9ef5799cfb6ef10", Run: "45942954918",
		Phase: umamiReplacementPlanned, Primary: benchmarkResource{"umami-db-1", "umami", "ac005841-6666-4666-8666-666666666666"},
		Original: witness,
	}
	record.OtherPods = map[string]benchmarkResource{
		record.Primary.Name:    record.Primary,
		"umami-db-3":           {"umami-db-3", "umami", "ac005841-5555-4555-8555-666666666666"},
		witness.Spare.Pod.Name: witness.Spare.Pod,
	}
	var cm object
	patches := 0
	hideReadback := false
	ctx := context.Background()
	c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if program == "git" {
			return []byte(record.Source + "\trefs/heads/main\n"), nil
		}
		if program != "kubectl" || len(args) < 3 {
			return nil, errors.New("unexpected tool")
		}
		switch args[2] {
		case "get":
			if hideReadback {
				return nil, errors.New("uncertain readback")
			}
			if cm == nil {
				return nil, nil
			}
			return json.Marshal(cm)
		case "create":
			if cm != nil || json.Unmarshal(input, &cm) != nil {
				return nil, errors.New("unexpected create")
			}
			meta := valueObject(cm, "metadata")
			meta["uid"] = "ac005841-7777-4777-8777-777777777777"
			meta["resourceVersion"] = "11"
			return json.Marshal(cm)
		case "patch":
			patches++
			var changes []object
			if json.Unmarshal(input, &changes) != nil || len(changes) != 3 ||
				text(changes[0], "value") != text(cm, "metadata", "uid") ||
				text(changes[1], "value") != text(cm, "metadata", "resourceVersion") ||
				text(changes[2], "path") != "/data/record" {
				return nil, errors.New("unexpected patch")
			}
			valueObject(cm, "data")["record"] = text(changes[2], "value")
			valueObject(cm, "metadata")["resourceVersion"] = "12"
			hideReadback = true
			return nil, nil
		default:
			return nil, errors.New("unexpected mutation")
		}
	}}
	journal, err := beginUmamiStandbyReplacement(ctx, c, record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = beginUmamiStandbyReplacement(ctx, c, record); err == nil {
		t.Fatal("a replacement run took over its existing journal")
	}
	next := record
	next.Phase = umamiReplacementRetentionPending
	if err = journal.transition(ctx, next); err == nil || journal.record.Phase != record.Phase {
		t.Fatal("an uncertain write advanced the local journal")
	}
	hideReadback = false
	if err = journal.transition(ctx, next); err == nil || patches != 1 {
		t.Fatal("the journal replayed an uncertain write")
	}
}
