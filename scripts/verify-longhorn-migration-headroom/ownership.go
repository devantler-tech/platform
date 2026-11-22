package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
)

func writeAccountOwnership(ctx context.Context, client *http.Client) error {
	servers, nodes, err := readStableAccount(ctx, client, os.Getenv("HCLOUD_TOKEN"), readNativeNodes)
	if err != nil {
		return err
	}
	evidence, err := describeAccountOwnership(servers, nodes)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(evidence)
}

type providerIdentity struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type controlPlaneIndex struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Expected string `json:"expected_index"`
	Observed string `json:"observed_index"`
}

type accountOwnershipEvidence struct {
	Admission     string              `json:"admission"`
	Unmatched     *providerIdentity   `json:"unmatched,omitempty"`
	IndexMismatch []controlPlaneIndex `json:"control_plane_index_mismatches,omitempty"`
}

// Build a locator for the provider investigation after the stable account
// read. The locator is passed only to the encrypted production operation
// output; it carries no ownership, cleanup or admission decision.
func describeAccountOwnership(servers []server, nodes []node) (accountOwnershipEvidence, error) {
	joined, unmatched, err := joinAccountServers(servers, nodes)
	if err != nil {
		return accountOwnershipEvidence{}, err
	}
	if len(nodes) < 8 || len(nodes) > 9 {
		return accountOwnershipEvidence{}, fmt.Errorf("%w: unexpected native roster size", errCensusHeld)
	}
	byID := make(map[string]node, len(nodes))
	names, uids := map[string]bool{}, map[string]bool{}
	for _, node := range nodes {
		id, name, uid := node.Spec.ProviderID, node.Metadata.Name, node.Metadata.UID
		if _, duplicate := byID[id]; duplicate || name == "" || uid == "" || names[name] || uids[uid] || node.Metadata.DeletionTimestamp != nil {
			return accountOwnershipEvidence{}, fmt.Errorf("%w: ambiguous native node", errCensusHeld)
		}
		byID[id], names[name], uids[uid] = node, true, true
	}
	evidence := accountOwnershipEvidence{Admission: "none"}
	for _, server := range joined {
		node := byID["hcloud://"+strconv.FormatInt(server.ID, 10)]
		if node.Metadata.Name != server.Name {
			return accountOwnershipEvidence{}, fmt.Errorf("%w: provider and native node names differ", errCensusHeld)
		}
		if !controlPlaneName.MatchString(server.Name) {
			continue
		}
		_, expected, _ := strings.Cut(server.Name, "prod-control-plane-")
		observed := server.Labels["ksail.node.index"]
		if expected != observed {
			evidence.IndexMismatch = append(evidence.IndexMismatch, controlPlaneIndex{
				ID: server.ID, Name: server.Name, Expected: expected, Observed: observed,
			})
		}
	}
	if unmatched != nil {
		evidence.Unmatched = &providerIdentity{ID: unmatched.ID, Name: unmatched.Name, Status: unmatched.Status}
	}
	return evidence, nil
}
