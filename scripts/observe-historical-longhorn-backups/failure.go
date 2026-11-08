package main

import "errors"

type observationFailure struct{ phase string }

func (e observationFailure) Error() string { return errHeld.Error() + "; stage=" + e.phase }
func (e observationFailure) Unwrap() error { return errHeld }

func failureStage(err error) string {
	var failure observationFailure
	if errors.As(err, &failure) {
		return failure.phase
	}
	return "unspecified"
}
