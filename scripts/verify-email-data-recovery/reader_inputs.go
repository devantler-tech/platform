package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
)

type retainedReaderInput struct {
	Kind, Name, Namespace, UID, Digest string
	Values                             object
}

type retainedReaderInputs struct {
	ReaderUID, ClaimUID string
	Inputs              []retainedReaderInput
}

// This captures custody inputs only. It neither establishes durable custody
// nor authorizes a pause, restart, detach or disposal. In particular it does
// not replace the original backup-validity gate with a credential snapshot.
// Values contain credentials and must stay in the protected private workspace.
func captureRetainedReaderInputs(i invocation, image, readerUID, claimUID, sourceClaimUID string, graph []object) (retainedReaderInputs, error) {
	fail := func() (retainedReaderInputs, error) { return retainedReaderInputs{}, errors.New("reader-inputs") }
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	imagePin := regexp.MustCompile(`^ghcr\.io/devantler-tech/platform-private-mail-bootstrap@sha256:[0-9a-f]{64}$`)
	if i != retainedRecovery || !imagePin.MatchString(image) || !uuid.MatchString(readerUID) || !uuid.MatchString(claimUID) || !uuid.MatchString(sourceClaimUID) || readerUID == claimUID || claimUID == sourceClaimUID {
		return fail()
	}
	wanted := append(recoveryResources(i, "", ""), recoveryReader(i, image))
	if len(graph) != len(wanted) {
		return fail()
	}
	actualByName := map[string]object{}
	seenUID := map[string]bool{}
	for _, actual := range graph {
		m := child(actual, "metadata")
		uid := text(m, "uid")
		ref := ownedReference{kind: text(actual, "kind"), name: text(m, "name"), namespace: text(m, "namespace")}
		if !uuid.MatchString(uid) || seenUID[uid] || !regexp.MustCompile(`^[1-9][0-9]{0,31}$`).MatchString(text(m, "resourceVersion")) || m["deletionTimestamp"] != nil || actualByName[ref.key()] != nil || !i.permitsMutation(ref.kind, ref.namespace, ref.name) {
			return fail()
		}
		if owners, exists := m["ownerReferences"]; exists && owners != nil {
			list, ok := owners.([]any)
			if !ok || len(list) != 0 {
				return fail()
			}
		}
		actualByName[ref.key()] = actual
		seenUID[uid] = true
	}
	bundle := retainedReaderInputs{ReaderUID: readerUID, ClaimUID: claimUID}
	for _, want := range wanted {
		m := child(want, "metadata")
		ref := ownedReference{kind: text(want, "kind"), name: text(m, "name"), namespace: text(m, "namespace")}
		actual := actualByName[ref.key()]
		if actual == nil || text(actual, "apiVersion") != text(want, "apiVersion") {
			return fail()
		}
		am := child(actual, "metadata")
		for key, value := range child(m, "labels") {
			if child(am, "labels")[key] != value {
				return fail()
			}
		}
		switch ref.kind {
		case "Pod":
			if text(am, "uid") != readerUID || text(child(actual, "status"), "phase") != "Running" || !admittedRecoveryObject(want, actual) || child(actual, "spec")["terminationGracePeriodSeconds"] != float64(30) {
				return fail()
			}
			ready := false
			for _, condition := range items(child(actual, "status"), "conditions") {
				if text(condition, "type") == "Ready" && text(condition, "status") == "True" {
					ready = true
				}
			}
			if !ready {
				return fail()
			}
		case "PersistentVolumeClaim":
			spec := child(actual, "spec")
			if text(am, "uid") != claimUID || text(child(actual, "status"), "phase") != "Bound" || !kubernetesName.MatchString(text(spec, "volumeName")) || text(spec, "storageClassName") != i.namespace || !reflect.DeepEqual(spec["accessModes"], []any{"ReadWriteOnce"}) || text(child(child(spec, "resources"), "requests"), "storage") != "2Gi" {
				return fail()
			}
		case "Secret":
			if !retainedSecret(actual, ref.name) || actual["stringData"] != nil {
				return fail()
			}
		case "ConfigMap":
			if !reflect.DeepEqual(actual["data"], want["data"]) || len(child(actual, "binaryData")) != 0 {
				return fail()
			}
		case "StorageClass":
			for _, key := range []string{"provisioner", "reclaimPolicy", "volumeBindingMode", "parameters"} {
				if !reflect.DeepEqual(actual[key], want[key]) {
					return fail()
				}
			}
		case "CiliumNetworkPolicy":
			if !reflect.DeepEqual(actual["spec"], want["spec"]) {
				return fail()
			}
		}
		// Marshal/unmarshal creates a private snapshot: later API reads cannot
		// mutate values already accepted for the conditional custody write.
		canonical, err := json.Marshal(retainedMutableFields(actual))
		if err != nil {
			return fail()
		}
		var frozen object
		if json.Unmarshal(canonical, &frozen) != nil {
			return fail()
		}
		digest := sha256.Sum256(canonical)
		bundle.Inputs = append(bundle.Inputs, retainedReaderInput{Kind: ref.kind, Name: ref.name, Namespace: ref.namespace, UID: text(am, "uid"), Digest: hex.EncodeToString(digest[:]), Values: frozen})
	}
	return bundle, nil
}
