package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestHostRecoveryRefreshesTheNodeAfterExpensiveProofs(t *testing.T) {
	for _, cordoned := range []bool{true, false} {
		t.Run(fmt.Sprintf("cordoned-%t", cordoned), func(t *testing.T) {
			f, h := hostRebootAPI(t)
			updates := 0
			f.afterDatabaseRead = func() {
				if f.rebooted && (value(f.base.node, "spec", "unschedulable") == true) == cordoned {
					updates++
					f.base.node["metadata"].(object)["resourceVersion"] = fmt.Sprint(200 + updates)
				}
			}
			if err := h.run(t.Context()); err != nil || f.reboots != 1 || updates == 0 {
				t.Fatal("an unrelated Node update stranded recovery:", err)
			}
		})
	}
}

func returningHostRecoveryFixture(t *testing.T) (*hostRebootFixture, hostMemoryReboot) {
	t.Helper()
	f, h := hostRebootAPI(t)
	r, err := h.initialize(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.rebooted, f.reboots, f.drains = true, 1, 1
	r.Phase, r.RecoveredBootID = "returning", "12345678-1234-4234-8234-000000000099"
	f.base.node["status"].(object)["nodeInfo"].(object)["bootID"] = r.RecoveredBootID
	f.base.node["status"].(object)["capacity"] = object{"hugepages-2Mi": "2Gi"}
	f.base.node["status"].(object)["allocatable"] = object{"hugepages-2Mi": "2Gi", "memory": "12932Mi"}
	f.base.node["spec"].(object)["unschedulable"] = true
	f.base.node["metadata"].(object)["annotations"].(object)[fenceAnnotation] = h.name()
	cm, _, err := h.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = h.write(t.Context(), cm, r); err != nil {
		t.Fatal(err)
	}
	return f, h
}

func TestHostRecoveryFreshNodeStillRequiresItsCompleteIdentity(t *testing.T) {
	changes := map[string]func(object){
		"uid":      func(n object) { n["metadata"].(object)["uid"] = "replacement" },
		"provider": func(n object) { n["spec"].(object)["providerID"] = "hcloud://999" },
		"hardware": func(n object) { n["status"].(object)["nodeInfo"].(object)["systemUUID"] = "replacement" },
		"boot": func(n object) {
			n["status"].(object)["nodeInfo"].(object)["bootID"] = "12345678-1234-4234-8234-000000000098"
		},
		"os":           func(n object) { n["status"].(object)["nodeInfo"].(object)["osImage"] = "replacement" },
		"architecture": func(n object) { n["status"].(object)["nodeInfo"].(object)["architecture"] = "arm64" },
		"address": func(n object) {
			n["status"].(object)["addresses"] = []any{object{"type": "InternalIP", "address": "10.0.99.99"}}
		},
		"fence":    func(n object) { n["metadata"].(object)["annotations"].(object)[fenceAnnotation] = "foreign" },
		"pages":    func(n object) { n["status"].(object)["capacity"].(object)["hugepages-2Mi"] = "0" },
		"deleting": func(n object) { n["metadata"].(object)["deletionTimestamp"] = "2026-11-15T00:00:00Z" },
		"pressure": func(n object) {
			for _, raw := range value(n, "status", "conditions").([]any) {
				condition := raw.(object)
				if text(condition, "type") == "MemoryPressure" {
					condition["status"] = "True"
				}
			}
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			f, h := returningHostRecoveryFixture(t)
			f.afterDatabaseRead = func() { change(f.base.node) }
			writes := 0
			h.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == "kubectl" && len(args) > 3 && args[2] == "patch" && args[3] == "node" {
					writes++
					return nil, errors.New("synthetic private command detail")
				}
				return f.run(ctx, input, program, args...)
			}
			if err := h.run(t.Context()); err == nil || writes != 0 || f.reboots != 1 {
				t.Fatal("changed live identity reached a scheduling write:", err, writes)
			}
		})
	}
}

func TestHostRecoveryDoesNotConfirmAnUncommittedWriteOrPublishCommandDetails(t *testing.T) {
	for _, stage := range []string{"journal-returning", "uncordon", "remove-fence", "journal-recovered"} {
		t.Run(stage, func(t *testing.T) {
			f, h := hostRebootAPI(t)
			writes := 0
			h.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if recoveryWriteAttemptMatches(f, input, program, args, stage) {
					writes++
					return nil, errors.New("synthetic private command detail")
				}
				return f.run(ctx, input, program, args...)
			}
			err := h.run(t.Context())
			_, record, readErr := h.read(t.Context())
			if err == nil || writes != 1 || readErr != nil || record.Phase == "recovered" || strings.Contains(err.Error(), "private command detail") {
				t.Fatal("an uncommitted write was accepted, retried, or exposed:", err, writes)
			}
		})
	}
}

func recoveryWriteAttemptMatches(f *hostRebootFixture, input []byte, program string, args []string, stage string) bool {
	if program != "kubectl" || len(args) < 4 || args[2] != "patch" || !f.rebooted {
		return false
	}
	if args[3] != "configmap" {
		return hostRecoveryWriteMatches(f, input, program, args, stage)
	}
	var patches []object
	var record hostRebootRecord
	return json.Unmarshal(input, &patches) == nil && len(patches) == 3 &&
		json.Unmarshal([]byte(text(patches[2], "value")), &record) == nil && stage == "journal-"+record.Phase
}

func TestHostRecoveryRejectsContradictoryOrUnreadableWriteReadback(t *testing.T) {
	for _, stage := range []string{"journal-returning", "uncordon", "remove-fence"} {
		faults := []string{"replacement", "unreadable", "ownership", "main", "journal-deleting"}
		if stage != "journal-returning" {
			faults = append(faults, "journal-replacement", "node-unreadable")
		}
		for _, fault := range faults {
			t.Run(stage+"/"+fault, func(t *testing.T) {
				f, h := hostRebootAPI(t)
				writes := 0
				sentinel := errors.New("synthetic private command detail")
				h.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
					if writes > 0 && fault == "unreadable" && program == "kubectl" && len(args) > 3 && args[2] == "get" && args[3] == "configmap" {
						return nil, sentinel
					}
					if writes > 0 && fault == "node-unreadable" && program == "kubectl" && len(args) > 3 && args[2] == "get" && args[3] == "node" {
						return nil, sentinel
					}
					output, err := f.run(ctx, input, program, args...)
					if err != nil || !hostRecoveryWriteMatches(f, input, program, args, stage) {
						return output, err
					}
					writes++
					switch fault {
					case "replacement":
						changed := f.base.cm
						if stage != "journal-returning" {
							changed = f.base.node
						}
						changed["metadata"].(object)["uid"] = "replacement"
					case "ownership":
						var record hostRebootRecord
						if json.Unmarshal([]byte(text(f.base.cm, "data", "record")), &record) != nil {
							t.Fatal("invalid fixture record")
						}
						record.OwnerRun = "9999"
						body, marshalErr := json.Marshal(record)
						if marshalErr != nil {
							t.Fatal(marshalErr)
						}
						f.base.cm["data"].(object)["record"] = string(body)
					case "journal-replacement":
						f.base.cm["metadata"].(object)["uid"] = "replacement"
					case "journal-deleting":
						f.base.cm["metadata"].(object)["deletionTimestamp"] = "2026-11-15T00:00:00Z"
					case "main":
						f.base.staleSource = true
					}
					return nil, sentinel
				}
				err := h.run(t.Context())
				if err == nil || !errors.Is(err, sentinel) || strings.Contains(err.Error(), "private command detail") || writes != 1 || f.reboots != 1 {
					t.Fatal("contradictory readback completed, repeated, or exposed recovery:", err, writes)
				}
			})
		}
	}
}

func TestHostRecoveryConfirmsLostWriteResponsesWithoutAnotherWrite(t *testing.T) {
	for _, stage := range []string{"journal-returning", "uncordon", "remove-fence", "journal-recovered"} {
		t.Run(stage, func(t *testing.T) {
			f, h := hostRebootAPI(t)
			writes := 0
			h.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
				output, err := f.run(ctx, input, program, args...)
				if err != nil || !hostRecoveryWriteMatches(f, input, program, args, stage) {
					return output, err
				}
				writes++
				return nil, errors.New("synthetic private command detail")
			}
			if err := h.run(t.Context()); err != nil || f.reboots != 1 || f.drains != 1 || writes != 1 {
				t.Fatal("an acknowledged-by-readback write was replayed or held:", err, writes)
			}
			_, record, err := h.read(t.Context())
			if err != nil || record.Phase != "recovered" || value(f.base.node, "spec", "unschedulable") == true || text(f.base.node, "metadata", "annotations", fenceAnnotation) != "" {
				t.Fatal("recovery completed without its final scheduling and journal proofs:", err)
			}
		})
	}
}

func hostRecoveryWriteMatches(f *hostRebootFixture, input []byte, program string, args []string, stage string) bool {
	if program != "kubectl" || len(args) < 4 || args[2] != "patch" || !f.rebooted {
		return false
	}
	if args[3] == "configmap" {
		var record hostRebootRecord
		return json.Unmarshal([]byte(text(f.base.cm, "data", "record")), &record) == nil && stage == "journal-"+record.Phase
	}
	if args[3] != "node" {
		return false
	}
	var patches []object
	if json.Unmarshal(input, &patches) != nil || len(patches) != 3 {
		f.base.t.Fatal("recovery mutation lost its fixed conditional scope")
	}
	return stage == "uncordon" && text(patches[2], "path") == "/spec/unschedulable" && patches[2]["value"] == false ||
		stage == "remove-fence" && text(patches[2], "op") == "remove"
}
