package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestMissingOrIneffectiveAdmissionCannotAuthorizeOwnerDeletion(t *testing.T) {
	for _, mode := range []string{"policy read fails", "policy missing", "negative control accepted", "positive control rejected"} {
		t.Run(mode, func(t *testing.T) {
			retirementRunner(t)
			f := retirementFixture(t)
			execute := func(c context.Context, p string, a ...string) ([]byte, error) {
				args := strings.Join(a, " ")
				if p == "kubectl" && strings.Contains(args, "get clusterpolicy restrict-backstage-retirement") {
					if mode == "policy read fails" {
						return nil, fmt.Errorf("PRIVATE_FAILURE")
					}
					if mode == "policy missing" {
						return []byte(`{}`), nil
					}
				}
				if strings.Contains(args, "?dryRun=All") {
					if mode == "negative control accepted" {
						return []byte(`{"apiVersion":"v1","kind":"Status","status":"Success"}`), nil
					}
					if mode == "positive control rejected" {
						return nil, fmt.Errorf("PRIVATE_FAILURE")
					}
				}
				return f.execute(c, p, a...)
			}
			var out bytes.Buffer
			if code := run([]string{"--execute"}, &out, execute); code != 2 || f.deleted || strings.Contains(out.String(), "PRIVATE") {
				t.Fatalf("ineffective admission authorized native lifecycle: code=%d %s", code, &out)
			}
		})
	}
}

func TestControllerCompletedRetirementDuringAdmissionIsObservedWithoutReplay(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	completed := false
	nativeDeletes := 0
	execute := func(c context.Context, p string, a ...string) ([]byte, error) {
		args := strings.Join(a, " ")
		if p == "kubectl" && strings.Contains(args, " delete ") && strings.Contains(args, "?dryRun=All") && strings.Contains(args, "/helmreleases/backstage?dryRun=All") && !completed {
			completed = true
			f.deleted = true
			f.historyStatus = "uninstalled"
			return nil, fmt.Errorf("target disappeared during dry-run")
		}
		if p == "kubectl" && strings.Contains(args, " delete ") && !strings.Contains(args, "?dryRun=All") {
			nativeDeletes++
			return nil, fmt.Errorf("duplicate lifecycle submission")
		}
		return f.execute(c, p, a...)
	}
	var out bytes.Buffer
	if code := run([]string{"--execute"}, &out, execute); code != 0 || !completed || !f.deleted || nativeDeletes != 0 || !strings.Contains(out.String(), "RETIRED_STATELESS") {
		t.Fatalf("controller-completed retirement was replayed or rejected: code=%d completed=%t deleted=%t nativeDeletes=%d output=%s", code, completed, f.deleted, nativeDeletes, &out)
	}
}

func TestPartialControllerRetirementDuringAdmissionIsNeverReplayed(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	started := false
	nativeDeletes := 0
	execute := func(c context.Context, p string, a ...string) ([]byte, error) {
		args := strings.Join(a, " ")
		if p == "kubectl" && strings.Contains(args, " delete ") && strings.Contains(args, "?dryRun=All") && strings.Contains(args, "/helmreleases/backstage?dryRun=All") && !started {
			started = true
			f.deleted = true
			return nil, fmt.Errorf("target disappeared during dry-run")
		}
		if p == "kubectl" && strings.Contains(args, " delete ") && !strings.Contains(args, "?dryRun=All") {
			nativeDeletes++
			return nil, fmt.Errorf("duplicate lifecycle submission")
		}
		return f.execute(c, p, a...)
	}
	var out bytes.Buffer
	if code := run([]string{"--execute"}, &out, execute); code != 2 || !started || nativeDeletes != 0 || !strings.Contains(out.String(), "HOLD_POSTCONDITION") {
		t.Fatalf("partial controller retirement was replayed or accepted: code=%d started=%t nativeDeletes=%d output=%s", code, started, nativeDeletes, &out)
	}
}

func TestForeignControllerLifecycleDuringAdmissionIsNeverAdopted(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	replaced := false
	nativeDeletes := 0
	execute := func(c context.Context, p string, a ...string) ([]byte, error) {
		args := strings.Join(a, " ")
		if p == "kubectl" && strings.Contains(args, " delete ") && strings.Contains(args, "?dryRun=All") && strings.Contains(args, "/helmreleases/backstage?dryRun=All") && !replaced {
			replaced = true
			object(f.hr["metadata"])["uid"] = "replacement-release-uid"
			object(f.hr["metadata"])["deletionTimestamp"] = "2026-10-17T21:30:00Z"
			return nil, fmt.Errorf("target changed during dry-run")
		}
		if p == "kubectl" && strings.Contains(args, " delete ") && !strings.Contains(args, "?dryRun=All") {
			nativeDeletes++
			return nil, fmt.Errorf("duplicate lifecycle submission")
		}
		return f.execute(c, p, a...)
	}
	var out bytes.Buffer
	if code := run([]string{"--execute"}, &out, execute); code != 2 || !replaced || nativeDeletes != 0 || !strings.Contains(out.String(), "HOLD_ADMISSION") {
		t.Fatalf("foreign controller lifecycle was adopted or replayed: code=%d replaced=%t nativeDeletes=%d output=%s", code, replaced, nativeDeletes, &out)
	}
}

func TestAPIDefaultedFenceMustMatchTheReviewedGetMethod(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		t.Run(method, func(t *testing.T) {
			retirementRunner(t)
			f := retirementFixture(t)
			execute := func(c context.Context, p string, a ...string) ([]byte, error) {
				b, err := f.execute(c, p, a...)
				if err == nil && strings.Contains(strings.Join(a, " "), "get clusterpolicy restrict-backstage-retirement") {
					policy, _ := decode(b)
					for _, rule := range array(field(policy, "spec", "rules")) {
						for _, context := range array(field(object(rule), "context")) {
							if api := object(field(object(context), "apiCall")); api != nil {
								api["method"] = method
							}
						}
					}
					return encode(t, policy), nil
				}
				return b, err
			}
			var out bytes.Buffer
			code := run([]string{"--execute"}, &out, execute)
			if method == "GET" && (code != 0 || !f.deleted) || method != "GET" && (code != 2 || f.fenceInstalled || f.deleted) {
				t.Fatalf("live API method was misclassified: code=%d %s", code, &out)
			}
		})
	}
}

func TestExistingDurableOperationCannotBeAdoptedOrReplayed(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	object(field(f.namespace, "metadata", "annotations"))["platform.devantler.tech/backstage-retirement"] = `{"phase":"fenced","run":"earlier"}`
	var out bytes.Buffer
	if code := run([]string{"--execute"}, &out, f.execute); code != 2 || f.receiptCreated || f.configured || f.deleted || !strings.Contains(out.String(), "HOLD_EXISTING_NATIVE_OPERATION") {
		t.Fatalf("existing durable operation was replayed: code=%d %s", code, &out)
	}
}

func TestAmbiguousOwnerDeleteDoesNotClaimNativeCancellation(t *testing.T) {
	retirementRunner(t)
	f := retirementFixture(t)
	execute := func(c context.Context, p string, a ...string) ([]byte, error) {
		if p == "kubectl" && strings.Contains(strings.Join(a, " "), " delete ") && !strings.Contains(strings.Join(a, " "), "?dryRun=All") {
			return nil, fmt.Errorf("PRIVATE_UNKNOWN_OUTCOME")
		}
		return f.execute(c, p, a...)
	}
	var out bytes.Buffer
	if code := run([]string{"--execute"}, &out, execute); code != 2 || !strings.Contains(out.String(), "HOLD_NATIVE_LIFECYCLE_CONTINUES") || strings.Contains(out.String(), "PRIVATE") {
		t.Fatalf("ambiguous submission was described as cancellation: code=%d %s", code, &out)
	}
}

func TestNonCharacterizedOrUnreadyControllerCannotStartUninstall(t *testing.T) {
	for _, mode := range []string{"not ready", "stale generation", "legacy defaults", "mixed defaults", "malformed arguments"} {
		t.Run(mode, func(t *testing.T) {
			retirementRunner(t)
			f := retirementFixture(t)
			execute := func(c context.Context, p string, a ...string) ([]byte, error) {
				b, err := f.execute(c, p, a...)
				if strings.Contains(strings.Join(a, " "), "get deployment helm-controller") && err == nil {
					controller, err := decode(b)
					if err != nil {
						return nil, err
					}
					status := object(controller["status"])
					if status == nil {
						status = map[string]any{}
						controller["status"] = status
					}
					spec := object(field(controller, "spec", "template", "spec"))
					switch mode {
					case "not ready":
						status["readyReplicas"] = 0
					case "stale generation":
						status["observedGeneration"] = 0
					case "legacy defaults":
						object(array(spec["containers"])[0])["args"] = []any{"--feature-gates=UseHelm3Defaults=true"}
					case "mixed defaults":
						object(array(spec["containers"])[0])["args"] = []any{"--feature-gates=UseHelm3Defaults=false,UseHelm3Defaults=true"}
					case "malformed arguments":
						object(array(spec["containers"])[0])["args"] = "invalid"
					}
					return encode(t, controller), nil
				}
				return b, err
			}
			var out bytes.Buffer
			if code := run([]string{"--execute"}, &out, execute); code != 2 || f.deleted || f.fenceInstalled {
				t.Fatalf("unproved native profile reached uninstall: code=%d %s", code, &out)
			}
		})
	}
}
