package main

import (
	"strconv"
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
