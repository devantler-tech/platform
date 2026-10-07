package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

const ksailRunnerRepository int64 = 737584922
const ksailRunnerWorkflow = "devantler-tech/ksail/.github/workflows/verify-ksail-arc-delivery.yaml@refs/heads/main"
const runnerGroupsPath = "/orgs/devantler-tech/actions/runner-groups"

type runnerGroup struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Default    *bool    `json:"default"`
	Inherited  *bool    `json:"inherited"`
	Visibility string   `json:"visibility"`
	Public     *bool    `json:"allows_public_repositories"`
	Restricted *bool    `json:"restricted_to_workflows"`
	ReadOnly   *bool    `json:"workflow_restrictions_read_only"`
	Workflows  []string `json:"selected_workflows"`
}

type groupAPI struct {
	options verificationOptions
	token   string
}

// This callback receives the App JWT only after the existing verifier has
// authenticated the App and its installation. Neither token leaves the process.
func verifyRunnerGroupCapability(ctx context.Context, options verificationOptions, appJWT string, installationID int64, runID string) (result outcome) {
	if _, err := positiveID(runID); err != nil || installationID <= 0 || options.client == nil || options.now == nil || !httpsOrigin(options.githubURL) || !groupToken(appJWT) {
		return "HOLD_INVOCATION"
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if options.cleanup == nil {
		var stop func()
		options.cleanup, stop = cleanupContext(ctx)
		defer stop()
	}
	payload, _ := json.Marshal(map[string]any{"repository_ids": []int64{ksailRunnerRepository}, "permissions": map[string]string{"organization_self_hosted_runners": "write", "metadata": "read"}})
	var access struct {
		Token       string            `json:"token"`
		Expiry      string            `json:"expires_at"`
		Selection   string            `json:"repository_selection"`
		Permissions map[string]string `json:"permissions"`
	}
	app := groupAPI{options: options, token: appJWT}
	code, status := app.call(ctx, http.MethodPost, "/app/installations/"+strconv.FormatInt(installationID, 10)+"/access_tokens", payload, http.StatusCreated, &access)
	if !groupToken(access.Token) {
		// A successful or uncertain write may have minted a token we cannot
		// revoke. Report missing cleanup evidence, never a clean scope refusal.
		if code == http.StatusCreated || code == 0 || code >= 500 {
			return failCleanup
		}
		return "HOLD_CAPABILITY"
	}
	api := groupAPI{options: options, token: access.Token}
	// Arm revocation before validating the returned scope, so a widened token
	// cannot survive a refusal. All cleanup shares one cancellation budget.
	defer func() {
		cleanup, stop := context.WithTimeout(options.cleanup, 10*time.Second)
		defer stop()
		if _, status := api.call(cleanup, http.MethodDelete, "/installation/token", nil, http.StatusNoContent, nil); status != pass {
			result = failCleanup
		}
	}()
	expiry, err := time.Parse(time.RFC3339, access.Expiry)
	if status != pass || err != nil || len(access.Permissions) != 2 || access.Permissions["organization_self_hosted_runners"] != "write" || access.Permissions["metadata"] != "read" || access.Selection != "selected" || !expiry.After(options.now()) || expiry.After(options.now().Add(65*time.Minute)) {
		return "HOLD_CAPABILITY"
	}
	if api.repositorySelection(ctx, "/installation/repositories") != pass {
		return "HOLD_CAPABILITY"
	}
	var repository struct {
		ID       int64  `json:"id"`
		Name     string `json:"full_name"`
		Private  *bool  `json:"private"`
		Archived *bool  `json:"archived"`
	}
	if _, status := api.call(ctx, http.MethodGet, "/repos/devantler-tech/ksail", nil, http.StatusOK, &repository); status != pass || repository.ID != ksailRunnerRepository || repository.Name != "devantler-tech/ksail" || !falseFlag(repository.Private) || !falseFlag(repository.Archived) {
		return "HOLD_CAPABILITY"
	}
	groups, status := api.groups(ctx)
	if status != pass {
		return status
	}
	name := "ksail-capability-" + runID
	for _, group := range groups {
		if strings.HasPrefix(group.Name, "ksail-capability-") {
			return "HOLD_OWNERSHIP"
		}
	}
	for _, group := range groups {
		if group.Name == "platform" {
			if !group.policy() || api.prove(ctx, group) != pass {
				return "HOLD_CAPABILITY"
			}
			return "PASS_EXISTING"
		}
	}
	payload, _ = json.Marshal(map[string]any{"name": name, "visibility": "selected", "selected_repository_ids": []int64{ksailRunnerRepository}, "runners": []int64{}, "allows_public_repositories": true, "restricted_to_workflows": true, "selected_workflows": []string{ksailRunnerWorkflow}})
	var created runnerGroup
	code, status = api.call(ctx, http.MethodPost, runnerGroupsPath, payload, http.StatusCreated, &created)
	if code != http.StatusCreated {
		if code == 0 || code >= 500 {
			return "HOLD_OWNERSHIP"
		}
		return "HOLD_CAPABILITY"
	}
	if status != pass || created.ID <= 0 || created.Name != name {
		return "HOLD_OWNERSHIP"
	}
	for _, previous := range groups {
		if previous.ID == created.ID {
			return "HOLD_OWNERSHIP"
		}
	}
	// Ownership comes only from this successful create, never a name lookup.
	defer func() {
		if api.removeOwned(options.cleanup, created) != pass {
			result = failCleanup
		}
	}()
	if !created.policy() || api.prove(ctx, created) != pass {
		return "HOLD_CAPABILITY"
	}
	return "PASS_DISPOSABLE"
}

func falseFlag(value *bool) bool { return value != nil && !*value }
func trueFlag(value *bool) bool  { return value != nil && *value }
func groupToken(value string) bool {
	return value != "" && len(value) <= maxResponse && !strings.ContainsAny(value, "\r\n\t ")
}
func (group runnerGroup) policy() bool {
	return group.ID > 0 && group.Name != "" && falseFlag(group.Default) && falseFlag(group.Inherited) && group.Visibility == "selected" && trueFlag(group.Public) && trueFlag(group.Restricted) && falseFlag(group.ReadOnly) && slices.Equal(group.Workflows, []string{ksailRunnerWorkflow})
}

// Fixed origins and locally constructed paths avoid following response URLs.
// Writes are never retried; an uncertain create is not permission to adopt it.
func (api groupAPI) call(ctx context.Context, method, path string, body []byte, expected int, target any) (int, outcome) {
	request, err := http.NewRequestWithContext(ctx, method, api.options.githubURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, "HOLD_CAPABILITY"
	}
	request.Header.Set("Authorization", "Bearer "+api.token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	client := *api.options.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return 0, "HOLD_CAPABILITY"
	}
	defer response.Body.Close()
	if response.StatusCode != expected {
		return response.StatusCode, "HOLD_CAPABILITY"
	}
	if target == nil {
		return response.StatusCode, pass
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse || validateJSON(data) != nil || json.Unmarshal(data, target) != nil {
		return response.StatusCode, "HOLD_CAPABILITY"
	}
	return response.StatusCode, pass
}

func (api groupAPI) groups(ctx context.Context) ([]runnerGroup, outcome) {
	var groups []runnerGroup
	seen := map[int64]bool{}
	names := map[string]bool{}
	total := -1
	for page := 1; page <= 10; page++ {
		var value struct {
			Total  *int          `json:"total_count"`
			Groups []runnerGroup `json:"runner_groups"`
		}
		if _, status := api.call(ctx, http.MethodGet, runnerGroupsPath+"?per_page=100&page="+strconv.Itoa(page), nil, http.StatusOK, &value); status != pass || value.Total == nil || *value.Total < 0 || *value.Total > 1000 || value.Groups == nil {
			return nil, "HOLD_CAPABILITY"
		}
		if total < 0 {
			total = *value.Total
		}
		if total != *value.Total {
			return nil, "HOLD_CAPABILITY"
		}
		remaining := total - len(groups)
		expected := min(100, remaining)
		if len(value.Groups) != expected {
			return nil, "HOLD_CAPABILITY"
		}
		for _, group := range value.Groups {
			if group.ID <= 0 || group.Name == "" || seen[group.ID] || names[group.Name] {
				return nil, "HOLD_CAPABILITY"
			}
			seen[group.ID] = true
			names[group.Name] = true
			groups = append(groups, group)
		}
		if len(groups) == total {
			return groups, pass
		}
	}
	return nil, "HOLD_CAPABILITY"
}

func (api groupAPI) prove(ctx context.Context, expected runnerGroup) outcome {
	path := runnerGroupsPath + "/" + strconv.FormatInt(expected.ID, 10)
	var live runnerGroup
	if _, status := api.call(ctx, http.MethodGet, path, nil, http.StatusOK, &live); status != pass || live.ID != expected.ID || live.Name != expected.Name || !live.policy() {
		return "HOLD_CAPABILITY"
	}
	if api.repositorySelection(ctx, path+"/repositories") != pass {
		return "HOLD_CAPABILITY"
	}
	return api.zeroRunners(ctx, path)
}

func (api groupAPI) repositorySelection(ctx context.Context, path string) outcome {
	var selection struct {
		Total        *int `json:"total_count"`
		Repositories []struct {
			ID      int64  `json:"id"`
			Name    string `json:"full_name"`
			Private *bool  `json:"private"`
		} `json:"repositories"`
	}
	if _, status := api.call(ctx, http.MethodGet, path+"?per_page=100&page=1", nil, http.StatusOK, &selection); status != pass || selection.Total == nil || *selection.Total != 1 || len(selection.Repositories) != 1 || selection.Repositories[0].ID != ksailRunnerRepository || selection.Repositories[0].Name != "devantler-tech/ksail" || !falseFlag(selection.Repositories[0].Private) {
		return "HOLD_CAPABILITY"
	}
	return pass
}

func (api groupAPI) zeroRunners(ctx context.Context, path string) outcome {
	for _, kind := range []string{"runners", "hosted-runners"} {
		var membership struct {
			Total   *int              `json:"total_count"`
			Runners []json.RawMessage `json:"runners"`
		}
		if _, status := api.call(ctx, http.MethodGet, path+"/"+kind+"?per_page=100&page=1", nil, http.StatusOK, &membership); status != pass || membership.Total == nil || *membership.Total != 0 || membership.Runners == nil || len(membership.Runners) != 0 {
			return "HOLD_CAPABILITY"
		}
	}
	return pass
}

func (api groupAPI) removeOwned(cleanup context.Context, created runnerGroup) outcome {
	ctx, cancel := context.WithTimeout(cleanup, 30*time.Second)
	defer cancel()
	path := runnerGroupsPath + "/" + strconv.FormatInt(created.ID, 10)
	var live runnerGroup
	if _, status := api.call(ctx, http.MethodGet, path, nil, http.StatusOK, &live); status != pass || live.ID != created.ID || live.Name != created.Name || !falseFlag(live.Default) || !falseFlag(live.Inherited) || api.zeroRunners(ctx, path) != pass {
		return failCleanup
	}
	if _, status := api.call(ctx, http.MethodDelete, path, nil, http.StatusNoContent, nil); status != pass {
		return failCleanup
	}
	if code, _ := api.call(ctx, http.MethodGet, path, nil, http.StatusOK, &live); code != http.StatusNotFound {
		return failCleanup
	}
	groups, status := api.groups(ctx)
	if status != pass {
		return failCleanup
	}
	for _, group := range groups {
		if group.ID == created.ID || group.Name == created.Name {
			return failCleanup
		}
	}
	return pass
}
