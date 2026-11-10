package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/devantler-tech/platform/internal/strictjson"
)

type journal struct {
	Schema               int    `json:"schema"`
	Target               string `json:"target"`
	Source               string `json:"source"`
	OwnerRun             string `json:"ownerRun"`
	Phase                string `json:"phase"`
	NodeUID              string `json:"nodeUID"`
	ProviderID           string `json:"providerID"`
	MachineUUID          string `json:"machineUUID"`
	LonghornUID          string `json:"longhornUID"`
	EphemeralUUID        string `json:"ephemeralUUID"`
	OriginalDisks        object `json:"originalDisks"`
	TemporarySelection   string `json:"temporarySelection,omitempty"`
	NativeEmptyDigest    string `json:"nativeEmptyDigest,omitempty"`
	OriginalBootID       string `json:"originalBootID,omitempty"`
	RawVolumeAbsent      bool   `json:"rawVolumeAbsent,omitempty"`
	StagingLayout        bool   `json:"stagingLayout,omitempty"`
	RetainedRawUUID      string `json:"retainedRawUUID,omitempty"`
	RetainedExtentDigest string `json:"retainedExtentDigest,omitempty"`
	PreparedRawUUID      string `json:"preparedRawUUID,omitempty"`
}

// validateJournal requires complete original identities and a known procedure
// phase. Its disjoint schemas keep permanent V1-disk records separate from
// temporary-worker records carrying positive empty-storage evidence.
func validateJournal(j journal, target string) error {
	if j.Target != target ||
		!regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(j.Source) || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(j.OwnerRun) ||
		j.NodeUID == "" || !regexp.MustCompile(`^hcloud://[1-9][0-9]*$`).MatchString(j.ProviderID) || j.MachineUUID == "" || j.LonghornUID == "" || j.EphemeralUUID == "" {
		return errors.New("journal does not bind the exact original storage identities")
	}
	switch j.Phase {
	case "evacuating", "draining", "reset-requested", "prepared":
	default:
		return errors.New("journal phase is outside the node-preparation procedure")
	}
	if j.Schema == 3 {
		return validateRetainedPreparation(j)
	}
	if j.RetainedRawUUID != "" || j.RetainedExtentDigest != "" || j.PreparedRawUUID != "" {
		return errors.New("retained extent proof belongs to a different preparation profile")
	}
	if j.Schema == 2 {
		if !regexp.MustCompile(`^autoscale-longhorn-v1-[0-9a-f]{16}$`).MatchString(target) ||
			(j.TemporarySelection != "temporary-v1-1" && j.TemporarySelection != "temporary-v1-2") ||
			!regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(j.NativeEmptyDigest) || !inventoryBoot.MatchString(j.OriginalBootID) ||
			!j.RawVolumeAbsent || j.OriginalDisks == nil || len(j.OriginalDisks) != 0 {
			return errors.New("temporary preparation lacks its separate empty-storage identity proof")
		}
		return nil
	}
	if j.Schema != 1 || !inventoryTarget.MatchString(target) || j.TemporarySelection != "" || j.NativeEmptyDigest != "" || j.OriginalBootID != "" || j.RawVolumeAbsent || j.StagingLayout {
		return errors.New("journal schema and preparation scope do not match")
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

func validateRetainedPreparation(j journal) error {
	preparedUUID := strings.ToLower(j.PreparedRawUUID)
	if !regexp.MustCompile(`^autoscale-longhorn-v2-[0-9a-f]{16}$`).MatchString(j.Target) ||
		j.TemporarySelection != "temporary-v2-1" || !j.StagingLayout || j.RawVolumeAbsent ||
		j.OriginalDisks == nil || len(j.OriginalDisks) != 0 || !inventoryBoot.MatchString(j.OriginalBootID) ||
		!regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(j.NativeEmptyDigest) ||
		!regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(j.RetainedExtentDigest) ||
		!inventoryBoot.MatchString(strings.ToLower(j.RetainedRawUUID)) ||
		(j.Phase == "prepared" && (!inventoryBoot.MatchString(preparedUUID) || strings.EqualFold(j.RetainedRawUUID, preparedUUID))) ||
		(j.Phase != "prepared" && preparedUUID != "") {
		return errors.New("retained preparation lacks distinct original and staged storage evidence")
	}
	return nil
}

type preparation struct {
	c                  client
	target, name       string
	source, owner      string
	j                  journal
	pools              map[string]migrationPool
	temporarySelection string
}

// validateRecord binds a structurally valid journal to the explicitly selected
// preparation profile and, for temporary workers, its fixed journal name.
func (p preparation) validateRecord(j journal) error {
	if err := validateJournal(j, p.target); err != nil {
		return err
	}
	if p.temporarySelection != j.TemporarySelection || (j.TemporarySelection != "" && p.name != "longhorn-v2-prepare-"+j.TemporarySelection) {
		return errors.New("journal does not match the explicitly selected preparation profile")
	}
	return nil
}

// readRecord decodes exactly one strict journal and validates its original
// identities against the selected preparation profile before it can be used.
func (p preparation) readRecord(ctx context.Context) (object, journal, error) {
	cm, err := p.c.get(ctx, "configmap", "longhorn-system", p.name, true)
	if err != nil || cm == nil {
		return cm, journal{}, err
	}
	j, err := decodePreparationJournal([]byte(text(cm, "data", "record")))
	if err != nil {
		return nil, journal{}, err
	}
	return cm, j, p.validateRecord(j)
}

func decodePreparationJournal(data []byte) (journal, error) {
	fields := map[string][]string{"": {"schema", "target", "source", "ownerRun", "phase", "nodeUID", "providerID", "machineUUID", "longhornUID", "ephemeralUUID", "originalDisks", "temporarySelection", "nativeEmptyDigest", "originalBootID", "rawVolumeAbsent", "stagingLayout", "retainedRawUUID", "retainedExtentDigest", "preparedRawUUID"}}
	refused := errors.New("node-preparation journal cannot be decoded")
	if len(data) == 0 || len(data) > 32<<10 || strictjson.Validate(data, fields) != nil {
		return journal{}, refused
	}
	var j journal
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&j) != nil {
		return journal{}, refused
	}
	return j, nil
}

// reviewedWorkflowPath accepts the bare path GitHub returns today and the suffixed
// form its documentation shows, which may only name main.
func reviewedWorkflowPath(path string) bool {
	const workflow = ".github/workflows/prepare-longhorn-v2-node.yaml"
	return path == workflow || path == workflow+"@main" || path == workflow+"@refs/heads/main"
}

// claimRecord creates a validated journal on current main or adopts a former
// owner only after its reviewed first-attempt workflow is proven terminal.
// Adoption preserves the recorded phase and original storage identities.
func (p *preparation) claimRecord(ctx context.Context, initial journal) error {
	cm, j, err := p.readRecord(ctx)
	if err != nil {
		return err
	}
	if cm == nil {
		if err = p.validateRecord(initial); err != nil {
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
	return p.advanceWithStage(ctx, phase, "")
}

func (p *preparation) advanceWithStage(ctx context.Context, phase, stagedUUID string) error {
	cm, j, err := p.readRecord(ctx)
	if err != nil {
		return err
	}
	if j.OwnerRun != p.owner || j.Source != p.source {
		return errors.New("node-preparation journal ownership changed")
	}
	j.Phase = phase
	j.PreparedRawUUID = stagedUUID
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
