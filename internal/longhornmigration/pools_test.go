package longhornmigration

import (
	"strings"
	"testing"
)

func sourceFixture() []byte {
	return []byte(`{"spec":{"cluster":{"controlPlanes":3,"workers":3,"autoscaler":{"node":{"maxNodesTotal":9,"pools":[{"name":"autoscale-longhorn-v1","serverType":"cpx42","location":"fsn1","min":2,"max":2,"labels":{"storage.platform.devantler.tech/migration-pool":"v1"}},{"name":"autoscale-longhorn-v2","serverType":"cpx42","location":"fsn1","min":1,"max":1,"labels":{"storage.platform.devantler.tech/migration-pool":"v2"}}]}}},"provider":{"hetzner":{"serverLimit":10}}}}`)
}

func TestConfiguredPoolsRefuseChangedHardwareOrLocation(t *testing.T) {
	for _, change := range [][2]string{{`"serverType":"cpx42"`, `"serverType":"cpx52"`}, {`"location":"fsn1"`, `"location":"nbg1"`}} {
		for _, name := range []string{"autoscale-longhorn-v1", "autoscale-longhorn-v2"} {
			prefix := `"name":"` + name + `",`
			source := strings.Replace(string(sourceFixture()), prefix+`"serverType":"cpx42","location":"fsn1"`, prefix+strings.Replace(`"serverType":"cpx42","location":"fsn1"`, change[0], change[1], 1), 1)
			if source == string(sourceFixture()) {
				t.Fatalf("fixture for %s did not change", name)
			}
			if _, err := ConfiguredPools([]byte(source)); err == nil {
				t.Fatalf("%s accepted %s", name, change[1])
			}
		}
	}
}
