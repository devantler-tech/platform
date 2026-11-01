package main

import (
	"context"
	"encoding/json"
	"errors"
)

// journal binds a run to the exact worker it observed.
type journal struct {
	Target      string
	NodeUID     string
	ProviderID  string
	MachineUUID string
}

func reviewedWorkflowPath(path string) bool {
	const workflow = ".github/workflows/prepare-longhorn-v2-node.yaml"
	return path == workflow || path == workflow+"@main" || path == workflow+"@refs/heads/main"
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
