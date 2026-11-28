package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

func (r openBaoPairRecord) admissionPolicies() ([2]object, error) {
	plan := r.Plan
	pod, err := openBaoCopyFence(plan.Run, plan.Source, plan.Input.Member, r.FenceUser)
	if err != nil {
		return [2]object{}, err
	}
	claims, err := openBaoClaimFence(plan.Input.Member, r.FenceUser, r.Destinations)
	return [2]object{pod, claims}, err
}

func (j *openBaoPairJournal) installAdmissionFences(ctx context.Context) error {
	r := j.record
	if j.client.context != "admin@prod" || r.Phase != openBaoPairStaged || !r.valid() || r.FenceUser != "" {
		return errOpenBaoProviderStorage
	}
	user, err := corootKeeperFenceUser(ctx, j.client)
	if err != nil {
		return err
	}
	r.FenceUser = user
	policies, err := r.admissionPolicies()
	if err != nil {
		return err
	}
	if err := j.advance(ctx, r); err != nil {
		return err
	}
	for index, kind := range openBaoFenceAssetKinds {
		r = j.record
		policy := policies[index/2]
		name := text(policy, "metadata", "name")
		owner := text(policy, "metadata", "annotations", openBaoCopyFenceOwner)
		desired := policy
		var existingPolicy object
		if index%2 == 1 {
			existingPolicy, err = j.client.get(ctx, keeperPolicyKind, "", name, false)
			if err != nil || benchmarkIdentity(existingPolicy) != r.FenceAssets[index-1] ||
				!matchesOwnedBenchmarkPolicy(existingPolicy, policy, r.FenceAssets[index-1].UID) ||
				text(existingPolicy, "metadata", "annotations", openBaoCopyFenceOwner) != owner {
				return errors.Join(err, errOpenBaoProviderStorage)
			}
			desired = benchmarkDenyBinding(name, r.FenceAssets[index-1].UID)
			valueObject(desired, "metadata")["annotations"] = object{openBaoCopyFenceOwner: owner}
		}
		r.FencePending = kind
		if err := j.advance(ctx, r); err != nil {
			return err
		}
		created, err := j.client.create(ctx, r.Plan.Source, desired)
		id := benchmarkIdentity(created)
		if err != nil || !openBaoPairIdentity(id, "", name) ||
			text(created, "metadata", "annotations", openBaoCopyFenceOwner) != owner {
			return errors.Join(err, errOpenBaoProviderStorage)
		}
		if existingPolicy != nil {
			if !matchesOwnedBenchmarkFence(existingPolicy, created, policy, benchmarkFenceReceipt{
				PolicyUID: r.FenceAssets[index-1].UID, BindingUID: id.UID}) {
				return errOpenBaoProviderStorage
			}
		} else if !matchesOwnedBenchmarkPolicy(created, policy, id.UID) {
			return errOpenBaoProviderStorage
		}
		r.FencePending, r.FenceAssets[index] = "", id
		if err := j.advance(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

func (j openBaoPairJournal) proveAdmissionFences(ctx context.Context) error {
	r := j.record
	policies, err := r.admissionPolicies()
	if err != nil || !r.valid() || r.FencePending != "" || r.FenceAssets[3].UID == "" {
		return errors.Join(err, errOpenBaoProviderStorage)
	}
	var originalRun string
	if os.Getenv("LONGHORN_V2_OPERATION") == openBaoPairResumeOperation {
		originalRun = r.Plan.Run
	}
	for index, policy := range policies {
		name := text(policy, "metadata", "name")
		receipt := benchmarkFenceReceipt{PolicyUID: r.FenceAssets[index*2].UID, BindingUID: r.FenceAssets[index*2+1].UID}
		var lastProof error
		err := waitReadyWithin(ctx, 75*time.Second, func(ctx context.Context) error {
			lastProof = proveReferenceFenceOnServingAPIs(ctx, j.client, r.Plan.Source, originalRun, policy, receipt,
				func(ctx context.Context, c *http.Client, endpoint *url.URL) error {
					return probeOpenBaoFence(ctx, c, endpoint, index, name, r.Plan)
				})
			return lastProof
		})
		if err != nil {
			resource := [2]string{"pod", "claim"}[index]
			return fmt.Errorf("OpenBao %s fence proof: %w", resource, errors.Join(err, lastProof))
		}
	}
	return nil
}

func probeOpenBaoFence(ctx context.Context, c *http.Client, endpoint *url.URL, index int, policy string, plan openBaoPairPlan) error {
	const api = "/api/v1/namespaces/" + openBaoNamespace + "/"
	for _, disk := range plan.Original.Disks {
		resource, message := "pods", openBaoCopyFenceMessage
		probe := keeperPodProbe(disk.PVC.Name)
		metadata := valueObject(probe, "metadata")
		metadata["name"], metadata["namespace"] = "openbao-storage-fence-probe", openBaoNamespace
		if index == 1 {
			resource, message = "persistentvolumeclaims", openBaoClaimFenceMessage
			probe = openBaoStagingClaims(plan.Run, plan.Source)[0]
			metadata := valueObject(probe, "metadata")
			metadata["name"] = disk.PVC.Name
			spec := valueObject(probe, "spec")
			spec["storageClassName"] = "hcloud"
		}
		code, body, err := benchmarkAdmissionRequest(ctx, c, endpoint, api+resource, http.MethodPost, "application/json", probe)
		if err != nil || !benchmarkDenialMatches(code, body, "", resource, text(probe, "metadata", "name"), policy, policy, message) {
			return fmt.Errorf("the serving API did not prove the exact OpenBao %s fence (HTTP %d)", resource, code)
		}
	}
	if index == 0 {
		copyPod := plan.copyPod()
		code, body, err := benchmarkAdmissionRequest(ctx, c, endpoint, api+"pods", http.MethodPost, "application/json", copyPod)
		var admitted object
		if err != nil || code != http.StatusCreated || strictjson.Validate(body, nil) != nil ||
			json.Unmarshal(body, &admitted) != nil || !matchStorageCopyVolumePodShape(admitted, copyPod, openBaoNamespace, openBaoCopyFenceOwner) {
			return fmt.Errorf("the serving API did not admit the required OpenBao copy pod shape in dry-run (HTTP %d)", code)
		}
	}
	return nil
}
