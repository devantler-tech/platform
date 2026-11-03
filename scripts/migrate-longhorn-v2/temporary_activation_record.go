package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/devantler-tech/platform/internal/strictjson"
)

const temporaryActivationJournal = "longhorn-v2-temporary-activation"

var temporaryV2Selections = [...]string{"temporary-v1-1", "temporary-v1-2", "temporary-v2-1"}

type temporaryActivationRecord struct {
	Version   int                           `json:"version"`
	Source    string                        `json:"source"`
	OwnerRun  string                        `json:"ownerRun"`
	Targets   map[string]temporaryV2Binding `json:"targets"`
	Completed bool                          `json:"completed"`
}

// decodeTemporaryActivationRecord rejects ambiguous JSON before its typed
// decode, including duplicate members and case aliases in every fixed target.
func decodeTemporaryActivationRecord(data []byte) (temporaryActivationRecord, error) {
	if len(data) == 0 || len(data) > 32<<10 {
		return temporaryActivationRecord{}, errTemporaryActivation
	}
	canonical := map[string][]string{"": {"version", "source", "ownerRun", "targets", "completed"}}
	fields := []string{"selection", "name", "nodeUID", "providerID", "machineUUID", "bootID", "osImage", "longhornUID", "ephemeralUUID", "rawPartitionUUID", "preparationUID"}
	for _, selection := range temporaryV2Selections {
		canonical["targets."+selection] = fields
	}
	if strictjson.Validate(data, canonical) != nil {
		return temporaryActivationRecord{}, errTemporaryActivation
	}
	var record temporaryActivationRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || record.validate() != nil {
		return temporaryActivationRecord{}, errTemporaryActivation
	}
	return record, nil
}

// validate requires three independent fixed pool members and complete native
// partition identities. An existing journal cannot substitute another worker or
// reuse one physical disk under multiple selections when a run resumes.
func (r temporaryActivationRecord) validate() error {
	if r.Version != 1 || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(r.Source) || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(r.OwnerRun) || len(r.Targets) != len(temporaryV2Selections) {
		return errTemporaryActivation
	}
	seen := [7]map[string]bool{}
	preparations := map[string]bool{}
	for i := range seen {
		seen[i] = map[string]bool{}
	}
	for _, selection := range temporaryV2Selections {
		b, ok := r.Targets[selection]
		if !ok || b.Selection != selection || !inventoryTemporaryNode.MatchString(b.Name) ||
			!strings.HasPrefix(b.Name, "autoscale-longhorn-"+strings.Split(selection, "-")[1]+"-") ||
			b.NodeUID == "" || !regexp.MustCompile(`^hcloud://[1-9][0-9]*$`).MatchString(b.ProviderID) || b.MachineUUID == "" ||
			!inventoryBoot.MatchString(b.BootID) || b.OSImage == "" || b.LonghornUID == "" || b.EphemeralUUID == "" ||
			!inventoryBoot.MatchString(strings.ToLower(b.RawPartitionUUID)) || strings.EqualFold(b.RawPartitionUUID, b.EphemeralUUID) {
			return errTemporaryActivation
		}
		if selection == "temporary-v2-1" {
			if b.PreparationUID != "" {
				return errTemporaryActivation
			}
		} else {
			if b.PreparationUID == "" || preparations[b.PreparationUID] {
				return errTemporaryActivation
			}
			preparations[b.PreparationUID] = true
		}
		for i, identity := range []string{b.Name, b.NodeUID, b.ProviderID, strings.ToLower(b.MachineUUID), b.LonghornUID, strings.ToLower(b.EphemeralUUID), strings.ToLower(b.RawPartitionUUID)} {
			if seen[i][identity] {
				return errTemporaryActivation
			}
			seen[i][identity] = true
		}
	}
	return nil
}
