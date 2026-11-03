package backupcatalogue

import "errors"

// observationFailure carries only a fixed phase. The underlying provider error
// is deliberately discarded because it may contain credentials or object keys.
type observationFailure struct{ phase string }

func (e observationFailure) Error() string { return errReadHeld.Error() + "; stage=" + e.phase }
func (e observationFailure) Unwrap() error { return errReadHeld }

// FailureStage returns a fixed phase for public failure diagnostics. It never
// interprets or serializes an arbitrary error's text.
func FailureStage(err error) string {
	var failure observationFailure
	if errors.As(err, &failure) {
		return failure.phase
	}
	return "observation"
}
