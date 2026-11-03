package main

import "errors"

// authorizeTemporaryPreparation restricts the destructive profile to its two
// reviewed aliases and requires the selection-specific protected-workflow token.
func authorizeTemporaryPreparation(getenv func(string) string, selection string) error {
	if (selection != "temporary-v1-1" && selection != "temporary-v1-2") || getenv("LONGHORN_V2_NODE") != selection || getenv("LONGHORN_V2_OPERATION") != "prepare-unused-temporary-worker" {
		return errors.New("the separate unused-temporary-worker preparation selection is required")
	}
	return authorizeWorkflow(getenv, "prepare-unused-longhorn-v2-"+selection)
}
