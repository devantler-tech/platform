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
			expected := map[string][]string{"containers": {"verify"}, "initContainers": {}}
			if row.copy {
				expected["initContainers"] = []string{"copy"}
			}
			for key, names := range expected {
				containers, _ := value(row.pod, "spec", key).([]any)
				if len(containers) != len(names) {
					t.Fatalf("the Pod has the wrong number of %s", key)
				}
				for i, item := range containers {
					container, _ := item.(object)
					options := valueObject(container, "securityContext", "seLinuxOptions")
					if text(container, "name") != names[i] || options == nil || len(options) != 0 {
						t.Fatal("the expected container lacks its baseline SELinux options")
					}
				}
			}
		})
	}
}
