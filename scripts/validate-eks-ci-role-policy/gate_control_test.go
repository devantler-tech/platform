package main

import (
	"strings"
	"testing"
)

// loadGateControlJob gives each negative control a fresh workflow map and its
// selected job. The caller still verifies that its own mutation took effect.
func loadGateControlJob(t *testing.T, key string) (map[string]workflow, string, string, job) {
	t.Helper()
	workflows := loadWorkflows(t)
	name, jobName, _ := strings.Cut(key, "/")
	parsedJob, ok := workflows[name].Jobs[jobName]
	if !ok {
		t.Fatalf("%s is missing, so this control cannot be applied", key)
	}
	return workflows, name, jobName, parsedJob
}
