package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
)

type journal struct {
	Schema        int    `json:"schema"`
	Target        string `json:"target"`
	Source        string `json:"source"`
	OwnerRun      string `json:"ownerRun"`
	Phase         string `json:"phase"`
	NodeUID       string `json:"nodeUID"`
	ProviderID    string `json:"providerID"`
	MachineUUID   string `json:"machineUUID"`
	LonghornUID   string `json:"longhornUID"`
	EphemeralUUID string `json:"ephemeralUUID"`
	OriginalDisks object `json:"originalDisks"`
}

func validateJournal(j journal, target string) error {
	if j.Schema != 1 || j.Target != target || !regexp.MustCompile(`^prod-worker-[123]$`).MatchString(target) ||
		!regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(j.Source) || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(j.OwnerRun) ||
		j.NodeUID == "" || !regexp.MustCompile(`^hcloud://[1-9][0-9]*$`).MatchString(j.ProviderID) || j.MachineUUID == "" || j.LonghornUID == "" || j.EphemeralUUID == "" {
		return errors.New("journal does not bind the exact original storage identities")
	}
	switch j.Phase {
	case "evacuating", "draining", "reset-requested", "prepared":
	default:
		return errors.New("journal phase is outside the node-preparation procedure")
	}
	if len(j.OriginalDisks) != 1 {
		return errors.New("node preparation requires exactly the original V1 filesystem disk")
	}
	for _, raw := range j.OriginalDisks {
		d, ok := raw.(map[string]any)
		if !ok || text(d, "path") != "/var/lib/longhorn" || text(d, "diskType") != "filesystem" || value(d, "allowScheduling") != true || value(d, "evictionRequested") != false {
			return errors.New("original disk is outside the reviewed node-reset scope")
		}
	}
	return nil
}

type preparation struct {
	c             client
	target, name  string
	source, owner string
	j             journal
	pools         map[string]migrationPool
}

func (p preparation) readRecord(ctx context.Context) (object, journal, error) {
	cm, err := p.c.get(ctx, "configmap", "longhorn-system", p.name, true)
	if err != nil || cm == nil {
		return cm, journal{}, err
	}
	var j journal
	d := json.NewDecoder(bytes.NewBufferString(text(cm, "data", "record")))
	d.DisallowUnknownFields()
	if d.Decode(&j) != nil {
		return nil, journal{}, errors.New("node-preparation journal cannot be decoded")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, journal{}, errors.New("node-preparation journal is not singular")
	}
	return cm, j, validateJournal(j, p.target)
}

// reviewedWorkflowPath accepts the bare path GitHub returns today and the suffixed
// form its documentation shows, which may only name main.
func reviewedWorkflowPath(path string) bool {
	const workflow = ".github/workflows/prepare-longhorn-v2-node.yaml"
	return path == workflow || path == workflow+"@main" || path == workflow+"@refs/heads/main"
}

func (p *preparation) claimRecord(ctx context.Context, initial journal) error {
	cm, j, err := p.readRecord(ctx)
	if err != nil {
		return err
	}
	if cm == nil {
		if err = validateJournal(initial, p.target); err != nil {
			return err
		}
		if err = sourceCurrent(ctx, p.c.run, p.source); err != nil {
			return err
		}
		b, err := json.Marshal(initial)
		if err != nil {
			return err
		}
		body, err := json.Marshal(object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{"name": p.name, "namespace": "longhorn-system"}, "data": object{"record": string(b)}})
		if err != nil {
			return err
		}
		if _, err = p.c.kubectl(ctx, body, "create", "--filename=/dev/stdin", "--output=name"); err != nil {
			return err
		}
		p.j = initial
		return nil
	}
	if j.OwnerRun != p.owner {
		if err = previousRunCompleted(ctx, p.c, j.OwnerRun, j.Source); err != nil {
			return err
		}
		j.OwnerRun, j.Source = p.owner, p.source
		b, err := json.Marshal(j)
		if err != nil {
			return err
		}
		if err = sourceCurrent(ctx, p.c.run, p.source); err != nil {
			return err
		}
		if err = p.c.patch(ctx, "configmap", "longhorn-system", cm, object{"op": "replace", "path": "/data/record", "value": string(b)}); err != nil {
			return err
		}
	}
	p.j = j
	return nil
}

func (p preparation) checkOwner(ctx context.Context) error {
	_, j, err := p.readRecord(ctx)
	if err != nil {
		return err
	}
	if j.OwnerRun != p.owner || j.Source != p.source {
		return errors.New("node-preparation journal belongs to another workflow")
	}
	return sourceCurrent(ctx, p.c.run, p.source)
}

func (p preparation) mutate(ctx context.Context, kind, namespace string, o object, changes ...object) error {
	if err := p.checkOwner(ctx); err != nil {
		return err
	}
	return p.c.patch(ctx, kind, namespace, o, changes...)
}

func (p *preparation) advance(ctx context.Context, phase string) error {
	cm, j, err := p.readRecord(ctx)
	if err != nil {
		return err
	}
	if j.OwnerRun != p.owner || j.Source != p.source {
		return errors.New("node-preparation journal ownership changed")
	}
	j.Phase = phase
	if err = validateJournal(j, p.target); err != nil {
		return err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err = sourceCurrent(ctx, p.c.run, p.source); err != nil {
		return err
	}
	if err = p.c.patch(ctx, "configmap", "longhorn-system", cm, object{"op": "replace", "path": "/data/record", "value": string(b)}); err != nil {
		return err
	}
	p.j = j
	return nil
}

// previousRunCompleted proves an earlier first-attempt main dispatch of this workflow has finished.
func previousRunCompleted(ctx context.Context, c client, runID, source string) error {
	b, err := c.run(ctx, nil, "gh", "api", "repos/devantler-tech/platform/actions/runs/"+runID+"/attempts/1")
	if err != nil {
		return errors.New("previous workflow attempt cannot be verified")
	}
	var run object
	if json.Unmarshal(b, &run) != nil || text(run, "status") != "completed" || text(run, "head_sha") != source ||
		text(run, "head_branch") != "main" || text(run, "event") != "workflow_dispatch" || value(run, "run_attempt") != float64(1) ||
		!reviewedWorkflowPath(text(run, "path")) {
		return errors.New("previous workflow is live, unknown or outside the reviewed procedure")
	}
	return nil
}
