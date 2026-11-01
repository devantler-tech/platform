package main

import (
	"errors"
	"regexp"
)

type journal struct {
	Schema        int    `json:"schema"`
	Target        string `json:"target"`
	Source        string `json:"source"`
	OwnerRun      string `json:"ownerRun"`
	Phase         string `json:"phase"`
	NodeUID       string `json:"nodeUID"`
	ProviderID    string `json:"providerID"`
	MachineUUID   string `json:"machineUUID"`
	LonghornUID   string `json:"longhornUID"`
	EphemeralUUID string `json:"ephemeralUUID"`
	OriginalDisks object `json:"originalDisks"`
}

func validateJournal(j journal, target string) error {
	if j.Schema != 1 || j.Target != target || !regexp.MustCompile(`^prod-worker-[123]$`).MatchString(target) ||
		!regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(j.Source) || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(j.OwnerRun) ||
		j.NodeUID == "" || !regexp.MustCompile(`^hcloud://[1-9][0-9]*$`).MatchString(j.ProviderID) || j.MachineUUID == "" || j.LonghornUID == "" || j.EphemeralUUID == "" {
		return errors.New("journal does not bind the exact original storage identities")
	}
	switch j.Phase {
	case "evacuating", "draining", "reset-requested", "prepared":
	default:
		return errors.New("journal phase is outside the node-preparation procedure")
	}
	if len(j.OriginalDisks) != 1 {
		return errors.New("node preparation requires exactly the original V1 filesystem disk")
	}
	for _, raw := range j.OriginalDisks {
		d, ok := raw.(map[string]any)
		if !ok || text(d, "path") != "/var/lib/longhorn" || text(d, "diskType") != "filesystem" || value(d, "allowScheduling") != true || value(d, "evictionRequested") != false {
			return errors.New("original disk is outside the reviewed node-reset scope")
		}
	}
	return nil
}
