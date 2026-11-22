package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strconv"
)

const censusConfirmation = "observe-longhorn-provider-account"
const ownershipConfirmation = "observe-longhorn-provider-ownership"

var snapshotBuildID = regexp.MustCompile(`^[A-Z2-7]{26}$`)
var errCensusHeld = errors.New("provider account reconciliation is incomplete, ambiguous or changed")

type accountCensus struct {
	Servers, Nodes, Extra int
	Classification, State string
}

func authorizeAccountCensus(getenv func(string) string) bool {
	operation := getenv("PROD_OPERATION")
	return getenv("GITHUB_EVENT_NAME") == "workflow_dispatch" &&
		getenv("GITHUB_REF") == "refs/heads/main" &&
		getenv("GITHUB_WORKFLOW_REF") == "devantler-tech/platform/.github/workflows/observe-longhorn-migration-headroom.yaml@refs/heads/main" &&
		getenv("GITHUB_RUN_ATTEMPT") == "1" && getenv("RUNNER_ENVIRONMENT") == "github-hosted" &&
		(operation == censusConfirmation || operation == ownershipConfirmation)
}

func observeAccountCensus(ctx context.Context, client *http.Client, token string, readNodes func(context.Context) ([]byte, error)) (accountCensus, error) {
	servers, nodes, err := readStableAccount(ctx, client, token, readNodes)
	if err != nil {
		return accountCensus{}, err
	}
	return classifyAccountCensus(servers, nodes)
}

func readStableAccount(ctx context.Context, client *http.Client, token string, readNodes func(context.Context) ([]byte, error)) ([]server, []node, error) {
	read := func() ([]node, error) {
		raw, err := readNodes(ctx)
		if err != nil {
			return nil, fmt.Errorf("%w: native node inventory is unavailable", errCensusHeld)
		}
		return decodeNodes(raw)
	}
	before, err := read()
	if err != nil {
		return nil, nil, err
	}
	first, err := collectServerInventory(ctx, client, token, maxAccountServers)
	if err != nil {
		return nil, nil, err
	}
	second, err := collectServerInventory(ctx, client, token, maxAccountServers)
	if err != nil {
		return nil, nil, err
	}
	byID := func(a, b server) int { return cmp.Compare(a.ID, b.ID) }
	slices.SortFunc(first, byID)
	slices.SortFunc(second, byID)
	after, err := read()
	if err != nil {
		return nil, nil, fmt.Errorf("%w: final native node inventory is unavailable or incomplete", errCensusHeld)
	}
	if !reflect.DeepEqual(first, second) {
		return nil, nil, fmt.Errorf("%w: provider identity or state changed during observation", errCensusHeld)
	}
	if !reflect.DeepEqual(before, after) {
		return nil, nil, fmt.Errorf("%w: node identity or availability changed during observation", errCensusHeld)
	}
	if active(ctx) != nil {
		return nil, nil, fmt.Errorf("%w: observation deadline or cancellation reached", errCensusHeld)
	}
	// Even a held roster's label-only diagnosis requires both complete provider
	// lists inside unchanged native Node reads. Never diagnose an unstable sample.
	return first, before, nil
}

// Classify the one provider-only row for investigation. Its labels never
// establish deletion authority or reserve the builder slot.
func classifyAccountCensus(servers []server, nodes []node) (accountCensus, error) {
	joined, unmatched, err := joinAccountServers(servers, nodes)
	if err != nil {
		return accountCensus{}, err
	}
	result := accountCensus{Servers: len(servers), Nodes: len(nodes), Extra: len(servers) - len(nodes), Classification: "none", State: "none"}
	if unmatched != nil {
		result.State = "not-running"
		if unmatched.Status == "running" {
			result.State = "running"
		}
		var network int64
		for _, server := range joined {
			if len(server.PrivateNet) != 1 || server.PrivateNet[0].Network <= 0 || network != 0 && network != server.PrivateNet[0].Network {
				network = 0
				break
			}
			network = server.PrivateNet[0].Network
		}
		result.Classification = classifyProviderOnly(*unmatched, network)
	}
	if err := qualify(joined, nodes, false); err != nil {
		// These are labels on the provider-only candidate, not ownership or
		// available capacity. Every original qualification predicate still holds.
		return accountCensus{}, fmt.Errorf("%w: %w: provider_only=%d classification=%s state=%s ownership=UNVERIFIED headroom=UNVERIFIED", errCensusHeld, err, result.Extra, result.Classification, result.State)
	}
	return result, nil
}

func joinAccountServers(servers []server, nodes []node) ([]server, *server, error) {
	extra := len(servers) - len(nodes)
	if len(servers) > maxAccountServers || extra < 0 || extra > 1 {
		return nil, nil, fmt.Errorf("%w: account and node counts exceed the reconciliation bound", errCensusHeld)
	}
	nodeIDs := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		nodeIDs[node.Spec.ProviderID] = true
	}
	ids, names := map[int64]bool{}, map[string]bool{}
	joined := make([]server, 0, len(nodes))
	var unmatched *server
	for _, server := range servers {
		if server.ID <= 0 || server.Name == "" || server.Status == "" || ids[server.ID] || names[server.Name] {
			return nil, nil, fmt.Errorf("%w: provider identities are incomplete or duplicated", errCensusHeld)
		}
		ids[server.ID], names[server.Name] = true, true
		if nodeIDs["hcloud://"+strconv.FormatInt(server.ID, 10)] {
			joined = append(joined, server)
		} else {
			unmatched = &server
		}
	}
	if len(joined) != len(nodes) {
		return nil, nil, fmt.Errorf("%w: provider-to-node join is incomplete", errCensusHeld)
	}
	return joined, unmatched, nil
}

func classifyProviderOnly(s server, network int64) string {
	cluster := s.Labels["ksail.cluster.name"]
	builderCluster := s.Labels["ksail.io/cluster"]
	group := s.Labels["hcloud/node-group"]
	switch {
	case (cluster != "" && cluster != "prod") || (builderCluster != "" && builderCluster != "prod"):
		return "other-cluster-labeled"
	case snapshotBuildID.MatchString(s.Labels["ksail.io/snapshot-build"]) && s.Labels["ksail.owned"] == "" && group == "":
		return "snapshot-build-labeled"
	case cluster == "prod" && s.Labels["ksail.owned"] == "true":
		return "production-managed-labeled"
	case group != "" && network > 0 && len(s.PrivateNet) == 1 && s.PrivateNet[0].Network == network:
		return "autoscaler-labeled"
	default:
		return "unclassified"
	}
}
