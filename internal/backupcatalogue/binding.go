package backupcatalogue

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

// DeploymentDocuments are private, bounded native API responses. Secret values
// never form part of a serialized catalogue or a provider error message.
type DeploymentDocuments struct {
	Location, Repository, CloudSecret, RepositorySecret []byte
}

type deploymentBinding struct {
	policy                         Policy
	accessKey, secretKey, password string
	digest                         string
}

type bindingMetadata struct {
	Name, Namespace, UID, ResourceVersion string
	CreationTimestamp                     time.Time
	DeletionTimestamp                     *time.Time
}

type bindingObject struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   bindingMetadata `json:"metadata"`
	Spec       struct {
		Provider, AccessMode, BackupStorageLocation, RepositoryType, VolumeNamespace string
		Credential                                                                   json.RawMessage
		ObjectStorage                                                                struct{ Bucket, Prefix string }
		Config                                                                       map[string]string
	} `json:"spec"`
	Status struct{ Phase string } `json:"status"`
	Type   string                 `json:"type"`
	Data   map[string]string      `json:"data"`
}

var bindingCanonical = map[string][]string{
	"":                   {"apiVersion", "kind", "metadata", "spec", "status", "type", "data"},
	"metadata":           {"name", "namespace", "uid", "resourceVersion", "creationTimestamp", "deletionTimestamp"},
	"spec":               {"provider", "accessMode", "backupStorageLocation", "repositoryType", "volumeNamespace", "credential", "objectStorage", "config"},
	"spec.objectStorage": {"bucket", "prefix"},
	"spec.config":        {"s3Url", "region", "s3ForcePathStyle", "checksumAlgorithm"},
	"status":             {"phase"},
	"data":               {"cloud", "repository-password"},
}

func decodeBinding(data []byte, version, kind, name string) (bindingObject, error) {
	var object bindingObject
	if len(data) == 0 || len(data) > 512<<10 || strictjson.Validate(data, bindingCanonical) != nil || json.Unmarshal(data, &object) != nil ||
		object.APIVersion != version || object.Kind != kind || object.Metadata.Name != name || object.Metadata.Namespace != "velero" ||
		!regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`).MatchString(object.Metadata.UID) ||
		!regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(object.Metadata.ResourceVersion) ||
		object.Metadata.CreationTimestamp.IsZero() || object.Metadata.DeletionTimestamp != nil {
		return bindingObject{}, errReadHeld
	}
	return object, nil
}

func secretField(object bindingObject, key string) (string, error) {
	if object.Type != "Opaque" || len(object.Data) != 1 {
		return "", errReadHeld
	}
	encoded, ok := object.Data[key]
	value, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if !ok || err != nil || len(value) == 0 || len(value) > 16<<10 {
		return "", errReadHeld
	}
	return string(value), nil
}

func cloudCredentials(cloud string) (string, string, error) {
	lines := strings.Split(strings.TrimSpace(cloud), "\n")
	if len(lines) != 3 || strings.TrimSpace(lines[0]) != "[default]" {
		return "", "", errReadHeld
	}
	values := map[string]string{}
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key != "aws_access_key_id" && key != "aws_secret_access_key" || values[key] != "" ||
			value == "PLACEHOLDER" || len(value) == 0 || len(value) > 512 {
			return "", "", errReadHeld
		}
		for _, c := range value {
			if c < 0x21 || c > 0x7e {
				return "", "", errReadHeld
			}
		}
		values[key] = value
	}
	if values["aws_access_key_id"] == "" || values["aws_secret_access_key"] == "" {
		return "", "", errReadHeld
	}
	return values["aws_access_key_id"], values["aws_secret_access_key"], nil
}

func bindDeployment(namespace string, expected Policy, docs DeploymentDocuments) (deploymentBinding, error) {
	if namespace != "wedding-app" && namespace != "actual-budget" || !cleanKey(expected.Prefix) || !strings.HasSuffix(expected.Prefix, "/") {
		return deploymentBinding{}, errReadHeld
	}
	location, err := decodeBinding(docs.Location, "velero.io/v1", "BackupStorageLocation", "default")
	if err != nil || location.Status.Phase != "Available" || location.Spec.Provider != "aws" ||
		(location.Spec.AccessMode != "ReadWrite" && location.Spec.AccessMode != "ReadOnly") ||
		len(location.Spec.Credential) != 0 && string(location.Spec.Credential) != "null" ||
		location.Spec.ObjectStorage.Bucket != expected.Bucket || location.Spec.ObjectStorage.Prefix != strings.TrimSuffix(expected.Prefix, "/") ||
		location.Spec.Config["s3Url"] != expected.Endpoint || location.Spec.Config["region"] != "auto" || location.Spec.Config["s3ForcePathStyle"] != "true" {
		return deploymentBinding{}, errReadHeld
	}
	for key, value := range location.Spec.Config {
		if key != "s3Url" && key != "region" && key != "s3ForcePathStyle" && (key != "checksumAlgorithm" || value != "") {
			return deploymentBinding{}, errReadHeld
		}
	}
	repository, err := decodeBinding(docs.Repository, "velero.io/v1", "BackupRepository", namespace+"-default-kopia")
	if err != nil || repository.Status.Phase != "Ready" || repository.Spec.BackupStorageLocation != "default" || repository.Spec.RepositoryType != "kopia" || repository.Spec.VolumeNamespace != namespace {
		return deploymentBinding{}, errReadHeld
	}
	cloud, err := decodeBinding(docs.CloudSecret, "v1", "Secret", "velero-r2-credentials")
	if err != nil {
		return deploymentBinding{}, errReadHeld
	}
	cloudValue, err := secretField(cloud, "cloud")
	if err != nil {
		return deploymentBinding{}, errReadHeld
	}
	accessKey, secretKey, err := cloudCredentials(cloudValue)
	if err != nil {
		return deploymentBinding{}, errReadHeld
	}
	passwordSecret, err := decodeBinding(docs.RepositorySecret, "v1", "Secret", "velero-repo-credentials")
	if err != nil {
		return deploymentBinding{}, errReadHeld
	}
	password, err := secretField(passwordSecret, "repository-password")
	password = strings.TrimSpace(password) // Velero's read path trims the actual Secret value; it has no fallback.
	if err != nil || password == "" {
		return deploymentBinding{}, errReadHeld
	}
	// Velero 1.18.4 provider/unified_repo.go uses the BSL prefix, the fixed
	// backend and volumeNamespace. The CR name/UID is not an object-key prefix.
	expected.Prefix += "kopia/" + namespace + "/"
	// Bind validated identity, scope, readiness and credential data. API resource
	// versions and unconsumed validation timestamps can advance during a read
	// without changing those facts. Resource versions still must be present on
	// each complete native response. This digest includes Secret data and stays
	// private; it is never part of a serialized catalogue.
	objects := []bindingObject{location, repository, cloud, passwordSecret}
	for i := range objects {
		objects[i].Metadata.ResourceVersion = ""
	}
	encoded, err := json.Marshal(objects)
	if err != nil {
		return deploymentBinding{}, errReadHeld
	}
	digest := sha256.Sum256(encoded)
	return deploymentBinding{policy: expected, accessKey: accessKey, secretKey: secretKey, password: password, digest: hex.EncodeToString(digest[:])}, nil
}
