package main

import "strings"

const nativeRestoreReclaimOperation = "reclaim-native-v2-seed"
const nativeRestoreReclaimConfirmation = "reclaim-longhorn-native-v2-seed:"
const nativeRestoreReclaimResume = "resume-native-v2-reclaim"
const nativeRestoreReclaimResumeConfirmation = "resume-longhorn-native-v2-reclaim:"

func authorizedNativeRestoreReclaimRun(getenv func(string) string) (string, error) {
	if getenv("LONGHORN_V2_NODE") != "" || getenv("LONGHORN_V2_DATABASE") != "" {
		return "", errNativeRestoreEvidence
	}
	var original, expected string
	switch getenv("LONGHORN_V2_OPERATION") {
	case nativeRestoreReclaimOperation:
		original, _ = strings.CutPrefix(getenv("LONGHORN_V2_CONFIRM"), nativeRestoreReclaimConfirmation)
		expected = nativeRestoreReclaimConfirmation + original
	case nativeRestoreReclaimResume:
		var failed string
		original, failed, _ = nativeRestoreReclaimResumeRuns(getenv)
		expected = nativeRestoreReclaimResumeConfirmation + original + ":" + failed
	default:
		return "", errNativeRestoreEvidence
	}
	if !validBenchmarkOwner(original, getenv("GITHUB_SHA")) || original == getenv("GITHUB_RUN_ID") {
		return "", errNativeRestoreEvidence
	}
	if err := authorizeWorkflow(getenv, expected); err != nil {
		return "", err
	}
	return original, nil
}

func nativeRestoreReclaimResumeRuns(getenv func(string) string) (original, failed string, valid bool) {
	body, prefixed := strings.CutPrefix(getenv("LONGHORN_V2_CONFIRM"), nativeRestoreReclaimResumeConfirmation)
	original, failed, split := strings.Cut(body, ":")
	if !prefixed || !split || !validBenchmarkOwner(original, getenv("GITHUB_SHA")) ||
		!validBenchmarkOwner(failed, getenv("GITHUB_SHA")) || failed == original || failed == getenv("GITHUB_RUN_ID") {
		return "", "", false
	}
	return original, failed, true
}
