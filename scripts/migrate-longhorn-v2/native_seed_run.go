package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"
)

type nativeSeedRun struct {
	c          client
	journalUID string
	record     benchmarkRecord
	state      canarySeedState
	created    map[string]object
	claim      object
	pv         object
	volume     object
}

func runNativeSeed(ctx context.Context, c client, source, run string) error {
	if !c.nativeSeed || c.context != "admin@prod" || authorizeNativeSeed(os.Getenv) != nil || source != os.Getenv("GITHUB_SHA") || run != os.Getenv("GITHUB_RUN_ID") {
		return errSeedEvidence
	}
	p, assets, err := initializeNativeSeed(ctx, c, source, run)
	if err != nil {
		return err
	}
	if err = benchmarkTags(ctx, c, source, benchmarkTag("v1", run), p.record.TagNodes, true); err != nil {
		return err
	}
	for _, desired := range assets {
		actual, err := createBenchmarkScratch(ctx, c, &p.record, desired)
		if err != nil {
			return err
		}
		p.created[text(desired, "kind")] = actual
	}
	if err = p.bind(ctx); err != nil {
		return err
	}
	if err = p.writePayload(ctx); err != nil {
		return err
	}
	if err = p.backup(ctx); err != nil {
		return err
	}
	return p.retireSource(ctx)
}

func initializeNativeSeed(ctx context.Context, c client, source, run string) (*nativeSeedRun, []object, error) {
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return nil, nil, err
	}
	s, err := c.snapshot(ctx)
	if err != nil {
		return nil, nil, err
	}
	machines, err := benchmarkMachines(s, "v1")
	if err != nil {
		return nil, nil, err
	}
	baseline := c
	baseline.nativeSeed = false
	if err = requireBenchmarkBaseline(ctx, baseline, s.nodes); err != nil {
		return nil, nil, err
	}
	budget, err := c.observeV1ScratchHeadroom(ctx)
	if err != nil || budget.write {
		return nil, nil, errors.New("the existing four-GiB source budget is not available; no reservation is changed here")
	}
	runtime, err := captureBenchmarkV1Runtime(ctx, c, slices.Sorted(maps.Keys(machines)))
	if err != nil {
		return nil, nil, err
	}
	location, err := c.get(ctx, "backupstoragelocations.velero.io", "velero", "default", false)
	if err != nil {
		return nil, nil, err
	}
	schedules, err := c.list(ctx, "schedules.velero.io", "velero")
	if err != nil {
		return nil, nil, err
	}
	locationID, locationSpec, err := verifySeedBackupInputs(location, schedules, time.Now())
	if err != nil {
		return nil, nil, err
	}
	old, err := c.get(ctx, "configmap", "longhorn-system", nativeSeedJournal, true)
	if err != nil || old != nil {
		return nil, nil, errors.New("a prior seed journal is present or unreadable; preserve it for explicit recovery")
	}
	namespace, err := observeBenchmarkWorkspace(ctx, c, "")
	if err != nil {
		return nil, nil, err
	}
	for _, kind := range []string{"pods", "persistentvolumeclaims"} {
		items, err := c.list(ctx, kind, benchmarkNamespace)
		if err != nil || len(items) != 0 {
			return nil, nil, errors.New("the dedicated canary workspace is occupied or unreadable")
		}
	}
	assets, err := benchmarkScratchAssets(c, "v1", run, benchmarkNamespace, benchmarkClientNode)
	if err != nil {
		return nil, nil, err
	}
	for _, desired := range assets {
		if text(desired, "kind") == "Pod" {
			if err := verifyBenchmarkClientMemory(s, desired); err != nil {
				return nil, nil, err
			}
		}
		existing, err := c.get(ctx, strings.ToLower(text(desired, "kind")), text(desired, "metadata", "namespace"), text(desired, "metadata", "name"), true)
		if err != nil || existing != nil {
			return nil, nil, errors.New("a prior source asset is present or unreadable; it cannot be adopted")
		}
	}
	backupName := "longhorn-v2-seed-" + run
	old, err = c.get(ctx, "backups.velero.io", "velero", backupName, true)
	if err != nil || old != nil {
		return nil, nil, errors.New("the source backup name is occupied or unreadable")
	}
	tagNodes, err := benchmarkTagNodes(s.storageNodes)
	if err != nil {
		return nil, nil, err
	}
	identity := benchmarkIdentity(namespace)
	p := &nativeSeedRun{c: c, created: map[string]object{}, record: benchmarkRecord{Version: 3, Phase: "v1", Source: source, Run: run, Image: fioImage, Workload: nativeSeedWorkload(), Namespace: benchmarkNamespace, NamespaceUID: identity.UID, ClientNode: benchmarkClientNode,
		Resources: map[string]benchmarkResource{"Namespace": identity}, Machines: machines, TagNodes: tagNodes, V1Runtime: &runtime},
		state: canarySeedState{Version: 1, Source: source, Run: run, Phase: "created", Location: locationID, LocationSpec: locationSpec,
			Backup: benchmarkResource{Name: backupName, Namespace: "velero"}}}
	if err = p.state.validate(p.record); err != nil {
		return nil, nil, err
	}
	record, err := json.Marshal(p.record)
	if err != nil {
		return nil, nil, err
	}
	state, err := json.Marshal(p.state)
	if err != nil {
		return nil, nil, err
	}
	cm, err := c.create(ctx, source, object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{"name": nativeSeedJournal, "namespace": "longhorn-system"},
		"data": object{"record": string(record), "seed": string(state)}})
	if err != nil {
		return nil, nil, err
	}
	p.journalUID = text(cm, "metadata", "uid")
	checked, actual, saved, err := readCanarySeedJournal(ctx, c)
	if err != nil || text(checked, "metadata", "uid") != p.journalUID || saved != p.state || !reflect.DeepEqual(actual, p.record) {
		return nil, nil, errors.New("initial seed ownership was not confirmed; preserve its journal")
	}
	return p, assets, nil
}

func (p *nativeSeedRun) advance(ctx context.Context, after canarySeedState) error {
	if err := saveCanarySeedState(ctx, p.c, p.journalUID, p.state, after); err != nil {
		return err
	}
	p.state = after
	return nil
}
