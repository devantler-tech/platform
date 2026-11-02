package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/devantler-tech/platform/internal/longhorninventory"
	"github.com/devantler-tech/platform/internal/strictjson"
)

type inventoryProjection struct {
	Address, BootID, Digest string
	Replicas                []longhorninventory.Replica
	Registered              bool
}

var errInventory = errors.New("replica inventory identity, placement or completeness is unproven")
var inventoryBoot = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var inventoryTarget = regexp.MustCompile(`^prod-worker-[123]$`)

type inventoryStage uint8

const (
	stageUnknown inventoryStage = iota
	stageDeadline
	stageProjectionBefore
	stageClient
	stageHostBefore
	stageBootBefore
	stageListingBefore
	stageOwnershipBefore
	stageListingAfter
	stageProjectionAfter
	stageAPIConsistency
	stageHostAfter
	stageBootAfter
	stageOwnershipAfter
	stageListingConsistency
	stageClose
	stageSourceBefore
	stageSourceAfter
	stageOutput
)

func (stage inventoryStage) Error() string {
	names := [...]string{"unknown", "deadline", "projection-before", "client", "host-before", "boot-before", "listing-before",
		"ownership-before", "listing-after", "projection-after", "api-consistency", "host-after", "boot-after", "ownership-after",
		"listing-consistency", "close", "source-before", "source-after", "output"}
	if int(stage) >= len(names) {
		stage = stageUnknown
	}
	return "Native replica inventory held [stage=" + names[stage] + "]; no complete observation or reset clearance is established."
}

func (stage inventoryStage) Unwrap() error { return errInventory }

// Only the closed stage vocabulary enters public logs, even if a future caller
// accidentally returns or wraps an unclassified provider error.
func inventoryFailureMessage(err error) string {
	var stage inventoryStage
	if errors.As(err, &stage) {
		return stage.Error()
	}
	return stageUnknown.Error()
}

func projectInventory(target string, node, storage object, replicas []object) (inventoryProjection, error) {
	p := inventoryProjection{BootID: text(node, "status", "nodeInfo", "bootID")}
	if !inventoryTarget.MatchString(target) || !inventoryBoot.MatchString(p.BootID) ||
		text(node, "metadata", "uid") == "" || text(node, "metadata", "resourceVersion") == "" ||
		text(node, "spec", "providerID") == "" || text(node, "status", "nodeInfo", "systemUUID") == "" ||
		text(storage, "metadata", "name") != target || text(storage, "metadata", "uid") == "" || text(storage, "metadata", "resourceVersion") == "" || len(replicas) > longhorninventory.MaxEntries {
		return inventoryProjection{}, errInventory
	}
	var err error
	p.Address, err = verifyNode(node, journal{Target: target, NodeUID: text(node, "metadata", "uid"),
		ProviderID: text(node, "spec", "providerID"), MachineUUID: text(node, "status", "nodeInfo", "systemUUID")}, true)
	if err != nil {
		return inventoryProjection{}, errInventory
	}
	disks, ok := value(storage, "spec", "disks").(map[string]any)
	statuses, statusOK := value(storage, "status", "diskStatus").(map[string]any)
	if !ok || !statusOK || len(disks) > 16 || len(statuses) > 16 {
		return inventoryProjection{}, errInventory
	}
	diskID := ""
	bindings := object{}
	for slot, raw := range disks {
		disk, ok := raw.(map[string]any)
		status, statusOK := statuses[slot].(map[string]any)
		if !ok || !statusOK || text(disk, "path") == "" {
			return inventoryProjection{}, errInventory
		}
		bindings[slot] = object{"diskUUID": text(status, "diskUUID"), "diskPath": text(status, "diskPath"), "diskName": text(status, "diskName"),
			"diskType": text(status, "diskType"), "diskDriver": text(status, "diskDriver")}
		if text(disk, "path") != "/var/lib/longhorn" {
			continue
		}
		if diskID != "" || text(disk, "diskType") != "filesystem" || text(status, "diskPath") != "/var/lib/longhorn" ||
			text(status, "diskUUID") == "" || text(status, "diskName") == "" || text(status, "diskType") != "filesystem" ||
			text(disk, "diskDriver") != text(status, "diskDriver") {
			return inventoryProjection{}, errInventory
		}
		diskID, p.Registered = text(status, "diskUUID"), true
	}
	selected := make([]object, 0)
	for _, replica := range replicas {
		placement := text(replica, "spec", "nodeID")
		if placement == "" || !kubeName.MatchString(placement) {
			return inventoryProjection{}, errInventory
		}
		if placement != target {
			continue
		}
		directory := text(replica, "spec", "dataDirectoryName")
		if !p.Registered || text(replica, "spec", "diskPath") != "/var/lib/longhorn" || text(replica, "spec", "diskID") != diskID ||
			text(replica, "spec", "dataEngine") != "v1" || len(directory) > 255 || !kubeName.MatchString(directory) ||
			text(replica, "metadata", "name") == "" || text(replica, "metadata", "uid") == "" || text(replica, "metadata", "resourceVersion") == "" {
			return inventoryProjection{}, errInventory
		}
		p.Replicas = append(p.Replicas, longhorninventory.Replica{Name: text(replica, "metadata", "name"), UID: text(replica, "metadata", "uid"), Directory: directory})
		selected = append(selected, object{"metadata": inventoryIdentity(replica), "spec": value(replica, "spec"), "status": value(replica, "status")})
	}
	slices.SortFunc(selected, func(a, b object) int {
		return strings.Compare(text(a, "metadata", "name"), text(b, "metadata", "name"))
	})
	slices.SortFunc(p.Replicas, func(a, b longhorninventory.Replica) int { return strings.Compare(a.Name, b.Name) })
	encoded, err := json.Marshal(object{"node": object{"metadata": inventoryIdentity(node), "providerID": text(node, "spec", "providerID"),
		"systemUUID": text(node, "status", "nodeInfo", "systemUUID"), "bootID": p.BootID, "address": p.Address},
		"storage": object{"metadata": inventoryIdentity(storage), "spec": value(storage, "spec"), "bindings": bindings}, "replicas": selected})
	if err != nil {
		return inventoryProjection{}, errInventory
	}
	digest := sha256.Sum256(encoded)
	p.Digest = hex.EncodeToString(digest[:])
	return p, nil
}

func inventoryIdentity(o object) object {
	return object{"name": text(o, "metadata", "name"), "uid": text(o, "metadata", "uid"),
		"deletionTimestamp": value(o, "metadata", "deletionTimestamp"), "ownerReferences": value(o, "metadata", "ownerReferences")}
}

func decodeInventory(data []byte) ([]object, error) {
	if len(data) > 16<<20 || strictjson.Validate(data, nil) != nil {
		return nil, errInventory
	}
	items, err := decodeList(data)
	var envelope object
	if err != nil || len(items) > longhorninventory.MaxEntries || json.Unmarshal(data, &envelope) != nil || text(envelope, "metadata", "resourceVersion") == "" {
		return nil, errInventory
	}
	seen := map[string]bool{}
	for _, item := range items {
		uid := text(item, "metadata", "uid")
		if text(item, "metadata", "resourceVersion") == "" || seen[uid] {
			return nil, errInventory
		}
		seen[uid] = true
	}
	return items, nil
}

func authorizeInventory(getenv func(string) string, target string) error {
	if !inventoryTarget.MatchString(target) || getenv("LONGHORN_V2_NODE") != target || getenv("LONGHORN_V2_OPERATION") != "inventory-replicas" {
		return errors.New("the explicit permanent-worker inventory operation is required")
	}
	return authorizeWorkflow(getenv, "inventory-longhorn-replicas-"+target)
}
