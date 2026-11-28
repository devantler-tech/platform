package main

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestCNPGStreamSourcePinsTheReplicaAndItsTLSNames(t *testing.T) {
	for _, count := range []int{1, 2} {
		for _, fault := range []string{"", "archive enabled", "wrong namespace", "foreign CA", "wrong SAN", "wrong role", "public endpoint"} {
			t.Run(strconv.Itoa(count)+"/"+fault, func(t *testing.T) {
				observation := cnpgStreamObservationFixture(t, count)
				cluster := observation.Cluster
				member := observation.Pods[count-1]
				switch fault {
				case "archive enabled":
					valueObject(cluster, "spec")["plugins"] = []any{object{"isWALArchiver": true}}
				case "wrong namespace":
					valueObject(cluster, "metadata")["namespace"] = "unreviewed"
				case "foreign CA":
					valueObject(cluster, "status", "certificates")["serverCASecret"] = "unrelated-ca"
				case "wrong SAN":
					valueObject(cluster, "status", "certificates")["serverAltDNSNames"] = []any{"unrelated.svc"}
				case "wrong role":
					name := text(member, "metadata", "name")
					state := observation.States[name]
					state.InRecovery = !state.InRecovery
					observation.States[name] = state
				case "public endpoint":
					valueObject(member, "status")["podIP"] = "203.0.113.22"
				}
				bound, err := bindCNPGStreamSource(observation)
				if fault == "" {
					if err != nil || bound.Member.Name != text(member, "metadata", "name") || bound.State.InRecovery != (count == 2) {
						t.Fatalf("the correct native member did not bind: %v", err)
					}
				} else if err == nil {
					t.Fatal("an unqualified physical source was accepted")
				}
			})
		}
	}
}

func TestCNPGStreamSourceRejectsASwitchoverDuringTheFinalProbe(t *testing.T) {
	for _, switchover := range []bool{false, true} {
		t.Run(strconv.FormatBool(switchover), func(t *testing.T) {
			fixture := cnpgStreamObservationFixture(t, 2)
			source := strings.Repeat("a", 40)
			namespace, name := "ate-system", "substrate-db"
			member := fixture.Pods[1]
			claimName := text(member, "metadata", "name")
			const claimUID = "d827f800-8471-40c6-815a-7e59617ee620"
			pvName := "pvc-" + claimUID
			valueObject(member, "spec")["volumes"] = []any{object{"name": "pgdata", "persistentVolumeClaim": object{"claimName": claimName}}}
			for _, raw := range value(member, "spec", "containers").([]any) {
				container := raw.(object)
				if text(container, "name") == "postgres" {
					container["volumeMounts"] = []any{object{"name": "pgdata", "mountPath": "/var/lib/postgresql/data"}}
				}
			}
			claim := object{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
				"metadata": object{"name": claimName, "namespace": namespace, "uid": claimUID, "resourceVersion": "51"},
				"spec":     object{"volumeName": pvName, "storageClassName": "longhorn-wffc", "accessModes": []any{"ReadWriteOnce"}, "volumeMode": "Filesystem"},
				"status":   object{"phase": "Bound", "capacity": object{"storage": "5Gi"}}}
			pv := object{"apiVersion": "v1", "kind": "PersistentVolume",
				"metadata": object{"name": pvName, "uid": "a4f66240-e801-4624-a98a-af06d7fbfb2f", "resourceVersion": "52"},
				"spec": object{"storageClassName": "longhorn-wffc", "accessModes": []any{"ReadWriteOnce"}, "volumeMode": "Filesystem",
					"capacity": object{"storage": "5Gi"}, "claimRef": object{"name": claimName, "namespace": namespace, "uid": claimUID},
					"csi": object{"driver": "driver.longhorn.io", "volumeHandle": pvName}},
				"status": object{"phase": "Bound"}}
			volume := object{"apiVersion": "longhorn.io/v1beta2", "kind": "Volume",
				"metadata": object{"name": pvName, "namespace": "longhorn-system", "uid": "7860ea13-bc1d-4e4c-9a12-ed34c67dbbed", "resourceVersion": "53"},
				"spec":     object{"dataEngine": "v1"},
				"status": object{"state": "attached", "robustness": "healthy", "currentNodeID": text(member, "spec", "nodeName"),
					"kubernetesStatus": object{"pvName": pvName, "namespace": namespace, "pvcName": claimName}}}
			roleChanged := false
			physicalReads := 0
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if len(input) != 0 {
					t.Fatal("the source observation attempted a write")
				}
				if program == "git" {
					return []byte(source + "\trefs/heads/main\n"), nil
				}
				if program != "kubectl" || len(args) < 4 {
					t.Fatal("unexpected command")
				}
				if args[2] == "exec" {
					if args[len(args)-1] == "--command="+postgresStateSQL {
						physicalReads++
						if physicalReads == 6 && switchover {
							roleChanged = true
						}
						return json.Marshal(fixture.States[args[3]])
					}
					if strings.Contains(args[len(args)-1], "max_wal_senders") {
						return []byte(`{"version":180007,"senders":2,"slots":1}`), nil
					}
					t.Fatal("unexpected SQL")
				}
				if args[2] != "get" {
					t.Fatal("unexpected Kubernetes operation")
				}
				switch args[3] {
				case "cluster.postgresql.cnpg.io":
					if roleChanged {
						next := cloneCNPGObservation(t, fixture)
						valueObject(next.Cluster, "status")["targetPrimary"] = claimName
						return json.Marshal(next.Cluster)
					}
					return json.Marshal(fixture.Cluster)
				case "pods":
					return json.Marshal(object{"metadata": object{"resourceVersion": "10"}, "items": fixture.Pods})
				case "pod":
					for _, pod := range fixture.Pods {
						if text(pod, "metadata", "name") == args[4] {
							return json.Marshal(pod)
						}
					}
				case "persistentvolumeclaim":
					return json.Marshal(claim)
				case "persistentvolume":
					return json.Marshal(pv)
				case "volumes.longhorn.io":
					return json.Marshal(volume)
				}
				return nil, errors.New("unexpected read")
			}}
			binding, err := resolveCNPGStreamSource(t.Context(), c, name, source)
			if physicalReads != 6 {
				t.Fatalf("did not reach the final SQL probe: probes=%d error=%v", physicalReads, err)
			}
			if switchover {
				if err == nil {
					t.Fatal("accepted an obsolete standby after the final probe")
				}
			} else if err != nil || binding.Claim.Name != claimName {
				t.Fatalf("the stable source was not verified: %v", err)
			}
		})
	}
}

func cnpgStreamObservationFixture(t *testing.T, count int) cnpgObservation {
	t.Helper()
	r := cnpgObservationFixture(t)
	const uid = "dc7a7f44-5101-4f1e-b768-4a19c670c458"
	name, ns, size := "substrate-db", "ate-system", "5Gi"
	if count == 1 {
		name, ns, size = previewCNPGName, previewCNPGNamespace, "2Gi"
	}
	r.Cluster["apiVersion"], r.Cluster["kind"] = "postgresql.cnpg.io/v1", "Cluster"
	meta := valueObject(r.Cluster, "metadata")
	meta["uid"], meta["resourceVersion"], meta["name"], meta["namespace"] = uid, "10", name, ns
	spec := valueObject(r.Cluster, "spec")
	spec["instances"], spec["imageName"] = float64(count), "ghcr.io/cloudnative-pg/postgresql:18.7-system-trixie"
	valueObject(spec, "storage")["size"] = size
	status := valueObject(r.Cluster, "status")
	status["instances"], status["readyInstances"] = float64(count), float64(count)
	status["currentPrimary"], status["targetPrimary"] = name+"-1", name+"-1"
	status["certificates"] = object{
		"serverCASecret": name + "-ca", "clientCASecret": name + "-ca", "replicationTLSSecret": name + "-replication", "serverTLSSecret": name + "-server",
		"serverAltDNSNames": []any{name + "-rw." + ns + ".svc", name + "-ro." + ns + ".svc"},
	}
	r.Pods = r.Pods[:count]
	statusNames := make([]any, 0, count)
	for i, pod := range r.Pods {
		meta := valueObject(pod, "metadata")
		memberName := name + "-" + strconv.Itoa(i+1)
		r.States[memberName] = r.States[text(meta, "name")]
		delete(r.States, text(meta, "name"))
		pod["apiVersion"], pod["kind"] = "v1", "Pod"
		meta["name"], meta["namespace"] = memberName, ns
		meta["uid"], meta["resourceVersion"] = "c73106de-7d50-468b-8531-7006a984387"+strconv.Itoa(i), "11"
		valueObject(meta, "labels")["cnpg.io/cluster"] = name
		ref := value(meta, "ownerReferences").([]any)[0].(object)
		ref["name"], ref["uid"] = name, uid
		for _, raw := range value(pod, "spec", "containers").([]any) {
			container := raw.(object)
			if text(container, "name") == "postgres" {
				container["image"] = spec["imageName"]
			}
		}
		valueObject(pod, "status")["podIP"] = "10.244.4." + strconv.Itoa(i+10)
		statusNames = append(statusNames, memberName)
	}
	status["instancesStatus"] = object{"healthy": statusNames}
	return r
}
