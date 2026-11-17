package main

import (
	"errors"
	"time"
)

// canaryRun retains the native Kopia repository across the two protected phases.
type canaryRun struct{ kopiaRepository object }

// Velero v1.18 omits zero warning/error counters. A missing completion or an
// explicit warning still cannot establish a complete native data transfer.
func cleanVeleroResult(o object) error {
	if text(o, "metadata", "uid") == "" || value(o, "metadata", "deletionTimestamp") != nil || text(o, "status", "phase") != "Completed" {
		return errors.New("native backup or restore has not completed")
	}
	if _, err := time.Parse(time.RFC3339, text(o, "status", "completionTimestamp")); err != nil {
		return errors.New("native backup or restore completion is missing")
	}
	for _, field := range []string{"errors", "warnings"} {
		if count := value(o, "status", field); count != nil && count != float64(0) {
			return errors.New("native backup or restore reported warnings or errors")
		}
	}
	return nil
}
