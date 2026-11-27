package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestRecoveredPortProbeDiscardsInheritedConnectionSettings(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "pg_isready")
	const fixture = `#!/bin/sh
if [ "${PGHOSTADDR+x}${PGSERVICE+x}${PGPORT+x}" != "" ] ||
   [ "$*" != "-q -h 192.0.2.23 -p 5432 -t 3" ]; then
  exit 2
fi
`
	if err := os.WriteFile(executable, []byte(fixture), 0700); err != nil {
		t.Fatal(err)
	}
	c := client{context: "synthetic", run: func(ctx context.Context, _ []byte, program string, args ...string) ([]byte, error) {
		start := slices.Index(args, "--") + 1
		script := slices.Index(args, cnpgRecoveryPortCheck)
		if program != "kubectl" || start == 0 || script <= start {
			t.Fatal("port probe did not use the fixed client command")
		}
		command := slices.Clone(args[start:])
		command[script-start] = strings.Replace(command[script-start], "/usr/lib/postgresql/18/bin/pg_isready", strconv.Quote(executable), 1)
		process := exec.CommandContext(ctx, command[0], command[1:]...)
		process.Env = []string{"PATH=" + dir, "PGHOSTADDR=192.0.2.99", "PGSERVICE=other-server", "PGPORT=5433"}
		return process.Output()
	}}
	_, pods, _, _ := cnpgFixture("longhorn-wffc", "longhorn-wffc", "longhorn-wffc")
	result, err := c.probeRecoveryPostgresPort(t.Context(), pods[1], "192.0.2.23")
	if err != nil || string(result) != "POSTGRES_PROBE=0\n" {
		t.Fatalf("connection environment could change the destination: %q, %v", result, err)
	}
}

func recoveredDataCiliumFixture(t *testing.T, p *cnpgProofRun, pod object, scenario string) {
	t.Helper()
	native, input, _ := recoveryObservationFixture(t, nil)
	host := p.record.Preparation.Agent
	valueObject(input.Agent, "spec")["nodeName"] = host.Node
	valueObject(input.Node, "metadata")["name"] = host.Node
	valueObject(input.Node, "metadata")["uid"] = host.NodeUID
	valueObject(input.Node, "metadata")["labels"] = object{"kubernetes.io/hostname": host.Node}
	valueObject(input.Node, "status", "nodeInfo")["bootID"] = host.BootID
	endpoints, cep, endpointPod := recoveryEndpointFixture(t)
	value(cep, "metadata", "ownerReferences").([]any)[0].(object)["uid"] = text(pod, "metadata", "uid")
	for _, key := range []string{"podIP", "podIPs"} {
		value(pod, "status").(object)[key] = value(endpointPod, "status", key)
	}
	_, entries := isolationFixture(t, "umami-db")
	read := p.c.run
	policyReads := 0
	p.c.run = func(ctx context.Context, body []byte, program string, args ...string) ([]byte, error) {
		if program != "kubectl" || len(args) < 4 {
			return read(ctx, body, program, args...)
		}
		if args[2] == "exec" && slices.Contains(args, "cilium-dbg") {
			if slices.Contains(args, "policy") {
				policyReads++
				if policyReads > 2 && scenario == "policy changed during data check" {
					return isolationResponse(t, entries, 818), nil
				}
			}
			if slices.Contains(args, "endpoint") && policyReads > 2 && scenario == "policy changed during data check" {
				value(endpoints[0], "status", "policy", "realized").(object)["policy-revision"] = float64(818)
				return json.Marshal(endpoints)
			}
			return native.run(ctx, body, program, args...)
		}
		if args[2] == "get" {
			switch args[3] {
			case "nodes":
				return json.Marshal(object{"items": []object{input.Node}})
			case "pods":
				if slices.Contains(args, "--namespace=kube-system") {
					return json.Marshal(object{"items": []object{input.Agent}})
				}
				return json.Marshal(object{"items": []object{pod}})
			case "ciliumendpoints.cilium.io":
				return json.Marshal(cep)
			case "pod":
				if args[4] == text(pod, "metadata", "name") {
					return json.Marshal(pod)
				}
				return native.run(ctx, body, program, args...)
			case "node", "daemonset", "ciliumnetworkpolicies.cilium.io":
				return native.run(ctx, body, program, args...)
			}
		}
		return read(ctx, body, program, args...)
	}
}

func TestRecoveredDataBracketsTheFilesystemWithHistoryAndThreeV2Copies(t *testing.T) {
	for _, scenario := range []string{"original", "postmaster restarted", "different lineage", "copy replaced", "wrong filesystem",
		"policy changed during data check", "database port reachable", "missing egress denial", "source address changed",
		"reachable control", "control address changed", "contradictory Hubble"} {
		t.Run(scenario, func(t *testing.T) {
			p, reads := recoveredStorageObservationFixture(t, "")
			pod := newRecoveredStorageFixture(t).pod
			recoveredDataCiliumFixture(t, p, pod, scenario)
			archive := p.record.Plan.Source
			source, peers, _, _ := cnpgFixture("longhorn-wffc", "longhorn-wffc", "longhorn-wffc")
			valueObject(source, "metadata")["uid"] = archive.ClusterUID
			valueObject(source, "metadata")["generation"] = float64(p.record.Archive.ClusterGeneration)
			for _, peer := range peers {
				value(peer, "metadata", "ownerReferences").([]any)[0].(object)["uid"] = archive.ClusterUID
				valueObject(peer, "status")["containerStatuses"] = value(pod, "status", "containerStatuses")
			}
			const destinationIP = "10.201.2.210"
			valueObject(peers[0], "status")["podIP"] = destinationIP
			valueObject(peers[1], "status")["podIP"] = "10.201.2.212"
			barrier := p.record.Plan.Barrier
			timeline, err := strconv.Atoi(barrier.Timeline)
			if err != nil {
				t.Fatal(err)
			}
			postmaster := "2026-11-22 22:14:00.000000+00"
			var historyReads, probes, observers, controls int
			delegate := p.c.run
			p.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == "kubectl" && args[2] == "get" && slices.Contains(args, "--namespace="+archive.Namespace) {
					switch args[3] {
					case "cluster.postgresql.cnpg.io":
						if args[4] != archive.Cluster {
							t.Fatal("recovery probe selected a different production database")
						}
						return json.Marshal(source)
					case "pods":
						if observers > 0 && scenario == "source address changed" {
							valueObject(peers[0], "status")["podIP"] = "10.201.2.211"
						}
						if controls > 0 && scenario == "control address changed" {
							valueObject(peers[1], "status")["podIP"] = "10.201.2.213"
						}
						return json.Marshal(object{"items": peers})
					case "pod":
						if args[4] != text(peers[1], "metadata", "name") {
							t.Fatal("control read selected a different standby")
						}
						return json.Marshal(peers[1])
					}
				}
				if program == "kubectl" && args[2] == "exec" && !slices.Contains(args, "cilium-dbg") {
					switch {
					case slices.Contains(args, cnpgRecoveryPortCheck):
						if observers > 0 {
							controls++
							if args[3] != text(peers[1], "metadata", "name") || !slices.Contains(args, "--namespace="+archive.Namespace) ||
								args[len(args)-1] != destinationIP || probes != 1 {
								t.Fatal("positive control did not use the healthy source standby and the same primary")
							}
							if scenario == "missing egress denial" {
								return []byte("POSTGRES_PROBE=2\n"), nil
							}
							return []byte("POSTGRES_PROBE=0\n"), nil
						}
						probes++
						if args[3] != "restore-1" || !slices.Contains(args, "--namespace="+benchmarkIdentity(pod).Namespace) ||
							args[len(args)-1] != destinationIP {
							t.Fatal("native probe did not use the original recovered and source Pods")
						}
						if scenario == "database port reachable" {
							return []byte("POSTGRES_PROBE=0\n"), nil
						}
						return []byte("POSTGRES_PROBE=2\n"), nil
					case slices.Contains(args, "hubble"):
						observers++
						since := slices.Index(args, "--since")
						if probes != 1 || since < 0 || since+1 >= len(args) ||
							!slices.Contains(args, "unix:///var/run/cilium/hubble.sock") || !slices.Contains(args, "jsonpb") {
							t.Fatal("the proof did not use the bounded native observer after the probe")
						}
						if slices.Contains([]string{"missing egress denial", "reachable control", "control address changed"}, scenario) {
							return []byte{}, nil
						}
						event := object{"verdict": "DROPPED", "drop_reason_desc": "POLICY_DENIED", "traffic_direction": "EGRESS",
							"time": args[since+1], "node_name": cnpgRecoveryHostname(p.record.Plan),
							"source":      object{"namespace": benchmarkIdentity(pod).Namespace, "pod_name": "restore-1"},
							"destination": object{"namespace": archive.Namespace, "pod_name": text(peers[0], "metadata", "name")},
							"IP":          object{"source": text(pod, "status", "podIP"), "destination": destinationIP},
							"l4":          object{"TCP": object{"destination_port": 5432, "flags": object{"SYN": true}}}}
						if scenario == "contradictory Hubble" {
							event["traffic_direction"] = "INGRESS"
						}
						return json.Marshal(object{"flow": event})
					case slices.Contains(args, "--command="+postgresRecoverySQL):
						historyReads++
						response, _ := recoveredHistoryFixture()
						response["system_id"], response["timeline"] = barrier.SystemID, strconv.Itoa(timeline+1)
						response["flush_lsn"], response["postmaster_start"] = barrier.FlushLSN, postmaster
						response["timeline_history"] = barrier.Timeline + "\t" + barrier.FlushLSN + "\trecovery\n"
						if historyReads == 2 && scenario == "postmaster restarted" {
							response["postmaster_start"] = "2026-11-22 22:15:00.000000+00"
						}
						if historyReads == 2 && scenario == "different lineage" {
							response["system_id"] = "7470000000000000002"
						}
						return json.Marshal(response)
					case slices.Contains(args, "/usr/bin/stat"):
						return []byte("64768 10 directory\n64768 11 directory\n64768 12 directory\n64768 13 directory\n"), nil
					case slices.Contains(args, "/usr/bin/findmnt"):
						device := "253:0"
						if scenario == "wrong filesystem" {
							device = "253:1"
						}
						return []byte(`{"filesystems":[{"target":"/var/lib/postgresql/data","fsroot":"/","fstype":"ext4","maj:min":"` + device + `","id":60,"vfs-options":"rw,relatime"}]}`), nil
					case slices.Contains(args, "/usr/bin/find"):
						return []byte("d 64768 11\nd 64768 12\nf 64768 14\nd 64768 13\n"), nil
					default:
						t.Fatal("data observation selected an unexpected executable")
					}
				}
				raw, err := delegate(ctx, input, program, args...)
				if err == nil && program == "kubectl" && args[3] == "replicas.longhorn.io" && reads[args[3]] > 2 && scenario == "copy replaced" {
					var inventory object
					if json.Unmarshal(raw, &inventory) != nil {
						t.Fatal("the synthetic inventory is incomplete")
					}
					value(inventory["items"].([]any)[0].(object), "metadata").(object)["uid"] = "99999999-9999-4999-8999-999999999999"
					return json.Marshal(inventory)
				}
				return raw, err
			}
			got, err := p.observeRecoveredDatabaseData(t.Context())
			if scenario == "original" || scenario == "reachable control" {
				wantControls := 0
				if scenario == "reachable control" {
					wantControls = 1
				}
				if err != nil || historyReads != 2 || probes != 1 || observers != 1 || reads["replicas.longhorn.io"] != 4 || len(got.storage.Replicas) != 3 ||
					controls != wantControls || got.filesystem.Entries != 4 || got.history.PostmasterStart != postmaster ||
					got.datapath.PolicyRevision != 817 || got.datapath.Endpoint.PodUID != got.storage.Pod.UID {
					t.Fatalf("native data observation did not bind: %+v %v", got, err)
				}
			} else if err == nil || len(got.storage.Replicas) > 0 || got.history.SystemID != "" || got.filesystem.Device != 0 {
				t.Fatalf("unstable data observation escaped: %+v %v", got, err)
			}
			if scenario == "policy changed during data check" && (historyReads != 2 || err == nil ||
				err.Error() != "recovered database data observation held: phase=cilium-after") {
				t.Fatalf("realized policy drift was not detected across the completed data check: %v", err)
			}
			denialStage := map[string]string{
				"database port reachable": "probe-reachable",
				"missing egress denial":   "control-unreachable",
				"source address changed":  "source-after",
				"control address changed": "control-after",
			}[scenario]
			if denialStage != "" && (err == nil || err.Error() != "recovered database data observation held: phase=network-denial/"+denialStage) {
				t.Fatalf("unexpected network evidence failure: %v", err)
			}
			if scenario == "contradictory Hubble" && (controls != 0 || err == nil) {
				t.Fatalf("a contradictory Hubble response selected the fallback: %v", err)
			}
			if p.record.RestoreVerified || p.record.Cleaned || p.record.Phase != "cluster-created" {
				t.Fatal("read-only evidence changed recovery acceptance")
			}
		})
	}
}
