package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
)

func verifyPod(desired, actual []byte, runtimeImage string) error {
	var expected, observed map[string]any
	if json.Unmarshal(desired, &expected) != nil || json.Unmarshal(actual, &observed) != nil {
		return errors.New("invalid pod readback")
	}
	want, ok := expected["spec"].(map[string]any)
	if !ok {
		return errors.New("missing desired spec")
	}
	got, ok := observed["spec"].(map[string]any)
	if !ok {
		return errors.New("missing actual spec")
	}
	if err := normalizePodSpec(want); err != nil {
		return err
	}
	if err := normalizePodSpec(got); err != nil {
		return err
	}
	if !reflect.DeepEqual(want, got) {
		return errors.New("admitted pod changed runtime fields")
	}
	digest := strings.TrimPrefix(runtimeImage, "ghcr.io/devantler-tech/ksail-analysis-runner@sha256:")
	decoded, decodeErr := hex.DecodeString(digest)
	if digest == runtimeImage || decodeErr != nil || len(decoded) != 32 {
		return errors.New("unproven runtime image")
	}
	status, ok := observed["status"].(map[string]any)
	if !ok {
		return errors.New("missing runtime status")
	}
	for _, pair := range [][2]string{{"containers", "containerStatuses"}, {"initContainers", "initContainerStatuses"}} {
		containers, ok := got[pair[0]].([]any)
		if !ok || len(containers) != 1 {
			return errors.New("unexpected container count")
		}
		statuses, ok := status[pair[1]].([]any)
		if !ok || len(statuses) != 1 {
			return errors.New("incomplete runtime status")
		}
		c := containers[0].(map[string]any)
		s, ok := statuses[0].(map[string]any)
		imageID := strings.TrimPrefix(stringField(s, "imageID"), "docker-pullable://")
		// Containerd may report the requested index identity. Both representations
		// are bound to the verified signed index and its unique linux/amd64 child;
		// the caller separately verifies the actual node architecture.
		requested := stringField(c, "image")
		if !strings.HasPrefix(requested, "ghcr.io/devantler-tech/ksail-analysis-runner@") ||
			!validDigest(strings.TrimPrefix(requested, "ghcr.io/devantler-tech/ksail-analysis-runner@")) {
			return errors.New("unproven requested image")
		}
		if !ok || s["name"] != c["name"] || s["restartCount"] != float64(0) ||
			(imageID != runtimeImage && imageID != requested) {
			return errors.New("runtime identity mismatch")
		}
		if pair[0] == "containers" {
			if s["ready"] != true {
				return errors.New("runtime not ready")
			}
		} else {
			state, ok := s["state"].(map[string]any)
			if !ok {
				return errors.New("missing init result")
			}
			terminated, ok := state["terminated"].(map[string]any)
			if !ok || terminated["exitCode"] != float64(0) {
				return errors.New("init failed")
			}
		}
	}
	return nil
}

func stringField(m map[string]any, key string) string { value, _ := m[key].(string); return value }

// Kubernetes core/v1 defaults and the defaulttolerationseconds admission plugin
// add these exact values. Unknown fields and every nondefault value remain in the
// comparison, including added containers, credentials, mounts and permissions.
func normalizePodSpec(spec map[string]any) error {
	delete(spec, "nodeName") // Scheduler assignment is verified separately against the dedicated node.
	for key, value := range map[string]any{
		"dnsPolicy": "ClusterFirst", "schedulerName": "default-scheduler",
		"terminationGracePeriodSeconds": float64(30), "enableServiceLinks": true,
		"priority": float64(0), "preemptionPolicy": "PreemptLowerPriority",
	} {
		removeDefault(spec, key, value)
	}
	if alias, exists := spec["serviceAccount"]; exists {
		if alias != spec["serviceAccountName"] {
			return errors.New("service account alias mismatch")
		}
		delete(spec, "serviceAccount")
	}
	if tolerations, ok := spec["tolerations"].([]any); ok {
		filtered := make([]any, 0, len(tolerations))
		for _, item := range tolerations {
			t, ok := item.(map[string]any)
			if !ok {
				return errors.New("invalid toleration")
			}
			if len(t) == 4 && (t["key"] == "node.kubernetes.io/not-ready" || t["key"] == "node.kubernetes.io/unreachable") &&
				t["operator"] == "Exists" && t["effect"] == "NoExecute" && t["tolerationSeconds"] == float64(300) {
				continue
			}
			filtered = append(filtered, item)
		}
		spec["tolerations"] = filtered
	}
	for _, key := range []string{"containers", "initContainers"} {
		containers, ok := spec[key].([]any)
		if !ok {
			return errors.New("missing container specification")
		}
		for _, item := range containers {
			c, ok := item.(map[string]any)
			if !ok {
				return errors.New("invalid container")
			}
			removeDefault(c, "imagePullPolicy", "IfNotPresent")
			removeDefault(c, "terminationMessagePath", "/dev/termination-log")
			removeDefault(c, "terminationMessagePolicy", "File")
			for _, name := range []string{"readinessProbe", "livenessProbe", "startupProbe"} {
				if probe, ok := c[name].(map[string]any); ok {
					for field, value := range map[string]any{
						"timeoutSeconds": float64(1), "periodSeconds": float64(10),
						"successThreshold": float64(1), "failureThreshold": float64(3),
					} {
						removeDefault(probe, field, value)
					}
				}
			}
		}
	}
	return nil
}

func removeDefault(m map[string]any, key string, value any) {
	if reflect.DeepEqual(m[key], value) {
		delete(m, key)
	}
}
