package main

import (
	"context"
	"errors"
	"sort"
	"time"
)

type commandFunc func(context.Context, string, []string, []byte, map[string]string) ([]byte, error)
type snapshot struct {
	objects            []object
	username, password string
}

func active(ctx context.Context) error {
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return ctx.Err()
}

var publicReads = []struct{ name, path string }{
	{"policy", "/apis/policies.kyverno.io/v1/imagevalidatingpolicies/verify-app-images"},
	{"namespace", "/api/v1/namespaces/default"},
	{"webhook", "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations/kyverno-resource-validating-webhook-cfg"},
	{"exceptions", "/apis/policies.kyverno.io/v1/policyexceptions?limit=1000"},
	{"deployment", "/apis/apps/v1/namespaces/kyverno/deployments/kyverno-admission-controller"},
	{"pdb", "/apis/policy/v1/namespaces/kyverno/poddisruptionbudgets/kyverno-admission-controller"},
	{"replicasets", "/apis/apps/v1/namespaces/kyverno/replicasets?limit=1000"},
	{"pods", "/api/v1/namespaces/kyverno/pods?limit=1000"},
	{"service", "/api/v1/namespaces/kyverno/services/kyverno-svc"},
	{"endpoints", "/apis/discovery.k8s.io/v1/namespaces/kyverno/endpointslices?labelSelector=kubernetes.io%2Fservice-name%3Dkyverno-svc&limit=1000"},
}

func nativeRead(ctx context.Context, run commandFunc, path string) ([]byte, error) {
	if err := active(ctx); err != nil {
		return nil, err
	}
	raw, err := run(ctx, "kubectl", []string{"--context", "admin@prod", "--request-timeout=15s", "get", "--raw", path}, nil, nil)
	if err != nil {
		return nil, errors.New("fixed native admission inventory read failed")
	}
	if err = active(ctx); err != nil {
		return nil, err
	}
	return raw, nil
}

func takeSnapshot(ctx context.Context, run commandFunc, desired []byte) (snapshot, error) {
	raw := map[string][]byte{}
	for _, target := range publicReads {
		value, err := nativeRead(ctx, run, target.path)
		if err != nil {
			return snapshot{}, err
		}
		raw[target.name] = value
	}
	policy, err := qualifyPolicy(raw["policy"], desired)
	if err != nil {
		return snapshot{}, err
	}
	namespace, err := nativeObject(raw["namespace"], "v1", "Namespace", "default", "")
	if err != nil {
		return snapshot{}, err
	}
	if text(namespace, "status", "phase") != "Active" || text(namespace, "metadata", "labels", "kubernetes.io/metadata.name") != "default" {
		return snapshot{}, errors.New("fixed probe namespace is unavailable or unqualified")
	}
	webhook, err := qualifyWebhook(raw["webhook"])
	if err != nil {
		return snapshot{}, err
	}
	exceptions, err := completeList(raw["exceptions"], "policies.kyverno.io/v1", "PolicyExceptionList")
	if err != nil {
		return snapshot{}, err
	}
	for _, exception := range exceptions {
		refs, ok := at(exception, "spec", "policyRefs").([]any)
		if !ok || len(refs) == 0 || text(exception, "metadata", "namespace") == "" {
			return snapshot{}, errors.New("CEL policy exception inventory is unqualified")
		}
		for _, ref := range refs {
			if text(ref, "kind") == "" || text(ref, "name") == "" {
				return snapshot{}, errors.New("CEL policy exception reference is unqualified")
			}
			if text(ref, "kind") == "ImageValidatingPolicy" && text(ref, "name") == "verify-app-images" {
				return snapshot{}, errors.New("an exception can affect the admission probe")
			}
		}
	}
	controllers, err := qualifyControllers(raw["deployment"], raw["pdb"], raw["replicasets"], raw["pods"])
	if err != nil {
		return snapshot{}, err
	}
	service, err := qualifyService(raw["service"], raw["endpoints"], controllers)
	if err != nil {
		return snapshot{}, err
	}
	// Read the one authoritative pull credential only after the public bindings
	// qualify. It and the complete private footprint are never serialized.
	secretRaw, err := nativeRead(ctx, run, "/api/v1/namespaces/kyverno/secrets/ghcr-auth")
	if err != nil {
		return snapshot{}, err
	}
	username, password, err := registryCredential(secretRaw)
	if err != nil {
		return snapshot{}, err
	}
	secret, err := decodeObject(secretRaw)
	if err != nil {
		return snapshot{}, err
	}
	objects := []object{policy, namespace, webhook, secret}
	objects = append(objects, exceptions...)
	objects = append(objects, controllers...)
	objects = append(objects, service...)
	seen := map[string]bool{}
	for _, o := range objects {
		uid := text(o, "metadata", "uid")
		if uid == "" || seen[uid] {
			return snapshot{}, errors.New("admission binding identities collide")
		}
		seen[uid] = true
	}
	sort.Slice(objects, func(i, j int) bool { return text(objects[i], "metadata", "uid") < text(objects[j], "metadata", "uid") })
	if err = active(ctx); err != nil {
		return snapshot{}, err
	}
	return snapshot{objects: objects, username: username, password: password}, nil
}
