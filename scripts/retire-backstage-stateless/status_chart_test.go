package main

import (
	"bytes"
	"context"
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

// The published CLI deliberately omits Chart from status JSON. Unlike the
// uninstall control, this executes the actual status producer used by collect.
func TestCollectedNativeStatusBindsStoredChart(t *testing.T) {
	bin := os.Getenv("PORTAL_HELM_TEST_BIN")
	if bin == "" {
		if os.Getenv("PORTAL_REQUIRE_NATIVE_CONTROL") == "1" {
			t.Fatal("required native status control has no pinned Helm binary")
		}
		t.Skip("native status requires explicitly pinned Helm")
	}
	version, err := exec.Command(bin, "version", "--short").Output()
	if err != nil || strings.TrimSpace(string(version)) != "v4.2.0+g0646808" {
		t.Fatal("native status requires Helm v4.2.0+g0646808")
	}
	checkRunner(t)
	f := retirementFixture(t)
	stored, err := decode(encode(t, f.release))
	if err != nil {
		t.Fatal(err)
	}
	delete(object(stored["info"]), "resources")
	api := nativeHelmAPI{secret: nativeReleaseSecret(t, stored), objects: map[string]map[string]any{}, deletes: map[string]int{}, waitReads: map[string]int{}}
	for _, values := range object(field(f.release, "info", "resources")) {
		for _, raw := range array(values) {
			o := object(raw)
			path := "/api/v1/namespaces/backstage/" + strings.ToLower(text(o["kind"])) + "s/" + text(field(o, "metadata", "name"))
			if o["kind"] == "Deployment" {
				path = "/apis/apps/v1/namespaces/backstage/deployments/backstage"
			}
			api.objects[path] = o
		}
	}
	server := httptest.NewServer(http.HandlerFunc(api.serve))
	defer server.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "kubeconfig")
	body := "apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: " + server.URL + "\ncontexts:\n- name: admin@prod\n  context:\n    cluster: fixture\n    user: fixture\ncurrent-context: admin@prod\nusers:\n- name: fixture\n  user: {}\n"
	if err := os.WriteFile(cfg, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	statusRead := false
	execute := func(ctx context.Context, program string, args ...string) ([]byte, error) {
		if program != "helm" {
			return f.execute(ctx, program, args...)
		}
		cmd := exec.CommandContext(ctx, bin, append(args, "--kubeconfig="+cfg)...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "KUBECONFIG=" + cfg, "HELM_DRIVER=secret", "HELM_CACHE_HOME=" + dir, "HELM_CONFIG_HOME=" + dir, "HELM_DATA_HOME=" + dir}
		b, err := cmd.Output()
		status, decodeErr := decode(b)
		if err != nil || decodeErr != nil || status["chart"] != nil {
			api.mu.Lock()
			defer api.mu.Unlock()
			var stderr []byte
			if exit, ok := err.(*exec.ExitError); ok {
				stderr = exit.Stderr
			}
			t.Fatalf("synthetic native status unavailable: error=%v decodeError=%v chartPresent=%t stderr=%q requests=%v", err, decodeErr, status["chart"] != nil, stderr, api.requests)
		}
		statusRead = true
		return b, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, err := (engine{execute: execute}).collect(ctx)
	if err != nil || !statusRead || !controllerHistoryMatches(s) {
		t.Fatalf("native status lost matching stored chart evidence: collectError=%t statusRead=%t matched=%t", err != nil, statusRead, controllerHistoryMatches(s))
	}
	var out bytes.Buffer
	code := run([]string{"--check"}, &out, execute)
	if code != 0 || out.String() != "PORTAL-RETIREMENT=CHECKED preservationSamples=2 mutation=NONE retirement=NOT_EXECUTED dataRetirement=UNASSESSED\n" || f.receiptCreated || f.fenceInstalled || f.configured || f.deleted || f.chartDeleted {
		t.Fatalf("native status did not complete the read-only entrypoint: code=%d output=%q", code, out.String())
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.foreignMutation || len(api.deletes) != 0 || api.historyUpdates != 0 {
		t.Fatal("native status control mutated synthetic storage")
	}
}

func TestReadOnlyCheckAcceptsBoundChartFreeStatus(t *testing.T) {
	checkRunner(t)
	f := retirementFixture(t)
	execute := func(ctx context.Context, program string, args ...string) ([]byte, error) {
		b, err := f.execute(ctx, program, args...)
		if program == "helm" && err == nil {
			r, decodeErr := decode(b)
			if decodeErr != nil {
				return nil, fmt.Errorf("synthetic status unavailable")
			}
			delete(r, "chart")
			return encode(t, r), nil
		}
		return b, err
	}
	var out bytes.Buffer
	code := run([]string{"--check"}, &out, execute)
	if code != 0 || out.String() != "PORTAL-RETIREMENT=CHECKED preservationSamples=2 mutation=NONE retirement=NOT_EXECUTED dataRetirement=UNASSESSED\n" || f.receiptCreated || f.fenceInstalled || f.configured || f.deleted || f.chartDeleted {
		t.Fatalf("bound chart-free status could not complete the read-only check: code=%d output=%q", code, out.String())
	}
}

func TestChartFreeStatusCannotBorrowUnboundOrChangedHistory(t *testing.T) {
	for _, mode := range []string{"--check", "--execute"} {
		for _, sample := range []int{1, 2} {
			for _, scenario := range []string{"missing", "duplicate", "foreign namespace", "foreign label", "wrong type", "foreign owner", "terminating", "invalid storage", "revision", "status", "content", "unknown content", "missing chart", "chart version"} {
				t.Run(fmt.Sprintf("%s/%d/%s", mode, sample, scenario), func(t *testing.T) {
					if mode == "--check" {
						checkRunner(t)
					} else {
						retirementRunner(t)
					}
					f := retirementFixture(t)
					samples := 0
					execute := func(ctx context.Context, program string, args ...string) ([]byte, error) {
						b, err := f.execute(ctx, program, args...)
						if program != "helm" || err != nil {
							return b, err
						}
						samples++
						status, decodeErr := decode(b)
						if decodeErr != nil {
							return nil, decodeErr
						}
						delete(status, "chart")
						if samples == sample {
							var secret map[string]any
							for _, raw := range array(f.inventory["items"]) {
								if field(object(raw), "metadata", "name") == "sh.helm.release.v1.backstage.v4" {
									secret = object(raw)
								}
							}
							if secret == nil {
								t.Fatal("synthetic history absent")
							}
							switch scenario {
							case "missing":
								object(secret["metadata"])["name"] = "other"
							case "duplicate":
								f.inventory["items"] = append(array(f.inventory["items"]), secret)
							case "foreign namespace":
								object(secret["metadata"])["namespace"] = "other"
							case "foreign label":
								object(field(secret, "metadata", "labels"))["name"] = "other"
							case "wrong type":
								secret["type"] = "Opaque"
							case "foreign owner":
								object(secret["metadata"])["ownerReferences"] = []any{map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "name": "other", "uid": "other-uid"}}
							case "terminating":
								object(secret["metadata"])["deletionTimestamp"] = "2026-10-15T00:00:00Z"
							case "invalid storage":
								secret["data"] = map[string]any{"release": "DO_NOT_PRINT"}
							default:
								r, err := storedRelease(secret)
								if err != nil {
									t.Fatal(err)
								}
								switch scenario {
								case "revision":
									r["version"] = 5
								case "status":
									object(r["info"])["status"] = "failed"
								case "content":
									object(r["config"])["marker"] = "CHANGED_DO_NOT_PRINT"
								case "unknown content":
									r["unknown"] = "DO_NOT_PRINT"
								case "missing chart":
									delete(r, "chart")
								case "chart version":
									object(field(r, "chart", "metadata"))["version"] = "DO_NOT_PRINT"
								}
								secret["data"] = nativeReleaseSecret(t, r)["data"]
							}
						}
						return encode(t, status), nil
					}
					var out bytes.Buffer
					code := run([]string{mode}, &out, execute)
					if code != 2 || samples != sample || !strings.HasPrefix(out.String(), "PORTAL-RETIREMENT=HOLD_") || strings.Contains(out.String(), "DO_NOT_PRINT") || f.receiptCreated || f.fenceInstalled || f.configured || f.deleted || f.chartDeleted {
						t.Fatalf("unbound status evidence admitted writes or leaked content: code=%d samples=%d output=%q", code, samples, out.String())
					}
				})
			}
		}
	}
}
