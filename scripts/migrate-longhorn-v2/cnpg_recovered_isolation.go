package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/hubblepolicy"
)

const cnpgRecoveryPortCheck = `pg_isready -q -h "$1" -p 5432 -t 3
printf 'POSTGRES_PROBE=%s\n' "$?"`

type recoveredNetworkDenialStage string

func (s recoveredNetworkDenialStage) Error() string { return string(s) }

// Verify that the isolated restore cannot reach the original PostgreSQL
// primary. A process timeout alone cannot distinguish policy from a bad target.
func (p *cnpgProofRun) observeRecoveredNetworkDenial(ctx context.Context, recoveredCluster, recovered object, datapath recoveryCiliumDatapath) error {
	source := p.record.Plan.Source
	recoveredID := benchmarkIdentity(recovered)
	sourceIP, err := recoveryAddress(text(recovered, "status", "podIP"))
	if err != nil || recoveredID.UID != datapath.Endpoint.PodUID || !slices.Contains(strings.Split(datapath.Endpoint.Addresses, ","), sourceIP) ||
		!supportedRecoveryCiliumConfig(datapath.Runtime) || datapath.Agent.Node != datapath.Endpoint.Node {
		return recoveredNetworkDenialStage("endpoint-binding")
	}
	readPrimary := func() (object, object, error) {
		observation, err := p.c.readCNPGObservation(ctx, cnpgSource{Name: source.Cluster, Namespace: source.Namespace})
		cluster := observation.Cluster
		if err != nil || text(cluster, "metadata", "uid") != source.ClusterUID ||
			value(cluster, "metadata", "generation") != float64(p.record.Archive.ClusterGeneration) {
			return nil, nil, recoveredNetworkDenialStage("source-before")
		}
		for _, pod := range observation.Pods {
			if text(pod, "metadata", "name") == text(cluster, "status", "currentPrimary") {
				return cluster, pod, nil
			}
		}
		return nil, nil, recoveredNetworkDenialStage("source-before")
	}
	cluster, primary, err := readPrimary()
	if err != nil {
		return err
	}
	original := benchmarkIdentity(primary)
	destinationIP, err := recoveryAddress(text(primary, "status", "podIP"))
	if err != nil || sourceIP == destinationIP || original.UID == recoveredID.UID {
		return recoveredNetworkDenialStage("endpoint-address")
	}
	if _, err := p.currentExecution(ctx); err != nil {
		return recoveredNetworkDenialStage("execution-binding")
	}
	current, err := p.c.get(ctx, "pod", recoveredID.Namespace, recoveredID.Name, false)
	if err != nil || benchmarkIdentity(current) != recoveredID || verifyPostgresPod(current, recovered, recoveredCluster) != nil ||
		!sameRecoveredProgram(recovered, current) || text(current, "status", "podIP") != text(recovered, "status", "podIP") {
		return recoveredNetworkDenialStage("recovered-before")
	}
	since := time.Now().UTC()
	result, err := p.c.kubectl(ctx, nil, "exec", recoveredID.Name, "--namespace="+recoveredID.Namespace, "--container=postgres",
		"--pod-running-timeout=10s", "--", "/bin/sh", "-c", cnpgRecoveryPortCheck, "restore-port-check", destinationIP)
	until := time.Now().UTC()
	if err != nil {
		return recoveredNetworkDenialStage("probe-command")
	}
	if !commandContextActive(ctx) || until.Sub(since) > 20*time.Second {
		return recoveredNetworkDenialStage("probe-deadline")
	}
	switch string(result) {
	case "POSTGRES_PROBE=2\n":
	case "POSTGRES_PROBE=0\n", "POSTGRES_PROBE=1\n":
		return recoveredNetworkDenialStage("probe-reachable")
	default:
		return recoveredNetworkDenialStage("probe-format")
	}
	expected := hubblepolicy.Denial{
		Source: hubblepolicy.Endpoint{Namespace: recoveredID.Namespace, Pod: recoveredID.Name}, SourceIP: sourceIP,
		Destination: hubblepolicy.Endpoint{Namespace: original.Namespace, Pod: original.Name}, DestinationIP: destinationIP,
		Port: 5432, Node: datapath.Agent.Node, Since: since, Until: until, RequireEgress: true,
	}
	if err := observeRecoveredHubbleDenial(ctx, p.c, datapath.Agent.Pod, expected); err != nil {
		return err
	}
	latestCluster, latestPrimary, err := readPrimary()
	if err != nil || benchmarkIdentity(latestCluster) != benchmarkIdentity(cluster) ||
		text(latestCluster, "status", "currentPrimary") != original.Name || benchmarkIdentity(latestPrimary) != original ||
		verifyPostgresPod(latestPrimary, primary, cluster) != nil || text(latestPrimary, "status", "podIP") != text(primary, "status", "podIP") {
		return recoveredNetworkDenialStage("source-after")
	}
	return nil
}

func observeRecoveredHubbleDenial(ctx context.Context, c client, agent string, expected hubblepolicy.Denial) error {
	filter, err := json.Marshal(map[string][]string{
		"source_ip": {expected.SourceIP}, "source_pod": {expected.Source.Namespace + "/" + expected.Source.Pod},
		"destination_ip": {expected.DestinationIP}, "destination_port": {"5432"}, "protocol": {"tcp"},
		"verdict": {"DROPPED"}, "drop_reason_desc": {"POLICY_DENIED"},
	})
	if err != nil {
		return recoveredNetworkDenialStage("hubble-filter")
	}
	result, err := c.kubectl(ctx, nil, "exec", agent, "--namespace=kube-system", "--container=cilium-agent", "--pod-running-timeout=10s",
		"--", "hubble", "observe", "--server", "unix:///var/run/cilium/hubble.sock", "--timeout", "5s",
		"--since", expected.Since.Format(time.RFC3339Nano), "--until", expected.Until.Format(time.RFC3339Nano), "--allowlist", string(filter), "--output", "jsonpb")
	if err != nil {
		return recoveredNetworkDenialStage("hubble-command")
	}
	if len(bytes.TrimSpace(result)) == 0 {
		return recoveredNetworkDenialStage("hubble-empty")
	}
	if err := hubblepolicy.VerifyTCPDenial(bytes.NewReader(result), expected); err != nil {
		var mismatch hubblepolicy.DenialFailure
		if errors.As(err, &mismatch) {
			return recoveredNetworkDenialStage("hubble-" + mismatch)
		}
		return recoveredNetworkDenialStage("hubble-stream")
	}
	return nil
}
