package main

import "testing"

func TestCorootKeeperPodSpecsIncludeBaselineAdmissionDefaults(t *testing.T) {
	const source = "39504b81e7cf68033f608ababf2e77983e88c42c"
	pods := []struct {
		pod  object
		copy bool
	}{
		{pod: corootKeeperStagePod("901", source, "fixture-worker")},
		{pod: corootKeeperCopyPod("901", source, "source-claim", "fixture-worker"), copy: true},
	}
	for _, row := range pods {
		name := text(row.pod, "metadata", "name")
		t.Run(name, func(t *testing.T) {
			context := valueObject(row.pod, "spec", "securityContext")
			if context["fsGroupChangePolicy"] != "OnRootMismatch" || row.copy && context["fsGroup"] != nil {
				t.Fatal("baseline mutation or volume permission change would alter the reviewed specification")
			}
			for _, key := range []string{"containers", "initContainers"} {
				containers, _ := value(row.pod, "spec", key).([]any)
				for _, item := range containers {
					container, _ := item.(object)
					options := valueObject(container, "securityContext", "seLinuxOptions")
					if options == nil || len(options) != 0 {
						t.Fatal("container has no empty SELinux options for the baseline admission policy")
					}
				}
			}
		})
	}
}
