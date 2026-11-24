package main

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// The observer reuses the recovery checks without claiming the journal,
// uncordoning the node or applying any configuration.
// Its workflow shares the production lock with the preparation workflow.
func (p temporaryPreparation) observeRecovery(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := sourceCurrent(ctx, p.c.run, p.source); err != nil {
		return err
	}
	cm, record, err := p.readRecord(ctx)
	if err != nil || cm == nil || record.Schema != 2 || record.Phase != "reset-requested" || record.OwnerRun == p.owner {
		return errors.New("the original reset journal is not ready for independent observation")
	}
	p.j, p.staging = record, record.StagingLayout
	_, gate := p.unusedPostState(ctx)
	current, _, err := p.readRecord(ctx)
	if err != nil || current == nil || text(current, "metadata", "uid") != text(cm, "metadata", "uid") ||
		text(current, "metadata", "resourceVersion") != text(cm, "metadata", "resourceVersion") {
		return errors.New("the recovery journal changed during observation")
	}
	if err = sourceCurrent(ctx, p.c.run, p.source); err != nil {
		return err
	}
	return gate
}

func temporaryRecoveryReadCommand(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
	if input != nil || !temporaryRecoveryReadAllowed(program, args) {
		return nil, errors.New("the temporary recovery observation only permits scoped reads")
	}
	return inventoryExecuteCommand(ctx, program, args...)
}

func temporaryRecoveryReadAllowed(program string, args []string) bool {
	switch program {
	case "git":
		return inventoryReadAllowed(program, args...)
	case "kubectl":
		if len(args) < 5 || !slices.Equal(args[:3], []string{"--context=admin@prod", "--request-timeout=20s", "get"}) {
			return false
		}
		switch args[3] {
		case "nodes":
			return slices.Equal(args[4:], []string{"--output=json"})
		case "node", "nodes.longhorn.io":
			return len(args) >= 6 && inventoryTemporaryNode.MatchString(args[4]) &&
				(args[3] == "node" && slices.Equal(args[5:], []string{"--output=json"}) ||
					args[3] == "nodes.longhorn.io" && slices.Equal(args[5:], []string{"--output=json", "--namespace=longhorn-system"}))
		case "configmap":
			return len(args) == 8 && (args[4] == "longhorn-v2-prepare-temporary-v1-1" || args[4] == "longhorn-v2-prepare-temporary-v1-2") &&
				slices.Equal(args[5:], []string{"--output=json", "--namespace=longhorn-system", "--ignore-not-found"})
		}
	case "talosctl":
		if inventoryReadAllowed(program, args...) {
			return true
		}
		if len(args) != 5 || !strings.HasPrefix(args[0], "--nodes=") || args[1] != "get" ||
			(args[2] != "volumeconfig" && args[2] != "volumestatus") || (args[3] != "EPHEMERAL" && args[3] != "r-longhorn") ||
			args[4] != "--output=json" {
			return false
		}
		address, err := netip.ParseAddr(strings.TrimPrefix(args[0], "--nodes="))
		return err == nil && netip.MustParsePrefix("10.0.0.0/16").Contains(address)
	}
	return false
}
