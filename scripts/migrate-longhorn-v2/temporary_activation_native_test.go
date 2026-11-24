package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

type activationBootReader struct {
	nativeReplicaReader
	before, after        string
	reads, identityReads int
	changedIdentity      bool
}

const activationEphemeralGPT = "12345678-1234-4234-8234-000000000045"
const activationEphemeralLUKS = "12345678-1234-4234-8234-000000000046"

// BootID supplies independently selected observations on the two bracket sides.
func (r *activationBootReader) BootID(context.Context) (string, error) {
	r.reads++
	if r.reads == 1 {
		return r.before, nil
	}
	return r.after, nil
}

func (r *activationBootReader) RebuiltEphemeralUUID(_ context.Context, expectedGPT string) (string, error) {
	r.identityReads++
	if expectedGPT != activationEphemeralGPT {
		return "", errors.New("native GPT identity does not match")
	}
	if r.changedIdentity && r.identityReads > 1 {
		return "12345678-1234-4234-8234-000000000047", nil
	}
	return activationEphemeralLUKS, nil
}

// A binding requires matching native boot, hardware and API identities plus a
// successful reader close; durable identity comparison cannot waive this bracket.
func TestTemporaryRawBindingBracketsNativeReadsAndClosesItsReader(t *testing.T) {
	for _, mode := range []string{"valid", "boot changed", "hardware changed", "node changed", "partition unreadable",
		"partition changed", "close failed", "newly provisioned", "native header changed", "unrelated GPT"} {
		t.Run(mode, func(t *testing.T) {
			n, storage, raw, eph, pool := temporaryRawBindingFixture()
			n["status"].(object)["addresses"] = []any{object{"type": "InternalIP", "address": "10.0.99.10"}}
			boot := text(n, "status", "nodeInfo", "bootID")
			reader := &activationBootReader{before: boot, after: boot}
			discoveryRequired := mode == "newly provisioned" || mode == "native header changed" || mode == "unrelated GPT"
			if discoveryRequired {
				spec := valueObject(eph, "spec")
				delete(spec, "uuid")
				spec["partitionUUID"] = activationEphemeralGPT
				reader.changedIdentity = mode == "native header changed"
				if mode == "unrelated GPT" {
					spec["partitionUUID"] = "12345678-1234-4234-8234-000000000048"
				}
			}
			if mode == "boot changed" {
				reader.after = "12345678-1234-4234-8234-000000000099"
			}
			closed, volumes := 0, 0
			activation := temporaryV2Activation{reader: func(context.Context, string) (nativeReplicaReader, func() error, error) {
				return reader, func() error {
					closed++
					if mode == "close failed" {
						return errors.New("reader close failed")
					}
					return nil
				}, nil
			}}
			activation.c = client{context: "fixture", run: func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
				if program == "talosctl" && len(args) == 5 && args[0] == "--nodes=10.0.99.10" && args[1] == "get" && args[4] == "--output=json" {
					switch {
					case args[2] == "systeminformation" && args[3] == "systeminformation":
						uuid := "hardware"
						if mode == "hardware changed" {
							uuid = "other"
						}
						return json.Marshal(object{"metadata": object{"namespace": "hardware", "id": "systeminformation"}, "spec": object{"uuid": uuid}})
					case args[2] == "volumestatus" && args[3] == "r-longhorn":
						volumes++
						if mode == "partition unreadable" {
							return nil, errors.New("native read failed")
						}
						if mode == "partition changed" && volumes > 2 {
							raw["spec"].(object)["partitionUUID"] = "12345678-1234-4234-8234-000000000099"
						}
						return json.Marshal(raw)
					case args[2] == "volumestatus" && args[3] == "EPHEMERAL":
						volumes++
						return json.Marshal(eph)
					}
				}
				if program == "kubectl" && reflect.DeepEqual(args, []string{"--context=fixture", "--request-timeout=20s", "get", "node", text(n, "metadata", "name"), "--output=json"}) {
					if mode == "node changed" {
						n["metadata"].(object)["uid"] = "replacement"
					}
					return json.Marshal(n)
				}
				t.Fatalf("native binding escaped its read-only fixed scope: %s %v", program, args)
				return nil, nil
			}}
			binding, err := activation.readBinding(t.Context(), "temporary-v1-1", n, storage, pool)
			if (err == nil) != (mode == "valid" || mode == "newly provisioned") {
				t.Fatal("native binding validity did not match its complete identity bracket:", err)
			}
			if mode == "valid" && (closed != 1 || reader.reads != 2 || volumes != 4 || binding.BootID != boot) {
				t.Fatal("successful binding omitted a native read or close")
			}
			if mode == "newly provisioned" && (closed != 3 || reader.reads != 2 || reader.identityReads != 2 ||
				binding.EphemeralUUID != activationEphemeralLUKS || binding.BootID != boot) {
				t.Fatal("newly provisioned volume lacked its native identity bracket")
			}
		})
	}
}
