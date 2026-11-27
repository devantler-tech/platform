package main

import (
	"bytes"
	"context"
	"errors"
	"time"
)

// readRecoveredPostgres reads only the owned isolated V2 recovery database.
func (c client) readRecoveredPostgres(ctx context.Context, cluster, pod object) ([]byte, error) {
	if !c.isolatedV2Database(cluster, pod) {
		return nil, errors.New("recovery read requires the isolated owned V2 cluster")
	}
	return c.readBoundPostgresSQL(ctx, cluster, pod, postgresRecoverySQL)
}

func (c client) isolatedV2Database(cluster, pod object) bool {
	ns, name := text(cluster, "metadata", "namespace"), text(pod, "metadata", "name")
	return c.context == "admin@prod" && text(cluster, "metadata", "name") == "restore" &&
		isolationNamespace.MatchString(ns) && text(pod, "metadata", "namespace") == ns &&
		safeKubeRequest("pod", ns, name, false) == nil && text(cluster, "metadata", "uid") != "" &&
		value(cluster, "metadata", "deletionTimestamp") == nil &&
		value(cluster, "spec", "instances") == float64(1) && text(cluster, "spec", "storage", "storageClass") == "longhorn-v2"
}

// Read only metadata on the same running recovery process. The caller still
// needs the independent storage graph, admitted specification and history proof.
func (c client) readRecoveredFilesystem(ctx context.Context, cluster, pod object) (cnpgFilesystemBinding, error) {
	var empty cnpgFilesystemBinding
	refused := errors.New("the isolated recovery filesystem observation is unverified")
	if !commandContextActive(ctx) || !c.isolatedV2Database(cluster, pod) {
		return empty, refused
	}
	ctx, cancel := context.WithTimeout(ctx, 95*time.Second)
	defer cancel()
	ns, name := text(pod, "metadata", "namespace"), text(pod, "metadata", "name")
	before, err := c.get(ctx, "pod", ns, name, false)
	if err != nil || verifyPostgresPod(before, pod, cluster) != nil || !sameRecoveredProgram(before, pod) {
		return empty, refused
	}
	executeRead := func(args ...string) ([]byte, error) {
		command := []string{"exec", name, "--namespace=" + ns, "--container=postgres", "--pod-running-timeout=20s", "--",
			"/usr/bin/env", "-i", "LC_ALL=C", "/usr/bin/timeout", "--kill-after=2s", "15s"}
		return c.kubectl(ctx, nil, append(command, args...)...)
	}
	const data = recoveredPostgresMountPath + "/pgdata"
	mount := []string{"/usr/bin/findmnt", "--kernel", "--nocanonicalize", "--json", "--submounts", "--mountpoint", recoveredPostgresMountPath, "--output", "TARGET,FSROOT,FSTYPE,MAJ:MIN,ID,VFS-OPTIONS"}
	stat := []string{"/usr/bin/stat", "--format=%d %i %F", "--", recoveredPostgresMountPath, data, data + "/pg_wal", data + "/pg_tblspc"}
	mountedBefore, err := executeRead(mount...)
	if err != nil {
		return empty, refused
	}
	mountedDevice, err := recoveredFilesystemMountDevice(mountedBefore)
	if err != nil {
		return empty, refused
	}
	first, err := executeRead(stat...)
	if err != nil {
		return empty, refused
	}
	walk, err := executeRead("/usr/bin/find", "-P", data, "-xdev", "-printf", "%y %D %i\n")
	if err != nil {
		return empty, refused
	}
	last, err := executeRead(stat...)
	if err != nil {
		return empty, refused
	}
	mountedAfter, err := executeRead(mount...)
	if err != nil || !bytes.Equal(mountedBefore, mountedAfter) {
		return empty, refused
	}
	after, err := c.get(ctx, "pod", ns, name, false)
	if err != nil || verifyPostgresPod(after, pod, cluster) != nil || !sameRecoveredProgram(after, before) || !commandContextActive(ctx) {
		return empty, refused
	}
	placement, err := bindCNPGRecoveredFilesystem(first, walk, last)
	if err != nil || placement.Device != mountedDevice {
		return empty, refused
	}
	return placement, nil
}
