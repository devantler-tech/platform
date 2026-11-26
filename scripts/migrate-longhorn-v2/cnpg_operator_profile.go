package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

// cnpgReviewedCRDSHA binds the complete released 1.30.2 CRD specification after
// Kubernetes adds its default conversion.strategy=None. Numeric tokens are
// retained exactly; neither a live schema nor admission output selects this pin.
const cnpgReviewedCRDSHA = "55fa73c34cc318c7cd4a757163921e9626e9f604aab9a2308ff33426b574428a"

type cnpgCRDBinding struct {
	UID, SpecSHA string
	Generation   uint64
}

type cnpgStartupBinding struct {
	Defaults                       recoveryAdmissionDefaults
	StartedAt, LoadedAt, ConfigSHA string
	OperatorPullSecretName         string
	CreateAnyService               bool
}

// bindCNPGCRD validates a complete CRD against the caller's reviewed immutable pin.
func bindCNPGCRD(raw []byte, reviewedSHA string) (cnpgCRDBinding, error) {
	return bindCNPGRecoveryCRD(raw, "clusters.postgresql.cnpg.io", reviewedSHA)
}

// bindCNPGRecoveryCRD shares complete native status/spec binding for the two
// compiled recovery API families. Callers supply reviewed pins, never live hashes.
func bindCNPGRecoveryCRD(raw []byte, reviewedName, reviewedSHA string) (cnpgCRDBinding, error) {
	refused := errors.New("installed database schema is not the established reviewed definition")
	if reviewedName != "clusters.postgresql.cnpg.io" && reviewedName != "objectstores.barmancloud.cnpg.io" {
		return cnpgCRDBinding{}, refused
	}
	crd, err := decodeCNPGProfileObject(raw)
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(reviewedSHA) || text(crd, "apiVersion") != "apiextensions.k8s.io/v1" || text(crd, "kind") != "CustomResourceDefinition" || text(crd, "metadata", "name") != reviewedName || text(crd, "metadata", "uid") == "" || text(crd, "metadata", "resourceVersion") == "" || value(crd, "metadata", "deletionTimestamp") != nil {
		return cnpgCRDBinding{}, refused
	}
	generation, ok := nativeObservationUint(value(crd, "metadata", "generation"))
	if !ok || generation == 0 || generation > 2147483647 || !recoveryJSONEqual(value(crd, "status", "storedVersions"), []any{"v1"}) {
		return cnpgCRDBinding{}, refused
	}
	conditions, ok := value(crd, "status", "conditions").([]any)
	seen := map[string]string{}
	if !ok {
		return cnpgCRDBinding{}, refused
	}
	for _, raw := range conditions {
		condition, valid := raw.(object)
		kind, status := text(condition, "type"), text(condition, "status")
		if !valid || seen[kind] != "" || status != "True" && status != "False" && status != "Unknown" {
			return cnpgCRDBinding{}, refused
		}
		switch kind {
		case "Established", "NamesAccepted":
		case "Terminating", "NonStructuralSchema":
			if status != "False" {
				return cnpgCRDBinding{}, refused
			}
		default:
			return cnpgCRDBinding{}, refused
		}
		seen[kind] = status
	}
	if seen["Established"] != "True" || seen["NamesAccepted"] != "True" {
		return cnpgCRDBinding{}, refused
	}
	spec, err := json.Marshal(value(crd, "spec"))
	digest := fmt.Sprintf("%x", sha256.Sum256(spec))
	if err != nil || digest != reviewedSHA {
		return cnpgCRDBinding{}, refused
	}
	return cnpgCRDBinding{UID: text(crd, "metadata", "uid"), Generation: generation, SpecSHA: digest}, nil
}

// bindCNPGOperatorStartup binds only the bounded current PID1 startup prefix.
// The caller must bracket the Pod, direct /manager command, image and container
// identity. This is not a complete-log audit or storage/creation authority.
func bindCNPGOperatorStartup(raw []byte, started string) (cnpgStartupBinding, error) {
	refused := errors.New("operator startup configuration is not bound to the current reviewed process")
	start, err := time.Parse(time.RFC3339Nano, started)
	if err != nil || len(raw) == 0 || len(raw) > 131072 {
		return cnpgStartupBinding{}, refused
	}
	phase := 0
	previous := start
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		record, e := decodeCNPGProfileObject(line)
		if e != nil {
			return cnpgStartupBinding{}, refused
		}
		message := text(record, "msg")
		if message != "Starting CloudNativePG Operator" && message != "Loading configuration from ConfigMap" && message != "Operator configuration loaded" {
			continue
		}
		at, e := time.Parse(time.RFC3339Nano, text(record, "ts"))
		if e != nil || at.Before(previous) || at.After(start.Add(time.Minute)) {
			return cnpgStartupBinding{}, refused
		}
		previous = at
		switch message {
		case "Starting CloudNativePG Operator":
			if phase != 0 || text(record, "version") != "1.30.2" || text(record, "build", "Version") != "1.30.2" || text(record, "build", "Commit") != "30cca36d3" {
				return cnpgStartupBinding{}, refused
			}
			phase = 1
		case "Loading configuration from ConfigMap":
			if phase != 1 || text(record, "name") != "cnpg-controller-manager-config" || text(record, "namespace") != "cnpg-system" {
				return cnpgStartupBinding{}, refused
			}
			phase = 2
		case "Operator configuration loaded":
			if phase != 2 {
				return cnpgStartupBinding{}, refused
			}
			configuration, ok := value(record, "configuration").(object)
			if !ok {
				return cnpgStartupBinding{}, refused
			}
			for key, expected := range map[string]string{"operatorImageName": "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.2", "operatorNamespace": "cnpg-system", "monitoringQueriesConfigmap": "cnpg-default-monitoring", "monitoringQueriesSecret": "", "includePlugins": "", "operatorPullSecretName": "cnpg-pull-secret"} {
				actual, present := configuration[key].(string)
				if !present || actual != expected {
					return cnpgStartupBinding{}, refused
				}
			}
			createAnyService, present := configuration["createAnyService"].(bool)
			if !present {
				return cnpgStartupBinding{}, refused
			}
			encoded, e := json.Marshal(configuration)
			if e != nil {
				return cnpgStartupBinding{}, refused
			}
			// Only the startup prefix through this record is the evidence input.
			// Later operational logs are outside this narrowly defined binding.
			return cnpgStartupBinding{Defaults: recoveryAdmissionDefaults{Version: "1.30.2", ConfigMapQueries: true}, StartedAt: started, LoadedAt: text(record, "ts"), ConfigSHA: fmt.Sprintf("%x", sha256.Sum256(encoded)), OperatorPullSecretName: configuration["operatorPullSecretName"].(string), CreateAnyService: createAnyService}, nil
		}
	}
	return cnpgStartupBinding{}, refused
}

// decodeCNPGProfileObject retains schema numeric tokens and rejects partial JSON.
func decodeCNPGProfileObject(raw []byte) (object, error) {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return nil, errors.New("profile input exceeds its bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var result object
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("profile object is missing")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("profile contains trailing JSON")
	}
	return result, nil
}
