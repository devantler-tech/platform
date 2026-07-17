package refreshfluxghcrauth

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

func fakeKSail(args []string) int {
	if containsSequence(args, "workload", "cipher", "decrypt") {
		selector := `["stringData"]["ghcr_dockerconfigjson"]`
		if !containsAdjacent(args, "--extract", selector) {
			return commandFailure(92, "missing exact encrypted-secret selector")
		}
		output := flagValue(args, "--output")
		if output == "" {
			return commandFailure(92, "missing decrypt output")
		}
		mustWriteEnvFile("KSAIL_OUTPUT_PATH_LOG", output)
		if err := copyFile(os.Getenv("FAKE_DECRYPTED_CONFIG"), output); err != nil {
			return commandFailure(92, "copy decrypted fixture: %v", err)
		}
		return 0
	}

	isLifecycle := containsSequence(args, "cluster", "create") ||
		containsSequence(args, "cluster", "update") ||
		containsSequence(args, "workload", "push") ||
		containsSequence(args, "workload", "reconcile")
	if !isLifecycle {
		return commandFailure(92, "unexpected ksail invocation")
	}
	configPath := flagValue(args, "--config")
	config, err := os.ReadFile(configPath)
	if err != nil {
		return commandFailure(92, "read KSail config: %v", err)
	}
	var parsedConfig struct {
		Spec struct {
			Cluster struct {
				LocalRegistry struct {
					Registry string `yaml:"registry"`
				} `yaml:"localRegistry"`
			} `yaml:"cluster"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(config, &parsedConfig); err != nil {
		return commandFailure(92, "parse KSail config: %v", err)
	}
	registry := parsedConfig.Spec.Cluster.LocalRegistry.Registry
	expectedRegistry := `devantler:${GHCR_TOKEN}@ghcr.io/devantler-tech/platform/manifests`
	if registry != expectedRegistry {
		return commandFailure(92, "protected registry template changed")
	}
	registryOverride := os.Getenv("KSAIL_SPEC_CLUSTER_LOCALREGISTRY_REGISTRY")
	if containsSequence(args, "workload", "push") {
		if registryOverride != "" {
			return commandFailure(92, "publish unexpectedly overrides registry")
		}
	} else if registryOverride != `${GHCR_USERNAME}:${GHCR_TOKEN}@ghcr.io/devantler-tech/platform/manifests` {
		return commandFailure(92, "lifecycle registry override missing")
	}
	if os.Getenv("GHCR_TOKEN") == "" || os.Getenv("GHCR_USERNAME") == "" || os.Getenv("GHCR_PULL_REVISION") == "" {
		return commandFailure(92, "missing lifecycle environment")
	}
	mustWriteEnvFile("KSAIL_TOKEN_CAPTURE", os.Getenv("GHCR_TOKEN"))
	mustWriteEnvFile("KSAIL_USERNAME_CAPTURE", os.Getenv("GHCR_USERNAME"))
	mustWriteEnvFile("KSAIL_REVISION_CAPTURE", os.Getenv("GHCR_PULL_REVISION"))
	mustWriteEnvFile("KSAIL_COMMAND_CAPTURE", strings.Join(args, " ")+"\n")
	mustWriteEnvFile("KSAIL_CONFIG_PATH_CAPTURE", configPath)
	mustWriteEnvFile("KSAIL_REGISTRY_CAPTURE", registry)
	mustWriteEnvFile("KSAIL_REGISTRY_OVERRIDE_CAPTURE", registryOverride)
	return 0
}

func fakeCurl(args []string) int {
	if len(args) == 0 || args[0] != "--disable" {
		return commandFailure(90, "curl must disable user config")
	}
	var configPath, outputPath, scope, requestURL string
	var connectTimeout, maxTime string
	for index := 1; index < len(args); {
		switch args[index] {
		case "--config":
			configPath = requiredNext(args, &index)
		case "--connect-timeout":
			connectTimeout = requiredNext(args, &index)
		case "--max-time":
			maxTime = requiredNext(args, &index)
		case "--output":
			outputPath = requiredNext(args, &index)
		case "--data-urlencode":
			value := requiredNext(args, &index)
			if strings.HasPrefix(value, "scope=") {
				scope = strings.TrimPrefix(value, "scope=")
			}
		case "--write-out", "--header":
			_ = requiredNext(args, &index)
		case "--silent", "--show-error", "--get":
			index++
		default:
			if strings.HasPrefix(args[index], "https://") {
				requestURL = args[index]
				index++
				continue
			}
			return commandFailure(90, "unexpected curl argument: %s", args[index])
		}
	}
	if connectTimeout != "10" || maxTime != "60" {
		return commandFailure(90, "curl requires 10-second connect and 60-second total timeouts")
	}
	config := mustReadCommandFile(configPath)
	if outputPath == "" || requestURL == "" {
		return commandFailure(90, "incomplete curl invocation")
	}
	if requestURL == "https://ghcr.io/token" {
		if scope == "" || !hasCurlUserConfig(config) {
			return commandFailure(90, "invalid token exchange")
		}
		if os.Getenv("FAKE_REVOKE_CURRENT_ROOT_TOKEN") == "true" && strings.Contains(config, "previous-runtime-token") {
			mustWriteCommandFile(outputPath, `{}`)
			fmt.Print("403")
			return 0
		}
		mustWriteCommandFile(outputPath, `{"token":"fixture-registry-token"}`)
		fmt.Print("200")
		return 0
	}
	if !strings.Contains(config, "Authorization: Bearer fixture-registry-token") {
		return commandFailure(90, "manifest read lacks bearer token")
	}
	parsed, err := url.Parse(requestURL)
	if err != nil {
		return commandFailure(90, "parse request URL: %v", err)
	}
	manifestPath := strings.TrimPrefix(parsed.Path, "/v2/")
	manifestSeparator := strings.LastIndex(manifestPath, "/manifests/")
	if manifestSeparator < 0 {
		return commandFailure(90, "invalid manifest URL")
	}
	repository := manifestPath[:manifestSeparator]
	reference := manifestPath[manifestSeparator+len("/manifests/"):]
	appendEnvFile("REGISTRY_READ_LOG", repository+":"+reference+"\n")
	if repository == os.Getenv("FAKE_CURL_DENY_REPOSITORY") {
		fmt.Print("403")
	} else {
		fmt.Print("200")
	}
	return 0
}

func fakeTalosctl(args []string) int {
	node := flagValue(args, "--nodes")
	if node == "" {
		for _, argument := range args {
			if strings.HasPrefix(argument, "--nodes=") {
				node = strings.TrimPrefix(argument, "--nodes=")
			}
		}
	}
	if node == "" {
		return commandFailure(93, "talosctl node missing")
	}

	if containsSequence(args, "etcd", "status") {
		if node == os.Getenv("FAKE_ETCD_STATUS_FAIL_NODE") ||
			(node == os.Getenv("FAKE_ETCD_STATUS_FAIL_AFTER_DRAIN_NODE") && markerExists("drained-prod-control-plane-1")) {
			return commandFailure(51, "etcd status failed")
		}
		learner := node == os.Getenv("FAKE_ETCD_LEARNER_NODE")
		statusError := ""
		if node == os.Getenv("FAKE_ETCD_STATUS_ERROR_NODE") {
			statusError = " rpc-timeout"
		}
		if node == os.Getenv("FAKE_ETCD_COMPACT_STATUS_NODE") {
			fmt.Println("NODE MEMBER DB SIZE IN USE LEADER RAFT INDEX RAFT TERM RAFT APPLIED INDEX LEARNER ERRORS")
			fmt.Printf("%s member-id 1.0 MB 0.5 MB (50.00%%) leader-id 100 2 100 %t%s\n", node, learner, statusError)
		} else {
			fmt.Println("NODE MEMBER DB SIZE IN USE LEADER RAFT INDEX RAFT TERM RAFT APPLIED INDEX LEARNER PROTOCOL STORAGE ERRORS")
			fmt.Printf("%s member-id 1.0 MB 0.5 MB (50.00%%) leader-id 100 2 100 %t 3.6.4 3.6.0%s\n", node, learner, statusError)
		}
		return 0
	}
	if containsSequence(args, "etcd", "alarm", "list") {
		if node == os.Getenv("FAKE_ETCD_ALARM_READ_FAIL_NODE") {
			return commandFailure(52, "etcd alarm read failed")
		}
		fmt.Println("NODE MEMBER ALARM")
		if node == os.Getenv("FAKE_ETCD_ALARM_NODE") {
			fmt.Printf("%s member-id NOSPACE\n", node)
		}
		return 0
	}

	if containsSequence(args, "patch", "machineconfig") {
		if !containsArg(args, "--mode=no-reboot") {
			return commandFailure(93, "Talos patch may not reboot implicitly")
		}
		patchPath := ""
		for _, argument := range args {
			if strings.HasPrefix(argument, "--patch-file=") {
				patchPath = strings.TrimPrefix(argument, "--patch-file=")
			}
		}
		if patchPath == "" {
			patchPath = flagValue(args, "--patch-file")
		}
		mustWriteEnvFile("TALOS_PATCH_PATH_LOG", patchPath)
		var patch map[string]any
		if err := json.Unmarshal([]byte(mustReadCommandFile(patchPath)), &patch); err != nil {
			return commandFailure(93, "parse Talos patch: %v", err)
		}
		if patch["kind"] == "RegistryAuthConfig" {
			if patch["apiVersion"] != "v1alpha1" || patch["name"] != "ghcr.io" ||
				patch["username"] != os.Getenv("EXPECTED_PULL_USERNAME") || patch["password"] != os.Getenv("EXPECTED_PULL_TOKEN") {
				return commandFailure(93, "invalid RegistryAuthConfig")
			}
			nodeName := fakeNodeName(node)
			if nodeName == "" || !markerExists("cordoned-"+nodeName) ||
				markerContent("cordon-owner-"+nodeName) == "" {
				return commandFailure(93, "Talos auth mutation lacked an owned Kubernetes cordon")
			}
			if markerContent("cordon-recovery-"+nodeName) != "" &&
				fakeRecoveryPhase(nodeName) != "active" {
				return commandFailure(93, "bootstrap Talos auth mutation lacked an active recovery journal")
			}
			appendTalosOperation("talos-auth:" + node)
			if talosFailure(node, "auth") {
				return commandFailure(45, "talos auth failed with %s", os.Getenv("EXPECTED_PULL_TOKEN"))
			}
			touchMarker("talos-auth-" + node)
			if nodeName == os.Getenv("FAKE_EXTERNAL_UNCORDON_AFTER_AUTH_NODE") {
				removeMarker("cordoned-" + nodeName)
				appendEnvFile("OPERATION_LOG", "operator-uncordon-after-auth:"+nodeName+"\n")
			}
			return 0
		}
		if markerExists("talos-auth-" + node) {
			if !markerExists("talos-reboot-" + node) {
				return commandFailure(93, "revision preceded reboot")
			}
		} else if os.Getenv("FAKE_TALOS_NODES_CURRENT") != "true" {
			return commandFailure(93, "image-only proof lacks current credential")
		}
		if !markerExists("talos-remove-"+node) || !markerExists("talos-pull-"+node) {
			return commandFailure(93, "revision preceded registry pull proof")
		}
		nodeName := fakeNodeName(node)
		if nodeName == "" || !markerExists("cordoned-"+nodeName) ||
			markerContent("cordon-owner-"+nodeName) == "" {
			return commandFailure(93, "Talos revision mutation lacked an owned Kubernetes cordon")
		}
		if markerContent("cordon-recovery-"+nodeName) != "" &&
			fakeRecoveryPhase(nodeName) != "retain" {
			return commandFailure(93, "bootstrap Talos revision mutation lacked a retained recovery journal")
		}
		machine, _ := patch["machine"].(map[string]any)
		annotations, _ := machine["nodeAnnotations"].(map[string]any)
		if annotations["platform.devantler.tech/ghcr-pull-verified-revision-v2"] != os.Getenv("EXPECTED_GHCR_REVISION") ||
			annotations["platform.devantler.tech/ghcr-pull-verified-image-v2"] != os.Getenv("EXPECTED_KSAIL_TARGET_IMAGE") {
			return commandFailure(93, "invalid verified pull marker")
		}
		appendTalosOperation("talos-revision:" + node)
		if talosFailure(node, "revision") {
			return commandFailure(48, "talos revision failed")
		}
		touchMarker("talos-revision-" + node)
		if node == "10.0.0.5" && os.Getenv("FAKE_CONSUMER_REVERT_DURING_LATE_NODE_NAMESPACE") != "" {
			namespace := os.Getenv("FAKE_CONSUMER_REVERT_DURING_LATE_NODE_NAMESPACE")
			touchMarker("consumer-reverted-" + namespace)
			appendEnvFile("OPERATION_LOG", "consumer-revert:"+namespace+"\n")
		}
		return 0
	}

	if containsArg(args, "reboot") {
		if !markerExists("talos-auth-"+node) || containsArg(args, "--drain") || !containsArg(args, "--wait") {
			return commandFailure(54, "unsafe Talos reboot")
		}
		appendTalosOperation("talos-reboot:" + node)
		if talosFailure(node, "reboot") {
			return commandFailure(49, "talos reboot failed")
		}
		touchMarker("talos-reboot-" + node)
		return 0
	}
	if containsSequence(args, "image", "remove") {
		if !containsAdjacent(args, "--namespace", "cri") {
			return commandFailure(93, "Talos image remove must use the cri namespace")
		}
		if markerExists("talos-auth-"+node) && !markerExists("talos-reboot-"+node) {
			return commandFailure(93, "credential-stale cache mutation")
		}
		if !markerExists("talos-auth-"+node) && os.Getenv("FAKE_TALOS_NODES_CURRENT") != "true" {
			return commandFailure(93, "image-only cache mutation lacks proof")
		}
		nodeName := fakeNodeName(node)
		if nodeName == "" || !markerExists("cordoned-"+nodeName) ||
			markerContent("cordon-owner-"+nodeName) == "" {
			return commandFailure(93, "Talos image removal lacked an owned Kubernetes cordon")
		}
		image := argumentAfter(args, "remove")
		operation := "talos-remove:" + node + ":" + image
		appendTalosOperation(operation)
		if node == os.Getenv("FAKE_TALOS_IMAGE_ABSENT_NODE") {
			touchMarker("talos-remove-" + node)
			return commandFailure(1, "rpc error: code = NotFound desc = image %s not found", image)
		}
		if talosFailure(node, "remove") {
			return commandFailure(49, "talos remove failed")
		}
		touchMarker("talos-remove-" + node)
		if nodeName == os.Getenv("FAKE_EXTERNAL_UNCORDON_AFTER_REMOVE_NODE") {
			removeMarker("cordoned-" + nodeName)
			appendEnvFile("OPERATION_LOG", "operator-uncordon-after-remove:"+nodeName+"\n")
		}
		return 0
	}
	if containsSequence(args, "image", "pull") {
		if !containsAdjacent(args, "--namespace", "cri") {
			return commandFailure(93, "Talos image pull must use the cri namespace")
		}
		if markerExists("talos-auth-"+node) && !markerExists("talos-reboot-"+node) {
			return commandFailure(93, "credential-stale pull")
		}
		if !markerExists("talos-auth-"+node) && os.Getenv("FAKE_TALOS_NODES_CURRENT") != "true" {
			return commandFailure(93, "image-only pull lacks proof")
		}
		if !markerExists("talos-remove-" + node) {
			return commandFailure(93, "cached image not removed")
		}
		nodeName := fakeNodeName(node)
		if nodeName == "" || !markerExists("cordoned-"+nodeName) ||
			markerContent("cordon-owner-"+nodeName) == "" {
			return commandFailure(93, "Talos image pull lacked an owned Kubernetes cordon")
		}
		image := argumentAfter(args, "pull")
		operation := "talos-pull:" + node + ":" + image
		appendTalosOperation(operation)
		if talosFailure(node, "pull") {
			return commandFailure(47, "talos pull failed with %s", os.Getenv("EXPECTED_PULL_TOKEN"))
		}
		touchMarker("talos-pull-" + node)
		if nodeName == os.Getenv("FAKE_EXTERNAL_UNCORDON_AFTER_PULL_NODE") {
			removeMarker("cordoned-" + nodeName)
			appendEnvFile("OPERATION_LOG", "operator-uncordon-after-pull:"+nodeName+"\n")
		}
		return 0
	}
	return commandFailure(93, "unexpected talosctl invocation")
}

func fakeRecoveryPhase(nodeName string) string {
	var recovery map[string]any
	if err := json.Unmarshal(
		[]byte(markerContent("cordon-recovery-"+nodeName)),
		&recovery,
	); err != nil {
		return ""
	}
	phase, _ := recovery["phase"].(string)
	return phase
}

func fakeKubectl(args []string) int {
	return fakeKubectlImplementation(args)
}

func hasCurlUserConfig(config string) bool {
	for _, line := range strings.Split(config, "\n") {
		if strings.HasPrefix(line, "user = ") {
			return true
		}
	}
	return false
}

func appendTalosOperation(operation string) {
	appendEnvFile("TALOS_LOG", operation+"\n")
	appendEnvFile("OPERATION_LOG", operation+"\n")
}

func talosFailure(node, operation string) bool {
	return node == os.Getenv("FAKE_TALOS_FAIL_NODE") && operation == defaultString(os.Getenv("FAKE_TALOS_FAIL_OPERATION"), "auth")
}

func containsSequence(args []string, sequence ...string) bool {
	if len(sequence) == 0 || len(sequence) > len(args) {
		return false
	}
	for start := 0; start <= len(args)-len(sequence); start++ {
		matches := true
		for offset := range sequence {
			if args[start+offset] != sequence[offset] {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func containsAdjacent(args []string, key, value string) bool {
	return containsSequence(args, key, value)
}

func containsArg(args []string, target string) bool {
	for _, argument := range args {
		if argument == target {
			return true
		}
	}
	return false
}

func flagValue(args []string, flag string) string {
	for index, argument := range args {
		if argument == flag && index+1 < len(args) {
			return args[index+1]
		}
		if strings.HasPrefix(argument, flag+"=") {
			return strings.TrimPrefix(argument, flag+"=")
		}
	}
	return ""
}

func argumentAfter(args []string, target string) string {
	for index, argument := range args {
		if argument == target && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

func requiredNext(args []string, index *int) string {
	if *index+1 >= len(args) {
		*index = len(args)
		return ""
	}
	value := args[*index+1]
	*index += 2
	return value
}

func commandFailure(code int, format string, values ...any) int {
	fmt.Fprintf(os.Stderr, format+"\n", values...)
	return code
}

func mustWriteEnvFile(name, content string) {
	mustWriteCommandFile(os.Getenv(name), content)
}

func appendEnvFile(name, content string) {
	path := os.Getenv(name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		panic(fmt.Sprintf("open %s: %v", path, err))
	}
	defer func() {
		if err := file.Close(); err != nil {
			panic(fmt.Sprintf("close %s: %v", path, err))
		}
	}()
	if _, err := file.WriteString(content); err != nil {
		panic(fmt.Sprintf("append %s: %v", path, err))
	}
}

func mustWriteCommandFile(path, content string) {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		panic(fmt.Sprintf("write %s: %v", path, err))
	}
}

func mustReadCommandFile(path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("read %s: %v", path, err))
	}
	return string(content)
}

func copyFile(source, destination string) error {
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(destination, content, 0o600)
}

func touchMarker(name string) {
	mustWriteCommandFile(filepath.Join(os.Getenv("FAKE_SYNC_STATE_DIR"), name), "")
}

func markerExists(name string) bool {
	_, err := os.Stat(filepath.Join(os.Getenv("FAKE_SYNC_STATE_DIR"), name))
	return err == nil
}

func markerContent(name string) string {
	content, err := os.ReadFile(filepath.Join(os.Getenv("FAKE_SYNC_STATE_DIR"), name))
	if err != nil {
		return ""
	}
	return string(content)
}

func setMarkerContent(name, content string) {
	mustWriteCommandFile(filepath.Join(os.Getenv("FAKE_SYNC_STATE_DIR"), name), content)
}

func removeMarker(name string) {
	_ = os.Remove(filepath.Join(os.Getenv("FAKE_SYNC_STATE_DIR"), name))
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func encodeJSON(value any) string {
	content, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(content)
}

func parseInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
