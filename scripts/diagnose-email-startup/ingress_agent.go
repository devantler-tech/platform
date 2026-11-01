package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

const approvedCiliumAgentImage = "quay.io/cilium/cilium:v1.20.4@sha256:20fe92e4c86641233a450098c158d2d29085f3419931ab6bf681d2485dedf16e"
const approvedCiliumAgentID = "quay.io/cilium/cilium@sha256:20fe92e4c86641233a450098c158d2d29085f3419931ab6bf681d2485dedf16e"

type ingressAgent struct {
	Name, UID, Node, ContainerID, ImageID, StartedAt string
	Restarts                                         int
}

// bindIngressAgent admits exactly the pinned controller's running container on
// the application's bound node, never an execution target selected by a label alone.
func bindIngressAgent(raw []byte, node, daemonUID string) (ingressAgent, error) {
	refuse := func() (ingressAgent, error) { return ingressAgent{}, errIngressEvidence }
	fields := map[string][]string{
		"": {"apiVersion", "kind", "items"}, "items[]": {"apiVersion", "kind", "metadata", "spec", "status"},
		"items[].metadata":                                 {"name", "namespace", "uid", "resourceVersion", "labels", "ownerReferences", "deletionTimestamp"},
		"items[].metadata.labels":                          {"k8s-app"},
		"items[].metadata.ownerReferences[]":               {"apiVersion", "kind", "name", "uid", "controller"},
		"items[].spec":                                     {"nodeName", "hostNetwork", "serviceAccountName", "containers"},
		"items[].spec.containers[]":                        {"name", "image"},
		"items[].status":                                   {"phase", "containerStatuses"},
		"items[].status.containerStatuses[]":               {"name", "imageID", "containerID", "ready", "restartCount", "state"},
		"items[].status.containerStatuses[].state":         {"running", "waiting", "terminated"},
		"items[].status.containerStatuses[].state.running": {"startedAt"},
	}
	if !nodeName.MatchString(node) || !uuidPattern.MatchString(daemonUID) || len(raw) == 0 || len(raw) > maxAPIOutput || strictjson.Validate(raw, fields) != nil {
		return refuse()
	}
	var list struct {
		APIVersion, Kind string
		Items            []struct {
			APIVersion, Kind string
			Metadata         struct {
				Name, Namespace, UID, ResourceVersion string
				Labels                                map[string]string
				DeletionTimestamp                     *string
				OwnerReferences                       []struct {
					APIVersion, Kind, Name, UID string
					Controller                  bool
				}
			}
			Spec struct {
				NodeName, ServiceAccountName string
				HostNetwork                  bool
				Containers                   []container
			}
			Status struct {
				Phase             string
				ContainerStatuses []struct {
					Name, ContainerID, ImageID string
					Ready                      bool
					RestartCount               int
					State                      map[string]json.RawMessage
				}
			}
		}
	}
	if json.Unmarshal(raw, &list) != nil || list.APIVersion != "v1" || (list.Kind != "PodList" && list.Kind != "List") || len(list.Items) == 0 || len(list.Items) > 64 {
		return refuse()
	}
	var selected ingressAgent
	for _, pod := range list.Items {
		typed := pod.APIVersion == "v1" && pod.Kind == "Pod"
		if (list.Kind == "List" || pod.APIVersion != "" || pod.Kind != "") && !typed {
			return refuse()
		}
		if pod.Spec.NodeName != node {
			continue
		}
		if selected.Name != "" || !strings.HasPrefix(pod.Metadata.Name, "cilium-") || !nodeName.MatchString(pod.Metadata.Name) || pod.Metadata.Namespace != "kube-system" ||
			!uuidPattern.MatchString(pod.Metadata.UID) || pod.Metadata.ResourceVersion == "" || pod.Metadata.DeletionTimestamp != nil || pod.Metadata.Labels["k8s-app"] != "cilium" ||
			len(pod.Metadata.OwnerReferences) != 1 || !pod.Spec.HostNetwork || pod.Spec.ServiceAccountName != "cilium" || len(pod.Spec.Containers) != 1 ||
			pod.Spec.Containers[0].Name != "cilium-agent" || pod.Spec.Containers[0].Image != approvedCiliumAgentImage || pod.Status.Phase != "Running" || len(pod.Status.ContainerStatuses) != 1 {
			return refuse()
		}
		owner := pod.Metadata.OwnerReferences[0]
		if owner.APIVersion != "apps/v1" || owner.Kind != "DaemonSet" || owner.Name != "cilium" || owner.UID != daemonUID || !owner.Controller {
			return refuse()
		}
		status := pod.Status.ContainerStatuses[0]
		id, hasRuntime := strings.CutPrefix(status.ContainerID, "containerd://")
		if status.Name != "cilium-agent" || !status.Ready || status.RestartCount < 0 || status.ImageID != approvedCiliumAgentID || !hasRuntime || !digestPattern.MatchString(id) || len(status.State) != 1 {
			return refuse()
		}
		var running struct{ StartedAt string }
		if json.Unmarshal(status.State["running"], &running) != nil {
			return refuse()
		}
		when, err := time.Parse(time.RFC3339Nano, running.StartedAt)
		if err != nil || when.IsZero() {
			return refuse()
		}
		selected = ingressAgent{Name: pod.Metadata.Name, UID: pod.Metadata.UID, Node: node, ContainerID: status.ContainerID, ImageID: status.ImageID, StartedAt: running.StartedAt, Restarts: status.RestartCount}
	}
	if selected.Name == "" {
		return refuse()
	}
	return selected, nil
}
