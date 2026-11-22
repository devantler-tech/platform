package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

const indexWorkflow = "repair-longhorn-provider-index"
const providerIndexLabel = "ksail.node.index"

// Each witness pins the provider ID, node name and UID, and old and new label
// values for one verified static server. A new node never inherits approval.
var approvedIndexes = []string{
	"44a5fe4d2e7446add3e490e773d56ca5be9a63bb707a66c533ede6b62810832e",
	"17b65789419bf8678d6c2dc72090ba6460fe7e08bdc89192ddc1fae76b56a8a2",
	"2f90e9c066e544b08e25817835033605c32ded87d813197e9663899f9d18b204",
	"667bcff556de6707ba50f67b1ecdd8e1a2a6942297247160879433c1f3dee58e",
}

type indexRepair struct {
	original server
	updated  server
	position int
}

func indexWitness(row controlPlaneIndex, uid string) string {
	data := fmt.Sprintf("%d\n%s\n%s\n%s\n%s\n", row.ID, row.Name, uid, row.Observed, row.Expected)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(data)))
}

func planIndexRepair(servers []server, nodes []node, expected []string) ([]indexRepair, error) {
	if _, err := describeAccountOwnership(servers, nodes); err != nil {
		return nil, err
	}
	uids := make(map[string]string, len(nodes))
	for _, node := range nodes {
		uids[node.Spec.ProviderID] = node.Metadata.UID
	}
	var repairs []indexRepair
	for i, candidate := range servers {
		uid := uids["hcloud://"+strconv.FormatInt(candidate.ID, 10)]
		if uid == "" || (!controlPlaneName.MatchString(candidate.Name) && !strings.HasPrefix(candidate.Name, "prod-worker-")) {
			continue
		}
		canonical := candidate.Name[strings.LastIndex(candidate.Name, "-")+1:]
		old := candidate.Labels[providerIndexLabel]
		if old == canonical {
			continue
		}
		row := controlPlaneIndex{ID: candidate.ID, Name: candidate.Name, Observed: old, Expected: canonical}
		if !slices.Contains(expected, indexWitness(row, uid)) {
			return nil, errors.New("a static index repair identity differs from the source-pinned witnesses")
		}
		updated := candidate
		updated.Labels = maps.Clone(candidate.Labels)
		updated.Labels[providerIndexLabel] = canonical
		repairs = append(repairs, indexRepair{original: candidate, updated: updated, position: i})
	}
	return repairs, nil
}

func runIndexRepair(ctx context.Context, a migrationProvider, readNodes func(context.Context) ([]byte, error), apply bool) error {
	before, nodes, err := readStableAccount(ctx, a.client, a.token, readNodes)
	if err != nil {
		return err
	}
	repairs, err := planIndexRepair(before, nodes, approvedIndexes)
	if err != nil {
		return err
	}
	updated := slices.Clone(before)
	for _, repair := range repairs {
		updated[repair.position] = repair.updated
	}
	joined, _, err := joinAccountServers(updated, nodes)
	if err != nil {
		return err
	}
	if err := qualify(joined, nodes, false); err != nil {
		return fmt.Errorf("proposed labels do not qualify the live roster: %w", err)
	}
	for _, repair := range repairs {
		if err := a.verifyIndexTarget(ctx, repair); err != nil {
			return err
		}
	}
	if !apply {
		fmt.Printf("PROVIDER_INDEX=VERIFIED_PLAN repairs=%d\n", len(repairs))
		return nil
	}
	for _, repair := range repairs {
		if err := a.verifyIndexTarget(ctx, repair); err != nil {
			return err
		}
		if err := a.updateIndex(ctx, repair); err != nil {
			return err
		}
	}
	after, finalNodes, err := readStableAccount(ctx, a.client, a.token, readNodes)
	if err != nil || !reflect.DeepEqual(updated, after) || !reflect.DeepEqual(nodes, finalNodes) {
		return errors.New("provider and native node rosters could not be verified after the index update")
	}
	fmt.Printf("PROVIDER_INDEX=VERIFIED repairs=%d\n", len(repairs))
	return nil
}

func (a migrationProvider) verifyIndexTarget(ctx context.Context, repair indexRepair) error {
	path := "/servers/" + strconv.FormatInt(repair.original.ID, 10)
	current, err := a.readBuilder(ctx, path)
	if err != nil || !reflect.DeepEqual(repair.original, current.server) || current.Locked == nil || *current.Locked {
		return errors.New("static server identity, labels or provider state changed before label repair")
	}
	return nil
}

func (a migrationProvider) updateIndex(ctx context.Context, change indexRepair) error {
	path := "/servers/" + strconv.FormatInt(change.original.ID, 10)
	payload, err := json.Marshal(struct {
		Labels map[string]string `json:"labels"`
	}{Labels: change.updated.Labels})
	if err != nil {
		return err
	}
	raw, status, err := a.requestJSON(ctx, http.MethodPut, path, payload)
	var result struct{ Server server }
	if err != nil || status != http.StatusOK || json.Unmarshal(raw, &result) != nil ||
		!reflect.DeepEqual(result.Server, change.updated) {
		return errors.New("index update response is unverified; read provider state before any retry")
	}
	return nil
}
