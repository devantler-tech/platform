package main

import (
	"os"

	"github.com/devantler-tech/platform/internal/longhornmigration"
)

func (e engine) migrationStorageEligible(node, storage object) (bool, error) {
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
	return longhornmigration.Eligible(data, node, storage)
}
