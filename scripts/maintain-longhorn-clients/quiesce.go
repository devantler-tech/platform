package main

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"time"
)

const quiesceKey = "platform.devantler.tech/longhorn-client-quiesce"

func (e engine) provePaused(ctx context.Context, resources []object, holder string) error {
	for _, old := range resources {
		current, err := e.c.read(ctx, resourceKind(old), str(old, "metadata", "namespace"), str(old, "metadata", "name"))
		if err != nil {
			return err
		}
		if _, err = restorePatch(old, current, holder, str(old, "kind") == "Deployment"); err != nil {
			return err
		}
	}
	return nil
}

func (e engine) backupsIdle(ctx context.Context) (bool, error) {
	backups, err := e.c.list(ctx, "backups.postgresql.cnpg.io", "*")
	if err != nil {
		return false, err
	}
	for _, b := range backups {
		switch str(b, "status", "phase") {
		case "completed", "failed":
		default:
			return false, nil
		}
	}
	velero, err := e.c.list(ctx, "backups.velero.io", "velero")
	if err != nil {
		return false, err
	}
	for _, b := range velero {
		switch str(b, "status", "phase") {
		case "Completed", "Failed", "PartiallyFailed", "FailedValidation":
		default:
			return false, nil
		}
	}
	return true, nil
}

func (e engine) scheduleIdentities(ctx context.Context, before []object) error {
	cnpg, err := e.c.list(ctx, "scheduledbackups.postgresql.cnpg.io", "*")
	if err != nil {
		return err
	}
	velero, err := e.c.list(ctx, "schedules.velero.io", "velero")
	if err != nil {
		return err
	}
	current := append(cnpg, velero...)
	byKey, err := index(current)
	if err != nil || len(before) != len(current) {
		return errors.New("scheduled backup inventory changed during maintenance")
	}
	for _, old := range before {
		o := byKey[key(old)]
		if o == nil || str(o, "metadata", "uid") != str(old, "metadata", "uid") {
			return errors.New("scheduled backup identity changed during maintenance")
		}
	}
	return nil
}

// A controller's old Pods, including terminating ones, must be absent. Merely
// observing the new ReplicaSet Ready leaves old executions inside their grace
// period able to write. Match the complete Deployment -> ReplicaSet -> Pod chain.
func (e engine) controllerPods(ctx context.Context, d object) ([]object, error) {
	rss, err := e.c.list(ctx, "replicasets", "flux-system")
	if err != nil {
		return nil, err
	}
	pods, err := e.c.list(ctx, "pods", "flux-system")
	if err != nil {
		return nil, err
	}
	var owned []object
	for _, p := range pods {
		if str(p, "metadata", "labels", "app") != str(d, "metadata", "name") {
			continue
		}
		matched := false
		for _, rs := range rss {
			if meta(rs) && owner(rs, "Deployment", str(d, "metadata", "uid")) && owner(p, "ReplicaSet", str(rs, "metadata", "uid")) {
				matched = true
			}
		}
		if !matched || str(p, "metadata", "uid") == "" {
			return nil, errors.New("flux controller process ownership is unproven")
		}
		owned = append(owned, p)
	}
	return owned, nil
}

func (e engine) quiesceReconcilers(ctx context.Context, paused []object) error {
	for _, name := range []string{"kustomize-controller", "helm-controller"} {
		if err := e.provePaused(ctx, paused, e.holder); err != nil {
			return err
		}
		d, err := e.c.read(ctx, "deployment", "flux-system", name)
		if err != nil {
			return err
		}
		if !meta(d) || num(d, "spec", "replicas") < 1 || num(d, "metadata", "generation") < 1 || num(d, "status", "observedGeneration") != num(d, "metadata", "generation") || num(d, "status", "readyReplicas") != num(d, "spec", "replicas") {
			return errors.New("flux controller baseline is not ready")
		}
		before, err := e.controllerPods(ctx, d)
		if err != nil {
			return err
		}
		if len(before) != num(d, "spec", "replicas") {
			return errors.New("flux controller baseline process coverage is incomplete")
		}
		oldUIDs := map[string]bool{}
		for _, p := range before {
			if !meta(p) || !condition(p, "Ready") {
				return errors.New("flux controller baseline process is not ready")
			}
			oldUIDs[str(p, "metadata", "uid")] = true
		}
		patch := tests(d)
		restartToken := e.holder + ":" + e.now().UTC().Format(time.RFC3339Nano)
		if val(d, "spec", "template", "metadata", "annotations") == nil {
			patch = append(patch, object{"op": "add", "path": "/spec/template/metadata/annotations", "value": object{}})
		}
		patch = append(patch, object{"op": "add", "path": "/spec/template/metadata/annotations/" + escape(quiesceKey), "value": restartToken})
		spec, _ := val(d, "spec").(object)
		template, _ := val(d, "spec", "template").(object)
		templateMetadata, _ := val(d, "spec", "template", "metadata").(object)
		if spec == nil || template == nil || templateMetadata == nil {
			return errors.New("flux controller restart specification is malformed")
		}
		expected := maps.Clone(spec)
		expectedTemplate := maps.Clone(template)
		expectedMetadata := maps.Clone(templateMetadata)
		annotations, _ := templateMetadata["annotations"].(object)
		annotations = maps.Clone(annotations)
		if annotations == nil {
			annotations = object{}
		}
		annotations[quiesceKey] = restartToken
		expectedMetadata["annotations"] = annotations
		expectedTemplate["metadata"] = expectedMetadata
		expected["template"] = expectedTemplate
		// Separate field ownership prevents Flux Operator's kubectl-manager
		// adoption from reverting the process-retirement restart annotation.
		if err = e.c.patch(ctx, "deployment", d, patch); err != nil {
			return err
		}
		if err = e.poll(ctx, "pre-pause Flux process retirement", func(c context.Context) (bool, error) {
			if err := e.provePaused(c, paused, e.holder); err != nil {
				return false, err
			}
			current, err := e.c.read(c, "deployment", "flux-system", name)
			if err != nil {
				return false, err
			}
			if !meta(current) || str(current, "metadata", "uid") != str(d, "metadata", "uid") || !reflect.DeepEqual(val(current, "spec"), expected) {
				return false, errors.New("flux controller restart ownership changed")
			}
			pods, err := e.controllerPods(c, current)
			if err != nil {
				return false, err
			}
			for _, p := range pods {
				if oldUIDs[str(p, "metadata", "uid")] || !meta(p) || !condition(p, "Ready") || str(p, "metadata", "annotations", quiesceKey) != restartToken {
					return false, nil
				}
			}
			return len(pods) == num(current, "spec", "replicas") && num(current, "status", "observedGeneration") == num(current, "metadata", "generation") && num(current, "status", "updatedReplicas") == num(current, "spec", "replicas") && num(current, "status", "readyReplicas") == num(current, "spec", "replicas"), nil
		}); err != nil {
			return err
		}
	}
	return e.provePaused(ctx, paused, e.holder)
}
