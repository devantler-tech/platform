package main

import (
	"slices"
	"strings"

	"github.com/devantler-tech/platform/internal/longhornmigration"
)

const nativeRestoreUnusedProfile = "unused-stage-144"
const nativeRestoreRetainedProfile = "retained-stage-144"

// These native identities must be derived by the read-only prepared-journal
// observer. A caller-supplied receipt alone is not physical layout evidence.
type nativeRestorePreparationReceipt struct {
	Profile, JournalSHA256, BootID, RawPartitionUUID string
	Journal                                          benchmarkResource
	ProviderID, MachineUUID, LonghornUID, DiskUUID   string
	Activation                                       benchmarkResource
	ActivationSHA256                                 string
}

func (p nativeRestorePreparationReceipt) selection() string {
	selection, ok := strings.CutPrefix(p.Journal.Name, "longhorn-v2-prepare-")
	if !ok || !inventoryTemporarySelection.MatchString(selection) {
		return ""
	}
	return selection
}

func (p nativeRestorePreparationReceipt) mapper(target string) (string, error) {
	selection := p.selection()
	if selection == "" || !inventoryTemporaryNode.MatchString(target) ||
		!strings.HasPrefix(target, "autoscale-longhorn-"+strings.Split(selection, "-")[1]+"-") ||
		!nativeRestoreUID(p.Journal.UID) || p.Journal.Namespace != "longhorn-system" || !seedSHA256.MatchString(p.JournalSHA256) ||
		!inventoryBoot.MatchString(p.BootID) || !inventoryBoot.MatchString(p.RawPartitionUUID) ||
		!nativeRestoreUID(p.ProviderID) || !nativeRestoreUID(p.MachineUUID) || !nativeRestoreUID(p.LonghornUID) || !nativeRestoreUID(p.DiskUUID) ||
		p.Activation.Name != longhornmigration.ActivationJournal || p.Activation.Namespace != "longhorn-system" ||
		!nativeRestoreUID(p.Activation.UID) || !seedSHA256.MatchString(p.ActivationSHA256) {
		return "", errNativeRestoreEvidence
	}
	profile := nativeRestoreUnusedProfile
	if selection == "temporary-v2-1" {
		profile = nativeRestoreRetainedProfile
	}
	if p.Profile != profile {
		return "", errNativeRestoreEvidence
	}
	return "/dev/mapper/luks2-" + temporaryRawID(selection), nil
}

func nativeRestoreFixedRoster(targets []nativeRestoreTarget) bool {
	selections := longhornmigration.ActivationSelections()
	if len(targets) != len(selections) {
		return false
	}
	original := targets[0].Preparation
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		prepared := target.Preparation
		selection := prepared.selection()
		if _, err := prepared.mapper(target.Name); err != nil || !slices.Contains(selections[:], selection) || seen[selection] ||
			prepared.Activation != original.Activation || prepared.ActivationSHA256 != original.ActivationSHA256 {
			return false
		}
		seen[selection] = true
	}
	return true
}
