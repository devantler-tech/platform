package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func resourceHolder(j journal) string {
	if j.ResourceHolder != "" {
		return j.ResourceHolder
	}
	return j.Holder
}

// Journal content is data, not authority to select arbitrary mutation targets.
func journalScope(j journal) error {
	// Temporary client-label borrowing was removed before this procedure shipped.
	// Such a journal is outside the reviewed mutation scope, not cleanup authority.
	if len(j.Nodes) != 0 {
		return errors.New("temporary client-label journals are outside the reviewed maintenance scope")
	}
	holder := regexp.MustCompile(`^[1-9][0-9]*\.[1-9][0-9]*$`)
	if !holder.MatchString(j.Holder) || !holder.MatchString(resourceHolder(j)) || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(j.Source) {
		return errors.New("journal writer identity is malformed")
	}
	if len(j.Clusters) != len(clusterTargets) || len(j.Extra) != len(extraTargets) || len(j.Claims) == 0 || len(j.PVs) == 0 || len(j.Volumes) == 0 {
		return errors.New("journal inventory is incomplete")
	}
	matches := func(o object, kind, ns, name string) bool {
		return meta(o) && str(o, "kind") == kind && str(o, "metadata", "namespace") == ns && str(o, "metadata", "name") == name
	}
	clusters, err := index(j.Clusters)
	if err != nil {
		return errors.New("journal database inventory is malformed")
	}
	for _, t := range clusterTargets {
		if !matches(clusters[t.ns+"/"+t.name], "Cluster", t.ns, t.name) {
			return errors.New("journal database scope differs")
		}
	}
	if _, err := index(j.Schedules); err != nil {
		return errors.New("journal scheduled backup identities are malformed")
	}
	for _, schedule := range j.Schedules {
		if !scheduleInScope(schedule, j.Clusters) {
			return errors.New("journal scheduled backup scope differs")
		}
	}
	if !matches(j.Release, "HelmRelease", "crossview", "crossview") || !matches(j.Deployment, "Deployment", "crossview", "crossview-postgres") {
		return errors.New("journal consumer scope differs")
	}
	if !matches(j.BackupRelease, "HelmRelease", "velero", "velero") {
		return errors.New("journal backup reconciler scope differs")
	}
	for i, t := range extraTargets {
		if !matches(j.Extra[i].Release, "HelmRelease", t.ns, t.release) || !matches(j.Extra[i].Deployment, "Deployment", t.ns, t.deployment) {
			return errors.New("journal consumer scope differs")
		}
	}
	for _, list := range [][]object{j.Claims, j.PVs, j.Volumes} {
		for _, o := range list {
			if !meta(o) {
				return errors.New("journal storage identity is malformed")
			}
		}
	}
	return nil
}

// Takeover never guesses whether the old writer is alive. After terminal proof,
// both paused and released resources must still match their original identities.
// ResourceHolder retains the exact old annotation for guarded restoration.
func (e engine) recoverInterrupted(ctx context.Context, j journal, original object) error {
	if j.Phase != "prepared" && j.Phase != "detached" {
		return errors.New("unknown interrupted maintenance phase")
	}
	if err := e.replicaExclusion(ctx); err != nil {
		return err
	}
	if j.Holder != e.holder || j.Source != e.source {
		if e.previousFinished == nil {
			return errors.New("previous holder liveness is unknown")
		}
		if err := e.previousFinished(ctx, j); err != nil {
			return err
		}
	}
	if err := e.storagePreserved(ctx, j); err != nil {
		return err
	}
	for _, old := range journalResources(j) {
		current, err := e.c.read(ctx, resourceKind(old), str(old, "metadata", "namespace"), str(old, "metadata", "name"))
		if err != nil {
			return err
		}
		if !released(old, current) {
			if _, err = restorePatch(old, current, resourceHolder(j), str(old, "kind") == "Deployment"); err != nil {
				return err
			}
		}
	}
	j.ResourceHolder = resourceHolder(j)
	j.Holder, j.Source = e.holder, e.source
	if err := e.writeJournal(ctx, j, original); err != nil {
		return err
	}
	if e.provenReleased(ctx, j) == nil {
		j.Phase = "restored"
		_, o, err := e.ownerJournal(ctx)
		if err != nil {
			return err
		}
		if err = e.writeJournal(ctx, j, o); err != nil {
			return err
		}
		return e.prepare(ctx)
	}
	if j.Phase == "detached" {
		if e.revisionSafe == nil {
			return errors.New("published revision proof is missing")
		}
		if err := e.revisionSafe(ctx); err != nil {
			return err
		}
		ok, err := e.detached(ctx, j)
		if err != nil {
			return err
		}
		if ok {
			return nil
		} // Resume the audited publication while still detached.
	}
	if err := e.restore(ctx); err != nil {
		return err
	}
	return e.prepare(ctx)
}

func (e engine) storagePreserved(ctx context.Context, j journal) error {
	s, err := e.collect(ctx)
	if err != nil {
		return err
	}
	if err = preserved(j.Claims, s.claims); err != nil {
		return err
	}
	if err = preserved(j.PVs, s.pvs); err != nil {
		return err
	}
	return backingPreserved(j.Volumes, s.volumes)
}

// The old selector may be republished only when publication positively never
// started, cleanup completed, and live release and backing identities agree.
func (e engine) legacyPublicationSafe(ctx context.Context) error {
	all, err := e.c.list(ctx, "configmaps", journalNS)
	if err != nil {
		return err
	}
	for _, o := range all {
		if str(o, "metadata", "name") != journalName {
			continue
		}
		j, original, err := e.readJournal(ctx)
		if err != nil {
			return err
		}
		// The failed deploy and its protected heal share one run attempt,
		// but the heal checks out validated main. Restore that attempt's
		// owned pauses before deciding whether legacy publication is safe.
		// A foreign attempt still needs the separate terminal takeover path.
		if (j.Phase == "prepared" || j.Phase == "detached") && j.Holder == e.holder {
			if err = e.replicaExclusion(ctx); err != nil {
				return err
			}
			if err = e.storagePreserved(ctx, j); err != nil {
				return err
			}
			for _, old := range journalResources(j) {
				current, readErr := e.c.read(ctx, resourceKind(old), str(old, "metadata", "namespace"), str(old, "metadata", "name"))
				if readErr != nil {
					return readErr
				}
				if !released(old, current) {
					if _, err = restorePatch(old, current, resourceHolder(j), str(old, "kind") == "Deployment"); err != nil {
						return err
					}
				}
			}
			j.Source = e.source
			if err = e.writeJournal(ctx, j, original); err != nil {
				return err
			}
			if err = e.restore(ctx); err != nil {
				return err
			}
			j, _, err = e.readJournal(ctx)
			if err != nil {
				return err
			}
		}
		if j.Phase != "restored" || !j.PublicationNotStarted {
			return errors.New("legacy publication after maintenance began is refused")
		}
		if err = e.provenReleased(ctx, j); err != nil {
			return err
		}
		s, err := e.collect(ctx)
		if err != nil {
			return err
		}
		if err = storageBindingsPreserved(j.Claims, s.claims); err != nil {
			return err
		}
		if err = storageBindingsPreserved(j.PVs, s.pvs); err != nil {
			return err
		}
		// Capacity is no longer owned; backing identity and format still are.
		return backingFieldsPreserved(j.Volumes, s.volumes, []string{"dataEngine", "frontend", "fromBackup", "backingImage"})
	}
	return nil
}

// A completed attempt is the only cross-run handoff supported here. Running,
// missing, forbidden, malformed or mismatched evidence stays HOLD.
func finishedAttempt(ctx context.Context, j journal) error {
	parts := strings.Split(j.Holder, ".")
	if len(parts) != 2 || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(parts[0]) || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(parts[1]) {
		return errors.New("previous holder identity is malformed")
	}
	token := os.Getenv("GH_TOKEN")
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if token == "" {
		return errors.New("previous holder liveness credential is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/devantler-tech/platform/actions/runs/"+parts[0]+"/attempts/"+parts[1], nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	c := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Do(req)
	if err != nil {
		return errors.New("previous holder liveness read failed")
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return errors.New("previous holder liveness is unavailable")
	}
	return verifyFinishedAttempt(j, io.LimitReader(res.Body, 2<<20))
}

func verifyFinishedAttempt(j journal, body io.Reader) error {
	parts := strings.Split(j.Holder, ".")
	if len(parts) != 2 {
		return errors.New("previous holder identity is malformed")
	}
	var run struct {
		ID      int64  `json:"id"`
		Attempt int64  `json:"run_attempt"`
		SHA     string `json:"head_sha"`
		Status  string `json:"status"`
	}
	if err := json.NewDecoder(body).Decode(&run); err != nil {
		return errors.New("previous holder liveness is malformed")
	}
	id, e1 := strconv.ParseInt(parts[0], 10, 64)
	attempt, e2 := strconv.ParseInt(parts[1], 10, 64)
	if e1 != nil || e2 != nil || run.ID != id || run.Attempt != attempt || run.SHA != j.Source || run.Status != "completed" {
		return errors.New("previous holder is running, mismatched or unknown")
	}
	return nil
}

// Only the selector manifest may change in the initial migration revision.
// Reject combined consumer/config changes before pausing, rather than comparing
// old workload specifications with a legitimately different published revision.
func isolatedRevision(ctx context.Context, source string) error {
	b, err := os.ReadFile(os.Getenv("GITHUB_EVENT_PATH"))
	if err != nil {
		return errors.New("publication event is unavailable")
	}
	var event struct {
		Before     string `json:"before"`
		MergeGroup *struct {
			Base string `json:"base_sha"`
			Head string `json:"head_sha"`
		} `json:"merge_group"`
	}
	if json.Unmarshal(b, &event) != nil {
		return errors.New("publication event is malformed")
	}
	head, err := exec.CommandContext(ctx, "git", "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(head)) != source {
		return errors.New("publication checkout does not match its source")
	}
	base := event.Before
	if os.Getenv("GITHUB_EVENT_NAME") == "merge_group" {
		if event.MergeGroup == nil || event.MergeGroup.Head != source {
			return errors.New("merge-group revision is unbound")
		}
		base = event.MergeGroup.Base
	} else if os.Getenv("GITHUB_EVENT_NAME") == "workflow_dispatch" {
		b, err = exec.CommandContext(ctx, "git", "rev-parse", "HEAD^").Output()
		if err != nil {
			return errors.New("publication parent is unavailable")
		}
		base = strings.TrimSpace(string(b))
	}
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(base) || base == strings.Repeat("0", 40) {
		return errors.New("publication base is unbound")
	}
	b, err = exec.CommandContext(ctx, "git", "diff", "--name-only", "--no-renames", base, source, "--", "k8s/").Output()
	if err != nil {
		return errors.New("publication change inventory is unavailable")
	}
	for _, path := range strings.Fields(string(b)) {
		if path != "k8s/providers/hetzner/infrastructure/controllers/longhorn/helm-release.yaml" {
			return fmt.Errorf("selector migration must be isolated from other production manifests")
		}
	}
	return nil
}
