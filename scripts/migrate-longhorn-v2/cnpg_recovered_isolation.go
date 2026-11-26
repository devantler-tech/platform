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
// primary. When Hubble has no sample, require a positive control to the
// same primary alongside the independently verified Cilium policy.
func (p *cnpgProofRun) observeRecoveredNetworkDenial(ctx context.Context, recoveredCluster, recovered object, datapath recoveryCiliumDatapath) error {
	source := p.record.Plan.Source
	recoveredID := benchmarkIdentity(recovered)
	sourceIP, err := recoveryAddress(text(recovered, "status", "podIP"))
	if err != nil || recoveredID.UID != datapath.Endpoint.PodUID || !slices.Contains(strings.Split(datapath.Endpoint.Addresses, ","), sourceIP) ||
		!supportedRecoveryCiliumConfig(datapath.Runtime) || datapath.Agent.Node != datapath.Endpoint.Node {
		return recoveredNetworkDenialStage("endpoint-binding")
	}
	readSource := func() (cnpgObservation, object, error) {
		observation, err := p.c.readCNPGObservation(ctx, cnpgSource{Name: source.Cluster, Namespace: source.Namespace})
		cluster := observation.Cluster
		if err != nil || text(cluster, "metadata", "uid") != source.ClusterUID ||
			value(cluster, "metadata", "generation") != float64(p.record.Archive.ClusterGeneration) {
			return cnpgObservation{}, nil, recoveredNetworkDenialStage("source-before")
		}
		primary := cnpgRecoveryPodByName(observation.Pods, text(cluster, "status", "currentPrimary"))
		if primary == nil {
			return cnpgObservation{}, nil, recoveredNetworkDenialStage("source-before")
		}
		return observation, primary, nil
	}
	before, primary, err := readSource()
	if err != nil {
		return err
	}
	cluster := before.Cluster
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
	result, err := p.c.probeRecoveryPostgresPort(ctx, recovered, destinationIP)
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
	var standby object
	switch err := observeRecoveredHubbleDenial(ctx, p.c, datapath.Agent.Pod, expected); err {
	case nil:
	case recoveredNetworkDenialStage("hubble-empty"):
		standby, err = p.verifyRecoveredNetworkControl(ctx, before, primary, sourceIP, destinationIP)
		if err != nil {
			return err
		}
	default:
		return err
	}
	after, latestPrimary, err := readSource()
	if err != nil || benchmarkIdentity(after.Cluster) != benchmarkIdentity(cluster) ||
		!sameCNPGRecoveryProbePod(latestPrimary, primary, cluster) {
		return recoveredNetworkDenialStage("source-after")
	}
	if standby != nil {
		current := cnpgRecoveryPodByName(after.Pods, text(standby, "metadata", "name"))
		if !sameCNPGRecoveryProbePod(current, standby, cluster) {
			return recoveredNetworkDenialStage("control-after")
		}
	}
	return nil
}

func cnpgRecoveryPodByName(pods []object, name string) object {
	for _, pod := range pods {
		if text(pod, "metadata", "name") == name {
			return pod
		}
	}
	return nil
}

func sameCNPGRecoveryProbePod(current, before, cluster object) bool {
	return current != nil && benchmarkIdentity(current) == benchmarkIdentity(before) &&
		verifyPostgresPod(current, before, cluster) == nil &&
		text(current, "metadata", "labels", "cnpg.io/instanceRole") == text(before, "metadata", "labels", "cnpg.io/instanceRole") &&
		text(current, "status", "podIP") == text(before, "status", "podIP")
}

func (c client) probeRecoveryPostgresPort(ctx context.Context, from object, destinationIP string) ([]byte, error) {
	id := benchmarkIdentity(from)
	return c.kubectl(ctx, nil, "exec", id.Name, "--namespace="+id.Namespace, "--container=postgres",
		"--pod-running-timeout=10s", "--", "/bin/sh", "-c", cnpgRecoveryPortCheck, "restore-port-check", destinationIP)
}

func (p *cnpgProofRun) verifyRecoveredNetworkControl(ctx context.Context, before cnpgObservation, primary object, recoveredIP, primaryIP string) (object, error) {
	// The source observation already requires all owned instances to be
	// healthy, independently verifies their roles and orders them by instance.
	index := slices.IndexFunc(before.Pods, func(pod object) bool {
		return text(pod, "metadata", "labels", "cnpg.io/instanceRole") == "replica"
	})
	if index < 0 {
		return nil, recoveredNetworkDenialStage("control-standby")
	}
	standby := before.Pods[index]
	id := benchmarkIdentity(standby)
	address, err := recoveryAddress(text(standby, "status", "podIP"))
	if err != nil || address == recoveredIP || address == primaryIP || id.UID == benchmarkIdentity(primary).UID {
		return nil, recoveredNetworkDenialStage("control-address")
	}
	if _, err := p.currentExecution(ctx); err != nil {
		return nil, recoveredNetworkDenialStage("control-binding")
	}
	current, err := p.c.get(ctx, "pod", id.Namespace, id.Name, false)
	if err != nil || !sameCNPGRecoveryProbePod(current, standby, before.Cluster) {
		return nil, recoveredNetworkDenialStage("control-before")
	}
	started := time.Now()
	result, err := p.c.probeRecoveryPostgresPort(ctx, standby, primaryIP)
	switch {
	case err != nil:
		return nil, recoveredNetworkDenialStage("control-command")
	case !commandContextActive(ctx) || time.Since(started) > 20*time.Second:
		return nil, recoveredNetworkDenialStage("control-deadline")
	case string(result) != "POSTGRES_PROBE=0\n":
		return nil, recoveredNetworkDenialStage("control-unreachable")
	}
	return standby, nil
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
