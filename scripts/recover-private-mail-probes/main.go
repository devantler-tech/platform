package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	setKey    = "set"
	podKey    = "pod"
	claimKey  = "claim"
	sourceKey = "source"
)

var errNotEligible = errors.New("not the obsolete mail probe transition")
var digestPattern = regexp.MustCompile("^sha256:[0-9a-f]{64}$")
var imagePattern = regexp.MustCompile("^ghcr.io/devantler-tech/platform-private-mail-bootstrap@sha256:[0-9a-f]{64}$")

type owner struct {
	Kind, Name, UID string
	Controller      bool
}
type metadata struct {
	Name, Namespace, UID, ResourceVersion string
	Generation                            int64
	DeletionTimestamp                     *time.Time
	Labels                                map[string]string
	OwnerReferences                       []owner
}
type condition struct {
	Type, Status       string
	ObservedGeneration int64
	LastTransitionTime time.Time
}
type httpCheck struct {
	Host, Path, Scheme string
	Port               any
}
type probe struct {
	Exec                                            *struct{ Command []string }
	HTTPGet                                         *httpCheck
	TimeoutSeconds, PeriodSeconds, FailureThreshold int
}
type container struct {
	Name, Image                                 string
	StartupProbe, ReadinessProbe, LivenessProbe *probe
	Ports                                       []struct {
		Name          string
		ContainerPort int
	}
}
type claimReference struct{ ClaimName string }
type volume struct {
	Name                  string
	PersistentVolumeClaim *claimReference
}
type podSpec struct {
	Containers []container
	Volumes    []volume
}
type statefulSet struct {
	Kind     string
	Metadata metadata
	Spec     struct {
		Replicas            *int32
		PodManagementPolicy string
		UpdateStrategy      struct {
			Type          string
			RollingUpdate *struct{ Partition int32 }
		}
		VolumeClaimTemplates                 []json.RawMessage
		Template                             struct{ Spec podSpec }
		PersistentVolumeClaimRetentionPolicy struct{ WhenDeleted, WhenScaled string }
	}
	Status struct {
		ObservedGeneration                       int64
		Replicas, ReadyReplicas, UpdatedReplicas int32
		CurrentRevision, UpdateRevision          string
	}
}
type containerStatus struct {
	Name         string
	Ready        bool
	RestartCount int32
}
type mailPod struct {
	Metadata metadata
	Spec     podSpec
	Status   struct {
		Conditions        []condition
		ContainerStatuses []containerStatus
	}
}
type persistentClaim struct {
	Metadata metadata
	Spec     struct{ VolumeName string }
	Status   struct{ Phase string }
}
type artifactSource struct {
	Metadata metadata
	Status   struct {
		Artifact   struct{ Revision string }
		Conditions []condition
	}
}
type checks struct{ Startup, Ready, Live *probe }

func loadChecks(b []byte) (checks, error) {
	var err error
	var raw map[string]any
	if err = yaml.Unmarshal(b, &raw); err != nil {
		return checks{}, errors.New("invalid reviewed probe definition")
	}
	b, err = json.Marshal(raw)
	if err != nil {
		return checks{}, err
	}
	var set statefulSet
	if err = json.Unmarshal(b, &set); err != nil {
		return checks{}, err
	}
	if set.Kind != "StatefulSet" || set.Metadata.Name != "private-mail" {
		return checks{}, errNotEligible
	}
	c, ok := serving(set.Spec.Template.Spec)
	if !ok {
		return checks{}, errNotEligible
	}
	out := checks{c.StartupProbe, c.ReadinessProbe, c.LivenessProbe}
	for _, p := range []*probe{out.Startup, out.Ready, out.Live} {
		if p == nil || p.Exec == nil || p.HTTPGet != nil || p.TimeoutSeconds != 2 || len(p.Exec.Command) < 3 || p.Exec.Command[0] != "/usr/bin/env" || p.Exec.Command[1] != "-i" {
			return checks{}, errNotEligible
		}
	}
	if out.Startup.PeriodSeconds != 5 || out.Startup.FailureThreshold != 24 {
		return checks{}, errNotEligible
	}
	return out, nil
}

func serving(spec podSpec) (container, bool) {
	var result container
	count := 0
	for _, c := range spec.Containers {
		if c.Name == "private-mail" {
			result = c
			count++
		}
	}
	return result, count == 1 && len(spec.Containers) == 1
}
func usesClaim(spec podSpec) bool {
	count := 0
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			if v.Name != "data" || v.PersistentVolumeClaim.ClaimName != "private-mail-data" {
				return false
			}
			count++
		}
	}
	return count == 1
}
func expectedProbes(c container, want checks) bool {
	pairs := [][2]*probe{{c.StartupProbe, want.Startup}, {c.ReadinessProbe, want.Ready}, {c.LivenessProbe, want.Live}}
	for _, pair := range pairs {
		actual, expected := pair[0], pair[1]
		if actual == nil || expected == nil || actual.Exec == nil || expected.Exec == nil || actual.HTTPGet != nil || actual.TimeoutSeconds != expected.TimeoutSeconds || !reflect.DeepEqual(actual.Exec.Command, expected.Exec.Command) {
			return false
		}
	}
	return true
}
func obsoleteProbes(c container) bool {
	for i, p := range []*probe{c.StartupProbe, c.ReadinessProbe, c.LivenessProbe} {
		path := "/healthz/live"
		if i == 1 {
			path = "/healthz/ready"
		}
		if p == nil || p.Exec != nil || p.HTTPGet == nil || p.HTTPGet.Host != "" || p.HTTPGet.Path != path || p.HTTPGet.Port != "management" || p.HTTPGet.Scheme != "HTTP" || p.TimeoutSeconds != 1 {
			return false
		}
	}
	for _, p := range c.Ports {
		if p.Name == "management" && p.ContainerPort == 8080 {
			return true
		}
	}
	return false
}

type clusterClient interface {
	read(context.Context, string, any) (bool, error)
	deletePod(context.Context, string, string) error
}
type snapshot struct {
	Set    statefulSet
	Pod    mailPod
	Claim  persistentClaim
	Source artifactSource
}

func readSnapshot(ctx context.Context, c clusterClient) (snapshot, error) {
	s := snapshot{}
	for _, item := range []struct {
		key string
		out any
	}{{setKey, &s.Set}, {podKey, &s.Pod}, {claimKey, &s.Claim}, {sourceKey, &s.Source}} {
		found, err := c.read(ctx, item.key, item.out)
		if err != nil {
			return s, errors.New("recovery observation unavailable")
		}
		if !found {
			return s, errNotEligible
		}
	}
	return s, nil
}
func validSource(source artifactSource, digest string) bool {
	if !digestPattern.MatchString(digest) || source.Metadata.Name != "flux-system" || source.Metadata.Namespace != "flux-system" || source.Metadata.UID == "" || source.Metadata.Generation < 1 || source.Metadata.DeletionTimestamp != nil || source.Status.Artifact.Revision != "latest@"+digest {
		return false
	}
	for _, kind := range []string{"Ready", "SourceVerified"} {
		count := 0
		for _, c := range source.Status.Conditions {
			if c.Type == kind && c.Status == "True" && c.ObservedGeneration == source.Metadata.Generation {
				count++
			}
		}
		if count != 1 {
			return false
		}
	}
	return true
}
func ownedPod(p mailPod, set statefulSet) bool {
	if p.Metadata.Name != "private-mail-0" || p.Metadata.Namespace != "private-mail" || p.Metadata.UID == "" || p.Metadata.ResourceVersion == "" || p.Metadata.DeletionTimestamp != nil {
		return false
	}
	owners := 0
	for _, o := range p.Metadata.OwnerReferences {
		if o.Controller {
			if o.Kind != "StatefulSet" || o.Name != "private-mail" || o.UID != set.Metadata.UID {
				return false
			}
			owners++
		}
	}
	return owners == 1 && usesClaim(p.Spec)
}
func validClaim(c persistentClaim) bool {
	if c.Metadata.Name != "private-mail-data" || c.Metadata.Namespace != "private-mail" || c.Metadata.UID == "" || c.Metadata.ResourceVersion == "" || c.Metadata.DeletionTimestamp != nil || c.Spec.VolumeName == "" || c.Status.Phase != "Bound" {
		return false
	}
	for _, o := range c.Metadata.OwnerReferences {
		if o.Kind == "Pod" {
			return false
		}
	}
	return true
}
func eligible(s snapshot, want checks, digest string, now time.Time) bool {
	set, p := s.Set, s.Pod
	if !validSource(s.Source, digest) || !validClaim(s.Claim) || set.Metadata.Name != "private-mail" || set.Metadata.Namespace != "private-mail" || set.Metadata.UID == "" || set.Metadata.ResourceVersion == "" || set.Metadata.DeletionTimestamp != nil || set.Metadata.Generation < 1 || set.Status.ObservedGeneration != set.Metadata.Generation {
		return false
	}
	if set.Spec.Replicas == nil || *set.Spec.Replicas != 1 || set.Status.Replicas != 1 || set.Status.ReadyReplicas != 0 || set.Status.UpdatedReplicas != 0 || set.Status.CurrentRevision == "" || set.Status.UpdateRevision == "" || set.Status.CurrentRevision == set.Status.UpdateRevision {
		return false
	}
	if set.Spec.PodManagementPolicy != "OrderedReady" || set.Spec.UpdateStrategy.Type != "RollingUpdate" || set.Spec.UpdateStrategy.RollingUpdate == nil || set.Spec.UpdateStrategy.RollingUpdate.Partition != 0 || len(set.Spec.VolumeClaimTemplates) != 0 {
		return false
	}
	retention := set.Spec.PersistentVolumeClaimRetentionPolicy
	if retention.WhenDeleted != "Retain" || retention.WhenScaled != "Retain" || !usesClaim(set.Spec.Template.Spec) || !ownedPod(p, set) || p.Metadata.Labels["controller-revision-hash"] != set.Status.CurrentRevision {
		return false
	}
	desired, ok := serving(set.Spec.Template.Spec)
	if !ok || !imagePattern.MatchString(desired.Image) || !expectedProbes(desired, want) {
		return false
	}
	old, ok := serving(p.Spec)
	if !ok || old.Image != desired.Image || !obsoleteProbes(old) {
		return false
	}
	unready := false
	for _, condition := range p.Status.Conditions {
		if condition.Type == "Ready" {
			age := now.Sub(condition.LastTransitionTime)
			unready = condition.Status == "False" && !condition.LastTransitionTime.IsZero() && age >= 5*time.Minute
		}
	}
	restarted := false
	for _, status := range p.Status.ContainerStatuses {
		if status.Name == "private-mail" {
			restarted = !status.Ready && status.RestartCount >= 3
		}
	}
	return unready && restarted
}
func recoverPod(ctx context.Context, c clusterClient, want checks, digest string, now func() time.Time) error {
	before, err := readSnapshot(ctx, c)
	if err != nil {
		return err
	}
	if !eligible(before, want, digest, now()) {
		return errNotEligible
	}
	// Rebind every observation immediately before the one conditional deletion.
	current, err := readSnapshot(ctx, c)
	if err != nil {
		return err
	}
	if !eligible(current, want, digest, now()) || current.Set.Metadata.UID != before.Set.Metadata.UID || current.Set.Metadata.Generation != before.Set.Metadata.Generation || current.Set.Status.UpdateRevision != before.Set.Status.UpdateRevision || current.Pod.Metadata.UID != before.Pod.Metadata.UID || current.Pod.Metadata.ResourceVersion != before.Pod.Metadata.ResourceVersion || current.Claim.Metadata.UID != before.Claim.Metadata.UID || current.Claim.Spec.VolumeName != before.Claim.Spec.VolumeName || current.Source.Metadata.UID != before.Source.Metadata.UID {
		return errors.New("recovery observations changed")
	}
	if err = c.deletePod(ctx, current.Pod.Metadata.UID, current.Pod.Metadata.ResourceVersion); err != nil {
		return errors.New("conditional replacement refused")
	}
	expectedContainer, _ := serving(current.Set.Spec.Template.Spec)
	for {
		var pod mailPod
		found, err := c.read(ctx, podKey, &pod)
		if err != nil {
			return errors.New("replacement observation unavailable")
		}
		if found && pod.Metadata.UID != current.Pod.Metadata.UID {
			var set statefulSet
			var claim persistentClaim
			var source artifactSource
			for _, item := range []struct {
				key string
				out any
			}{{setKey, &set}, {claimKey, &claim}, {sourceKey, &source}} {
				exists, err := c.read(ctx, item.key, item.out)
				if err != nil || !exists {
					return errors.New("replacement verification unavailable")
				}
			}
			if !validSource(source, digest) || source.Metadata.UID != current.Source.Metadata.UID || set.Metadata.UID != current.Set.Metadata.UID || set.Metadata.Generation != current.Set.Metadata.Generation || set.Status.UpdateRevision != current.Set.Status.UpdateRevision || !validClaim(claim) || claim.Metadata.UID != current.Claim.Metadata.UID || claim.Spec.VolumeName != current.Claim.Spec.VolumeName {
				return errors.New("source, workload or retained claim changed")
			}
			servingContainer, ok := serving(pod.Spec)
			if !ownedPod(pod, set) || pod.Metadata.Labels["controller-revision-hash"] != current.Set.Status.UpdateRevision || !ok || servingContainer.Image != expectedContainer.Image || !expectedProbes(servingContainer, want) {
				return errors.New("replacement does not match the reviewed transition")
			}
			ready := false
			for _, condition := range pod.Status.Conditions {
				if condition.Type == "Ready" {
					ready = condition.Status == "True"
				}
			}
			containerReady := false
			for _, status := range pod.Status.ContainerStatuses {
				if status.Name == "private-mail" {
					containerReady = status.Ready
				}
			}
			if ready && containerReady {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("replacement readiness deadline exceeded")
		case <-time.After(time.Second):
		}
	}
}

type commandClient struct{ kubeconfig string }
type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 1<<20 {
		return 0, errors.New("Kubernetes response too large")
	}
	return b.Buffer.Write(p)
}
func (c commandClient) run(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	prefix := []string{"--context", "admin@prod", "--request-timeout=20s"}
	if c.kubeconfig != "" {
		prefix = append(prefix, "--kubeconfig", c.kubeconfig)
	}
	cmd := exec.CommandContext(requestCtx, "kubectl", append(prefix, args...)...)
	cmd.Stdin = bytes.NewReader(input)
	var out boundedOutput
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, errors.New("bounded Kubernetes request failed")
	}
	return out.Bytes(), nil
}
func (c commandClient) read(ctx context.Context, key string, out any) (bool, error) {
	targets := map[string][]string{
		setKey:    {"statefulset", "private-mail", "-n", "private-mail"},
		podKey:    {"pod", "private-mail-0", "-n", "private-mail"},
		claimKey:  {"pvc", "private-mail-data", "-n", "private-mail"},
		sourceKey: {"ocirepository.source.toolkit.fluxcd.io", "flux-system", "-n", "flux-system"},
	}
	target, ok := targets[key]
	if !ok {
		return false, errors.New("unknown fixed observation")
	}
	args := append([]string{"get"}, target...)
	args = append(args, "--ignore-not-found", "-o", "json")
	b, err := c.run(ctx, nil, args...)
	if err != nil {
		return false, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return false, nil
	}
	if err = json.Unmarshal(b, out); err != nil {
		return false, errors.New("malformed Kubernetes observation")
	}
	return true, nil
}
func (c commandClient) deletePod(ctx context.Context, uid, version string) error {
	if uid == "" || version == "" {
		return errors.New("missing deletion preconditions")
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "DeleteOptions",
		"preconditions": map[string]string{"uid": uid, "resourceVersion": version},
	})
	if err != nil {
		return err
	}
	// Core Kubernetes performs this one DELETE atomically against both identities.
	// Graceful termination and normal garbage-collection defaults are retained.
	_, err = c.run(ctx, body, "delete", "--raw=/api/v1/namespaces/private-mail/pods/private-mail-0", "-f", "-")
	return err
}
func main() {
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("GITHUB_REPOSITORY") != "devantler-tech/platform" || os.Getenv("GITHUB_EVENT_NAME") != "merge_group" || os.Getenv("GITHUB_WORKFLOW") != "CI" || os.Getenv("GITHUB_RUN_ATTEMPT") != "1" || !strings.HasPrefix(os.Getenv("GITHUB_REF"), "refs/heads/gh-readonly-queue/main/pr-") {
		fmt.Println("MAIL_PROBE_ROLLOUT=UNVERIFIED")
		os.Exit(2)
	}
	digest := os.Getenv("MAIL_PROBE_ROLLOUT_DIGEST")
	if !digestPattern.MatchString(digest) {
		fmt.Println("MAIL_PROBE_ROLLOUT=UNVERIFIED")
		os.Exit(2)
	}
	definition, err := os.ReadFile("k8s/bases/apps/private-mail-administration/patches/serve-after-initialization.yaml")
	if err != nil {
		fmt.Println("MAIL_PROBE_ROLLOUT=UNVERIFIED")
		os.Exit(2)
	}
	want, err := loadChecks(definition)
	if errors.Is(err, errNotEligible) {
		fmt.Println("MAIL_PROBE_ROLLOUT=NOT_ELIGIBLE")
		os.Exit(10)
	}
	if err != nil {
		fmt.Println("MAIL_PROBE_ROLLOUT=UNVERIFIED")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	err = recoverPod(ctx, commandClient{}, want, digest, time.Now)
	if errors.Is(err, errNotEligible) {
		fmt.Println("MAIL_PROBE_ROLLOUT=NOT_ELIGIBLE")
		os.Exit(10)
	}
	if err != nil {
		fmt.Println("MAIL_PROBE_ROLLOUT=UNVERIFIED")
		os.Exit(2)
	}
	fmt.Println("MAIL_PROBE_ROLLOUT=RECOVERED")
}
