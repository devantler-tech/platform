package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	keeperPolicyKind  = "validatingadmissionpolicies.admissionregistration.k8s.io"
	keeperBindingKind = "validatingadmissionpolicybindings.admissionregistration.k8s.io"
	keeperPolicyAPI   = "/apis/admissionregistration.k8s.io/v1/"
)

type corootKeeperFences struct{ pod, claim object }

func (j *corootKeeperJournal) installFences(ctx context.Context) (corootKeeperFences, error) {
	r := j.record
	member := r.Before.Members[r.Input.Ordinal]
	raw, err := j.c.kubectl(ctx, nil, "auth", "whoami", "--output=json")
	var actor object
	if err != nil || json.Unmarshal(raw, &actor) != nil {
		return corootKeeperFences{}, errCorootKeeperReceipt
	}
	user := text(actor, "status", "userInfo", "username")
	if user == "" || len(user) > 256 || strings.ContainsAny(user, "\r\n\x00") {
		return corootKeeperFences{}, errCorootKeeperReceipt
	}
	owner := r.Run + ":" + r.Source + ":" + r.Rebind.Original.Claim.UID
	fences := corootKeeperFences{
		pod:   corootKeeperPodFence("lh-keeper-pod-"+r.Run, owner, member.Pod, r.Rebind.Original.Claim.Name, corootKeeperCopyPrefix+r.Run, user),
		claim: corootKeeperClaimFence("lh-keeper-pvc-"+r.Run, owner, r.Rebind.Original.Claim.Name, r.Rebind.StagedPV.Name),
	}
	for _, item := range []struct {
		desired object
		phase   string
		pod     bool
	}{{fences.pod, "member-fenced", true}, {fences.claim, "claim-fenced", false}} {
		policy, err := j.c.create(ctx, r.Source, item.desired)
		if err != nil {
			return fences, err
		}
		receipt := benchmarkFenceReceipt{PolicyUID: text(policy, "metadata", "uid")}
		if !isolationUID.MatchString(receipt.PolicyUID) || !matchesOwnedBenchmarkPolicy(policy, item.desired, receipt.PolicyUID) ||
			text(policy, "metadata", "annotations", corootKeeperFenceOwner) != owner {
			return fences, errCorootKeeperReceipt
		}
		if item.pod {
			r.PodFence = receipt
		} else {
			r.ClaimFence = receipt
		}
		r.Phase = item.phase + "-policy"
		if err := j.advance(ctx, r); err != nil {
			return fences, err
		}
		name := text(item.desired, "metadata", "name")
		expected := benchmarkDenyBinding(name, receipt.PolicyUID)
		valueObject(expected, "metadata")["annotations"] = object{corootKeeperFenceOwner: owner}
		binding, err := j.c.create(ctx, r.Source, expected)
		if err != nil {
			return fences, err
		}
		receipt.BindingUID = text(binding, "metadata", "uid")
		if !isolationUID.MatchString(receipt.BindingUID) || !matchesOwnedBenchmarkFence(policy, binding, item.desired, receipt) ||
			text(binding, "metadata", "annotations", corootKeeperFenceOwner) != owner {
			return fences, errCorootKeeperReceipt
		}
		if item.pod {
			r.PodFence = receipt
		} else {
			r.ClaimFence = receipt
		}
		r.Phase = item.phase
		if err := j.advance(ctx, r); err != nil {
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
			proof = proveReferenceFenceOnServingAPIs(ctx, j.c, r.Source, "", witness.policy, witness.receipt,
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
