// Command verify-longhorn-migration-headroom reads the Hetzner server
// inventory between two Node reads to observe temporary migration headroom.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type server struct {
	ID         int64             `json:"id"`
	Name       string            `json:"name"`
	Status     string            `json:"status"`
	Labels     map[string]string `json:"labels"`
	PrivateNet []struct {
		Network int64  `json:"network"`
		IP      string `json:"ip"`
	} `json:"private_net"`
	ServerType struct {
		Name string `json:"name"`
	} `json:"server_type"`
	Location struct {
		Name string `json:"name"`
	} `json:"location"`
}

type node struct {
	Metadata struct {
		Name              string            `json:"name"`
		UID               string            `json:"uid"`
		Labels            map[string]string `json:"labels"`
		DeletionTimestamp *string           `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		ProviderID    string `json:"providerID"`
		Unschedulable bool   `json:"unschedulable"`
	} `json:"spec"`
	Status struct {
		Addresses []struct {
			Type    string `json:"type"`
			Address string `json:"address"`
		} `json:"addresses"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

var migrationName = regexp.MustCompile(`^autoscale-longhorn-(v[12])-[0-9a-f]{16}$`)
var controlPlaneName = regexp.MustCompile(`^prod-control-plane-[1-9][0-9]*$`)

// qualify requires the complete migration roster and matching live identities,
// hardware, location, ownership and availability before reporting headroom.
func qualify(servers []server, nodes []node, requireV2 bool) error {
	if len(servers) != len(nodes) || len(nodes) < 8 || len(nodes) > 9 {
		return fmt.Errorf("inventory count gate failed: provider_servers=%d kubernetes_nodes=%d expected_equal=true permitted_nodes=8..9", len(servers), len(nodes))
	}
	byID := map[int64]server{}
	var network int64
	for _, s := range servers {
		if s.ID <= 0 || s.Name == "" || s.Status != "running" {
			return errors.New("provider inventory contains an unidentified or transitional server")
		}
		if _, exists := byID[s.ID]; exists {
			return errors.New("provider inventory contains duplicate IDs")
		}
		if len(s.PrivateNet) != 1 || s.PrivateNet[0].Network <= 0 || (network != 0 && s.PrivateNet[0].Network != network) {
			return errors.New("provider nodes do not share one production network")
		}
		network = s.PrivateNet[0].Network
		byID[s.ID] = s
	}
	static := map[string]string{}
	for i := 1; i <= 3; i++ {
		static[fmt.Sprintf("prod-worker-%d", i)] = "worker"
	}
	seenNames, seenUIDs, seenIDs := map[string]bool{}, map[string]bool{}, map[int64]bool{}
	counts := map[string]int{}
	for _, n := range nodes {
		name, uid := n.Metadata.Name, n.Metadata.UID
		if name == "" || uid == "" || seenNames[name] || seenUIDs[uid] || n.Metadata.DeletionTimestamp != nil || n.Spec.Unschedulable {
			return errors.New("node inventory contains an unidentified, duplicate or unavailable member")
		}
		seenNames[name], seenUIDs[uid] = true, true
		ready := 0
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" {
				if c.Status != "True" {
					return errors.New("a node is not Ready")
				}
				ready++
			}
		}
		if ready != 1 {
			return errors.New("node readiness is incomplete or ambiguous")
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(n.Spec.ProviderID, "hcloud://"), 10, 64)
		if err != nil || n.Spec.ProviderID != "hcloud://"+strconv.FormatInt(id, 10) || id <= 0 || seenIDs[id] {
			return errors.New("node provider identity is invalid or duplicated")
		}
		seenIDs[id] = true
		s, exists := byID[id]
		if !exists || s.Name != name {
			return errors.New("node and provider identities do not join")
		}
		privateIP, err := netip.ParseAddr(s.PrivateNet[0].IP)
		if err != nil {
			return errors.New("provider private address is invalid")
		}
		internalIPs := 0
		for _, a := range n.Status.Addresses {
			if a.Type != "InternalIP" {
				continue
			}
			address, parseErr := netip.ParseAddr(a.Address)
			if parseErr != nil || address != privateIP {
				return errors.New("node and provider private addresses do not join")
			}
			internalIPs++
		}
		if internalIPs != 1 {
			return errors.New("node private address is missing or ambiguous")
		}
		if s.ServerType.Name == "" || s.ServerType.Name != n.Metadata.Labels["node.kubernetes.io/instance-type"] || s.Location.Name == "" || s.Location.Name != n.Metadata.Labels["topology.kubernetes.io/region"] {
			return errors.New("node hardware or location does not match the provider")
		}
		role, isStatic := static[name]
		if controlPlaneName.MatchString(name) {
			role, isStatic = "control-plane", true
		}
		if isStatic {
			if s.Labels["ksail.owned"] != "true" || s.Labels["ksail.cluster.name"] != "prod" || s.Labels["ksail.node.type"] != role || s.Labels["ksail.node.index"] != name[strings.LastIndex(name, "-")+1:] || s.Labels["hcloud/node-group"] != "" || n.Metadata.Labels["storage.platform.devantler.tech/migration-pool"] != "" {
				return errors.New("static provider ownership does not match the production roster")
			}
			counts["static"]++
			counts[role]++
			continue
		}
		match := migrationName.FindStringSubmatch(name)
		if match == nil || n.Metadata.Labels["storage.platform.devantler.tech/migration-pool"] != match[1] || s.Labels["hcloud/node-group"] != "autoscale-longhorn-"+match[1] {
			return errors.New("an unexpected worker or migration-pool identity remains")
		}
		if s.ServerType.Name != "cpx42" || s.Location.Name != "fsn1" || (s.Labels["ksail.cluster.name"] != "" && s.Labels["ksail.cluster.name"] != "prod") || (s.Labels["ksail.owned"] != "" && s.Labels["ksail.owned"] != "true") {
			return errors.New("migration provider hardware, location or ownership is unexpected")
		}
		counts[match[1]]++
	}
	if counts["static"] != 6 || counts["control-plane"] != 3 || counts["worker"] != 3 || counts["v1"] != 2 || counts["v2"] > 1 || (requireV2 && counts["v2"] != 1) {
		return errors.New("the required migration roster is incomplete")
	}
	return nil
}

func main() {
	if len(os.Args) != 2 || !slices.Contains([]string{"before-publish", "before-update", "after-update", "account-census"}, os.Args[1]) ||
		os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("GITHUB_REPOSITORY") != "devantler-tech/platform" {
		fmt.Fprintln(os.Stderr, "HOLD: migration headroom requires a protected production deployment")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}
	if os.Args[1] == "account-census" {
		if !authorizeAccountCensus(os.Getenv) {
			fmt.Fprintln(os.Stderr, "HOLD: provider account reconciliation requires a confirmed first attempt on main")
			os.Exit(1)
		}
		result, err := observeAccountCensus(ctx, client, os.Getenv("HCLOUD_TOKEN"), readNativeNodes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "HOLD: provider account reconciliation: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("OBSERVED: account_servers=%d joined_nodes=%d provider_only=%d classification=%s state=%s admission=none\n",
			result.Servers, result.Nodes, result.Extra, result.Classification, result.State)
		return
	}
	n, err := observe(ctx, client, os.Getenv("HCLOUD_TOKEN"), readNativeNodes, os.Args[1] == "after-update")
	if err != nil {
		fmt.Fprintf(os.Stderr, "HOLD: migration headroom stage=%s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
	fmt.Printf("PASS: migration headroom stage=%s joined_nodes=%d account_servers=%d reserved_builder_slots=1\n", os.Args[1], n, n)
}
