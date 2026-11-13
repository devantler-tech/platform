package main

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

type object = map[string]any

func child(o object, key string) object {
	if v, ok := o[key].(map[string]any); ok {
		return v
	}
	return object{}
}
func text(o object, key string) string { v, _ := o[key].(string); return v }
func items(o object, key string) []object {
	a, _ := o[key].([]any)
	out := make([]object, 0, len(a))
	for _, v := range a {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		} else {
			return nil
		}
	}
	return out
}
func authorize(get func(string) string) (string, error) {
	for k, want := range map[string]string{"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_REF": "refs/heads/main", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_RUN_ATTEMPT": "1", "EMAIL_CANARY_CONFIRM": "verify-email-storage-canary", "RUNNER_ENVIRONMENT": "github-hosted"} {
		if get(k) != want {
			return "", errors.New("dispatch")
		}
	}
	run := get("GITHUB_RUN_ID")
	if !regexp.MustCompile(`^[1-9][0-9]{0,19}$`).MatchString(run) || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(get("GITHUB_SHA")) {
		return "", errors.New("identity")
	}
	return run, nil
}
func checkVolume(o object, name, uid string, detached bool) error {
	spec, status, meta := child(o, "spec"), child(o, "status"), child(o, "metadata")
	if uid == "" || text(meta, "name") != name || text(meta, "uid") != uid || spec["encrypted"] != true || spec["numberOfReplicas"] != float64(3) || text(spec, "dataEngine") != "v1" {
		return errors.New("volume")
	}
	if detached && (text(status, "state") != "detached" || text(spec, "nodeID") != "") {
		return errors.New("detach")
	}
	return nil
}
func negativeMount(pod object, events []object, uid, volume, mode string) bool {
	if uid == "" || text(child(pod, "metadata"), "uid") != uid || text(child(pod, "status"), "phase") != "Pending" {
		return false
	}
	if raw, present := child(pod, "status")["containerStatuses"]; present {
		statuses, ok := raw.([]any)
		if !ok || len(statuses) != len(items(child(pod, "status"), "containerStatuses")) {
			return false
		}
	}
	for _, c := range items(child(pod, "status"), "containerStatuses") {
		state := child(c, "state")
		if c["started"] == true || (c["restartCount"] != nil && c["restartCount"] != float64(0)) || len(child(c, "lastState")) != 0 || len(child(state, "waiting")) == 0 || len(child(state, "running")) != 0 || len(child(state, "terminated")) != 0 {
			return false
		}
	}
	for _, e := range events {
		involved := child(e, "involvedObject")
		if text(involved, "uid") != uid || text(involved, "kind") != "Pod" || text(e, "reason") != "FailedMount" || (text(child(e, "source"), "component") != "kubelet" && text(e, "reportingComponent") != "kubelet") {
			continue
		}
		message := text(e, "message")
		switch mode {
		case "missing":
			if (strings.Contains(message, `secrets "encryption-key"`) || strings.Contains(message, `secret "encryption-key"`)) && strings.Contains(message, "not found") {
				return true
			}
		case "wrong":
			if volume != "" && strings.Contains(message, `"`+volume+`"`) && strings.Contains(message, "No key available with this passphrase.") {
				return true
			}
		}
	}
	return false
}
func probeSucceeded(pod object, uid, logs, phase string) bool {
	if uid == "" || text(child(pod, "metadata"), "uid") != uid || text(child(pod, "status"), "phase") != "Succeeded" || logs != "EMAIL_VOLUME_PROBE=PASS phase="+phase+"\n" {
		return false
	}
	cs := items(child(pod, "status"), "containerStatuses")
	if len(cs) != 1 || text(cs[0], "name") != "probe" || text(cs[0], "imageID") == "" {
		return false
	}
	terminated := child(child(cs[0], "state"), "terminated")
	return terminated["exitCode"] == float64(0)
}
func deletionBody(uid string) ([]byte, error) {
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,127}$`).MatchString(uid) {
		return nil, errors.New("cleanup-identity")
	}
	return json.Marshal(object{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": object{"uid": uid}, "propagationPolicy": "Foreground"})
}
