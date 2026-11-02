package main

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

const mirrorImage = "quay.io/minio/aistor/mc:RELEASE.2026-03-12T04-18-55Z@sha256:6c33dc0fbf65c362be95003cd010ed95a41c556500833ea139f86de40c4c4e9f"

type target struct{ Endpoint, Bucket, AccessKey, SecretKey string }

// fixedObject reads only the named deployment facts, never a target from user text.
func fixedObject(raw []byte, kind, namespace, name string) (map[string]string, error) {
	var v struct {
		Kind     string                           `json:"kind"`
		Metadata struct{ Name, Namespace string } `json:"metadata"`
		Data     map[string]string                `json:"data"`
	}
	if len(raw) == 0 || len(raw) > 1<<20 || json.Unmarshal(raw, &v) != nil || v.Kind != kind || v.Metadata.Name != name || v.Metadata.Namespace != namespace || v.Data == nil {
		return nil, errEvidence
	}
	if kind == "Secret" {
		for k, value := range v.Data {
			decoded, err := base64.StdEncoding.Strict().DecodeString(value)
			if err != nil {
				return nil, errEvidence
			}
			v.Data[k] = string(decoded)
		}
	}
	return v.Data, nil
}

// backupTarget follows the exact Flux substitution precedence and fixed R2 credential source.
func backupTarget(base, cluster, secret, keys []byte) (target, error) {
	var result target
	values := map[string]string{}
	for _, item := range []struct {
		raw        []byte
		kind, name string
	}{{base, "ConfigMap", "variables-base"}, {cluster, "ConfigMap", "variables-cluster"}, {secret, "Secret", "variables-cluster"}} {
		data, err := fixedObject(item.raw, item.kind, "flux-system", item.name)
		if err != nil {
			return result, errEvidence
		}
		for key, value := range data {
			values[key] = value
		}
	}
	credentials, err := fixedObject(keys, "Secret", "openbao", "vault-snapshot-r2")
	if err != nil {
		return result, errEvidence
	}
	result = target{Endpoint: strings.TrimSuffix(values["r2_endpoint"], "/"), Bucket: values["r2_bucket"], AccessKey: credentials["access_key_id"], SecretKey: credentials["secret_access_key"]}
	if !regexp.MustCompile(`^https://[a-f0-9]{32}(?:\.eu)?\.r2\.cloudflarestorage\.com$`).MatchString(result.Endpoint) ||
		!regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`).MatchString(result.Bucket) || strings.Contains(result.Bucket, "..") ||
		!regexp.MustCompile(`^[A-Za-z0-9]{20,64}$`).MatchString(result.AccessKey) ||
		!regexp.MustCompile(`^[A-Za-z0-9+/=]{32,128}$`).MatchString(result.SecretKey) {
		return target{}, errEvidence
	}
	return result, nil
}

// freshRecoveryWindow requires a recently completed consumer of the replacement storage.
func freshRecoveryWindow(raw []byte, now time.Time) (time.Time, time.Time, error) {
	// Active custody can be newer than the one-time copy's snapshot. Its full
	// records must survive an actual restore from the newest bounded independent
	// backup, with no user-supplied producer window. Legacy copy keeps its exact
	// completed-job binding below.
	if canonicalRecoverySet() {
		if len(raw) != 0 || now.IsZero() {
			return time.Time{}, time.Time{}, errEvidence
		}
		return now.Add(-24 * time.Hour), now.Add(5 * time.Minute), nil
	}
	var job struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name, Namespace   string
			DeletionTimestamp *string `json:"deletionTimestamp"`
		} `json:"metadata"`
		Spec struct {
			Deadline int `json:"activeDeadlineSeconds"`
			Template struct {
				Spec struct {
					ServiceAccount string `json:"serviceAccountName"`
					Automount      *bool  `json:"automountServiceAccountToken"`
					Volumes        []struct {
						Name  string
						Claim *struct {
							Name string `json:"claimName"`
						} `json:"persistentVolumeClaim"`
					} `json:"volumes"`
					Containers []struct {
						Name, Image string
						Env         []struct{ Name, Value string }
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
		Status struct {
			Start      time.Time `json:"startTime"`
			End        time.Time `json:"completionTime"`
			Succeeded  int       `json:"succeeded"`
			Conditions []struct{ Type, Status string }
		} `json:"status"`
	}
	refuse := func() (time.Time, time.Time, error) { return time.Time{}, time.Time{}, errEvidence }
	if len(raw) == 0 || len(raw) > 1<<20 || json.Unmarshal(raw, &job) != nil || job.Kind != "Job" || job.Metadata.Name != "vault-snapshot-init" || job.Metadata.Namespace != "openbao" || job.Metadata.DeletionTimestamp != nil || job.Spec.Deadline != 3600 {
		return refuse()
	}
	pod := job.Spec.Template.Spec
	if pod.ServiceAccount != "vault-snapshot" || pod.Automount == nil || *pod.Automount {
		return refuse()
	}
	volumes, mirrors := 0, 0
	for _, v := range pod.Volumes {
		if v.Name == "snapshots" {
			if v.Claim == nil || v.Claim.Name != "vault-snapshots-recovery" {
				return refuse()
			}
			volumes++
		}
	}
	for _, c := range pod.Containers {
		if c.Name != "mirror" {
			continue
		}
		if c.Image != mirrorImage {
			return refuse()
		}
		strict := 0
		for _, env := range c.Env {
			if env.Name == "REQUIRE_REMOTE_SNAPSHOT" {
				if env.Value != "true" {
					return refuse()
				}
				strict++
			}
		}
		if strict != 1 {
			return refuse()
		}
		mirrors++
	}
	complete := 0
	for _, condition := range job.Status.Conditions {
		if condition.Type == "Failed" && condition.Status == "True" {
			return refuse()
		}
		if condition.Type == "Complete" && condition.Status == "True" {
			complete++
		}
	}
	start, end := job.Status.Start, job.Status.End
	if volumes != 1 || mirrors != 1 || complete != 1 || job.Status.Succeeded != 1 || start.IsZero() || end.IsZero() || end.Before(start) || start.Before(now.Add(-24*time.Hour)) || end.After(now.Add(5*time.Minute)) {
		return refuse()
	}
	return start, end, nil
}
