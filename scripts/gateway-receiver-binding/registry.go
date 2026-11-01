package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const receiverSourceDir = "k8s/providers/hetzner/infrastructure/cilium/gateway-receiver"

const (
	restoreAttempts  = 8
	restorePollEvery = 5 * time.Second
)

type admissionResource struct{ Kind, Resource, File string }

var preparedPolicies = []admissionResource{
	{"MutatingAdmissionPolicy", "mutatingadmissionpolicies", "mutating-admission-policy-gateway-receiver.yaml"},
	{"ValidatingAdmissionPolicy", "validatingadmissionpolicies", "validating-admission-policy-gateway-receiver.yaml"},
}

// Validation is removed first so it cannot reject restoration of the old state.
var preparedBindings = []admissionResource{
	{"ValidatingAdmissionPolicyBinding", "validatingadmissionpolicybindings", "components/physical-peer-fence/validating-admission-policy-binding-gateway-receiver.yaml"},
	{"MutatingAdmissionPolicyBinding", "mutatingadmissionpolicybindings", "components/physical-peer-fence/mutating-admission-policy-binding-gateway-receiver.yaml"},
}

type admissionObject struct {
	APIVersion, Kind string
	Metadata         struct {
		metadata
		DeletionTimestamp *string
	}
	Spec   map[string]any
	Status struct {
		ObservedGeneration int64
		TypeChecking       struct{ ExpressionWarnings []any }
	}
}

func admissionMatches(raw []byte, r admissionResource, b binding) bool {
	var actual admissionObject
	if strictJSON(raw, &actual) != nil || actual.APIVersion != "admissionregistration.k8s.io/v1" || actual.Kind != r.Kind || actual.Metadata.Name != "gateway-receiver" || actual.Metadata.Namespace != "" || !uuid.MatchString(actual.Metadata.UID) || actual.Metadata.ResourceVersion == "" || actual.Metadata.DeletionTimestamp != nil || actual.Metadata.Annotations[receiverOwner] != "v1" {
		return false
	}
	source, err := os.ReadFile(filepath.Join(receiverSourceDir, r.File))
	if err != nil {
		return false
	}
	var expected struct{ Spec map[string]any }
	if yaml.Unmarshal([]byte(strings.ReplaceAll(string(source), "${gateway_receiver_source_cidr}", b.CIDR)), &expected) != nil || !reflect.DeepEqual(actual.Spec, expected.Spec) {
		return false
	}
	if r.Kind == "ValidatingAdmissionPolicy" && (actual.Metadata.Generation < 1 || actual.Status.ObservedGeneration != actual.Metadata.Generation || len(actual.Status.TypeChecking.ExpressionWarnings) != 0) {
		return false
	}
	return true
}

type receiverCommand func(context.Context, []byte, ...string) ([]byte, error)

func executeReceiver(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	if err != nil || len(out) > 512<<10 {
		return nil, errors.New("receiver-command")
	}
	return out, nil
}

type receiverClient struct {
	context string
	command receiverCommand
	pause   func(context.Context, time.Duration) error
}

func (c receiverClient) wait(ctx context.Context, d time.Duration) error {
	if c.pause != nil {
		return c.pause(ctx, d)
	}
	return pauseFor(ctx, d)
}

func (c receiverClient) read(ctx context.Context, resource, namespace, name string) ([]byte, error) {
	args := []string{"--context=" + c.context, "--request-timeout=20s"}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	args = append(args, "get", resource, name, "--ignore-not-found=true", "-o=json")
	out, err := c.command(ctx, nil, args...)
	if err != nil {
		return nil, errors.New("receiver-read")
	}
	return out, nil
}
func (c receiverClient) policies(ctx context.Context, b binding) error {
	for _, r := range preparedPolicies {
		raw, err := c.read(ctx, r.Resource, "", "gateway-receiver")
		if err != nil || !admissionMatches(raw, r, b) {
			return errors.New("source-admission-policy")
		}
	}
	return nil
}
func (c receiverClient) bindingsAbsent(ctx context.Context) error {
	for _, r := range preparedBindings {
		raw, err := c.read(ctx, r.Resource, "", "gateway-receiver")
		if err != nil || len(raw) != 0 {
			return errors.New("unexpected-policy-binding")
		}
	}
	return nil
}
func (c receiverClient) removeBindings(ctx context.Context, b binding) error {
	// Inspect the whole set before the first mutation. A foreign second binding
	// must never leave the first binding partially removed.
	type removal struct {
		resource admissionResource
		object   admissionObject
	}
	var removals []removal
	for _, r := range preparedBindings {
		raw, err := c.read(ctx, r.Resource, "", "gateway-receiver")
		if err != nil {
			return err
		}
		if len(raw) == 0 {
			continue
		}
		if !admissionMatches(raw, r, b) {
			return errors.New("foreign-policy-binding")
		}
		var before admissionObject
		if strictJSON(raw, &before) != nil {
			return errors.New("binding-input")
		}
		removals = append(removals, removal{r, before})
	}
	for _, planned := range removals {
		r, before := planned.resource, planned.object
		options, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]string{"uid": before.Metadata.UID, "resourceVersion": before.Metadata.ResourceVersion}})
		_, _ = c.command(ctx, options, "--context="+c.context, "--request-timeout=20s", "delete", "--raw", "/apis/admissionregistration.k8s.io/v1/"+r.Resource+"/gateway-receiver", "-f", "-")
		after, err := c.read(ctx, r.Resource, "", "gateway-receiver")
		if err != nil || len(after) != 0 {
			return errors.New("binding-removal-unverified")
		}
	}
	return nil
}
func (c receiverClient) restoreConfig(ctx context.Context, b binding) error {
	uid := ""
	// An API server still holding a removed binding reapplies the fence, so a
	// patch can have no effect for a moment after the bindings are gone.
	for attempt := 0; attempt < restoreAttempts; attempt++ {
		raw, err := c.read(ctx, "ciliumenvoyconfig", "kube-system", "cilium-gateway-platform")
		if err != nil {
			return err
		}
		patch, err := recoveryPatch(raw, b)
		if err != nil {
			return err
		}
		if len(patch) == 0 {
			return nil
		}
		var observed receiverConfig
		if strictJSON(raw, &observed) != nil || (uid != "" && observed.Metadata.UID != uid) {
			return errors.New("receiver-replacement")
		}
		uid = observed.Metadata.UID
		_, _ = c.command(ctx, patch, "--context="+c.context, "--request-timeout=20s", "-n", "kube-system", "patch", "ciliumenvoyconfig", "cilium-gateway-platform", "--type=json", "--patch-file=/dev/stdin", "-o=json")
		after, err := c.read(ctx, "ciliumenvoyconfig", "kube-system", "cilium-gateway-platform")
		if err != nil {
			return err
		}
		var newObject receiverConfig
		if strictJSON(after, &newObject) != nil || newObject.Metadata.UID != uid {
			return errors.New("receiver-replacement")
		}
		remaining, err := recoveryPatch(after, b)
		if err != nil {
			return errors.New("receiver-restoration-unverified")
		}
		if len(remaining) == 0 {
			return nil
		}
		if c.wait(ctx, restorePollEvery) != nil {
			break
		}
	}
	return errors.New("receiver-restoration-unverified")
}
func (c receiverClient) restoreReceiver(ctx context.Context, b binding) error {
	// An active PROXY listener must retain its physical-peer fence. Inspect
	// the receiver before removing either binding, then inspect it again at
	// the conditional patch to reject a concurrent configuration change.
	raw, err := c.read(ctx, "ciliumenvoyconfig", "kube-system", "cilium-gateway-platform")
	if err != nil {
		return err
	}
	if _, err = recoveryPatch(raw, b); err != nil {
		return err
	}
	if err := c.removeBindings(ctx, b); err != nil {
		return err
	}
	if err := c.restoreConfig(ctx, b); err != nil {
		return err
	}
	if err := c.bindingsAbsent(ctx); err != nil {
		return err
	}
	// Checked after the removal: the fence may be what stops Flux from delivering
	// the policies. The deployment still fails, with public traffic restored.
	return c.policies(ctx, b)
}
func restoreDisabled(ctx context.Context, kubeContext string) error {
	if kubeContext != "admin@prod" {
		return errors.New("arguments")
	}
	before, err := collect(ctx, kubeContext)
	if err != nil {
		return err
	}
	c := receiverClient{context: kubeContext, command: executeReceiver}
	if err = c.restoreReceiver(ctx, before); err != nil && !isPolicyDrift(err) {
		return err
	}
	restored := err
	// A stale binding is reported after the removal for the same reason.
	secret, err := readNamespace(ctx, kubeContext, "flux-system", "secret", "variables-gateway-receiver")
	if err != nil || !secretMatches(secret, before) {
		return errors.New("source-secret")
	}
	if restored != nil {
		return restored
	}
	after, err := collect(ctx, kubeContext)
	if err != nil || before != after {
		return errors.New("source-changed")
	}
	return c.bindingsAbsent(ctx)
}

func isPolicyDrift(err error) bool {
	return err != nil && err.Error() == "source-admission-policy"
}
