package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
)

type receipt struct {
	Schema                   int    `json:"schema"`
	Source                   string `json:"source"`
	RunID                    string `json:"runID"`
	SignedAdmitted           int    `json:"signedAdmitted"`
	UnsignedDenied           int    `json:"unsignedDenied"`
	StableBindings           bool   `json:"stableBindings"`
	ServerDryRun             bool   `json:"serverDryRun"`
	NegativeCauseObservable  bool   `json:"negativeCauseObservable"`
	PerReplicaAttribution    bool   `json:"perReplicaAttribution"`
	ColdFirstRequestObserved bool   `json:"coldFirstRequestObserved"`
}

func prove(ctx context.Context, run commandFunc, meta runMetadata, desired []byte) (receipt, error) {
	hold := func(stage string) (receipt, error) {
		return receipt{}, fmt.Errorf("admission recovery hold at %s", stage)
	}
	if !sourcePattern.MatchString(meta.source) || !runPattern.MatchString(meta.runID) || !credentialPattern.MatchString(meta.credentialDir) {
		return hold("dispatch")
	}
	pin := func() error {
		if err := active(ctx); err != nil {
			return err
		}
		if _, err := run(ctx, "bash", []string{"./scripts/verify-prod-recovery-source.sh", meta.source}, nil, nil); err != nil {
			return errors.New("current main is unverified")
		}
		return active(ctx)
	}
	unsigned := func(s snapshot) error {
		if err := active(ctx); err != nil {
			return err
		}
		_, err := run(ctx, "bash", []string{"./scripts/verify-image-unsigned.sh", "--image", unsignedImage}, nil, map[string]string{"REGISTRY_USERNAME": s.username, "REGISTRY_PASSWORD": s.password, "TMPDIR": meta.credentialDir})
		if err != nil {
			return errors.New("authoritative readable-and-unsigned precheck is unverified")
		}
		return active(ctx)
	}
	if pin() != nil {
		return hold("source-before")
	}
	before, err := takeSnapshot(ctx, run, desired)
	if err != nil {
		return receipt{}, fmt.Errorf("admission recovery hold at snapshot-before: %w", err)
	}
	if pin() != nil {
		return hold("source-before-registry")
	}
	if unsigned(before) != nil {
		return hold("unsigned-before")
	}
	for _, phase := range []string{"signed-before", "unsigned", "signed-after"} {
		if pin() != nil {
			return hold("source-before-" + phase)
		}
		body, name, err := controlPod(meta.runID, phase)
		if err != nil {
			return hold(phase)
		}
		response, runErr := run(ctx, "kubectl", []string{"--context", "admin@prod", "--request-timeout=45s", "create", "--dry-run=server", "--validate=strict", "--namespace", "default", "--filename", "-", "-o", "json"}, body, nil)
		if active(ctx) != nil {
			return hold("deadline")
		}
		if phase == "unsigned" {
			if !policyDenied(response, runErr) {
				return hold("unsigned-policy-outcome")
			}
		} else if runErr != nil || !admitted(response, name, signedImage) {
			return hold(phase)
		}
	}
	if pin() != nil {
		return hold("source-before-final-snapshot")
	}
	after, err := takeSnapshot(ctx, run, desired)
	if err != nil {
		return receipt{}, fmt.Errorf("admission recovery hold at snapshot-after: %w", err)
	}
	if !reflect.DeepEqual(before, after) {
		return hold("binding-changed")
	}
	if pin() != nil {
		return hold("source-before-final-registry")
	}
	if unsigned(after) != nil {
		return hold("unsigned-after")
	}
	if pin() != nil {
		return hold("source-after")
	}
	if active(ctx) != nil {
		return hold("deadline")
	}
	return receipt{Schema: 1, Source: meta.source, RunID: meta.runID, SignedAdmitted: 2, UnsignedDenied: 1, StableBindings: true, ServerDryRun: true}, nil
}
