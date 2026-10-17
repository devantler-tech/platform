package main

import "errors"

type preservationFailureError struct {
	category string
	cause    error
}

func (e *preservationFailureError) Error() string { return e.cause.Error() }
func (e *preservationFailureError) Unwrap() error { return e.cause }

func classifiedPreservationFailure(category string, err error) error {
	if err == nil {
		return nil
	}
	return &preservationFailureError{category: category, cause: err}
}

// preservationCategory exposes only a reviewed, coarse failure family. The
// underlying error can contain object details and therefore remains private.
func preservationCategory(err error) string {
	var classified *preservationFailureError
	if errors.As(err, &classified) {
		switch classified.category {
		case "namespace",
			"evidence-shape",
			"retirement-targets",
			"retirement-dependency",
			"database-health",
			"retained-identity",
			"release-history",
			"claim-ownership",
			"controller-evidence",
			"database-coverage",
			"storage-retention":
			return classified.category
		}
	}
	return "unknown"
}
