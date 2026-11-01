package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise acknowledged ownership, source identity, refused SMTP, and deletion
// uncertainty. A successful client log alone must never produce a green proof.
func TestProviderProofLifecycle(t *testing.T) {
	for _, fault := range []string{"", "image-drift", "leftover", "source-restarted", "provider-refused", "probe-image-drift", "probe-node-drift", "probe-command-drift", "pod-replaced", "delete-denied", "create-lost"} {
		t.Run(fault, func(t *testing.T) {
			f := &providerFixture{t: t, fault: fault, live: map[string]map[string]any{}}
			r := runner{k: f, run: "123", sha: strings.Repeat("a", 40), expectedImage: "reviewed-image@sha256:" + strings.Repeat("b", 64), receipt: filepath.Join(t.TempDir(), "ownership.json"), pause: func(context.Context, time.Duration) error { return nil }}
			err := r.execute(context.Background())
			if (err == nil) != (fault == "") {
				t.Fatalf("fault %q: %v", fault, err)
			}
			if fault == "image-drift" || fault == "leftover" {
				if f.creates != 0 {
					t.Fatal("preflight failure wrote to production")
				}
				return
			}
			if fault == "create-lost" {
				if f.deletes != 0 || f.creates != 1 {
					t.Fatal("uncertain create guessed ownership or continued")
				}
				return
			}
			if fault == "pod-replaced" || fault == "delete-denied" {
				if _, statErr := os.Stat(r.receipt); statErr != nil {
					t.Fatal("uncertain cleanup lost its receipt")
				}
				return
			}
			if len(f.live) != 0 || f.deletes != 2 {
				t.Fatal("temporary resources survived known-ownership cleanup")
			}
			if _, statErr := os.Stat(r.receipt); !os.IsNotExist(statErr) {
				t.Fatal("successful cleanup retained its receipt")
			}
		})
	}
}

func TestFailedSessionKeepsOnlyBoundedNumericDiagnostics(t *testing.T) {
	for _, tc := range []struct{ log, want string }{
		{"SMTP_PROVIDER_SESSION=UNVERIFIED curl_exit=28 response=000\n", "probe-session-curl-28-response-000"},
		{"SMTP_PROVIDER_SESSION=UNVERIFIED curl_exit=8 response=550\n", "probe-session-curl-8-response-550"},
		{"PRIVATE_PROVIDER_BANNER", "probe-session"},
		{"SMTP_PROVIDER_SESSION=UNVERIFIED curl_exit=28 response=000\nPRIVATE_PROVIDER_BANNER", "probe-session"},
	} {
		if got := sessionFailure([]byte(tc.log)).Error(); got != tc.want {
			t.Fatalf("unsafe or missing diagnostic: %q", got)
		}
	}
}

type providerFixture struct {
	t                       *testing.T
	fault                   string
	reads, creates, deletes int
	live                    map[string]map[string]any
}

func (f *providerFixture) call(_ context.Context, input []byte, args ...string) ([]byte, error) {
	f.t.Helper()
	a := strings.Join(args, " ")
	encode := func(v any) ([]byte, error) { return json.Marshal(v) }
	if a == "get pod private-mail-0 -n private-mail -o json" {
		f.reads++
		image := "reviewed-image@sha256:" + strings.Repeat("b", 64)
		if f.fault == "image-drift" {
			image = "other-image"
		}
		restarts := 0
		if f.fault == "source-restarted" && f.reads > 1 {
			restarts = 1
		}
		return encode(map[string]any{"metadata": map[string]any{"name": "private-mail-0", "uid": "source-uid", "ownerReferences": []any{map[string]any{"kind": "StatefulSet", "name": "private-mail", "uid": "sts-uid", "controller": true}}}, "spec": map[string]any{"nodeName": "node-a", "containers": []any{map[string]any{"name": "private-mail", "image": image}}}, "status": map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}, "containerStatuses": []any{map[string]any{"name": "private-mail", "image": image, "imageID": "containerd://sha256:" + strings.Repeat("b", 64), "ready": true, "restartCount": restarts}}}})
	}
	if a == "get statefulset private-mail -n private-mail -o json" {
		return encode(map[string]any{"metadata": map[string]any{"uid": "sts-uid"}, "spec": map[string]any{"replicas": 1}, "status": map[string]any{"readyReplicas": 1}})
	}
	if a == "get node node-a -o json" {
		return encode(map[string]any{"metadata": map[string]any{"uid": "node-uid", "labels": map[string]any{"kubernetes.io/hostname": "node-a"}}, "spec": map[string]any{"providerID": "hcloud://12345"}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}})
	}
	if a == "get pods,ciliumnetworkpolicies -n whoami -l platform.devantler.tech/email-provider-probe -o json" {
		items := []any{}
		if f.fault == "leftover" {
			items = append(items, map[string]any{"metadata": map[string]any{"uid": "prior-run"}})
		}
		return encode(map[string]any{"items": items})
	}
	if a == "create -f - -o json" {
		f.creates++
		var o map[string]any
		if json.Unmarshal(input, &o) != nil {
			f.t.Fatal("create is not JSON")
		}
		meta := o["metadata"].(map[string]any)
		if meta["namespace"] != "whoami" || meta["name"] != "email-provider-probe-123" {
			f.t.Fatal("write escaped fixed diagnostic resource")
		}
		kind := o["kind"].(string)
		if kind == "Pod" {
			spec := o["spec"].(map[string]any)
			if spec["hostNetwork"] == true || spec["automountServiceAccountToken"] != false || spec["hostUsers"] != false || spec["volumes"] != nil || spec["nodeName"] != nil || spec["restartPolicy"] != "Never" {
				f.t.Fatal("probe gained host, credential, volume, or scheduler bypass access")
			}
			container := spec["containers"].([]any)[0].(map[string]any)
			if container["image"] != clientImage || container["env"] != nil || container["envFrom"] != nil {
				f.t.Fatal("probe image or credential boundary changed")
			}
			security := container["securityContext"].(map[string]any)
			if security["readOnlyRootFilesystem"] != true || security["allowPrivilegeEscalation"] != false {
				f.t.Fatal("probe is not restricted")
			}
			if strings.Contains(string(input), "MAIL FROM") || strings.Contains(string(input), "RCPT TO") {
				f.t.Fatal("probe can send mail")
			}
		} else if kind == "CiliumNetworkPolicy" {
			if !strings.Contains(string(input), `"matchName":"mx01.mail.icloud.com"`) || strings.Contains(string(input), `"toEntities"`) {
				f.t.Fatal("probe network access is not confined to fixed SMTP and DNS")
			}
		} else {
			f.t.Fatal("unexpected resource kind")
		}
		if f.fault == "create-lost" {
			return nil, errors.New("PRIVATE_CREATE_ACK_LOST")
		}
		meta["uid"] = "owned-" + kind
		f.live[kind] = o
		return encode(o)
	}
	if strings.HasPrefix(a, "logs ") {
		if f.fault == "provider-refused" {
			return []byte("SMTP_PROVIDER_SESSION=UNVERIFIED\n"), nil
		}
		return []byte("SMTP_PROVIDER_SESSION=VERIFIED\n"), nil
	}
	if strings.HasPrefix(a, "get Pod ") || strings.HasPrefix(a, "get CiliumNetworkPolicy ") {
		kind := args[1]
		o, ok := f.live[kind]
		if !ok {
			return nil, nil
		}
		if f.fault == "pod-replaced" && kind == "Pod" {
			o["metadata"].(map[string]any)["uid"] = "replacement"
		}
		if kind == "Pod" {
			o["spec"].(map[string]any)["nodeName"] = "node-a"
			if f.fault == "probe-node-drift" {
				o["spec"].(map[string]any)["nodeName"] = "node-b"
			}
			if f.fault == "probe-image-drift" {
				o["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["image"] = "other-image"
			}
			if f.fault == "probe-command-drift" {
				o["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["command"] = []any{"/bin/true"}
			}
			o["status"] = map[string]any{"phase": "Succeeded", "containerStatuses": []any{map[string]any{"name": "probe", "restartCount": 0, "imageID": "containerd://sha256:" + strings.Split(clientImage, "sha256:")[1], "state": map[string]any{"terminated": map[string]any{"exitCode": 0}}}}}
		}
		return encode(o)
	}
	if strings.HasPrefix(a, "delete --raw=") {
		var options map[string]any
		if json.Unmarshal(input, &options) != nil {
			f.t.Fatal("delete has no options")
		}
		uid := options["preconditions"].(map[string]any)["uid"].(string)
		kind := strings.TrimPrefix(uid, "owned-")
		if f.fault == "pod-replaced" && kind == "Pod" {
			f.t.Fatal("cleanup tried to remove successor")
		}
		if f.fault == "delete-denied" {
			return nil, errors.New("PRIVATE_DELETE_DENIED")
		}
		if f.live[kind] == nil || f.live[kind]["metadata"].(map[string]any)["uid"] != uid {
			f.t.Fatal("delete lacks owned UID")
		}
		delete(f.live, kind)
		f.deletes++
		return []byte(`{}`), nil
	}
	f.t.Fatalf("unexpected Kubernetes request %q", a)
	return nil, errors.New("unexpected request")
}
