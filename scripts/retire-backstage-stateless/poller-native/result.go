package main

import "context"

// The published native adapter wraps termination deadlines and may join more
// than one failure. A deadline mixed with a storage/API failure is not proof
// that the negative control failed solely because objects remained present.
func onlyDeadline(err error) bool {
	if err == nil {
		return false
	}
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		children := e.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyDeadline(child) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return onlyDeadline(e.Unwrap())
	default:
		return err == context.DeadlineExceeded
	}
}

func resultMarker(err error) string {
	if err == nil {
		return "WAITED"
	}
	if onlyDeadline(err) {
		return "WAITED_TIMEOUT"
	}
	return "HOLD_UNINSTALL"
}
