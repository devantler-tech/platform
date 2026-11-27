package main

import "errors"

// verifyCNPGBackupBarrier checks ordering after the fresh backup binding.
func verifyCNPGBackupBarrier(backup object, barrier postgresState) error {
	refused := errors.New("recovery target does not follow the completed backup on the current physical history")
	if text(backup, "metadata", "uid") == "" || text(backup, "status", "phase") != "completed" || validPostgresState(barrier) != nil || barrier.InRecovery || text(backup, "status", "pluginMetadata", "timeline") != barrier.Timeline {
		return refused
	}
	begin, err := walPosition(text(backup, "status", "beginLSN"))
	if err != nil {
		return refused
	}
	end, err := walPosition(text(backup, "status", "endLSN"))
	if err != nil {
		return refused
	}
	position, err := walPosition(barrier.FlushLSN)
	if err != nil || begin > end || end > position {
		return refused
	}
	return nil
}
