package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (*fakeClient, checks) {
	t.Helper()
	definition, err := os.ReadFile("../../k8s/bases/apps/private-mail-administration/patches/serve-after-initialization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := loadChecks(definition)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	set := statefulSet{}
	set.Metadata = metadata{Name: "private-mail", Namespace: "private-mail", UID: "11111111-1111-4111-8111-111111111111", ResourceVersion: "10", Generation: 3}
	one := int32(1)
	set.Spec.Replicas = &one
	set.Spec.PodManagementPolicy = "OrderedReady"
	set.Spec.UpdateStrategy.Type = "RollingUpdate"
	set.Spec.UpdateStrategy.RollingUpdate = &struct{ Partition int32 }{}
	set.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted = "Retain"
	set.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled = "Retain"
	set.Spec.Template.Spec.Containers = []container{{Name: "private-mail", Image: "ghcr.io/devantler-tech/platform-private-mail-bootstrap@sha256:" + strings.Repeat("a", 64), StartupProbe: expected.Startup, ReadinessProbe: expected.Ready, LivenessProbe: expected.Live}}
	set.Spec.Template.Spec.Containers[0].Ports = append(set.Spec.Template.Spec.Containers[0].Ports, struct {
		Name          string
		ContainerPort int
	}{"management", 8080})
	set.Spec.Template.Spec.Volumes = []volume{{Name: "data", PersistentVolumeClaim: &claimReference{ClaimName: "private-mail-data"}}}
	set.Status.ObservedGeneration = 3
	set.Status.Replicas = 1
	set.Status.CurrentRevision = "private-mail-old"
	set.Status.UpdateRevision = "private-mail-new"
	pod := mailPod{Metadata: metadata{Name: "private-mail-0", Namespace: "private-mail", UID: "22222222-2222-4222-8222-222222222222", ResourceVersion: "20", Labels: map[string]string{"controller-revision-hash": "private-mail-old"}, OwnerReferences: []owner{{Kind: "StatefulSet", Name: "private-mail", UID: set.Metadata.UID, Controller: true}}}}
	pod.Spec = set.Spec.Template.Spec
	pod.Spec.Containers = append([]container(nil), set.Spec.Template.Spec.Containers...)
	oldProbe := func(path string) *probe {
		return &probe{HTTPGet: &httpCheck{Path: path, Port: "management", Scheme: "HTTP"}, TimeoutSeconds: 1}
	}
	pod.Spec.Containers[0].StartupProbe = oldProbe("/healthz/live")
	pod.Spec.Containers[0].ReadinessProbe = oldProbe("/healthz/ready")
	pod.Spec.Containers[0].LivenessProbe = oldProbe("/healthz/live")
	pod.Status.Conditions = []condition{{Type: "Ready", Status: "False", LastTransitionTime: now.Add(-time.Hour)}}
	pod.Status.ContainerStatuses = []containerStatus{{Name: "private-mail", RestartCount: 20}}
	claim := persistentClaim{Metadata: metadata{Name: "private-mail-data", Namespace: "private-mail", UID: "33333333-3333-4333-8333-333333333333", ResourceVersion: "30"}}
	claim.Spec.VolumeName = "retained-volume"
	claim.Status.Phase = "Bound"
	source := artifactSource{Metadata: metadata{Name: "flux-system", Namespace: "flux-system", UID: "44444444-4444-4444-8444-444444444444", ResourceVersion: "40", Generation: 7}}
	source.Status.Artifact.Revision = "latest@sha256:" + strings.Repeat("b", 64)
	source.Status.Conditions = []condition{{Type: "Ready", Status: "True", ObservedGeneration: 7}, {Type: "SourceVerified", Status: "True", ObservedGeneration: 7}}
	f := &fakeClient{objects: map[string]any{setKey: set, podKey: pod, claimKey: claim, sourceKey: source}}
	return f, expected
}

type fakeClient struct {
	objects                    map[string]any
	deletes                    int
	deleteAttempts             int
	deletedUID, deletedVersion string
	deleteErr                  error
	afterDelete                func(*fakeClient)
	beforeDelete               func(*fakeClient)
	reads                      int
	failRead                   bool
}

func (f *fakeClient) read(_ context.Context, key string, out any) (bool, error) {
	f.reads++
	if f.failRead {
		return false, fmt.Errorf("read unavailable")
	}
	v, ok := f.objects[key]
	if !ok {
		return false, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, out)
}
func (f *fakeClient) deletePod(_ context.Context, uid, version string) error {
	f.deleteAttempts++
	if f.beforeDelete != nil {
		f.beforeDelete(f)
	}
	old := f.objects[podKey].(mailPod)
	if old.Metadata.UID != uid || old.Metadata.ResourceVersion != version {
		return fmt.Errorf("conflict")
	}
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletes++
	f.deletedUID = uid
	f.deletedVersion = version
	set := f.objects[setKey].(statefulSet)
	old.Metadata.UID = "55555555-5555-4555-8555-555555555555"
	old.Metadata.ResourceVersion = "50"
	old.Metadata.Labels = map[string]string{"controller-revision-hash": set.Status.UpdateRevision}
	old.Spec = set.Spec.Template.Spec
	old.Status.Conditions = []condition{{Type: "Ready", Status: "True", LastTransitionTime: time.Now().UTC()}}
	old.Status.ContainerStatuses = []containerStatus{{Name: "private-mail", Ready: true}}
	f.objects[podKey] = old
	if f.afterDelete != nil {
		f.afterDelete(f)
	}
	return nil
}

func TestRecoverOnlyObsoleteProbePod(t *testing.T) {
	f, want := fixture(t)
	if err := recoverPod(context.Background(), f, want, "sha256:"+strings.Repeat("b", 64), time.Now); err != nil {
		t.Fatal(err)
	}
	if f.deletes != 1 || f.deletedUID != "22222222-2222-4222-8222-222222222222" || f.deletedVersion != "20" {
		t.Fatalf("wrong deletion receipt: %+v", f)
	}
	claim := f.objects[claimKey].(persistentClaim)
	if claim.Metadata.UID != "33333333-3333-4333-8333-333333333333" || claim.Spec.VolumeName != "retained-volume" {
		t.Fatal("claim changed")
	}
}

func TestRefuseUnsafeOrUnprovenRecovery(t *testing.T) {
	cases := map[string]func(*fakeClient){
		"already-current": func(f *fakeClient) {
			s := f.objects[setKey].(statefulSet)
			s.Status.CurrentRevision = s.Status.UpdateRevision
			f.objects[setKey] = s
		},
		"desired-not-observed": func(f *fakeClient) {
			s := f.objects[setKey].(statefulSet)
			s.Status.ObservedGeneration--
			f.objects[setKey] = s
		},
		"more-than-one-replica": func(f *fakeClient) { s := f.objects[setKey].(statefulSet); *s.Spec.Replicas = 2; f.objects[setKey] = s },
		"non-retained-storage": func(f *fakeClient) {
			s := f.objects[setKey].(statefulSet)
			s.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled = "Delete"
			f.objects[setKey] = s
		},
		"different-image": func(f *fakeClient) {
			s := f.objects[setKey].(statefulSet)
			s.Spec.Template.Spec.Containers[0].Image = "other:latest"
			f.objects[setKey] = s
		},
		"incorrect-desired-probe": func(f *fakeClient) {
			s := f.objects[setKey].(statefulSet)
			s.Spec.Template.Spec.Containers[0].ReadinessProbe = &probe{}
			f.objects[setKey] = s
		},
		"pod-already-ready": func(f *fakeClient) {
			p := f.objects[podKey].(mailPod)
			p.Status.Conditions[0].Status = "True"
			f.objects[podKey] = p
		},
		"pod-only-recently-unready": func(f *fakeClient) {
			p := f.objects[podKey].(mailPod)
			p.Status.Conditions[0].LastTransitionTime = time.Now()
			f.objects[podKey] = p
		},
		"pod-owned-by-other-controller": func(f *fakeClient) {
			p := f.objects[podKey].(mailPod)
			p.Metadata.OwnerReferences[0].UID = "66666666-6666-4666-8666-666666666666"
			f.objects[podKey] = p
		},
		"pod-using-new-checks": func(f *fakeClient) {
			p := f.objects[podKey].(mailPod)
			s := f.objects[setKey].(statefulSet)
			p.Spec = s.Spec.Template.Spec
			f.objects[podKey] = p
		},
		"foreign-data-claim": func(f *fakeClient) {
			p := f.objects[podKey].(mailPod)
			p.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "other"
			f.objects[podKey] = p
		},
		"claim-owned-by-pod": func(f *fakeClient) {
			c := f.objects[claimKey].(persistentClaim)
			c.Metadata.OwnerReferences = []owner{{Kind: "Pod", UID: "22222222-2222-4222-8222-222222222222"}}
			f.objects[claimKey] = c
		},
		"claim-unbound": func(f *fakeClient) {
			c := f.objects[claimKey].(persistentClaim)
			c.Status.Phase = "Pending"
			f.objects[claimKey] = c
		},
		"source-not-verified": func(f *fakeClient) {
			s := f.objects[sourceKey].(artifactSource)
			s.Status.Conditions[1].Status = "False"
			f.objects[sourceKey] = s
		},
		"different-source": func(f *fakeClient) {
			s := f.objects[sourceKey].(artifactSource)
			s.Status.Artifact.Revision = "latest@sha256:" + strings.Repeat("c", 64)
			f.objects[sourceKey] = s
		},
		"failed-read": func(f *fakeClient) { f.failRead = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f, want := fixture(t)
			mutate(f)
			if err := recoverPod(context.Background(), f, want, "sha256:"+strings.Repeat("b", 64), time.Now); err == nil {
				t.Fatal("unsafe recovery accepted")
			}
			if f.deletes != 0 {
				t.Fatal("unsafe deletion occurred")
			}
		})
	}
}

func TestDeletionConflictDoesNotRetry(t *testing.T) {
	for _, field := range []string{"uid", "version"} {
		t.Run(field, func(t *testing.T) {
			f, want := fixture(t)
			f.beforeDelete = func(f *fakeClient) {
				p := f.objects[podKey].(mailPod)
				if field == "uid" {
					p.Metadata.UID = "66666666-6666-4666-8666-666666666666"
				} else {
					p.Metadata.ResourceVersion = "changed"
				}
				f.objects[podKey] = p
			}
			if recoverPod(context.Background(), f, want, "sha256:"+strings.Repeat("b", 64), time.Now) == nil {
				t.Fatal("conflict accepted")
			}
			if f.deletes != 0 {
				t.Fatal("conflicting object deleted")
			}
			if f.deleteAttempts != 1 {
				t.Fatalf("conditional deletion attempts=%d", f.deleteAttempts)
			}
		})
	}
}

func TestReplacementAndStorageMustBeVerified(t *testing.T) {
	for _, scenario := range []string{"unready", "claim-replaced", "source-changed"} {
		t.Run(scenario, func(t *testing.T) {
			f, want := fixture(t)
			f.afterDelete = func(f *fakeClient) {
				switch scenario {
				case "unready":
					p := f.objects[podKey].(mailPod)
					p.Status.Conditions[0].Status = "False"
					f.objects[podKey] = p
				case "claim-replaced":
					c := f.objects[claimKey].(persistentClaim)
					c.Metadata.UID = "66666666-6666-4666-8666-666666666666"
					f.objects[claimKey] = c
				case "source-changed":
					s := f.objects[sourceKey].(artifactSource)
					s.Status.Artifact.Revision = "latest@sha256:" + strings.Repeat("c", 64)
					f.objects[sourceKey] = s
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if recoverPod(ctx, f, want, "sha256:"+strings.Repeat("b", 64), time.Now) == nil {
				t.Fatal("incomplete recovery accepted")
			}
			if f.deletes != 1 {
				t.Fatalf("recovery retried deletion %d times", f.deletes)
			}
		})
	}
}

func TestNativeKubectlCarriesDeletePreconditions(t *testing.T) {
	_, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skip("kubectl is not installed")
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "DELETE" || r.URL.Path != "/api/v1/namespaces/private-mail/pods/private-mail-0" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 400)
			return
		}
		var body struct {
			APIVersion, Kind string
			Preconditions    struct{ UID, ResourceVersion string }
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Kind != "DeleteOptions" || body.APIVersion != "v1" || body.Preconditions.UID != "fixture-uid" || body.Preconditions.ResourceVersion != "42" {
			t.Errorf("missing preconditions: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, "{\"apiVersion\":\"v1\",\"kind\":\"Status\",\"status\":\"Success\"}")
	}))
	defer server.Close()
	config := filepath.Join(t.TempDir(), "config")
	yaml := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: %s\ncontexts:\n- name: admin@prod\n  context:\n    cluster: fixture\n    user: fixture\ncurrent-context: admin@prod\nusers:\n- name: fixture\n  user: {}\n", server.URL)
	if err := os.WriteFile(config, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	client := commandClient{kubeconfig: config}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.deletePod(ctx, "fixture-uid", "42"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("delete requests=%d", calls)
	}
}
