package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

type nativeResult struct {
	stdout, stderr []byte
	status         int
}

func finishScan(result nativeResult, scanErr error, cache string, stdout, stderr io.Writer) (int, error) {
	if _, err := stdout.Write(result.stdout); err != nil {
		return 2, err
	}
	if _, err := stderr.Write(result.stderr); err != nil {
		return 2, err
	}
	if scanErr != nil {
		return 2, scanErr
	}
	if err := verifyBundle(cache, correctedInventory); err != nil {
		return 2, err
	}
	if err := validateLoaderLog(result.stderr); err != nil {
		return 2, err
	}
	return result.status, nil
}

func runNative(engine, cache string, args []string, budget time.Duration) (nativeResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, engine, append([]string{"--cache-dir", cache, "--debug"}, args...)...)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "TRIVY_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := nativeResult{stdout: stdout.Bytes(), stderr: stderr.Bytes()}
	if ctx.Err() != nil {
		return result, fmt.Errorf("native scanner exceeded its command budget")
	}
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() < 0 {
			return result, fmt.Errorf("native scanner did not complete: %w", err)
		}
		result.status = exit.ExitCode()
	}
	return result, nil
}

type nativeReport struct {
	Results []struct {
		Target, Type      string
		Misconfigurations []map[string]any
	}
}

func reportRows(data []byte) (quotaFailures, other []string, quotaPasses int, err error) {
	var report nativeReport
	if err = json.Unmarshal(data, &report); err != nil || len(report.Results) == 0 {
		return nil, nil, 0, fmt.Errorf("native control report has no examined resources")
	}
	for _, result := range report.Results {
		for _, finding := range result.Misconfigurations {
			row, marshalErr := json.Marshal(struct {
				Target, Type string
				Finding      map[string]any
			}{result.Target, result.Type, finding})
			if marshalErr != nil {
				return nil, nil, 0, marshalErr
			}
			if finding["ID"] == "KSV-0040" {
				switch finding["Status"] {
				case "FAIL":
					quotaFailures = append(quotaFailures, string(row))
				case "PASS":
					quotaPasses++
				default:
					return nil, nil, 0, fmt.Errorf("unknown native quota finding status")
				}
			} else {
				other = append(other, string(row))
			}
		}
	}
	sort.Strings(quotaFailures)
	sort.Strings(other)
	return quotaFailures, other, quotaPasses, nil
}

const controlPod = `apiVersion: v1
kind: Pod
metadata:
  name: unsafe-pod
spec:
  containers:
    - name: busybox
      image: busybox:latest
      securityContext:
        privileged: true
`

const controlQuota = `apiVersion: v1
kind: ResourceQuota
metadata:
  name: invalid-quota
  namespace: controls
spec:
  hard:
    requests.cpu: "1"
`

const controlValidQuota = `apiVersion: v1
kind: ResourceQuota
metadata:
  name: complete-quota
  namespace: controls
spec:
  hard:
    requests.cpu: "1"
    requests.memory: 1Gi
    limits.cpu: "2"
    limits.memory: 2Gi
`

func nativeControls(engine, original, corrected, root string) error {
	cases := []struct {
		name                     string
		files                    map[string]string
		oldFailures, newFailures int
	}{
		{"pod", map[string]string{"pod.yaml": controlPod}, 0, 0},
		{"quota", map[string]string{"quota.yaml": controlQuota}, 1, 1},
		{"complete", map[string]string{"quota.yaml": controlValidQuota}, 0, 0},
		{"separate", map[string]string{"pod.yaml": controlPod, "quota.yaml": controlQuota}, 2, 1},
		{"pod-first", map[string]string{"all.yaml": controlPod + "---\n" + controlQuota}, 2, 1},
		{"quota-first", map[string]string{"all.yaml": controlQuota + "---\n" + controlPod}, 2, 1},
		{"offset", map[string]string{"all.yaml": controlPod + "---\n" + strings.Repeat("# retained offset\n", 40) + controlQuota}, 2, 1},
		{"mixed", map[string]string{
			"pod.yaml": controlPod, "quota.yaml": controlQuota,
			"Dockerfile": "FROM alpine:latest\nRUN echo hello\n",
			"main.tf":    "resource \"aws_s3_bucket\" \"unsafe\" {\n bucket = \"scanner-control-only\"\n acl = \"public-read\"\n}\n",
		}, 2, 1},
	}
	emptyIgnore := filepath.Join(root, "empty-ignore")
	if err := os.WriteFile(emptyIgnore, nil, 0600); err != nil {
		return err
	}
	for _, tc := range cases {
		dir := filepath.Join(root, tc.name)
		if err := os.Mkdir(dir, 0700); err != nil {
			return err
		}
		for name, data := range tc.files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
				return err
			}
		}
		args := []string{"fs", "--scanners", "misconfig", "--skip-check-update", "--include-non-failures", "--ignorefile", emptyIgnore, "--format", "json", "--exit-code", "0", dir}
		before, err := runNative(engine, original, args, 2*time.Minute)
		if err != nil || before.status != 0 {
			return fmt.Errorf("original native control %s failed: %v", tc.name, err)
		}
		after, err := runNative(engine, corrected, args, 2*time.Minute)
		if err != nil || after.status != 0 {
			return fmt.Errorf("corrected native control %s failed: %v", tc.name, err)
		}
		for _, log := range [][]byte{before.stderr, after.stderr} {
			if err := validateLoaderLog(log); err != nil {
				return fmt.Errorf("native control %s: %w", tc.name, err)
			}
		}
		oldQuota, oldOther, oldPass, err := reportRows(before.stdout)
		if err != nil {
			return err
		}
		newQuota, newOther, newPass, err := reportRows(after.stdout)
		if err != nil {
			return err
		}
		if len(oldQuota) != tc.oldFailures || len(newQuota) != tc.newFailures || !reflect.DeepEqual(oldOther, newOther) {
			return fmt.Errorf("native %s control lost a quota or unrelated finding", tc.name)
		}
		for _, genuine := range newQuota {
			if !containsRow(oldQuota, genuine) {
				return fmt.Errorf("genuine quota finding fields changed in native %s control", tc.name)
			}
		}
		if tc.name == "complete" && (oldPass != 1 || newPass != 1) {
			return fmt.Errorf("complete quota was not positively examined")
		}
		if tc.name == "mixed" && len(newOther) == 0 {
			return fmt.Errorf("mixed native control examined no unrelated checks")
		}
	}
	args := []string{"config", "--skip-check-update", "--ignorefile", emptyIgnore, "--format", "json", "--exit-code", "23", filepath.Join(root, "quota")}
	result, err := runNative(engine, corrected, args, 2*time.Minute)
	if err != nil || result.status != 23 {
		return fmt.Errorf("native quota finding exit status was not retained: %v", err)
	}
	return validateLoaderLog(result.stderr)
}

func containsRow(rows []string, wanted string) bool {
	for _, row := range rows {
		if row == wanted {
			return true
		}
	}
	return false
}
