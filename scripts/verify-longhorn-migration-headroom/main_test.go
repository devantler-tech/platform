package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// These fixtures model the six static nodes plus the two existing V1 pool
// members. Accepting a leftover server, substituted provider ID or ordinary
// overflow node would falsely authorize spending the reserved builder slot.
func roster(t *testing.T, v2 bool) ([]server, []node) {
	t.Helper()
	names := []string{"prod-control-plane-2", "prod-control-plane-4", "prod-control-plane-5", "prod-worker-1", "prod-worker-2", "prod-worker-3", "autoscale-longhorn-v1-0123456789abcdef", "autoscale-longhorn-v1-fedcba9876543210"}
	if v2 {
		names = append(names, "autoscale-longhorn-v2-1111111111111111")
	}
	var servers []server
	var nodes []node
	for i, name := range names {
		id := int64(100 + i)
		labels := map[string]string{"ksail.owned": "true", "ksail.cluster.name": "prod", "ksail.node.type": "worker"}
		if i < 3 {
			labels["ksail.node.type"] = "controlplane"
		}
		if i < 6 {
			labels["ksail.node.index"] = name[strings.LastIndex(name, "-")+1:]
		}
		role := ""
		if i >= 6 {
			role = "v1"
			if i == 8 {
				role = "v2"
			}
			labels = map[string]string{"hcloud/node-group": "autoscale-longhorn-" + role}
		}
		s := server{ID: id, Name: name, Status: "running", Labels: labels}
		if err := json.Unmarshal(fmt.Appendf(nil, `[{"network":4242,"ip":"10.0.0.%d"}]`, i+1), &s.PrivateNet); err != nil {
			t.Fatal(err)
		}
		s.ServerType.Name = "cx43"
		if i < 3 {
			s.ServerType.Name = "cx33"
		} else if i >= 6 {
			s.ServerType.Name = "cpx42"
		}
		s.Location.Name = "fsn1"
		servers = append(servers, s)
		payload := fmt.Sprintf(`{"metadata":{"name":%q,"uid":"node-%d","labels":{"storage.platform.devantler.tech/migration-pool":%q}},"spec":{"providerID":"hcloud://%d"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`, name, i, role, id)
		var n node
		if err := json.Unmarshal([]byte(payload), &n); err != nil {
			t.Fatal(err)
		}
		n.Metadata.Labels["node.kubernetes.io/instance-type"] = s.ServerType.Name
		n.Metadata.Labels["topology.kubernetes.io/region"] = "fsn1"
		if err := json.Unmarshal(fmt.Appendf(nil, `[{"type":"InternalIP","address":"10.0.0.%d"}]`, i+1), &n.Status.Addresses); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, n)
	}
	return servers, nodes
}

func TestQualifiedActivationRoster(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		s, n := roster(t, v2)
		if err := qualify(s, n, false); err != nil {
			t.Fatalf("valid roster v2=%v rejected: %v", v2, err)
		}
	}
	s, n := roster(t, true)
	if err := qualify(s, n, true); err != nil {
		t.Fatalf("completed activation rejected: %v", err)
	}
}

// TestUnsafeHeadroomIsHeld refuses incomplete or contradictory rosters without
// treating an unmatched provider member as free capacity.
func TestUnsafeHeadroomIsHeld(t *testing.T) {
	cases := map[string]func(*[]server, *[]node){
		"leftover provider server": func(s *[]server, _ *[]node) {
			*s = append(*s, server{ID: 999, Name: "old-overflow", Status: "deleting"})
		},
		"unjoined node":              func(_ *[]server, n *[]node) { (*n)[0].Spec.ProviderID = "hcloud://999" },
		"missing provider ID":        func(_ *[]server, n *[]node) { (*n)[0].Spec.ProviderID = "" },
		"wrong provider scheme":      func(_ *[]server, n *[]node) { (*n)[0].Spec.ProviderID = "other://100" },
		"missing provider network":   func(s *[]server, _ *[]node) { (*s)[0].PrivateNet = nil },
		"foreign autoscaler network": func(s *[]server, _ *[]node) { (*s)[6].PrivateNet[0].Network = 999 },
		"wrong static index":         func(s *[]server, _ *[]node) { (*s)[0].Labels["ksail.node.index"] = "1" },
		"private address mismatch":   func(s *[]server, _ *[]node) { (*s)[6].PrivateNet[0].IP = "10.0.0.99" },
		"missing private address":    func(_ *[]server, n *[]node) { (*n)[0].Status.Addresses = nil },
		"instance type mismatch":     func(s *[]server, _ *[]node) { (*s)[6].ServerType.Name = "cx43" },
		"wrong migration hardware": func(s *[]server, n *[]node) {
			(*s)[6].ServerType.Name = "cx43"
			(*n)[6].Metadata.Labels["node.kubernetes.io/instance-type"] = "cx43"
		},
		"migration location mismatch": func(s *[]server, _ *[]node) { (*s)[6].Location.Name = "hel1" },
		"contradictory ownership":     func(s *[]server, _ *[]node) { (*s)[6].Labels["ksail.cluster.name"] = "foreign" },
		"duplicate provider ID":       func(s *[]server, _ *[]node) { (*s)[1].ID = (*s)[0].ID },
		"duplicate node UID":          func(_ *[]server, n *[]node) { (*n)[1].Metadata.UID = (*n)[0].Metadata.UID },
		"duplicate node name":         func(_ *[]server, n *[]node) { (*n)[1].Metadata.Name = (*n)[0].Metadata.Name },
		"provider name mismatch":      func(s *[]server, _ *[]node) { (*s)[0].Name = "different" },
		"transitional provider":       func(s *[]server, _ *[]node) { (*s)[0].Status = "starting" },
		"foreign static owner":        func(s *[]server, _ *[]node) { (*s)[0].Labels["ksail.cluster.name"] = "foreign" },
		"missing static owner":        func(s *[]server, _ *[]node) { delete((*s)[0].Labels, "ksail.owned") },
		"wrong pool":                  func(s *[]server, _ *[]node) { (*s)[6].Labels["hcloud/node-group"] = "autoscale-cx43" },
		"pool role mismatch": func(_ *[]server, n *[]node) {
			(*n)[6].Metadata.Labels["storage.platform.devantler.tech/migration-pool"] = "v2"
		},
		"ordinary overflow": func(s *[]server, n *[]node) {
			(*s)[6].Name = "autoscale-cx43-0123456789abcdef"
			(*n)[6].Metadata.Name = (*s)[6].Name
			(*s)[6].Labels["hcloud/node-group"] = "autoscale-cx43"
		},
		"unready node":      func(_ *[]server, n *[]node) { (*n)[0].Status.Conditions[0].Status = "False" },
		"cordoned node":     func(_ *[]server, n *[]node) { (*n)[0].Spec.Unschedulable = true },
		"terminating node":  func(_ *[]server, n *[]node) { ts := "2026-11-02T08:00:00Z"; (*n)[0].Metadata.DeletionTimestamp = &ts },
		"missing V1 member": func(s *[]server, n *[]node) { *s = (*s)[:7]; *n = (*n)[:7] },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s, n := roster(t, false)
			change(&s, &n)
			if err := qualify(s, n, false); err == nil {
				t.Fatal("unsafe inventory accepted")
			}
		})
	}
	s, n := roster(t, false)
	if err := qualify(s, n, true); err == nil {
		t.Fatal("post-update accepted absent V2 worker")
	}
}
