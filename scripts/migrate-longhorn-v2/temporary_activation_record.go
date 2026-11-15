package main

import "github.com/devantler-tech/platform/internal/longhornmigration"

const temporaryActivationJournal = longhornmigration.ActivationJournal

var temporaryV2Selections = longhornmigration.ActivationSelections()

type temporaryActivationRecord = longhornmigration.ActivationRecord

// decodeTemporaryActivationRecord shares the strict receipt parser with the
// read-only publication gate, while preserving this operation's error boundary.
func decodeTemporaryActivationRecord(data []byte) (temporaryActivationRecord, error) {
	record, err := longhornmigration.DecodeActivationRecord(data)
	if err != nil {
		return temporaryActivationRecord{}, errTemporaryActivation
	}
	return record, nil
}
