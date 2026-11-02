package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRecoveryRejectsRetiringObjects(t *testing.T) {
	for _, name := range []string{"configuration", "deployment", "cron"} {
		t.Run(name, func(t *testing.T) {
			e := goodEvidence()
			var target *[]byte
			switch name {
			case "configuration":
				target = &e.Configuration
			case "deployment":
				target = &e.Deployment
			case "cron":
				target = &e.Cron
			}
			var object map[string]any
			if err := json.Unmarshal(*target, &object); err != nil {
				t.Fatal(err)
			}
			object["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-27T23:05:30Z"
			*target, _ = json.Marshal(object)
			if _, err := assessRecovery(e, map[string]string{"managed-resources.tsv": "sample-v1"}, "reviewed-checksum", observationTime); err == nil {
				t.Fatal("a retiring observation must not establish recovery")
			}
		})
	}
}
func TestRecoveryRejectsContradictoryAvailability(t *testing.T) {
	e := goodEvidence()
	e.Deployment = []byte(strings.ReplaceAll(string(e.Deployment), `"type":"Available","status":"True"`, `"type":"Available","status":"False"`))
	if _, err := assessRecovery(e, map[string]string{"managed-resources.tsv": "sample-v1"}, "reviewed-checksum", observationTime); err == nil {
		t.Fatal("an unavailable deployment must not establish recovery")
	}
}
