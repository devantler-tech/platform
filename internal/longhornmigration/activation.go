package longhornmigration

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/devantler-tech/platform/internal/strictjson"
)

const (
	// ActivationJournal is the fixed three-worker raw registration receipt.
	ActivationJournal = "longhorn-v2-temporary-activation"
	// ActivationOwner binds registered storage to that receipt's Kubernetes UID.
	ActivationOwner = "storage.platform.devantler.tech/v2-activation"
)

// ActivationBinding retains the native identities verified before registration.
type ActivationBinding struct {
	Selection        string `json:"selection"`
	Name             string `json:"name"`
	NodeUID          string `json:"nodeUID"`
	ProviderID       string `json:"providerID"`
	MachineUUID      string `json:"machineUUID"`
	BootID           string `json:"bootID"`
	OSImage          string `json:"osImage"`
	LonghornUID      string `json:"longhornUID"`
	EphemeralUUID    string `json:"ephemeralUUID"`
	RawPartitionUUID string `json:"rawPartitionUUID"`
	PreparationUID   string `json:"preparationUID,omitempty"`
}

// SameStorageIdentity compares the durable worker, partition and preparation
// identities. Boot and OS observations describe the original read bracket; a
// later caller must freshly prove its own bracket rather than reuse those values.
func (b ActivationBinding) SameStorageIdentity(other ActivationBinding) bool {
	b.BootID, b.OSImage = "", ""
	other.BootID, other.OSImage = "", ""
	return b == other
}

// ActivationRecord binds registration and canary acceptance to the same plan.
type ActivationRecord struct {
	Version   int                          `json:"version"`
	Source    string                       `json:"source"`
	OwnerRun  string                       `json:"ownerRun"`
	Targets   map[string]ActivationBinding `json:"targets"`
	Completed bool                         `json:"completed"`
}

var errActivation = errors.New("temporary raw storage identity or registration ownership is unproven")

// ActivationSelections returns the fixed roster without exposing mutable state.
func ActivationSelections() [3]string {
	return [3]string{"temporary-v1-1", "temporary-v1-2", "temporary-v2-1"}
}

// DecodeActivationRecord rejects oversized, duplicate, aliased and unknown JSON.
func DecodeActivationRecord(data []byte) (ActivationRecord, error) {
	if len(data) == 0 || len(data) > 32<<10 {
		return ActivationRecord{}, errActivation
	}
	canonical := map[string][]string{"": {"version", "source", "ownerRun", "targets", "completed"}}
	fields := []string{"selection", "name", "nodeUID", "providerID", "machineUUID", "bootID", "osImage", "longhornUID", "ephemeralUUID", "rawPartitionUUID", "preparationUID"}
	for _, selection := range ActivationSelections() {
		canonical["targets."+selection] = fields
	}
	if strictjson.Validate(data, canonical) != nil {
		return ActivationRecord{}, errActivation
	}
	var record ActivationRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || record.Validate() != nil {
		return ActivationRecord{}, errActivation
	}
	return record, nil
}

// Validate requires the three distinct workers, partitions and preparation UIDs.
// It validates receipt structure; native callers still establish live acceptance.
func (r ActivationRecord) Validate() error {
	if r.Version != 1 || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(r.Source) || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(r.OwnerRun) || len(r.Targets) != 3 {
		return errActivation
	}
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	seen := [7]map[string]bool{}
	preparations := map[string]bool{}
	for i := range seen {
		seen[i] = map[string]bool{}
	}
	for _, selection := range ActivationSelections() {
		b, ok := r.Targets[selection]
		if !ok || b.Selection != selection || !regexp.MustCompile(`^autoscale-longhorn-(v[12])-[0-9a-f]{16}$`).MatchString(b.Name) ||
			!strings.HasPrefix(b.Name, "autoscale-longhorn-"+strings.Split(selection, "-")[1]+"-") ||
			b.NodeUID == "" || !regexp.MustCompile(`^hcloud://[1-9][0-9]*$`).MatchString(b.ProviderID) || b.MachineUUID == "" ||
			!uuid.MatchString(b.BootID) || b.OSImage == "" || b.LonghornUID == "" || b.EphemeralUUID == "" ||
			!uuid.MatchString(strings.ToLower(b.RawPartitionUUID)) || strings.EqualFold(b.RawPartitionUUID, b.EphemeralUUID) {
			return errActivation
		}
		if b.PreparationUID == "" || preparations[b.PreparationUID] {
			return errActivation
		}
		preparations[b.PreparationUID] = true
		for i, identity := range []string{b.Name, b.NodeUID, b.ProviderID, strings.ToLower(b.MachineUUID), b.LonghornUID, strings.ToLower(b.EphemeralUUID), strings.ToLower(b.RawPartitionUUID)} {
			if seen[i][identity] {
				return errActivation
			}
			seen[i][identity] = true
		}
	}
	return nil
}
