package main

import (
	"strings"
	"testing"
)

func TestCorootKeeperLeadershipRequiresTwoV2MembersAndTheRetainedProviderDigest(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, fault := range []string{"", "target on provider", "other member still on provider", "shared V2 claim", "stale provider", "dual leaders", "no leader"} {
		t.Run(fault, func(t *testing.T) {
			before := corootKeeperQuorumReceipt{
				Members: [3]corootKeeperMember{{Pod: "keeper-0", UID: "a", Leader: true}, {Pod: "keeper-1", UID: "b"}, {Pod: "keeper-2", UID: "c"}},
				Disks:   [3]corootKeeperProviderDisk{{Digest: digest}},
				V2Claims: [3]benchmarkResource{
					{}, {Name: "data-keeper-1", UID: "d"}, {Name: "data-keeper-2", UID: "e"},
				},
			}
			input := corootKeeperCutoverInput{Ordinal: 1, Digest: digest}
			switch fault {
			case "target on provider":
				input.Ordinal = 0
			case "other member still on provider":
				before.V2Claims[2] = benchmarkResource{}
				before.Disks[2].Digest = strings.Repeat("b", 64)
			case "shared V2 claim":
				before.V2Claims[2] = before.V2Claims[1]
			case "stale provider":
				input.Digest = strings.Repeat("b", 64)
			case "dual leaders":
				before.Members[2].Leader = true
			case "no leader":
				before.Members[0].Leader = false
			}
			leader, remaining, err := bindCorootKeeperLeadership(before, input)
			if (err == nil) != (fault == "") {
				t.Fatalf("unexpected handoff admission: %v", err)
			}
			if err == nil && (leader != 0 || remaining != 0) {
				t.Fatal("the only provider member was not identified")
			}
			if err != nil {
				return
			}
			after := before
			after.Members[0].Leader, after.Members[1].Leader = false, true
			if !unchangedKeeperMembership(before, after) {
				t.Fatal("a role change incorrectly counted as a member replacement")
			}
			after.Members[1].UID = "replacement"
			if unchangedKeeperMembership(before, after) {
				t.Fatal("a new member was accepted during the transfer")
			}
		})
	}
}
