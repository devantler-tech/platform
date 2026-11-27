package main

import "testing"

func TestKeeperSourceDataMountCannotBeShadowed(t *testing.T) {
	const root = "/var/lib/clickhouse-keeper"
	data := object{"name": "data", "mountPath": root}
	testCases := []struct {
		name   string
		mounts []any
		want   bool
	}{
		{"data and unrelated configuration", []any{data, object{"name": "configuration", "mountPath": "/etc/clickhouse-keeper"}}, true},
		{"similarly named sibling", []any{data, object{"name": "scratch", "mountPath": root + "-scratch"}}, true},
		{"shadowed coordination directory", []any{data, object{"name": "scratch", "mountPath": root + "/coordination"}}, false},
		{"shadowed snapshot file", []any{data, object{"name": "scratch", "mountPath": root + "/coordination/snapshots/current"}}, false},
		{"shadowed parent", []any{data, object{"name": "scratch", "mountPath": "/var/lib"}}, false},
		{"shadowed root", []any{data, object{"name": "scratch", "mountPath": "/"}}, false},
		{"duplicate data mount", []any{data, object{"name": "scratch", "mountPath": root + "/."}}, false},
		{"data subpath", []any{object{"name": "data", "mountPath": root, "subPath": "coordination"}}, false},
		{"missing data mount", []any{object{"name": "scratch", "mountPath": "/tmp"}}, false},
	}
	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			pod := object{"spec": object{"containers": []any{
				object{"name": corootKeeperContainer, "volumeMounts": tt.mounts},
			}}}
			if got := corootKeeperSourceDataMount(pod); got != tt.want {
				t.Fatalf("source layout accepted = %t, want %t", got, tt.want)
			}
		})
	}
}
