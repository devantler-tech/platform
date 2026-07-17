package refreshfluxghcrauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type jsonPatchOperation struct {
	Operation string `json:"op"`
	Path      string `json:"path"`
	Value     any    `json:"value"`
}

func fakeKubectlImplementation(args []string) int {
	if flagValue(args, "--context") != "admin@prod" {
		return commandFailure(91, "kubectl must use the production context")
	}
	if calledFile := os.Getenv("KUBECTL_CALLED"); calledFile != "" {
		mustWriteCommandFile(calledFile, "")
	}

	namespace := flagValue(args, "--namespace")
	patchFile := flagValue(args, "--patch-file")
	manifestFile := flagValue(args, "--filename")
	if manifestFile == "" {
		manifestFile = flagValue(args, "-f")
	}

	switch {
	case containsSequence(args, "get", "nodes"):
		return fakeKubectlGetNodes()
	case containsSequence(args, "get", "pods"):
		return fakeKubectlGetPods(args)
	case containsSequence(args, "get", "node"):
		return fakeKubectlGetNode(args)
	case containsArg(args, "drain"):
		return fakeKubectlDrain(args)
	case containsArg(args, "uncordon"):
		return fakeKubectlUncordon(args)
	case containsSequence(args, "patch", "node") && containsArg(args, "--type=json"):
		return fakeKubectlPatchNode(args, patchFile)
	case containsArg(args, "cordon"):
		return fakeKubectlCordon(args)
	case containsArg(args, "wait"):
		return fakeKubectlWaitForNode(args)
	case containsArg(args, "create") && manifestFile != "":
		return fakeKubectlCreateRuntimeProbe(namespace, manifestFile)
	case containsSequence(args, "get", "pod"):
		return fakeKubectlGetRuntimeProbe(args)
	case containsSequence(args, "delete", "pod"):
		return fakeKubectlDeleteRuntimeProbe(args)
	}

	if namespace == "" {
		return commandFailure(91, "namespaced kubectl invocation omitted namespace")
	}

	switch {
	case containsSequence(args, "get", "secret", "ksail-registry-credentials") && containsSequence(args, "-o", "json"):
		return fakeKubectlGetRootSecret()
	case containsArg(args, "api-resources"):
		return fakeKubectlAPIResources(args)
	case containsSequence(args, "patch", "secret", "ksail-registry-credentials"):
		return fakeKubectlPatchRootSecret(args, patchFile)
	case containsSequence(args, "get", "secret", "variables-base"):
		return fakeKubectlGetVariablesBase(args)
	case containsSequence(args, "patch", "secret", "variables-base"):
		return fakeKubectlPatchVariablesBase(args, patchFile)
	}

	kind, name := fanoutResource(args)
	if kind != "" {
		return fakeKubectlFanoutResource(args, namespace, kind, name)
	}
	if containsSequence(args, "get", "secret", "ghcr-auth") {
		return fakeKubectlGetConsumerSecret(namespace)
	}

	return commandFailure(91, "unexpected kubectl invocation: %s", strings.Join(args, " "))
}

func fakeKubectlGetNodes() int {
	if os.Getenv("FAKE_NODE_DISCOVERY_FAIL") == "true" {
		return commandFailure(46, "node discovery failed")
	}
	if custom := os.Getenv("FAKE_NODE_JSON"); custom != "" {
		fmt.Println(custom)
		return 0
	}

	revision := os.Getenv("EXPECTED_GHCR_REVISION")
	image := os.Getenv("EXPECTED_KSAIL_TARGET_IMAGE")
	verifiedImage := defaultString(os.Getenv("FAKE_TALOS_VERIFIED_IMAGE"), image)
	nodes := []any{
		fakeInventoryNode("prod-worker-1", "prod-worker-1-uid", "10.0.0.2", "198.51.100.2", false, revision, "", "", true),
		fakeInventoryNode("prod-control-plane-1", "prod-control-plane-1-uid", "10.0.0.1", "198.51.100.1", true, revision, "", "", true),
		fakeInventoryNode("prod-control-plane-2", "prod-control-plane-2-uid", "10.0.0.3", "198.51.100.3", true, revision, revision, image, false),
		fakeInventoryNode("prod-control-plane-3", "prod-control-plane-3-uid", "10.0.0.4", "198.51.100.4", true, revision, revision, image, false),
	}
	if os.Getenv("FAKE_ALL_TALOS_NODES_STALE") == "true" {
		for _, node := range nodes {
			nodeMap := node.(map[string]any)
			metadata := nodeMap["metadata"].(map[string]any)
			annotations := metadata["annotations"].(map[string]any)
			delete(annotations, "platform.devantler.tech/ghcr-pull-verified-revision-v2")
			delete(annotations, "platform.devantler.tech/ghcr-pull-verified-image-v2")
		}
	}
	if bootstrapWorker := os.Getenv("FAKE_BOOTSTRAP_WORKER_NAME"); bootstrapWorker != "" {
		verifiedRevision := ""
		verifiedWorkerImage := ""
		if markerExists("talos-revision-10.0.0.5") {
			verifiedRevision = revision
			verifiedWorkerImage = image
		}
		nodes = append(nodes, fakeInventoryNode(
			bootstrapWorker,
			bootstrapWorker+"-uid",
			"10.0.0.5",
			"198.51.100.5",
			false,
			revision,
			verifiedRevision,
			verifiedWorkerImage,
			false,
		))
	}
	if os.Getenv("FAKE_TALOS_NODES_CURRENT") == "true" {
		setInventoryProof(nodes[0], revision, verifiedImage)
		setInventoryProof(nodes[1], revision, verifiedImage)
	}
	if markerExists("talos-revision-10.0.0.2") {
		setInventoryProof(nodes[0], revision, image)
	}
	if markerExists("talos-revision-10.0.0.1") {
		setInventoryProof(nodes[1], revision, image)
	}
	for _, node := range nodes {
		nodeMap := node.(map[string]any)
		status := nodeMap["status"].(map[string]any)
		addresses := status["addresses"].([]any)
		internalIP := addresses[0].(map[string]any)["address"].(string)
		if markerExists("talos-revision-" + internalIP) {
			setInventoryProof(node, revision, image)
		}
		if markerExists("talos-reboot-" + internalIP) {
			status["conditions"] = []any{map[string]any{"type": "Ready", "status": "True"}}
		}
	}

	newNodeName := ""
	if configured := os.Getenv("FAKE_NODE_APPEARS_AFTER_ROLL"); configured != "" &&
		markerExists("talos-revision-10.0.0.2") && markerExists("talos-revision-10.0.0.1") {
		newNodeName = configured
	} else if configured := os.Getenv("FAKE_NODE_APPEARS_DURING_SECOND_FANOUT"); configured != "" &&
		parseInt(markerContent("variables-patch-count"), 0) >= 2 {
		newNodeName = configured
	}
	if newNodeName != "" {
		verifiedRevision := ""
		newNodeImage := ""
		if markerExists("talos-revision-10.0.0.5") {
			verifiedRevision = revision
			newNodeImage = image
		}
		nodes = append(nodes, fakeInventoryNode(
			newNodeName,
			newNodeName+"-uid",
			"10.0.0.5",
			"198.51.100.5",
			false,
			revision,
			verifiedRevision,
			newNodeImage,
			true,
		))
	}

	fmt.Println(encodeJSON(map[string]any{"items": nodes}))
	return 0
}

func fakeInventoryNode(
	name string,
	uid string,
	internalIP string,
	externalIP string,
	controlPlane bool,
	desiredRevision string,
	verifiedRevision string,
	verifiedImage string,
	omitReady bool,
) map[string]any {
	labels := map[string]any{}
	if controlPlane {
		labels["node-role.kubernetes.io/control-plane"] = ""
	}
	annotations := map[string]any{
		"platform.devantler.tech/ghcr-pull-desired-revision": desiredRevision,
	}
	if verifiedRevision != "" {
		annotations["platform.devantler.tech/ghcr-pull-verified-revision-v2"] = verifiedRevision
	}
	if verifiedImage != "" {
		annotations["platform.devantler.tech/ghcr-pull-verified-image-v2"] = verifiedImage
	}
	status := map[string]any{
		"addresses": []any{
			map[string]any{"type": "InternalIP", "address": internalIP},
			map[string]any{"type": "ExternalIP", "address": externalIP},
		},
	}
	if !omitReady {
		status["conditions"] = []any{map[string]any{"type": "Ready", "status": "True"}}
	}
	cordoned := wordListContains(os.Getenv("FAKE_CORDONED_NODES"), name) || markerExists("cordoned-"+name)
	return map[string]any{
		"metadata": map[string]any{
			"name":        name,
			"uid":         uid,
			"labels":      labels,
			"annotations": annotations,
		},
		"spec": map[string]any{
			"unschedulable": cordoned,
		},
		"status": status,
	}
}

func fakeKubectlGetPods(args []string) int {
	nodeName := flagValue(args, "--field-selector")
	nodeName = strings.TrimPrefix(nodeName, "spec.nodeName=")
	if nodeName == "" || (!containsSequence(args, "-o", "json") && !containsArg(args, "-o=json")) {
		return commandFailure(91, "pod inventory must select one node as JSON")
	}
	items := []any{
		map[string]any{
			"metadata": map[string]any{
				"name":            "cilium-" + nodeName,
				"ownerReferences": []any{map[string]any{"kind": "DaemonSet"}},
			},
			"status": map[string]any{"phase": "Running"},
		},
	}
	if !wordListContains(os.Getenv("FAKE_EMPTY_WORKLOAD_NODES"), nodeName) {
		items = append(items, map[string]any{
			"metadata": map[string]any{
				"name":            "workload-" + nodeName,
				"ownerReferences": []any{map[string]any{"kind": "ReplicaSet"}},
			},
			"status": map[string]any{"phase": "Running"},
		})
	}
	fmt.Println(encodeJSON(map[string]any{"items": items}))
	return 0
}

func setInventoryProof(node any, revision, image string) {
	nodeMap := node.(map[string]any)
	metadata := nodeMap["metadata"].(map[string]any)
	annotations := metadata["annotations"].(map[string]any)
	annotations["platform.devantler.tech/ghcr-pull-verified-revision-v2"] = revision
	annotations["platform.devantler.tech/ghcr-pull-verified-image-v2"] = image
}

func fakeKubectlGetNode(args []string) int {
	nodeName := argumentAfter(args, "node")
	if nodeName == "" {
		return commandFailure(91, "node target missing")
	}
	if !containsSequence(args, "--output", "json") && !containsArg(args, "--output=json") {
		if wordListContains(os.Getenv("FAKE_CORDONED_NODES"), nodeName) {
			fmt.Print("true")
		}
		return 0
	}

	nodeUID := nodeName + "-uid"
	nodeIP, controlPlane := fakeNodeAddress(nodeName)
	if nodeName == os.Getenv("FAKE_NODE_REPLACED_BEFORE_PROCESS_NODE") {
		nodeUID = nodeName + "-replacement-uid"
		nodeIP = "10.0.0.99"
	}
	if nodeName == os.Getenv("FAKE_NODE_REPLACED_AFTER_READY_NODE") && markerExists("ready-"+nodeName) {
		nodeUID = nodeName + "-replacement-uid"
		nodeIP = "10.0.0.99"
	}
	if nodeName == os.Getenv("FAKE_NODE_REPLACED_AFTER_UNCORDON_NODE") && markerExists("uncordoned-"+nodeName) {
		nodeUID = nodeName + "-replacement-uid"
		nodeIP = "10.0.0.99"
	}
	if nodeName == os.Getenv("FAKE_NODE_IP_CHANGED_AFTER_DRAIN_NODE") && markerExists("drained-"+nodeName) {
		nodeIP = "10.0.0.99"
	}

	labels := map[string]any{}
	if controlPlane {
		labels["node-role.kubernetes.io/control-plane"] = ""
	}
	annotations := map[string]any{}
	if owner := markerContent("cordon-owner-" + nodeName); owner != "" {
		annotations["platform.devantler.tech/ghcr-auth-drain-owner"] = owner
	}
	cordoned := wordListContains(os.Getenv("FAKE_CORDONED_NODES"), nodeName) || markerExists("cordoned-"+nodeName)
	if nodeName == os.Getenv("FAKE_EXTERNAL_UNCORDON_AFTER_READY_NODE") && markerExists("ready-"+nodeName) {
		cordoned = false
	}
	taints := make([]any, 0, 2)
	if cordoned {
		taints = append(taints, map[string]any{
			"key":    "node.kubernetes.io/unschedulable",
			"effect": "NoSchedule",
		})
	}
	if markerExists("autoscaler-cordon-" + nodeName) {
		taints = append(taints, map[string]any{
			"key":    "ToBeDeletedByClusterAutoscaler",
			"effect": "NoSchedule",
		})
	}
	if markerExists("ready-"+nodeName) &&
		(nodeName == os.Getenv("FAKE_TRANSIENT_LIFECYCLE_TAINT_AFTER_READY_NODE") ||
			nodeName == os.Getenv("FAKE_PERSISTENT_LIFECYCLE_TAINT_AFTER_READY_NODE")) {
		readMarker := "post-ready-node-read-count-" + nodeName
		readCount := parseInt(markerContent(readMarker), 0) + 1
		setMarkerContent(readMarker, strconv.Itoa(readCount))
		if readCount == 1 || nodeName == os.Getenv("FAKE_PERSISTENT_LIFECYCLE_TAINT_AFTER_READY_NODE") {
			taints = append(taints,
				map[string]any{
					"key":    "node.kubernetes.io/not-ready",
					"effect": "NoSchedule",
				},
				map[string]any{
					"key":    "node.kubernetes.io/unreachable",
					"effect": "NoExecute",
				},
			)
		}
	}
	resourceVersion := defaultString(markerContent("resource-version-"+nodeName), "10")
	node := map[string]any{
		"metadata": map[string]any{
			"name":              nodeName,
			"uid":               nodeUID,
			"labels":            labels,
			"resourceVersion":   resourceVersion,
			"deletionTimestamp": nil,
			"annotations":       annotations,
		},
		"spec": map[string]any{
			"unschedulable": cordoned,
			"taints":        taints,
		},
		"status": map[string]any{
			"addresses":  []any{map[string]any{"type": "InternalIP", "address": nodeIP}},
			"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
		},
	}
	fmt.Println(encodeJSON(node))
	return 0
}

func fakeNodeAddress(nodeName string) (string, bool) {
	switch nodeName {
	case "prod-worker-1":
		return "10.0.0.2", false
	case "prod-control-plane-1":
		return "10.0.0.1", true
	case "prod-control-plane-2":
		return "10.0.0.3", true
	case "prod-control-plane-3":
		return "10.0.0.4", true
	default:
		return "10.0.0.5", false
	}
}

func fakeKubectlDrain(args []string) int {
	nodeName := argumentAfter(args, "drain")
	if nodeName == "" || !containsArg(args, "--ignore-daemonsets") ||
		!containsArg(args, "--delete-emptydir-data") || !containsArg(args, "--timeout=45m") ||
		containsArg(args, "--disable-eviction") || containsArg(args, "--force") {
		return commandFailure(55, "unsafe or incomplete kubectl drain flags")
	}
	appendEnvFile("OPERATION_LOG", "node-drain:"+nodeName+"\n")
	if !wordListContains(os.Getenv("FAKE_CORDONED_NODES"), nodeName) && !markerExists("cordoned-"+nodeName) {
		return commandFailure(55, "drain target was not cordoned")
	}
	if nodeName == os.Getenv("FAKE_DRAIN_API_FAIL_NODE") {
		return commandFailure(54, "could not list pods before eviction")
	}
	if nodeName == os.Getenv("FAKE_CORDON_OWNER_REPLACED_NODE") {
		setMarkerContent("cordon-owner-"+nodeName, "operator-cordon")
	}
	if nodeName == os.Getenv("FAKE_AUTOSCALER_CORDON_NODE") {
		touchMarker("autoscaler-cordon-" + nodeName)
	}
	if nodeName == os.Getenv("FAKE_DRAIN_FAIL_NODE") {
		return commandFailure(53, "cannot evict pod backstage-db-4: would violate PodDisruptionBudget backstage-db-primary")
	}
	touchMarker("drained-" + nodeName)
	if nodeName == os.Getenv("FAKE_EXTERNAL_UNCORDON_AFTER_DRAIN_NODE") {
		removeMarker("cordoned-" + nodeName)
		appendEnvFile("OPERATION_LOG", "operator-uncordon:"+nodeName+"\n")
	}
	return 0
}

func fakeKubectlUncordon(args []string) int {
	nodeName := argumentAfter(args, "uncordon")
	if nodeName == "" {
		return commandFailure(91, "uncordon target missing")
	}
	if nodeName == os.Getenv("FAKE_CORDON_OWNER_REPLACED_NODE") || nodeName == os.Getenv("FAKE_UNCORDON_FAIL_NODE") {
		return commandFailure(56, "cordon ownership changed; refusing to uncordon")
	}
	appendEnvFile("OPERATION_LOG", "node-uncordon:"+nodeName+"\n")
	return 0
}

func fakeKubectlPatchNode(args []string, patchFile string) int {
	nodeName := argumentAfter(args, "node")
	if nodeName == "" || patchFile == "" {
		return commandFailure(91, "node patch target or patch file missing")
	}
	var patch []jsonPatchOperation
	if err := json.Unmarshal([]byte(mustReadCommandFile(patchFile)), &patch); err != nil {
		return commandFailure(91, "parse node patch: %v", err)
	}
	currentResourceVersion := defaultString(markerContent("resource-version-"+nodeName), "10")
	isClaim := hasPatchOperation(patch, "add", "/spec/unschedulable", true)
	if isClaim {
		if nodeName == os.Getenv("FAKE_CORDON_BEFORE_CLAIM_NODE") {
			touchMarker("cordoned-" + nodeName)
			appendEnvFile("OPERATION_LOG", "operator-cordon:"+nodeName+"\n")
			return commandFailure(56, "resourceVersion test failed after concurrent cordon")
		}
		owner := patchValueString(patch, "add", "/metadata/annotations/platform.devantler.tech~1ghcr-auth-drain-owner")
		if owner == "" || markerExists("cordon-owner-"+nodeName) ||
			len(patch) == 0 || patch[0].Operation != "test" ||
			patch[0].Path != "/metadata/resourceVersion" || fmt.Sprint(patch[0].Value) != currentResourceVersion {
			return commandFailure(56, "invalid atomic cordon claim")
		}
		if !hasPatchOperation(patch, "test", "/metadata/uid", nodeName+"-uid") {
			return commandFailure(56, "atomic cordon claim omitted node UID")
		}
		setMarkerContent("cordon-owner-"+nodeName, owner)
		touchMarker("cordoned-" + nodeName)
		setMarkerContent("resource-version-"+nodeName, incrementDecimal(currentResourceVersion))
		appendEnvFile("OPERATION_LOG", "node-claim-cordon:"+nodeName+"\n")
		return 0
	}

	expectedOwner := ""
	if len(patch) > 0 {
		expectedOwner = fmt.Sprint(patch[0].Value)
	}
	if nodeName == os.Getenv("FAKE_UNCORDON_FAIL_NODE") || markerContent("cordon-owner-"+nodeName) != expectedOwner {
		return commandFailure(56, "cordon ownership changed; refusing to uncordon")
	}
	if len(patch) == 0 || patch[0].Operation != "test" ||
		patch[0].Path != "/metadata/annotations/platform.devantler.tech~1ghcr-auth-drain-owner" ||
		!hasPatchOperation(patch, "test", "/metadata/uid", nodeName+"-uid") ||
		!hasPatchOperation(patch, "test", "/metadata/resourceVersion", currentResourceVersion) ||
		!hasPatchOperation(patch, "add", "/spec/unschedulable", false) ||
		!hasPatchPath(patch, "remove", "/metadata/annotations/platform.devantler.tech~1ghcr-auth-drain-owner") {
		return commandFailure(56, "invalid atomic cordon release")
	}
	appendEnvFile("OPERATION_LOG", "node-uncordon:"+nodeName+"\n")
	setMarkerContent("resource-version-"+nodeName, incrementDecimal(currentResourceVersion))
	removeMarker("cordon-owner-" + nodeName)
	removeMarker("cordoned-" + nodeName)
	touchMarker("uncordoned-" + nodeName)
	return 0
}

func hasPatchOperation(patch []jsonPatchOperation, operation, path string, value any) bool {
	for _, item := range patch {
		if item.Operation == operation && item.Path == path && fmt.Sprint(item.Value) == fmt.Sprint(value) {
			return true
		}
	}
	return false
}

func hasPatchPath(patch []jsonPatchOperation, operation, path string) bool {
	for _, item := range patch {
		if item.Operation == operation && item.Path == path {
			return true
		}
	}
	return false
}

func patchValueString(patch []jsonPatchOperation, operation, path string) string {
	for _, item := range patch {
		if item.Operation == operation && item.Path == path {
			return fmt.Sprint(item.Value)
		}
	}
	return ""
}

func incrementDecimal(value string) string {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return value
	}
	return strconv.Itoa(parsed + 1)
}

func fakeKubectlCordon(args []string) int {
	nodeName := argumentAfter(args, "cordon")
	if nodeName == "" {
		return commandFailure(91, "cordon target missing")
	}
	appendEnvFile("OPERATION_LOG", "node-cordon:"+nodeName+"\n")
	return 0
}

func fakeKubectlWaitForNode(args []string) int {
	if !containsArg(args, "--for=condition=Ready") || !containsArg(args, "--timeout=10m") {
		return commandFailure(91, "unsafe node readiness wait")
	}
	nodeName := ""
	for _, argument := range args {
		if strings.HasPrefix(argument, "node/") {
			nodeName = strings.TrimPrefix(argument, "node/")
		}
	}
	if nodeName == "" {
		return commandFailure(91, "readiness target missing")
	}
	appendEnvFile("OPERATION_LOG", "node-ready:"+nodeName+"\n")
	if nodeName == os.Getenv("FAKE_NODE_READY_FAIL_NODE") {
		return commandFailure(50, "node did not become ready")
	}
	touchMarker("ready-" + nodeName)
	return 0
}

func fakeKubectlCreateRuntimeProbe(namespace, manifestFile string) int {
	if namespace != "ksail-operator" || manifestFile == "" {
		return commandFailure(91, "invalid runtime probe namespace or manifest")
	}
	var manifest map[string]any
	if err := json.Unmarshal([]byte(mustReadCommandFile(manifestFile)), &manifest); err != nil {
		return commandFailure(91, "parse runtime probe: %v", err)
	}
	metadata, _ := manifest["metadata"].(map[string]any)
	spec, _ := manifest["spec"].(map[string]any)
	containers, _ := spec["containers"].([]any)
	if manifest["kind"] != "Pod" || metadata["namespace"] != "ksail-operator" ||
		spec["automountServiceAccountToken"] != false || len(containers) != 1 ||
		len(anySlice(spec["imagePullSecrets"])) != 0 {
		return commandFailure(91, "unsafe runtime probe manifest")
	}
	container, _ := containers[0].(map[string]any)
	securityContext, _ := container["securityContext"].(map[string]any)
	image, _ := container["image"].(string)
	if (image != "ghcr.io/devantler-tech/wedding-app:latest" && image != "ghcr.io/devantler-tech/ascoachingogvaner:latest") ||
		container["imagePullPolicy"] != "Always" || securityContext["allowPrivilegeEscalation"] != false {
		return commandFailure(91, "runtime probe does not prove a private package pull")
	}
	probeName, _ := metadata["name"].(string)
	probeNode, _ := spec["nodeName"].(string)
	if probeName == "" || probeNode == "" {
		return commandFailure(91, "runtime probe name or node missing")
	}
	if wordListContains(
		os.Getenv("FAKE_RUNTIME_PROBE_CREATE_PERSIST_THEN_TIMEOUT_ONCE_NODES"),
		probeNode,
	) {
		attemptMarker := "runtime-probe-create-attempts-" + probeNode
		attempt := parseInt(markerContent(attemptMarker), 0) + 1
		setMarkerContent(attemptMarker, strconv.Itoa(attempt))
		if attempt == 1 {
			setMarkerContent("runtime-probe-"+probeName, probeNode+"\n"+image+"\n")

			return commandFailure(
				75,
				"Error from server (InternalError): failed calling webhook: context deadline exceeded",
			)
		}
	}
	if wordListContains(
		os.Getenv("FAKE_RUNTIME_PROBE_CREATE_TIMEOUT_ONCE_NODES"),
		probeNode,
	) && !markerExists("runtime-probe-create-timeout-once-"+probeNode) {
		touchMarker("runtime-probe-create-timeout-once-" + probeNode)

		return commandFailure(
			75,
			"Error from server (InternalError): failed calling webhook: context deadline exceeded",
		)
	}
	if wordListContains(os.Getenv("FAKE_RUNTIME_PROBE_CREATE_ALWAYS_FAIL_NODES"), probeNode) {
		return commandFailure(
			75,
			"Error from server (InternalError): failed calling webhook: context deadline exceeded",
		)
	}
	setMarkerContent("runtime-probe-"+probeName, probeNode+"\n"+image+"\n")
	fmt.Printf("pod/%s\n", probeName)
	return 0
}

func fakeKubectlGetRuntimeProbe(args []string) int {
	probeName := argumentAfter(args, "pod")
	contents := markerContent("runtime-probe-" + probeName)
	lines := strings.Split(strings.TrimSuffix(contents, "\n"), "\n")
	if probeName == "" || len(lines) < 2 {
		return commandFailure(91, "runtime probe state missing")
	}
	probeNode, probeImage := lines[0], lines[1]
	pullSecrets := []any{}
	if wordListContains(os.Getenv("FAKE_RUNTIME_PROBE_INJECT_PULL_SECRET_NODES"), probeNode) {
		pullSecrets = append(pullSecrets, map[string]any{"name": "injected-pull-secret"})
	}
	status := map[string]any{}
	probeIP, _ := fakeNodeAddress(probeNode)
	if (wordListContains(os.Getenv("FAKE_RUNTIME_PULL_FAIL_NODES"), probeNode) &&
		!markerExists("talos-reboot-"+probeIP)) ||
		wordListContains(os.Getenv("FAKE_RUNTIME_PULL_FAIL_IMAGES"), probeImage) {
		status["containerStatuses"] = []any{map[string]any{
			"state": map[string]any{"waiting": map[string]any{"reason": "ImagePullBackOff"}},
		}}
	} else {
		status["containerStatuses"] = []any{map[string]any{
			"imageID": "ghcr.io/private@sha256:runtime-probe",
			"state": map[string]any{
				"terminated": map[string]any{"reason": "Completed", "exitCode": 0},
			},
		}}
	}
	fmt.Println(encodeJSON(map[string]any{
		"spec":   map[string]any{"imagePullSecrets": pullSecrets},
		"status": status,
	}))
	return 0
}

func fakeKubectlDeleteRuntimeProbe(args []string) int {
	probeName := argumentAfter(args, "pod")
	if probeName == "" {
		return commandFailure(91, "runtime probe delete target missing")
	}
	removeMarker("runtime-probe-" + probeName)
	fmt.Printf("pod %q deleted\n", probeName)
	return 0
}

func fakeKubectlGetRootSecret() int {
	token := defaultString(os.Getenv("FAKE_CURRENT_ROOT_TOKEN"), "previous-runtime-token")
	config := encodeJSON(map[string]any{
		"auths": map[string]any{
			"ghcr.io": map[string]any{"username": "devantler", "password": token},
		},
	})
	encoded := base64.StdEncoding.EncodeToString([]byte(config))
	fmt.Println(encodeJSON(map[string]any{
		"data": map[string]any{".dockerconfigjson": encoded},
	}))
	return 0
}

func fakeKubectlAPIResources(args []string) int {
	if flagValue(args, "--api-group") != "external-secrets.io" {
		return commandFailure(91, "unexpected api-resources group")
	}
	if os.Getenv("FAKE_FANOUT_CRDS_ABSENT") != "true" {
		fmt.Println("externalsecrets.external-secrets.io")
		fmt.Println("pushsecrets.external-secrets.io")
	}
	return 0
}

func fakeKubectlPatchRootSecret(args []string, patchFile string) int {
	if !containsArg(args, "--type=merge") || patchFile == "" {
		return commandFailure(91, "invalid root secret patch")
	}
	if err := copyFile(patchFile, os.Getenv("PATCH_CAPTURE")); err != nil {
		return commandFailure(91, "capture root patch: %v", err)
	}
	appendEnvFile("OPERATION_LOG", "root-patch\n")
	if os.Getenv("FAKE_KUBECTL_FAIL") == "true" {
		return commandFailure(43, "cluster patch failed")
	}
	fmt.Println("secret/ksail-registry-credentials patched")
	return 0
}

func fakeKubectlGetVariablesBase(args []string) int {
	if !containsArg(args, "--ignore-not-found") {
		return commandFailure(91, "variables-base lookup must tolerate a fresh cluster")
	}
	if os.Getenv("FAKE_VARIABLES_BASE_ABSENT") != "true" {
		fmt.Println("secret/variables-base")
	}
	return 0
}

func fakeKubectlPatchVariablesBase(args []string, patchFile string) int {
	if !containsArg(args, "--type=merge") || patchFile == "" {
		return commandFailure(91, "invalid variables-base patch")
	}
	if err := copyFile(patchFile, os.Getenv("VARIABLES_PATCH_CAPTURE")); err != nil {
		return commandFailure(91, "capture variables-base patch: %v", err)
	}
	count := parseInt(markerContent("variables-patch-count"), 0) + 1
	setMarkerContent("variables-patch-count", strconv.Itoa(count))
	appendEnvFile("OPERATION_LOG", "variables-patch\n")
	fmt.Println("secret/variables-base patched")
	return 0
}

func fanoutResource(args []string) (string, string) {
	if containsSequence(args, "pushsecret", "seed-ghcr") {
		return "pushsecret", "seed-ghcr"
	}
	if containsSequence(args, "externalsecret", "ghcr-auth") {
		return "externalsecret", "ghcr-auth"
	}
	return "", ""
}

func fakeKubectlFanoutResource(args []string, namespace, kind, name string) int {
	resource := kind + "/" + namespace + "/" + name
	missingResource := os.Getenv("FAKE_MISSING_FANOUT_RESOURCE")
	if containsArg(args, "--ignore-not-found") && containsSequence(args, "get", kind, name) {
		if resource != missingResource {
			fmt.Printf("%s/%s\n", kind, name)
		}
		return 0
	}
	if resource == missingResource {
		return commandFailure(44, "%s/%s not found", kind, name)
	}
	if containsSequence(args, "get", kind, name) {
		markerName := kind + "-" + namespace + "-" + name
		refreshTime := "2026-07-13T00:00:00Z"
		resourceVersion := "1"
		if markerExists(markerName + "-annotated") {
			resourceVersion = "2"
		}
		if markerExists(markerName) && os.Getenv("FAKE_SYNC_SAME_REFRESH_TIME") != "true" {
			refreshTime = "2026-07-13T00:00:01Z"
		}
		if markerExists(markerName) {
			resourceVersion = "3"
		}
		fmt.Println(encodeJSON(map[string]any{
			"metadata": map[string]any{"resourceVersion": resourceVersion},
			"status": map[string]any{
				"refreshTime": refreshTime,
				"conditions":  []any{map[string]any{"type": "Ready", "status": "True"}},
			},
		}))
		return 0
	}
	if containsSequence(args, "annotate", kind, name) {
		appendEnvFile("FANOUT_LOG", resource+"\n")
		appendEnvFile("OPERATION_LOG", "fanout:"+resource+"\n")
		markerName := kind + "-" + namespace + "-" + name
		touchMarker(markerName + "-annotated")
		if resource != os.Getenv("FAKE_SYNC_STALL_RESOURCE") {
			touchMarker(markerName)
		}
		fmt.Println(`{"metadata":{"resourceVersion":"2"}}`)
		return 0
	}
	return commandFailure(91, "unexpected fanout resource invocation")
}

func fakeKubectlGetConsumerSecret(namespace string) int {
	variablesPatchCount := parseInt(markerContent("variables-patch-count"), 0)
	revertedMarker := "consumer-reverted-" + namespace
	mismatch := namespace == os.Getenv("FAKE_CONSUMER_MISMATCH_NAMESPACE") ||
		(namespace == os.Getenv("FAKE_CONSUMER_MISMATCH_ON_SECOND_PASS_NAMESPACE") && variablesPatchCount >= 2) ||
		(markerExists(revertedMarker) && variablesPatchCount < 3)
	encoded := ""
	if mismatch {
		encoded = base64.StdEncoding.EncodeToString([]byte(`{"auths":{}}`))
	} else {
		var patch map[string]any
		if err := json.Unmarshal([]byte(mustReadCommandFile(os.Getenv("VARIABLES_PATCH_CAPTURE"))), &patch); err != nil {
			return commandFailure(91, "parse variables-base patch: %v", err)
		}
		data, _ := patch["data"].(map[string]any)
		encoded, _ = data["ghcr_dockerconfigjson"].(string)
		if variablesPatchCount >= 3 {
			removeMarker(revertedMarker)
		}
	}
	fmt.Println(encodeJSON(map[string]any{
		"data": map[string]any{".dockerconfigjson": encoded},
	}))
	return 0
}

func wordListContains(list, target string) bool {
	for _, item := range strings.Fields(list) {
		if item == target {
			return true
		}
	}
	return false
}

func anySlice(value any) []any {
	if value == nil {
		return nil
	}
	items, _ := value.([]any)
	return items
}
