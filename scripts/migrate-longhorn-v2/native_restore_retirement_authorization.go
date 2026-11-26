package main

import "strings"

const nativeRestoreReclaimOperation = "reclaim-native-v2-seed"
const nativeRestoreReclaimConfirmation = "reclaim-longhorn-native-v2-seed:"

func authorizedNativeRestoreReclaimRun(getenv func(string) string) (string, error) {
	if getenv("LONGHORN_V2_OPERATION") != nativeRestoreReclaimOperation ||
		getenv("LONGHORN_V2_NODE") != "" || getenv("LONGHORN_V2_DATABASE") != "" {
		return "", errNativeRestoreEvidence
	}
	ownerRun, found := strings.CutPrefix(getenv("LONGHORN_V2_CONFIRM"), nativeRestoreReclaimConfirmation)
	if !found || !validBenchmarkOwner(ownerRun, getenv("GITHUB_SHA")) || ownerRun == getenv("GITHUB_RUN_ID") {
		return "", errNativeRestoreEvidence
	}
	if err := authorizeWorkflow(getenv, nativeRestoreReclaimConfirmation+ownerRun); err != nil {
		return "", err
	}
	return ownerRun, nil
}
