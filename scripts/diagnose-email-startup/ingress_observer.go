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
	filter, err := json.Marshal(map[string]any{
		"destination_ip": []string{addresses.Pod.String()}, "destination_pod": []string{"private-mail/private-mail-0"},
		"destination_port": []string{"8080"}, "protocol": []string{"tcp"}, "source_identity": []uint32{8},
	})
	if err != nil {
		return refuse()
	}
	raw, err := request(ctx, run, "exec", "-n", "kube-system", agent.Name, "-c", "cilium-agent", "--", "hubble", "observe", "--server", "unix:///var/run/cilium/hubble.sock", "--timeout", "5s", "--since", since.Format(time.RFC3339Nano), "--until", until.Format(time.RFC3339Nano), "--allowlist", string(filter), "--output", "jsonpb")
	defer clear(raw)
	if err != nil {
		return refuse()
	}
	sources, err := decodeIngressFlows(raw, addresses.Pod, "prod/"+app.Node, since, until)
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
	result.Sources = sources
	return result, nil
}
