package main

import "testing"

func TestObserveCorootKeeperRequiresTheFullRaftQuorum(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		leader, fail   bool
	}{
		{"leader", "zk_server_state\tleader\nzk_followers\t2\nzk_synced_followers\t2\n", true, false},
		{"follower", "zk_server_state\tfollower\nzk_znode_count\t4\n", false, false},
		{"stale follower", "zk_server_state\tleader\nzk_followers\t2\nzk_synced_followers\t1\n", false, true},
		{"ambiguous role", "zk_server_state\tfollower\nzk_server_state\tleader\nzk_followers\t2\nzk_synced_followers\t2\n", false, true},
		{"truncated", "zk_server_state\tfollower", false, true},
		{"standalone", "zk_server_state\tstandalone\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leader, err := observedKeeperLeader([]byte(tc.response))
			if leader != tc.leader || (err != nil) != tc.fail {
				t.Fatalf("leader=%t err=%v, want leader=%t error=%t", leader, err, tc.leader, tc.fail)
			}
		})
	}
}
