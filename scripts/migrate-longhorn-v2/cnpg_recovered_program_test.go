package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Synthetic declaration derived from CNPG 1.30.2 specs.{pods,containers,volumes}.
// It is not native Pod admission, filesystem or recovery acceptance evidence.
func addRecoveredProgramFixture(pod object) {
	spec := value(pod, "spec").(object)
	mounts := []any{
		object{"name": "pgdata", "mountPath": "/var/lib/postgresql/data"},
		object{"name": "scratch-data", "mountPath": "/run"},
		object{"name": "scratch-data", "mountPath": "/controller"},
		object{"name": "shm", "mountPath": "/dev/shm"},
	}
	postgres := value(pod, "spec", "containers").([]any)[0].(object)
	postgres["command"] = []any{"/controller/manager", "instance", "run", "--status-port-tls", "--log-level=info"}
	postgres["volumeMounts"] = mounts
	postgres["env"] = []any{
		object{"name": "PGDATA", "value": "/var/lib/postgresql/data/pgdata"},
		object{"name": "POD_NAME", "value": text(pod, "metadata", "name")},
		object{"name": "NAMESPACE", "value": text(pod, "metadata", "namespace")},
		object{"name": "CLUSTER_NAME", "value": "restore"},
		object{"name": "PSQL_HISTORY", "value": "/controller/tmp/.psql_history"},
		object{"name": "PGPORT", "value": "5432"},
		object{"name": "PGHOST", "value": "/controller/run"},
		object{"name": "TMPDIR", "value": "/controller/tmp"},
	}
	spec["initContainers"] = []any{object{"name": "bootstrap-controller", "image": "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.2", "command": []any{"/manager", "bootstrap", "/controller/manager", "--log-level=info"}, "volumeMounts": mounts}}
	spec["volumes"] = append(spec["volumes"].([]any), object{"name": "scratch-data", "emptyDir": object{}}, object{"name": "shm", "emptyDir": object{"medium": "Memory"}})
	value(pod, "status").(object)["initContainerStatuses"] = []any{object{
		"name": "bootstrap-controller", "imageID": "ghcr.io/cloudnative-pg/cloudnative-pg@sha256:7172ea6acc90b0af204e513ee7b5f05fb04a44dcf96a8666655de88511758d7f", "containerID": "containerd://" + strings.Repeat("c", 64), "restartCount": float64(0),
		"state": object{"terminated": object{"exitCode": float64(0), "startedAt": "2026-11-12T00:00:00Z", "finishedAt": "2026-11-12T00:00:01Z"}},
	}}
}

func TestRecoveredSQLRejectsProducerSubstitutionInsideItsReadBracket(t *testing.T) {
	for _, scenario := range []string{"before command", "before init image", "before restartable init", "after init container"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRecoveredStorageFixture(t)
			if scenario == "before restartable init" {
				value(f.pod, "spec", "initContainers").([]any)[0].(object)["restartPolicy"] = "Always"
			}
			reads, executions := 0, 0
			c := client{context: "admin@prod", run: func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
				if program != "kubectl" || len(args) < 3 {
					t.Fatal("unexpected recovered read boundary")
				}
				if args[2] == "get" {
					reads++
					pod, err := cloneRecoverySpec(f.pod)
					if err != nil {
						t.Fatal(err)
					}
					switch {
					case scenario == "before command":
						value(pod, "spec", "containers").([]any)[0].(object)["command"] = []any{"/bin/sh", "-c", "sleep 3600"}
					case scenario == "before init image":
						value(pod, "status", "initContainerStatuses").([]any)[0].(object)["imageID"] = "sha256:" + strings.Repeat("f", 64)
					case scenario == "before restartable init":
						value(pod, "spec", "initContainers").([]any)[0].(object)["restartPolicy"] = "Always"
					case scenario == "after init container" && reads == 2:
						value(pod, "status", "initContainerStatuses").([]any)[0].(object)["containerID"] = "containerd://" + strings.Repeat("d", 64)
					}
					return json.Marshal(pod)
				}
				if args[2] != "exec" {
					t.Fatal("unexpected recovered read operation")
				}
				executions++
				return []byte("{}"), nil
			}}
			got, err := c.readRecoveredPostgres(t.Context(), f.cluster, f.pod)
			wantExecutions := 0
			if scenario == "after init container" {
				wantExecutions = 1
			}
			if err == nil || len(got) != 0 || executions != wantExecutions {
				t.Fatal("recovered SQL used or accepted a substituted executable producer")
			}
		})
	}
}

func TestRecoveredStorageRejectsAnUnboundExecutableProducer(t *testing.T) {
	baseline := newRecoveredStorageFixture(t)
	for name, mutate := range map[string]func(object){
		"missing command": func(p object) { delete(value(p, "spec", "containers").([]any)[0].(object), "command") },
		"different command": func(p object) {
			value(p, "spec", "containers").([]any)[0].(object)["command"] = []any{"/bin/sh", "-c", "sleep 3600"}
		},
		"extra args": func(p object) { value(p, "spec", "containers").([]any)[0].(object)["args"] = []any{"--unreviewed"} },
		"environment override": func(p object) {
			c := value(p, "spec", "containers").([]any)[0].(object)
			c["env"] = append(c["env"].([]any), object{"name": "LD_PRELOAD", "value": "/controller/other.so"})
		},
		"missing producer":     func(p object) { delete(value(p, "spec").(object), "initContainers") },
		"restartable producer": func(p object) { value(p, "spec", "initContainers").([]any)[0].(object)["restartPolicy"] = "Always" },
		"producer restart rules": func(p object) {
			value(p, "spec", "initContainers").([]any)[0].(object)["restartPolicyRules"] = []any{object{"action": "Restart", "exitCodes": object{"operator": "In", "values": []any{float64(0)}}}}
		},
		"different producer": func(p object) { value(p, "spec", "initContainers").([]any)[0].(object)["image"] = "other:latest" },
		"producer digest": func(p object) {
			value(p, "status", "initContainerStatuses").([]any)[0].(object)["imageID"] = "sha256:" + strings.Repeat("f", 64)
		},
		"producer still running": func(p object) {
			value(p, "status", "initContainerStatuses").([]any)[0].(object)["state"] = object{"running": object{"startedAt": "2026-11-12T00:00:00Z"}}
		},
		"unobserved producer retry": func(p object) {
			value(p, "status", "initContainerStatuses").([]any)[0].(object)["restartCount"] = float64(1)
		},
		"executable shadow mount": func(p object) {
			c := value(p, "spec", "containers").([]any)[0].(object)
			c["volumeMounts"] = append(c["volumeMounts"].([]any), object{"name": "foreign", "mountPath": "/controller/manager"})
		},
		"producer different scratch": func(p object) {
			value(p, "spec", "initContainers").([]any)[0].(object)["volumeMounts"] = []any{object{"name": "foreign", "mountPath": "/controller"}}
		},
		"scratch substitution": func(p object) { value(p, "spec", "volumes").([]any)[1].(object)["emptyDir"] = nil },
		"additional init producer": func(p object) {
			value(p, "spec").(object)["initContainers"] = append(value(p, "spec", "initContainers").([]any), object{"name": "other", "image": "other:latest"})
		},
		"additional sidecar": func(p object) {
			value(p, "spec").(object)["containers"] = append(value(p, "spec", "containers").([]any), object{"name": "other", "image": "other:latest"})
		},
		"ephemeral executable writer": func(p object) {
			value(p, "spec").(object)["ephemeralContainers"] = []any{object{"name": "other", "image": "other:latest"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := baseline.copyBindingObjects(t)
			mutate(f.pod)
			if got, err := f.bind(); err == nil || !reflect.DeepEqual(got, cnpgRecoveredStorage{}) {
				t.Fatal("an unbound executable producer produced storage evidence")
			}
		})
	}
}
