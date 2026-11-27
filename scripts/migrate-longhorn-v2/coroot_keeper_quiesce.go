package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	keeperPolicyKind  = "validatingadmissionpolicies.admissionregistration.k8s.io"
	keeperBindingKind = "validatingadmissionpolicybindings.admissionregistration.k8s.io"
	keeperPolicyAPI   = "/apis/admissionregistration.k8s.io/v1/"
)

type corootKeeperFences struct{ pod, claim object }

func corootKeeperFenceUser(ctx context.Context, c client) (string, error) {
	raw, err := c.kubectl(ctx, nil, "auth", "whoami", "--output=json")
	var actor object
	if err != nil || json.Unmarshal(raw, &actor) != nil {
		return "", errCorootKeeperReceipt
	}
	user := text(actor, "status", "userInfo", "username")
	if !validCorootKeeperFenceUser(user) {
		return "", errCorootKeeperReceipt
	}
	return user, nil
}

func validCorootKeeperFenceUser(user string) bool {
	return user != "" && len(user) <= 256 && !strings.ContainsAny(user, "\r\n\x00")
}

func keeperFencesFor(r corootKeeperCutoverRecord) corootKeeperFences {
	member := r.Before.Members[r.Input.Ordinal]
	owner := r.Run + ":" + r.Source + ":" + r.Rebind.Original.Claim.UID
	return corootKeeperFences{
		pod:   corootKeeperPodFence("lh-keeper-pod-"+r.Run, owner, member.Pod, r.Rebind.Original.Claim.Name, corootKeeperCopyPrefix+r.Run, r.FenceUser),
		claim: corootKeeperClaimFence("lh-keeper-pvc-"+r.Run, owner, r.Rebind.Original.Claim.Name, r.Rebind.StagedPV.Name),
	}
}

func (j *corootKeeperJournal) installFences(ctx context.Context) (corootKeeperFences, error) {
	fences := keeperFencesFor(j.record)
	for _, phase := range []string{"member-fenced-policy", "member-fenced", "claim-fenced-policy", "claim-fenced"} {
		if _, err := j.createKeeperAsset(ctx, phase); err != nil {
			return fences, err
		}
	}
	return fences, nil
}

func (j *corootKeeperJournal) proveFences(ctx context.Context, fences corootKeeperFences) error {
	r := j.record
	for _, witness := range []struct {
		policy                  object
		receipt                 benchmarkFenceReceipt
		resource, name, message string
		body                    object
	}{
		{fences.pod, r.PodFence, "pods", "coroot-keeper-source-fence-probe", corootKeeperPodFenceMessage, keeperPodProbe(r.Rebind.Original.Claim.Name)},
		{fences.claim, r.ClaimFence, "persistentvolumeclaims", r.Rebind.Original.Claim.Name, corootKeeperClaimFenceMessage, keeperClaimProbe(r.Rebind.Original.Claim.Name)},
	} {
		policyName := text(witness.policy, "metadata", "name")
		rawPath := "/api/v1/namespaces/" + corootKeeperNamespace + "/" + witness.resource
		var proof error
		err := waitReadyWithin(ctx, 75*time.Second, func(ctx context.Context) error {
			proof = proveReferenceFenceOnServingAPIs(ctx, j.c, j.source(), "", witness.policy, witness.receipt,
				func(ctx context.Context, c *http.Client, endpoint *url.URL) error {
					code, raw, err := benchmarkAdmissionRequest(ctx, c, endpoint, rawPath, http.MethodPost, "application/json", witness.body)
					if err != nil {
						return err
					}
					if !benchmarkDenialMatches(code, raw, "", witness.resource, witness.name, policyName, policyName, witness.message) {
						return fmt.Errorf("the serving API did not prove the exact Keeper %s fence (HTTP %d)", witness.resource, code)
					}
					return nil
				})
			return proof
		})
		if err != nil {
			return errors.Join(err, proof)
		}
	}
	return nil
}

func keeperPodProbe(claim string) object {
	return object{"apiVersion": "v1", "kind": "Pod",
		"metadata": object{"name": "coroot-keeper-source-fence-probe", "namespace": corootKeeperNamespace},
		"spec": object{
			"restartPolicy": "Never", "automountServiceAccountToken": false,
			"securityContext": object{"runAsNonRoot": true, "runAsUser": float64(65534),
				"seccompProfile": object{"type": "RuntimeDefault"}},
			"containers": []any{object{"name": "probe", "image": corootKeeperImageID,
				"securityContext": object{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
					"capabilities": object{"drop": []any{"ALL"}}}}},
			"volumes": []any{object{"name": "source", "persistentVolumeClaim": object{"claimName": claim}}},
		},
	}
}

func keeperClaimProbe(claim string) object {
	return object{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": object{"name": claim, "namespace": corootKeeperNamespace},
		"spec": object{"storageClassName": "hcloud", "accessModes": []any{"ReadWriteOnce"}, "volumeMode": "Filesystem",
			"resources": object{"requests": object{"storage": corootKeeperDataSize}}},
	}
}

func (j *corootKeeperJournal) evictFollower(ctx context.Context, fences corootKeeperFences, disks []cnpgRecoveryDiskBinding) error {
	r := j.record
	target := r.Before.Members[r.Input.Ordinal]
	ready, err := readCorootKeeperQuorum(ctx, j.c, r.Source, disks)
	if err != nil || ready != r.Before || target.Leader {
		return errCorootKeeperQuorum
	}
	if err := j.proveFences(ctx, fences); err != nil {
		return err
	}
	r.Phase = "eviction-requested"
	if err := j.advance(ctx, r); err != nil {
		return err
	}
	pod, err := j.c.get(ctx, "pod", corootKeeperNamespace, target.Pod, false)
	if err != nil || !recoveredNativeObject(pod, "v1", "Pod", corootKeeperNamespace) ||
		text(pod, "metadata", "uid") != target.UID || text(pod, "spec", "nodeName") != target.Node {
		return errCorootKeeperQuorum
	}
	path := "/api/v1/namespaces/" + corootKeeperNamespace + "/pods/" + target.Pod + "/eviction"
	for _, dry := range []bool{true, false} {
		if err := sourceCurrent(ctx, j.c.run, r.Source); err != nil {
			return err
		}
		deleteOptions := object{"preconditions": object{"uid": target.UID, "resourceVersion": text(pod, "metadata", "resourceVersion")}}
		if dry {
			deleteOptions["dryRun"] = []any{"All"}
		}
		body, err := json.Marshal(object{"apiVersion": "policy/v1", "kind": "Eviction",
			"metadata": object{"name": target.Pod, "namespace": corootKeeperNamespace}, "deleteOptions": deleteOptions})
		if err != nil {
			return err
		}
		if !dry {
			if err := j.verifyKeeperSourceCopyLayout(ctx, target); err != nil {
				return err
			}
			stateful, err := j.c.get(ctx, "statefulset", corootKeeperNamespace, corootKeeperName, false)
			if err != nil || benchmarkIdentity(stateful) != r.Before.Controller {
				return errCorootKeeperQuorum
			}
			leader, err := probeCorootKeeperMember(ctx, j.c, target, stateful, r.Input.Ordinal, 2)
			if err != nil || leader {
				return errCorootKeeperQuorum
			}
		}
		if _, err := j.c.kubectl(ctx, body, "create", "--raw="+path, "--filename=/dev/stdin"); err != nil {
			return err
		}
	}
	return waitReadyWithin(ctx, 3*time.Minute, func(ctx context.Context) error {
		if err := verifyCorootKeeperSurvivors(ctx, j.c, r.Before, r.Input.Ordinal); err != nil {
			return err
		}
		return verifyCorootKeeperDetach(ctx, j.c, r.Rebind, true)
	})
}

func (j *corootKeeperJournal) verifyKeeperSourceCopyLayout(ctx context.Context, member corootKeeperMember) error {
	const path = "/var/lib/clickhouse-keeper"
	const check = `set -euo pipefail
test "$(id -u)" = 65534
test "$(id -g)" = 65534
root=$1
test -d "$root/coordination/log"
test -d "$root/coordination/snapshots"
foreign=$(find "$root" -xdev -mindepth 1 -maxdepth 1 ! -name lost+found ! -name coordination -printf x -quit)
test -z "$foreign"
foreign=$(find "$root/coordination" -xdev \( ! -type f ! -type d -o ! -uid 65534 -o ! -gid 65534 -o -type f -links +1 \) -printf x -quit)
test -z "$foreign"
du -sx --apparent-size --block-size=1 -- "$root/coordination" | cut -f1
`
	stateful, err := j.c.get(ctx, "statefulset", corootKeeperNamespace, corootKeeperName, false)
	if err != nil || benchmarkIdentity(stateful) != j.record.Before.Controller {
		return errCorootKeeperCopy
	}
	pod, err := j.c.get(ctx, "pod", corootKeeperNamespace, member.Pod, false)
	if err != nil {
		return err
	}
	bound, err := bindCorootKeeperMember(pod, stateful, j.record.Input.Ordinal)
	if err != nil || bound != member {
		return errCorootKeeperCopy
	}
	found := 0
	containers, _ := value(pod, "spec", "containers").([]any)
	for _, raw := range containers {
		c, _ := raw.(object)
		if text(c, "name") != corootKeeperContainer {
			continue
		}
		mounts, _ := value(c, "volumeMounts").([]any)
		for _, rawMount := range mounts {
			mount, _ := rawMount.(object)
			if text(mount, "name") == "data" && text(mount, "mountPath") == path &&
				absentOrEmptyString(value(mount, "subPath")) && absentOrEmptyString(value(mount, "subPathExpr")) {
				found++
			}
		}
	}
	if found != 1 {
		return errCorootKeeperCopy
	}
	raw, err := j.c.kubectl(ctx, nil, "exec", member.Pod, "--namespace="+corootKeeperNamespace,
		"--container="+corootKeeperContainer, "--pod-running-timeout=10s", "--", "bash", "-ec", check, "keeper-layout", path)
	need, parseErr := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || parseErr != nil || need < 1 || need > 10<<30 || need+(64<<20) > j.record.StagedFreeBytes {
		return errCorootKeeperCopy
	}
	pod, err = j.c.get(ctx, "pod", corootKeeperNamespace, member.Pod, false)
	bound, bindErr := bindCorootKeeperMember(pod, stateful, j.record.Input.Ordinal)
	if err != nil || bindErr != nil || bound != member {
		return errCorootKeeperCopy
	}
	return nil
}
