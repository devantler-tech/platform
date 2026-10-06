package main

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const (
	bootstrapPath           = "k8s/clusters/prod/bootstrap/config-map.yaml"
	storePath               = "k8s/bases/infrastructure/actions-runners/secret-store.yaml"
	stagedStorePath         = "k8s/bases/infrastructure/actions-runners/credentials/secret-store.yaml"
	baoServer               = "https://openbao-arc.openbao.svc.cluster.local:8204"
	baoTLSPort              = 8204
	failConfig      outcome = "FAIL_CONFIG"
)

type storeConfiguration struct{ server, caBundle, caName, caKey string }
type configuration struct {
	clientID string
	store    storeConfiguration
}

func loadConfiguration(root string) (configuration, outcome) {
	bootstrap, err := readYAMLFile(filepath.Join(root, bootstrapPath))
	if err != nil {
		return configuration{}, failConfig
	}
	clientID, status := parseBootstrap(bootstrap)
	if status != pass {
		return configuration{}, status
	}
	store, err := readStoreSource(root)
	if err != nil {
		return configuration{}, failConfig
	}
	parsed, status := parseStore(store)
	return configuration{clientID: clientID, store: parsed}, status
}

func readStoreSource(root string) (map[string]any, error) {
	var selected map[string]any
	found := false
	for _, path := range []string{storePath, stagedStorePath} {
		value, err := readYAMLFile(filepath.Join(root, path))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if found {
			return nil, fmt.Errorf("ambiguous store sources")
		}
		selected, found = value, true
	}
	if !found {
		return nil, fmt.Errorf("missing store source")
	}
	return selected, nil
}

func readYAMLFile(path string) (map[string]any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxResponse+1))
	if err != nil || len(data) > maxResponse {
		return nil, fmt.Errorf("invalid configuration")
	}
	return yamlObject(data)
}
func yamlObject(data []byte) (map[string]any, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("multiple configuration documents")
	}
	return value, nil
}
func object(value any) map[string]any { result, _ := value.(map[string]any); return result }
func stringValue(value any) string    { result, _ := value.(string); return result }
func exactKeys(value map[string]any, allowed ...string) bool {
	if len(value) == 0 {
		return false
	}
	for key := range value {
		found := false
		for _, candidate := range allowed {
			found = found || key == candidate
		}
		if !found {
			return false
		}
	}
	return true
}
func namedResource(value map[string]any, kind, name, namespace string) bool {
	meta := object(value["metadata"])
	return stringValue(value["kind"]) == kind && stringValue(meta["name"]) == name && stringValue(meta["namespace"]) == namespace
}
func parseBootstrap(value map[string]any) (string, outcome) {
	clientID := stringValue(object(value["data"])["github_app_client_id"])
	if stringValue(value["apiVersion"]) != "v1" || !namedResource(value, "ConfigMap", "variables-cluster", "flux-system") || !clientIDPattern.MatchString(clientID) {
		return "", failConfig
	}
	return clientID, pass
}
func parseStore(value map[string]any) (storeConfiguration, outcome) {
	if stringValue(value["apiVersion"]) != "external-secrets.io/v1" || !namedResource(value, "SecretStore", "openbao", "arc-runners") {
		return storeConfiguration{}, failConfig
	}
	provider := object(object(value["spec"])["provider"])
	vault := object(provider["vault"])
	auth := object(vault["auth"])
	kube := object(auth["kubernetes"])
	account := object(kube["serviceAccountRef"])
	if !exactKeys(provider, "vault") || !exactKeys(vault, "server", "path", "version", "auth", "caBundle", "caProvider") || !exactKeys(auth, "kubernetes") || !exactKeys(kube, "mountPath", "role", "serviceAccountRef") || !exactKeys(account, "name") || stringValue(vault["path"]) != "secret" || stringValue(vault["version"]) != "v2" || stringValue(kube["mountPath"]) != "kubernetes" || stringValue(kube["role"]) != "arc-secret-reader" || stringValue(account["name"]) != "arc-secret-reader" {
		return storeConfiguration{}, failConfig
	}
	config := storeConfiguration{server: stringValue(vault["server"]), caBundle: stringValue(vault["caBundle"])}
	if _, ok := baoListenerPort(config.server); !ok {
		return config, holdTransport
	}
	if config.caBundle != "" {
		if _, exists := vault["caProvider"]; exists {
			return config, holdTransport
		}
		ca, err := base64.StdEncoding.DecodeString(config.caBundle)
		if err != nil || trustedRoots(ca) == nil {
			return config, holdTransport
		}
	} else {
		ca := object(vault["caProvider"])
		config.caName = stringValue(ca["name"])
		config.caKey = stringValue(ca["key"])
		namespace := stringValue(ca["namespace"])
		if value, present := ca["namespace"]; present {
			if _, ok := value.(string); !ok {
				return config, holdTransport
			}
		}
		if !exactKeys(ca, "type", "name", "key", "namespace") || stringValue(ca["type"]) != "ConfigMap" || config.caName != "arc-openbao-ca" || config.caKey != "ca.crt" || (namespace != "" && namespace != "arc-runners") {
			return config, holdTransport
		}
	}
	return config, pass
}

func baoListenerPort(server string) (int, bool) {
	return baoTLSPort, server == baoServer
}
func trustedRoots(ca []byte) *x509.CertPool {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil
	}
	return roots
}

type runtimeModes struct {
	identity, transport func(configuration) outcome
}

func run(args []string, root string, output io.Writer, modes runtimeModes) int {
	result := outcome("HOLD_INVOCATION")
	transport := len(args) == 1 && args[0] == "--transport"
	confirmation := "verify-arc-app-identity"
	if transport {
		confirmation = "verify-arc-app-transport"
	}
	if len(args) == 1 && (args[0] == "--preflight" || args[0] == "--verify" || transport) {
		if args[0] == "--preflight" || protectedInvocation(confirmation) {
			config, status := loadConfiguration(root)
			result = status
			if result == pass {
				if args[0] == "--preflight" {
					result = "TRANSPORT_CONFIG_READY"
				} else if transport {
					result = modes.transport(config)
				} else {
					result = modes.identity(config)
				}
			}
		}
	}
	// Neither arguments, configuration values, API bodies nor underlying errors
	// are an output surface, including when an invocation is rejected.
	switch result {
	case pass, holdTransport, holdEntry, failTransport, failIdentity, failAPI, failCleanup, failConfig, failHealth, "HOLD_INVOCATION", "HOLD_READER", "TRANSPORT_CONFIG_READY":
	default:
		result = failAPI
	}
	prefix := "ARC_APP_IDENTITY"
	if transport {
		prefix = "ARC_APP_TRANSPORT"
		// Credential-specific outcomes cannot be emitted as transport evidence.
		switch result {
		case pass, holdTransport, failTransport, failAPI, failConfig, failHealth, "HOLD_INVOCATION":
		default:
			result = failAPI
		}
	}
	_, _ = fmt.Fprintf(output, "%s=%s\n", prefix, result)
	if result == pass || result == "TRANSPORT_CONFIG_READY" {
		return 0
	}
	return 1
}
func protectedInvocation(confirmation string) bool {
	for name, expected := range map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_REPOSITORY": "devantler-tech/platform", "ARC_IDENTITY_CONFIRM": confirmation} {
		if os.Getenv(name) != expected {
			return false
		}
	}
	return true
}
