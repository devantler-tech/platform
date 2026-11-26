package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func recoveryObservationFixture(t *testing.T, change func(string, int, object)) (client, recoveryCiliumObservation, *int) {
	t.Helper()
	agent, ds, node := recoveryAgentFixture()
	_, cep, pod := recoveryEndpointFixture(t)
	policy, entries := isolationFixture(t, "umami-db")
	for _, o := range []object{agent, ds, node, pod, policy} {
		o["metadata"].(object)["resourceVersion"] = "100001"
	}
	pod["metadata"].(object)["ownerReferences"] = []any{object{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "name": "restore", "uid": "55555555-5555-4555-8555-555555555555", "controller": true}}
	input := recoveryCiliumObservation{Agent: agent, DaemonSet: ds, Node: node, Pod: pod, Policy: policy, ArchiveEndpoint: "https://" + strings.Repeat("a", 32) + ".r2.cloudflarestorage.com"}
	config := []byte(`{"ClusterName":"default","EnableNonDefaultDenyPolicies":true,"EnableL7Proxy":true,"RestoreState":true,"policy-enforcement":"default"}`)
	version := []byte(`{"Client":{"Version":"1.20.5","Revision":"15012141"},"Daemon":{"Version":"1.20.5","Revision":"15012141"}}`)
	endpoint, err := os.ReadFile("testdata/synthetic-endpoint-get-cilium-1.20.5.json")
	if err != nil {
		t.Fatal(err)
	}
	calls, counts := 0, map[string]int{}
	c := client{context: "admin@prod", run: func(_ context.Context, stdin []byte, command string, args ...string) ([]byte, error) {
		calls++
		if command != "kubectl" || stdin != nil || len(args) < 4 || args[0] != "--context=admin@prod" || args[1] != "--request-timeout=20s" {
			return nil, errors.New("unexpected observation command")
		}
		a := args[2:]
		if a[0] == "get" {
			key := a[1] + "/" + a[2]
			objects := map[string]object{"pod/" + text(agent, "metadata", "name"): agent, "pod/" + text(pod, "metadata", "name"): pod, "node/" + text(node, "metadata", "name"): node, "daemonset/cilium": ds,
				"ciliumnetworkpolicies.cilium.io/" + text(policy, "metadata", "name"): policy, "ciliumendpoints.cilium.io/" + text(pod, "metadata", "name"): cep}
			original := objects[key]
			if original == nil {
				return nil, errors.New("unrecognized read")
			}
			raw, _ := json.Marshal(original)
			var observed object
			if json.Unmarshal(raw, &observed) != nil {
				t.Fatal("fixture copy")
			}
			counts[key]++
			if change != nil {
				change(key, counts[key], observed)
			}
			return json.Marshal(observed)
		}
		prefix := []string{"exec", text(agent, "metadata", "name"), "--namespace=kube-system", "--container=cilium-agent", "--pod-running-timeout=20s", "--", "cilium-dbg"}
		if len(a) < len(prefix) || !reflect.DeepEqual(a[:len(prefix)], prefix) {
			return nil, errors.New("unbound native command")
		}
		switch strings.Join(a[len(prefix):], " ") {
		case "config --all -o json":
			return config, nil
		case "version -o json":
			return version, nil
		case "policy get -o json":
			return isolationResponse(t, entries, 817), nil
		case "endpoint get ipv4:192.0.2.17 -o json":
			return endpoint, nil
		default:
			return nil, errors.New("unreviewed native command")
		}
	}}
	return c, input, &calls
}

func TestRecoveryObservationCollectsOneBoundNativeDatapath(t *testing.T) {
	c, input, calls := recoveryObservationFixture(t, nil)
	proof, err := c.observeRecoveryCilium(context.Background(), input)
	if err != nil || proof.PolicyRevision != 817 || proof.Endpoint.EndpointID != 2718 || !proof.Endpoint.Monitor.Notifications || proof.Endpoint.Monitor.Audit || proof.Agent.Node != text(input.Pod, "spec", "nodeName") || *calls == 0 {
		t.Fatalf("native observation after %d synthetic calls: %+v %v", *calls, proof, err)
	}
}

func TestRecoveryObservationRejectsChangesDuringCollection(t *testing.T) {
	for _, field := range []string{"agent process", "target Pod", "Pod ownership", "Pod specification", "policy UID", "policy spec", "Node boot", "CEP UID"} {
		t.Run(field, func(t *testing.T) {
			c, input, _ := recoveryObservationFixture(t, func(key string, count int, o object) {
				if count != 2 {
					return
				}
				switch {
				case field == "agent process" && strings.HasPrefix(key, "pod/cilium-"):
					value(o, "status", "containerStatuses").([]any)[0].(object)["restartCount"] = float64(1)
				case field == "target Pod" && key == "pod/restore-1":
					o["metadata"].(object)["uid"] = "55555555-5555-4555-8555-555555555555"
				case field == "Pod ownership" && key == "pod/restore-1":
					o["metadata"].(object)["ownerReferences"] = []any{}
				case field == "Pod specification" && key == "pod/restore-1":
					o["spec"].(object)["hostNetwork"] = true
				case field == "policy UID" && strings.HasPrefix(key, "ciliumnetworkpolicies."):
					o["metadata"].(object)["uid"] = "55555555-5555-4555-8555-555555555555"
				case field == "policy spec" && strings.HasPrefix(key, "ciliumnetworkpolicies."):
					o["spec"].(object)["egressDeny"] = []any{}
				case field == "Node boot" && strings.HasPrefix(key, "node/"):
					value(o, "status", "nodeInfo").(object)["bootID"] = "55555555-5555-4555-8555-555555555555"
				case field == "CEP UID" && strings.HasPrefix(key, "ciliumendpoints."):
					o["metadata"].(object)["uid"] = "55555555-5555-4555-8555-555555555555"
				}
			})
			if _, err := c.observeRecoveryCilium(context.Background(), input); err == nil {
				t.Fatal("changed observation accepted")
			}
		})
	}
}

func TestRecoveryObservationRefusesAnotherNodeBeforeNativeCommands(t *testing.T) {
	c, input, calls := recoveryObservationFixture(t, nil)
	input.Pod["spec"].(object)["nodeName"] = "another-worker"
	if _, err := c.observeRecoveryCilium(context.Background(), input); err == nil || *calls != 0 {
		t.Fatal("unbound target selected a native observation")
	}
}

func TestRecoveryObservationReportsFixedPhaseWithoutCommandDetails(t *testing.T) {
	c, input, _ := recoveryObservationFixture(t, nil)
	c.run = func(context.Context, []byte, string, ...string) ([]byte, error) {
		return nil, errors.New("private-response-must-never-be-logged")
	}
	proof, err := c.observeRecoveryCilium(context.Background(), input)
	if err == nil || err.Error() != "native recovery datapath observation failed: phase=binding-before" || proof != (recoveryCiliumDatapath{}) {
		t.Fatalf("unsafe or unhelpful observation error: %+v %v", proof, err)
	}
}

func TestRecoveryObservationRejectsNativeDriftBetweenPasses(t *testing.T) {
	for name, tc := range map[string]struct {
		command string
		change  func(any)
	}{
		"policy revision":     {"cilium-dbg policy get -o json", func(v any) { v.(map[string]any)["revision"] = float64(818) }},
		"runtime enforcement": {"cilium-dbg config --all -o json", func(v any) { v.(map[string]any)["EnableL7Proxy"] = false }},
		"endpoint notifications": {"cilium-dbg endpoint get ipv4:192.0.2.17 -o json", func(v any) {
			value(v.([]any)[0].(object), "status", "realized", "options").(object)["DropNotification"] = "Disabled"
		}},
		"native attachment without CEP identifier": {"cilium-dbg endpoint get ipv4:192.0.2.17 -o json", func(v any) {
			value(v.([]any)[0].(object), "status", "external-identifiers").(object)["cni-attachment-id"] = "replacement-attachment:eth0"
		}},
	} {
		t.Run(name, func(t *testing.T) {
			c, input, _ := recoveryObservationFixture(t, nil)
			original, reads := c.run, 0
			c.run = func(ctx context.Context, in []byte, command string, args ...string) ([]byte, error) {
				raw, err := original(ctx, in, command, args...)
				if err != nil || !strings.HasSuffix(strings.Join(args, " "), tc.command) {
					return raw, err
				}
				reads++
				if reads != 2 {
					return raw, nil
				}
				var payload any
				if err = json.Unmarshal(raw, &payload); err != nil {
					t.Fatal(err)
				}
				tc.change(payload)
				return json.Marshal(payload)
			}
			if _, err := c.observeRecoveryCilium(context.Background(), input); err == nil || reads != 2 {
				t.Fatalf("native drift accepted or not examined: reads=%d err=%v", reads, err)
			}
		})
	}
}
