package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestFreshnessAndSubmissionBoundaries(t *testing.T) {
	for _, stage := range []string{"after fence", "after admission", "request preparation"} {
		t.Run(stage, func(t *testing.T) {
			retirementRunner(t)
			f := retirementFixture(t)
			stale := false
			missingDirectory := t.TempDir() + "/missing"
			execute := func(c context.Context, p string, a ...string) ([]byte, error) {
				args := strings.Join(a, " ")
				if p == "bash" && stale {
					return nil, fmt.Errorf("source advanced")
				}
				b, err := f.execute(c, p, a...)
				if err == nil && stage == "after fence" && strings.Contains(args, "patch namespace backstage") {
					stale = true
				}
				if strings.Contains(args, "create -f") && strings.Contains(args, "--dry-run=server") {
					if stage == "after admission" {
						stale = true
					} else if stage == "request preparation" {
						t.Setenv("TMPDIR", missingDirectory)
					}
				}
				return b, err
			}
			var out bytes.Buffer
			code := run([]string{"--execute"}, &out, execute)
			if code != 2 || f.deleted || strings.Contains(out.String(), "HOLD_NATIVE_LIFECYCLE_CONTINUES") || stage == "after fence" && f.configured {
				t.Fatalf("pre-request failure crossed the boundary: code=%d configured=%t deleted=%t %s", code, f.configured, f.deleted, &out)
			}
		})
	}
}

func TestEveryRetainedNonPodKindNeedsLiveDenial(t *testing.T) {
	for _, target := range []struct{ api, kind, plural string }{
		{"postgresql.cnpg.io/v1", "Cluster", "clusters"},
		{"v1", "ConfigMap", "configmaps"},
		{"v1", "Service", "services"},
		{"v1", "ServiceAccount", "serviceaccounts"},
		{"external-secrets.io/v1", "ExternalSecret", "externalsecrets"},
		{"postgresql.cnpg.io/v1", "Backup", "backups"},
		{"postgresql.cnpg.io/v1", "ScheduledBackup", "scheduledbackups"},
		{"barmancloud.cnpg.io/v1", "ObjectStore", "objectstores"},
	} {
		t.Run(target.kind, func(t *testing.T) {
			retirementRunner(t)
			f := retirementFixture(t)
			name := "backstage-db"
			if target.kind != "Cluster" {
				name = "retained-probe"
				r := map[string]any{"apiVersion": target.api, "kind": target.kind, "metadata": metadata(name, "retained-probe-uid"), "spec": map[string]any{}}
				f.inventory["items"] = append(array(f.inventory["items"]), r)
			}
			probed := false
			execute := func(c context.Context, p string, a ...string) ([]byte, error) {
				if p == "kubectl" && strings.Contains(strings.Join(a, " "), "/"+target.plural+"/"+name+"?dryRun=All") {
					probed = true
					return []byte(`{"apiVersion":"v1","kind":"Status","status":"Success"}`), nil
				}
				return f.execute(c, p, a...)
			}
			var out bytes.Buffer
			if code := run([]string{"--execute"}, &out, execute); code != 2 || !probed || f.deleted || !strings.Contains(out.String(), "HOLD_ADMISSION") {
				t.Fatalf("retained kind was not exercised: code=%d probed=%t deleted=%t %s", code, probed, f.deleted, &out)
			}
		})
	}
}

func TestHelmWriterIdentityIsCapturedAndCompared(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprint(replacement), func(t *testing.T) {
			retirementRunner(t)
			f := retirementFixture(t)
			execute := func(c context.Context, p string, a ...string) ([]byte, error) {
				args := strings.Join(a, " ")
				if strings.Contains(args, "get serviceaccount helm-controller") {
					uid := "helm-writer-uid"
					if f.fenceInstalled && replacement {
						uid = "replacement-writer"
					}
					return encode(t, map[string]any{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": "helm-controller", "namespace": "flux-system", "uid": uid, "resourceVersion": "1"}}), nil
				}
				return f.execute(c, p, a...)
			}
			var out bytes.Buffer
			code := run([]string{"--execute"}, &out, execute)
			if replacement {
				if code != 2 || f.deleted {
					t.Fatalf("replacement writer accepted: code=%d %s", code, &out)
				}
				return
			}
			record, err := decode([]byte(text(field(f.namespace, "metadata", "annotations", fenceAnnotation))))
			if code != 0 || err != nil || record["helmWriterUID"] != "helm-writer-uid" {
				t.Fatalf("native writer identity not captured: code=%d %s", code, &out)
			}
		})
	}
}
