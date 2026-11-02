package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type probeCalls struct{ reads, unsigned, signed, denied int }

func proofRunner(t *testing.T, mode string) (commandFunc, *probeCalls) {
	t.Helper()
	responses := capturedResponses(t)
	calls := &probeCalls{}
	read := readRunner(t, responses, &calls.reads)
	counts := map[string]int{}
	return func(ctx context.Context, program string, args []string, input []byte, env map[string]string) ([]byte, error) {
		if !commandAllowed(program, args, input, env) {
			t.Fatal("proof requested an out-of-scope command")
		}
		if program == "bash" {
			if args[0] == "./scripts/verify-prod-recovery-source.sh" {
				if mode == "stale source" {
					return nil, errors.New("private source failure")
				}
				return nil, nil
			}
			calls.unsigned++
			if env["REGISTRY_USERNAME"] != "probe" || env["REGISTRY_PASSWORD"] != "private-test-token" {
				t.Fatal("registry precheck used a different credential")
			}
			if mode == "registry unknown" {
				return nil, errors.New("private verifier failure")
			}
			return nil, nil
		}
		if args[3] == "get" {
			counts[args[5]]++
			if mode == "binding changed" && args[5] == "/api/v1/namespaces/default" && counts[args[5]] == 2 {
				var value object
				if err := json.Unmarshal(responses[args[5]], &value); err != nil {
					t.Fatal(err)
				}
				value["metadata"].(object)["resourceVersion"] = "changed"
				responses[args[5]] = encoded(t, value)
			}
			return read(ctx, program, args, input, env)
		}
		var pod object
		if err := json.Unmarshal(input, &pod); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(text(pod, "metadata", "name"), "longhorn-cache-unsigned-") {
			calls.denied++
			if mode == "unattributed rejection" {
				return []byte("resource quota exceeded"), errors.New("refused")
			}
			return []byte(`admission webhook "ivpol.validate.kyverno.svc-fail-finegrained-verify-app-images" denied the request: Policy verify-app-images failed: first-party app image is not signed by the publish-app release pipeline`), errors.New("API refused")
		}
		calls.signed++
		if mode == "signed denied" {
			return nil, errors.New("private signed failure")
		}
		return input, nil
	}, calls
}

func proofMetadata(t *testing.T) runMetadata {
	t.Helper()
	env := dispatchEnv()
	m, err := authorize(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCompleteProofReportsOnlyObservedPublicOutcomes(t *testing.T) {
	run, calls := proofRunner(t, "")
	r, err := prove(context.Background(), run, proofMetadata(t), []byte(desiredPolicySpec))
	if err != nil || r.Schema != 1 || r.SignedAdmitted != 2 || r.UnsignedDenied != 1 || !r.StableBindings || !r.ServerDryRun || r.NegativeCauseObservable || r.PerReplicaAttribution || r.ColdFirstRequestObserved || calls.reads != 22 || calls.unsigned != 2 || calls.signed != 2 || calls.denied != 1 {
		t.Fatalf("complete proof failed: calls=%+v err=%v", calls, err)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private-test-token", "credential-uid", "pod-a", "10.0.0.1"} {
		if strings.Contains(string(raw), private) {
			t.Fatal("proof serialized a private binding")
		}
	}
}

func TestFailedProofDiscardsTheWholeReceipt(t *testing.T) {
	for _, mode := range []string{"stale source", "registry unknown", "binding changed", "unattributed rejection", "signed denied"} {
		t.Run(mode, func(t *testing.T) {
			run, _ := proofRunner(t, mode)
			r, err := prove(context.Background(), run, proofMetadata(t), []byte(desiredPolicySpec))
			if err == nil || r.Schema != 0 || r.StableBindings || strings.Contains(err.Error(), "private") {
				t.Fatal("failed proof exposed a receipt or private error")
			}
		})
	}
}
