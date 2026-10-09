package arcstaging_test

import "testing"

const metricsComponent = "k8s/bases/infrastructure/actions-runners"

func TestRunnerRetainsWholeJobCgroupMeasurement(t *testing.T) {
	config := readYAML(t, metricsComponent+"/config-map-job-metrics.yaml")
	equal(t, field(t, config, "metadata", "namespace"), "arc-runners")
	equal(t, field(t, config, "immutable"), true)
	equal(t, field(t, config, "metadata", "annotations", "kustomize.toolkit.fluxcd.io/substitute"), "disabled")
	release := readYAML(t, metricsComponent+"/helm-release.yaml")
	runner := field(t, release, "spec", "values", "template", "spec", "containers").([]any)[0]
	env := field(t, runner, "env").([]any)
	equal(t, len(env), 1)
	equal(t, field(t, env[0], "name"), "ACTIONS_RUNNER_HOOK_JOB_COMPLETED")
	equal(t, field(t, env[0], "value"), "/etc/ksail-arc-metrics/job-metrics.sh")
}
