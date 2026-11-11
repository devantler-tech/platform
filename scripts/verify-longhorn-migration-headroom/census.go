package main

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strconv"
)

const censusConfirmation = "observe-longhorn-provider-account"

var snapshotBuildID = regexp.MustCompile(`^[A-Z2-7]{26}$`)
var errCensusHeld = errors.New("provider account reconciliation is incomplete, ambiguous or changed")

type accountCensus struct {
	Servers, Nodes, Extra int
	Classification, State string
}

func authorizeAccountCensus(getenv func(string) string) bool {
	return getenv("GITHUB_EVENT_NAME") == "workflow_dispatch" &&
		getenv("GITHUB_REF") == "refs/heads/main" &&
		getenv("GITHUB_WORKFLOW_REF") == "devantler-tech/platform/.github/workflows/observe-longhorn-migration-headroom.yaml@refs/heads/main" &&
		getenv("GITHUB_RUN_ATTEMPT") == "1" && getenv("RUNNER_ENVIRONMENT") == "github-hosted" &&
		getenv("LONGHORN_HEADROOM_CONFIRM") == censusConfirmation
}

func observeAccountCensus(ctx context.Context, client *http.Client, token string, readNodes func(context.Context) ([]byte, error)) (accountCensus, error) {
	read := func() ([]node, error) {
		raw, err := readNodes(ctx)
		if err != nil {
			return nil, errCensusHeld
		}
		return decodeNodes(raw)
	}
	before, err := read()
	if err != nil {
		return accountCensus{}, err
	}
	first, err := collectServerInventory(ctx, client, token, maxAccountServers)
	if err != nil {
		return accountCensus{}, err
	}
	result, err := classifyAccountCensus(first, before)
	if err != nil {
		return accountCensus{}, err
	}
	second, err := collectServerInventory(ctx, client, token, maxAccountServers)
	if err != nil {
		return accountCensus{}, err
	}
	byID := func(a, b server) int { return cmp.Compare(a.ID, b.ID) }
	slices.SortFunc(first, byID)
	slices.SortFunc(second, byID)
	after, err := read()
	if err != nil || !reflect.DeepEqual(first, second) || !reflect.DeepEqual(before, after) || active(ctx) != nil {
		return accountCensus{}, errCensusHeld
	}
	return result, nil
}

// Classify the one provider-only row for investigation. Its labels never
// establish deletion authority or reserve the builder slot.
func classifyAccountCensus(servers []server, nodes []node) (accountCensus, error) {
	extra := len(servers) - len(nodes)
	if len(servers) > maxAccountServers || extra < 0 || extra > 1 {
		return accountCensus{}, errCensusHeld
	}
	nodeIDs := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		nodeIDs[node.Spec.ProviderID] = true
	}
	ids, names := map[int64]bool{}, map[string]bool{}
	joined := make([]server, 0, len(nodes))
	var unmatched server
	for _, server := range servers {
		if server.ID <= 0 || server.Name == "" || server.Status == "" || ids[server.ID] || names[server.Name] {
			return accountCensus{}, errCensusHeld
		}
		ids[server.ID], names[server.Name] = true, true
		if nodeIDs["hcloud://"+strconv.FormatInt(server.ID, 10)] {
			joined = append(joined, server)
		} else {
			unmatched = server
		}
	}
	if len(joined) != len(nodes) || qualify(joined, nodes, false) != nil {
		return accountCensus{}, errCensusHeld
	}
	result := accountCensus{Servers: len(servers), Nodes: len(nodes), Extra: extra, Classification: "none", State: "none"}
	if extra == 1 {
		result.State = "not-running"
		if unmatched.Status == "running" {
			result.State = "running"
		}
		result.Classification = classifyProviderOnly(unmatched, joined[0].PrivateNet[0].Network)
	}
	return result, nil
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
	case group != "" && len(s.PrivateNet) == 1 && s.PrivateNet[0].Network == network:
		return "autoscaler-labeled"
	default:
		return "unclassified"
	}
}
