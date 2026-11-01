package main

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

type object = map[string]any

const recoveryLabel = "email.devantler.tech/data-recovery"

type sourceIdentity struct{ podUID, claimUID, volume, pv, claimSize, pvUID, reclaim string }
type messageIdentity struct{ email, blob, mailbox, messageID, body string }

// Scratch receipts may coexist with the data receipt. Select one data volume;
// proveBackup still binds its owner, source Pod, completion and byte accounting.
func sourceVolumeBackup(volumes []object, volume string) (object, error) {
	if volume == "" {
		return nil, errors.New("backup-volume-count")
	}
	var selected object
	for _, candidate := range volumes {
		if text(child(candidate, "spec"), "volume") != volume {
			continue
		}
		if selected != nil {
			return nil, errors.New("backup-volume-count")
		}
		selected = candidate
	}
	if selected == nil {
		return nil, errors.New("backup-volume-count")
	}
	return selected, nil
}

func child(o object, k string) object {
	v, _ := o[k].(map[string]any)
	if v == nil {
		return object{}
	}
	return v
}
func text(o object, k string) string { v, _ := o[k].(string); return v }
func items(o object, k string) []object {
	a, _ := o[k].([]any)
	var out []object
	for _, raw := range a {
		v, ok := raw.(map[string]any)
		if !ok {
			return nil
		}
		out = append(out, v)
	}
	return out
}

// A backup name alone never authorizes a restore. Bind the controller receipt
// to the created Backup UID and the exact serving Pod's data volume.
func proveBackup(b, p object, s sourceIdentity, i invocation, uid string, after time.Time) (string, error) {
	fail := func() (string, error) { return "", errors.New("backup-proof") }
	bm, bs := child(b, "metadata"), child(b, "status")
	if uid == "" || s.podUID == "" || text(bm, "uid") != uid || text(bm, "name") != i.namespace+"-backup" || text(bm, "namespace") != "velero" || text(child(bm, "labels"), recoveryLabel) != i.run || text(bs, "phase") != "Completed" || (bs["errors"] != nil && bs["errors"] != float64(0)) {
		return fail()
	}
	owners := items(child(p, "metadata"), "ownerReferences")
	if len(owners) != 1 || text(owners[0], "apiVersion") != "velero.io/v1" || text(owners[0], "kind") != "Backup" || text(owners[0], "name") != i.namespace+"-backup" || text(owners[0], "uid") != uid || owners[0]["controller"] != true {
		return fail()
	}
	ps, status := child(p, "spec"), child(p, "status")
	pod := child(ps, "pod")
	if text(child(p, "metadata"), "namespace") != "velero" || text(pod, "name") != "private-mail-0" || text(pod, "namespace") != "private-mail" || text(pod, "uid") != s.podUID || text(ps, "volume") != s.volume || text(ps, "backupStorageLocation") != "default" || text(ps, "uploaderType") != "kopia" || text(status, "phase") != "Completed" {
		return fail()
	}
	snap := text(status, "snapshotID")
	if !regexp.MustCompile(`^[0-9a-f]{32,64}$`).MatchString(snap) {
		return fail()
	}
	progress := child(status, "progress")
	total, ok := progress["totalBytes"].(float64)
	if !ok || total <= 0 || progress["bytesDone"] != total {
		return fail()
	}
	completed, err := time.Parse(time.RFC3339, text(status, "completionTimestamp"))
	backupCompleted, berr := time.Parse(time.RFC3339, text(bs, "completionTimestamp"))
	if err != nil || berr != nil || completed.Before(after) || backupCompleted.Before(completed) {
		return fail()
	}
	return snap, nil
}

func proveMessage(m object, w messageIdentity) bool {
	if w.email == "" || w.blob == "" || w.mailbox == "" || w.messageID == "" || w.body == "" || text(m, "id") != w.email || text(m, "blobId") != w.blob {
		return false
	}
	ids, ok := m["messageId"].([]any)
	if !ok || len(ids) != 1 || ids[0] != w.messageID {
		return false
	}
	boxes := child(m, "mailboxIds")
	if len(boxes) != 1 || boxes[w.mailbox] != true {
		return false
	}
	parts := items(m, "textBody")
	if len(parts) != 1 {
		return false
	}
	body := child(child(m, "bodyValues"), text(parts[0], "partId"))
	// RFC 8621 BodyValue flags default to false when omitted. A present
	// non-boolean, truncation or encoding problem still refuses the proof.
	for _, flag := range []string{"isTruncated", "isEncodingProblem"} {
		if value, present := body[flag]; present && value != false {
			return false
		}
	}
	return strings.TrimRight(text(body, "value"), "\r\n") == w.body
}
