package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNativeRestoreAttachmentObserverRequiresAStableReadOnlyEpoch(t *testing.T) {
	for _, fault := range []string{"", "wrong context", "wrong owner", "wrong grant", "cancelled", "source changes", "read failure", "unread continuation", "incomplete frontend list", "frontend replaced", "consumer process replaced", "manager process replaced", "manager UID replaced"} {
		t.Run(fault, func(t *testing.T) {
			r, seed, state, o := nativeRestoreAttachedFixture(t, false)
			for key, val := range nativeRestoreDispatch(state) {
				t.Setenv(key, val)
			}
			if fault == "wrong owner" {
				t.Setenv("GITHUB_RUN_ID", "9999")
			}
			if fault == "wrong grant" {
				t.Setenv("LONGHORN_V2_OPERATION", nativeSeedOperation)
			}
			reads := map[string]int{}
			managers, managerPods := map[string]object{}, map[string]object{}
			for i, target := range state.Targets {
				manager, pod := nativeRestoreAttachedManagerFixture(target, i)
				name := o.Managers[target.Name].Pod
				managers[name], managerPods[name] = manager, pod
			}
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if input != nil {
					t.Fatal("the attachment observer submitted a write body")
				}
				if program == "git" {
					if strings.Join(args, " ") != "ls-remote --exit-code https://github.com/devantler-tech/platform.git refs/heads/main" {
						t.Fatal("the observer rebound an unrelated source", args)
					}
					reads["source"]++
					source := state.Source
					if fault == "source changes" && reads["source"] > 1 {
						source = strings.Repeat("f", 40)
					}
					return []byte(source + "\trefs/heads/main\n"), nil
				}
				if program != "kubectl" || len(args) < 5 || args[0] != "--context=admin@prod" || args[1] != "--request-timeout=20s" || args[2] != "get" {
					t.Fatal("the attachment observer escaped its read-only cluster scope", program, args)
				}
				kind := args[3]
				reads[kind]++
				gets := map[string]object{"persistentvolumeclaim": o.Claim, "storageclass": o.Class, "persistentvolume": o.PV, "pod": o.Pod}
				if kind == "instancemanagers.longhorn.io" {
					gets[kind] = managers[args[4]]
				}
				if kind == "pod" && managerPods[args[4]] != nil {
					gets[kind] = managerPods[args[4]]
				}
				if item, ok := gets[kind]; ok {
					namespace := namespaceArgs(text(item, "metadata", "namespace"))
					want := append([]string{"--context=admin@prod", "--request-timeout=20s", "get", kind, text(item, "metadata", "name"), "--output=json"}, namespace...)
					if strings.Join(args, " ") != strings.Join(want, " ") {
						t.Fatal("a journaled attachment read changed its fixed resource scope", args)
					}
					if kind == "pod" && namespace[0] == "--namespace="+benchmarkNamespace && fault == "consumer process replaced" && reads[kind] > 1 {
						value(item, "status", "containerStatuses").([]any)[0].(object)["containerID"] = "containerd://" + strings.Repeat("f", 64)
					}
					if reads["enginefrontends.longhorn.io"] > 1 {
						if kind == "instancemanagers.longhorn.io" && fault == "manager UID replaced" {
							valueObject(item, "metadata")["uid"] = "replacement-manager"
						}
						if kind == "pod" && managerPods[args[4]] != nil && fault == "manager process replaced" {
							value(item, "status", "containerStatuses").([]any)[0].(object)["containerID"] = "containerd://" + strings.Repeat("f", 64)
						}
					}
					return json.Marshal(item)
				}
				lists := map[string][]object{
					"nodes": o.Inventory.nodes, "nodes.longhorn.io": o.Inventory.storageNodes, "volumes.longhorn.io": o.Inventory.volumes,
					"replicas.longhorn.io": o.Inventory.replicas, "engines.longhorn.io": o.Inventory.engines, "orphans.longhorn.io": {},
					"pods": o.Inventory.pods, "enginefrontends.longhorn.io": o.Frontends,
				}
				items, ok := lists[kind]
				if !ok || args[4] != "--output=json" {
					t.Fatal("the attachment observer requested an unreviewed collection", args)
				}
				namespace := "longhorn-system"
				switch kind {
				case "nodes":
					namespace = ""
				case "pods":
					namespace = "*"
				}
				want := append([]string{"--context=admin@prod", "--request-timeout=20s", "get", kind, "--output=json"}, namespaceArgs(namespace)...)
				if strings.Join(args, " ") != strings.Join(want, " ") {
					t.Fatal("the attachment observer filtered or redirected a complete inventory", args)
				}
				if kind == "enginefrontends.longhorn.io" {
					switch fault {
					case "read failure":
						return nil, errors.New("fixture collection failure")
					case "unread continuation":
						return json.Marshal(object{"metadata": object{"continue": "unread"}, "items": items})
					case "incomplete frontend list":
						return json.Marshal(object{"items": nil})
					case "frontend replaced":
						if reads[kind] > 1 {
							valueObject(items[0], "metadata")["uid"] = "replacement-frontend-uid"
						}
					}
				}
				return json.Marshal(object{"items": items})
			}}
			if fault == "wrong context" {
				c.context = "oidc@prod"
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if fault == "cancelled" {
				cancel()
			}
			got, err := observeNativeRestoreAttached(ctx, c, r, seed, state, o.Config, o.HelperDigest, o.Managers)
			if (err != nil) != (fault != "") {
				t.Fatalf("observer returned the wrong verdict for %q: %v", fault, err)
			}
			if fault == "" && (reads["source"] != 2 || reads["enginefrontends.longhorn.io"] != 2 || reads["instancemanagers.longhorn.io"] != 6 || got.ConsumerContainerID == "" || got.Frontend.UID == "") {
				t.Fatal("a successful observation did not bind two complete original attachment epochs", reads, got)
			}
			if (fault == "wrong context" || fault == "wrong owner" || fault == "wrong grant" || fault == "cancelled") && len(reads) != 0 {
				t.Fatal("an unauthorized attachment observer touched the cluster or source", reads)
			}
		})
	}
}
