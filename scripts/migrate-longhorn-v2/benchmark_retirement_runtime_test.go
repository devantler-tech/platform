package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

const testRetirementImage = "ghcr.io/devantler-tech/platform-longhorn-instance-manager:v1.13.0-retirement.1-591ed0ccebd032c4d9705c356c267e724b30e2a9-1@sha256:4f7c2ff5666c6ce1ccab78a5e8c5c108147b12b80729303b9f1d91dfb9ee9cd7"

func TestBenchmarkV1RuntimeReceiptRequiresOriginalDistinctEpochs(t *testing.T) {
	for _, fault := range []string{"", "missing", "wrong image", "old published image", "digest-only declaration", "unreviewed release tag", "unreviewed digest", "reused container", "reused UID", "missing worker", "missing start", "unexpected V2"} {
		t.Run(fault, func(t *testing.T) {
			record := recoveryFixture(t)
			if err := validateBenchmarkRuntimeReceipt(record); err != nil {
				t.Fatalf("legacy journals must remain readable: %v", err)
			}
			record.Version = 3
			runtime := &benchmarkV1Runtime{Image: testRetirementImage, SettingUID: "setting-uid", SettingVersion: "17"}
			for _, index := range []string{"1", "2", "3"} {
				runtime.Epochs = append(runtime.Epochs, benchmarkManagerEpoch{
					Pod: "instance-manager-" + index, PodUID: "pod-" + index, ManagerUID: "manager-" + index,
					Node: "prod-worker-" + index, ContainerID: "containerd://" + strings.Repeat(index, 64),
					StartedAt: time.Date(2026, 11, 10, 1, 2, 3, 0, time.UTC),
				})
			}
			record.V1Runtime = runtime
			switch fault {
			case "missing":
				record.V1Runtime = nil
			case "wrong image":
				runtime.Image = "longhornio/longhorn-instance-manager:v1.13.0"
			case "old published image":
				runtime.Image = "ghcr.io/devantler-tech/platform-longhorn-instance-manager:v1.13.0-retirement.1-725bb1d09517c84f8b73ff2c0cf175421df351f1-1@sha256:93cfc83d8bab91f9b6fbffe20bf9bdaca28e877d91d7b740cae003bf6853059f"
			case "digest-only declaration":
				_, digest, _ := strings.Cut(testRetirementImage, "@")
				runtime.Image = "ghcr.io/devantler-tech/platform-longhorn-instance-manager@" + digest
			case "unreviewed release tag":
				runtime.Image = strings.Replace(testRetirementImage, "591ed0ccebd032c4d9705c356c267e724b30e2a9", strings.Repeat("b", 40), 1)
			case "unreviewed digest":
				ref, _, _ := strings.Cut(testRetirementImage, "@")
				runtime.Image = ref + "@sha256:" + strings.Repeat("a", 64)
			case "reused container":
				runtime.Epochs[1].ContainerID = runtime.Epochs[0].ContainerID
			case "reused UID":
				runtime.Epochs[1].ManagerUID = runtime.Epochs[0].PodUID
			case "missing worker":
				runtime.Epochs[2].Node = "different-worker"
			case "missing start":
				runtime.Epochs[2].StartedAt = time.Time{}
			case "unexpected V2":
				record.Phase = "v2"
			}
			if err := validateBenchmarkRuntimeReceipt(record); (err == nil) != (fault == "") {
				t.Fatalf("runtime receipt validation did not match %q: %v", fault, err)
			}
		})
	}
}

func TestObserveBenchmarkV1RuntimeRequiresStableDefaultImage(t *testing.T) {
	for _, scenario := range []string{"stable", "stock default", "changed default epoch"} {
		t.Run(scenario, func(t *testing.T) {
			reads := 0
			c := client{context: "admin@prod", run: func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
				if program != "kubectl" || args[2] != "get" {
					t.Fatal("runtime observation attempted a mutation")
				}
				switch args[3] {
				case "settings.longhorn.io":
					reads++
					image, version := testRetirementImage, "1"
					if scenario == "stock default" {
						image = "longhornio/longhorn-instance-manager:v1.13.0"
					}
					if scenario == "changed default epoch" && reads == 2 {
						version = "2"
					}
					return json.Marshal(object{
						"metadata": object{"name": "default-instance-manager-image", "namespace": "longhorn-system",
							"uid": "setting-uid", "resourceVersion": version},
						"value": image,
					})
				case "instancemanagers.longhorn.io":
					return json.Marshal(object{"items": []object{retirementManager("instance-manager-a", "worker-a", testRetirementImage)}})
				case "pods":
					return json.Marshal(object{"items": []object{retirementPod("instance-manager-a", "worker-a")}})
				default:
					t.Fatal("unexpected runtime observation")
					return nil, nil
				}
			}}
			observed, err := observeBenchmarkV1Runtime(t.Context(), c, testRetirementImage, []string{"worker-a"})
			if scenario == "stable" {
				if err != nil || reads != 2 || observed.SettingUID != "setting-uid" || len(observed.Epochs) != 1 {
					t.Fatalf("native runtime was not verified: %v", err)
				}
			} else if err == nil {
				t.Fatal("unverified default image qualified as the V1 runtime")
			}
		})
	}
}

func TestV1RetirementSealRequiresExactVolumeAndNativeCompletion(t *testing.T) {
	const claimUID = "a1b2c3d4-1234-5678-9abc-def012345678"
	for _, tc := range []struct {
		name, response string
		change         func(*benchmarkRecord)
		execError      error
		wantCall, ok   bool
	}{
		{name: "sealed", response: "SEALED\n", wantCall: true, ok: true},
		{name: "pending with success exit", response: "PENDING\n", wantCall: true},
		{name: "ambiguous output", response: "PENDING\nSEALED\n", wantCall: true},
		{name: "failed transport", response: "SEALED\n", execError: errors.New("unknown response"), wantCall: true},
		{name: "different backend", change: func(r *benchmarkRecord) {
			b := r.Resources["LonghornVolume"]
			b.Name = "pvc-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
			r.Resources["LonghornVolume"] = b
		}},
		{name: "V2 volume", change: func(r *benchmarkRecord) { r.Phase = "v2" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := retirementFenceFixture(t)
			pvc := record.Resources["PersistentVolumeClaim"]
			pvc.UID = claimUID
			record.Resources["PersistentVolumeClaim"] = pvc
			for _, kind := range []string{"PersistentVolume", "LonghornVolume"} {
				resource := record.Resources[kind]
				resource.Name = "pvc-" + claimUID
				record.Resources[kind] = resource
			}
			if tc.change != nil {
				tc.change(&record)
			}
			called := false
			c := client{context: "admin@prod", run: func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
				called = true
				want := []string{"--context=admin@prod", "--request-timeout=20s", "exec", "--namespace=longhorn-system",
					"instance-manager-a", "--container=instance-manager", "--", "/usr/local/bin/longhorn-instance-manager",
					"process", "seal-volume-for-retirement", "--volume", "pvc-" + claimUID}
				if program != "kubectl" || !slices.Equal(args, want) {
					t.Fatalf("seal invocation escaped the reviewed scope")
				}
				return []byte(tc.response), tc.execError
			}}
			err := requestBenchmarkV1Seal(t.Context(), c, record, benchmarkManagerEpoch{Pod: "instance-manager-a"})
			if (err == nil) != tc.ok || called != tc.wantCall {
				t.Fatalf("seal disposition: called=%t, error=%v", called, err)
			}
		})
	}
}

func TestQualifyV1RetirementManagerEpochs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*[]object, *[]object)
		ok     bool
	}{
		{name: "exact image and durable host mount", ok: true},
		{
			name: "stock runtime cannot satisfy a required node",
			change: func(managers, _ *[]object) {
				valueObject((*managers)[0], "spec")["image"] = "longhornio/longhorn-instance-manager:v1.13.0"
			},
		},
		{
			name: "mismatched running image",
			change: func(_, pods *[]object) {
				value((*pods)[0], "status", "containerStatuses").([]any)[0].(object)["imageID"] = "sha256:" + strings.Repeat("b", 64)
			},
		},
		{
			name: "unpaired runtime",
			change: func(_ *[]object, pods *[]object) {
				*pods = append(*pods, retirementPod("instance-manager-unpaired", "worker-a"))
			},
		},
		{
			name: "transient marker path",
			change: func(_, pods *[]object) {
				value((*pods)[0], "spec", "volumes").([]any)[0].(object)["hostPath"] = nil
			},
		},
		{
			name: "storage runtime identity not explicitly selected",
			change: func(_, pods *[]object) {
				container := value((*pods)[0], "spec", "containers").([]any)[0].(object)
				delete(valueObject(container, "securityContext"), "runAsUser")
			},
		},
		{
			name: "stock runtime group not explicitly selected",
			change: func(_, pods *[]object) {
				container := value((*pods)[0], "spec", "containers").([]any)[0].(object)
				delete(valueObject(container, "securityContext"), "runAsGroup")
			},
		},
		{
			name: "different controller UID",
			change: func(_, pods *[]object) {
				value((*pods)[0], "metadata", "ownerReferences").([]any)[0].(object)["uid"] = "replacement"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			managers := []object{
				retirementManager("instance-manager-a", "worker-a", testRetirementImage),
				retirementManager("instance-manager-b", "worker-b", testRetirementImage),
				retirementManager("instance-manager-old", "worker-a", "longhornio/longhorn-instance-manager:v1.13.0"),
			}
			pods := []object{
				retirementPod("instance-manager-a", "worker-a"),
				retirementPod("instance-manager-b", "worker-b"),
			}
			if tc.change != nil {
				tc.change(&managers, &pods)
			}

			epochs, err := qualifyV1RetirementManagerEpochs(testRetirementImage, []string{"worker-a", "worker-b"}, managers, pods)
			if tc.ok {
				if err != nil || len(epochs) != 2 || epochs[0].Pod != "instance-manager-a" || epochs[0].PodUID != "pod-instance-manager-a" {
					t.Fatalf("wrong active retirement epochs: %+v, %v", epochs, err)
				}
			} else if err == nil {
				t.Fatal("an ambiguous native runtime was admitted")
			}
		})
	}
}

func TestQualifyV1RetirementManagerTagAndDigest(t *testing.T) {
	const repository = "ghcr.io/devantler-tech/platform-longhorn-instance-manager"
	ref, digest, _ := strings.Cut(testRetirementImage, "@")
	running := repository + "@" + digest
	for _, tc := range []struct {
		name, declared, running string
		ok                      bool
	}{
		{"published tag with digest", testRetirementImage, "docker-pullable://" + running, true},
		{"tag retained by runtime", testRetirementImage, testRetirementImage, true},
		{"digest-only declaration", running, running, false},
		{"different release tag", strings.Replace(testRetirementImage, "591ed0ccebd032c4d9705c356c267e724b30e2a9", strings.Repeat("b", 40), 1), running, false},
		{"unreviewed digest", ref + "@sha256:" + strings.Repeat("a", 64), repository + "@sha256:" + strings.Repeat("a", 64), false},
		{"tag without digest", ref, testRetirementImage, false},
		{"unrecognized tag", repository + ":latest@" + digest, testRetirementImage, false},
		{"foreign image", testRetirementImage, repository + "-other@" + digest, false},
		{"different digest", testRetirementImage, repository + "@sha256:" + strings.Repeat("c", 64), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := retirementManager("instance-manager-a", "worker-a", tc.declared)
			pod := retirementPod("instance-manager-a", "worker-a")
			value(pod, "spec", "containers").([]any)[0].(object)["image"] = tc.declared
			value(pod, "status", "containerStatuses").([]any)[0].(object)["imageID"] = tc.running
			_, err := qualifyV1RetirementManagerEpochs(tc.declared, []string{"worker-a"}, []object{manager}, []object{pod})
			if (err == nil) != tc.ok {
				t.Fatalf("runtime qualification disagrees with pinned image provenance: %v", err)
			}
		})
	}
}

func retirementManager(name, node, image string) object {
	return object{
		"metadata": object{"name": name, "namespace": "longhorn-system", "uid": "manager-" + name},
		"spec":     object{"nodeID": node, "image": image, "dataEngine": "v1", "type": "aio"},
		"status":   object{"currentState": "running"},
	}
}

func retirementPod(name, node string) object {
	return object{
		"metadata": object{
			"name": name, "namespace": "longhorn-system", "uid": "pod-" + name,
			"ownerReferences": []any{object{
				"apiVersion": "longhorn.io/v1beta2", "kind": "InstanceManager", "name": name,
				"uid": "manager-" + name, "controller": true,
			}},
		},
		"spec": object{
			"nodeName": node, "restartPolicy": "Never",
			"containers": []any{object{
				"name": "instance-manager", "image": testRetirementImage,
				"volumeMounts":    []any{object{"name": "log", "mountPath": "/log"}},
				"securityContext": object{"privileged": true, "runAsUser": float64(0), "runAsGroup": float64(0)},
			}},
			"volumes": []any{object{"name": "log", "hostPath": object{
				"path": "/var/lib/longhorn/logs", "type": "DirectoryOrCreate",
			}}},
		},
		"status": object{
			"phase": "Running", "conditions": readyConditions(),
			"containerStatuses": []any{object{
				"name": "instance-manager", "imageID": testRetirementImage,
				"containerID": "containerd://" + strings.Repeat("b", 64),
				"ready":       true, "started": true, "restartCount": float64(0),
				"state": object{"running": object{"startedAt": "2026-11-10T09:00:00Z"}},
			}},
		},
	}
}
