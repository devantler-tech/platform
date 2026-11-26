package main

import (
	"context"
	"errors"
)

// qualifyDatabaseStorage re-observes native disks and complete allocations; a
// previously recorded activation is a binding to compare, never cached clearance.
func (p *cnpgProofRun) qualifyDatabaseStorage(ctx context.Context) (cnpgStorageEnvironment, error) {
	owner, err := p.executionOwner()
	if err != nil || p.record.Plan == nil || p.storage.source != owner.Source || p.storage.owner != owner.Run || p.storage.c.context != p.c.context {
		return cnpgStorageEnvironment{}, errors.New("database storage lacks the current proof owner")
	}
	current, err := p.storage.recoveryStorage(ctx, p.record.Plan.Source)
	if err != nil {
		return cnpgStorageEnvironment{}, err
	}
	if p.record.Storage != nil && !recoveryJSONEqual(*p.record.Storage, current) {
		return cnpgStorageEnvironment{}, errors.New("database storage identity changed after intent")
	}
	return current, nil
}
