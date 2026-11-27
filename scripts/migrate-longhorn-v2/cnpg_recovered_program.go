package main

import "time"

type cnpgRecoveredProducer struct {
	Image, Container, StartedAt, FinishedAt string
}

// Call only after validating the full program profile below.
func recoveredProgramProducer(pod object) cnpgRecoveredProducer {
	statuses, _ := value(pod, "status", "initContainerStatuses").([]any)
	if len(statuses) != 1 {
		return cnpgRecoveredProducer{}
	}
	status, _ := statuses[0].(object)
	return cnpgRecoveredProducer{text(status, "imageID"), text(status, "containerID"), text(status, "state", "terminated", "startedAt"), text(status, "state", "terminated", "finishedAt")}
}

func sameRecoveredProgram(a, b object) bool {
	return recoveredPostgresProgram(a) && recoveredPostgresProgram(b) && recoveryJSONEqual(value(a, "spec"), value(b, "spec")) && recoveredProgramProducer(a) == recoveredProgramProducer(b)
}

// Bind the executable producer of the fixed, newly created CNPG 1.30.2
// recovery instance. Generated credentials/RBAC, Node/boot, realized isolation,
// SQL and actual filesystem placement remain independent required evidence.
func recoveredPostgresProgram(pod object) bool {
	spec, _ := value(pod, "spec").(object)
	containers, _ := value(spec, "containers").([]any)
	init, _ := value(spec, "initContainers").([]any)
	if len(containers) != 1 || len(init) != 1 || !absentOrEmptyArray(value(spec, "ephemeralContainers")) {
		return false
	}
	for _, field := range []string{"hostPID", "hostIPC", "hostNetwork", "shareProcessNamespace"} {
		if !falseOrAbsent(value(spec, field)) {
			return false
		}
	}
	postgres, _ := containers[0].(object)
	bootstrap, _ := init[0].(object)
	// New instances use TLS on the status port. No custom cluster annotations,
	// metrics TLS, extra environments or log remapping belong to this plan.
	if text(postgres, "name") != "postgres" || !recoveryJSONEqual(postgres["command"], []any{"/controller/manager", "instance", "run", "--status-port-tls", "--log-level=info"}) ||
		text(bootstrap, "name") != "bootstrap-controller" || text(bootstrap, "image") != "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.2" || !recoveryJSONEqual(bootstrap["command"], []any{"/manager", "bootstrap", "/controller/manager", "--log-level=info"}) ||
		!absentOrEmptyArray(bootstrap["env"]) || !recoveredPostgresEnvironment(pod, postgres) {
		return false
	}
	for _, c := range []object{postgres, bootstrap} {
		if !absentOrEmptyArray(c["args"]) || !absentOrEmptyArray(c["envFrom"]) || !absentOrEmptyArray(c["volumeDevices"]) || !absentOrEmptyString(c["workingDir"]) || c["lifecycle"] != nil || c["restartPolicy"] != nil || c["restartPolicyRules"] != nil || !falseOrAbsent(value(c, "securityContext", "privileged")) || !falseOrAbsent(value(c, "securityContext", "allowPrivilegeEscalation")) || !recoveredProgramMounts(c) {
			return false
		}
	}
	volumes, _ := spec["volumes"].([]any)
	scratch := 0
	for _, raw := range volumes {
		v, _ := raw.(object)
		if text(v, "name") == "scratch-data" {
			directory, ok := v["emptyDir"].(object)
			if !ok || len(v) != 2 || !absentOrEmptyString(directory["medium"]) || directory["sizeLimit"] != nil || len(directory) > 1 {
				return false
			}
			scratch++
		}
	}
	if scratch != 1 {
		return false
	}
	statuses, _ := value(pod, "status", "initContainerStatuses").([]any)
	if len(statuses) != 1 {
		return false
	}
	status, _ := statuses[0].(object)
	state, _ := status["state"].(object)
	terminated, _ := state["terminated"].(object)
	start, startErr := time.Parse(time.RFC3339Nano, text(terminated, "startedAt"))
	finish, finishErr := time.Parse(time.RFC3339Nano, text(terminated, "finishedAt"))
	return text(status, "name") == "bootstrap-controller" && (cnpgReviewedManagerImage(text(status, "imageID"), "amd64") || cnpgReviewedManagerImage(text(status, "imageID"), "arm64")) &&
		postgresContainerID.MatchString(text(status, "containerID")) && value(status, "restartCount") == float64(0) && len(state) == 1 && value(terminated, "exitCode") == float64(0) && startErr == nil && finishErr == nil && !start.IsZero() && !finish.Before(start)
}

func recoveredPostgresEnvironment(pod, container object) bool {
	expected := map[string]string{
		"PGDATA": "/var/lib/postgresql/data/pgdata", "POD_NAME": text(pod, "metadata", "name"), "NAMESPACE": text(pod, "metadata", "namespace"), "CLUSTER_NAME": "restore",
		"PSQL_HISTORY": "/controller/tmp/.psql_history", "PGPORT": "5432", "PGHOST": "/controller/run", "TMPDIR": "/controller/tmp",
	}
	env, _ := container["env"].([]any)
	if len(env) != len(expected) {
		return false
	}
	seen := map[string]bool{}
	for _, raw := range env {
		e, _ := raw.(object)
		name := text(e, "name")
		want, ok := expected[name]
		if !ok || seen[name] || len(e) != 2 || text(e, "value") != want {
			return false
		}
		seen[name] = true
	}
	return true
}

func recoveredProgramMounts(container object) bool {
	mounts, _ := container["volumeMounts"].([]any)
	expected := map[string]string{"/var/lib/postgresql/data": "pgdata", "/run": "scratch-data", "/controller": "scratch-data", "/dev/shm": "shm"}
	seen := map[string]bool{}
	for _, raw := range mounts {
		m, _ := raw.(object)
		location := text(m, "mountPath")
		want := expected[location]
		// Kubernetes may add its native read-only service-account projection.
		// Its owning account, sources and RBAC still need separate qualification.
		if location == "/var/run/secrets/kubernetes.io/serviceaccount" && text(m, "name") != "" && value(m, "readOnly") == true {
			want = text(m, "name")
		} else if !falseOrAbsent(m["readOnly"]) {
			return false
		}
		if want == "" || seen[location] || text(m, "name") != want || !absentOrEmptyString(m["subPath"]) || !absentOrEmptyString(m["subPathExpr"]) || m["mountPropagation"] != nil || m["recursiveReadOnly"] != nil {
			return false
		}
		seen[location] = true
	}
	for location := range expected {
		if !seen[location] {
			return false
		}
	}
	return true
}

func absentOrEmptyArray(value any) bool {
	if value == nil {
		return true
	}
	items, ok := value.([]any)
	return ok && len(items) == 0
}
