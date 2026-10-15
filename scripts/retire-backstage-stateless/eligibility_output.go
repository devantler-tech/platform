package main

// eligibilityCategory translates only exact, locally authored refusal messages
// into fixed public categories. It neither changes eligibility nor prints the
// error: parser errors, wrapped messages and new failures remain unclassified.
func eligibilityCategory(err error) string {
	if err == nil {
		return "unclassified"
	}
	switch err.Error() {
	case "malformed hooks":
		return "release-hooks"
	case "release is not stable":
		return "release-state"
	case "unreviewed footprint", "incomplete footprint":
		return "release-footprint"
	case "persistent workload":
		return "workload-storage"
	case "unproven Helm ownership":
		return "release-ownership"
	case "incomplete Helm GET":
		return "release-resources"
	case "native wait profile uncharacterized":
		return "wait-profile"
	case "uninstall owner unverified", "cross-scope release":
		return "owner-profile"
	case "native uninstall finalizer absent":
		return "owner-finalizer"
	case "unobserved release generation":
		return "owner-observation"
	case "active Helm action":
		return "owner-active"
	case "unready uninstall owner":
		return "owner-readiness"
	case "unreleased uninstall owner":
		return "owner-release"
	case "controller and stored release history differ":
		return "release-history"
	case "chart is terminating":
		return "chart-termination"
	case "chart ownership unproven", "chart has a foreign owner":
		return "chart-binding"
	case "chart has another consumer":
		return "chart-consumer"
	case "deployed source unverified":
		return "deployment-source"
	case "unsettled inventory":
		return "deployment-inventory"
	case "missing inventory":
		return "missing-inventory"
	case "source still owns target":
		return "source-ownership"
	case "applications source unverified":
		return "applications-source"
	default:
		return "unclassified"
	}
}
