package main

import (
	"strings"
	"testing"
)

func TestBackupControllerImageRefusesRegistryLookalikes(t *testing.T) {
	for _, image := range []string{
		"velero/velero:v1.18.4",
		"docker.io/velero/velero:v1.18.4",
		"docker.io/velero/velero:v1.18.4@sha256:" + strings.Repeat("a", 64),
	} {
		if !approvedBackupControllerImage(image) {
			t.Errorf("approved controller refused: %s", image)
		}
	}
	for _, image := range []string{
		"dockerXio/velero/velero:v1.18.4",
		"docker-io/velero/velero:v1.18.4",
		"evildocker.io/velero/velero:v1.18.4",
		"docker.io.example.test/velero/velero:v1.18.4",
		"docker.io/velero/velero:v1.18.40",
		"docker.io/velero/velero:v1.18.4@sha256:" + strings.Repeat("a", 63),
		"docker.io/velero/velero:v1.18.4\n",
	} {
		if approvedBackupControllerImage(image) {
			t.Errorf("unapproved controller accepted: %s", image)
		}
	}
}
