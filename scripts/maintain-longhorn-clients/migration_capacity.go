package main

import (
	"context"
	"os"

	"github.com/devantler-tech/platform/internal/longhornmigration"
)

func (e engine) migrationStorageEligible(ctx context.Context, node, storage object) (bool, error) {
	if str(storage, "metadata", "labels", longhornmigration.EnabledLabel) == "" {
		return false, nil
	}
	readSource := e.migrationSource
	if readSource == nil {
		readSource = func() ([]byte, error) { return os.ReadFile("ksail.prod.yaml") }
	}
	data, err := readSource()
	if err != nil {
		return false, err
	}
	var activation object
	if str(node, "metadata", "labels", longhornmigration.PoolLabel) == "v1" && val(storage, "spec", "disks", "longhorn-v2") != nil {
		activation, err = e.c.read(ctx, "configmap", "longhorn-system", longhornmigration.ActivationJournal)
		if err != nil {
			return false, err
		}
	}
	return longhornmigration.Eligible(data, node, storage, activation)
}
