package main

// holdReason exposes only fixed guard categories. Unknown or wrapped errors
// cannot turn a future private transport diagnostic into public output.
func holdReason(err error) string {
	if err == nil {
		return "UNKNOWN"
	}
	switch err.Error() {
	case "protected main invocation unverified", "protected run identity unverified":
		return "INVOCATION_UNVERIFIED"
	case "current main unverified", "current main changed during read":
		return "CURRENT_SOURCE_UNVERIFIED"
	case "deployment run unavailable":
		return "DEPLOYMENT_RUN_UNAVAILABLE"
	case "deployment run identity unverified":
		return "DEPLOYMENT_RUN_UNVERIFIED"
	case "deployment census unavailable", "deployment status unavailable":
		return "DEPLOYMENT_READ_UNAVAILABLE"
	case "deployment identity unverified", "deployment acceptance unverified":
		return "DEPLOYMENT_ACCEPTANCE_UNVERIFIED"
	case "artifact verifier unavailable":
		return "ARTIFACT_VERIFIER_UNAVAILABLE"
	case "artifact response unverified":
		return "ARTIFACT_RESPONSE_UNVERIFIED"
	case "artifact identity unverified":
		return "ARTIFACT_IDENTITY_UNVERIFIED"
	case "deployed layers unverified", "deployed source unverified":
		return "LIVE_REVISION_UNVERIFIED"
	case "namespace retirement journal or lifecycle unverified":
		return "NAMESPACE_LIFECYCLE_UNVERIFIED"
	case "database identity unverified", "database availability unverified":
		return "DATABASE_UNVERIFIED"
	case "claim census unavailable", "volume census unavailable", "complete database storage join unverified", "physical storage source unverified":
		return "STORAGE_CENSUS_UNVERIFIED"
	case "pod census unavailable", "pod census incomplete", "pod identity incomplete", "database pod ownership differs", "retained claim consumer differs", "database pod availability unverified", "database pod coverage incomplete":
		return "CLAIM_CONSUMERS_UNVERIFIED"
	case "sample separation failed", "initial separated samples differ", "pre-write state changed", "pre-write target missing":
		return "PRE_WRITE_SAMPLE_UNVERIFIED"
	case "only Delete to Retain is permitted", "volume patch outcome unverified; no retry", "volume patch readback differs; no retry":
		return "PATCH_OUTCOME_UNVERIFIED"
	case "post-write state unverified; protections retained", "physical retention readback unverified", "final separated samples differ":
		return "RETENTION_READBACK_UNVERIFIED"
	case "result encoding failed", "terminal receipt incomplete; protections retained":
		return "TERMINAL_RECEIPT_UNVERIFIED"
	default:
		return "UNKNOWN"
	}
}
