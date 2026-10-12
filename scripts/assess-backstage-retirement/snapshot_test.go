package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// statusFixture mirrors Helm 4.2's derived related PodList, not stored release data.
func statusFixture() (map[string]any, map[string]any) {
	r, l := fixture()
	object(r["info"])["resources"] = map[string]any{"v1/Pod(related)": []any{
		map[string]any{"apiVersion": "v1", "kind": "PodList",
			"metadata": map[string]any{"resourceVersion": "100"},
			"items": []any{map[string]any{"apiVersion": "v1", "kind": "Pod",
				"metadata": metadata("backstage-pod", "pod-uid")}},
		},
	}}
	return r, l
}

// relatedList selects the single complete synthetic list supplied above.
func relatedList(r map[string]any) map[string]any {
	return object(r["info"])["resources"].(map[string]any)["v1/Pod(related)"].([]any)[0].(map[string]any)
}

// TestRelatedListClockDoesNotHideRealChanges exercises the actual live collector.
func TestRelatedListClockDoesNotHideRealChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any, map[string]any)
		want   int
	}{
		{"collection clock only", func(r, _ map[string]any) {
			object(relatedList(r)["metadata"])["resourceVersion"] = "101"
		}, 0},
		{"installed revision", func(r, _ map[string]any) { r["version"] = 5 }, 2},
		{"installed manifest", func(r, _ map[string]any) { r["manifest"] = deploymentManifest + "\n# changed" }, 2},
		{"related pod version", func(r, _ map[string]any) {
			pod := relatedList(r)["items"].([]any)[0].(map[string]any)
			object(pod["metadata"])["resourceVersion"] = "13"
		}, 2},
		{"related pod identity", func(r, _ map[string]any) {
			pod := relatedList(r)["items"].([]any)[0].(map[string]any)
			object(pod["metadata"])["uid"] = "replacement"
		}, 2},
		{"related membership", func(r, _ map[string]any) { relatedList(r)["items"] = []any{} }, 2},
		{"live ownership", func(_, l map[string]any) {
			claim := l["items"].([]any)[3].(map[string]any)
			object(claim["metadata"])["resourceVersion"] = "13"
		}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			protectedRunner(t)
			r, l := statusFixture()
			reads := 0
			execute := func(_ context.Context, program string, _ ...string) ([]byte, error) {
				if program == "helm" {
					reads++
					if reads == 2 {
						tc.change(r, l)
					}
					return encode(t, r), nil
				}
				return encode(t, l), nil
			}
			var output bytes.Buffer
			if got := run([]string{"--live"}, nil, &output, execute); got != tc.want {
				t.Fatalf("collector exit = %d, want %d: %s", got, tc.want, &output)
			}
			if strings.Contains(output.String(), "DO_NOT_PRINT") {
				t.Fatal("private evidence escaped")
			}
		})
	}
}

// TestMalformedDerivedListsHold rejects malformed shapes even when both reads match.
func TestMalformedDerivedListsHold(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing collection version", func(l map[string]any) { delete(object(l["metadata"]), "resourceVersion") }},
		{"empty collection version", func(l map[string]any) { object(l["metadata"])["resourceVersion"] = "" }},
		{"numeric collection version", func(l map[string]any) { object(l["metadata"])["resourceVersion"] = 100 }},
		{"wrong kind", func(l map[string]any) { l["kind"] = "Pod" }},
		{"missing items", func(l map[string]any) { delete(l, "items") }},
		{"non-object member", func(l map[string]any) { l["items"] = []any{"invalid"} }},
		{"missing member version", func(l map[string]any) {
			pod := l["items"].([]any)[0].(map[string]any)
			delete(object(pod["metadata"]), "resourceVersion")
		}},
		{"foreign member", func(l map[string]any) {
			pod := l["items"].([]any)[0].(map[string]any)
			object(pod["metadata"])["namespace"] = "elsewhere"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			protectedRunner(t)
			r, l := statusFixture()
			tc.change(relatedList(r))
			var output bytes.Buffer
			if got := run([]string{"--live"}, nil, &output, func(_ context.Context, program string, _ ...string) ([]byte, error) {
				if program == "helm" {
					return encode(t, r), nil
				}
				return encode(t, l), nil
			}); got != 2 {
				t.Fatalf("malformed derived list passed: %d %s", got, &output)
			}
		})
	}
}

// TestNativeLiveCollector uses the real command capture against synthetic readers.
func TestNativeLiveCollector(t *testing.T) {
	protectedRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "assessment")
	if err := exec.CommandContext(ctx, "go", "build", "-o", binary, ".").Run(); err != nil {
		t.Fatalf("build actual consumer: %v", err)
	}
	for _, changedPod := range []bool{false, true} {
		r, l := statusFixture()
		dir := t.TempDir()
		write := func(name string, data []byte, mode os.FileMode) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, name), data, mode); err != nil {
				t.Fatal(err)
			}
		}
		write("first.json", encode(t, r), 0o600)
		object(relatedList(r)["metadata"])["resourceVersion"] = "101"
		if changedPod {
			pod := relatedList(r)["items"].([]any)[0].(map[string]any)
			object(pod["metadata"])["resourceVersion"] = "13"
		}
		write("second.json", encode(t, r), 0o600)
		write("live.json", encode(t, l), 0o600)
		write("helm", []byte(`#!/bin/sh
set -eu
[ "$*" = 'status backstage --namespace=backstage --kube-context=admin@prod --output=json' ]
if [ -f "$FIXTURE_DIR/read" ]; then
  /bin/cat "$FIXTURE_DIR/second.json"
else
  : > "$FIXTURE_DIR/read"
  /bin/cat "$FIXTURE_DIR/first.json"
fi
`), 0o700)
		write("kubectl", []byte(`#!/bin/sh
set -eu
[ "$*" = '--context=admin@prod --namespace=backstage get deployment,pod,persistentvolumeclaim,clusters.postgresql.cnpg.io --output=json --request-timeout=30s' ]
/bin/cat "$FIXTURE_DIR/live.json"
`), 0o700)
		cmd := exec.CommandContext(ctx, binary, "--live")
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "FIXTURE_DIR="+dir, "KUBECONFIG="+filepath.Join(dir, "absent"))
		output, err := cmd.CombinedOutput()
		want := "PORTAL-FOOTPRINT=ASSESSED_STATELESS objects=2 dataRetirement=UNASSESSED mutation=NONE\n"
		if changedPod {
			want = "PORTAL-FOOTPRINT=HOLD_CHANGED_SNAPSHOT\n"
		}
		if string(output) != want || (err != nil) != changedPod {
			t.Fatalf("native changedPod=%v: result or exit mismatch, err=%v", changedPod, err)
		}
	}
}
