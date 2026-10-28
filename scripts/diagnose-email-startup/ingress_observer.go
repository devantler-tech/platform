package main

import (
	"context"
	"encoding/json"
	"net/netip"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

type ingressObservation struct {
	Sources              []netip.Addr
	Public200, Public503 int
}

type ingressCluster struct{ UID, Revision, Name string }

func bindIngressCluster(raw []byte) (ingressCluster, error) {
	fields := map[string][]string{"": {"apiVersion", "kind", "metadata", "data"}, "metadata": {"name", "namespace", "uid", "resourceVersion", "deletionTimestamp"}, "data": {"cluster-name", "enable-hubble"}}
	var object struct {
		APIVersion, Kind string
		Metadata         struct {
			Name, Namespace, UID, ResourceVersion string
			DeletionTimestamp                     *string
		}
		Data map[string]string
	}
	if len(raw) == 0 || len(raw) > maxAPIOutput || strictjson.Validate(raw, fields) != nil || json.Unmarshal(raw, &object) != nil || object.APIVersion != "v1" || object.Kind != "ConfigMap" || object.Metadata.Name != "cilium-config" || object.Metadata.Namespace != "kube-system" || !uuidPattern.MatchString(object.Metadata.UID) || object.Metadata.ResourceVersion == "" || object.Metadata.DeletionTimestamp != nil || object.Data["enable-hubble"] != "true" || !nodeName.MatchString(object.Data["cluster-name"]) {
		return ingressCluster{}, errIngressEvidence
	}
	return ingressCluster{UID: object.Metadata.UID, Revision: object.Metadata.ResourceVersion, Name: object.Data["cluster-name"]}, nil
}

func ingressFlowFilter(destination netip.Addr) ([]byte, error) {
	// Cilium v1.20.3 monitor API: trace type 4, to-endpoint subtype 0.
	// Native TCP flag filters require the listed bits; a false ACK bit would
	// not exclude ACK packets. SYN, reply direction and the receiver port are
	// constrained here; the strict wire parser independently refuses SYN+ACK.
	return json.Marshal(map[string]any{
		"destination_ip": []string{destination.String()}, "destination_pod": []string{"private-mail/private-mail-0"},
		"destination_port": []string{"8080"}, "protocol": []string{"tcp"}, "source_identity": []uint32{8},
		"tcp_flags": []map[string]bool{{"SYN": true}}, "reply": []bool{false},
		"verdict": []string{"FORWARDED"}, "traffic_direction": []string{"INGRESS", "EGRESS"},
		"event_type": []map[string]any{{"type": 4, "match_sub_type": true, "sub_type": 0}},
	})
}

func ingressDaemonUID(raw []byte) (string, error) {
	fields := map[string][]string{"": {"apiVersion", "kind", "metadata"}, "metadata": {"name", "namespace", "uid", "deletionTimestamp"}}
	if len(raw) == 0 || len(raw) > maxAPIOutput || strictjson.Validate(raw, fields) != nil {
		return "", errIngressEvidence
	}
	var object struct {
		APIVersion, Kind string
		Metadata         struct {
			Name, Namespace, UID string
			DeletionTimestamp    *string
		}
	}
	if json.Unmarshal(raw, &object) != nil || object.APIVersion != "apps/v1" || object.Kind != "DaemonSet" || object.Metadata.Name != "cilium" || object.Metadata.Namespace != "kube-system" || !uuidPattern.MatchString(object.Metadata.UID) || object.Metadata.DeletionTimestamp != nil {
		return "", errIngressEvidence
	}
	return object.Metadata.UID, nil
}

// observeIngressSources performs one fixed, bounded read from an owned agent.
// Raw flows and addresses remain private and cannot choose a command or destination.
func observeIngressSources(ctx context.Context, run executor, app binding, addresses accessAddresses, probe func(context.Context) (int, error), pause func(context.Context) error, now func() time.Time) (ingressObservation, error) {
	refuse := func() (ingressObservation, error) { return ingressObservation{}, errIngressEvidence }
	if !uuidPattern.MatchString(app.UID) || !nodeName.MatchString(app.Node) || !addresses.Pod.IsGlobalUnicast() || addresses.Pod.Is4In6() || addresses.Pod.Zone() != "" {
		return refuse()
	}
	readCluster := func() (ingressCluster, error) {
		raw, err := request(ctx, run, "get", "configmap", "cilium-config", "-n", "kube-system", "-o", "json")
		if err != nil {
			return ingressCluster{}, errIngressEvidence
		}
		return bindIngressCluster(raw)
	}
	cluster, err := readCluster()
	if err != nil {
		return refuse()
	}
	readDaemon := func() (string, error) {
		raw, err := request(ctx, run, "get", "daemonset", "cilium", "-n", "kube-system", "-o", "json")
		if err != nil {
			return "", errIngressEvidence
		}
		return ingressDaemonUID(raw)
	}
	daemon, err := readDaemon()
	if err != nil {
		return refuse()
	}
	readAgent := func() (ingressAgent, error) {
		raw, err := request(ctx, run, "get", "pods", "-n", "kube-system", "-l", "k8s-app=cilium", "-o", "json")
		if err != nil {
			return ingressAgent{}, errIngressEvidence
		}
		return bindIngressAgent(raw, app.Node, daemon)
	}
	agent, err := readAgent()
	if err != nil {
		return refuse()
	}
	since := now().UTC()
	var result ingressObservation
	for attempt := 0; attempt < 2; attempt++ {
		status, err := probe(ctx)
		if err != nil {
			return refuse()
		}
		switch status {
		case 200:
			result.Public200++
		case 503:
			result.Public503++
		default:
			return refuse()
		}
	}
	if pause(ctx) != nil {
		return refuse()
	}
	until := now().UTC()
	if since.IsZero() || !until.After(since) || until.Sub(since) > 45*time.Second {
		return refuse()
	}
	filter, err := ingressFlowFilter(addresses.Pod)
	if err != nil {
		return refuse()
	}
	raw, err := request(ctx, run, "exec", "-n", "kube-system", agent.Name, "-c", "cilium-agent", "--", "hubble", "observe", "--server", "unix:///var/run/cilium/hubble.sock", "--timeout", "5s", "--since", since.Format(time.RFC3339Nano), "--until", until.Format(time.RFC3339Nano), "--allowlist", string(filter), "--output", "jsonpb")
	defer clear(raw)
	if err != nil {
		return refuse()
	}
	// Cilium omits its default cluster prefix in absolute flow node names.
	flowNode := app.Node
	if cluster.Name != "default" {
		flowNode = cluster.Name + "/" + app.Node
	}
	sources, err := decodeIngressFlows(raw, addresses.Pod, flowNode, since, until)
	if err != nil {
		return refuse()
	}
	after, err := readAgent()
	if err != nil || after != agent {
		return refuse()
	}
	daemonAfter, err := readDaemon()
	if err != nil || daemonAfter != daemon {
		return refuse()
	}
	clusterAfter, err := readCluster()
	if err != nil || clusterAfter != cluster {
		return refuse()
	}
	result.Sources = sources
	return result, nil
}
